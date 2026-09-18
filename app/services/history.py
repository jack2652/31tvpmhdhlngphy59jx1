"""日线历史行情服务：SQLite 缓存 + 新鲜期控制。

斐波那契回撤、筹码分布和承接位都需要日线 OHLCV；上游的最后一根日线在盘中也会变化，
因此按 max_age_seconds（默认 1 小时）控制回源频率，新鲜期内直接复用 SQLite。
回源失败时退回本地旧缓存并标注 warning，保证压力位/支撑位面板仍能给出期权口径的结果。
"""

from __future__ import annotations

import logging
import threading
from typing import Any

from app.db import Database, iso
from app.levels import price_extremes
from app.providers.market import ProviderError, MarketDataProvider
from app.services.snapshots import snapshot_age_seconds

logger = logging.getLogger(__name__)

# 回看周期：6 个月的日线足以覆盖摆动高低点、筹码分布和近期承接位。
HISTORY_PERIOD = "6mo"
# 极值回看周期：52 周与历史高低点需要全量日线，因此单独抓一次并缓存一天。
EXTREMES_PERIOD = "max"


class HistoryService:
    def __init__(
        self,
        database: Database,
        provider: MarketDataProvider,
        max_age_seconds: int = 3600,
        extremes_max_age_seconds: int = 86400,
    ):
        self.database = database
        self.provider = provider
        self.max_age_seconds = max(max_age_seconds, 0)
        self.extremes_max_age_seconds = max(extremes_max_age_seconds, 0)
        self._locks: dict[str, threading.Lock] = {}
        self._locks_guard = threading.Lock()

    def _lock_for(self, symbol: str) -> threading.Lock:
        with self._locks_guard:
            return self._locks.setdefault(symbol, threading.Lock())

    def _is_fresh(self, fetched_at: str | None, max_age_seconds: int | None = None) -> bool:
        age = snapshot_age_seconds(fetched_at)
        return age is not None and age < (self.max_age_seconds if max_age_seconds is None else max_age_seconds)

    def bars(self, symbol: str) -> dict[str, Any]:
        """返回 {symbol, bars, fetched_at, source, warning}；同一标的的并发请求只回源一次。"""
        normalized = self.provider.normalize_symbol(symbol)
        cached = self.database.latest_history(normalized)
        if cached and self._is_fresh(cached.get("fetched_at")):
            return self._result(normalized, cached, "sqlite", None)
        with self._lock_for(normalized):
            # 等锁期间可能已有其他请求完成了回源，进入临界区后再确认一次。
            cached = self.database.latest_history(normalized)
            if cached and self._is_fresh(cached.get("fetched_at")):
                return self._result(normalized, cached, "sqlite", None)
            try:
                bars = self.provider.history(normalized, period=HISTORY_PERIOD)
            except ProviderError as exc:
                logger.warning("获取 %s 日线历史失败: %s", normalized, exc)
                return self._result(normalized, cached, "sqlite" if cached else "none", str(exc))
            fetched_at = iso()
            self.database.write_history(normalized, bars, fetched_at)
            return self._result(normalized, {"bars": bars, "fetched_at": fetched_at}, "upstream", None)

    def extremes(self, symbol: str) -> dict[str, Any]:
        """返回 {symbol, extremes, fetched_at, source, warning}：52 周与历史最高/最低价。

        极值取自全量日线（period=max），默认一天最多回源一次；回源失败时退回本地旧缓存并标注 warning，
        页面因此仍能显示上一次的结果或占位符，不影响压力位/支撑位。
        """
        normalized = self.provider.normalize_symbol(symbol)
        cached = self.database.latest_extremes(normalized)
        if cached and self._is_fresh(cached.get("fetched_at"), self.extremes_max_age_seconds):
            return self._extremes_result(normalized, cached, "sqlite", None)
        # 与日线历史分开加锁：两者回源周期不同，互不阻塞。
        with self._lock_for(f"{normalized}:extremes"):
            cached = self.database.latest_extremes(normalized)
            if cached and self._is_fresh(cached.get("fetched_at"), self.extremes_max_age_seconds):
                return self._extremes_result(normalized, cached, "sqlite", None)
            try:
                computed = price_extremes(self.provider.history(normalized, period=EXTREMES_PERIOD))
                if computed is None:
                    raise ProviderError(f"{normalized} 的全量日线没有可用的高低价")
            except ProviderError as exc:
                logger.warning("获取 %s 日线极值失败: %s", normalized, exc)
                return self._extremes_result(normalized, cached, "sqlite" if cached else "none", str(exc))
            fetched_at = iso()
            self.database.write_extremes(normalized, computed, fetched_at)
            return self._extremes_result(normalized, {"extremes": computed, "fetched_at": fetched_at}, "upstream", None)

    @staticmethod
    def _extremes_result(symbol: str, cached: dict[str, Any] | None, source: str, warning: str | None) -> dict[str, Any]:
        return {
            "symbol": symbol,
            "extremes": (cached or {}).get("extremes"),
            "fetched_at": (cached or {}).get("fetched_at"),
            "source": source,
            "warning": warning,
        }

    @staticmethod
    def _result(symbol: str, cached: dict[str, Any] | None, source: str, warning: str | None) -> dict[str, Any]:
        bars = list((cached or {}).get("bars") or [])
        return {
            "symbol": symbol,
            "bars": bars,
            "fetched_at": (cached or {}).get("fetched_at"),
            "source": source,
            "warning": warning,
        }
