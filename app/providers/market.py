"""上游行情接口的期权链快照适配器。

本模块是应用里唯一与外部行情 SDK 打交道的地方：其余代码只依赖这里暴露的
`MarketDataProvider` 接口（标的、行情、期权链、日线），因此更换上游实现时
不需要改动业务层。
"""

from __future__ import annotations

import logging
import json
import math
import re
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from datetime import date, datetime, timedelta, timezone
from typing import Any, Callable
from urllib.parse import urlencode
from urllib.error import HTTPError, URLError
from urllib.request import ProxyHandler, Request, build_opener
from zoneinfo import ZoneInfo

from app.services.concurrency import UpstreamGate
from app.services.market_calendar import is_regular_session, is_trading_day, localize, regular_session_bounds
from app.providers.valuation_config import VALUATION_CONFIG

from app.providers.market_common import (
    FAIR_VALUE_SOURCE, MARKET_TIMEZONE, SESSION_FRESH_SECONDS, SESSION_STATES,
    UPSTREAM_REQUEST_TIMEOUT_SECONDS, YAHOO_TIMESERIES_URL, MarketRegime, ProviderError,
    _earnings_new_york_date, _earnings_reported, _is_transient_upstream_error,
    _positive_price, _read_fast_info, _record_upstream_error, _regular_close_map, _valuation_debug,
    choose_pre_session_close, choose_previous_close, current_session_state, is_session_trading_day,
    load_upstream_sdk, normalize_row, parse_earnings_dates, prior_session_regular_close,
    previous_regular_close, regular_session_open, safe_value, session_of, session_trading_day,
    summarize_extended_hours,
)
from app.providers.market_valuation import MarketValuationMixin

logger = logging.getLogger(__name__)

# 美股时段划分（美东时间）：盘前 04:00–09:30、盘中 09:30–16:00、盘后 16:00–20:00、夜盘 20:00–04:00。
# 最近一根 K 线超过该秒数就认为已经离开该时段，改按美东时钟判断。
# 现货附加分钟线只是展示盘前/盘后摘要。给上游 SDK 一个有限等待时间，避免
# yfinance 网络异常把请求线程长期挂住，进而叠加期权链和历史任务。

# 盘前主行情通常把上一交易日的正式收盘价放在 last_price，分钟线最后一根
# 可能只是 15:59 的成交价。两者在这个范围内视为同一收盘，优先正式收盘。
PREVIOUS_CLOSE_AUCTION_AGREEMENT = 0.001

class MarketDataProvider(MarketValuationMixin):
    _fair_value_executor = MarketValuationMixin._fair_value_executor

    def _fetch_yahoo_timeseries(self, symbol: str) -> dict[str, Any]:
        """读取 Yahoo 网页财务页的公开时序接口；失败只返回空结果。"""
        metrics = (
            "annualFreeCashFlow,annualOperatingCashFlow,annualCapitalExpenditure,"
            "annualDilutedAverageShares,annualDilutedEPS,annualTotalDebt,"
            "annualCashCashEquivalentsAndShortTermInvestments,trailingDilutedEPS,"
            "annualTotalRevenue,annualOperatingIncome,annualNetIncome,"
            "annualDepreciationAndAmortization,annualChangeInWorkingCapital,"
            "annualRepurchaseOfCapitalStock,annualStockholdersEquity,quarterlyTotalRevenue,quarterlyDilutedEPS"
        )
        query = urlencode({
            "symbol": symbol,
            "type": metrics,
            "period1": 1262304000,
            "period2": int(time.time()) + 86400,
        })
        url = f"{YAHOO_TIMESERIES_URL.format(symbol=symbol)}?{query}"
        request = Request(url, headers={"User-Agent": "Mozilla/5.0"})
        opener = build_opener(ProxyHandler({"http": self.proxy, "https": self.proxy}) if self.proxy else ProxyHandler())
        try:
            with self.upstream_gate.slot():
                with opener.open(request, timeout=UPSTREAM_REQUEST_TIMEOUT_SECONDS) as response:
                    payload = json.loads(response.read())
            result = ((payload.get("timeseries") or {}).get("result") or [])
            merged: dict[str, list[dict[str, Any]]] = {}
            for item in result:
                if not isinstance(item, dict):
                    continue
                types = ((item.get("meta") or {}).get("type") or [])
                metric = types[0] if types else None
                entries = item.get(metric) if metric else None
                if not metric or not isinstance(entries, list):
                    continue
                timestamps = item.get("timestamp")
                if isinstance(timestamps, list) and len(timestamps) == len(entries):
                    paired: list[tuple[int, int, dict[str, Any]]] = []
                    for index, entry in enumerate(entries):
                        if not isinstance(entry, dict):
                            continue
                        try:
                            timestamp = int(timestamps[index])
                        except (TypeError, ValueError):
                            timestamp = index
                        paired.append((timestamp, index, entry))
                    entries = [entry for _, _, entry in sorted(paired, key=lambda pair: (pair[0], pair[1]), reverse=True)]
                merged[metric] = entries
            return merged
        except Exception as exc:  # noqa: BLE001 - 备用数据失败不能影响行情
            logger.debug("Yahoo 财务时序备用接口暂不可用（%s）：%s", symbol, exc)
            return {"__status__": "retry" if _is_transient_upstream_error(exc) else "unavailable"}

    # 对外暴露的数据来源标识：只表示「来自上游接口」，不暴露具体供应商
    name = "upstream"

    def __init__(
        self,
        ticker_factory: Any | None = None,
        proxy: str | None = None,
        upstream_gate: UpstreamGate | None = None,
        analysis_cache: Any | None = None,
    ):
        self.proxy = proxy.strip() if proxy and proxy.strip() else None
        self.upstream_gate = upstream_gate or UpstreamGate()
        # 分析师目标价更新频率远低于报价；进程内短缓存避免每分钟快照都再发一次分析接口请求。
        self._fair_value_cache: dict[str, tuple[float, dict[str, Any]]] = {}
        self._fair_value_inflight: set[str] = set()
        self._fair_value_lock = threading.Lock()
        # 基准指数动量在短时间内对所有标的一致；15 分钟缓存避免批量估值重复请求 ^GSPC。
        self._benchmark_cache: dict[str, tuple[float, list[dict[str, Any]]]] = {}
        self._benchmark_cache_lock = threading.Lock()
        # SQLite 作为可选的跨 worker 共享缓存；测试和轻量调用没有传入时仍只用进程内缓存。
        self.analysis_cache = analysis_cache
        self._uses_builtin_factory = ticker_factory is None
        if ticker_factory is None:
            sdk = load_upstream_sdk()
            if self.proxy:
                # 新版 SDK 统一由全局配置对象管理网络代理（旧的 set_config 已弃用）
                sdk.config.network.proxy = self.proxy
            ticker_factory = sdk.Ticker
        self.ticker_factory = ticker_factory

    @staticmethod
    def normalize_symbol(symbol: str) -> str:
        value = symbol.strip().upper()
        if not value or len(value) > 10 or re.fullmatch(r"[A-Z0-9][A-Z0-9.-]*", value) is None:
            raise ValueError("标的代码格式无效")
        return value

    def _ticker(self, symbol: str) -> Any:
        normalized = self.normalize_symbol(symbol)
        return self._ticker_raw(normalized)

    def _ticker_raw(self, symbol: str) -> Any:
        """创建任意上游代码的行情对象；Beta 需要访问 ^GSPC 这类指数代码。"""
        try:
            if self.proxy and not self._uses_builtin_factory:
                return self.ticker_factory(symbol, proxy=self.proxy)
            return self.ticker_factory(symbol)
        except Exception as exc:
            raise ProviderError(f"无法创建 {symbol} 行情对象: {exc}") from exc

    def expirations(self, symbol: str) -> list[str]:
        ticker = self._ticker(symbol)
        try:
            with self.upstream_gate.slot():
                values = list(ticker.options or ())
        except Exception as exc:
            raise ProviderError(f"获取 {symbol.upper()} 到期日失败: {exc}") from exc
        return [str(value) for value in values]

    def quote(self, symbol: str) -> dict[str, Any]:
        normalized = self.normalize_symbol(symbol)
        ticker = self._ticker(normalized)
        try:
            try:
                with self.upstream_gate.slot():
                    info = ticker.fast_info
            except Exception:
                info = {}
            price = _read_fast_info(info, "last_price")
            previous = _read_fast_info(info, "previous_close")
            today_open = _read_fast_info(info, "open")
            if price is None or previous is None:
                with self.upstream_gate.slot():
                    price, previous, today_open = self._history_quote(ticker, price, previous, today_open)
            extended = self._extended_hours(ticker, normalized)
            # fast_info.market_state 在非交易时段可能沿用上一个状态；分钟线摘要包含时区和交易日历判断，优先采用它。
            market_state = extended["state"] or _read_fast_info(info, "market_state")
            # 未完成日线和 fast_info.open 都会把昨开留在今开上。盘中开始后改用第一根盘中分钟线。
            regular_open = extended.get("regular_open")
            if market_state in {"REGULAR", "POST", "OVERNIGHT"} and regular_open is not None:
                today_open = regular_open
            # 没有 Alpaca 夜盘价时，主行情的 price 可能只是最近一个已完成交易日的收盘，
            # 所以涨跌基准要再往前取一个交易日：例如当前显示周一收盘 132.60，
            # “相对上一个交易日”应使用上周五收盘约 137.10，而不是把周一收盘 132.63
            # 同时当成当前价和基准。盘前摘要的 reference_close 正好记录了这个上一个交易日；
            # 缺失时再回退分钟线识别出的上一交易日收盘和官方 previous_close。
            regular_close = None
            if market_state == "OVERNIGHT":
                regular_close = (extended.get("sessions", {}).get("pre") or {}).get("reference_close")
            if regular_close is None:
                regular_close = extended.get("prior_session_regular_close")
            if regular_close is None:
                regular_close = extended.get("previous_regular_close")
            # 夜盘没有独立现货源时，`pre.reference_close` 是分钟线明确识别出的
            # 上一个交易日收盘；不要再用 fast_info 的近似值（例如 137.04）覆盖它。
            if market_state == "PRE" and regular_close is not None:
                # 盘前没有当日正常盘成交；主行情 price 若与 15:59 分钟线收盘
                # 只差收盘竞价级别的几分钱，优先它作为上一交易日正式收盘。
                previous = choose_pre_session_close(
                    price,
                    regular_close,
                    choose_previous_close(previous, regular_close),
                )
            elif market_state == "OVERNIGHT" and regular_close is not None:
                previous = regular_close
            else:
                previous = choose_previous_close(previous, regular_close)
            # 让前端盘前基准与后端 quote.previous_close 使用同一口径，
            # 不再把分钟线的 132.63 单独传给 quoteReference。
            pre_summary = (extended.get("sessions") or {}).get("pre")
            if market_state == "PRE" and isinstance(pre_summary, dict) and previous not in (None, 0):
                pre_summary["reference_close"] = previous
                pre_price = _positive_price(pre_summary.get("price"))
                if pre_price is not None:
                    pre_summary["change_percent"] = safe_value((pre_price - previous) / previous * 100)
                # 同一份分钟线里的盘后摘要也引用这次正式收盘，避免后续趋势面板
                # 或切换时段时重新暴露 15:59 的 132.63。
                post_summary = (extended.get("sessions") or {}).get("post")
                if isinstance(post_summary, dict):
                    post_summary["reference_close"] = previous
                    post_price = _positive_price(post_summary.get("price"))
                    if post_price is not None:
                        post_summary["change_percent"] = safe_value((post_price - previous) / previous * 100)
            change = None
            if price is not None and previous not in (None, 0):
                change = (price - previous) / previous * 100
            fair_value = self._fair_value(normalized, ticker)
            return {
                "symbol": normalized,
                "price": price,
                "change_percent": safe_value(change),
                "today_open": today_open,
                "previous_close": previous,
                "fair_value": fair_value.get("value"),
                "fair_value_low": fair_value.get("low"),
                "fair_value_high": fair_value.get("high"),
                "fair_value_buy_low": fair_value.get("buy_low"),
                "fair_value_buy_high": fair_value.get("buy_high"),
                "fair_value_source": fair_value.get("source"),
                "fair_value_model": fair_value.get("model_label") or fair_value.get("model"),
                "fair_value_forward_eps": fair_value.get("forward_eps"),
                "fair_value_forward_eps_source": fair_value.get("forward_eps_source"),
                "fair_value_safety_margin": fair_value.get("safety_margin"),
                "fair_value_confidence": fair_value.get("confidence"),
                "fair_value_confidence_score": fair_value.get("confidence_score"),
                "fair_value_interest_coverage": fair_value.get("interest_coverage"),
                "fair_value_regime": fair_value.get("regime"),
                "fair_value_regime_signals": fair_value.get("regime_signals"),
                "fair_value_model_under_regime": fair_value.get("model_under_regime"),
                "fair_value_defensive": fair_value.get("defensive"),
                "fair_value_optimistic": fair_value.get("optimistic"),
                "fair_value_normalized_eps_source": fair_value.get("normalized_eps_source"),
                "fair_value_quarterly_momentum": fair_value.get("quarterly_momentum"),
                "fair_value_historical_valuation_percentiles": fair_value.get("historical_valuation_percentiles"),
                "fair_value_shareholder_total_return_yield": fair_value.get("shareholder_total_return_yield"),
                "fair_value_market_cap_data_quality": fair_value.get("market_cap_data_quality"),
                "fair_value_data_quality_score": fair_value.get("data_quality_score"),
                "fair_value_owner_earnings_maintenance_ratio": fair_value.get("owner_earnings_maintenance_ratio"),
                "fair_value_owner_earnings_ratio_source": fair_value.get("owner_earnings_ratio_source"),
                "fair_value_status": fair_value.get("status", "pending"),
                "fair_value_warning": fair_value.get("warning"),
                "currency": safe_value(info.get("currency")) or "USD",
                "market_state": market_state,
                "sessions": extended["sessions"],
                "provider": self.name,
                "raw": {"last_price": price, "previous_close": previous},
            }
        except Exception as exc:
            raise ProviderError(f"获取 {normalized} 行情失败: {exc}") from exc

    def _fetch_yahoo_timeseries(self, symbol: str) -> dict[str, Any]:
        """读取 Yahoo 网页财务页的公开时序接口；失败只返回空结果。"""
        metrics = (
            "annualFreeCashFlow,annualOperatingCashFlow,annualCapitalExpenditure,"
            "annualDilutedAverageShares,annualDilutedEPS,annualTotalDebt,"
            "annualCashCashEquivalentsAndShortTermInvestments,trailingDilutedEPS,"
            "annualTotalRevenue,annualOperatingIncome,annualNetIncome,"
            "annualDepreciationAndAmortization,annualChangeInWorkingCapital,"
            "annualRepurchaseOfCapitalStock,annualStockholdersEquity,quarterlyTotalRevenue,quarterlyDilutedEPS"
        )
        query = urlencode({
            "symbol": symbol,
            "type": metrics,
            "period1": 1262304000,
            "period2": int(time.time()) + 86400,
        })
        url = f"{YAHOO_TIMESERIES_URL.format(symbol=symbol)}?{query}"
        request = Request(url, headers={"User-Agent": "Mozilla/5.0"})
        opener = build_opener(ProxyHandler({"http": self.proxy, "https": self.proxy}) if self.proxy else ProxyHandler())
        try:
            with self.upstream_gate.slot():
                with opener.open(request, timeout=UPSTREAM_REQUEST_TIMEOUT_SECONDS) as response:
                    payload = json.loads(response.read())
            result = ((payload.get("timeseries") or {}).get("result") or [])
            merged: dict[str, list[dict[str, Any]]] = {}
            for item in result:
                if not isinstance(item, dict):
                    continue
                types = ((item.get("meta") or {}).get("type") or [])
                metric = types[0] if types else None
                entries = item.get(metric) if metric else None
                if not metric or not isinstance(entries, list):
                    continue
                timestamps = item.get("timestamp")
                if isinstance(timestamps, list) and len(timestamps) == len(entries):
                    paired: list[tuple[int, int, dict[str, Any]]] = []
                    for index, entry in enumerate(entries):
                        if not isinstance(entry, dict):
                            continue
                        try:
                            timestamp = int(timestamps[index])
                        except (TypeError, ValueError):
                            timestamp = index
                        paired.append((timestamp, index, entry))
                    entries = [entry for _, _, entry in sorted(paired, key=lambda pair: (pair[0], pair[1]), reverse=True)]
                merged[metric] = entries
            return merged
        except Exception as exc:  # noqa: BLE001 - 备用数据失败不能影响行情
            logger.debug("Yahoo 财务时序备用接口暂不可用（%s）：%s", symbol, exc)
            return {"__status__": "retry" if _is_transient_upstream_error(exc) else "unavailable"}

    def _extended_hours(self, ticker: Any, symbol: str) -> dict[str, Any]:
        """盘前 / 盘后 / 夜盘属于附加信息：抓取失败只记日志，不影响行情快照本身。"""
        try:
            with self.upstream_gate.slot():
                frame = ticker.history(
                    period="5d",
                    interval="1m",
                    prepost=True,
                    auto_adjust=False,
                    timeout=UPSTREAM_REQUEST_TIMEOUT_SECONDS,
                )
            summary = summarize_extended_hours(frame, now=datetime.now(MARKET_TIMEZONE))
            current_time = datetime.now(MARKET_TIMEZONE)
            summary["previous_regular_close"] = previous_regular_close(frame, now=current_time)
            summary["prior_session_regular_close"] = prior_session_regular_close(frame, now=current_time)
            summary["regular_open"] = regular_session_open(frame, now=current_time)
            return summary
        except Exception as exc:
            logger.warning("获取 %s 盘前盘后行情失败: %s", symbol, exc)
            return {"state": None, "sessions": {}}

    @staticmethod
    def _history_quote(ticker: Any, price: Any, previous: Any, today_open: Any) -> tuple[Any, Any, Any]:
        """fast_info 缺字段时，使用最近两个交易日日线补齐现价、昨收和今开。"""
        history = ticker.history(period="5d", auto_adjust=False, timeout=UPSTREAM_REQUEST_TIMEOUT_SECONDS)
        if history is None or history.empty or "Close" not in history:
            return price, previous, today_open
        rows = []
        for _, row in history.iterrows():
            close = safe_value(row.get("Close"))
            if close is not None:
                rows.append((safe_value(row.get("Open")), close))
        if not rows:
            return price, previous, today_open
        if price is None:
            price = rows[-1][1]
        if previous is None and len(rows) > 1:
            previous = rows[-2][1]
        if today_open is None:
            today_open = rows[-1][0]
        return price, previous, today_open

    def history(self, symbol: str, period: str = "6mo") -> list[dict[str, Any]]:
        """抓取日线 OHLCV，供斐波那契回撤、筹码分布和承接位计算使用。"""
        normalized = self.normalize_symbol(symbol)
        ticker = self._ticker(normalized)
        try:
            with self.upstream_gate.slot():
                frame = ticker.history(
                    period=period,
                    interval="1d",
                    auto_adjust=False,
                    timeout=UPSTREAM_REQUEST_TIMEOUT_SECONDS,
                )
        except Exception as exc:
            raise ProviderError(f"获取 {normalized} 日线历史失败: {exc}") from exc
        return self._history_bars(frame, normalized)

    @staticmethod
    def _history_bars(frame: Any, symbol: str) -> list[dict[str, Any]]:
        if frame is None or frame.empty:
            raise ProviderError(f"{symbol} 没有可用的日线历史数据")
        bars: list[dict[str, Any]] = []
        for timestamp, row in frame.iterrows():
            item = normalize_row(row)
            close = item.get("Close")
            if close is None:
                continue
            bars.append({
                "date": str(timestamp)[:10],
                "open": item.get("Open"),
                "high": item.get("High"),
                "low": item.get("Low"),
                "close": close,
                "volume": item.get("Volume"),
            })
        if not bars:
            raise ProviderError(f"{symbol} 的日线历史没有有效的收盘价")
        return bars

    def benchmark_history(self, symbol: str = "^GSPC", period: str = "2y") -> list[dict[str, Any]]:
        """抓取 Beta 基准指数的日线；指数代码不走普通股票代码校验。"""
        ticker = self._ticker_raw(symbol)
        try:
            with self.upstream_gate.slot():
                frame = ticker.history(
                    period=period,
                    interval="1d",
                    auto_adjust=True,
                    timeout=UPSTREAM_REQUEST_TIMEOUT_SECONDS,
                )
        except Exception as exc:
            raise ProviderError(f"获取 {symbol} 日线历史失败: {exc}") from exc
        return self._history_bars(frame, symbol)

    def earnings_dates(self, symbol: str) -> list[str]:
        """读取尚未公布的财报日期，按美东日历日返回。"""
        normalized = self.normalize_symbol(symbol)
        ticker = self._ticker(normalized)
        try:
            with self.upstream_gate.slot():
                frame = ticker.get_earnings_dates(limit=12)
        except Exception as exc:
            raise ProviderError(f"获取 {normalized} 财报日期失败: {exc}") from exc
        return parse_earnings_dates(frame)

    @staticmethod
    def estimate_gamma(spot: Any, strike: Any, implied_volatility: Any, expiration: str) -> float | None:
        """用 Black-Scholes 估算单张合约 Gamma；上游期权链通常不返回原始 Gamma。"""
        try:
            spot_value = float(spot)
            strike_value = float(strike)
            volatility = float(implied_volatility)
            expiry = date.fromisoformat(expiration)
        except (TypeError, ValueError):
            return None
        if spot_value <= 0 or strike_value <= 0 or volatility <= 0:
            return None
        volatility = max(volatility, 0.05)
        seconds = (datetime.combine(expiry, datetime.min.time(), tzinfo=timezone.utc) - datetime.now(timezone.utc)).total_seconds()
        time_years = max(seconds / (365 * 24 * 60 * 60), 1 / 365)
        volatility_time = volatility * math.sqrt(time_years)
        d1 = (math.log(spot_value / strike_value) + (0.005 + 0.5 * volatility**2) * time_years) / volatility_time
        return math.exp(-0.5 * d1**2) / (spot_value * volatility_time * math.sqrt(2 * math.pi))

    def chain(self, symbol: str, expiration: str) -> list[dict[str, Any]]:
        normalized = self.normalize_symbol(symbol)
        ticker = self._ticker(normalized)
        try:
            with self.upstream_gate.slot():
                options = ticker.option_chain(expiration)
        except Exception as exc:
            raise ProviderError(f"获取 {normalized} {expiration} 期权链失败: {exc}") from exc
        rows: list[dict[str, Any]] = []
        try:
            for contract_type, frame in (("call", options.calls), ("put", options.puts)):
                records = frame.to_dict(orient="records")
                for raw in records:
                    item = normalize_row(raw)
                    # 只保留入库字段。整行原始字典再复制一份，会让一份期权链在内存里变成三份。
                    rows.append({
                        "symbol": normalized,
                        "expiration": expiration,
                        "contract_type": contract_type,
                        "contract_symbol": item.get("contractSymbol") or item.get("contract_symbol") or "",
                        "strike": item.get("strike"),
                        "last_price": item.get("lastPrice"),
                        "bid": item.get("bid"),
                        "ask": item.get("ask"),
                        "volume": item.get("volume"),
                        "open_interest": item.get("openInterest"),
                        "implied_volatility": item.get("impliedVolatility"),
                        "gamma": item.get("gamma"),
                        "in_the_money": item.get("inTheMoney"),
                        "change_percent": item.get("percentChange"),
                        "provider": self.name,
                    })
                del records
        finally:
            del options
        if not rows:
            raise ProviderError(f"{normalized} {expiration} 没有期权数据")
        return rows

    def fetch(
        self,
        symbol: str,
        expiration: str,
        *,
        quote_override: dict[str, Any] | None = None,
    ) -> tuple[dict[str, Any], list[dict[str, Any]], str]:
        fetched_at = datetime.now(timezone.utc).isoformat()
        quote = quote_override if quote_override is not None else self.quote(symbol)
        rows = self.chain(symbol, expiration)
        for row in rows:
            if row.get("gamma") is None:
                row["gamma"] = self.estimate_gamma(quote.get("price"), row.get("strike"), row.get("implied_volatility"), expiration)
        return quote, rows, fetched_at


from app.providers.hybrid_market import HybridMarketDataProvider
