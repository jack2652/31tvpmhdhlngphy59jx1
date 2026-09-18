"""HTTP API 路由。"""

from __future__ import annotations

import secrets
from datetime import datetime
from pathlib import Path
from typing import Any

from fastapi import APIRouter, FastAPI, HTTPException, Query, Request
from fastapi.responses import FileResponse, HTMLResponse, JSONResponse

from app.config import Settings
from app.db import Database, iso, parse_sessions
from app.gamma import annotate_model_greeks, find_zero_gamma
from app.levels import build_levels
from app.providers.market import ProviderError, MarketDataProvider
from app.services.history import HistoryService
from app.services.snapshots import SnapshotService, active_expirations


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


def create_router(database: Database, snapshots: SnapshotService, provider: MarketDataProvider, settings: Settings) -> APIRouter:
    router = APIRouter(prefix="/api")
    history = HistoryService(
        database, provider, settings.history_max_age_seconds, settings.extremes_max_age_seconds
    )

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
            iv_model = annotate_model_greeks(cached["data"], quote.get("price"))
            return {"symbol": normalized, "expiration": expiration, "iv_model": iv_model, **cached, "source": "sqlite"}
        return {"symbol": normalized, "expiration": expiration, "fetched_at": None, "data": [], "source": "pending"}

    @router.get("/gamma/{stock_symbol}")
    def gamma_profile(
        stock_symbol: str,
        refresh: bool = Query(default=False),
        horizon_days: int = Query(default=45, ge=1, le=365),
    ) -> dict[str, Any]:
        """返回近期期限的完整链，供 Zero Gamma/Gamma Flip 曲线使用。"""
        normalized = symbol(stock_symbol)
        refresh_result: dict[str, Any] | None = None
        if refresh:
            try:
                refresh_result = snapshots.refresh_window(normalized, horizon_days)
            except (ProviderError, RuntimeError, ValueError) as exc:
                refresh_result = {
                    "symbol": normalized,
                    "horizon_days": horizon_days,
                    "expirations": [],
                    "results": [],
                    "errors": [str(exc)],
                }
        profile = database.latest_chains(normalized, horizon_days)
        quote = database.latest_quote(normalized) or {}
        rows = profile.get("data") or []
        iv_model = annotate_model_greeks(rows, quote.get("price"))
        zero_gamma = find_zero_gamma(rows, quote.get("price"))
        return {
            "symbol": normalized,
            "iv_model": iv_model,
            **profile,
            "source": "sqlite",
            "refresh": refresh_result,
            "zero_gamma": zero_gamma,
        }

    @router.post("/refresh/{stock_symbol}")
    def refresh(stock_symbol: str, expiration: str | None = Query(default=None, pattern=r"^\d{4}-\d{2}-\d{2}$"), max_age: int = Query(default=60, ge=0, le=3600)) -> dict[str, Any]:
        """刷新一次快照；本地快照在 max_age 秒内时直接复用（skipped=True），不再请求上游接口。"""
        normalized = symbol(stock_symbol)
        try:
            return snapshots.refresh(normalized, expiration, max_age_seconds=max_age)
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
        """压力位/支撑位：斐波那契回撤 + 筹码密集 + 承接位 + 所选到期日期权持仓综合。

        `spot` 是前端传入的基准价（默认取盘后价，避免用盘前冲高/盘中回落的假突破当基准）；
        缺省时退回快照里的常规价。
        """
        normalized = symbol(stock_symbol)
        quote = database.latest_quote(normalized) or {}
        chain = database.latest_chain(normalized, expiration)
        history_payload = history.bars(normalized)
        extremes_payload = history.extremes(normalized)
        computed = build_levels(
            history_payload.get("bars") or [],
            chain.get("data") or [],
            spot if spot is not None else quote.get("price"),
            expiration,
            extremes_payload.get("extremes"),
        )
        bars = list(history_payload.get("bars") or [])
        return {
            "symbol": normalized,
            "chain_fetched_at": chain.get("fetched_at"),
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
