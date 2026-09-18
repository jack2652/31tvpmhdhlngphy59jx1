"""上游行情接口的期权链快照适配器。

本模块是应用里唯一与外部行情 SDK 打交道的地方：其余代码只依赖这里暴露的
`MarketDataProvider` 接口（标的、行情、期权链、日线），因此更换上游实现时
不需要改动业务层。
"""

from __future__ import annotations

import logging
import math
import re
from datetime import date, datetime, timezone
from typing import Any
from zoneinfo import ZoneInfo


logger = logging.getLogger(__name__)

# 美股时段划分（美东时间）：盘前 04:00–09:30、盘中 09:30–16:00、盘后 16:00–20:00、夜盘 20:00–04:00。
MARKET_TIMEZONE = ZoneInfo("America/New_York")
SESSION_STATES = {"pre": "PRE", "regular": "REGULAR", "post": "POST", "overnight": "OVERNIGHT"}
# 最近一根 K 线超过该秒数就认为已经离开该时段，改按美东时钟判断。
SESSION_FRESH_SECONDS = 30 * 60


def session_of(moment: datetime) -> str:
    """按美东时间把一根 K 线归入盘前 / 盘中 / 盘后 / 夜盘。"""
    minute = moment.hour * 60 + moment.minute
    if 4 * 60 <= minute < 9 * 60 + 30:
        return "pre"
    if 9 * 60 + 30 <= minute < 16 * 60:
        return "regular"
    if 16 * 60 <= minute < 20 * 60:
        return "post"
    return "overnight"


def current_session_state(now: datetime, latest_bar: datetime | None) -> str:
    """当前时段：最近一根 K 线足够新时以它所属时段为准，否则按美东时钟判断。

    取绝对差值是为了容忍数据源与本地的少量时钟偏差；时钟判断不识别节假日，
    但只有在最近一根 K 线超出新鲜期时才走到这一步。
    """
    if latest_bar is not None and abs((now - latest_bar).total_seconds()) <= SESSION_FRESH_SECONDS:
        return SESSION_STATES[session_of(latest_bar)]
    if now.weekday() >= 5:
        return "CLOSED"
    return SESSION_STATES[session_of(now)]


def summarize_extended_hours(frame: Any, now: datetime | None = None) -> dict[str, Any]:
    """把含盘前盘后的分钟线归纳成各时段的最新价格。

    每个时段的涨跌幅都以「该时段之前最近一次盘中收盘价」为基准：盘前对应前一交易日
    收盘，盘后/夜盘对应当日收盘，与行情软件的盘前盘后涨跌口径一致。上游接口的
    扩展时段分钟线只覆盖 04:00–20:00，没有 20:00–04:00 的隔夜时段，
    夜盘一旦有数据会按同一规则自动纳入。
    """
    moment_now = now or datetime.now(MARKET_TIMEZONE)
    sessions: dict[str, dict[str, Any]] = {}
    latest_bar: datetime | None = None
    regular_close: float | None = None
    index = getattr(frame, "index", None)
    if (
        frame is None
        or getattr(frame, "empty", True)
        or "Close" not in getattr(frame, "columns", [])
        or not hasattr(index, "tz_convert")
    ):
        return {"state": current_session_state(moment_now, None), "sessions": sessions}
    if getattr(index, "tz", None) is None:
        index = index.tz_localize("UTC")
    index = index.tz_convert(MARKET_TIMEZONE)
    for stamp, raw_close in zip(index, frame["Close"].tolist()):
        close = safe_value(raw_close)
        if close is None:
            continue
        latest_bar = stamp.to_pydatetime()
        session = session_of(latest_bar)
        if session == "regular":
            regular_close = close
            continue
        summary = {"price": close, "change_percent": None, "as_of": latest_bar.isoformat()}
        if regular_close not in (None, 0):
            summary["change_percent"] = safe_value((close - regular_close) / regular_close * 100)
            summary["reference_close"] = regular_close
        sessions[session] = summary
    return {"state": current_session_state(moment_now, latest_bar), "sessions": sessions}


class ProviderError(RuntimeError):
    """供应商返回异常或网络请求失败。"""


def safe_value(value: Any) -> Any:
    """把 pandas/numpy 值转换成可写入 JSON 和 SQLite 的基础类型。"""
    if value is None:
        return None
    if value.__class__.__name__ in {"NAType", "NaTType"}:
        return None
    try:
        if value != value or (isinstance(value, float) and not math.isfinite(value)):
            return None
    except (TypeError, ValueError):
        pass
    if hasattr(value, "item"):
        try:
            return safe_value(value.item())
        except (ValueError, TypeError):
            return None
    if hasattr(value, "isoformat") and not isinstance(value, (str, bytes)):
        return value.isoformat()
    if isinstance(value, (str, int, float, bool)):
        return value
    return str(value)


def normalize_row(row: Any) -> dict[str, Any]:
    if hasattr(row, "to_dict"):
        row = row.to_dict()
    return {str(key): safe_value(value) for key, value in dict(row).items()}


def load_upstream_sdk() -> Any:
    """加载上游行情 SDK。

    整份代码里只有这里导入第三方行情库：一是把外部依赖收敛到一个入口，
    方便日后替换实现；二是测试可以替换本函数注入假 SDK，在无网络环境下
    验证代理、异常处理等逻辑，不必真的发起请求。
    """
    import yfinance as yf

    return yf


class MarketDataProvider:
    # 对外暴露的数据来源标识：只表示「来自上游接口」，不暴露具体供应商
    name = "upstream"

    def __init__(self, ticker_factory: Any | None = None, proxy: str | None = None):
        self.proxy = proxy.strip() if proxy and proxy.strip() else None
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
        try:
            if self.proxy and not self._uses_builtin_factory:
                return self.ticker_factory(normalized, proxy=self.proxy)
            return self.ticker_factory(normalized)
        except Exception as exc:
            raise ProviderError(f"无法创建 {normalized} 行情对象: {exc}") from exc

    def expirations(self, symbol: str) -> list[str]:
        ticker = self._ticker(symbol)
        try:
            values = list(ticker.options or ())
        except Exception as exc:
            raise ProviderError(f"获取 {symbol.upper()} 到期日失败: {exc}") from exc
        return [str(value) for value in values]

    def quote(self, symbol: str) -> dict[str, Any]:
        normalized = self.normalize_symbol(symbol)
        ticker = self._ticker(normalized)
        try:
            try:
                info = ticker.fast_info
            except Exception:
                info = {}
            price = safe_value(info.get("last_price"))
            previous = safe_value(info.get("previous_close"))
            if price is None or previous is None:
                price, previous = self._history_quote(ticker, price, previous)
            change = None
            if price is not None and previous not in (None, 0):
                change = (price - previous) / previous * 100
            extended = self._extended_hours(ticker, normalized)
            return {
                "symbol": normalized,
                "price": price,
                "change_percent": safe_value(change),
                "currency": safe_value(info.get("currency")) or "USD",
                "market_state": safe_value(info.get("market_state")) or extended["state"],
                "sessions": extended["sessions"],
                "provider": self.name,
                "raw": {"last_price": price, "previous_close": previous},
            }
        except Exception as exc:
            raise ProviderError(f"获取 {normalized} 行情失败: {exc}") from exc

    def _extended_hours(self, ticker: Any, symbol: str) -> dict[str, Any]:
        """盘前 / 盘后 / 夜盘属于附加信息：抓取失败只记日志，不影响行情快照本身。"""
        try:
            frame = ticker.history(period="5d", interval="1m", prepost=True, auto_adjust=False)
            return summarize_extended_hours(frame)
        except Exception as exc:
            logger.warning("获取 %s 盘前盘后行情失败: %s", symbol, exc)
            return {"state": None, "sessions": {}}

    @staticmethod
    def _history_quote(ticker: Any, price: Any, previous: Any) -> tuple[Any, Any]:
        """fast_info 缺字段时，使用最近两个交易日收盘价计算快照。"""
        history = ticker.history(period="5d", auto_adjust=False)
        if history is None or history.empty or "Close" not in history:
            return price, previous
        closes = [safe_value(value) for value in history["Close"].dropna().tolist()]
        if not closes:
            return price, previous
        if price is None:
            price = closes[-1]
        if previous is None and len(closes) > 1:
            previous = closes[-2]
        return price, previous

    def history(self, symbol: str, period: str = "6mo") -> list[dict[str, Any]]:
        """抓取日线 OHLCV，供斐波那契回撤、筹码分布和承接位计算使用。"""
        normalized = self.normalize_symbol(symbol)
        ticker = self._ticker(normalized)
        try:
            frame = ticker.history(period=period, interval="1d", auto_adjust=False)
        except Exception as exc:
            raise ProviderError(f"获取 {normalized} 日线历史失败: {exc}") from exc
        if frame is None or frame.empty:
            raise ProviderError(f"{normalized} 没有可用的日线历史数据")
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
            raise ProviderError(f"{normalized} 的日线历史没有有效的收盘价")
        return bars

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
            options = ticker.option_chain(expiration)
        except Exception as exc:
            raise ProviderError(f"获取 {normalized} {expiration} 期权链失败: {exc}") from exc
        rows: list[dict[str, Any]] = []
        for contract_type, frame in (("call", options.calls), ("put", options.puts)):
            for raw in frame.to_dict(orient="records"):
                item = normalize_row(raw)
                item.update({
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
                    "raw": item.copy(),
                })
                rows.append(item)
        if not rows:
            raise ProviderError(f"{normalized} {expiration} 没有期权数据")
        return rows

    def fetch(self, symbol: str, expiration: str) -> tuple[dict[str, Any], list[dict[str, Any]], str]:
        fetched_at = datetime.now(timezone.utc).isoformat()
        quote = self.quote(symbol)
        rows = self.chain(symbol, expiration)
        for row in rows:
            if row.get("gamma") is None:
                row["gamma"] = self.estimate_gamma(quote.get("price"), row.get("strike"), row.get("implied_volatility"), expiration)
        return quote, rows, fetched_at
