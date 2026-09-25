"""上游行情接口的期权链快照适配器。

本模块是应用里唯一与外部行情 SDK 打交道的地方：其余代码只依赖这里暴露的
`MarketDataProvider` 接口（标的、行情、期权链、日线），因此更换上游实现时
不需要改动业务层。
"""

from __future__ import annotations

import logging
import math
import re
from datetime import date, datetime, timedelta, timezone
from typing import Any, Callable
from zoneinfo import ZoneInfo

from app.services.concurrency import UpstreamGate
from app.services.market_calendar import is_regular_session, is_trading_day, localize, regular_session_bounds

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


def session_trading_day(moment: datetime) -> date:
    """返回时段所属的美东交易日，夜盘开盘后的日期归到次日。"""
    local = localize(moment)
    minute = local.hour * 60 + local.minute
    if minute >= 20 * 60:
        return local.date() + timedelta(days=1)
    return local.date()


def is_session_trading_day(moment: datetime) -> bool:
    """判断当前时段对应的交易日是否开市，正确处理周末和节假日夜盘。"""
    return is_trading_day(session_trading_day(moment))


def current_session_state(now: datetime, latest_bar: datetime | None) -> str:
    """当前时段：最近一根 K 线足够新时以它所属时段为准，否则按美东时钟判断。

    取绝对差值是为了容忍数据源与本地的少量时钟偏差；时钟判断不识别节假日，
    但只有在最近一根 K 线超出新鲜期时才走到这一步。
    """
    now = localize(now)
    if latest_bar is not None:
        latest_bar = localize(latest_bar)
    if not is_session_trading_day(now):
        return "CLOSED"
    if latest_bar is not None and abs((now - latest_bar).total_seconds()) <= SESSION_FRESH_SECONDS:
        return SESSION_STATES[session_of(latest_bar)] if is_session_trading_day(latest_bar) else "CLOSED"
    if not is_regular_session(now):
        bounds = regular_session_bounds(now)
        if bounds is not None and now >= bounds[1] and now.hour < 20:
            return SESSION_STATES["post"]
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


# 正式昨收和分钟线昨收的相对偏差不超过该比例时，视为同一交易日，优先正式收盘。
PREVIOUS_CLOSE_AGREEMENT = 0.01


def _positive_price(value: Any) -> float | None:
    """把行情数值收成正的有限价格；缺失或无效时返回 None。"""
    number = safe_value(value)
    if isinstance(number, bool) or not isinstance(number, (int, float)):
        return None
    number = float(number)
    if not math.isfinite(number) or number <= 0:
        return None
    return number


def _regular_close_map(frame: Any) -> tuple[dict[date, float], datetime | None]:
    """每个交易日保留最后一笔盘中收盘，并返回全部分钟线里的最后一根时间。"""
    index = getattr(frame, "index", None)
    if (
        frame is None
        or getattr(frame, "empty", True)
        or "Close" not in getattr(frame, "columns", [])
        or not hasattr(index, "tz_convert")
    ):
        return {}, None
    if getattr(index, "tz", None) is None:
        index = index.tz_localize("UTC")
    index = index.tz_convert(MARKET_TIMEZONE)
    closes: dict[date, float] = {}
    latest_bar: datetime | None = None
    for stamp, raw_close in zip(index, frame["Close"].tolist()):
        close = safe_value(raw_close)
        if close is None:
            continue
        latest_bar = stamp.to_pydatetime()
        if session_of(latest_bar) != "regular":
            continue
        closes[session_trading_day(latest_bar)] = float(close)
    return closes, latest_bar


def previous_regular_close(frame: Any, now: datetime | None = None) -> float | None:
    """最近一个已经走完的盘中收盘价。

    正在进行的盘中没有收盘价，需要排除。盘后和休市则保留当天已经结束的那一笔。
    """
    moment_now = now or datetime.now(MARKET_TIMEZONE)
    closes, latest_bar = _regular_close_map(frame)
    if not closes or latest_bar is None:
        return None
    days = sorted(closes)
    state = current_session_state(moment_now, latest_bar)
    if state == "REGULAR" and days[-1] == session_trading_day(moment_now):
        days = days[:-1]
    if not days:
        return None
    return closes[days[-1]]


def prior_session_regular_close(frame: Any, now: datetime | None = None) -> float | None:
    """对标行情源 previous_close 的上一交易日盘中收盘。

    盘前还没有当天盘中 K 线，最近一笔就是昨收。其余时段数据里的最近一个盘中
    已经是当前或刚刚结束的交易日，正式昨收指的是它的前一天。
    """
    moment_now = now or datetime.now(MARKET_TIMEZONE)
    closes, latest_bar = _regular_close_map(frame)
    if not closes or latest_bar is None:
        return None
    days = sorted(closes)
    state = current_session_state(moment_now, latest_bar)
    if state != "PRE":
        if len(days) < 2:
            return None
        days = days[:-1]
    return closes[days[-1]]


def regular_session_open(frame: Any, now: datetime | None = None) -> float | None:
    """最近一个已经开始的盘中交易日里，第一根盘中分钟线的开盘价。

    盘前最后一笔不是开盘价。没有 Open 列时返回 None，不用收盘价顶替。
    """
    moment_now = localize(now or datetime.now(MARKET_TIMEZONE))
    index = getattr(frame, "index", None)
    columns = getattr(frame, "columns", [])
    if (
        frame is None
        or getattr(frame, "empty", True)
        or "Open" not in columns
        or not hasattr(index, "tz_convert")
    ):
        return None
    if getattr(index, "tz", None) is None:
        index = index.tz_localize("UTC")
    index = index.tz_convert(MARKET_TIMEZONE)
    current_day = session_trading_day(moment_now)
    first_open: dict[date, float] = {}
    for stamp, raw_open in sorted(zip(index, frame["Open"].tolist()), key=lambda item: item[0]):
        opening = _positive_price(raw_open)
        if opening is None:
            continue
        bar_time = localize(stamp.to_pydatetime())
        # 还没走到的 K 线不属于已经开始的交易日。
        if bar_time > moment_now or session_of(bar_time) != "regular":
            continue
        day = session_trading_day(bar_time)
        if day > current_day or day in first_open:
            continue
        first_open[day] = opening
    if not first_open:
        return None
    return first_open[max(first_open)]


def choose_previous_close(official: Any, minute_close: Any) -> Any:
    """正式昨收与分钟线昨收接近时用正式昨收，偏离一整根日线时改用分钟线。"""
    official_number = _positive_price(official)
    minute_number = _positive_price(minute_close)
    if minute_number is None:
        return official if official_number is None else official_number
    if official_number is None:
        return minute_number
    if abs(official_number - minute_number) / minute_number <= PREVIOUS_CLOSE_AGREEMENT:
        return official_number
    return minute_number


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


def _read_fast_info(info: Any, key: str) -> Any:
    """读取 fast_info 字段。

    上游对象的公开键是驼峰，`.get("last_price")` 会直接给出 None；
    下标访问才同时接受蛇形键。测试用的普通 dict 仍走 `.get`。
    """
    if isinstance(info, dict):
        return safe_value(info.get(key))
    try:
        return safe_value(info[key])
    except Exception:
        return None


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

    def __init__(
        self,
        ticker_factory: Any | None = None,
        proxy: str | None = None,
        upstream_gate: UpstreamGate | None = None,
    ):
        self.proxy = proxy.strip() if proxy and proxy.strip() else None
        self.upstream_gate = upstream_gate or UpstreamGate()
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
            # 正式昨收含收盘竞价。它和分钟线上一交易日收盘接近时以它为准；
            # 日线空掉一根时两者会差出一整段行情，这时改用分钟线，避免涨跌幅被放大。
            previous = choose_previous_close(previous, extended.get("prior_session_regular_close"))
            change = None
            if price is not None and previous not in (None, 0):
                change = (price - previous) / previous * 100
            return {
                "symbol": normalized,
                "price": price,
                "change_percent": safe_value(change),
                "today_open": today_open,
                "previous_close": previous,
                "currency": safe_value(info.get("currency")) or "USD",
                "market_state": market_state,
                "sessions": extended["sessions"],
                "provider": self.name,
                "raw": {"last_price": price, "previous_close": previous},
            }
        except Exception as exc:
            raise ProviderError(f"获取 {normalized} 行情失败: {exc}") from exc

    def _extended_hours(self, ticker: Any, symbol: str) -> dict[str, Any]:
        """盘前 / 盘后 / 夜盘属于附加信息：抓取失败只记日志，不影响行情快照本身。"""
        try:
            with self.upstream_gate.slot():
                frame = ticker.history(period="5d", interval="1m", prepost=True, auto_adjust=False)
            summary = summarize_extended_hours(frame)
            summary["previous_regular_close"] = previous_regular_close(frame)
            summary["prior_session_regular_close"] = prior_session_regular_close(frame)
            summary["regular_open"] = regular_session_open(frame)
            return summary
        except Exception as exc:
            logger.warning("获取 %s 盘前盘后行情失败: %s", symbol, exc)
            return {"state": None, "sessions": {}}

    @staticmethod
    def _history_quote(ticker: Any, price: Any, previous: Any, today_open: Any) -> tuple[Any, Any, Any]:
        """fast_info 缺字段时，使用最近两个交易日日线补齐现价、昨收和今开。"""
        history = ticker.history(period="5d", auto_adjust=False)
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
                frame = ticker.history(period=period, interval="1d", auto_adjust=False)
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
                frame = ticker.history(period=period, interval="1d", auto_adjust=True)
        except Exception as exc:
            raise ProviderError(f"获取 {symbol} 日线历史失败: {exc}") from exc
        return self._history_bars(frame, symbol)

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


class HybridMarketDataProvider:
    """按交易时段路由期权链，现货与历史行情仍由主行情适配器提供。"""

    name = "hybrid"

    def __init__(
        self,
        regular_provider: Any,
        delayed_provider: Any,
        now_factory: Callable[[], datetime] | None = None,
    ):
        self.regular_provider = regular_provider
        self.delayed_provider = delayed_provider
        self._now_factory = now_factory or (lambda: datetime.now(MARKET_TIMEZONE))

    @staticmethod
    def normalize_symbol(symbol: str) -> str:
        return MarketDataProvider.normalize_symbol(symbol)

    def uses_delayed_options(self) -> bool:
        moment = self._now_factory()
        if moment.tzinfo is None:
            moment = moment.replace(tzinfo=MARKET_TIMEZONE)
        moment = moment.astimezone(MARKET_TIMEZONE)
        return not is_regular_session(moment)

    def expirations(self, symbol: str) -> list[str]:
        provider = self.delayed_provider if self.uses_delayed_options() else self.regular_provider
        return provider.expirations(symbol)

    def quote(self, symbol: str) -> dict[str, Any]:
        return self.regular_provider.quote(symbol)

    def history(self, symbol: str, period: str = "6mo") -> list[dict[str, Any]]:
        return self.regular_provider.history(symbol, period)

    def benchmark_history(self, symbol: str = "^GSPC", period: str = "2y") -> list[dict[str, Any]]:
        return self.regular_provider.benchmark_history(symbol, period)

    def chain(self, symbol: str, expiration: str) -> list[dict[str, Any]]:
        provider = self.delayed_provider if self.uses_delayed_options() else self.regular_provider
        return provider.chain(symbol, expiration)

    def fetch(self, symbol: str, expiration: str) -> tuple[dict[str, Any], list[dict[str, Any]], str]:
        if not self.uses_delayed_options():
            return self.regular_provider.fetch(symbol, expiration)

        try:
            quote = self.regular_provider.quote(symbol)
        except ProviderError as exc:
            logger.warning("非盘中主行情请求失败，改用 Cboe 标的延迟价：%s", exc)
            quote = self.delayed_provider.quote(symbol)
        rows = self.delayed_provider.chain(symbol, expiration)
        if quote.get("price") is None:
            delayed_quote = self.delayed_provider.quote(symbol)
            delayed_quote["sessions"] = quote.get("sessions") or delayed_quote.get("sessions", {})
            quote = delayed_quote
        for row in rows:
            if row.get("gamma") is None:
                row["gamma"] = MarketDataProvider.estimate_gamma(
                    quote.get("price"), row.get("strike"), row.get("implied_volatility"), expiration
                )
        return quote, rows, datetime.now(timezone.utc).isoformat()
