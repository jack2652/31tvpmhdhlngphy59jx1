"""HTTP API 路由。"""

from __future__ import annotations

import hashlib
import json
import math
import secrets
import threading
from fastapi import APIRouter, FastAPI, HTTPException, Query, Request
from concurrent.futures import ThreadPoolExecutor
from datetime import date, datetime
from pathlib import Path
from typing import Any

from fastapi.responses import FileResponse, HTMLResponse, JSONResponse

from app.config import Settings
from app.db import Database, expiration_dates, iso, parse_sessions
from app.gamma import annotate_model_greeks, find_zero_gamma
from app.levels import build_levels
from app.providers.market import FAIR_VALUE_SOURCE, ProviderError, MarketDataProvider
from app.runtime import release_memory
from app.services.concurrency import SingleFlightCache, get_heavy_gate
from app.services.earnings import summarize_earnings
from app.services.history import HistoryService
from app.services.snapshots import SnapshotService, active_expirations, market_today, snapshot_age_seconds


# 后台 Gamma 窗口超过这个时间还没写回，只把这一轮标记失败。晚到的线程靠 started_at 避免覆盖新任务。
GAMMA_JOB_TIMEOUT_SECONDS = 180
GAMMA_JOB_TIMEOUT_MESSAGE = "分析超时，已停止本轮计算"


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


def _positive_price(value: Any) -> float | None:
    """把行情数值收成正的有限价格；缺失或无效时返回 None。"""
    if isinstance(value, bool):
        return None
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    if not math.isfinite(number) or number <= 0:
        return None
    return number


def _open_copied_from_previous(current: Any, previous: Any) -> bool:
    """今开和前一根日线开盘价几乎相同，视为未完成日线把昨开抄了进来。"""
    current_number = _positive_price(current)
    previous_number = _positive_price(previous)
    if current_number is None or previous_number is None:
        return False
    tolerance = max(0.01, abs(previous_number) * 1e-4)
    return abs(current_number - previous_number) <= tolerance


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
    today_open_date = latest.get("date")
    calendar_today = market_today().isoformat()
    # “昨开”始终指最近一个已完成交易日的常规时段开盘价。
    previous_open_row = previous if latest_is_today else latest
    previous_open = previous_open_row.get("open")
    previous_open_date = previous_open_row.get("date")
    previous_close = previous.get("close") if latest_is_today else latest.get("close")
    previous_close_date = previous.get("date") if latest_is_today else latest.get("date")
    # Yahoo 在盘后/夜盘的 fast_info.previous_close 可能仍停留在前一个交易日。
    # 扩展时段摘要中的 reference_close 是同一批分钟线对应的最近正常盘收盘价，
    # 优先使用它，避免日线缓存或上游 previous_close 落后时显示上周五数据。
    sessions = quote.get("sessions") or {}
    # 盘前的基准来自上一交易日正式收盘；盘后/夜盘才使用盘后摘要。
    # 不能固定先取 post：它可能保留 15:59 的 132.63，而 quote 已校准为正式收盘 132.60。
    if market_state == "PRE":
        regular_summary = sessions.get("pre") or {}
    else:
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
    quote_open = _positive_price(quote.get("today_open"))
    # 日线还没滚到今天，或今天的开盘价和前一根日线开盘价相同，都说明今开还不可信。
    daily_missing_today = latest.get("date") != calendar_today
    daily_open_copied = (not daily_missing_today) and _open_copied_from_previous(today_open, previous.get("open"))
    if market_state in {"REGULAR", "POST", "OVERNIGHT"} and quote_open is not None and (daily_missing_today or daily_open_copied):
        today_open = quote_open
        today_open_date = calendar_today
    elif today_open is None:
        today_open = quote.get("today_open")
    if previous_close is None:
        previous_close = quote.get("previous_close")
    return {
        "today_open": today_open,
        "previous_open": previous_open,
        "previous_close": previous_close,
        "today_open_date": today_open_date,
        "previous_open_date": previous_open_date,
        "previous_close_date": previous_close_date,
        "market_state": quote.get("market_state"),
    }


def _align_quote_with_history(
    quote: dict[str, Any],
    history_payload: dict[str, Any] | None,
    max_age_seconds: int,
) -> dict[str, Any]:
    """用新鲜日线校准缓存行情，避免首屏 quote 与趋势通道使用不同昨收。"""
    if not quote or not isinstance(history_payload, dict):
        return quote
    age = snapshot_age_seconds(history_payload.get("fetched_at"))
    if age is None or age > max_age_seconds:
        return quote
    bars = history_payload.get("bars")
    if not isinstance(bars, list) or not bars:
        return quote
    recent_dates = sorted(str(item.get("date"))[:10] for item in bars if isinstance(item, dict) and item.get("date"))
    try:
        latest_day = date.fromisoformat(recent_dates[-1])
    except (IndexError, ValueError):
        return quote
    # 防止上游刚返回一份“抓取时间新、实际交易日很旧”的降级历史覆盖可靠 quote。
    # 正常周末/节假日最多相隔几天，超过一周说明它不能作为首屏昨收依据。
    if (market_today() - latest_day).days > 7:
        return quote
    market = trend_market_data(bars, quote)
    previous_close = _positive_price(market.get("previous_close"))
    if previous_close is None:
        return quote
    aligned = dict(quote)
    aligned["previous_close"] = previous_close
    price = _positive_price(aligned.get("price"))
    if price is not None:
        aligned["change_percent"] = (price - previous_close) / previous_close * 100
    return aligned


def create_router(database: Database, snapshots: SnapshotService, provider: MarketDataProvider, settings: Settings) -> APIRouter:
    router = APIRouter(prefix="/api")
    history = HistoryService(
        database, provider, settings.history_max_age_seconds, settings.extremes_max_age_seconds
    )
    # 只缓存同一份输入快照的计算结果。现价按小格子复用，跨出格子才重算；未命中时仍用精确现价。
    # 低内存机器只留当前标的的计算结果，避免几十份期权链同时待在进程里。
    levels_cache: SingleFlightCache[tuple[Any, ...], dict[str, Any]] = SingleFlightCache(maxsize=2 if settings.low_memory else 32)
    gamma_cache: SingleFlightCache[tuple[Any, ...], dict[str, Any]] = SingleFlightCache(maxsize=2 if settings.low_memory else 16)
    chain_cache: SingleFlightCache[tuple[Any, ...], dict[str, Any]] = SingleFlightCache(maxsize=2 if settings.low_memory else 32)

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
        bulky = settings.low_memory and (
            namespace == "chain" or (namespace == "gamma" and bool(value.get("data")))
        )
        if not bulky:
            database.put_analysis_cache(shared_key, value, settings.analysis_cache_entries)
        return value

    def quote_history(symbol_name: str) -> dict[str, Any] | None:
        """为现货响应准备与趋势通道相同的新鲜日线。"""
        cached = database.latest_history(symbol_name)
        cached_age = snapshot_age_seconds((cached or {}).get("fetched_at"))
        if cached and cached_age is not None and cached_age <= settings.history_max_age_seconds:
            return cached
        # 新标的首屏通常先请求 quote，日线则由 levels 随后才触发；这里提前补齐一次，
        # 让现货卡片不会先显示旧快照的昨收，再被趋势通道的正确值覆盖。
        try:
            refreshed = history.bars(symbol_name)
        except Exception:
            refreshed = None
        return refreshed if refreshed and refreshed.get("bars") else cached

    def run_gamma_refresh(
        job_key: str,
        symbol_name: str,
        horizon_days: int,
        started_at: str,
        start_after: str | None = None,
    ) -> None:
        """后台刷新 Gamma 窗口；任务状态写入 SQLite，允许多 worker 共享。

        用独立线程而不是请求里的 BackgroundTasks：后者要等任务结束，ASGI 调用才返回，
        外面的响应缓冲会把 status 接口一起拖住。超时只失败这一轮的 started_at。
        """
        def expire() -> None:
            database.finish_analysis_job(job_key, "failed", None, GAMMA_JOB_TIMEOUT_MESSAGE, started_at)

        timer = threading.Timer(GAMMA_JOB_TIMEOUT_SECONDS, expire)
        timer.daemon = True
        timer.start()
        try:
            try:
                if start_after is None:
                    # 保持旧版测试适配器和第三方 SnapshotService 子类的两参数接口。
                    result = snapshots.refresh_window(symbol_name, horizon_days)
                else:
                    result = snapshots.refresh_window(symbol_name, horizon_days, batch_size=1, start_after=start_after)
            except Exception as exc:  # noqa: BLE001 - 后台任务必须把异常写回状态
                database.finish_analysis_job(job_key, "failed", None, str(exc), started_at)
                return
            if result.get("deferred"):
                default_warning = (
                    "内存保护：Gamma 窗口已让路"
                    if settings.low_memory
                    else "Gamma 窗口正在等待首屏刷新完成"
                )
                database.finish_analysis_job(
                    job_key,
                    "failed",
                    result,
                    result.get("warning") or default_warning,
                    started_at,
                )
            else:
                database.finish_analysis_job(job_key, "completed", result, None, started_at)
        finally:
            timer.cancel()

    def symbol(value: str) -> str:
        try:
            return provider.normalize_symbol(value)
        except ValueError as exc:
            raise HTTPException(status_code=422, detail=str(exc)) from exc

    def merge_fair_value_payload(payload: dict[str, Any], result: dict[str, Any]) -> None:
        """把内存/SQLite 估值结果覆盖到行情行，避免等待下一次完整快照。"""
        mapping = {
            "value": "fair_value", "low": "fair_value_low", "high": "fair_value_high",
            "buy_low": "fair_value_buy_low", "buy_high": "fair_value_buy_high",
            "source": "fair_value_source", "model": "fair_value_model",
            "forward_eps": "fair_value_forward_eps", "forward_eps_source": "fair_value_forward_eps_source",
            "safety_margin": "fair_value_safety_margin", "confidence": "fair_value_confidence",
            "confidence_score": "fair_value_confidence_score", "interest_coverage": "fair_value_interest_coverage",
            "regime": "fair_value_regime", "regime_signals": "fair_value_regime_signals",
            "model_under_regime": "fair_value_model_under_regime", "defensive": "fair_value_defensive",
            "optimistic": "fair_value_optimistic", "status": "fair_value_status", "warning": "fair_value_warning",
            "normalized_eps_source": "fair_value_normalized_eps_source",
            "quarterly_momentum": "fair_value_quarterly_momentum",
            "historical_valuation_percentiles": "fair_value_historical_valuation_percentiles",
            "shareholder_total_return_yield": "fair_value_shareholder_total_return_yield",
            "market_cap_data_quality": "fair_value_market_cap_data_quality",
            "data_quality_score": "fair_value_data_quality_score",
            "owner_earnings_maintenance_ratio": "fair_value_owner_earnings_maintenance_ratio",
            "owner_earnings_ratio_source": "fair_value_owner_earnings_ratio_source",
        }
        for key, payload_key in mapping.items():
            if key in result:
                payload[payload_key] = result.get(key)

    def quote_response(row: dict[str, Any], source: str) -> dict[str, Any]:
        """统一行情响应结构：把 SQLite 里的 sessions_json 解析成前端的 sessions 对象。"""
        payload = dict(row)
        # 估值任务是异步的；旧快照可能只有行情而没有估值，状态字段不能继续
        # 让前端把「没有候选」误显示成「仍在等待」。
        payload.setdefault(
            "fair_value_status",
            "ready" if payload.get("fair_value_source") == FAIR_VALUE_SOURCE and payload.get("fair_value") is not None else "pending",
        )
        payload.setdefault("fair_value_warning", None)
        # 估值在行情快照之后异步完成；没有结果时由前端做轻量轮询，而不是重复抓整份期权链。
        payload.setdefault("fair_value_pending", payload.get("fair_value_source") != FAIR_VALUE_SOURCE)
        # 估值在后台线程完成后先写入共享分析缓存；这里合并到旧行情快照，
        # 让多 worker 和首次估值都能在下一次轻量报价请求中显示，不必等待期权链刷新。
        symbol_name = str(payload.get("symbol") or "").upper()
        if symbol_name and payload.get("fair_value_source") != FAIR_VALUE_SOURCE:
            shared = database.get_analysis_cache(f"fair-value:{FAIR_VALUE_SOURCE.removeprefix('valuation_')}:{symbol_name}")
            if isinstance(shared, dict) and shared.get("source") == FAIR_VALUE_SOURCE and shared.get("value") is not None:
                merge_fair_value_payload(payload, shared)
        stored = payload.pop("sessions_json", None)
        for field, column in (("fair_value_defensive", "fair_value_defensive_json"), ("fair_value_optimistic", "fair_value_optimistic_json"), ("fair_value_regime_signals", "fair_value_regime_signals_json"), ("fair_value_quarterly_momentum", "fair_value_quarterly_momentum_json")):
            raw = payload.pop(column, None)
            if field not in payload or payload.get(field) is None:
                try:
                    payload[field] = json.loads(raw) if raw else None
                except (TypeError, ValueError):
                    payload[field] = None
        sessions = payload.get("sessions") or parse_sessions(stored)
        # 最新快照没有时段数据时（数据源限流或旧进程写入）回退最近 24 小时内的有效值。
        if not sessions and payload.get("symbol"):
            sessions = database.latest_sessions(payload["symbol"])
        payload["sessions"] = sessions
        # 首次进入标的时 quote 可能先读到旧 SQLite 行，而 levels 已经拿到较新的日线。
        # 先在响应层按同一趋势口径校准，避免现货卡片短暂显示错误昨收，随后又被覆盖。
        if symbol_name:
            payload = _align_quote_with_history(payload, quote_history(symbol_name), settings.history_max_age_seconds)
        # 财报提示是现货卡片的一部分，不能依赖稍后才触发的 levels 请求；统一走
        # HistoryService，缓存缺失或过期时会补读上游，盘后发布当天也能立即显示。
        earnings_payload: dict[str, Any]
        try:
            earnings_payload = history.earnings(symbol_name) if symbol_name else {"dates": [], "source": "none"}
        except Exception as exc:
            earnings_payload = {"dates": [], "source": "none", "warning": str(exc)}
        earnings_dates = earnings_payload.get("dates")
        payload["earnings"] = summarize_earnings(
            earnings_dates if isinstance(earnings_dates, list) else None,
            market_today(),
        )
        payload["earnings"]["fetched_at"] = earnings_payload.get("fetched_at")
        payload["earnings"]["source"] = earnings_payload.get("source")
        payload["earnings"]["warning"] = earnings_payload.get("warning")
        if payload.get("fair_value_source") == FAIR_VALUE_SOURCE and payload.get("fair_value") is not None:
            payload["fair_value_status"] = "ready"
        payload["fair_value_pending"] = payload.get("fair_value_source") != FAIR_VALUE_SOURCE and payload.get("fair_value_status") in {"pending", "retry"}
        payload["source"] = source
        return payload

    def pending_quote(symbol_name: str) -> dict[str, Any]:
        """本地还没有快照时先返回占位结构，前端据此显示「后台刷新中」。"""
        return {
            "symbol": symbol_name, "price": None, "change_percent": None, "currency": "USD",
            "market_state": None, "sessions": {}, "provider": "upstream", "source": "pending",
            "fair_value": None, "fair_value_low": None, "fair_value_high": None,
            "fair_value_buy_low": None, "fair_value_buy_high": None, "fair_value_source": None,
            "fair_value_model": None, "fair_value_forward_eps": None,
            "fair_value_forward_eps_source": None, "fair_value_safety_margin": None,
            "fair_value_confidence": None,
            "fair_value_confidence_score": None, "fair_value_interest_coverage": None,
            "fair_value_regime": None, "fair_value_regime_signals": None, "fair_value_model_under_regime": None,
            "fair_value_defensive": None, "fair_value_optimistic": None,
            "fair_value_normalized_eps_source": None, "fair_value_quarterly_momentum": None,
            "fair_value_historical_valuation_percentiles": None, "fair_value_shareholder_total_return_yield": None,
            "fair_value_market_cap_data_quality": None, "fair_value_data_quality_score": None,
            "fair_value_owner_earnings_maintenance_ratio": None, "fair_value_owner_earnings_ratio_source": None,
            "fair_value_status": "pending", "fair_value_warning": None,
            "earnings": summarize_earnings(None, market_today()),
            "fair_value_pending": True,
        }

    @router.get("/quote/{stock_symbol}")
    def quote(stock_symbol: str, refresh: bool = Query(default=False)) -> dict[str, Any]:
        normalized = symbol(stock_symbol)
        cached = database.latest_quote(normalized)
        if cached and cached.get("price") is not None and not refresh:
            payload = quote_response(cached, "sqlite")
            # 本地行情快照存在但估值缺失时，轻量请求也要触发估值任务；不能
            # 让前端的轮询永远只读同一份没有估值的 SQLite 行。
            if payload.get("fair_value_source") != FAIR_VALUE_SOURCE:
                try:
                    pending = provider.ensure_fair_value(normalized)
                    if pending.get("status") == "ready" and pending.get("value") is not None:
                        merge_fair_value_payload(payload, pending)
                        payload["fair_value_pending"] = False
                    elif pending.get("status") in {"retry", "unavailable", "failed"}:
                        merge_fair_value_payload(payload, pending)
                        payload["fair_value_pending"] = pending.get("status") == "retry"
                except Exception:
                    # 估值是可选字段，触发失败不能影响现货快照响应。
                    pass
            return payload
        if not refresh:
            try:
                pending = provider.ensure_fair_value(normalized)
                response = pending_quote(normalized)
                if pending.get("status") == "ready" and pending.get("value") is not None:
                    merge_fair_value_payload(response, pending)
                    response["fair_value_pending"] = False
                    return response
                if pending.get("status") in {"retry", "unavailable", "failed"}:
                    merge_fair_value_payload(response, pending)
                    response["fair_value_pending"] = pending.get("status") == "retry"
                    return response
            except Exception:
                pass
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

    def listed_expirations(stock_symbol: str) -> list[str]:
        """下拉框优先用完整到期日缓存；旧库没有这份缓存时才退回已经落库的链。"""
        catalog = active_expirations(database.latest_expiration_catalog(stock_symbol))
        if catalog:
            return catalog
        return active_expirations(database.latest_expirations(stock_symbol))

    @router.get("/expirations/{stock_symbol}")
    def expirations(stock_symbol: str, refresh: bool = Query(default=False)) -> dict[str, Any]:
        normalized = symbol(stock_symbol)
        # 已过期的到期日不再下发给前端：这类历史合约无法再从上游刷新，会让页面一直停在旧快照。
        cached = listed_expirations(normalized)
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
        active = active_expirations(values)
        # 空列表不覆盖已有缓存：没有期权的标的保持「未缓存」，避免把旧的完整列表抹掉。
        if active:
            database.write_expiration_catalog(normalized, active, iso())
        return {"symbol": normalized, "expirations": active, "source": "upstream"}

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

    @router.get("/flow/{stock_symbol}")
    def option_flow(stock_symbol: str, expiration: str = Query(..., pattern=r"^\d{4}-\d{2}-\d{2}$")) -> dict[str, Any]:
        """比较同一期限最近两次快照，返回估算的 Call/Put 买卖流向。"""
        normalized = symbol(stock_symbol)
        return database.option_flow(normalized, expiration)

    @router.get("/gamma/{stock_symbol}")
    def gamma_profile(
        stock_symbol: str,
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
                # 先把状态返回给轮询，窗口刷新放到守护线程里，不占用这次请求。
                threading.Thread(
                    target=run_gamma_refresh,
                    args=(
                        refresh_result["job_id"],
                        normalized,
                        horizon_days,
                        refresh_result["started_at"],
                        (refresh_result.get("resume_result") or {}).get("next_cursor"),
                    ),
                    name=f"gamma-{normalized}-{horizon_days}",
                    daemon=True,
                ).start()
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
        # 整窗合约是小内存机器上最大的一块临时对象。已有刷新时直接降级，连窗口都不再读出来。
        held_gate = None
        if settings.low_memory:
            candidate = get_heavy_gate()
            if not candidate.acquire(timeout=0):
                job_state = refresh_result or database.analysis_job(normalized, horizon_days)
                return {
                    "symbol": normalized,
                    "source": "sqlite",
                    "refresh": job_state,
                    "data": [],
                    "contract_count": 0,
                    "iv_model": {},
                    "zero_gamma": None,
                    "status_only": False,
                    "degraded": True,
                    "warning": "内存保护：已有刷新在进行，Gamma 窗口稍后计算",
                }
            held_gate = candidate
        try:
            profile = database.latest_chains(normalized, horizon_days)
            quote = database.latest_quote(normalized) or {}
            rows = profile.get("data") or []
            quote_price = quote.get("price")
            gamma_key = (
                normalized,
                horizon_days,
                include_rows,
                profile.get("fetched_at"),
                profile.get("oi_fallback", {}).get("as_of"),
                quote_price,
                snapshot_signature(rows),
            )

            def compute_gamma() -> dict[str, Any]:
                analysis_rows = [dict(row) for row in rows]
                iv_model = annotate_model_greeks(analysis_rows, quote_price)
                # 分批窗口每次只对当前已落库的期限求零 Gamma；批次越多，结果越接近完整 45 天窗口。
                zero_gamma = find_zero_gamma(analysis_rows, quote_price, horizon_days=horizon_days)
                contract_count = len(analysis_rows)
                # 页面只要汇总时丢掉合约副本，避免精简缓存再留一份整窗期权链。
                if include_rows:
                    stored_rows = analysis_rows
                else:
                    stored_rows = []
                    analysis_rows.clear()
                return {
                    "data": stored_rows,
                    "contract_count": contract_count,
                    "iv_model": iv_model,
                    "zero_gamma": zero_gamma,
                }

            gamma_result = gamma_cache.get_or_compute(
                gamma_key,
                lambda: shared_cached("gamma", gamma_key, compute_gamma),
            )
            job_state = refresh_result or database.analysis_job(normalized, horizon_days)
            contracts = gamma_result.get("data") or []
            # 默认响应保持完整合约；页面展示不需要逐行数据时去掉这一大段，避免浏览器解析整窗合约。
            profile_meta = {key: value for key, value in profile.items() if key != "data"}
            return {
                "symbol": normalized,
                "iv_model": gamma_result["iv_model"],
                **profile_meta,
                "data": contracts if include_rows else [],
                "contract_count": gamma_result.get("contract_count", len(contracts)),
                "source": "sqlite",
                "refresh": job_state,
                "zero_gamma": gamma_result["zero_gamma"],
                "status_only": False,
            }
        finally:
            if held_gate is not None:
                held_gate.release()

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
            # 本次抓取已经拿到的完整列表优先；跳过回源时用上次缓存，最后才退回已落库的链。
            provided = result.get("expirations")
            if result.get("quote_only"):
                result["expirations"] = []
            elif isinstance(provided, list) and provided:
                active = active_expirations(expiration_dates(item for item in provided if isinstance(item, str)))
                result["expirations"] = active or listed_expirations(normalized)
            else:
                result["expirations"] = listed_expirations(normalized)
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
        raw: bool = Query(default=False),
        raw_expirations: int = Query(default=4, ge=1, le=8),
    ) -> dict[str, Any]:
        """压力位/支撑位：技术面与近 45 天多期限期权持仓综合。

        ``raw=true`` 是轻量客户端计算模式。它只读取已经落库的日线和分析快照，
        不触发历史、Beta、财报回源，也不执行 ``build_levels``；浏览器可以用这些
        原始数据计算趋势、极值和基础价位。原始数据不足时，前端显示期权分布回退，
        不在倒计时刷新链路里启动默认综合模式。

        `spot` 是前端传入的当前展示基准价；候选池优先使用快照中的前一交易日收盘价作为
        日内稳定锚点，避免实时价变化或实时价与盘后价切换时反复生成不同的价位簇。
        """
        normalized = symbol(stock_symbol)
        quote = database.latest_quote(normalized) or {}
        cached_history = database.latest_history(normalized) or {}
        quote = _align_quote_with_history(quote, cached_history, settings.history_max_age_seconds)
        # 价位接口使用跨期限链；选中的远期期限若不在 45 天窗口内，额外并入，避免切换期限后期权因子消失。
        profile = database.latest_chains(normalized, horizon_days=45)
        option_rows = list(profile.get("data") or [])
        selected_chain: dict[str, Any] | None = None
        # 选中期限已经在 45 天窗口时直接复用窗口查询结果，避免再次读取同一批次。
        # 远期期限不在窗口内时才额外读取，保持切换远期期限后仍能参与合成。
        # 选中日期可能来自旧页面 URL；过期合约不再补读，避免切换旧日期重新拉起无效链。
        try:
            expiration_is_active = datetime.fromisoformat(expiration).date() >= market_today()
        except ValueError:
            expiration_is_active = False
        if expiration_is_active and expiration not in profile.get("expirations", []):
            selected_chain = database.latest_chain(normalized, expiration)
            seen = {(str(row.get("expiration")), str(row.get("contract_symbol"))) for row in option_rows}
            for row in selected_chain.get("data") or []:
                key = (str(row.get("expiration")), str(row.get("contract_symbol")))
                if key not in seen:
                    option_rows.append(row)
                    seen.add(key)
        option_expirations = sorted({str(row.get("expiration")) for row in option_rows if row.get("expiration")})
        fetched_values = [value for value in (profile.get("fetched_at"), (selected_chain or {}).get("fetched_at")) if value]

        resolved_spot = spot if spot is not None else quote.get("price")
        candidate_spot = quote.get("previous_close")
        try:
            if candidate_spot is None or float(candidate_spot) <= 0:
                candidate_spot = quote.get("price") or resolved_spot
        except (TypeError, ValueError):
            candidate_spot = resolved_spot

        if raw:
            # raw 模式严格只读 SQLite；极值、趋势和基础价位交给浏览器计算。
            # 这里不触发日线、Beta、财报回源，也不执行 build_levels，避免切换新
            # 标的时和 Gamma/期权刷新叠加成一次重量级请求。
            cached_extremes = database.latest_extremes(normalized) or {}
            cached_beta = database.latest_beta(normalized) or {}
            cached_earnings = database.latest_earnings(normalized) or {}
            raw_bars = list(cached_history.get("bars") or [])
            earnings_dates = cached_earnings.get("dates")
            # 浏览器首屏只需要选中期限及其最近几期；完整 45 天窗口继续由 Gamma
            # 后台任务按需加载，避免 raw 响应和浏览器解析一次性膨胀。
            available_expirations = sorted({str(row.get("expiration")) for row in option_rows if row.get("expiration")})
            try:
                selected_index = available_expirations.index(expiration)
            except ValueError:
                selected_index = 0
            start = max(0, min(selected_index, len(available_expirations) - raw_expirations)) if available_expirations else 0
            scoped_expirations = set(available_expirations[start:start + raw_expirations])
            scoped_expirations.add(expiration)
            raw_options = [
                {
                    "contract_symbol": row.get("contract_symbol"),
                    "expiration": row.get("expiration"),
                    "contract_type": row.get("contract_type"),
                    "strike": row.get("strike"),
                    "last_price": row.get("last_price"),
                    "bid": row.get("bid"),
                    "ask": row.get("ask"),
                    "volume": row.get("volume"),
                    "open_interest": row.get("open_interest"),
                    "implied_volatility": row.get("implied_volatility"),
                    "gamma": row.get("gamma"),
                }
                for row in option_rows
                if str(row.get("expiration")) in scoped_expirations
            ]
            # raw 也返回按报价反解的到期日模型 IV，浏览器只负责轻量排序与展示。
            raw_iv_model = annotate_model_greeks(raw_options, candidate_spot or resolved_spot)
            for row in raw_options:
                source = (raw_iv_model.get(str(row.get("expiration"))) or {}).get("source")
                if source:
                    row["model_iv_source"] = source
            scoped_expirations = sorted({str(row.get("expiration")) for row in raw_options if row.get("expiration")})
            return {
                "raw": True,
                "symbol": normalized,
                "expiration": expiration,
                "spot": resolved_spot,
                "candidate_spot": candidate_spot,
                "chain_fetched_at": max(fetched_values) if fetched_values else None,
                "options_fetched_at": max(fetched_values) if fetched_values else None,
                "options_expirations": scoped_expirations,
                "options_window_expirations": option_expirations,
                "options_horizon_days": 45,
                # 原始期权行只在 raw=true 时返回，供浏览器按多期限重新聚合；默认综合
                # 接口仍只返回压缩后的价位，避免普通页面响应体变大。
                "options": raw_options,
                "iv_model": raw_iv_model,
                "generated_at": iso(),
                "bars": raw_bars,
                "history": {
                    "bars": len(raw_bars),
                    "from": raw_bars[0]["date"] if raw_bars else None,
                    "to": raw_bars[-1]["date"] if raw_bars else None,
                    "fetched_at": cached_history.get("fetched_at"),
                    "source": "sqlite" if raw_bars else "none",
                    "warning": None if raw_bars else "本地没有日线缓存",
                    "extremes_fetched_at": cached_extremes.get("fetched_at"),
                    "extremes_source": "sqlite" if cached_extremes.get("extremes") else "none",
                    "extremes_warning": None if cached_extremes.get("extremes") else "本地没有极值缓存",
                },
                "extremes": cached_extremes.get("extremes"),
                "beta": cached_beta.get("beta"),
                "beta_meta": {
                    "fetched_at": cached_beta.get("fetched_at"),
                    "source": "sqlite" if cached_beta.get("beta") else "none",
                    "warning": None if cached_beta.get("beta") else "本地没有 Beta 缓存",
                },
                "trend_market": trend_market_data(raw_bars, quote),
                "earnings": summarize_earnings(
                    earnings_dates if isinstance(earnings_dates, list) else None,
                    market_today(),
                ),
            }

        history_payload = history.bars(normalized)
        # 非 raw 模式的日线可能是本次请求刚回源得到的；前面计算 candidate_spot 时
        # 还只有 SQLite quote，必须在这里重新按同一日线口径校准，否则 levels 的
        # trend_market 是正确昨收，但 candidate_spot 仍会保留旧行情快照的昨收。
        quote = _align_quote_with_history(quote, history_payload, settings.history_max_age_seconds)
        candidate_spot = quote.get("previous_close")
        try:
            if candidate_spot is None or float(candidate_spot) <= 0:
                candidate_spot = quote.get("price") or resolved_spot
        except (TypeError, ValueError):
            candidate_spot = resolved_spot
        # 极值和 Beta 使用不同缓存/锁；并行读取可以缩短首次加载等待。Beta 复用已经取回的两年日线，
        # 避免同一请求再次向行情源请求同一份标的数据。财报日期同样并行，失败不能拖垮价位接口。
        earnings_payload: dict[str, Any] = {
            "dates": [],
            "fetched_at": None,
            "source": "none",
            "warning": "财报日期读取失败",
        }

        def load_level_inputs() -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
            extremes_value = history.extremes(normalized)
            beta_value = history.beta(normalized, history_payload.get("bars") or None)
            earnings_value = {
                "dates": [],
                "fetched_at": None,
                "source": "none",
                "warning": "财报日期读取失败",
            }
            try:
                loaded_earnings = history.earnings(normalized)
            except Exception as exc:
                earnings_value["warning"] = str(exc)
            else:
                if isinstance(loaded_earnings, dict):
                    earnings_value = loaded_earnings
            return extremes_value, beta_value, earnings_value

        # 三个日线任务各自都会进重任务闸门。低内存时串行，避免线程池把三份结果同时堆在内存里。
        if settings.low_memory:
            extremes_payload, beta_payload, earnings_payload = load_level_inputs()
        else:
            with ThreadPoolExecutor(max_workers=3, thread_name_prefix="levels-input") as executor:
                extremes_future = executor.submit(history.extremes, normalized)
                beta_future = executor.submit(history.beta, normalized, history_payload.get("bars") or None)
                earnings_future = executor.submit(history.earnings, normalized)
                extremes_payload = extremes_future.result()
                beta_payload = beta_future.result()
                try:
                    loaded_earnings = earnings_future.result()
                except Exception as exc:
                    earnings_payload["warning"] = str(exc)
                else:
                    if isinstance(loaded_earnings, dict):
                        earnings_payload = loaded_earnings
        # 候选池使用昨收作为日内稳定锚点；最新价只负责当前侧别、距离和触及概率。
        # 这样盘中价格小幅波动时不会反复重建相邻价位簇，昨收缺失时才回退最新价。
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
                # 趋势通道增加止损价，升级缓存命名空间以避开旧结果结构。
                "levels-v12",
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
        if settings.low_memory:
            del option_rows
            release_memory()
        bars = list(history_payload.get("bars") or [])
        # 窗口内外按本次请求的美东日期判断，挂在缓存结果之后，避免把财报写进 levels 缓存。
        earnings_dates = earnings_payload.get("dates")
        earnings = summarize_earnings(
            earnings_dates if isinstance(earnings_dates, list) else None,
            market_today(),
        )
        earnings["fetched_at"] = earnings_payload.get("fetched_at")
        earnings["source"] = earnings_payload.get("source")
        earnings["warning"] = earnings_payload.get("warning")
        return {
            "symbol": normalized,
            "expiration": expiration,
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
            "earnings": earnings,
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
