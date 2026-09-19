"""日线历史行情服务：SQLite 缓存 + 新鲜期控制。

斐波那契回撤、筹码分布和承接位都需要日线 OHLCV；上游的最后一根日线在盘中也会变化，
因此按 max_age_seconds（默认 1 小时）控制回源频率，新鲜期内直接复用 SQLite。
回源失败时退回本地旧缓存并标注 warning，保证压力位/支撑位面板仍能给出期权口径的结果。
"""

from __future__ import annotations

import logging
import math
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
# Beta 采用两年日线收益率，并按天缓存结果。
BETA_PERIOD = "2y"
BETA_MAX_AGE_SECONDS = 86400
BETA_BENCHMARK = "^GSPC"


def calculate_beta(stock_bars: list[dict[str, Any]], benchmark_bars: list[dict[str, Any]]) -> dict[str, Any] | None:
    """按共同交易日的日收益率计算标的相对标普 500 的 Beta。"""
    def close_map(bars: list[dict[str, Any]]) -> dict[str, float]:
        values: dict[str, float] = {}
        for bar in bars:
            try:
                day = str(bar.get("date"))[:10]
                close = float(bar.get("close"))
            except (TypeError, ValueError):
                continue
            if day and math.isfinite(close) and close > 0:
                values[day] = close
        return values

    stock = close_map(stock_bars)
    benchmark = close_map(benchmark_bars)
    days = sorted(set(stock) & set(benchmark))
    if len(days) < 31:
        return None
    stock_returns: list[float] = []
    benchmark_returns: list[float] = []
    for previous_day, current_day in zip(days, days[1:]):
        stock_previous = stock[previous_day]
        benchmark_previous = benchmark[previous_day]
        stock_current = stock[current_day]
        benchmark_current = benchmark[current_day]
        if stock_previous <= 0 or benchmark_previous <= 0:
            continue
        stock_returns.append(stock_current / stock_previous - 1.0)
        benchmark_returns.append(benchmark_current / benchmark_previous - 1.0)
    if len(stock_returns) < 30:
        return None
    stock_mean = sum(stock_returns) / len(stock_returns)
    benchmark_mean = sum(benchmark_returns) / len(benchmark_returns)
    covariance = sum(
        (stock_value - stock_mean) * (benchmark_value - benchmark_mean)
        for stock_value, benchmark_value in zip(stock_returns, benchmark_returns)
    )
    variance = sum((value - benchmark_mean) ** 2 for value in benchmark_returns)
    if variance <= 0:
        return None
    return {
        "value": round(covariance / variance, 4),
        "benchmark": "标普500",
        "benchmark_symbol": BETA_BENCHMARK,
        "period": BETA_PERIOD,
        "period_label": "2年",
        "observations": len(stock_returns),
    }


class HistoryService:
    def __init__(
        self,
        database: Database,
        provider: MarketDataProvider,
        max_age_seconds: int = 3600,
        extremes_max_age_seconds: int = 86400,
        beta_max_age_seconds: int = BETA_MAX_AGE_SECONDS,
    ):
        self.database = database
        self.provider = provider
        self.max_age_seconds = max(max_age_seconds, 0)
        self.extremes_max_age_seconds = max(extremes_max_age_seconds, 0)
        self.beta_max_age_seconds = max(beta_max_age_seconds, 0)
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

    def beta(self, symbol: str) -> dict[str, Any]:
        """返回相对标普 500 的两年 Beta；回源失败时退回日缓存。"""
        normalized = self.provider.normalize_symbol(symbol)
        cached = self.database.latest_beta(normalized)
        if cached and self._is_fresh(cached.get("fetched_at"), self.beta_max_age_seconds):
            return self._beta_result(normalized, cached, "sqlite", None)
        with self._lock_for(f"{normalized}:beta"):
            cached = self.database.latest_beta(normalized)
            if cached and self._is_fresh(cached.get("fetched_at"), self.beta_max_age_seconds):
                return self._beta_result(normalized, cached, "sqlite", None)
            benchmark_loader = getattr(self.provider, "benchmark_history", None)
            try:
                if not callable(benchmark_loader):
                    raise ProviderError("行情源不支持标普500基准历史")
                computed = calculate_beta(
                    self.provider.history(normalized, period=BETA_PERIOD),
                    benchmark_loader(BETA_BENCHMARK, BETA_PERIOD),
                )
                if computed is None:
                    raise ProviderError("两年共同交易日不足，无法计算 Beta")
            except ProviderError as exc:
                logger.warning("获取 %s Beta 失败: %s", normalized, exc)
                return self._beta_result(normalized, cached, "sqlite" if cached else "none", str(exc))
            fetched_at = iso()
            self.database.write_beta(normalized, computed, fetched_at)
            return self._beta_result(normalized, {"beta": computed, "fetched_at": fetched_at}, "upstream", None)

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
    def _beta_result(symbol: str, cached: dict[str, Any] | None, source: str, warning: str | None) -> dict[str, Any]:
        return {
            "symbol": symbol,
            "beta": (cached or {}).get("beta"),
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
