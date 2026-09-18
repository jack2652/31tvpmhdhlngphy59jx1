"""快照采集服务。"""

from __future__ import annotations

import logging
import threading
from datetime import date, datetime, timedelta, timezone
from typing import Any
from zoneinfo import ZoneInfo

from app.db import Database, iso, parse_sessions
from app.providers.yahoo import ProviderError, YahooProvider

logger = logging.getLogger(__name__)

# 美股期权到期以美国东部时间为准，过滤到期日时使用美东当天日期。
MARKET_TIMEZONE = ZoneInfo("America/New_York")

# 跨期限 Gamma 窗口的单到期日新鲜期（秒）：窗口刷新很慢，短期内的重复请求直接跳过。
WINDOW_FRESH_SECONDS = 120


def market_today() -> date:
    """返回美东当天日期。"""
    return datetime.now(MARKET_TIMEZONE).date()


def snapshot_age_seconds(fetched_at: str | None) -> float | None:
    """快照年龄（秒）：时间戳缺失或无法解析返回 None，时钟偏差导致的负值按 0 处理。"""
    if not fetched_at:
        return None
    try:
        moment = datetime.fromisoformat(fetched_at)
    except ValueError:
        return None
    if moment.tzinfo is None:
        moment = moment.replace(tzinfo=timezone.utc)
    return max((datetime.now(timezone.utc) - moment).total_seconds(), 0.0)


def active_expirations(values: list[str]) -> list[str]:
    """过滤已经过期的到期日，避免页面停留在无法刷新的历史合约上。"""
    today = market_today()
    return [value for value in values if date.fromisoformat(value) >= today]


class SnapshotService:
    def __init__(self, database: Database, provider: YahooProvider | None = None):
        self.database = database
        self.provider = provider or YahooProvider()
        self._locks: dict[str, threading.Lock] = {}
        self._locks_guard = threading.Lock()

    def _lock_for(self, symbol: str) -> threading.Lock:
        with self._locks_guard:
            return self._locks.setdefault(symbol, threading.Lock())

    def refresh(self, symbol: str, expiration: str | None = None, max_age_seconds: int = 0) -> dict[str, Any]:
        """抓取一次快照。

        max_age_seconds 大于 0 时先读本地快照：仍在新鲜期内直接返回 skipped 结果，不再请求 yfinance，
        避免定时刷新与手动刷新对同一份数据反复打接口。
        """
        normalized = self.provider.normalize_symbol(symbol)
        lock = self._lock_for(normalized)
        if not lock.acquire(blocking=False):
            raise RuntimeError(f"{normalized} 正在刷新，请稍后再试")
        try:
            if max_age_seconds > 0:
                cached = self.recent_snapshot(normalized, expiration, max_age_seconds)
                if cached is not None:
                    logger.debug(
                        "跳过刷新 %s %s：本地快照仍有 %.1f 秒新鲜度",
                        normalized,
                        cached.get("expiration") or "(仅现货)",
                        cached["age_seconds"],
                    )
                    return cached
            return self._fetch_and_store(normalized, expiration)
        finally:
            lock.release()

    def recent_snapshot(self, symbol: str, expiration: str | None, max_age_seconds: int) -> dict[str, Any] | None:
        """本地快照仍在新鲜期内时返回可复用的结果，否则返回 None。"""
        cached_expirations = active_expirations(self.database.latest_expirations(symbol))
        target = expiration or (cached_expirations[0] if cached_expirations else None)
        if not target:
            # 完全没有缓存的到期日（例如压根不支持期权的标的）：退化为按现货快照判断新鲜度，
            # 否则每次定时刷新都会重新打一次 yfinance。
            quote = self.database.latest_quote(symbol) or {}
            age = snapshot_age_seconds(quote.get("fetched_at"))
            if age is None or age >= max_age_seconds:
                return None
            return {
                "symbol": symbol,
                "expiration": None,
                "fetched_at": quote["fetched_at"],
                "rows": 0,
                "skipped": True,
                "quote_only": True,
                "age_seconds": round(age, 1),
            }
        cached = self.database.latest_chain(symbol, target)
        age = snapshot_age_seconds(cached.get("fetched_at"))
        if age is None or age >= max_age_seconds:
            return None
        return {
            "symbol": symbol,
            "expiration": target,
            "fetched_at": cached["fetched_at"],
            "rows": 0,
            "skipped": True,
            "age_seconds": round(age, 1),
        }

    def _fetch_and_store(self, normalized: str, expiration: str | None) -> dict[str, Any]:
        """请求 yfinance 并写入 SQLite，失败时记录刷新日志后抛出。"""
        run_id = self.database.start_run(normalized)
        try:
            expirations = self.provider.expirations(normalized)
            available = active_expirations(expirations)
            # 有些标的（例如 SPCX 这类没有挂牌期权合约的标的）根本没有到期日：此时退化成只抓现货，
            # 现货卡片照常可用，页面其余面板提示没有期权数据，而不是整页无数据。
            if not available:
                quote = self._with_cached_sessions(normalized, self.provider.quote(normalized))
                fetched_at = iso()
                self.database.write_snapshot(quote, [], fetched_at)
                self.database.finish_run(run_id, "success", 0)
                logger.info("%s 没有可用的期权到期日，本次只记录现货快照", normalized)
                return {"symbol": normalized, "expiration": None, "fetched_at": fetched_at, "rows": 0, "quote_only": True}
            selected = expiration or available[0]
            if selected not in expirations:
                raise ValueError(f"{normalized} 没有到期日 {selected}（最新可用: {available[0]}）")
            if selected not in available:
                raise ValueError(f"{normalized} 的到期日 {selected} 已过期，最新可用到期日: {available[0]}")
            quote, rows, fetched_at = self.provider.fetch(normalized, selected)
            written = self.database.write_snapshot(self._with_cached_sessions(normalized, quote), rows, fetched_at)
            self.database.finish_run(run_id, "success", written)
            return {"symbol": normalized, "expiration": selected, "fetched_at": fetched_at, "rows": written}
        except Exception as exc:
            self.database.finish_run(run_id, "failed", 0, str(exc))
            if isinstance(exc, (ValueError, ProviderError, RuntimeError)):
                raise
            raise ProviderError(str(exc)) from exc

    def _with_cached_sessions(self, normalized: str, quote: dict[str, Any]) -> dict[str, Any]:
        """盘前盘后抓取失败（例如被数据源限流）时沿用上一次快照的时段数据，避免卡片闪空。"""
        if not quote.get("sessions"):
            cached_quote = self.database.latest_quote(normalized) or {}
            quote["sessions"] = parse_sessions(cached_quote.get("sessions_json"))
        return quote

    def refresh_default(self, symbols: tuple[str, ...]) -> list[dict[str, Any]]:
        results = []
        for symbol in symbols:
            try:
                results.append(self.refresh(symbol))
            except Exception as exc:
                logger.warning("定时刷新 %s 失败: %s", symbol, exc)
                continue
        return results

    def refresh_window(self, symbol: str, horizon_days: int = 45) -> dict[str, Any]:
        """刷新近期期限，供跨到期日 Gamma 曲线使用。"""
        normalized = self.provider.normalize_symbol(symbol)
        expirations = self.provider.expirations(normalized)
        today = market_today()
        cutoff = today + timedelta(days=horizon_days)
        selected = [
            value for value in expirations
            if today <= date.fromisoformat(value) <= cutoff
        ]
        results: list[dict[str, Any]] = []
        errors: list[str] = []
        for expiration in selected:
            try:
                cached = self.database.latest_chain(normalized, expiration)
                age = snapshot_age_seconds(cached.get("fetched_at"))
                if age is not None and age < WINDOW_FRESH_SECONDS:
                    continue
                results.append(self.refresh(normalized, expiration))
            except Exception as exc:
                errors.append(f"{expiration}: {exc}")
                logger.warning("刷新 %s %s 失败: %s", normalized, expiration, exc)
        return {
            "symbol": normalized,
            "horizon_days": horizon_days,
            "expirations": selected,
            "results": results,
            "errors": errors,
        }
