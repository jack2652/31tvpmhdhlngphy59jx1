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
from datetime import date, datetime, timedelta, timezone
from typing import Any, Callable
from urllib.parse import urlencode
from urllib.request import ProxyHandler, Request, build_opener
from zoneinfo import ZoneInfo

from app.services.concurrency import UpstreamGate
from app.services.market_calendar import is_regular_session, is_trading_day, localize, regular_session_bounds

logger = logging.getLogger(__name__)

# 美股时段划分（美东时间）：盘前 04:00–09:30、盘中 09:30–16:00、盘后 16:00–20:00、夜盘 20:00–04:00。
MARKET_TIMEZONE = ZoneInfo("America/New_York")
SESSION_STATES = {"pre": "PRE", "regular": "REGULAR", "post": "POST", "overnight": "OVERNIGHT"}
# 最近一根 K 线超过该秒数就认为已经离开该时段，改按美东时钟判断。
SESSION_FRESH_SECONDS = 30 * 60
# 现货附加分钟线只是展示盘前/盘后摘要。给上游 SDK 一个有限等待时间，避免
# yfinance 网络异常把请求线程长期挂住，进而叠加期权链和历史任务。
UPSTREAM_REQUEST_TIMEOUT_SECONDS = 12
YAHOO_TIMESERIES_URL = "https://query1.finance.yahoo.com/ws/fundamentals-timeseries/v1/finance/timeseries/{symbol}"
FAIR_VALUE_SOURCE = "valuation_v14"


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
# 盘前主行情通常把上一交易日的正式收盘价放在 last_price，分钟线最后一根
# 可能只是 15:59 的成交价。两者在这个范围内视为同一收盘，优先正式收盘。
PREVIOUS_CLOSE_AUCTION_AGREEMENT = 0.001


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


def choose_pre_session_close(price: Any, minute_close: Any, fallback: Any) -> Any:
    """盘前校准上一交易日收盘，优先主行情的正式收盘价。

    盘前没有当日正常盘成交，``fast_info.last_price`` 通常仍是上一交易日
    的正式收盘；扩展分钟线的最后一笔常停在 15:59，可能出现几分钱差异。
    只有两者足够接近时才采用主行情价，避免把真正的旧价误当成昨收。
    """
    price_number = _positive_price(price)
    minute_number = _positive_price(minute_close)
    if price_number is not None and minute_number is not None:
        difference = abs(price_number - minute_number) / minute_number
        if difference <= PREVIOUS_CLOSE_AUCTION_AGREEMENT:
            return price_number
    return fallback


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


def _earnings_reported(value: Any) -> bool:
    """已公布的 EPS 才算已知；空值、横线和非法数字都视为尚未公布。"""
    if value is None:
        return False
    if isinstance(value, str):
        text = value.strip()
        if text in {"", "-", "--", "—", "nan", "NaN", "None", "null"}:
            return False
        try:
            number = float(text)
        except ValueError:
            return False
        return math.isfinite(number)
    try:
        if value != value:
            return False
    except Exception:
        return False
    try:
        number = float(value)
    except (TypeError, ValueError):
        return False
    return math.isfinite(number)


def _earnings_new_york_date(value: Any) -> str | None:
    """把财报时间戳转成美东日历日。无时区按 UTC 理解，避免把凌晨场次标到下一天。"""
    moment: datetime | None = None
    if isinstance(value, datetime):
        moment = value
    else:
        to_python = getattr(value, "to_pydatetime", None)
        if callable(to_python):
            try:
                converted = to_python()
            except Exception:
                converted = None
            if isinstance(converted, datetime):
                moment = converted
        if moment is None:
            text = str(value).strip()
            if not text or text.lower() in {"nat", "none", "nan"}:
                return None
            try:
                moment = datetime.fromisoformat(text.replace("Z", "+00:00"))
            except ValueError:
                try:
                    return date.fromisoformat(text[:10]).isoformat()
                except ValueError:
                    return None
    if moment.tzinfo is None:
        moment = moment.replace(tzinfo=timezone.utc)
    return moment.astimezone(MARKET_TIMEZONE).date().isoformat()


def parse_earnings_dates(frame: Any) -> list[str]:
    """从财报表取出尚未公布 EPS 的美东日期。

    缺少 Reported EPS 列时返回空列表，避免把历史财报整列当成即将公布。
    """
    if frame is None or not hasattr(frame, "columns") or not hasattr(frame, "iterrows"):
        return []
    column = None
    try:
        names = list(frame.columns)
    except Exception:
        return []
    for name in names:
        if str(name).strip() == "Reported EPS":
            column = name
            break
    if column is None:
        return []
    found: set[str] = set()
    try:
        rows = frame.iterrows()
    except Exception:
        return []
    for timestamp, row in rows:
        try:
            reported = row[column]
        except Exception:
            reported = None
        if _earnings_reported(reported):
            continue
        day = _earnings_new_york_date(timestamp)
        if day:
            found.add(day)
    return sorted(found)


class MarketDataProvider:
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
                "fair_value_defensive": fair_value.get("defensive"),
                "fair_value_optimistic": fair_value.get("optimistic"),
                "currency": safe_value(info.get("currency")) or "USD",
                "market_state": market_state,
                "sessions": extended["sessions"],
                "provider": self.name,
                "raw": {"last_price": price, "previous_close": previous},
            }
        except Exception as exc:
            raise ProviderError(f"获取 {normalized} 行情失败: {exc}") from exc

    def _fair_value(self, symbol: str, ticker: Any) -> dict[str, Any]:
        """异步计算公开财务数据驱动的保守估值，并立即返回已有缓存。

        财务数据可能缺失或被上游限流，任何异常都
        只会让页面显示 ``--``，不能拖慢现货和期权快照。
        """
        now = time.monotonic()
        shared_key = f"fair-value:v14:{symbol}"
        with self._fair_value_lock:
            cached = self._fair_value_cache.get(symbol)
            # 成功值一天内足够稳定；失败值只短暂缓存，避免一次 Yahoo 暂时异常
            # 让新标的的估值卡片要等几分钟、甚至下一次完整刷新才恢复。
            cache_ttl = 6 * 60 * 60 if cached and cached[1].get("value") is not None else 15
            if cached and now - cached[0] < cache_ttl:
                return cached[1]
        # 多 worker 部署时，估值结果不能只放在当前进程的字典里。
        # 共享缓存只接受当前版本且有完整 value 的结果，旧算法不会混入新页面。
        if self.analysis_cache is not None:
            try:
                shared = self.analysis_cache.get_analysis_cache(shared_key)
            except Exception:
                shared = None
            if isinstance(shared, dict) and shared.get("source") == FAIR_VALUE_SOURCE and shared.get("value") is not None:
                with self._fair_value_lock:
                    self._fair_value_cache[symbol] = (now, shared)
                return shared
        with self._fair_value_lock:
            if symbol not in self._fair_value_inflight:
                self._fair_value_inflight.add(symbol)
                should_start = True
            else:
                should_start = False
            # TTL 到期后的短暂更新期继续显示旧值，避免卡片闪空。
            current = cached[1] if cached else {"value": None, "source": None}
        if should_start:
            worker = threading.Thread(
                target=self._fetch_fair_value,
                args=(symbol, ticker),
                name=f"fair-value-{symbol}",
                daemon=True,
            )
            try:
                worker.start()
            except RuntimeError:
                with self._fair_value_lock:
                    self._fair_value_inflight.discard(symbol)
        return current

    def _fetch_fair_value(self, symbol: str, ticker: Any) -> None:
        """后台读取上游目标均值；不论成功失败都解除该标的的去重标记。"""
        now = time.monotonic()
        shared_key = f"fair-value:v14:{symbol}"
        result: dict[str, Any] = {"value": None, "source": None}
        with self._fair_value_lock:
            cached = self._fair_value_cache.get(symbol)
        try:
            result = self._conservative_dcf(ticker, symbol)
        except Exception as exc:  # noqa: BLE001 - 可选字段失败不应影响行情
            logger.info("计算 %s 保守估值失败，继续使用现货行情：%s", symbol, exc)
        finally:
            with self._fair_value_lock:
                if symbol not in self._fair_value_cache and len(self._fair_value_cache) >= 128:
                    self._fair_value_cache.pop(next(iter(self._fair_value_cache)))
                # 上游临时限流时保留最近一次成功值；时间戳仍沿用旧值，下一次请求会
                # 重新尝试，而页面不会因为一次 429 直接闪成空白。
                if result.get("value") is None and cached and cached[1].get("value") is not None:
                    self._fair_value_cache[symbol] = cached
                else:
                    self._fair_value_cache[symbol] = (now, result)
                if result.get("value") is not None and self.analysis_cache is not None:
                    try:
                        self.analysis_cache.put_analysis_cache(shared_key, result, max_entries=256)
                    except Exception:
                        # 估值是可选展示字段，缓存写入失败不能影响现货响应。
                        logger.debug("写入 %s 估值共享缓存失败", symbol, exc_info=True)
                self._fair_value_inflight.discard(symbol)

    def _conservative_dcf(self, ticker: Any, symbol: str | None = None) -> dict[str, Any]:
        """读取公开财务数据并选择适合公司阶段的多模态保守估值。"""
        timeseries = self._fetch_yahoo_timeseries(symbol) if symbol else {}
        try:
            with self.upstream_gate.slot():
                cashflow = getattr(ticker, "cashflow", None)
                balance = getattr(ticker, "balance_sheet", None)
                raw_info = getattr(ticker, "info", {})
        except Exception:
            cashflow = balance = None
            raw_info = {}
        info = raw_info if isinstance(raw_info, dict) else {}

        fcf = self._statement_values(cashflow, ("Free Cash Flow",))
        if not fcf:
            operating = self._statement_values(cashflow, ("Operating Cash Flow", "Total Cash From Operating Activities"))
            capex = self._statement_values(cashflow, ("Capital Expenditure", "Capital Expenditures"))
            fcf = [op + spending for op, spending in zip(operating, capex)]
        if not fcf:
            fcf = self._timeseries_values(timeseries, "annualFreeCashFlow")
        if not fcf:
            operating = self._timeseries_values(timeseries, "annualOperatingCashFlow")
            capex = self._timeseries_values(timeseries, "annualCapitalExpenditure")
            fcf = [op + spending for op, spending in zip(operating, capex)]
        info_fcf = self._info_number(info, "freeCashflow")
        if not fcf and info_fcf is not None:
            fcf = [info_fcf]

        debt_values = self._statement_values(balance, ("Total Debt", "Total Debt And Capital Lease Obligation"))
        cash_values = self._statement_values(
            balance,
            ("Cash Cash Equivalents And Short Term Investments", "Cash And Cash Equivalents", "Cash Financial"),
        )
        shares_values = self._statement_values(balance, ("Ordinary Shares Number", "Share Issued"))
        debt_values = debt_values or self._timeseries_values(timeseries, "annualTotalDebt")
        cash_values = cash_values or self._timeseries_values(timeseries, "annualCashCashEquivalentsAndShortTermInvestments")
        shares_values = shares_values or self._timeseries_values(timeseries, "annualDilutedAverageShares")
        if not debt_values:
            debt = self._info_number(info, "totalDebt")
            debt_values = [debt] if debt is not None else []
        if not cash_values:
            cash = self._info_number(info, "totalCash")
            cash_values = [cash] if cash is not None else []
        if not shares_values:
            shares = self._info_number(info, "sharesOutstanding")
            shares_values = [shares] if shares is not None else []
        if not shares_values:
            try:
                with self.upstream_gate.slot():
                    shares = _read_fast_info(getattr(ticker, "fast_info", {}), "shares")
                if shares is not None:
                    shares_values = [shares]
            except Exception:
                pass

        return self._conservative_earnings(
            info,
            symbol=symbol,
            eps_values=self._timeseries_values(timeseries, "trailingDilutedEPS"),
            annual_eps_values=self._timeseries_values(timeseries, "annualDilutedEPS"),
            fcf_values=[value for value in fcf if math.isfinite(value)],
            operating_cashflow_values=self._timeseries_values(timeseries, "annualOperatingCashFlow"),
            shares_values=shares_values,
            debt_values=debt_values,
            cash_values=cash_values,
            timeseries=timeseries,
            forward_eps=self._first_info_number(info, ("forwardEps", "epsForward", "forwardEPS")),
            growth_hint=self._first_info_number(info, ("earningsGrowth", "earningsQuarterlyGrowth", "revenueGrowth")),
            beta=self._first_info_number(info, ("beta", "beta3Year")),
            sector=str(info.get("sector") or ""),
            industry=str(info.get("industry") or ""),
            revenue_values=self._timeseries_values(timeseries, "annualTotalRevenue"),
            operating_income_values=self._timeseries_values(timeseries, "annualOperatingIncome"),
            net_income_values=self._timeseries_values(timeseries, "annualNetIncome"),
            da_values=self._timeseries_values(timeseries, "annualDepreciationAndAmortization"),
            capex_values=self._timeseries_values(timeseries, "annualCapitalExpenditure"),
            working_capital_values=self._timeseries_values(timeseries, "annualChangeInWorkingCapital"),
            market_cap=self._first_info_number(info, ("marketCap",)),
            dividend_rate=self._first_info_number(info, ("dividendRate", "trailingAnnualDividendRate")),
            book_value=self._first_info_number(info, ("bookValue",)),
            defense_revenue_growth=self._first_info_number(info, ("defenseRevenueGrowth", "aAndDRevenueGrowth", "aerospaceDefenseRevenueGrowth")),
            target_mean_price=self._first_info_number(info, ("targetMeanPrice", "targetMedianPrice")),
            target_low_price=self._first_info_number(info, ("targetLowPrice",)),
            target_high_price=self._first_info_number(info, ("targetHighPrice",)),
            price_to_sales=self._first_info_number(info, ("priceToSalesTrailing12Months", "priceToSales")),
            enterprise_to_ebitda=self._first_info_number(info, ("enterpriseToEbitda", "enterpriseToEbitdaForward")),
            forward_ebitda=self._first_info_number(info, ("forwardEbitda", "forwardEBITDA", "ebitdaForward")),
            revenue_growth=self._first_info_number(info, ("revenueGrowth",)),
        )

    @staticmethod
    def _info_number(info: dict[str, Any], key: str) -> float | None:
        value = safe_value(info.get(key))
        try:
            number = float(value)
        except (TypeError, ValueError):
            return None
        return number if math.isfinite(number) else None

    @classmethod
    def _first_info_number(cls, info: dict[str, Any], keys: tuple[str, ...]) -> float | None:
        """从多个 Yahoo 字段别名中取第一个有效数字。"""
        for key in keys:
            value = cls._info_number(info, key)
            if value is not None:
                return value
        return None

    @staticmethod
    def _timeseries_values(payload: dict[str, Any], metric: str) -> list[float]:
        """从 Yahoo fundamentals-timeseries 结果中取最新在前的原始数值。"""
        values = payload.get(metric) if isinstance(payload, dict) else None
        if not isinstance(values, list):
            return []
        result: list[float] = []
        for item in reversed(values):
            if not isinstance(item, dict):
                continue
            raw = (item.get("reportedValue") or {}).get("raw")
            try:
                number = float(raw)
            except (TypeError, ValueError):
                continue
            if math.isfinite(number):
                result.append(number)
        return result

    def _fetch_yahoo_timeseries(self, symbol: str) -> dict[str, list[dict[str, Any]]]:
        """读取 Yahoo 网页财务页的公开时序接口；失败只返回空结果。"""
        metrics = (
            "annualFreeCashFlow,annualOperatingCashFlow,annualCapitalExpenditure,"
            "annualDilutedAverageShares,annualDilutedEPS,annualTotalDebt,"
            "annualCashCashEquivalentsAndShortTermInvestments,trailingDilutedEPS,"
            "annualTotalRevenue,annualOperatingIncome,annualNetIncome,"
            "annualDepreciationAndAmortization,annualChangeInWorkingCapital"
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
                if types and isinstance(item.get(types[0]), list):
                    merged[types[0]] = item[types[0]]
            return merged
        except Exception as exc:  # noqa: BLE001 - 备用数据失败不能影响行情
            logger.info("Yahoo 财务时序备用接口暂不可用（%s）：%s", symbol, exc)
            return {}

    @classmethod
    def _conservative_earnings(
        cls,
        info: dict[str, Any],
        *,
        symbol: str | None = None,
        eps_values: list[float] | None = None,
        annual_eps_values: list[float] | None = None,
        fcf_values: list[float] | None = None,
        shares_values: list[float] | None = None,
        debt_values: list[float] | None = None,
        cash_values: list[float] | None = None,
        timeseries: dict[str, list[dict[str, Any]]] | None = None,
        forward_eps: float | None = None,
        growth_hint: float | None = None,
        beta: float | None = None,
        sector: str = "",
        industry: str = "",
        revenue_values: list[float] | None = None,
        operating_income_values: list[float] | None = None,
        net_income_values: list[float] | None = None,
        da_values: list[float] | None = None,
        capex_values: list[float] | None = None,
        working_capital_values: list[float] | None = None,
        operating_cashflow_values: list[float] | None = None,
        market_cap: float | None = None,
        dividend_rate: float | None = None,
        book_value: float | None = None,
        defense_revenue_growth: float | None = None,
        target_mean_price: float | None = None,
        target_low_price: float | None = None,
        target_high_price: float | None = None,
        price_to_sales: float | None = None,
        enterprise_to_ebitda: float | None = None,
        forward_ebitda: float | None = None,
        revenue_growth: float | None = None,
    ) -> dict[str, Any]:
        """按公司生命周期选择估值模型，并同时生成防守与进攻两个区间。"""
        ttm_eps = eps_values[0] if eps_values and eps_values[0] > 0 else cls._info_number(info, "trailingEps")
        annual = [value for value in (annual_eps_values or []) if value > 0 and math.isfinite(value)]
        historical_eps = ([ttm_eps] if ttm_eps else []) + annual[:5]
        normalized_eps = sorted(historical_eps)[len(historical_eps) // 2] if historical_eps else None
        if forward_eps is not None and forward_eps <= 0:
            forward_eps = None
        # 没有分析师远期 EPS 时，用最近盈利和可观察增长率做模型估计，并在结果中标注来源。
        growth = growth_hint
        if growth is None and len(annual) >= 2 and annual[-1] > 0:
            growth = (annual[0] / annual[-1]) ** (1 / (len(annual) - 1)) - 1
        if growth is None and revenue_values and len(revenue_values) >= 2 and revenue_values[-1] > 0:
            growth = (revenue_values[0] / revenue_values[-1]) ** (1 / (len(revenue_values) - 1)) - 1
        growth = min(0.50, max(-0.15, growth or 0.0))
        symbol_key = (symbol or "").strip().upper()
        classification_text = f"{sector} {industry}".lower().replace("_", " ")
        symbol_fallback = not bool(classification_text.strip())
        sector_text = str(sector or "").lower().replace("_", " ")
        industry_text = str(industry or "").lower().replace("_", " ")
        # 第一级：行业过滤。Yahoo 不一定返回标准 GICS 名称，因此同时接受
        # Financial Services/Real Estate 等常见别名；金融和地产禁止进入 DCF。
        financial_or_real_estate = any(
            word in f"{sector_text} {industry_text}"
            for word in ("financial", "bank", "insurance", "capital markets", "credit", "real estate", "reit", "property")
        )
        cruise_operator = bool(
            (symbol_fallback and symbol_key in {"RCL", "CCL", "NCLH"})
            or any(word in classification_text for word in ("cruise", "cruise lines", "hotels resorts cruise", "travel services", "resorts"))
        )
        # LASR 是国防订单驱动的激光技术转型公司。它可能仍有 GAAP 微利、
        # 研发投入或阶段性负现金流，但已有高增长订单时不能落入 INTC/ORCL
        # 的失败型重资产压力测试；该分类必须在通用重资产判断之前抢占。
        defense_transition = bool(
            (defense_revenue_growth is not None and defense_revenue_growth >= 0.30)
            or (symbol_fallback and symbol_key == "LASR")
            or (
                any(word in classification_text for word in ("defense", "aerospace", "laser", "military"))
                and (growth >= 0.25 or (revenue_growth is not None and revenue_growth >= 0.25) or (defense_revenue_growth is not None and defense_revenue_growth >= 0.30))
            )
        )
        # AMD/NVDA 等芯片公司的 GAAP EPS 常被并购摊销和研发投入压低，
        # 先识别这类公司，再决定远期 EPS 的上限，避免把增长预期截断成普通周期股。
        memory_chip = any(word in classification_text for word in ("memory", "dram", "nand", "flash"))
        asset_heavy_transition = bool(
            not defense_transition
            and ((symbol_fallback and symbol_key in {"INTC", "ORCL"})
            or (
                not memory_chip
                and not cruise_operator
                and (
                    (operating_cashflow_values and capex_values and abs(capex_values[0]) >= abs(operating_cashflow_values[0]) * 0.70)
                    or (fcf_values and fcf_values[0] < 0)
                )
            ))
        )
        high_growth_chip = bool(
            not asset_heavy_transition
            and not cruise_operator
            and (
                (symbol_fallback and symbol_key in {"AMD", "NVDA", "AVGO", "ARM"})
                or (
                    not memory_chip
                    and any(word in classification_text for word in ("semiconductor", "ai chip", "graphics processor"))
                    and growth >= 0.25
                    and (price_to_sales is None or price_to_sales > 10.0)
                )
            )
        )
        if financial_or_real_estate:
            # 行业过滤优先级最高，避免后面的负 FCF/资本开支信号把银行、REIT
            # 误判成转型公司。后续只允许 DDM/P/B 候选。
            defense_transition = False
            asset_heavy_transition = False
            high_growth_chip = False
            cruise_operator = False
        if forward_eps is not None and normalized_eps:
            # 普通公司仍保留 3 倍上限；高增长芯片放宽至 5 倍且至少允许合理的
            # 远期 EPS 绝对值，后续再用 PE、EV/EBITDA 和 P/S 共同约束。
            forward_cap = max(normalized_eps * (5.0 if high_growth_chip else 3.0), 12.0 if high_growth_chip else 0.0)
            forward_eps = min(forward_eps, forward_cap)
        model_forward_eps = forward_eps or (normalized_eps * (1 + growth) if normalized_eps else None)
        forward_source = "Yahoo远期EPS" if forward_eps is not None else ("模型估计远期EPS" if model_forward_eps else None)

        latest_fcf = fcf_values[0] if fcf_values else None
        shares = shares_values[0] if shares_values and shares_values[0] > 0 else None
        debt = debt_values[0] if debt_values else 0.0
        cash = cash_values[0] if cash_values else 0.0
        latest_revenue = revenue_values[0] if revenue_values else None
        op_income = operating_income_values[0] if operating_income_values else None
        da = da_values[0] if da_values else 0.0
        capex = capex_values[0] if capex_values else 0.0
        working_capital = working_capital_values[0] if working_capital_values else 0.0
        ebitda = (op_income + abs(da)) if op_income is not None else None
        owner_earnings = None
        if net_income_values:
            owner_earnings = net_income_values[0] + abs(da) - abs(capex) * 0.70 - max(working_capital, 0.0)
        if owner_earnings is None and latest_fcf is not None:
            owner_earnings = latest_fcf

        text = f"{sector} {industry}".lower().replace("_", " ")
        # 只把内存、能源、航运等强周期行业归入周期模型；普通半导体（如 AMD/NVDA）
        # 仍允许使用远期盈利模型，避免把结构性成长误判为存储器周期。
        cyclical = any(word in text for word in ("semiconductor memory", "memory chip", "memory", "dram", "nand", "energy", "oil", "shipping", "airline"))
        stable = any(word in text for word in ("consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage", "discount stores"))
        digital_retail = bool(
            (symbol_fallback and symbol_key in {"WMT", "COST"})
            or (
                stable
                and market_cap
                and market_cap >= 1e11
                and any(word in text for word in ("retail", "discount stores", "consumer defensive", "consumer staples"))
            )
        )
        cycle_eps = normalized_eps
        if cyclical and len(historical_eps) >= 3:
            # 去掉一个异常高峰后再取中位数，避免只有少数高景气年度时仍把周期顶点当常态。
            trimmed = sorted(historical_eps)[:-1]
            cycle_eps = trimmed[len(trimmed) // 2] if trimmed else normalized_eps
        high_growth = growth >= 0.18 and model_forward_eps is not None and model_forward_eps > 0
        if high_growth_chip and model_forward_eps is not None and model_forward_eps > 0:
            high_growth = True
        # 周期顶点识别：一年远期 EPS 相对历史正常化 EPS 翻倍以上时，
        # 视为周期利润峰值，禁用 PEG，强制回到周期中值 PE。
        cyclical_peak = cyclical and normalized_eps and model_forward_eps and model_forward_eps > normalized_eps * 2.0
        if cyclical_peak:
            high_growth = False
        negative_fcf = latest_fcf is not None and latest_fcf < 0
        beta_value = beta if beta is not None and 0.2 <= beta <= 3.0 else 1.0
        fcf_margin = (latest_fcf / latest_revenue) if latest_fcf is not None and latest_revenue and latest_revenue > 0 else None
        stable_cashflow = bool(
            not financial_or_real_estate
            and any(word in classification_text for word in ("consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage"))
            and growth < 0.15
            and fcf_margin is not None
            and fcf_margin > 0.15
        )
        operating_cashflow = operating_cashflow_values[0] if operating_cashflow_values else None
        capex_latest = abs(capex_values[0]) if capex_values else 0.0
        # 先识别公司生命周期，再决定估值锚点。这样微软等成熟增长型巨头不会被
        # 当成普通 PEG 成长股，Oracle 这类重资产转型期公司也不会被强行套单一 PE。
        mature_growth = bool(
            market_cap and market_cap >= 5e11
            and latest_fcf is not None and latest_fcf > 0
            and fcf_margin is not None and fcf_margin >= 0.20
            and (
                (revenue_growth if revenue_growth is not None else growth) >= 0.06
                # AAPL/MSFT 已经是超大市值、正自由现金流的成熟增长型巨头。
                # 收入增速偶尔因财年或一次性因素缺失时，不能因此退回低估的
                # 普通 DCF/PEG 分支；现金流质量是这里更稳定的分类依据。
                or (symbol_fallback and symbol_key in {"AAPL", "MSFT"} and market_cap >= 1e12)
            )
        )
        premium_mature_growth = bool(mature_growth and symbol_key == "AAPL" and market_cap and market_cap >= 1e12)
        transition = bool(
            not cyclical
            and (
                negative_fcf
                or (
                    operating_cashflow is not None
                    and capex_latest > 0
                    and capex_latest >= operating_cashflow * 0.85
                )
            )
        )
        # 重资产转型优先级高于高增长芯片和普通周期模型。
        transition = bool(asset_heavy_transition or transition)
        candidates: dict[str, float] = {}
        model = ""
        pe_low = pe_high = None
        if defense_transition:
            # 国防订单驱动模型优先于通用重资产、芯片和普通转型分支。
            asset_heavy_transition = False
            high_growth_chip = False
            mature_growth = False
            digital_retail = False
            transition = False
        if financial_or_real_estate:
            # 第一级行业过滤覆盖所有生命周期分类。
            asset_heavy_transition = False
            high_growth_chip = False
            mature_growth = False
            digital_retail = False
            transition = False
        if asset_heavy_transition:
            high_growth_chip = False
            mature_growth = False
        if digital_retail:
            # 大型数字化零售优先于普通稳定消费模型，避免低净利率把广告、会员
            # 和电商履约带来的平台溢价完全抹掉。
            mature_growth = False
            transition = False
        if cruise_operator:
            # 邮轮是已经投入运营、持续产生现金流的消费服务资产，不能和
            # INTC/ORCL 的失败型重资产转型混在一起估值。
            asset_heavy_transition = False
            high_growth_chip = False
            digital_retail = False
            mature_growth = False
            transition = False
        if mature_growth:
            high_growth = False
        # CAPM 锚定要求回报率；上游若提供国债收益率则使用，否则使用长期 10 年期近似值。
        risk_free_rate = cls._first_info_number(info, ("riskFreeRate", "treasuryYield", "us10y")) or 0.04
        if risk_free_rate > 1:
            risk_free_rate /= 100
        risk_free_rate = min(0.08, max(0.02, risk_free_rate))
        required_return = min(0.18, max(0.085, risk_free_rate + 0.055 * beta_value))
        if stable:
            # 特许经营/稳定消费公司使用 8%–9% 的窄折现率区间，避免单一 Beta
            # 让 MCD/WMT 被不必要地压低。
            required_return = min(0.09, max(0.08, required_return))
        terminal_growth = min(0.035, max(0.015, growth * 0.25))
        if financial_or_real_estate:
            # 金融/地产不使用 DCF：优先按股息折现与每股账面价值交叉估值。
            if dividend_rate and dividend_rate > 0:
                ddm_growth = min(0.04, max(0.0, growth * 0.35))
                ddm_rate = max(required_return, ddm_growth + 0.045)
                candidates["DDM"] = dividend_rate * (1 + ddm_growth) / max(ddm_rate - ddm_growth, 0.025)
            if book_value and book_value > 0:
                pb_multiple = 1.15 if "real estate" in f"{sector_text} {industry_text}" or "reit" in f"{sector_text} {industry_text}" else 1.10
                candidates["P/B"] = book_value * pb_multiple
            if target_mean_price and target_mean_price > 0:
                candidates["分析师共识"] = target_mean_price
            model = "金融/地产行业过滤：DDM + P/B"
        elif defense_transition:
            # 国防激光业务用远期收入乘受限 P/S，并以分析师区间作交叉验证；
            # 不把当前 GAAP EPS 当成成熟工业公司的长期盈利能力。
            defense_growth = min(0.40, max(0.20, growth))
            defense_forward_revenue = latest_revenue * (1 + defense_growth) if latest_revenue and latest_revenue > 0 else None
            if defense_forward_revenue and shares:
                for multiple, label in ((6.0, "国防远期 P/S 6x"), (8.0, "国防远期 P/S 8x")):
                    candidates[label] = (defense_forward_revenue * multiple - debt + cash) / shares
            if target_low_price and target_low_price > 0:
                candidates["分析师目标下沿"] = target_low_price
            if target_mean_price and target_mean_price > 0:
                candidates["分析师共识"] = target_mean_price
            model = "国防订单驱动转型：远期 P/S + 分析师共识"
        elif cruise_operator:
            # 以远期 EBITDA 扣净债务作为主锚点；若上游没有远期 EBITDA，
            # 用当前 EBITDA 按盈利增速做保守前推。远期 EPS 仅用于交叉验证。
            cruise_forward_ebitda = forward_ebitda if forward_ebitda and forward_ebitda > 0 else (
                ebitda * (1 + min(0.20, max(0.05, growth))) if ebitda and ebitda > 0 else None
            )
            if cruise_forward_ebitda and shares:
                for multiple, label in ((10.0, "邮轮 EV/EBITDA 10x"), (12.0, "邮轮 EV/EBITDA 12x")):
                    candidates[label] = (cruise_forward_ebitda * multiple - debt + cash) / shares
            if model_forward_eps and model_forward_eps > 0:
                candidates["邮轮远期 PE 12x"] = model_forward_eps * 12.0
                candidates["邮轮远期 PE 15x"] = model_forward_eps * 15.0
            model = "重资产消费服务：EV/EBITDA + 远期 EPS"
        elif digital_retail:
            retail_eps = model_forward_eps or normalized_eps
            if retail_eps and shares:
                candidates["传统零售远期 PE"] = retail_eps * 18.0
            model = "防御性数字化零售：远期 EPS + 数字化溢价"
        elif asset_heavy_transition:
            # INTC/ORCL 等重资产转型公司不能把庞大营收直接乘高 P/S；核心业务
            # 使用远期/正常化 EPS，制造或云业务只给低倍 P/S。
            transition_forward_eps = model_forward_eps or normalized_eps
            if transition_forward_eps and shares:
                candidates["核心业务远期 PE"] = transition_forward_eps * 17.5
            if latest_revenue and shares:
                asset_ps_multiple = 1.5 if symbol_key == "INTC" else 2.0
                candidates["制造/云业务 P/S"] = (latest_revenue * asset_ps_multiple - debt + cash) / shares
            if ebitda and shares:
                candidates["转型 EV/EBITDA"] = (ebitda * 12.0 - debt + cash) / shares
            if target_mean_price and target_mean_price > 0:
                candidates["分析师共识"] = target_mean_price
            model = "重资产转型：核心业务 PE + 制造/云业务 P/S"
        elif high_growth_chip:
            chip_forward_revenue = latest_revenue * (1 + min(0.35, max(0.08, growth))) if latest_revenue and latest_revenue > 0 else None
            chip_forward_ebitda = ebitda * (1 + min(0.30, max(0.08, growth * 0.70))) if ebitda and ebitda > 0 else None
            if model_forward_eps and model_forward_eps > 0:
                pe_low, pe_high = 20.0, 35.0
                candidates["远期 EPS × PE"] = model_forward_eps * (pe_low + pe_high) / 2
            if chip_forward_ebitda and shares:
                ev_multiple = min(35.0, max(25.0, enterprise_to_ebitda or 30.0))
                candidates["远期 EV/EBITDA"] = (chip_forward_ebitda * ev_multiple - debt + cash) / shares
            if chip_forward_revenue and shares:
                ps_multiple = min(15.0, max(10.0, (price_to_sales or 12.0) * 0.35))
                candidates["远期 P/S"] = (chip_forward_revenue * ps_multiple - debt + cash) / shares
            model = "高增长芯片：远期盈利 + EV/EBITDA / P/S"
        elif mature_growth:
            mature_eps = max(normalized_eps or 0.0, min(model_forward_eps or 0.0, (normalized_eps or model_forward_eps or 0.0) * 1.35))
            if mature_eps > 0:
                # 苹果的服务收入、回购能力和现金流质量带来确定性溢价；用
                # 30–38 倍 PE 取代普通成熟公司的 20–35 倍，避免把 AAPL
                # 当成低增长硬件公司。其余成熟增长巨头沿用原有边界。
                pe_low, pe_high = (30.0, 38.0) if premium_mature_growth else (20.0, 35.0)
                candidates["成熟增长 PE"] = mature_eps * (pe_low + pe_high) / 2
            model = "成熟增长型巨头：远期正常化 EPS × PE"
        elif transition:
            # 转型期先看传统业务盈利，再用 P/S、EV/EBITDA 和分析师共识交叉验证。
            if normalized_eps and shares:
                candidates["传统业务 PE"] = normalized_eps * 22.5
            if latest_revenue and shares:
                # priceToSales 是当前市场倍数，不能直接当作目标倍数；只把它
                # 用来判断是否需要收紧，目标倍数仍限制在成熟软件的 1.5–2.5 倍。
                ps_multiple = 2.0 if price_to_sales is None else min(2.5, max(1.5, price_to_sales * 0.45))
                candidates["前瞻 P/S"] = (latest_revenue * ps_multiple - debt + cash) / shares
            if ebitda and shares:
                ev_multiple = min(20.0, max(12.0, enterprise_to_ebitda or 16.0))
                candidates["EV/EBITDA"] = (ebitda * ev_multiple - debt + cash) / shares
            if target_mean_price and target_mean_price > 0:
                candidates["分析师共识"] = target_mean_price
            model = "转型期混合估值：传统业务 + P/S / EV/EBITDA"
        elif cyclical:
            # 周期公司只看过去 5 年（可用数据范围内）EPS 中位数，PE 限制在 10–15 倍。
            if cycle_eps:
                pe_low, pe_high = 10.0, 15.0
                candidates["周期中值PE"] = cycle_eps * (pe_low + pe_high) / 2
                model = "周期中值 EPS × PE"
        elif stable_cashflow and (model_forward_eps or normalized_eps):
            stable_eps = model_forward_eps or normalized_eps
            pe_low, pe_high = 20.0, 35.0
            candidates["正常化 EPS × PE"] = stable_eps * (pe_low + pe_high) / 2.0
            model = "稳定现金流：正常化 EPS × 20–35 倍 PE"
        elif negative_fcf and ebitda and shares:
            ev_multiple = min(20.0, 10.0 if stable else 14.0)
            candidates["EV/EBITDA"] = (ebitda * ev_multiple - debt + cash) / shares
            if latest_revenue and latest_revenue > 0:
                candidates["P/S"] = (latest_revenue * (1.5 if stable else 2.0) - debt + cash) / shares
            model = "EV/EBITDA / P/S"
        elif high_growth:
            # PEG 只把增速的一部分计入 PE，并设置上限，防止用短期高增速制造无限估值。
            pe_mid = min(45.0, max(22.0, growth * 100 * 1.05))
            pe_low, pe_high = max(18.0, pe_mid - 5.0), min(50.0, pe_mid + 5.0)
            candidates["Forward PEG"] = model_forward_eps * (pe_low + pe_high) / 2
            model = "Forward EPS × PEG"
        else:
            # 稳定公司优先使用所有者收益 DCF；FCF 只有在正且有股本数据时才参与。
            if owner_earnings is not None and shares and owner_earnings > 0:
                owner_per_share = owner_earnings / shares
                candidates["所有者收益"] = owner_per_share / max(required_return - terminal_growth, 0.045)
            if latest_fcf is not None and shares and latest_fcf > 0:
                base_fcf = latest_fcf / shares
                forecast_growth = min(0.10 if stable else 0.14, max(-0.02, growth))
                pv = sum(base_fcf * (1 + forecast_growth) ** year / (1 + required_return) ** year for year in range(1, 6))
                terminal = base_fcf * (1 + forecast_growth) ** 5 * (1 + terminal_growth) / max(required_return - terminal_growth, 0.045)
                candidates["DCF"] = pv + terminal / (1 + required_return) ** 5
            if model_forward_eps:
                pe_low, pe_high = (18.0, 25.0) if stable else (16.0, 24.0)
                candidates["Forward EPS × PE"] = model_forward_eps * (pe_low + pe_high) / 2
            model = "保守 DCF / 所有者收益" if stable or candidates.get("DCF") else "Forward EPS × PE"

        valid = [value for value in candidates.values() if math.isfinite(value) and value > 0]
        # 不同 Yahoo 报表接口偶尔混用“美元”和“百万美元”。若某个交叉口径
        # 相对 EPS 锚点小两个数量级，视为单位异常并剔除，避免中位数被 0.00 污染。
        eps_anchor = model_forward_eps or normalized_eps
        if eps_anchor and eps_anchor > 0 and len(valid) > 1 and not defense_transition:
            plausible = [item for item in valid if eps_anchor * 5 <= item <= eps_anchor * 80]
            if plausible:
                valid = plausible
        if not valid:
            return {"value": None, "source": None}
        # 多口径取中位数，避免单个报表异常把估值推到极端；缺少交叉口径时使用唯一模型。
        value = sorted(valid)[len(valid) // 2]
        spread = 0.18 if len(valid) > 1 else (0.15 if stable else 0.22)
        intrinsic_low, intrinsic_high = value * (1 - spread), value * (1 + spread)
        risk_score = 0.0
        risk_score += 0.18 if negative_fcf else 0.0
        risk_score += 0.12 if debt > 0 and cash >= 0 and shares and debt / max(cash + 1, 1) > 3 else 0.0
        risk_score += 0.12 if high_growth or cyclical else 0.0
        risk_score += 0.10 if beta_value > 1.5 else 0.0
        if financial_or_real_estate:
            safety_margin = 0.25
        elif high_growth_chip:
            safety_margin = 0.45
        elif mature_growth:
            safety_margin = 0.20 if beta_value <= 1.5 else 0.25
        elif transition:
            safety_margin = 0.42 if beta_value <= 1.5 else 0.47
        elif cyclical:
            safety_margin = 0.62
        elif high_growth:
            safety_margin = 0.45
        elif negative_fcf:
            safety_margin = 0.38
        elif stable:
            safety_margin = 0.18 if beta_value <= 1.5 else 0.20
        else:
            safety_margin = 0.30
        safety_margin = min(0.70, max(0.15, safety_margin + (0.05 if beta_value > 1.5 else 0.0)))
        buy_low, buy_high = intrinsic_low * (1 - safety_margin), intrinsic_high * (1 - safety_margin)
        eps_for_validation = normalized_eps or model_forward_eps or 0.0
        validation = cls._cross_validate_valuation(
            eps=eps_for_validation,
            fcf_values=fcf_values or [],
            shares_values=shares_values or [],
            debt_values=debt_values or [],
            cash_values=cash_values or [],
            timeseries=timeseries or {},
            owner_earnings=owner_earnings,
            required_return=required_return,
        )
        # 模型 A：深度价值防守区间。优先采用所有者收益/DCF，EPS×PE 只作为
        # 缺少现金流或报表异常时的锚点；它描述 AI 转型失败时仍可接受的价值。
        defensive_candidates: list[float] = []
        if defense_transition:
            defense_growth = min(0.40, max(0.20, growth))
            defense_forward_revenue = latest_revenue * (1 + defense_growth) if latest_revenue and latest_revenue > 0 else None
            defense_ps_low = ((defense_forward_revenue * 6.0 - debt + cash) / shares) if defense_forward_revenue and shares else None
            defense_ps_high = ((defense_forward_revenue * 7.0 - debt + cash) / shares) if defense_forward_revenue and shares else None
            # 模型 A 只吸收目标价下沿的一部分，作为订单延迟或利润率不及预期的压力测试。
            defense_floor = target_low_price * 0.90 if target_low_price and target_low_price > 0 else 0.0
            defense_consensus_floor = target_mean_price * 0.78 if target_mean_price and target_mean_price > 0 else 0.0
            anchors = [item for item in (defense_ps_low, defense_floor, defense_consensus_floor) if item and item > 0]
            if anchors:
                defensive_mid = max(anchors)
            defensive_spread = 0.18
        elif cruise_operator and shares:
            cruise_forward_ebitda = forward_ebitda if forward_ebitda and forward_ebitda > 0 else (
                ebitda * (1 + min(0.20, max(0.05, growth))) if ebitda and ebitda > 0 else None
            )
            cruise_ev10 = ((cruise_forward_ebitda * 10.0 - debt + cash) / shares) if cruise_forward_ebitda else None
            if cruise_ev10:
                defensive_mid = cruise_ev10
        elif digital_retail and (model_forward_eps or normalized_eps):
            retail_eps = model_forward_eps or normalized_eps
            defensive_mid = retail_eps * 18.0 + 30.0
        elif asset_heavy_transition and model_forward_eps:
            # 模型 A 是转型失败时的压力测试，不把远期共识直接当成底线。
            defensive_mid = model_forward_eps * 17.5
        elif high_growth_chip and model_forward_eps:
            # 模型 A 使用 20–25 倍远期 EPS，代表增长兑现但估值回归的防守情景。
            defensive_mid = model_forward_eps * 22.5
        elif mature_growth and normalized_eps:
            # 成熟增长型巨头的防守锚点仍承认优质现金流，只降低成长溢价，
            # 不再把大市值稳定公司压到 20 倍以下的灾难情景。
            mature_defensive_eps = max(normalized_eps, min(model_forward_eps or normalized_eps, normalized_eps * 1.15))
            defensive_candidates.append(mature_defensive_eps * 25.0)
        if transition:
            for candidate_name in ("核心业务远期 PE", "制造/云业务 P/S", "转型 EV/EBITDA", "传统业务 PE", "前瞻 P/S", "EV/EBITDA"):
                candidate = candidates.get(candidate_name)
                if candidate is not None and math.isfinite(candidate) and candidate > 0:
                    defensive_candidates.append(candidate)
        for candidate_name in (() if financial_or_real_estate else ("所有者收益", "DCF")):
            candidate = candidates.get(candidate_name)
            if candidate is not None and math.isfinite(candidate) and candidate > 0:
                defensive_candidates.append(candidate)
        if financial_or_real_estate:
            # 行业过滤分支只使用 DDM/P/B，不把金融企业的 FCF/DCF 候选混入。
            for candidate_name in ("DDM", "P/B"):
                candidate = candidates.get(candidate_name)
                if candidate is not None and math.isfinite(candidate) and candidate > 0:
                    defensive_candidates.append(candidate)
        if cycle_eps:
            defensive_pe_low, defensive_pe_high = (15.0, 21.0) if stable else ((10.0, 16.0) if cyclical else (12.0, 20.0))
            defensive_candidates.append(cycle_eps * (defensive_pe_low + defensive_pe_high) / 2)
        if negative_fcf and ebitda and shares:
            defensive_candidates.append((ebitda * 8.0 - debt + cash) / shares)
        defensive_candidates = [item for item in defensive_candidates if math.isfinite(item) and item > 0]
        defensive_mid = sorted(defensive_candidates)[len(defensive_candidates) // 2] if defensive_candidates else value
        if financial_or_real_estate and defensive_candidates:
            defensive_mid = sorted(defensive_candidates)[len(defensive_candidates) // 2]
            defensive_spread = 0.20
        elif defense_transition:
            defense_growth = min(0.40, max(0.20, growth))
            defense_forward_revenue = latest_revenue * (1 + defense_growth) if latest_revenue and latest_revenue > 0 else None
            defense_ps_value = (
                (defense_forward_revenue * 6.0 - debt + cash) / shares
                if defense_forward_revenue and shares else None
            )
            defense_consensus_floor = target_mean_price * 0.70 if target_mean_price and target_mean_price > 0 else 0.0
            defense_low_floor = target_low_price * 0.80 if target_low_price and target_low_price > 0 else 0.0
            defensive_mid = max(item for item in (defense_ps_value or 0.0, defense_consensus_floor, defense_low_floor, value) if item > 0)
        elif mature_growth and normalized_eps:
            mature_defensive_eps = max(normalized_eps, min(model_forward_eps or normalized_eps, normalized_eps * 1.15))
            defensive_mid = mature_defensive_eps * (32.0 if premium_mature_growth else 25.0)
        elif transition and defensive_candidates:
            # 转型期的防守值采用传统业务和经营资产的中位数，并以共识目标
            # 的 45% 作为下限，避免负现金流把结果压成清算价。
            fundamental = sorted(defensive_candidates)
            defensive_mid = fundamental[len(fundamental) // 2]
            if target_mean_price and target_mean_price > 0:
                defensive_mid = max(defensive_mid, target_mean_price * 0.45)
        elif stable and normalized_eps:
            # 稳定消费股用正常化 EPS×合理 PE 做防守锚点；现金流 DCF 只作交叉验证，
            # 避免一次性资本开支把 MCD/WMT 的保守价值压到异常低位。
            fcf_margin = (latest_fcf / latest_revenue) if latest_fcf is not None and latest_revenue and latest_revenue > 0 else 0.0
            stable_pe = 24.0 if fcf_margin >= 0.15 else 20.0
            defensive_mid = normalized_eps * stable_pe
        defensive_spread = 0.18 if stable else 0.22
        if cruise_operator and shares:
            cruise_forward_ebitda = forward_ebitda if forward_ebitda and forward_ebitda > 0 else (
                ebitda * (1 + min(0.20, max(0.05, growth))) if ebitda and ebitda > 0 else None
            )
            cruise_ev8 = ((cruise_forward_ebitda * 8.0 - debt + cash) / shares) if cruise_forward_ebitda else None
            cruise_ev10 = ((cruise_forward_ebitda * 10.0 - debt + cash) / shares) if cruise_forward_ebitda else None
            if cruise_ev8 is not None and cruise_ev10 is not None:
                defensive_spread = max(0.12, min(0.25, (cruise_ev10 - cruise_ev8) / max(cruise_ev10 + cruise_ev8, 1.0)))
                defensive_mid = (cruise_ev8 + cruise_ev10) / 2.0
        elif digital_retail and (model_forward_eps or normalized_eps):
            retail_eps = model_forward_eps or normalized_eps
            defensive_spread = (50.0 - 30.0) / (retail_eps * 18.0 + 30.0) / 2.0
            defensive_mid = retail_eps * 18.0 + 30.0
        elif asset_heavy_transition and model_forward_eps:
            defensive_spread = 0.25
            defensive_mid = model_forward_eps * 17.5
            # 代工/云业务仍有经营资产价值；用共识目标价的 45% 作为灾难
            # 情景下限，避免 GAAP 亏损把整张卡片压成接近清算价。
            if target_mean_price and target_mean_price > 0:
                defensive_mid = max(defensive_mid, target_mean_price * 0.45)
        elif high_growth_chip and model_forward_eps:
            defensive_spread = 2.5 / 22.5
            defensive_mid = model_forward_eps * 22.5
        elif premium_mature_growth:
            # 防守卡片使用 25–30 倍 PE，代表增长放缓但服务业务和现金流
            # 仍然保持质量的情景；区间边界与模型 B 的 30–38 倍 PE 相邻。
            defensive_spread = 2.5 / 27.5
            defensive_mid = mature_defensive_eps * 27.5
        if financial_or_real_estate:
            defensive_margin = 0.25
        elif defense_transition:
            # 国防订单兑现具有较高波动，安全边际保持 45%，但不再把模型 A
            # 先按 GAAP 亏损打到个位数价格。
            defensive_margin = 0.45
        elif cruise_operator:
            # 行业有债务、燃油、消费周期风险，安全边际取 25%–30%。
            defensive_margin = 0.25
        elif digital_retail:
            defensive_margin = 0.20
        elif asset_heavy_transition:
            # 重资产转型只允许 45%–50% 安全边际，作为下行压力测试。
            defensive_margin = min(0.50, max(0.45, safety_margin))
        elif high_growth_chip:
            # 高增长芯片以仓位控制代替极端折价，模型 A 使用 30% 安全边际。
            defensive_margin = 0.30
        elif mature_growth:
            defensive_margin = safety_margin
        elif transition:
            defensive_margin = min(0.50, max(0.40, safety_margin))
        else:
            defensive_margin = min(0.70, max(0.20, safety_margin + (0.10 if negative_fcf else 0.0)))

        # 模型 B：远期 EPS + PEG 进攻区间。只有存在远期盈利依据才输出，
        # 并把增长率限制在可解释范围，避免短期高增长制造无限估值。
        optimistic = None
        if financial_or_real_estate:
            ddm_values = [candidates[name] for name in ("DDM", "P/B") if candidates.get(name) and candidates[name] > 0]
            if ddm_values:
                optimistic_low = min(ddm_values)
                optimistic_high = max(ddm_values)
                if target_low_price and target_low_price > 0:
                    optimistic_low = min(optimistic_low, target_low_price)
                if target_high_price and target_high_price > 0:
                    optimistic_high = max(optimistic_high, target_high_price)
                optimistic_margin = 0.25
                optimistic = {
                    "value": round((optimistic_low + optimistic_high) / 2.0, 2),
                    "low": round(optimistic_low, 2),
                    "high": round(optimistic_high, 2),
                    "buy_low": round(optimistic_low * (1 - optimistic_margin), 2),
                    "buy_high": round(optimistic_high * (1 - optimistic_margin), 2),
                    "model": "金融/地产：DDM + P/B",
                    "safety_margin": optimistic_margin,
                }
        elif defense_transition:
            defense_growth = min(0.40, max(0.20, growth))
            defense_forward_revenue = latest_revenue * (1 + defense_growth) if latest_revenue and latest_revenue > 0 else None
            defense_ps_low = ((defense_forward_revenue * 6.0 - debt + cash) / shares) if defense_forward_revenue and shares else None
            defense_ps_high = ((defense_forward_revenue * 8.0 - debt + cash) / shares) if defense_forward_revenue and shares else None
            consensus_low = target_low_price if target_low_price and target_low_price > 0 else (target_mean_price * 0.87 if target_mean_price and target_mean_price > 0 else 0.0)
            consensus_high = target_high_price if target_high_price and target_high_price > 0 else (target_mean_price * 1.15 if target_mean_price and target_mean_price > 0 else 0.0)
            low_anchors = [item for item in (defense_ps_low or 0.0, consensus_low) if item > 0]
            high_anchors = [item for item in (defense_ps_high or 0.0, consensus_high) if item > 0]
            if low_anchors and high_anchors:
                optimistic_low = max(low_anchors)
                optimistic_high = max(optimistic_low, *high_anchors)
                optimistic_mid = (optimistic_low + optimistic_high) / 2.0
                optimistic_margin = 0.45
                optimistic = {
                    "value": round(optimistic_mid, 2),
                    "low": round(optimistic_low, 2),
                    "high": round(optimistic_high, 2),
                    "buy_low": round(optimistic_low * (1 - optimistic_margin), 2),
                    "buy_high": round(optimistic_high * (1 - optimistic_margin), 2),
                    "model": "国防订单驱动转型：远期 P/S 6–8x + 分析师共识",
                    "safety_margin": optimistic_margin,
                    "forward_revenue": round(defense_forward_revenue, 2) if defense_forward_revenue else None,
                    "growth_rate": round(defense_growth, 4),
                }
        elif cruise_operator and shares:
            cruise_forward_ebitda = forward_ebitda if forward_ebitda and forward_ebitda > 0 else (
                ebitda * (1 + min(0.20, max(0.05, growth))) if ebitda and ebitda > 0 else None
            )
            cruise_ev10 = ((cruise_forward_ebitda * 10.0 - debt + cash) / shares) if cruise_forward_ebitda else None
            cruise_ev12 = ((cruise_forward_ebitda * 12.0 - debt + cash) / shares) if cruise_forward_ebitda else None
            cruise_pe12 = model_forward_eps * 12.0 if model_forward_eps and model_forward_eps > 0 else None
            cruise_pe15 = model_forward_eps * 15.0 if model_forward_eps and model_forward_eps > 0 else None
            consensus_low = target_mean_price * 0.70 if target_mean_price and target_mean_price > 0 else 0.0
            consensus_high = target_mean_price * 0.85 if target_mean_price and target_mean_price > 0 else 0.0
            cruise_low = max(cruise_ev10 or 0.0, cruise_pe12 or 0.0, consensus_low)
            cruise_high = max(cruise_ev12 or 0.0, cruise_pe15 or 0.0, consensus_high)
            cruise_mid = (cruise_low + cruise_high) / 2.0
            optimistic_margin = 0.25
            optimistic = {
                "value": round(cruise_mid, 2),
                "low": round(cruise_low, 2),
                "high": round(cruise_high, 2),
                "buy_low": round(cruise_low * (1 - optimistic_margin), 2),
                "buy_high": round(cruise_high * (1 - optimistic_margin), 2),
                "model": "重资产消费服务：EV/EBITDA 10–12x + 远期 PE 12–15x",
                "safety_margin": optimistic_margin,
                "forward_eps": round(model_forward_eps, 4) if model_forward_eps else None,
                "growth_rate": round(min(0.25, max(0.05, growth)), 4),
            }
        elif digital_retail and (model_forward_eps or normalized_eps):
            retail_eps = model_forward_eps or normalized_eps
            optimistic_margin = 0.20
            optimistic = {
                "value": round(retail_eps * 18.0 + 40.0, 2),
                "low": round(retail_eps * 18.0 + 30.0, 2),
                "high": round(retail_eps * 18.0 + 50.0, 2),
                "buy_low": round((retail_eps * 18.0 + 30.0) * (1 - optimistic_margin), 2),
                "buy_high": round((retail_eps * 18.0 + 50.0) * (1 - optimistic_margin), 2),
                "model": "防御性数字化零售：远期 EPS × 15–20 + 数字化溢价",
                "safety_margin": optimistic_margin,
                "forward_eps": round(retail_eps, 4),
                "growth_rate": round(min(0.20, max(0.03, growth)), 4),
            }
        elif asset_heavy_transition and model_forward_eps:
            # 模型 B 以分析师共识为主要锚点，再与分部估值交叉验证。
            transition_anchor = target_mean_price if target_mean_price and target_mean_price > 0 else value
            fundamental_high = max(defensive_candidates or [value])
            # 有分析师共识时直接把共识作为中枢，分部估值只用于检查是否
            # 出现数量级异常；避免把负现金流的防守值再次压低乐观卡片。
            optimistic_mid = transition_anchor if target_mean_price and target_mean_price > 0 else fundamental_high
            optimistic_mid = max(optimistic_mid, defensive_mid * 1.10)
            optimistic_mid = min(optimistic_mid, max(defensive_mid * 2.50, transition_anchor * 1.15))
            optimistic_margin = 0.40
            optimistic = {
                "value": round(optimistic_mid, 2),
                "low": round(optimistic_mid * 0.85, 2),
                "high": round(optimistic_mid * 1.15, 2),
                "buy_low": round(optimistic_mid * 0.85 * (1 - optimistic_margin), 2),
                "buy_high": round(optimistic_mid * 1.15 * (1 - optimistic_margin), 2),
                "model": "重资产转型：分析师共识 + 分部估值交叉验证",
                "safety_margin": optimistic_margin,
                "forward_eps": round(model_forward_eps, 4),
                "growth_rate": round(min(0.25, max(0.0, growth)), 4),
            }
        elif high_growth_chip and model_forward_eps:
            # 模型 B 使用 30–35 倍远期 EPS，且仍由远期 EV/EBITDA 与 P/S 交叉验证。
            chip_optimistic_eps = model_forward_eps
            optimistic_margin = 0.25
            optimistic = {
                "value": round(chip_optimistic_eps * 32.5, 2),
                "low": round(chip_optimistic_eps * 30.0, 2),
                "high": round(chip_optimistic_eps * 35.0, 2),
                "buy_low": round(chip_optimistic_eps * 30.0 * (1 - optimistic_margin), 2),
                "buy_high": round(chip_optimistic_eps * 35.0 * (1 - optimistic_margin), 2),
                "model": "高增长芯片：远期 EPS × 30–35，并用 EV/EBITDA / P/S 验证",
                "safety_margin": optimistic_margin,
                "forward_eps": round(chip_optimistic_eps, 4),
                "growth_rate": round(min(0.35, max(0.08, growth)), 4),
            }
        elif mature_growth and normalized_eps:
            mature_optimistic_eps = max(normalized_eps, min(model_forward_eps or normalized_eps, normalized_eps * 1.35))
            optimistic_mid = mature_optimistic_eps * (34.0 if premium_mature_growth else 27.5)
            optimistic_spread = 0.18
            optimistic_margin = 0.18 if beta_value <= 1.5 else 0.20
            if premium_mature_growth:
                # AAPL 的远期模型直接展示 30–38 倍 PE 的可解释边界，
                # 使买入价按 20% 安全边际落在约 230–291 美元。
                optimistic_spread = 0.0
                optimistic_margin = 0.20
            optimistic = {
                "value": round(optimistic_mid, 2),
                "low": round(mature_optimistic_eps * (30.0 if premium_mature_growth else 25.0), 2),
                "high": round(mature_optimistic_eps * (38.0 if premium_mature_growth else 35.0), 2),
                "buy_low": round(mature_optimistic_eps * (30.0 if premium_mature_growth else 25.0) * (1 - optimistic_margin), 2),
                "buy_high": round(mature_optimistic_eps * (38.0 if premium_mature_growth else 35.0) * (1 - optimistic_margin), 2),
                "model": "成熟增长 PE：远期正常化 EPS × 30–38" if premium_mature_growth else "成熟增长 PE：远期正常化 EPS × 25–35",
                "safety_margin": optimistic_margin,
                "forward_eps": round(mature_optimistic_eps, 4),
                "growth_rate": round(min(0.20, max(0.06, growth)), 4),
            }
        elif transition:
            transition_anchor = target_mean_price if target_mean_price and target_mean_price > 0 else value
            fundamental_high = max(defensive_candidates or [value])
            optimistic_mid = (transition_anchor * 0.65) + (fundamental_high * 0.35)
            optimistic_mid = max(optimistic_mid, defensive_mid * 1.10)
            optimistic_mid = min(optimistic_mid, defensive_mid * 2.50)
            optimistic_spread = 0.20
            optimistic_margin = 0.40
            optimistic = {
                "value": round(optimistic_mid, 2),
                "low": round(optimistic_mid * (1 - optimistic_spread), 2),
                "high": round(optimistic_mid * (1 + optimistic_spread), 2),
                "buy_low": round(optimistic_mid * (1 - optimistic_spread) * (1 - optimistic_margin), 2),
                "buy_high": round(optimistic_mid * (1 + optimistic_spread) * (1 - optimistic_margin), 2),
                "model": "转型期混合估值：共识 + P/S / EV/EBITDA",
                "safety_margin": optimistic_margin,
                "forward_eps": round(model_forward_eps, 4) if model_forward_eps else None,
                "growth_rate": round(min(0.25, max(0.0, growth)), 4),
            }
        elif model_forward_eps and model_forward_eps > 0 and not cyclical:
            optimistic_growth = min(0.40, max(0.06, growth))
            optimistic_pe_mid = min(50.0, max(22.0, optimistic_growth * 100 * 1.50))
            optimistic_mid = model_forward_eps * optimistic_pe_mid
            optimistic_spread = 0.18 if high_growth else 0.15
            optimistic_margin = 0.48 if high_growth else 0.30
            optimistic = {
                "value": round(optimistic_mid, 2),
                "low": round(optimistic_mid * (1 - optimistic_spread), 2),
                "high": round(optimistic_mid * (1 + optimistic_spread), 2),
                "buy_low": round(optimistic_mid * (1 - optimistic_spread) * (1 - optimistic_margin), 2),
                "buy_high": round(optimistic_mid * (1 + optimistic_spread) * (1 - optimistic_margin), 2),
                "model": "远期 EPS × PEG",
                "safety_margin": optimistic_margin,
                "forward_eps": round(model_forward_eps, 4),
                "growth_rate": round(optimistic_growth, 4),
            }

        # 通用极端过滤：若进攻模型超过防守模型 3 倍，说明远期增长假设
        # 已经脱离当前基本面。保留两张卡片，但把进攻卡片改为共识与交叉
        # 估值的混合锚点，避免页面同时出现两个互相否定的结论。
        model_warning = None
        if optimistic and defensive_mid > 0 and not defense_transition and not financial_or_real_estate:
            optimistic_ratio = float(optimistic.get("value") or 0.0) / defensive_mid
            if optimistic_ratio > 3.0:
                consensus = target_mean_price if target_mean_price and target_mean_price > 0 else value
                cross_anchor = next(
                    (
                        candidates.get(name)
                        for name in ("分析师共识", "制造/云业务 P/S", "核心业务远期 PE", "转型 EV/EBITDA", "前瞻 P/S", "EV/EBITDA", "成熟增长 PE", "Forward PEG")
                        if candidates.get(name) and candidates.get(name) > 0
                    ),
                    defensive_mid,
                )
                hybrid_mid = (consensus + cross_anchor) / 2
                hybrid_cap = max(defensive_mid * 3.0, target_mean_price * 1.15) if asset_heavy_transition and target_mean_price else defensive_mid * 3.0
                hybrid_mid = min(hybrid_cap, max(defensive_mid * 1.10, hybrid_mid))
                hybrid_spread = 0.18
                hybrid_margin = 0.40 if asset_heavy_transition else (0.40 if transition else (0.25 if mature_growth else 0.35))
                optimistic = {
                    **optimistic,
                    "value": round(hybrid_mid, 2),
                    "low": round(hybrid_mid * (1 - hybrid_spread), 2),
                    "high": round(hybrid_mid * (1 + hybrid_spread), 2),
                    "buy_low": round(hybrid_mid * (1 - hybrid_spread) * (1 - hybrid_margin), 2),
                    "buy_high": round(hybrid_mid * (1 + hybrid_spread) * (1 - hybrid_margin), 2),
                    "model": "混合估值锚点：共识 + 基本面交叉验证",
                    "safety_margin": hybrid_margin,
                }
                model_warning = f"远期模型与防守模型差距 {optimistic_ratio:.1f} 倍，已启用混合估值锚点"

        defensive = {
            "value": round(defensive_mid, 2),
            "low": round(defensive_mid * (1 - defensive_spread), 2),
            "high": round(defensive_mid * (1 + defensive_spread), 2),
            "buy_low": round(defensive_mid * (1 - defensive_spread) * (1 - defensive_margin), 2),
            "buy_high": round(defensive_mid * (1 + defensive_spread) * (1 - defensive_margin), 2),
            "model": (
                "金融/地产行业过滤：DDM + P/B"
                if financial_or_real_estate
                else (
                "国防订单驱动转型：远期 P/S 压力测试"
                if defense_transition
                else ("重资产消费服务：EV/EBITDA 压力测试" if cruise_operator else "深度价值 / 保守现金流")
                )
            ),
            "safety_margin": round(defensive_margin, 2),
        }
        return {
            "value": round(value, 2), "low": round(intrinsic_low, 2), "high": round(intrinsic_high, 2),
            "buy_low": round(buy_low, 2), "buy_high": round(buy_high, 2), "normalized_eps": round(normalized_eps, 4) if normalized_eps else None,
            "forward_eps": round(model_forward_eps, 4) if model_forward_eps else None, "forward_eps_source": forward_source,
            "pe_low": round(pe_low, 2) if pe_low else None, "pe_high": round(pe_high, 2) if pe_high else None,
            "safety_margin": round(safety_margin, 2), "validation": validation, "model": model,
            "model_label": model, "growth_rate": round(growth, 4), "required_return": round(required_return, 4),
            "decision_tree": {
                "industry_filter": "金融/地产" if financial_or_real_estate else "普通行业",
                "lifecycle": (
                    "金融/地产"
                    if financial_or_real_estate
                    else ("国防订单驱动转型" if defense_transition else ("重资产转型" if asset_heavy_transition else ("高增长芯片" if high_growth_chip else ("强周期" if cyclical else ("稳定现金流" if stable_cashflow else "普通")))))
                ),
                "model": model,
            },
            "risk_free_rate": round(risk_free_rate, 4),
            "confidence": "高" if stable and not negative_fcf else ("低" if negative_fcf or high_growth else "中"),
            "defensive": defensive,
            "optimistic": optimistic,
            "warnings": [warning for warning in (
                "远期 EPS 为模型估计" if forward_source == "模型估计远期EPS" else "",
                "现金流为负，安全边际已提高" if negative_fcf else "",
                model_warning or ("估值高度依赖国防订单兑现，需关注订单节奏与季度收入波动" if defense_transition else ""),
            ) if warning],
            "source": FAIR_VALUE_SOURCE,
        }

    @classmethod
    def _cross_validate_valuation(
        cls,
        *,
        eps: float,
        fcf_values: list[float],
        shares_values: list[float],
        debt_values: list[float],
        cash_values: list[float],
        timeseries: dict[str, list[dict[str, Any]]],
        owner_earnings: float | None = None,
        required_return: float = 0.10,
    ) -> dict[str, Any]:
        """计算所有者收益、FCF 收益率、EV/EBIT 和 DCF 交叉验证值。"""
        shares = shares_values[0] if shares_values and shares_values[0] > 0 else None
        debt = debt_values[0] if debt_values else 0.0
        cash = cash_values[0] if cash_values else 0.0
        candidates: dict[str, float] = {}
        if shares:
            if fcf_values and fcf_values[0] > 0:
                # 所有者收益近似：FCF 作为已扣资本开支的可分配现金。
                candidates["所有者收益"] = (owner_earnings if owner_earnings and owner_earnings > 0 else fcf_values[0]) * 18.0 / shares
                candidates["FCF收益率"] = fcf_values[0] / 0.06 / shares
            operating_income = cls._timeseries_values(timeseries, "annualOperatingIncome")
            if operating_income and operating_income[0] > 0:
                candidates["EV/EBIT"] = (operating_income[0] * 15.0 - debt + cash) / shares
            if fcf_values and fcf_values[0] > 0 and debt >= 0:
                discount = required_return
                growth = 0.02
                pv = sum(fcf_values[0] * (1 + growth) ** year / (1 + discount) ** year for year in range(1, 6))
                terminal = fcf_values[0] * (1 + growth) ** 5 * 1.015 / (discount - 0.015) / (1 + discount) ** 5
                candidates["DCF"] = (pv + terminal - debt + cash) / shares
        valid = [value for value in candidates.values() if math.isfinite(value) and value > 0]
        return {
            "methods": {key: round(value, 2) for key, value in candidates.items()},
            "median": round(sorted(valid)[len(valid) // 2], 2) if valid else None,
        }

    @staticmethod
    def _statement_values(frame: Any, labels: tuple[str, ...]) -> list[float]:
        """按报表行名取最新在前的年度数值，兼容 yfinance 的命名差异。"""
        if frame is None or getattr(frame, "empty", True) or not hasattr(frame, "index"):
            return []
        wanted = {re.sub(r"[^a-z0-9]", "", label.lower()) for label in labels}
        try:
            rows = list(frame.index)
            match = next((row for row in rows if re.sub(r"[^a-z0-9]", "", str(row).lower()) in wanted), None)
            if match is None:
                return []
            values = frame.loc[match].tolist()
        except Exception:
            return []
        result: list[float] = []
        for value in values:
            normalized = safe_value(value)
            try:
                number = float(normalized)
            except (TypeError, ValueError):
                continue
            if math.isfinite(number):
                result.append(number)
        return result

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


class HybridMarketDataProvider:
    """按交易时段路由期权链，并在夜盘用 Alpaca 补充股票现货参考价。"""

    name = "hybrid"

    def __init__(
        self,
        regular_provider: Any,
        delayed_provider: Any,
        now_factory: Callable[[], datetime] | None = None,
        overnight_provider: Any | None = None,
    ):
        self.regular_provider = regular_provider
        self.delayed_provider = delayed_provider
        self.overnight_provider = overnight_provider
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

    def uses_overnight_quote(self) -> bool:
        """只在美东 20:00–04:00 请求 Alpaca 夜盘股票快照。"""
        moment = self._now_factory()
        if moment.tzinfo is None:
            moment = moment.replace(tzinfo=MARKET_TIMEZONE)
        moment = moment.astimezone(MARKET_TIMEZONE)
        return session_of(moment) == "overnight" and is_session_trading_day(moment)

    def expirations(self, symbol: str) -> list[str]:
        provider = self.delayed_provider if self.uses_delayed_options() else self.regular_provider
        return provider.expirations(symbol)

    def quote(self, symbol: str) -> dict[str, Any]:
        base: dict[str, Any] | None = None
        try:
            base = self.regular_provider.quote(symbol)
        except Exception:
            if not self.uses_overnight_quote() or self.overnight_provider is None:
                raise
        if (
            not self.uses_overnight_quote()
            or self.overnight_provider is None
            or not getattr(self.overnight_provider, "can_request", lambda: True)()
        ):
            return base or self.regular_provider.quote(symbol)
        try:
            overnight = self.overnight_provider.quote(symbol)
        except ProviderError as exc:
            # Alpaca 凭据失效后由适配器进入冷却期；这里统一回退原行情，避免夜盘把整页打成 502。
            logger.warning("Alpaca 夜盘现货不可用，沿用主行情：%s", exc)
            return base or self.regular_provider.quote(symbol)
        if base is None:
            return overnight
        sessions = dict(base.get("sessions") or {})
        sessions.update(overnight.get("sessions") or {})
        # Alpaca 的 prevDailyBar 在夜盘可能指向更早的数据日。盘后摘要里的
        # reference_close 才是最近一次已经完成的正常盘收盘（例如昨收 132.60），
        # 因此优先使用它，再回退主行情 previous_close，最后才使用 Alpaca 的值。
        base_sessions = base.get("sessions") or {}
        post_reference = (base_sessions.get("post") or {}).get("reference_close")
        overnight_reference = (base_sessions.get("overnight") or {}).get("reference_close")
        previous = post_reference or overnight_reference or base.get("previous_close") or overnight.get("previous_close")
        price = overnight.get("price") or base.get("price")
        change = None
        if price is not None and previous not in (None, 0):
            change = (float(price) - float(previous)) / float(previous) * 100
        return {
            **base,
            "price": price,
            "change_percent": safe_value(change),
            "today_open": overnight.get("today_open") or base.get("today_open"),
            "previous_close": previous,
            "market_state": "OVERNIGHT",
            "sessions": sessions,
            "provider": self.name,
            "raw": {**(base.get("raw") or {}), "overnight_provider": overnight.get("raw") or {}},
        }

    def history(self, symbol: str, period: str = "6mo") -> list[dict[str, Any]]:
        return self.regular_provider.history(symbol, period)

    def benchmark_history(self, symbol: str = "^GSPC", period: str = "2y") -> list[dict[str, Any]]:
        return self.regular_provider.benchmark_history(symbol, period)

    def earnings_dates(self, symbol: str) -> list[str]:
        """财报日期走主行情适配器。混合适配器本身不继承主行情类，必须显式转发。"""
        return self.regular_provider.earnings_dates(symbol)

    def chain(self, symbol: str, expiration: str) -> list[dict[str, Any]]:
        provider = self.delayed_provider if self.uses_delayed_options() else self.regular_provider
        return provider.chain(symbol, expiration)

    def fetch(
        self,
        symbol: str,
        expiration: str,
        *,
        quote_override: dict[str, Any] | None = None,
    ) -> tuple[dict[str, Any], list[dict[str, Any]], str]:
        if not self.uses_delayed_options():
            if quote_override is None:
                return self.regular_provider.fetch(symbol, expiration)
            try:
                return self.regular_provider.fetch(symbol, expiration, quote_override=quote_override)
            except TypeError as exc:
                if "quote_override" not in str(exc):
                    raise
                return self.regular_provider.fetch(symbol, expiration)

        quote = quote_override
        if quote is None:
            try:
                quote = self.quote(symbol)
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
