"""HTTP API 路由。"""

from __future__ import annotations

import hashlib
import json
import math
import secrets
from fastapi import APIRouter, BackgroundTasks, FastAPI, HTTPException, Query, Request
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime
from pathlib import Path
from typing import Any

from fastapi.responses import FileResponse, HTMLResponse, JSONResponse

from app.config import Settings
from app.db import Database, iso, parse_sessions
from app.gamma import annotate_model_greeks, find_zero_gamma
from app.levels import build_levels
from app.providers.market import ProviderError, MarketDataProvider
from app.services.concurrency import SingleFlightCache
from app.services.history import HistoryService
from app.services.snapshots import SnapshotService, active_expirations, market_today


_FORBIDDEN_PAGE = """<!doctype html>
<html lang="en">
<head><title>403 Forbidden</title></head>
<body>
<center><h1>403 Forbidden</h1></center>
<hr>
<center>nginx</center>
</body>
</html>
"""


def snapshot_signature(value: Any) -> str:
    """为缓存键生成稳定的输入摘要，避免仅依赖时间戳漏掉同批次数据变化。"""
    encoded = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), default=str).encode("utf-8")
    return hashlib.blake2b(encoded, digest_size=16).hexdigest()


def spot_cache_bucket(value: Any) -> float | None:
    """把现价收成稳定格子，供价位缓存复用。

    格子宽度大约是价格数量级的 1/500：200 元附近约 0.2 元。
    半入规则与前端 Math.round 一致。未命中时仍用本次精确现价重算。
    """
    try:
        price = float(value)
    except (TypeError, ValueError):
        return None
    if not math.isfinite(price) or price <= 0:
        return None
    magnitude = 10 ** math.floor(math.log10(price))
    step = magnitude / 500
    units = math.floor(price / step + 0.5)
    return round(units * step, 6)


def install_access_guard(app: FastAPI, settings: Settings) -> None:
    """为页面和 API 安装统一访问密钥校验；空密钥保持旧部署兼容。"""
    expected = settings.access_key.strip()
    if not expected:
        return
    expected_bytes = expected.encode("utf-8")

    @app.middleware("http")
    async def access_guard(request: Request, call_next):
        path = request.url.path
        # 静态资源和健康检查不携带密钥：前者是页面首屏依赖，后者供看门狗判断进程状态。
        if path == "/health" or path.startswith("/static/"):
            return await call_next(request)
        provided = request.query_params.get("key") or request.headers.get("X-Access-Key") or ""
        if not provided or not secrets.compare_digest(provided.encode("utf-8"), expected_bytes):
            if path == "/api" or path.startswith("/api/"):
                return JSONResponse(status_code=403, content={"detail": "403 Forbidden"})
            return HTMLResponse(_FORBIDDEN_PAGE, status_code=403)
        return await call_next(request)


def trend_market_data(bars: list[dict[str, Any]], quote: dict[str, Any]) -> dict[str, Any]:
    """整理趋势面板的今开/昨收；非交易时段缺少当日 K 线时回退最近交易日。"""
    valid = []
    for bar in bars:
        try:
            day = str(bar.get("date"))[:10]
            opening = float(bar.get("open")) if bar.get("open") is not None else None
            close = float(bar.get("close")) if bar.get("close") is not None else None
        except (TypeError, ValueError):
            continue
        valid.append({"date": day, "open": opening, "close": close})
    valid.sort(key=lambda item: item["date"])
    latest = valid[-1] if valid else {}
    previous = valid[-2] if len(valid) > 1 else {}
    # 只有盘中当日日线仍可能继续变化；盘后、夜盘和休市时，最新日线已经是最近一个完整交易日。
    # 原先只比较日期，导致周一收盘后的夜盘仍把周一当成「今日」，错误返回周五收盘。
    market_state = str(quote.get("market_state") or "").upper()
    latest_is_today = (
        latest.get("date") == market_today().isoformat()
        and market_state == "REGULAR"
    )
    today_open = latest.get("open")
    previous_close = previous.get("close") if latest_is_today else latest.get("close")
    previous_close_date = previous.get("date") if latest_is_today else latest.get("date")
    # Yahoo 在盘后/夜盘的 fast_info.previous_close 可能仍停留在前一个交易日。
    # 扩展时段摘要中的 reference_close 是同一批分钟线对应的最近正常盘收盘价，
    # 优先使用它，避免日线缓存或上游 previous_close 落后时显示上周五数据。
    sessions = quote.get("sessions") or {}
    regular_summary = sessions.get("post") or sessions.get("overnight") or {}
    try:
        reference_close = float(regular_summary.get("reference_close"))
    except (TypeError, ValueError):
        reference_close = None
    as_of = str(regular_summary.get("as_of") or "")[:10]
    history_is_behind_session = bool(as_of and (not latest.get("date") or latest.get("date") < as_of))
    if market_state != "REGULAR" and history_is_behind_session and reference_close is not None and reference_close > 0:
        previous_close = reference_close
        previous_close_date = as_of
    if today_open is None:
        today_open = quote.get("today_open")
    if previous_close is None:
        previous_close = quote.get("previous_close")
    return {
        "today_open": today_open,
        "previous_close": previous_close,
        "today_open_date": latest.get("date"),
        "previous_close_date": previous_close_date,
        "market_state": quote.get("market_state"),
    }


def create_router(database: Database, snapshots: SnapshotService, provider: MarketDataProvider, settings: Settings) -> APIRouter:
    router = APIRouter(prefix="/api")
    history = HistoryService(
        database, provider, settings.history_max_age_seconds, settings.extremes_max_age_seconds
    )
    # 只缓存同一份输入快照的计算结果。现价按小格子复用，跨出格子才重算；未命中时仍用精确现价。
    levels_cache: SingleFlightCache[tuple[Any, ...], dict[str, Any]] = SingleFlightCache(maxsize=32)
    gamma_cache: SingleFlightCache[tuple[Any, ...], dict[str, Any]] = SingleFlightCache(maxsize=16)
    chain_cache: SingleFlightCache[tuple[Any, ...], dict[str, Any]] = SingleFlightCache(maxsize=32)

    def shared_cached(
        namespace: str,
        cache_key: tuple[Any, ...],
        callback,
    ) -> dict[str, Any]:
        """先用进程内缓存，再用 SQLite 共享缓存，支持多 worker 复用同一快照结果。"""
        shared_key = f"{namespace}:{snapshot_signature(cache_key)}"
        stored = database.get_analysis_cache(shared_key)
        if isinstance(stored, dict):
            return stored
        value = callback()
        database.put_analysis_cache(shared_key, value)
        return value

    def run_gamma_refresh(job_key: str, symbol_name: str, horizon_days: int) -> None:
        """后台刷新 Gamma 窗口；任务状态写入 SQLite，允许多 worker 共享。"""
        try:
            result = snapshots.refresh_window(symbol_name, horizon_days)
        except Exception as exc:  # noqa: BLE001 - 后台任务必须把异常写回状态
            database.finish_analysis_job(job_key, "failed", None, str(exc))
            return
        database.finish_analysis_job(job_key, "completed", result, None)

    def symbol(value: str) -> str:
        try:
            return provider.normalize_symbol(value)
        except ValueError as exc:
            raise HTTPException(status_code=422, detail=str(exc)) from exc

    def quote_response(row: dict[str, Any], source: str) -> dict[str, Any]:
        """统一行情响应结构：把 SQLite 里的 sessions_json 解析成前端的 sessions 对象。"""
        payload = dict(row)
        stored = payload.pop("sessions_json", None)
        sessions = payload.get("sessions") or parse_sessions(stored)
        # 最新快照没有时段数据时（数据源限流或旧进程写入）回退最近 24 小时内的有效值。
        if not sessions and payload.get("symbol"):
            sessions = database.latest_sessions(payload["symbol"])
        payload["sessions"] = sessions
        payload["source"] = source
        return payload

    def pending_quote(symbol_name: str) -> dict[str, Any]:
        """本地还没有快照时先返回占位结构，前端据此显示「后台刷新中」。"""
        return {
            "symbol": symbol_name, "price": None, "change_percent": None, "currency": "USD",
            "market_state": None, "sessions": {}, "provider": "upstream", "source": "pending",
        }

    @router.get("/quote/{stock_symbol}")
    def quote(stock_symbol: str, refresh: bool = Query(default=False)) -> dict[str, Any]:
        normalized = symbol(stock_symbol)
        cached = database.latest_quote(normalized)
        if cached and cached.get("price") is not None and not refresh:
            return quote_response(cached, "sqlite")
        if not refresh:
            return pending_quote(normalized)
        try:
            fresh = provider.quote(normalized)
            if fresh.get("price") is not None:
                return quote_response(fresh, "upstream")
            if cached and cached.get("price") is not None:
                return quote_response(cached, "sqlite")
            return pending_quote(normalized)
        except ProviderError as exc:
            if cached and cached.get("price") is not None:
                return {**quote_response(cached, "sqlite"), "warning": str(exc)}
            if not refresh:
                return pending_quote(normalized)
            raise HTTPException(status_code=502, detail=str(exc)) from exc

    @router.get("/expirations/{stock_symbol}")
    def expirations(stock_symbol: str, refresh: bool = Query(default=False)) -> dict[str, Any]:
        normalized = symbol(stock_symbol)
        # 已过期的到期日不再下发给前端：这类历史合约无法再从上游刷新，会让页面一直停在旧快照。
        cached = active_expirations(database.latest_expirations(normalized))
        if cached and not refresh:
            return {"symbol": normalized, "expirations": cached, "source": "sqlite"}
        if not refresh:
            return {"symbol": normalized, "expirations": [], "source": "pending"}
        try:
            values = provider.expirations(normalized)
        except ProviderError as exc:
            if cached:
                return {"symbol": normalized, "expirations": cached, "source": "sqlite", "warning": str(exc)}
            raise HTTPException(status_code=502, detail=str(exc)) from exc
        return {"symbol": normalized, "expirations": active_expirations(values), "source": "upstream"}

    @router.get("/chain/{stock_symbol}")
    def chain(stock_symbol: str, expiration: str = Query(..., pattern=r"^\d{4}-\d{2}-\d{2}$")) -> dict[str, Any]:
        normalized = symbol(stock_symbol)
        cached = database.latest_chain(normalized, expiration)
        if cached["data"]:
            quote = database.latest_quote(normalized) or {}
            quote_price = quote.get("price")
            chain_key = (
                normalized,
                expiration,
                cached.get("fetched_at"),
                cached.get("oi_fallback", {}).get("as_of"),
                quote_price,
                snapshot_signature(cached["data"]),
            )

            def compute_chain() -> dict[str, Any]:
                rows = [dict(row) for row in cached["data"]]
                return {"data": rows, "iv_model": annotate_model_greeks(rows, quote_price)}

            analyzed = chain_cache.get_or_compute(
                chain_key,
                lambda: shared_cached("chain", chain_key, compute_chain),
            )
            return {
                "symbol": normalized,
                "expiration": expiration,
                "iv_model": analyzed["iv_model"],
                **cached,
                "data": analyzed["data"],
                "source": "sqlite",
            }
        return {"symbol": normalized, "expiration": expiration, "fetched_at": None, "data": [], "source": "pending"}

    @router.get("/gamma/{stock_symbol}")
    def gamma_profile(
        stock_symbol: str,
        background_tasks: BackgroundTasks,
        refresh: bool = Query(default=False),
        horizon_days: int = Query(default=45, ge=1, le=365),
        status_only: bool = Query(default=False),
        include_rows: bool = Query(default=True),
    ) -> dict[str, Any]:
        """返回近期期限的链，供 Zero Gamma/Gamma Flip 曲线使用。

        轮询传 status_only 时只回任务状态，避免每秒读取并序列化全部合约。
        页面展示传 include_rows=false：仍计算零 Gamma 和模型 IV，但不把合约行送到浏览器。
        """
        normalized = symbol(stock_symbol)
        refresh_result: dict[str, Any] | None = None
        if refresh:
            refresh_result = database.claim_analysis_job(normalized, horizon_days)
            if refresh_result.get("claimed"):
                background_tasks.add_task(
                    run_gamma_refresh,
                    refresh_result["job_id"],
                    normalized,
                    horizon_days,
                )
        if status_only:
            job_state = refresh_result or database.analysis_job(normalized, horizon_days)
            return {
                "symbol": normalized,
                "source": "sqlite",
                "refresh": job_state,
                "data": [],
                "contract_count": 0,
                "status_only": True,
            }
        profile = database.latest_chains(normalized, horizon_days)
        quote = database.latest_quote(normalized) or {}
        rows = profile.get("data") or []
        quote_price = quote.get("price")
        gamma_key = (
            normalized,
            horizon_days,
            profile.get("fetched_at"),
            profile.get("oi_fallback", {}).get("as_of"),
            quote_price,
            snapshot_signature(rows),
        )

        def compute_gamma() -> dict[str, Any]:
            analysis_rows = [dict(row) for row in rows]
            iv_model = annotate_model_greeks(analysis_rows, quote_price)
            return {
                "data": analysis_rows,
                "iv_model": iv_model,
                "zero_gamma": find_zero_gamma(analysis_rows, quote_price),
            }

        gamma_result = gamma_cache.get_or_compute(
            gamma_key,
            lambda: shared_cached("gamma", gamma_key, compute_gamma),
        )
        job_state = refresh_result or database.analysis_job(normalized, horizon_days)
        contracts = gamma_result["data"]
        # 默认响应保持完整合约；页面展示不需要逐行数据时去掉这一大段，避免浏览器解析整窗合约。
        profile_meta = {key: value for key, value in profile.items() if key != "data"}
        return {
            "symbol": normalized,
            "iv_model": gamma_result["iv_model"],
            **profile_meta,
            "data": contracts if include_rows else [],
            "contract_count": len(contracts),
            "source": "sqlite",
            "refresh": job_state,
            "zero_gamma": gamma_result["zero_gamma"],
            "status_only": False,
        }

    @router.post("/refresh/{stock_symbol}")
    def refresh(stock_symbol: str, expiration: str | None = Query(default=None, pattern=r"^\d{4}-\d{2}-\d{2}$"), max_age: int = Query(default=60, ge=0, le=3600)) -> dict[str, Any]:
        """刷新一次快照；本地快照在 max_age 秒内时直接复用（skipped=True），不再请求上游接口。"""
        normalized = symbol(stock_symbol)
        try:
            result = snapshots.refresh(normalized, expiration, max_age_seconds=max_age)
            # 刷新请求与页面的 quote 请求可能在弱服务器上出现先后顺序差异；
            # 把本次最终快照里的行情一并返回，避免期权链已显示而现价仍停在占位符。
            cached_quote = database.latest_quote(normalized)
            if cached_quote:
                result["quote"] = quote_response(cached_quote, "sqlite")
            return result
        except ValueError as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from exc
        except (ProviderError, RuntimeError) as exc:
            raise HTTPException(status_code=502, detail=str(exc)) from exc

    @router.get("/levels/{stock_symbol}")
    def levels(
        stock_symbol: str,
        expiration: str = Query(..., pattern=r"^\d{4}-\d{2}-\d{2}$"),
        spot: float | None = Query(default=None, gt=0),
    ) -> dict[str, Any]:
        """压力位/支撑位：技术面与近 45 天多期限期权持仓综合。

        `spot` 是前端传入的当前展示基准价；候选池优先使用快照中的前一交易日收盘价作为
        日内稳定锚点，避免实时价变化或实时价与盘后价切换时反复生成不同的价位簇。
        """
        normalized = symbol(stock_symbol)
        quote = database.latest_quote(normalized) or {}
        # 价位接口使用跨期限链；选中的远期期限若不在 45 天窗口内，额外并入，避免切换期限后期权因子消失。
        profile = database.latest_chains(normalized, horizon_days=45)
        option_rows = list(profile.get("data") or [])
        selected_chain: dict[str, Any] | None = None
        # 选中期限已经在 45 天窗口时直接复用窗口查询结果，避免再次读取同一批次。
        # 远期期限不在窗口内时才额外读取，保持切换远期期限后仍能参与合成。
        if expiration not in profile.get("expirations", []):
            selected_chain = database.latest_chain(normalized, expiration)
            seen = {(str(row.get("expiration")), str(row.get("contract_symbol"))) for row in option_rows}
            for row in selected_chain.get("data") or []:
                key = (str(row.get("expiration")), str(row.get("contract_symbol")))
                if key not in seen:
                    option_rows.append(row)
                    seen.add(key)
        option_expirations = sorted({str(row.get("expiration")) for row in option_rows if row.get("expiration")})
        fetched_values = [value for value in (profile.get("fetched_at"), (selected_chain or {}).get("fetched_at")) if value]
        history_payload = history.bars(normalized)
        # 极值和 Beta 使用不同缓存/锁；并行读取可以缩短首次加载等待。Beta 复用已经取回的两年日线，
        # 避免同一请求再次向行情源请求同一份标的数据。
        with ThreadPoolExecutor(max_workers=2, thread_name_prefix="levels-input") as executor:
            extremes_future = executor.submit(history.extremes, normalized)
            beta_future = executor.submit(history.beta, normalized, history_payload.get("bars") or None)
            extremes_payload = extremes_future.result()
            beta_payload = beta_future.result()
        resolved_spot = spot if spot is not None else quote.get("price")
        # 候选池使用昨收作为日内稳定锚点；最新价只负责当前侧别、距离和触及概率。
        # 这样盘中价格小幅波动时不会反复重建相邻价位簇，昨收缺失时才回退最新价。
        candidate_spot = quote.get("previous_close")
        try:
            if candidate_spot is None or float(candidate_spot) <= 0:
                candidate_spot = quote.get("price") or resolved_spot
        except (TypeError, ValueError):
            candidate_spot = resolved_spot
        cache_key = (
            normalized,
            expiration,
            spot_cache_bucket(resolved_spot),
            candidate_spot,
            profile.get("fetched_at"),
            (selected_chain or {}).get("fetched_at"),
            profile.get("oi_fallback", {}).get("as_of"),
            (selected_chain or {}).get("oi_fallback", {}).get("as_of"),
            history_payload.get("fetched_at"),
            extremes_payload.get("fetched_at"),
            snapshot_signature(option_rows),
            len(history_payload.get("bars") or []),
            snapshot_signature(extremes_payload.get("extremes")),
        )
        computed = levels_cache.get_or_compute(
            cache_key,
            lambda: shared_cached(
                # 买方结构改为合约波动率、触及概率和 10 到 45 天搜索，升级缓存命名空间。
                "levels-v10",
                cache_key,
                lambda: build_levels(
                    history_payload.get("bars") or [],
                    option_rows,
                    resolved_spot,
                    expiration,
                    extremes_payload.get("extremes"),
                    candidate_spot,
                ),
            ),
        )
        bars = list(history_payload.get("bars") or [])
        return {
            "symbol": normalized,
            "chain_fetched_at": max(fetched_values) if fetched_values else None,
            "options_fetched_at": max(fetched_values) if fetched_values else None,
            "options_expirations": option_expirations,
            "options_horizon_days": 45,
            "generated_at": iso(),
            "history": {
                "bars": len(bars),
                "from": bars[0]["date"] if bars else None,
                "to": bars[-1]["date"] if bars else None,
                "fetched_at": history_payload.get("fetched_at"),
                "source": history_payload.get("source"),
                "warning": history_payload.get("warning"),
                # 52 周/历史高低点来自全量日线，单独一套缓存与回源周期，页面按 source 标注新鲜度。
                "extremes_fetched_at": extremes_payload.get("fetched_at"),
                "extremes_source": extremes_payload.get("source"),
                "extremes_warning": extremes_payload.get("warning"),
            },
            "trend_market": trend_market_data(bars, quote),
            "beta": beta_payload.get("beta"),
            "beta_meta": {
                "fetched_at": beta_payload.get("fetched_at"),
                "source": beta_payload.get("source"),
                "warning": beta_payload.get("warning"),
            },
            **computed,
        }

    @router.get("/status/{stock_symbol}")
    def status(stock_symbol: str) -> dict[str, Any]:
        normalized = symbol(stock_symbol)
        cached = database.latest_quote(normalized)
        return {
            "symbol": normalized,
            "quote": quote_response(cached, "sqlite") if cached else None,
            "refresh": database.latest_status(normalized),
        }

    @router.post("/cleanup")
    def cleanup() -> dict[str, Any]:
        deleted = database.cleanup(settings.raw_retention_days)
        return {
            "deleted": deleted,
            # 按体积上限做的二次清理：DATABASE_MAX_MB 为 0 时直接返回当前占用。
            "size": database.cleanup_by_size(settings.database_max_mb * 1048576),
        }

    return router
