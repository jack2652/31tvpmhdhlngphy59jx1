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

logger = logging.getLogger(__name__)


def _valuation_debug(message: str, *args: Any) -> None:
    """仅在估值调试开关开启时输出逐标的诊断日志。"""
    if VALUATION_CONFIG.get("debug"):
        logger.info(message, *args)


def _is_transient_upstream_error(exc: BaseException) -> bool:
    """判断上游错误是否值得稍后重试。

    Yahoo 的 403/429、超时和连接重置通常是限流或短暂网络问题，不能直接
    当成“该标的没有财务数据”。保留字符串兜底是因为 yfinance 会把 HTTP
    异常包装成普通 RuntimeError。
    """
    if isinstance(exc, HTTPError) and exc.code in {403, 408, 425, 429, 500, 502, 503, 504}:
        return True
    if isinstance(exc, (TimeoutError, URLError)):
        return True
    text = str(exc).lower()
    return any(
        marker in text
        for marker in (
            "429",
            "403",
            "too many requests",
            "rate limit",
            "timed out",
            "timeout",
            "temporarily unavailable",
            "service unavailable",
            "connection reset",
            "connection aborted",
        )
    )


def _record_upstream_error(errors: list[str], source: str, exc: BaseException) -> None:
    """把上游失败压缩成不含请求细节的结构化原因。"""
    suffix = "rate_limited" if _is_transient_upstream_error(exc) else "unavailable"
    errors.append(f"{source}_{suffix}")

# 美股时段划分（美东时间）：盘前 04:00–09:30、盘中 09:30–16:00、盘后 16:00–20:00、夜盘 20:00–04:00。
MARKET_TIMEZONE = ZoneInfo("America/New_York")
SESSION_STATES = {"pre": "PRE", "regular": "REGULAR", "post": "POST", "overnight": "OVERNIGHT"}
# 最近一根 K 线超过该秒数就认为已经离开该时段，改按美东时钟判断。
SESSION_FRESH_SECONDS = 30 * 60
# 现货附加分钟线只是展示盘前/盘后摘要。给上游 SDK 一个有限等待时间，避免
# yfinance 网络异常把请求线程长期挂住，进而叠加期权链和历史任务。
UPSTREAM_REQUEST_TIMEOUT_SECONDS = 12
YAHOO_TIMESERIES_URL = "https://query1.finance.yahoo.com/ws/fundamentals-timeseries/v1/finance/timeseries/{symbol}"
FAIR_VALUE_SOURCE = f"valuation_{VALUATION_CONFIG['cache_version']}"


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
    # 只有分钟线已经包含当前交易日的正常盘时，才跳过当天并取前一交易日。
    # 数据源在当前盘中暂时没有新分钟线时，最新一根可能仍是昨天；此时不能
    # 再无条件删除它，否则会把昨收错误地退回到更早一天。夜盘例外：夜盘现价
    # 没有独立报价时通常就是刚结束的正常盘收盘，仍需取它的前一交易日作基准。
    skip_latest = state == "OVERNIGHT" or (
        state != "PRE" and days[-1] == session_trading_day(moment_now)
    )
    if skip_latest:
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
    """优先使用已识别的正常盘收盘，缺失时才回退上游昨收。"""
    official_number = _positive_price(official)
    minute_number = _positive_price(minute_close)
    if minute_number is None:
        return official if official_number is None else official_number
    # fast_info.previous_close 可能是滞后的近似值，即使只差几分钱也会让现货卡片
    # 与趋势通道的正式昨收不一致。分钟线已识别出正常盘收盘时，统一以它为基准。
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
    """从财报表取出可用于提示的未来/当天美东日期。

    ``get_earnings_dates`` 在财报发布后可能立即把当天的 ``Reported EPS`` 填上；
    如果仍按「有 EPS 就过滤」处理，盘后发布当天会被误判为没有财报。历史日期
    继续要求 EPS 未公布，避免把整张历史表误当成未来财报。
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
    today = datetime.now(MARKET_TIMEZONE).date().isoformat()
    for timestamp, row in rows:
        day = _earnings_new_york_date(timestamp)
        if not day:
            continue
        try:
            reported = row[column]
        except Exception:
            reported = None
        if day < today and _earnings_reported(reported):
            continue
        found.add(day)
    return sorted(found)


class MarketRegime:
    """根据可观察的宏观、行业和盈利修正信号识别市场状态。

    普通信号仍需至少两个独立指标确认；VIX、指数动量和 EPS 修正的极端值
    拥有单一信号否决权，以便在熔断式下跌中及时切换到寒冬估值。
    """

    BOOM = "BOOM"
    NORMAL = "NORMAL"
    SLOWDOWN = "SLOWDOWN"
    WINTER = "WINTER"

    @classmethod
    def detect(cls, info: dict[str, Any], benchmark_bars: list[dict[str, Any]] | None = None) -> tuple[str, dict[str, Any]]:
        thresholds = VALUATION_CONFIG["regime_thresholds"]
        signals: dict[str, Any] = {}

        def number(keys: tuple[str, ...]) -> float | None:
            for key in keys:
                value = safe_value(info.get(key))
                try:
                    parsed = float(value)
                except (TypeError, ValueError):
                    continue
                if math.isfinite(parsed):
                    return parsed
            return None

        tnx = number(("us10y", "treasuryYield", "riskFreeRate", "tenYearYield"))
        if tnx is not None and tnx > 1:
            tnx /= 100.0
        vix = number(("vix", "vixClose", "volatilityIndex"))
        revenue_growth = number(("revenueGrowth", "earningsGrowth"))
        eps_revision = number(("epsRevision1m", "epsRevisionOneMonth", "forwardEpsRevision", "analystEpsRevision"))
        capex_growth = number(("aiCapexGrowth", "capexGrowth", "capitalExpenditureGrowth"))
        inventory_days = number(("daysInventoryOutstanding", "inventoryDays", "daysInventory"))
        signals.update(
            tnx_yield=round(tnx, 4) if tnx is not None else None,
            vix=round(vix, 2) if vix is not None else None,
            revenue_growth=round(revenue_growth, 4) if revenue_growth is not None else None,
            eps_revision_1m=round(eps_revision, 4) if eps_revision is not None else None,
            capex_growth=round(capex_growth, 4) if capex_growth is not None else None,
            inventory_days=round(inventory_days, 2) if inventory_days is not None else None,
        )

        momentum = None
        if benchmark_bars and len(benchmark_bars) >= 60:
            closes = [safe_value(item.get("close")) for item in benchmark_bars]
            closes = [value for value in closes if value and value > 0]
            if len(closes) >= 60:
                recent = closes[-20:]
                earlier = closes[-60:-20]
                momentum = sum(recent) / len(recent) / (sum(earlier) / len(earlier)) - 1
        signals["benchmark_momentum_60d"] = round(momentum, 4) if momentum is not None else None

        # 极端单一信号优先于普通双信号门槛：2020 年式熔断中 VIX 可能已经
        # 飙升，而长债收益率同时下行，不能因为缺少第二个方向相同的信号而延迟保护。
        extreme_hits: list[str] = []
        if vix is not None and vix > thresholds.get("vix_extreme", 45.0):
            extreme_hits.append("vix_extreme")
        if momentum is not None and momentum < thresholds.get("benchmark_momentum_extreme", -0.20):
            extreme_hits.append("momentum_extreme")
        if eps_revision is not None and eps_revision < thresholds.get("eps_revision_extreme", -0.35):
            extreme_hits.append("eps_revision_extreme")
        if extreme_hits:
            signals["extreme_hits"] = extreme_hits
            return cls.WINTER, signals
        signals["extreme_hits"] = []

        winter_hits: list[str] = []
        if revenue_growth is not None and revenue_growth < thresholds["revenue_growth_winter"]:
            winter_hits.append("revenue_growth")
        if eps_revision is not None and eps_revision < thresholds["eps_revision_winter"]:
            winter_hits.append("eps_revision")
        if capex_growth is not None and capex_growth < thresholds["capex_growth_winter"]:
            winter_hits.append("capex_growth")
        if inventory_days is not None and inventory_days > thresholds["inventory_days_winter"]:
            winter_hits.append("inventory_days")
        if tnx is not None and tnx > thresholds["tnx_yield_winter"]:
            winter_hits.append("tnx_yield")
        if vix is not None and vix > thresholds["vix_winter"]:
            winter_hits.append("vix")
        if momentum is not None and momentum < thresholds["benchmark_momentum_winter"]:
            winter_hits.append("benchmark_momentum")

        slowdown_hits: list[str] = []
        if revenue_growth is not None and revenue_growth < thresholds["revenue_growth_slowdown"]:
            slowdown_hits.append("revenue_growth")
        if eps_revision is not None and eps_revision < thresholds["eps_revision_slowdown"]:
            slowdown_hits.append("eps_revision")
        if tnx is not None and tnx > thresholds["tnx_yield_slowdown"]:
            slowdown_hits.append("tnx_yield")
        if vix is not None and vix > thresholds["vix_slowdown"]:
            slowdown_hits.append("vix")
        if momentum is not None and momentum < thresholds["benchmark_momentum_slowdown"]:
            slowdown_hits.append("benchmark_momentum")
        signals["winter_hits"] = winter_hits
        signals["slowdown_hits"] = slowdown_hits

        if len(winter_hits) >= 2:
            return cls.WINTER, signals
        if len(slowdown_hits) >= 2:
            return cls.SLOWDOWN, signals
        if momentum is not None and momentum > 0.10 and (revenue_growth is None or revenue_growth > 0.20):
            return cls.BOOM, signals
        return cls.NORMAL, signals


class MarketDataProvider:
    # 对外暴露的数据来源标识：只表示「来自上游接口」，不暴露具体供应商
    name = "upstream"
    _fair_value_executor = ThreadPoolExecutor(
        max_workers=int(VALUATION_CONFIG["fair_value_workers"]),
        thread_name_prefix="fair-value",
    )

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

    def ensure_fair_value(self, symbol: str) -> dict[str, Any]:
        """只启动估值任务，不重复抓取现货、期权链或扩展时段行情。

        轻量 quote 轮询可能只读到 SQLite 里的旧行情快照；这时必须显式把估值
        任务重新接上，否则前端只能等待一个永远不会变化的占位结果。
        ``_fair_value`` 自身负责缓存和 inflight 去重，因此重复轮询不会叠加任务。
        """
        normalized = self.normalize_symbol(symbol)
        ticker = self._ticker(normalized)
        return self._fair_value(normalized, ticker)

    def _fair_value(self, symbol: str, ticker: Any) -> dict[str, Any]:
        """异步计算公开财务数据驱动的保守估值，并立即返回已有缓存。

        财务数据可能缺失或被上游限流，任何异常都
        只会让页面显示 ``--``，不能拖慢现货和期权快照。
        """
        now = time.monotonic()
        shared_key = f"fair-value:{VALUATION_CONFIG['cache_version']}:{symbol}"
        with self._fair_value_lock:
            cached = self._fair_value_cache.get(shared_key)
            # 成功值一天内足够稳定；失败值只短暂缓存，避免一次 Yahoo 暂时异常
            # 让新标的的估值卡片要等几分钟、甚至下一次完整刷新才恢复。
            if cached and cached[1].get("value") is not None:
                cache_ttl = 6 * 60 * 60
            elif cached and cached[1].get("status") == "retry":
                # 限流结果只短暂缓存；前端轮询时可在几秒后重新抢占任务。
                cache_ttl = 2
            else:
                cache_ttl = 15
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
                    self._fair_value_cache[shared_key] = (now, shared)
                return shared
        with self._fair_value_lock:
            if symbol not in self._fair_value_inflight:
                self._fair_value_inflight.add(symbol)
                should_start = True
            else:
                should_start = False
            # TTL 到期后的短暂更新期继续显示旧值，避免卡片闪空。
            current = cached[1] if cached else {"value": None, "source": None, "status": "pending"}
        if should_start:
            try:
                type(self)._fair_value_executor.submit(self._fetch_fair_value, symbol, ticker)
            except RuntimeError:
                with self._fair_value_lock:
                    self._fair_value_inflight.discard(symbol)
        return current

    def _fetch_fair_value(self, symbol: str, ticker: Any) -> None:
        """后台读取上游目标均值；不论成功失败都解除该标的的去重标记。"""
        now = time.monotonic()
        shared_key = f"fair-value:{VALUATION_CONFIG['cache_version']}:{symbol}"
        result: dict[str, Any] = {"value": None, "source": None, "status": "pending"}
        with self._fair_value_lock:
            cached = self._fair_value_cache.get(shared_key)
        try:
            regime, regime_signals = self._detect_regime(ticker)
            result = self._conservative_dcf(ticker, symbol, regime=regime, regime_signals=regime_signals)
            result["regime"] = regime
            result["regime_signals"] = regime_signals
            if result.get("value") is None:
                result.setdefault("status", "unavailable")
                result.setdefault("warning", "公开财务数据不足，未生成有效估值候选")
            else:
                result.setdefault("status", "ready")
        except Exception as exc:  # noqa: BLE001 - 可选字段失败不应影响行情
            logger.debug("计算 %s 保守估值失败，继续使用现货行情：%s", symbol, exc)
            result = {
                "value": None,
                "source": None,
                "status": "retry" if _is_transient_upstream_error(exc) else "failed",
                "warning": "Yahoo 数据源暂时限流，将自动重试" if _is_transient_upstream_error(exc) else "估值计算暂时失败，将在下一次刷新重试",
            }
        finally:
            with self._fair_value_lock:
                if shared_key not in self._fair_value_cache and len(self._fair_value_cache) >= 128:
                    self._fair_value_cache.pop(next(iter(self._fair_value_cache)))
                # 上游临时限流时保留最近一次成功值；时间戳仍沿用旧值，下一次请求会
                # 重新尝试，而页面不会因为一次 429 直接闪成空白。
                if result.get("value") is None and cached and cached[1].get("value") is not None:
                    self._fair_value_cache[shared_key] = cached
                else:
                    self._fair_value_cache[shared_key] = (now, result)
                if result.get("value") is not None and self.analysis_cache is not None:
                    try:
                        self.analysis_cache.put_analysis_cache(shared_key, result, max_entries=256)
                    except Exception:
                        # 估值是可选展示字段，缓存写入失败不能影响现货响应。
                        logger.debug("写入 %s 估值共享缓存失败", symbol, exc_info=True)
                self._fair_value_inflight.discard(symbol)

    def _detect_regime(self, ticker: Any) -> tuple[str, dict[str, Any]]:
        """读取个股/宏观公开字段和基准动量，返回当前估值状态。"""
        info_failed = False
        try:
            with self.upstream_gate.slot():
                raw_info = getattr(ticker, "info", {})
        except Exception:
            raw_info = {}
            info_failed = True
        info = raw_info if isinstance(raw_info, dict) else {}
        benchmark_bars = None
        cache_key = "^GSPC"
        now_ts = time.monotonic()
        with self._benchmark_cache_lock:
            cached = self._benchmark_cache.get(cache_key)
            if cached and now_ts - cached[0] < 900:
                benchmark_bars = cached[1]
        if benchmark_bars is None and not info_failed:
            try:
                benchmark_bars = self.benchmark_history(cache_key, period="6mo")
                with self._benchmark_cache_lock:
                    self._benchmark_cache[cache_key] = (now_ts, benchmark_bars)
            except Exception:
                logger.debug("读取基准指数动量失败", exc_info=True)
                # 基准动量是可选信号；缓存空结果，避免 Yahoo 限流时每次个股
                # 估值重试都额外请求一次 ^GSPC。
                with self._benchmark_cache_lock:
                    self._benchmark_cache[cache_key] = (now_ts, [])
        return MarketRegime.detect(info, benchmark_bars)

    def _conservative_dcf(
        self,
        ticker: Any,
        symbol: str | None = None,
        *,
        regime: str = MarketRegime.NORMAL,
        regime_signals: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """读取公开财务数据并选择适合公司阶段的多模态保守估值。"""
        upstream_errors: list[str] = []
        timeseries = self._fetch_yahoo_timeseries(symbol) if symbol else {}
        if timeseries.get("__status__") == "retry":
            upstream_errors.append("fundamentals_timeseries_rate_limited")
        try:
            with self.upstream_gate.slot():
                cashflow = getattr(ticker, "cashflow", None)
                balance = getattr(ticker, "balance_sheet", None)
                raw_info = getattr(ticker, "info", {})
        except Exception as exc:
            cashflow = balance = None
            raw_info = {}
            _record_upstream_error(upstream_errors, "yahoo_info", exc)
        info = raw_info if isinstance(raw_info, dict) else {}

        # fundamentals-timeseries 对小盘股更容易被限流；Yahoo 的 info 仍可能
        # 提供 TTM 营收，先用它恢复 P/S、资产周转等需要营收锚点的分支。
        info_revenue = self._first_info_number(
            info,
            ("totalRevenue", "trailingAnnualRevenue", "revenue", "totalRevenueTTM"),
        )

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
        shares_source = "balance_sheet" if shares_values else None
        equity_values = self._statement_values(balance, ("Stockholders Equity", "Total Stockholder Equity", "Common Stock Equity"))
        if not equity_values:
            equity_values = self._timeseries_values(timeseries, "annualStockholdersEquity")
        interest_values = self._statement_values(
            cashflow,
            ("Interest Expense Non Operating", "Interest Expense", "Net Non Operating Interest Income Expense"),
        )
        debt_values = debt_values or self._timeseries_values(timeseries, "annualTotalDebt")
        cash_values = cash_values or self._timeseries_values(timeseries, "annualCashCashEquivalentsAndShortTermInvestments")
        if not shares_values:
            shares_values = self._timeseries_values(timeseries, "annualDilutedAverageShares")
            if shares_values:
                shares_source = "fundamentals_timeseries"
        if not debt_values:
            debt = self._info_number(info, "totalDebt")
            debt_values = [debt] if debt is not None else []
        if not cash_values:
            cash = self._info_number(info, "totalCash")
            cash_values = [cash] if cash is not None else []
        if not shares_values:
            shares = self._info_number(info, "sharesOutstanding")
            shares_values = [shares] if shares is not None else []
            if shares_values:
                shares_source = "info"
        if not shares_values:
            try:
                with self.upstream_gate.slot():
                    shares = _read_fast_info(getattr(ticker, "fast_info", {}), "shares")
                if shares is not None:
                    shares_values = [shares]
                    shares_source = "fast_info"
            except Exception:
                pass
        current_price = self._first_info_number(info, ("regularMarketPrice", "currentPrice", "previousClose"))
        if current_price is None:
            try:
                with self.upstream_gate.slot():
                    current_price = _read_fast_info(getattr(ticker, "fast_info", {}), "last_price")
            except Exception:
                pass
        historical_price_values: list[float] = []
        # 历史价格只用于估值分位展示；时序接口已限流时跳过这次额外请求，
        # 避免 ONDS 首次估值同时触发 fundamentals、历史价格和基准三组限流。
        if timeseries.get("__status__") != "retry":
            try:
                with self.upstream_gate.slot():
                    historical_frame = ticker.history(
                        period="6y", interval="1mo", auto_adjust=True,
                        timeout=UPSTREAM_REQUEST_TIMEOUT_SECONDS,
                    )
                historical_price_values = self._annual_closing_prices(historical_frame)
            except Exception as exc:
                logger.debug("读取 %s 多年估值价格序列失败", symbol, exc_info=True)
                _record_upstream_error(upstream_errors, "historical_prices", exc)

        revenue_values = self._timeseries_values(timeseries, "annualTotalRevenue")
        if not revenue_values and info_revenue is not None and info_revenue > 0:
            revenue_values = [info_revenue]

        return self._conservative_earnings(
            info,
            symbol=symbol,
            eps_values=self._timeseries_values(timeseries, "trailingDilutedEPS"),
            annual_eps_values=self._timeseries_values(timeseries, "annualDilutedEPS"),
            fcf_values=[value for value in fcf if math.isfinite(value)],
            operating_cashflow_values=self._timeseries_values(timeseries, "annualOperatingCashFlow"),
            shares_values=shares_values,
            shares_source=shares_source,
            debt_values=debt_values,
            cash_values=cash_values,
            timeseries=timeseries,
            forward_eps=self._first_info_number(info, ("forwardEps", "epsForward", "forwardEPS")),
            current_price=current_price,
            growth_hint=self._first_info_number(info, ("earningsGrowth", "earningsQuarterlyGrowth", "revenueGrowth")),
            beta=self._first_info_number(info, ("beta", "beta3Year")),
            sector=str(info.get("sector") or ""),
            industry=str(info.get("industry") or ""),
            industry_key=str(info.get("industryKey") or info.get("industry_key") or ""),
            quarterly_revenue_values=self._timeseries_values(timeseries, "quarterlyTotalRevenue"),
            quarterly_eps_values=self._timeseries_values(timeseries, "quarterlyDilutedEPS"),
            revenue_values=revenue_values,
            operating_income_values=self._timeseries_values(timeseries, "annualOperatingIncome"),
            net_income_values=self._timeseries_values(timeseries, "annualNetIncome"),
            da_values=self._timeseries_values(timeseries, "annualDepreciationAndAmortization"),
            capex_values=self._timeseries_values(timeseries, "annualCapitalExpenditure"),
            working_capital_values=self._timeseries_values(timeseries, "annualChangeInWorkingCapital"),
            buyback_values=self._timeseries_values(timeseries, "annualRepurchaseOfCapitalStock"),
            historical_price_values=historical_price_values,
            equity_values=equity_values,
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
            interest_expense=(interest_values[0] if interest_values else self._first_info_number(info, ("interestExpense", "interestExpenseToRevenue", "interestExpenseNonOperating"))),
            revenue_growth=self._first_info_number(info, ("revenueGrowth",)),
            roe=self._first_info_number(info, ("returnOnEquity", "roe", "returnOnAverageEquity")),
            ffo_per_share=self._first_info_number(info, ("ffoPerShare", "fundsFromOperationsPerShare", "normalizedFfoPerShare")),
            affo_per_share=self._first_info_number(info, ("affoPerShare", "adjustedFundsFromOperationsPerShare")),
            nav_per_share=self._first_info_number(info, ("navPerShare", "netAssetValuePerShare")),
            rab_per_share=self._first_info_number(info, ("rabPerShare", "regulatedAssetBasePerShare")),
            embedded_value_per_share=self._first_info_number(info, ("embeddedValuePerShare", "evPerShare", "embeddedValue")),
            new_business_value_per_share=self._first_info_number(info, ("newBusinessValuePerShare", "nbvPerShare", "newBusinessValue")),
            pipeline_rnpv=self._first_info_number(info, ("pipelineRnPV", "pipelineRNPV", "riskAdjustedNPV", "rNPV")),
            approved_drug_value=self._first_info_number(info, ("approvedDrugValue", "marketedProductValue", "approvedProductsValue")),
            platform_users=self._first_info_number(info, ("monthlyActiveUsers", "activeUsers", "users", "userCount")),
            arpu=self._first_info_number(info, ("arpu", "averageRevenuePerUser")),
            churn_rate=self._first_info_number(info, ("churnRate", "userChurnRate")),
            ai_revenue_share=self._first_info_number(info, ("aiRevenueShare", "aiRevenuePercentage", "artificialIntelligenceRevenueShare")),
            analyst_count=self._first_info_number(info, ("numberOfAnalystOpinions", "analystCount", "analystCoverage")),
            regime=regime,
            regime_signals=regime_signals,
            upstream_errors=upstream_errors,
        )

    @staticmethod
    def _info_number(info: dict[str, Any], key: str) -> float | None:
        value = safe_value(info.get(key))
        try:
            number = float(value)
        except (TypeError, ValueError):
            return None
        return number if math.isfinite(number) else None

    @staticmethod
    def _owner_earnings_maintenance_ratio(
        sector: str,
        industry: str,
        industry_key: str,
        asset_heavy_transition: bool,
    ) -> tuple[float, str]:
        """按行业估计资本开支中可视为维护性支出的比例。"""
        text = f"{sector} {industry} {industry_key}".lower().replace("_", " ")
        cfg = VALUATION_CONFIG.get("owner_earnings", {})
        if asset_heavy_transition:
            return float(cfg.get("asset_heavy_transition_maintenance_capex_ratio", 0.30)), "重资产转型"
        if any(word in text for word in ("utility", "utilities", "regulated electric", "water utility", "gas utility")):
            return float(cfg.get("mature_manufacturing_maintenance_capex_ratio", 0.85)), "公用事业"
        if any(word in text for word in ("manufacturing", "industrials", "machinery", "factory")):
            return float(cfg.get("mature_manufacturing_maintenance_capex_ratio", 0.85)), "成熟制造"
        if any(word in text for word in ("technology", "software", "consumer", "internet", "semiconductor")):
            return float(cfg.get("technology_maintenance_capex_ratio", 0.50)), "科技/消费"
        return float(cfg.get("default_maintenance_capex_ratio", 0.70)), "通用"

    @staticmethod
    def _valuation_percentile(values: list[float], current: float | None) -> float | None:
        """返回当前倍数在可用历史代理序列中的百分位。"""
        valid = sorted(value for value in values if value > 0 and math.isfinite(value))
        if not valid or current is None or not math.isfinite(current) or current <= 0:
            return None
        return round(sum(value <= current for value in valid) / len(valid) * 100.0, 1)

    @classmethod
    def _historical_valuation_percentiles(
        cls,
        *,
        current_price: float | None,
        historical_price_values: list[float] | None,
        annual_eps_values: list[float] | None,
        ttm_eps: float | None,
        debt: float,
        cash: float,
        shares: float | None,
        debt_values: list[float] | None,
        cash_values: list[float] | None,
        shares_values: list[float] | None,
        equity_values: list[float] | None,
        book_value: float | None,
        operating_income_values: list[float] | None,
        da_values: list[float] | None,
    ) -> dict[str, Any] | None:
        """用年度盈利序列生成透明的“当前价格隐含倍数”分位。"""
        if not current_price or current_price <= 0:
            return None
        result: dict[str, Any] = {}
        prices = list(historical_price_values or [])[:5]
        annual_eps = list(annual_eps_values or [])[:5]
        pe_history = [price / eps for price, eps in zip(prices, annual_eps) if price > 0 and eps > 0 and math.isfinite(price) and math.isfinite(eps)]
        current_pe = current_price / ttm_eps if ttm_eps and ttm_eps > 0 else None
        if current_pe and pe_history:
            result.update(
                pe_current=round(current_pe, 2),
                pe_percentile=cls._valuation_percentile(pe_history, current_pe),
                pe_observations=len(pe_history),
            )
        historical_shares = list(shares_values or [])[:5]
        historical_equity = list(equity_values or [])[:5]
        pb_history = [
            price * share_count / equity
            for price, share_count, equity in zip(prices, historical_shares, historical_equity)
            if price > 0 and share_count > 0 and equity > 0
            and all(math.isfinite(value) for value in (price, share_count, equity))
        ]
        current_pb = current_price / book_value if book_value and book_value > 0 else None
        if current_pb and pb_history:
            result.update(
                pb_current=round(current_pb, 2),
                pb_percentile=cls._valuation_percentile(pb_history, current_pb),
                pb_observations=len(pb_history),
            )
        if shares and shares > 0:
            enterprise_value = current_price * shares + debt - cash
            ebitda_history = []
            historical_debt = list(debt_values or [])
            historical_cash = list(cash_values or [])
            for index, (price, operating) in enumerate(zip(prices, operating_income_values or [])):
                share_count = historical_shares[index] if index < len(historical_shares) else shares
                debt_value = historical_debt[index] if index < len(historical_debt) else debt
                cash_value = historical_cash[index] if index < len(historical_cash) else cash
                da = (da_values or [])[index] if index < len(da_values or []) else 0.0
                ebitda = operating + abs(da)
                historical_ev = price * share_count + debt_value - cash_value
                if ebitda > 0 and historical_ev > 0 and math.isfinite(ebitda):
                    ebitda_history.append(historical_ev / ebitda)
            current_ebitda = ((operating_income_values or [])[0] + abs((da_values or [])[0])) if operating_income_values else None
            current_ev_ebitda = enterprise_value / current_ebitda if current_ebitda and current_ebitda > 0 and enterprise_value > 0 else None
            if current_ev_ebitda and ebitda_history:
                result.update(
                    ev_ebitda_current=round(current_ev_ebitda, 2),
                    ev_ebitda_percentile=cls._valuation_percentile(ebitda_history, current_ev_ebitda),
                    ev_ebitda_observations=len(ebitda_history),
                )
        return result or None

    @staticmethod
    def _annual_closing_prices(frame: Any) -> list[float]:
        """从月线中提取每个自然年的最后收盘价，按年份从新到旧返回。"""
        if frame is None or getattr(frame, "empty", True) or "Close" not in getattr(frame, "columns", ()):
            return []
        annual: dict[int, float] = {}
        try:
            for timestamp, raw in frame["Close"].items():
                price = float(safe_value(raw))
                year = int(timestamp.year)
                if price > 0 and math.isfinite(price):
                    annual[year] = price
        except (AttributeError, TypeError, ValueError):
            return []
        return [annual[year] for year in sorted(annual, reverse=True)]

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
        """从已按外层 timestamp 排序的时序结果中提取数值。"""
        values = payload.get(metric) if isinstance(payload, dict) else None
        if not isinstance(values, list):
            return []
        result: list[float] = []
        for item in values:
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
        shares_source: str | None = None,
        debt_values: list[float] | None = None,
        cash_values: list[float] | None = None,
        timeseries: dict[str, list[dict[str, Any]]] | None = None,
        forward_eps: float | None = None,
        current_price: float | None = None,
        growth_hint: float | None = None,
        beta: float | None = None,
        sector: str = "",
        industry: str = "",
        industry_key: str = "",
        revenue_values: list[float] | None = None,
        quarterly_revenue_values: list[float] | None = None,
        quarterly_eps_values: list[float] | None = None,
        operating_income_values: list[float] | None = None,
        net_income_values: list[float] | None = None,
        da_values: list[float] | None = None,
        capex_values: list[float] | None = None,
        working_capital_values: list[float] | None = None,
        buyback_values: list[float] | None = None,
        historical_price_values: list[float] | None = None,
        equity_values: list[float] | None = None,
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
        interest_expense: float | None = None,
        revenue_growth: float | None = None,
        roe: float | None = None,
        ffo_per_share: float | None = None,
        affo_per_share: float | None = None,
        nav_per_share: float | None = None,
        rab_per_share: float | None = None,
        embedded_value_per_share: float | None = None,
        new_business_value_per_share: float | None = None,
        pipeline_rnpv: float | None = None,
        approved_drug_value: float | None = None,
        platform_users: float | None = None,
        arpu: float | None = None,
        churn_rate: float | None = None,
        ai_revenue_share: float | None = None,
        analyst_count: float | None = None,
        regime: str = MarketRegime.NORMAL,
        regime_signals: dict[str, Any] | None = None,
        upstream_errors: list[str] | None = None,
    ) -> dict[str, Any]:
        """按公司生命周期选择估值模型，并同时生成防守与进攻两个区间。"""
        ttm_eps = eps_values[0] if eps_values and eps_values[0] > 0 else cls._info_number(info, "trailingEps")
        annual = [value for value in (annual_eps_values or []) if value > 0 and math.isfinite(value)]
        historical_eps = ([ttm_eps] if ttm_eps else []) + annual[:5]
        normalized_eps = sorted(historical_eps)[len(historical_eps) // 2] if historical_eps else None
        normalized_eps_source = "历史 EPS 中位数" if normalized_eps is not None else None
        current_shares_for_normalization = next(
            (value for value in (shares_values or []) if value and value > 0 and math.isfinite(value)),
            None,
        )
        normalized_net_income = [
            value / current_shares_for_normalization
            for value in (net_income_values or [])
            if current_shares_for_normalization and value and value > 0 and math.isfinite(value)
        ]
        if len(normalized_net_income) >= 2:
            normalized_net_income_eps = sorted(normalized_net_income)[len(normalized_net_income) // 2]
            # 报表接口偶尔混用美元、千美元和百万美元；在切换到净利润/当前股本
            # 前先与历史 EPS 中位数做量纲检查，避免一个单位错误把保守估值放大数十倍。
            eps_ratio_ok = True
            if normalized_eps and normalized_eps > 0:
                eps_ratio = normalized_net_income_eps / normalized_eps
                eps_cfg = VALUATION_CONFIG.get("normalized_eps", {})
                eps_ratio_ok = (
                    float(eps_cfg.get("min_ratio", 0.25))
                    <= eps_ratio
                    <= float(eps_cfg.get("max_ratio", 4.0))
                )
            if eps_ratio_ok:
                normalized_eps = normalized_net_income_eps
                normalized_eps_source = "净利润 / 当前股本"
            else:
                normalized_eps_source = "历史 EPS 中位数（净利润股本量纲异常回退）"
        forward_eps_observed = forward_eps is not None and math.isfinite(forward_eps) and forward_eps > 0
        if forward_eps is not None and forward_eps <= 0:
            forward_eps = None
        # 没有分析师远期 EPS 时，用最近盈利和可观察增长率做模型估计，并在结果中标注来源。
        growth = growth_hint
        if growth is None and len(annual) >= 2 and annual[-1] > 0:
            growth = (annual[0] / annual[-1]) ** (1 / (len(annual) - 1)) - 1
        if growth is None and revenue_values and len(revenue_values) >= 2 and revenue_values[-1] > 0:
            growth = (revenue_values[0] / revenue_values[-1]) ** (1 / (len(revenue_values) - 1)) - 1
        growth = min(0.50, max(-0.15, growth or 0.0))
        # Yahoo 的 marketCap 可能滞后于现价或使用了不同股本口径；先校正市值，
        # 再参与行业门槛判断，避免 ONDS 这类标的在错误市值下走错分支。
        input_shares = next(
            (value for value in (shares_values or []) if value and value > 0 and math.isfinite(value)),
            None,
        )
        if shares_source is None and input_shares is not None:
            # 直接调用估值函数时无法知道上游来源；将其标成 provided，按保守口径
            # 处理“推导市值更低”的情况，避免未知股本未经确认就压低市值。
            shares_source = "provided"
        implied_market_cap = input_shares * current_price if input_shares and current_price and current_price > 0 else None
        market_cap_quality = "unknown"
        market_cap_warning = None
        reconciliation_cfg = VALUATION_CONFIG.get("market_cap_reconciliation", {})
        mismatch_threshold = float(
            reconciliation_cfg.get(
                "mismatch_threshold",
                VALUATION_CONFIG.get("market_cap_mismatch_threshold", 0.20),
            )
        )
        trusted_shares_sources = set(reconciliation_cfg.get("trusted_shares_sources", ("balance_sheet", "info", "fast_info")))
        shares_source_trusted = shares_source in trusted_shares_sources
        if market_cap and market_cap > 0 and implied_market_cap and implied_market_cap > 0:
            mismatch_ratio = abs(market_cap - implied_market_cap) / market_cap
            if mismatch_ratio > mismatch_threshold:
                scale_ratio = market_cap / implied_market_cap
                # 测试/上游有时把 shares 以“十亿股”返回，而 marketCap 仍是原始美元；
                # 这种百万倍偏差是单位问题，不能把市值改成几百万美元。
                if scale_ratio > 100.0 or scale_ratio < 0.01:
                    market_cap_quality = "unit_mismatch"
                    market_cap_warning = "market_cap 与股本×现价存在数量级差异，已保留市值并降低数据质量评分"
                elif not shares_source_trusted:
                    # 方向必须对称：年度时序股数、provided 或未知来源都可能滞后/稀释；
                    # 无法证明当前口径时，无论推导市值更高还是更低，都保留 Yahoo market_cap，
                    # 避免把错误股本带入大市值门槛或把真实市值压低。
                    market_cap_quality = "market_cap_preferred"
                    market_cap_warning = (
                        f"股本来源 {shares_source or 'unknown'} 未通过当前口径核验，"
                        "已保留 Yahoo market_cap"
                    )
                else:
                    market_cap = implied_market_cap
                    market_cap_quality = "reconciled"
                    market_cap_warning = (
                        f"market_cap 与股本×现价偏差超过 {mismatch_threshold:.0%}，"
                        f"已使用{shares_source or '可用股本'}×现价校正市值"
                    )
            else:
                market_cap_quality = "consistent"
        elif market_cap is None and implied_market_cap:
            market_cap = implied_market_cap
            market_cap_quality = "derived"
        # Yahoo 的 industry 字段可能把国防/激光公司归为 Semiconductors 或
        # Electronic Components；业务摘要通常包含 aerospace、defense、laser、
        # directed energy 等更准确的业务线索，因此将其一并用于行业识别。
        business_summary = str(info.get("longBusinessSummary") or "").lower()
        industry_key_normalized = str(industry_key or "").strip().lower().replace("_", "-")
        industry_key_text = industry_key_normalized.replace("-", " ")
        industry_key_category = str(
            VALUATION_CONFIG.get("industry_key_map", {}).get(industry_key_normalized) or ""
        ).lower()
        # 映射类别是结构化标签，不拼进自由文本，避免新类别值凭空制造关键词命中。
        classification_text = f"{sector} {industry} {industry_key_text} {business_summary}".lower().replace("_", " ")
        mobility_technology_keywords = (
            "autonomous",
            "self-driving",
            "self driving",
            "full self-driving",
            "full self driving",
            "robotaxi",
            "robotics",
            "optimus",
            "humanoid",
            "robot",
            "autopilot",
            "fsd",
        )
        mobility_strong_signals = (
            "self-driving",
            "self driving",
            "full self-driving",
            "full self driving",
            "robotaxi",
            "autopilot",
            "fsd",
            "humanoid",
            "optimus",
        )
        electric_mobility_keywords = ("electric vehicle", "electric vehicles", "ev maker", "battery electric")
        sector_text = str(sector or "").lower().replace("_", " ")
        industry_text = f"{industry} {industry_key_text}".lower().replace("_", " ")
        category_is_memory_chip = industry_key_category == "memory_chip"
        category_is_computer_hardware = industry_key_category == "computer_hardware"
        category_is_defense = industry_key_category == "defense"
        category_is_bank = industry_key_category == "banking"
        category_is_insurance = industry_key_category == "insurance"
        category_is_reit = industry_key_category == "reit"
        category_is_biotech = industry_key_category == "biotech"
        category_is_utility = industry_key_category == "utility"
        category_is_internet_platform = industry_key_category == "internet_platform"
        mobility_industry_context = any(
            word in f"{sector_text} {industry_text}"
            for word in ("auto", "automotive", "electric vehicle", "mobility", "transportation", "ride-hailing")
        )
        mobility_signal_count = sum(word in classification_text for word in mobility_strong_signals)
        mobility_cfg = VALUATION_CONFIG.get("mobility_technology", {})
        try:
            mobility_signal_min_count = int(mobility_cfg.get("strong_signal_min_count", 2))
            mobility_signal_market_cap = float(mobility_cfg.get("strong_signal_market_cap", 1e11))
        except (TypeError, ValueError):
            mobility_signal_min_count, mobility_signal_market_cap = 2, 1e11
        # “autonomous database”“robotic process automation”等企业软件术语不能单独
        # 触发移动科技模型。汽车/移动行业需要行业上下文；没有上下文时，至少要有
        # 两个明确指向自动驾驶、Robotaxi 或人形机器人的强信号。
        mobility_technology_tag = bool(
            (
                mobility_industry_context
                and any(word in classification_text for word in mobility_technology_keywords)
            )
            or (
                mobility_signal_count >= mobility_signal_min_count
                and market_cap is not None
                and market_cap_quality not in {"unit_mismatch", "market_cap_preferred"}
                and market_cap >= mobility_signal_market_cap
            )
        )
        electric_mobility_tag = any(word in classification_text for word in electric_mobility_keywords)
        # 第一级：行业过滤。Yahoo 不一定返回标准 GICS 名称，因此同时接受
        # Financial Services/Real Estate 等常见别名；金融和地产禁止进入 DCF。
        financial_or_real_estate = (
            category_is_bank
            or category_is_insurance
            or category_is_reit
            or any(
                word in f"{sector_text} {industry_text}"
                for word in ("financial", "bank", "insurance", "capital markets", "credit", "real estate", "reit", "property")
            )
        )
        cruise_operator = bool(
            any(word in classification_text for word in ("cruise", "cruise lines", "hotels resorts cruise", "travel services", "resorts"))
        )
        # LASR 是国防订单驱动的激光技术转型公司。它可能仍有 GAAP 微利、
        # 研发投入或阶段性负现金流，但已有高增长订单时不能落入 INTC/ORCL
        # 的失败型重资产压力测试；该分类必须在通用重资产判断之前抢占。
        defense_industry_tag = category_is_defense or any(
            word in classification_text for word in ("defense", "aerospace", "laser", "military", "directed energy")
        )
        defense_transition = bool(
            (defense_revenue_growth is not None and defense_revenue_growth >= 0.30)
            or (
                defense_industry_tag
                and (
                    growth >= 0.10
                    or (revenue_growth is not None and revenue_growth >= 0.10)
                    or (defense_revenue_growth is not None and defense_revenue_growth >= 0.30)
                    or bool(fcf_values and fcf_values[0] < 0)
                )
            )
        )
        if defense_transition:
            _valuation_debug(
                "国防订单驱动转型分类：symbol=%s defense_industry_tag=%s classification_text=%s",
                symbol or "未知",
                defense_industry_tag,
                classification_text[:200],
            )
        # AMD/NVDA 等芯片公司的 GAAP EPS 常被并购摊销和研发投入压低，
        # 先识别这类公司，再决定远期 EPS 的上限，避免把增长预期截断成普通周期股。
        memory_chip = category_is_memory_chip or any(word in classification_text for word in ("memory", "dram", "nand", "flash"))
        # 软件公司的业务摘要常出现 cloud/data storage/storage infrastructure；
        # 这些是服务对象或功能，不代表公司制造存储硬件。只有明确的物理存储
        # 产品信号，或“Computer Hardware”行业同时出现 storage/disk，才命中。
        storage_hardware_strong_signals = (
            "hard disk",
            "disk drive",
            "hdd",
            "nand",
            "flash memory",
            "solid state drive",
            "ssd",
            "memory chip",
            "storage chip",
            "storage device",
            "storage hardware",
            "storage products",
            "storage systems",
            "enterprise storage",
            "computer storage",
        )
        storage_hardware = bool(
            any(word in classification_text for word in storage_hardware_strong_signals)
            or (
                ("computer hardware" in classification_text or category_is_computer_hardware)
                and any(word in classification_text for word in ("storage", "disk"))
            )
        )
        # 存储硬件既有周期属性，又可能处在 AI 需求带来的景气上行阶段；
        # 普通软件公司仅提到 cloud/data storage 时不会进入该模型。
        ai_storage_cycle = bool(
            not financial_or_real_estate
            and (
                memory_chip
                or (
                    storage_hardware
                    and (
                        growth >= 0.20
                        or (revenue_growth is not None and revenue_growth >= 0.20)
                        or (
                            forward_eps is not None
                            and normalized_eps is not None
                            and forward_eps >= normalized_eps * 1.25
                        )
                    )
                )
            )
        )
        asset_heavy_transition = bool(
            not defense_transition
            and not ai_storage_cycle
            and (
                not memory_chip
                and not cruise_operator
                and (
                    (operating_cashflow_values and capex_values and abs(capex_values[0]) >= abs(operating_cashflow_values[0]) * 0.70)
                    or (fcf_values and fcf_values[0] < 0)
                )
            )
        )
        high_growth_chip_signal = bool(
            not asset_heavy_transition
            and not ai_storage_cycle
            and not cruise_operator
            and not defense_industry_tag
            and (
                not memory_chip
                and any(word in classification_text for word in ("semiconductor", "ai chip", "graphics processor"))
                and growth >= 0.25
                and (price_to_sales is None or price_to_sales > 10.0)
            )
        )
        # 没有分析师远期 EPS 时不启用芯片 PEG/高增长估值，后续回退到
        # DCF、EV/EBITDA 或历史盈利模型，并把降级原因写入 warnings。
        high_growth_chip = high_growth_chip_signal and forward_eps_observed
        # 行业专用模型优先于普通 DCF/PE。字段命名同时兼容 Yahoo 行业名和
        # GICS/供应商自定义标签，缺少专用指标时再回退到可观察的公开字段。
        banking_model = category_is_bank or bool(any(word in classification_text for word in ("bank", "banking", "regional bank", "diversified banks")))
        insurance_model = category_is_insurance or bool(any(word in classification_text for word in ("insurance", "life insurance", "property & casualty", "reinsurance", "insurer")))
        reit_model = category_is_reit or bool(any(word in classification_text for word in ("reit", "real estate investment trust")))
        biotech_model = category_is_biotech or bool(any(word in classification_text for word in ("biotechnology", "biotech", "drug manufacturer", "pharmaceutical", "drug manufacturers")))
        # “directed energy”是国防激光业务的术语，不能因为业务摘要出现 energy
        # 就误判为能源/矿业公司；国防标签命中时保留 defense_transition 优先级。
        energy_mining_model = bool(
            not defense_industry_tag
            and not mobility_technology_tag
            and any(word in classification_text for word in ("energy", "oil", "gas", "coal", "uranium", "mining", "gold", "silver", "copper"))
        )
        # 业务摘要中的 “utilities” 可能只是客户群体（例如“为公用事业公司提供
        # 储能产品”），不能据此把电动车/自动驾驶公司本身归类为公用事业。
        utility_model = bool(
            category_is_utility
            or (
                not mobility_technology_tag
                and any(word in classification_text for word in ("utilities", "utility", "regulated electric", "electric utilities", "water utilities", "gas utilities"))
            )
        )
        internet_platform_model = bool(
            category_is_internet_platform
            or any(word in classification_text for word in ("internet content", "internet retail", "interactive media", "social media", "e commerce", "e-commerce", "online platform", "internet services"))
            or ("communication services" in sector_text and any(word in classification_text for word in ("internet", "media", "interactive", "social")))
        )
        internet_ai_keywords = (
            "artificial intelligence",
            "generative ai",
            "large language model",
            "machine learning",
            "ai infrastructure",
            "ai recommendation",
            "recommendation engine",
            "ad ranking",
            "personalized ads",
            "ai-powered",
            "ai powered",
            "ai-driven",
            "ai driven",
            "ai models",
            "llama",
        )
        internet_ai_platform = bool(
            internet_platform_model
            and any(word in classification_text for word in internet_ai_keywords)
            and market_cap is not None
            and market_cap >= float(VALUATION_CONFIG.get("internet_platform", {}).get("ai_premium_market_cap", 1e12))
        )
        if internet_platform_model:
            _valuation_debug(
                "互联网平台估值输入：symbol=%s ai_platform=%s market_cap=%s summary=%s",
                symbol or "未知",
                internet_ai_platform,
                market_cap,
                classification_text[:200],
            )
        # 移动科技公司的业务摘要经常同时提到汽车保险、储能客户、公用事业或
        # 能源业务。这些是附属业务/客户，不应把 TSLA、RIVN 等锁进金融、能源、
        # 公用事业或互联网平台专用估值模型；移动科技识别优先于关键词分类。
        if mobility_technology_tag:
            banking_model = False
            insurance_model = False
            reit_model = False
            biotech_model = False
            energy_mining_model = False
            utility_model = False
            internet_platform_model = False
            # TSLA 等移动科技公司的摘要也会提到 storage/battery/energy，
            # 这些是附属业务或产品线，不应把公司误判为 AI 存储周期股。
            ai_storage_cycle = False
            storage_hardware = False
        specialized_industry = any((banking_model, insurance_model, reit_model, biotech_model, energy_mining_model, utility_model, internet_platform_model))
        if specialized_industry:
            _valuation_debug(
                "专用行业模型命中：symbol=%s banking=%s insurance=%s reit=%s biotech=%s energy=%s utility=%s internet=%s",
                symbol or "未知",
                banking_model,
                insurance_model,
                reit_model,
                biotech_model,
                energy_mining_model,
                utility_model,
                internet_platform_model,
            )
        if financial_or_real_estate:
            # 行业过滤优先级最高，避免后面的负 FCF/资本开支信号把银行、REIT
            # 误判成转型公司。后续只允许 DDM/P/B 候选。
            defense_transition = False
            asset_heavy_transition = False
            high_growth_chip = False
            cruise_operator = False
        if forward_eps is not None and normalized_eps:
            # 普通公司仍保留 3 倍上限；高增长芯片和 AI 存储周期股放宽至 5 倍，
            # 使分析师对景气周期的远期 EPS 不会被 GAAP 正常化利润过早截断。
            growth_forward_class = high_growth_chip or ai_storage_cycle
            forward_cap = max(normalized_eps * (5.0 if growth_forward_class else 3.0), 12.0 if growth_forward_class else 0.0)
            # 分析师共识硬锚定：剥离/转型公司的 Yahoo EPS 可能出现单位或口径异常，
            # 用目标均价反推一个不超过 35 倍 PE 的远期 EPS 上限，避免把模型放大数倍。
            if growth_forward_class and target_mean_price and target_mean_price > 0:
                forward_cap = min(forward_cap, target_mean_price / 35.0)
            forward_eps = min(forward_eps, forward_cap)
        model_forward_eps = forward_eps or (normalized_eps * (1 + growth) if normalized_eps else None)
        forward_source = "Yahoo远期EPS" if forward_eps is not None else ("模型估计远期EPS" if model_forward_eps else None)
        forward_eps_missing_warning = bool(
            not forward_eps_observed
            and (high_growth_chip_signal or ai_storage_cycle or defense_transition)
        )
        if forward_eps_missing_warning and (high_growth_chip_signal or ai_storage_cycle):
            # 高增长/存储模型不得把历史 EPS × 增长率伪装成分析师远期 EPS。
            # 保留行业分类以便走周期或现金流交叉验证，但禁用远期 PEG 主路径。
            model_forward_eps = None
            forward_source = None
            ai_storage_cycle = False
            high_growth_chip = False

        latest_fcf = fcf_values[0] if fcf_values else None
        shares = shares_values[0] if shares_values and shares_values[0] > 0 else None
        debt = debt_values[0] if debt_values else 0.0
        cash = cash_values[0] if cash_values else 0.0
        latest_revenue = revenue_values[0] if revenue_values else None
        op_income = operating_income_values[0] if operating_income_values else None
        da = da_values[0] if da_values else 0.0
        capex = capex_values[0] if capex_values else 0.0
        working_capital = working_capital_values[0] if working_capital_values else 0.0
        quarterly_momentum = None
        if quarterly_revenue_values and len(quarterly_revenue_values) >= 4:
            latest_quarter = quarterly_revenue_values[0]
            year_ago_quarter = quarterly_revenue_values[3]
            yoy_growth = latest_quarter / year_ago_quarter - 1 if year_ago_quarter > 0 else None
            qoq_growth = latest_quarter / quarterly_revenue_values[1] - 1 if quarterly_revenue_values[1] > 0 else None
            quarterly_momentum = {
                "revenue_yoy": round(yoy_growth, 4) if yoy_growth is not None else None,
                "revenue_qoq": round(qoq_growth, 4) if qoq_growth is not None else None,
                "eps_yoy": (
                    round(quarterly_eps_values[0] / quarterly_eps_values[3] - 1, 4)
                    if quarterly_eps_values and len(quarterly_eps_values) >= 4 and quarterly_eps_values[3] > 0
                    else None
                ),
            }
        historical_valuation_percentiles = cls._historical_valuation_percentiles(
            current_price=current_price,
            historical_price_values=historical_price_values,
            annual_eps_values=annual_eps_values,
            ttm_eps=ttm_eps,
            debt=debt,
            cash=cash,
            shares=shares,
            debt_values=debt_values,
            cash_values=cash_values,
            shares_values=shares_values,
            equity_values=equity_values,
            book_value=book_value,
            operating_income_values=operating_income_values,
            da_values=da_values,
        )
        shareholder_total_return_yield = None
        if current_price and current_price > 0 and shares and shares > 0:
            dividend_per_share = dividend_rate if dividend_rate and dividend_rate > 0 else 0.0
            latest_buyback = buyback_values[0] if buyback_values else None
            buyback_per_share = abs(latest_buyback) / shares if latest_buyback and math.isfinite(latest_buyback) else 0.0
            total_return = (dividend_per_share + buyback_per_share) / current_price
            if math.isfinite(total_return) and 0 <= total_return <= 1.0:
                shareholder_total_return_yield = round(total_return, 4)
        _valuation_debug(
            "估值诊断 symbol=%s | sector=%s industry=%s | rev_series(前5)=%s | shares=%s(%s) | fwd_eps=%s | ttm_eps=%s | growth=%s | market_cap=%s | market_cap_quality=%s",
            symbol or "未知",
            sector,
            industry,
            [round(value / 1e9, 2) for value in (revenue_values or [])[:5]],
            f"{shares / 1e9:.3f}B" if shares is not None else None,
            shares_source,
            forward_eps,
            ttm_eps,
            round(growth, 4) if growth is not None else None,
            f"{market_cap / 1e9:.1f}B" if market_cap is not None else None,
            market_cap_quality,
        )
        ebitda = (op_income + abs(da)) if op_income is not None else None
        interest_amount = abs(interest_expense) if interest_expense is not None else None
        if interest_amount is not None and interest_amount <= 1.0 and latest_revenue and latest_revenue > 0:
            # Yahoo 某些字段返回利息/营收比例；按比例换算成金额后再计算覆盖率。
            interest_amount *= latest_revenue
        interest_coverage = None
        if op_income is not None and interest_amount and interest_amount > 0:
            interest_coverage = op_income / interest_amount
        low_interest_coverage = interest_coverage is not None and interest_coverage < float(VALUATION_CONFIG["interest_coverage_warning"])
        owner_earnings_ratio, owner_earnings_ratio_source = cls._owner_earnings_maintenance_ratio(
            sector,
            industry,
            industry_key,
            asset_heavy_transition,
        )
        owner_earnings = None
        if net_income_values:
            owner_earnings = net_income_values[0] + abs(da) - abs(capex) * owner_earnings_ratio - max(working_capital, 0.0)
        if owner_earnings is None and latest_fcf is not None:
            owner_earnings = latest_fcf
        data_quality_score = 100.0
        if market_cap and market_cap < float(VALUATION_CONFIG.get("small_cap_threshold", 5e9)):
            data_quality_score -= 10.0
        if market_cap_quality in ("reconciled", "unit_mismatch", "market_cap_preferred"):
            data_quality_score -= 15.0
        if not revenue_values or any(value <= 0 or not math.isfinite(value) for value in revenue_values):
            data_quality_score -= 15.0
        if analyst_count is not None and analyst_count < 3:
            data_quality_score -= 20.0
        elif analyst_count is None and target_mean_price is None:
            data_quality_score -= 10.0
        data_quality_score = round(min(100.0, max(0.0, data_quality_score)), 1)

        text = f"{sector} {industry} {industry_key}".lower().replace("_", " ")
        # 只把内存、能源、航运等强周期行业归入周期模型；普通半导体（如 AMD/NVDA）
        # 仍允许使用远期盈利模型，避免把结构性成长误判为存储器周期。
        cyclical = ai_storage_cycle or any(word in text for word in ("semiconductor memory", "memory chip", "memory", "dram", "nand", "energy", "oil", "shipping", "airline"))
        stable = any(word in text for word in ("consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage", "discount stores"))
        digital_retail = bool(
            stable
            and market_cap
            and market_cap >= 1e11
            and any(word in text for word in ("retail", "discount stores", "consumer defensive", "consumer staples"))
        )
        cycle_eps = normalized_eps
        if cyclical and len(historical_eps) >= 3:
            # 去掉一个异常高峰后再取中位数，避免只有少数高景气年度时仍把周期顶点当常态。
            trimmed = sorted(historical_eps)[:-1]
            cycle_eps = trimmed[len(trimmed) // 2] if trimmed else normalized_eps
        ai_revenue_share_value = ai_revenue_share
        if ai_revenue_share_value is not None and ai_revenue_share_value > 1:
            ai_revenue_share_value /= 100.0
        ai_revenue_share_value = min(1.0, max(0.0, ai_revenue_share_value or 0.0))
        if ai_revenue_share_value <= 0 and not defense_industry_tag:
            defaults = VALUATION_CONFIG.get("ai_revenue_share_defaults", {})
            if "ai chip" in classification_text or high_growth_chip_signal:
                ai_revenue_share_value = float(defaults.get("ai_chip", 0.70))
            elif "semiconductor" in classification_text:
                ai_revenue_share_value = float(defaults.get("semiconductor", 0.60))
            elif "storage" in classification_text:
                ai_revenue_share_value = float(defaults.get("storage", 0.30))
            ai_revenue_share_value = min(1.0, max(0.0, ai_revenue_share_value))
        ai_exposed = bool(
            not specialized_industry
            and (
                high_growth_chip_signal
                or ai_storage_cycle
                or storage_hardware
                or any(word in classification_text for word in ("semiconductor", "ai chip", "graphics processor"))
                or mobility_technology_tag
                or ai_revenue_share_value > 0
            )
        )
        ai_winter = regime == MarketRegime.WINTER and ai_exposed
        if ai_winter:
            # 寒冬模式使用 5–7 年盈利中枢，先剔除最近两个高景气峰值，
            # 再取最低若干年均值，避免 AI 高景气利润继续抬高压力测试底线；
            # 若历史盈利不足，则保留可用的正常化 EPS，不伪造长期数据。
            winter_history = [value for value in (annual_eps_values or [])[:7] if value > 0 and math.isfinite(value)]
            if len(winter_history) >= 5:
                winter_history = sorted(winter_history)[:-2]
            winter_cfg = VALUATION_CONFIG.get("winter_model", {})
            try:
                floor_years = max(1, int(winter_cfg.get("eps_floor_years", 3)))
            except (TypeError, ValueError):
                floor_years = 3
            sorted_winter_history = sorted(winter_history)
            if len(sorted_winter_history) >= floor_years:
                winter_cycle_eps = sum(sorted_winter_history[:floor_years]) / floor_years
            else:
                winter_cycle_eps = (sum(sorted_winter_history) / len(sorted_winter_history)) if sorted_winter_history else cycle_eps
            cycle_eps = winter_cycle_eps
            cyclical = True
        if regime == MarketRegime.SLOWDOWN and ai_exposed:
            # 放缓阶段把远期盈利打七折，仍保留行业模型但收紧其盈利输入。
            if model_forward_eps is not None and model_forward_eps > 0:
                model_forward_eps *= 0.70
                forward_source = f"{forward_source or '远期EPS'}（放缓调整70%）"
        high_growth = (
            growth >= 0.18
            and model_forward_eps is not None
            and model_forward_eps > 0
            and (forward_eps_observed or not high_growth_chip_signal)
        )
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
        # 部分超大市值汽车公司同时经营自动驾驶、机器人和能源存储，Yahoo
        # 往往仍只返回 Auto Manufacturers。该分类只用于进攻情景，保守卡片
        # 继续保留重资产汽车制造的下行压力测试，且要求可观察的规模/经营信号。
        mobility_cfg = VALUATION_CONFIG.get("mobility_technology", {})
        try:
            mobility_min_market_cap = float(mobility_cfg.get("min_market_cap", 1e11))
            mobility_min_growth = float(mobility_cfg.get("min_growth", 0.05))
        except (TypeError, ValueError):
            mobility_min_market_cap, mobility_min_growth = 1e11, 0.05
        mobility_technology_growth = bool(
            not ai_winter
            and not financial_or_real_estate
            and not specialized_industry
            and not defense_transition
            and market_cap is not None
            and market_cap >= mobility_min_market_cap
            and (
                mobility_technology_tag
                or (
                    electric_mobility_tag
                    and market_cap >= max(mobility_min_market_cap, float(mobility_cfg.get("large_ev_market_cap", 5e11)))
                )
            )
            and (
                growth >= mobility_min_growth
                or (revenue_growth is not None and revenue_growth >= mobility_min_growth)
                or (fcf_margin is not None and fcf_margin > 0)
                or (target_mean_price is not None and target_mean_price > 0)
                # 超大市值且业务摘要明确包含自动驾驶/AI 的公司，即使当前收入增速
                # 放缓、自由现金流为负或缺少分析师目标价，也保留成长型期权估值。
                # 保守卡片仍走汽车制造底线，成长分支只影响乐观卡片。
                or (
                    market_cap >= float(mobility_cfg.get("large_ev_market_cap", 5e11))
                    and mobility_technology_tag
                    and fcf_values
                    and latest_revenue is not None
                    and latest_revenue > 0
                )
            )
        )
        if mobility_technology_growth:
            _valuation_debug(
                "移动科技成长分类：symbol=%s signal_count=%s/%s market_cap=%s threshold=%s quality=%s classification_text=%s",
                symbol or "未知",
                mobility_signal_count,
                mobility_signal_min_count,
                market_cap,
                mobility_signal_market_cap,
                market_cap_quality,
                classification_text[:200],
            )
        # 只有明确属于硬盘、磁盘驱动器或企业级存储的公司，才允许进入
        # “结构性增长”子类。NAND/DRAM/Flash 等内存制造商即使当前 FCF
        # 和增速很高，本质仍是强周期，不能因为景气高点而使用 35–55 倍 PE。
        structural_storage = any(
            word in classification_text
            for word in ("hard disk", "disk drive", "enterprise storage")
        )
        ai_storage_growth = bool(
            ai_storage_cycle
            and storage_hardware
            and not memory_chip
            and structural_storage
            and fcf_margin is not None
            and fcf_margin >= 0.15
            and (growth >= 0.30 or (revenue_growth is not None and revenue_growth >= 0.30))
        )
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
                or (market_cap >= 1e12 and revenue_growth is None and growth_hint is None)
            )
        )
        premium_mature_growth = bool(
            mature_growth
            and market_cap
            and market_cap >= 1e12
            and (revenue_growth if revenue_growth is not None else growth) >= 0.08
        )
        if ai_winter:
            # 市场状态只改写 AI 相关生命周期模型；行业专用模型仍保持优先级。
            high_growth_chip = False
            ai_storage_cycle = False
            ai_storage_growth = False
            mobility_technology_growth = False
            mature_growth = False
            premium_mature_growth = False
            asset_heavy_transition = False
            transition = False
        if specialized_industry:
            # 专用行业模型优先于生命周期模型，避免银行/REIT 的负 FCF、药企的
            # 研发投入或平台公司的早期亏损触发普通重资产/PEG 分支。
            defense_transition = False
            asset_heavy_transition = False
            high_growth_chip = False
            cruise_operator = False
            ai_storage_cycle = False
            ai_storage_growth = False
            mature_growth = False
            premium_mature_growth = False
            digital_retail = False
        industry_model = (
            "banking" if banking_model else
            "insurance" if insurance_model else
            "reit" if reit_model else
            "biotech" if biotech_model else
            "energy_mining" if energy_mining_model else
            "utility" if utility_model else
            "internet_platform" if internet_platform_model else
            None
        )
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
        if ai_winter:
            # 寒冬状态下不让负 FCF/资本开支再次把估值送入普通转型分支。
            transition = False
        candidates: dict[str, float] = {}
        model = ""
        pe_low = pe_high = None
        industry_low = industry_high = None
        winter_value = None
        if defense_transition:
            # 国防订单驱动模型优先于通用重资产、芯片和普通转型分支。
            asset_heavy_transition = False
            high_growth_chip = False
            mature_growth = False
            digital_retail = False
            transition = False
        if ai_storage_cycle:
            # AI 存储/硬件周期优先于通用重资产和高增长芯片模型；它们使用
            # “远期 EPS 打七折 + 20–30 倍 PE”的专用估值带。
            asset_heavy_transition = False
            high_growth_chip = False
            mature_growth = False
            digital_retail = False
            transition = False
        if mobility_technology_growth:
            # 保留 asset_heavy_transition 供模型 A 计算汽车制造底线；这里只
            # 禁止它抢占模型 B 的移动科技成长分支。
            high_growth_chip = False
            mature_growth = False
            digital_retail = False
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
        if ai_winter:
            winter_eps = cycle_eps or normalized_eps
            winter_cfg = VALUATION_CONFIG.get("winter_model", {})
            if ai_revenue_share_value >= 0.50:
                winter_discount = float(winter_cfg.get("ai_share_discount_high", 0.40))
            elif ai_revenue_share_value >= 0.10:
                winter_discount = float(winter_cfg.get("ai_share_discount_medium", 0.80))
            else:
                winter_discount = float(winter_cfg.get("ai_share_discount_low", 1.0))
            if winter_eps and winter_eps > 0:
                pe_low = float(winter_cfg.get("pe_low", 8.0))
                pe_high = float(winter_cfg.get("pe_high", 12.0))
                candidates["AI寒冬周期中值 PE"] = winter_eps * (pe_low + pe_high) / 2.0 * winter_discount
            if book_value and book_value > 0:
                candidates["寒冬 P/B 底线"] = book_value * winter_discount
            if shares and cash - debt > 0:
                candidates["寒冬净现金保护"] = (cash - debt) / shares
            if not candidates and ebitda and shares:
                candidates["寒冬 EV/EBITDA 兜底"] = max(0.0, (ebitda * 6.0 - debt + cash) / shares)
            winter_candidates = [item for item in candidates.values() if math.isfinite(item) and item > 0]
            winter_value = sorted(winter_candidates)[len(winter_candidates) // 2] if winter_candidates else None
            model = "AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线"
        elif banking_model:
            roe_value = roe if roe is not None else 0.10
            if abs(roe_value) > 1:
                roe_value /= 100.0
            roe_value = min(0.40, max(-0.10, roe_value))
            bank_book = book_value if book_value and book_value > 0 else (normalized_eps / roe_value if normalized_eps and roe_value > 0 else None)
            if bank_book:
                pb_mid = min(2.75, max(0.65, 1.0 + (roe_value - 0.10) / 0.10))
                industry_low, industry_high = bank_book * max(0.65, pb_mid - 0.20), bank_book * min(3.0, pb_mid + 0.20)
                candidates["银行 P/B-ROE"] = bank_book * pb_mid
                if roe_value > 0:
                    ri_growth = min(0.04, max(0.0, growth * 0.25))
                    ri_value = bank_book + bank_book * (roe_value - required_return) / max(required_return - ri_growth, 0.035)
                    if ri_value > 0:
                        candidates["银行剩余收益 RI"] = ri_value
            model = "银行：P/B-ROE + 剩余收益 RI"
        elif insurance_model:
            ev_share = embedded_value_per_share if embedded_value_per_share and embedded_value_per_share > 0 else (book_value if book_value and book_value > 0 else None)
            nbv_share = new_business_value_per_share if new_business_value_per_share and new_business_value_per_share > 0 else 0.0
            nbv_multiple = min(4.0, max(1.5, 2.0 + max(0.0, growth) * 3.0))
            if ev_share:
                industry_low, industry_high = ev_share + nbv_share * max(1.5, nbv_multiple - 0.5), ev_share + nbv_share * min(4.5, nbv_multiple + 0.5)
                candidates["保险内含价值 + NBV"] = ev_share + nbv_share * nbv_multiple
            elif model_forward_eps:
                industry_low, industry_high = model_forward_eps * 12.0, model_forward_eps * 18.0
                candidates["保险远期盈利代理"] = (industry_low + industry_high) / 2.0
            model = "保险：内含价值 EV + 新业务价值 NBV"
        elif reit_model:
            derived_ffo = ffo_per_share
            if derived_ffo is None and shares and net_income_values:
                derived_ffo = (net_income_values[0] + abs(da)) / shares
            if derived_ffo and derived_ffo > 0:
                affo = affo_per_share if affo_per_share and affo_per_share > 0 else derived_ffo * 0.85
                industry_low, industry_high = affo * 12.0, affo * 18.0
                candidates["P/AFFO"] = affo * 15.0
                candidates["P/FFO"] = derived_ffo * 14.0
            if nav_per_share and nav_per_share > 0:
                industry_low, industry_high = max(industry_low or 0.0, nav_per_share * 0.85), max(industry_high or 0.0, nav_per_share * 1.15)
                candidates["REIT NAV"] = nav_per_share
            model = "REIT：P/FFO + P/AFFO + NAV"
        elif biotech_model:
            approved_value = approved_drug_value if approved_drug_value and approved_drug_value > 0 else (model_forward_eps * 18.0 if model_forward_eps else (normalized_eps * 18.0 if normalized_eps else 0.0))
            pipeline_value = pipeline_rnpv if pipeline_rnpv and pipeline_rnpv > 0 else 0.0
            industry_low, industry_high = approved_value * 0.75 + pipeline_value * 0.60, approved_value * 1.10 + pipeline_value * 0.90
            if industry_high > 0:
                candidates["已上市药品 DCF + 管线 rNPV"] = (industry_low + industry_high) / 2.0
            model = "生物科技：已上市药品 DCF + 管线风险调整 NPV"
        elif energy_mining_model:
            if nav_per_share and nav_per_share > 0:
                industry_low, industry_high = nav_per_share * 0.75, nav_per_share * 1.15
                candidates["资源 NAV"] = nav_per_share
            elif shares and (forward_ebitda or ebitda):
                resource_ebitda = forward_ebitda if forward_ebitda and forward_ebitda > 0 else ebitda
                industry_low, industry_high = max(0.0, (resource_ebitda * 5.0 - debt + cash) / shares), max(0.0, (resource_ebitda * 8.0 - debt + cash) / shares)
                candidates["EV/EBITDA"] = (industry_low + industry_high) / 2.0
            model = "能源/矿业：有限寿命 NAV + EV/EBITDA"
        elif utility_model:
            ddm_growth = min(0.04, max(0.01, growth * 0.35))
            ddm_rate = min(0.10, max(0.07, required_return))
            ddm_value = dividend_rate * (1 + ddm_growth) / max(ddm_rate - ddm_growth, 0.025) if dividend_rate and dividend_rate > 0 else None
            if ddm_value:
                industry_low, industry_high = ddm_value * 0.85, ddm_value * 1.15
                candidates["DDM"] = ddm_value
            if rab_per_share and rab_per_share > 0:
                industry_low, industry_high = max(industry_low or 0.0, rab_per_share * 0.90), max(industry_high or 0.0, rab_per_share * 1.20)
                candidates["RAB"] = rab_per_share
            model = "公用事业：DDM + RAB"
        elif internet_platform_model:
            revenue_per_share = latest_revenue / shares if latest_revenue and shares else None
            churn = churn_rate if churn_rate is not None and churn_rate <= 1 else ((churn_rate or 0.0) / 100.0)
            quality = 1.0 - min(0.30, max(0.0, churn))
            platform_cfg = VALUATION_CONFIG.get("internet_platform", {})
            ps_low = float(platform_cfg.get("base_ps_low", 3.0))
            ps_high = float(platform_cfg.get("base_ps_high", 8.0))
            user_revenue = None
            if platform_users and platform_users > 0 and arpu and arpu > 0:
                # ARPU 默认按月处理；以公开营收的两倍封顶，避免用户数/ARPU
                # 单位差异把平台估值放大到不合理数量级。
                user_revenue = min(latest_revenue * 2.0 if latest_revenue and latest_revenue > 0 else float("inf"), platform_users * arpu * 12.0)
                if shares and user_revenue > 0:
                    user_value_per_share = user_revenue / shares
                    candidates["用户价值校验"] = user_value_per_share * 0.50 * quality
            if revenue_per_share:
                # 防守区间始终使用广告平台基础倍数；AI 溢价只在乐观卡片启用。
                industry_low, industry_high = revenue_per_share * ps_low * quality, revenue_per_share * ps_high * quality
                if user_revenue and shares:
                    user_revenue_per_share = user_revenue / shares
                    industry_low = max(industry_low, user_revenue_per_share * 2.0 * quality)
                    industry_high = max(industry_high, user_revenue_per_share * 5.0 * quality)
                candidates["互联网平台远期 P/S"] = revenue_per_share * (ps_low + ps_high) / 2.0 * quality
            _valuation_debug(
                "互联网平台估值明细：symbol=%s revenue_per_share=%s churn=%s quality=%s user_revenue=%s",
                symbol or "未知",
                round(revenue_per_share, 4) if revenue_per_share is not None else None,
                round(churn, 4),
                round(quality, 4),
                round(user_revenue, 2) if user_revenue is not None else None,
            )
            model = "AI 互联网平台：广告/推荐基础设施 + P/S" if internet_ai_platform else "互联网平台：用户质量 + 远期 P/S"
        elif financial_or_real_estate:
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
        elif ai_storage_growth:
            # WDC/STX 等 AI 存储需求已形成结构性增长，直接使用远期 EPS，
            # 但把合理 PE 限制在 35–55 倍，保留周期风险而不回到传统硬盘估值。
            if model_forward_eps and model_forward_eps > 0:
                pe_low, pe_high = 35.0, 55.0
                candidates["AI 存储远期 EPS × PE"] = model_forward_eps * (pe_low + pe_high) / 2.0
            model = "AI 存储结构性增长：远期 EPS × 35–55 倍 PE"
        elif ai_storage_cycle:
            # STX/MU 这类存储公司处于 AI 驱动的强周期时，TTM/历史 EPS
            # 会把景气跃升完全抹掉；远期 EPS 先打七折，再限制在 20–30 倍 PE，
            # 既承认结构性需求，也避免把周期顶点利润永久化。
            cycle_forward_eps = model_forward_eps * 0.70 if model_forward_eps and model_forward_eps > 0 else None
            if cycle_forward_eps:
                pe_low, pe_high = 20.0, 30.0
                candidates["周期调整远期 EPS × PE"] = cycle_forward_eps * (pe_low + pe_high) / 2.0
            model = "AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE"
        elif mobility_technology_growth:
            # 自动驾驶/机器人/能源软件等业务用远期收入 P/S 建立成长情景。
            # 该分支只作为主估值的成长交叉项，保守卡片仍在后面按汽车/重资产
            # 资产价值计算，避免把未兑现的叙事当成确定利润。
            mobility_shares = shares or (
                market_cap / current_price
                if market_cap and current_price and current_price > 0 else None
            )
            mobility_forward_revenue = (
                latest_revenue * (1.0 + min(0.35, max(0.05, growth)))
                if latest_revenue and latest_revenue > 0 else None
            )
            if mobility_forward_revenue and mobility_shares:
                ps_low, ps_high = (4.0, 7.0) if regime == MarketRegime.SLOWDOWN else (6.0, 12.0)
                candidates["移动科技远期 P/S"] = (
                    mobility_forward_revenue * (ps_low + ps_high) / 2.0 - debt + cash
                ) / mobility_shares
                model = "移动科技成长：自动驾驶/机器人远期 P/S"
            elif model_forward_eps and model_forward_eps > 0:
                pe_low, pe_high = (22.0, 32.0) if regime == MarketRegime.SLOWDOWN else (30.0, 50.0)
                candidates["移动科技远期 EPS × PE"] = model_forward_eps * (pe_low + pe_high) / 2.0
                model = "移动科技成长：远期 EPS × PE"
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
                asset_ps_multiple = 1.5 if any(
                    word in classification_text for word in ("semiconductor", "foundry", "chip manufacturing", "integrated device")
                ) else 2.0
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

        # 小盘股最容易在 fundamentals-timeseries 被 Yahoo 限流，但 info 页仍可能
        # 给出 TTM 营收、股数和 P/S。只在已有上游错误且主模型没有候选时使用这个
        # 低置信度交叉估值，避免 ONDS 因一次 403/429 永久显示空白。
        if not candidates and upstream_errors and latest_revenue and latest_revenue > 0 and shares:
            fallback_ps = price_to_sales if price_to_sales and price_to_sales > 0 else 2.0
            fallback_ps = min(4.0, max(0.75, fallback_ps * 0.65))
            fallback_value = (latest_revenue * fallback_ps - debt + cash) / shares
            if math.isfinite(fallback_value) and fallback_value > 0:
                candidates["信息页 P/S 低置信度兜底"] = fallback_value
                model = "信息页 P/S 低置信度兜底"

        valid = [value for value in candidates.values() if math.isfinite(value) and value > 0]
        # 不同 Yahoo 报表接口偶尔混用“美元”和“百万美元”。若某个交叉口径
        # 相对 EPS 锚点小两个数量级，视为单位异常并剔除，避免中位数被 0.00 污染。
        eps_anchor = model_forward_eps or normalized_eps
        if eps_anchor and eps_anchor > 0 and len(valid) > 1 and not defense_transition:
            plausible = [item for item in valid if eps_anchor * 5 <= item <= eps_anchor * 80]
            if plausible:
                valid = plausible
        if not valid:
            retryable = any(error.endswith("_rate_limited") for error in (upstream_errors or []))
            return {
                "value": None,
                "source": None,
                "status": "retry" if retryable else "unavailable",
                "warning": "Yahoo 数据源暂时限流，估值将在稍后自动重试" if retryable else "公开财务数据不足，未生成有效估值候选",
                "data_quality": "upstream_rate_limited" if retryable else "insufficient_public_data",
            }
        # 多口径取中位数，避免单个报表异常把估值推到极端；缺少交叉口径时使用唯一模型。
        value = sorted(valid)[len(valid) // 2]
        # 结合市值/股数和当前价做单位锚定，避免百万美元与美元混用时
        # 只靠 EPS×5–80 的范围误杀低 EPS 公司的合理估值。
        price_anchor = current_price
        if price_anchor is None and market_cap and shares and shares > 0:
            price_anchor = market_cap / shares
        if price_anchor and price_anchor > 0 and len(valid) > 1:
            unit_valid = [item for item in valid if price_anchor * float(VALUATION_CONFIG["unit_price_floor_ratio"]) <= item <= price_anchor * float(VALUATION_CONFIG["unit_price_ceiling_ratio"])]
            if unit_valid:
                valid = unit_valid
                value = sorted(valid)[len(valid) // 2]
        data_source_warning = None
        if upstream_errors:
            data_source_warning = "部分 Yahoo 财务接口暂时不可用，已使用可用字段生成估值"
        if price_anchor and price_anchor > 0 and value > price_anchor * 5.0:
            # 共识目标价只用于远期 EPS 上限和人工交叉验证，不作为异常值回退的主候选。
            # 当财务口径明显失真时，使用当前价格的有限倍数，避免景气顶点共识把估值再次抬高。
            value = price_anchor * 1.5
            data_source_warning = "模型估值远高于现价，Yahoo 财务数据口径异常，已降级到现价锚定"
        if industry_low and industry_high and industry_high >= industry_low > 0:
            value = (industry_low + industry_high) / 2.0
        # AI 存储周期的远期 EPS 已经过七折处理，主估值直接展示 20–30 倍 PE
        # 的完整边界，避免单一候选再额外放大成过宽区间。
        spread = (
            (industry_high - industry_low) / max(industry_high + industry_low, 1.0)
            if industry_low and industry_high and industry_high >= industry_low
            else (0.20 if ai_storage_cycle or ai_storage_growth else (0.18 if len(valid) > 1 else (0.15 if stable else 0.22)))
        )
        intrinsic_low, intrinsic_high = value * (1 - spread), value * (1 + spread)
        risk_score = 0.0
        risk_score += 0.18 if negative_fcf else 0.0
        risk_score += 0.12 if debt > 0 and cash >= 0 and shares and debt / max(cash + 1, 1) > 3 else 0.0
        risk_score += 0.12 if high_growth or cyclical else 0.0
        risk_score += 0.10 if beta_value > 1.5 else 0.0
        risk_score += 0.15 if low_interest_coverage else 0.0
        margins_cfg = VALUATION_CONFIG.get("margins", {})
        if industry_model:
            safety_margin = float(VALUATION_CONFIG["margins"][industry_model])
        elif financial_or_real_estate:
            safety_margin = 0.25
        elif ai_storage_cycle:
            # 景气周期波动大，采用 40% 安全边际；这部分是价格纪律，
            # 不把高波动简单误读成低价值。
            safety_margin = float(VALUATION_CONFIG["margins"]["ai_storage_cycle"])
        elif high_growth_chip:
            safety_margin = float(VALUATION_CONFIG["margins"]["high_growth_chip"])
        elif mature_growth:
            safety_margin = float(margins_cfg.get("mature_growth_high_beta" if beta_value > 1.5 else "mature_growth", 0.22))
        elif transition:
            safety_margin = float(margins_cfg.get("transition_high_beta" if beta_value > 1.5 else "transition", 0.42))
        elif cyclical:
            safety_margin = float(margins_cfg.get("cyclical", 0.62))
        elif high_growth:
            safety_margin = float(margins_cfg.get("high_growth", 0.45))
        elif negative_fcf:
            safety_margin = float(margins_cfg.get("negative_fcf", 0.38))
        elif stable:
            safety_margin = float(margins_cfg.get("stable_high_beta" if beta_value > 1.5 else "stable", 0.18))
        else:
            safety_margin = float(margins_cfg.get("default", 0.30))
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
        if industry_model:
            defensive_mid = value
            defensive_spread = spread
        elif defense_transition:
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
        elif ai_storage_growth and model_forward_eps:
            # 模型 A 采用 35–45 倍 PE，承认盈利基线已抬升，同时保留估值回归空间。
            defensive_mid = model_forward_eps * 40.0
            defensive_spread = 5.0 / 40.0
        elif ai_storage_cycle and model_forward_eps:
            # 模型 A 也要承认 AI 景气已经改变盈利基线，但只给七折远期 EPS
            # 的 20–25 倍 PE；模型 B 再放宽到 20–30 倍 PE。
            cycle_forward_eps = model_forward_eps * 0.70
            defensive_mid = cycle_forward_eps * 22.5
            defensive_spread = 2.5 / 22.5
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
        if ai_winter and winter_value:
            # 寒冬估值不能被后续普通 DCF/周期候选重新抬高。
            defensive_mid = winter_value
            defensive_spread = 0.20
        if industry_model:
            defensive_mid = value
            defensive_spread = spread
        elif financial_or_real_estate and defensive_candidates:
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
        elif mobility_technology_growth and (model_forward_eps or normalized_eps):
            defensive_spread = 0.22
            # 成长分类的模型 A 固定使用汽车制造底线，不受 P/S 成长候选抬高。
            defensive_mid = (model_forward_eps or normalized_eps) * 17.5
        elif asset_heavy_transition and model_forward_eps:
            defensive_spread = 0.25
            defensive_mid = model_forward_eps * 17.5
            # 代工/云业务仍有经营资产价值；用共识目标价的 45% 作为灾难
            # 情景下限，避免 GAAP 亏损把整张卡片压成接近清算价。
            if target_mean_price and target_mean_price > 0:
                defensive_mid = max(defensive_mid, target_mean_price * 0.45)
        elif ai_storage_growth and model_forward_eps:
            defensive_spread = 5.0 / 40.0
            defensive_mid = model_forward_eps * 40.0
        elif ai_storage_cycle and model_forward_eps:
            cycle_forward_eps = model_forward_eps * 0.70
            defensive_spread = 2.5 / 22.5
            defensive_mid = cycle_forward_eps * 22.5
        elif high_growth_chip and model_forward_eps:
            defensive_spread = 2.5 / 22.5
            defensive_mid = model_forward_eps * 22.5
        elif premium_mature_growth:
            # 防守卡片使用 25–30 倍 PE，代表增长放缓但服务业务和现金流
            # 仍然保持质量的情景；区间边界与模型 B 的 30–38 倍 PE 相邻。
            defensive_spread = 2.5 / 27.5
            defensive_mid = mature_defensive_eps * 27.5
        if industry_model:
            defensive_margin = {
                "banking": 0.25,
                "insurance": 0.35,
                "reit": 0.25,
                "biotech": 0.45,
                "energy_mining": 0.35,
                "utility": 0.18,
                "internet_platform": 0.40,
            }[industry_model]
        elif financial_or_real_estate:
            defensive_margin = 0.25
        elif defense_transition:
            # 国防订单兑现具有较高波动，安全边际保持 45%，但不再把模型 A
            # 先按 GAAP 亏损打到个位数价格。
            defensive_margin = 0.45
        elif ai_storage_growth:
            defensive_margin = 0.35
        elif ai_storage_cycle:
            defensive_margin = 0.40
        elif cruise_operator:
            # 行业有债务、燃油、消费周期风险，安全边际取 25%–30%。
            defensive_margin = float(VALUATION_CONFIG["margins"]["cruise"])
        elif digital_retail:
            defensive_margin = float(VALUATION_CONFIG["margins"]["digital_retail"])
        elif asset_heavy_transition:
            # 重资产转型只允许 45%–50% 安全边际，作为下行压力测试。
            defensive_margin = min(0.50, max(float(VALUATION_CONFIG["margins"]["asset_heavy_transition"]), safety_margin))
        elif high_growth_chip:
            # 高增长芯片以仓位控制代替极端折价，模型 A 使用 30% 安全边际。
            defensive_margin = 0.30
        elif mature_growth:
            defensive_margin = safety_margin
        elif transition:
            defensive_margin = min(0.50, max(0.40, safety_margin))
        else:
            defensive_margin = min(0.70, max(0.20, safety_margin + (0.10 if negative_fcf else 0.0)))
        if ai_storage_growth:
            # 35% 安全边际对应高波动的结构性增长股；PE 区间本身已包含周期折扣。
            defensive_margin = 0.35
        elif ai_storage_cycle:
            # Beta 只用于提示波动风险，不再把周期模型额外打折；本模型固定使用
            # 40% 安全边际，与七折远期 EPS 的周期折扣分开计算。
            defensive_margin = 0.40
        if ai_winter and winter_value:
            defensive_margin = min(0.70, max(0.50, 0.60 + (0.10 if beta_value > 1.5 else 0.0)))

        # 模型 B：远期 EPS + PEG 进攻区间。只有存在远期盈利依据才输出，
        # 并把增长率限制在可解释范围，避免短期高增长制造无限估值。
        optimistic = None
        if ai_winter and winter_value:
            optimistic = {
                "value": round(winter_value, 2),
                "low": round(winter_value * 0.80, 2),
                "high": round(winter_value * 1.20, 2),
                "buy_low": round(winter_value * 0.80 * (1 - defensive_margin), 2),
                "buy_high": round(winter_value * 1.20 * (1 - defensive_margin), 2),
                "model": "AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线",
                "safety_margin": defensive_margin,
                "forward_eps": None,
                "growth_rate": round(growth, 4),
            }
        elif internet_platform_model and internet_ai_platform and latest_revenue and shares:
            # 超大市值 AI 平台：模型 A 仍使用广告平台基础 P/S，模型 B 才加入
            # 推荐系统、AI 基础设施和新商业化的成长溢价；不直接使用目标价。
            platform_cfg = VALUATION_CONFIG.get("internet_platform", {})
            platform_quality = 1.0 - min(0.30, max(0.0, churn_rate if churn_rate is not None and churn_rate <= 1 else ((churn_rate or 0.0) / 100.0)))
            platform_revenue_per_share = latest_revenue / shares
            platform_low = platform_revenue_per_share * float(platform_cfg.get("base_ps_low", 3.0)) * platform_quality
            platform_high = platform_revenue_per_share * float(platform_cfg.get("ai_ps_high", 12.0)) * platform_quality
            # 乐观情景不能因通用 spread 锚点而跌破保守卡片下限；AI 溢价只
            # 扩大上行空间，不能制造“乐观下限低于保守下限”的区间倒挂。
            optimistic_low = max(platform_low, defensive_mid * (1.0 - defensive_spread))
            optimistic_high = max(platform_high, optimistic_low)
            optimistic_margin = float(VALUATION_CONFIG["margins"].get("internet_platform", 0.40))
            optimistic = {
                "value": round((optimistic_low + optimistic_high) / 2.0, 2),
                "low": round(optimistic_low, 2),
                "high": round(optimistic_high, 2),
                "buy_low": round(optimistic_low * (1 - optimistic_margin), 2),
                "buy_high": round(optimistic_high * (1 - optimistic_margin), 2),
                "model": "AI 互联网平台：基础 P/S + AI 溢价 P/S 上限",
                "safety_margin": optimistic_margin,
                "forward_revenue_per_share": round(platform_revenue_per_share, 4),
                "growth_rate": round(min(0.50, max(0.0, growth)), 4),
            }
        elif industry_model:
            industry_margin = float(VALUATION_CONFIG["margins"][industry_model])
            industry_low = industry_low or value * (1 - spread)
            industry_high = industry_high or value * (1 + spread)
            optimistic = {
                "value": round((industry_low + industry_high) / 2.0, 2),
                "low": round(industry_low, 2),
                "high": round(industry_high, 2),
                "buy_low": round(industry_low * (1 - industry_margin), 2),
                "buy_high": round(industry_high * (1 - industry_margin), 2),
                "model": model,
                "safety_margin": industry_margin,
                "forward_eps": round(model_forward_eps, 4) if model_forward_eps else None,
                "growth_rate": round(min(0.50, max(0.0, growth)), 4),
            }
        elif ai_storage_growth and model_forward_eps:
            # 模型 B 使用远期 EPS × 35–55 倍，代表 AI 存储需求延续且利润率
            # 保持高位，但不再使用无上限 PEG。
            optimistic_pe_low, optimistic_pe_high = ((20.0, 30.0) if regime == MarketRegime.SLOWDOWN else (35.0, 55.0))
            optimistic_margin = 0.45 if regime == MarketRegime.SLOWDOWN else 0.35
            optimistic = {
                "value": round(model_forward_eps * (optimistic_pe_low + optimistic_pe_high) / 2.0, 2),
                "low": round(model_forward_eps * optimistic_pe_low, 2),
                "high": round(model_forward_eps * optimistic_pe_high, 2),
                "buy_low": round(model_forward_eps * optimistic_pe_low * (1 - optimistic_margin), 2),
                "buy_high": round(model_forward_eps * optimistic_pe_high * (1 - optimistic_margin), 2),
                "model": "AI 存储结构性增长：远期 EPS × 20–30 倍 PE（放缓）" if regime == MarketRegime.SLOWDOWN else "AI 存储结构性增长：远期 EPS × 35–55 倍 PE",
                "safety_margin": optimistic_margin,
                "forward_eps": round(model_forward_eps, 4),
                "growth_rate": round(min(0.50, max(0.10, growth)), 4),
            }
        elif ai_storage_cycle and model_forward_eps:
            # 乐观情景假设 AI 景气延续、NAND 价格维持高位，远期 EPS 不打折。
            # 周期股仍限制在 20–30 倍 PE；保守卡片继续使用七折 EPS，
            # 让两张卡片分别表达“周期回归”和“高景气延续”。
            optimistic_pe_low, optimistic_pe_high = ((15.0, 25.0) if regime == MarketRegime.SLOWDOWN else (20.0, 30.0))
            optimistic_margin = 0.50 if regime == MarketRegime.SLOWDOWN else 0.40
            optimistic = {
                "value": round(model_forward_eps * (optimistic_pe_low + optimistic_pe_high) / 2.0, 2),
                "low": round(model_forward_eps * optimistic_pe_low, 2),
                "high": round(model_forward_eps * optimistic_pe_high, 2),
                "buy_low": round(model_forward_eps * optimistic_pe_low * (1 - optimistic_margin), 2),
                "buy_high": round(model_forward_eps * optimistic_pe_high * (1 - optimistic_margin), 2),
                "model": "AI 存储/硬件周期：景气放缓 EPS × 15–25 倍 PE" if regime == MarketRegime.SLOWDOWN else "AI 存储/硬件周期：景气延续 EPS × 20–30 倍 PE",
                "safety_margin": optimistic_margin,
                "forward_eps": round(model_forward_eps, 4),
                "growth_rate": round(min(0.50, max(0.10, growth)), 4),
            }
        elif financial_or_real_estate:
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
        elif mobility_technology_growth:
            # 模型 B 只表达自动驾驶/机器人/能源等成长兑现情景；目标价用于
            # 交叉验证，不直接塞入主候选，避免把分析师共识变成估值中位数。
            mobility_shares = shares or (
                market_cap / current_price
                if market_cap and current_price and current_price > 0 else None
            )
            mobility_forward_revenue = (
                latest_revenue * (1.0 + min(0.35, max(0.05, growth)))
                if latest_revenue and latest_revenue > 0 else None
            )
            if mobility_forward_revenue and mobility_shares:
                optimistic_ps_low, optimistic_ps_high = (
                    (4.0, 7.0) if regime == MarketRegime.SLOWDOWN else (6.0, 12.0)
                )
                optimistic_margin = 0.45 if regime == MarketRegime.SLOWDOWN else 0.35
                optimistic_low = (mobility_forward_revenue * optimistic_ps_low - debt + cash) / mobility_shares
                optimistic_high = (mobility_forward_revenue * optimistic_ps_high - debt + cash) / mobility_shares
                if optimistic_high > 0:
                    optimistic = {
                        "value": round((optimistic_low + optimistic_high) / 2.0, 2),
                        "low": round(max(0.0, optimistic_low), 2),
                        "high": round(max(optimistic_low, optimistic_high), 2),
                        "buy_low": round(max(0.0, optimistic_low) * (1 - optimistic_margin), 2),
                        "buy_high": round(max(optimistic_low, optimistic_high) * (1 - optimistic_margin), 2),
                        "model": "移动科技成长：远期收入 P/S 6–12x" if regime != MarketRegime.SLOWDOWN else "移动科技成长：远期收入 P/S 4–7x（放缓）",
                        "safety_margin": optimistic_margin,
                        "forward_revenue": round(mobility_forward_revenue, 2),
                        "growth_rate": round(min(0.35, max(0.05, growth)), 4),
                    }
            elif model_forward_eps and model_forward_eps > 0:
                optimistic_pe_low, optimistic_pe_high = (
                    (22.0, 32.0) if regime == MarketRegime.SLOWDOWN else (30.0, 50.0)
                )
                optimistic_margin = 0.45 if regime == MarketRegime.SLOWDOWN else 0.35
                optimistic = {
                    "value": round(model_forward_eps * (optimistic_pe_low + optimistic_pe_high) / 2.0, 2),
                    "low": round(model_forward_eps * optimistic_pe_low, 2),
                    "high": round(model_forward_eps * optimistic_pe_high, 2),
                    "buy_low": round(model_forward_eps * optimistic_pe_low * (1 - optimistic_margin), 2),
                    "buy_high": round(model_forward_eps * optimistic_pe_high * (1 - optimistic_margin), 2),
                    "model": "移动科技成长：远期 EPS × 30–50 倍 PE" if regime != MarketRegime.SLOWDOWN else "移动科技成长：远期 EPS × 22–32 倍 PE（放缓）",
                    "safety_margin": optimistic_margin,
                    "forward_eps": round(model_forward_eps, 4),
                    "growth_rate": round(min(0.35, max(0.05, growth)), 4),
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
            optimistic_pe_low, optimistic_pe_high = ((15.0, 25.0) if regime == MarketRegime.SLOWDOWN else (30.0, 35.0))
            optimistic_margin = 0.40 if regime == MarketRegime.SLOWDOWN else 0.25
            optimistic = {
                "value": round(chip_optimistic_eps * (optimistic_pe_low + optimistic_pe_high) / 2.0, 2),
                "low": round(chip_optimistic_eps * optimistic_pe_low, 2),
                "high": round(chip_optimistic_eps * optimistic_pe_high, 2),
                "buy_low": round(chip_optimistic_eps * optimistic_pe_low * (1 - optimistic_margin), 2),
                "buy_high": round(chip_optimistic_eps * optimistic_pe_high * (1 - optimistic_margin), 2),
                "model": "高增长芯片：远期 EPS × 15–25（放缓）" if regime == MarketRegime.SLOWDOWN else "高增长芯片：远期 EPS × 30–35，并用 EV/EBITDA / P/S 验证",
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
        if optimistic and defensive_mid > 0 and not industry_model and not defense_transition and not financial_or_real_estate and not ai_storage_cycle and not ai_storage_growth and not mobility_technology_growth:
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

        if low_interest_coverage and optimistic:
            # 高杠杆公司即使 EBITDA 为正，利息也可能吞掉远期利润；收紧进攻上限。
            optimistic_high = float(optimistic.get("high") or 0.0) * 0.85
            optimistic["high"] = round(optimistic_high, 2)
            optimistic["value"] = round(min(float(optimistic.get("value") or optimistic_high), optimistic_high), 2)
            optimistic["buy_high"] = round(float(optimistic.get("buy_high") or optimistic_high) * 0.85, 2)

        defensive_model = model if model.endswith("压力测试") else f"{model} 压力测试"
        defensive = {
            "value": round(defensive_mid, 2),
            "low": round(defensive_mid * (1 - defensive_spread), 2),
            "high": round(defensive_mid * (1 + defensive_spread), 2),
            "buy_low": round(defensive_mid * (1 - defensive_spread) * (1 - defensive_margin), 2),
            "buy_high": round(defensive_mid * (1 + defensive_spread) * (1 - defensive_margin), 2),
            "model": defensive_model,
            "safety_margin": round(defensive_margin, 2),
        }
        valuation_spread = (
            abs(float(optimistic.get("high") or 0.0) - float(defensive.get("low") or 0.0))
            / max(float(defensive.get("value") or value), 1.0)
            if optimistic else 0.0
        )
        confidence_score = 100.0
        confidence_score -= min(35.0, valuation_spread * 12.0)
        confidence_score -= 20.0 if forward_eps_missing_warning else 0.0
        confidence_score -= 18.0 if negative_fcf else 0.0
        confidence_score -= 12.0 if beta_value > 1.5 else 0.0
        confidence_score -= 15.0 if low_interest_coverage else 0.0
        confidence_score -= 20.0 if ai_winter else (8.0 if regime == MarketRegime.SLOWDOWN and ai_exposed else 0.0)
        confidence_score -= max(0.0, 100.0 - data_quality_score) * 0.35
        confidence_score += 8.0 if industry_model else 0.0
        confidence_score = round(min(100.0, max(0.0, confidence_score)), 1)
        confidence_label = "高" if confidence_score >= 75 else ("低" if confidence_score < 45 else "中")
        _valuation_debug(
            "最终估值结果：symbol=%s model=%s defensive_model=%s defensive=(value=%s,low=%s,high=%s) optimistic_model=%s optimistic=(value=%s,low=%s,high=%s)",
            symbol or "未知",
            model,
            defensive.get("model"),
            defensive.get("value"),
            defensive.get("low"),
            defensive.get("high"),
            optimistic.get("model") if optimistic else None,
            optimistic.get("value") if optimistic else None,
            optimistic.get("low") if optimistic else None,
            optimistic.get("high") if optimistic else None,
        )
        return {
            "value": round(value, 2), "low": round(intrinsic_low, 2), "high": round(intrinsic_high, 2),
            "buy_low": round(buy_low, 2), "buy_high": round(buy_high, 2), "normalized_eps": round(normalized_eps, 4) if normalized_eps else None,
            "forward_eps": round(model_forward_eps, 4) if model_forward_eps else None, "forward_eps_source": forward_source,
            "pe_low": round(pe_low, 2) if pe_low else None, "pe_high": round(pe_high, 2) if pe_high else None,
            "safety_margin": round(safety_margin, 2), "validation": validation, "model": model,
            "model_label": model, "growth_rate": round(growth, 4), "required_return": round(required_return, 4),
            "decision_tree": {
                "industry_filter": industry_model or ("金融/地产" if financial_or_real_estate else "普通行业"),
                "lifecycle": (
                    industry_model or ("金融/地产"
                    if financial_or_real_estate
                    else ("国防订单驱动转型" if defense_transition else ("AI存储结构性增长" if ai_storage_growth else ("AI存储/硬件周期" if ai_storage_cycle else ("移动科技成长" if mobility_technology_growth else ("重资产转型" if asset_heavy_transition else ("高增长芯片" if high_growth_chip else ("强周期" if cyclical else ("稳定现金流" if stable_cashflow else "普通"))))))))
                )),
                "model": model,
            },
            "risk_free_rate": round(risk_free_rate, 4),
            "regime": regime,
            "regime_signals": regime_signals or {},
            "model_under_regime": f"{model} ({regime})" if regime != MarketRegime.NORMAL else model,
            "normalized_eps_source": normalized_eps_source,
            "quarterly_momentum": quarterly_momentum,
            "historical_valuation_percentiles": historical_valuation_percentiles,
            "shareholder_total_return_yield": shareholder_total_return_yield,
            "market_cap_data_quality": market_cap_quality,
            "shares_source": shares_source,
            "data_quality_score": data_quality_score,
            "owner_earnings_maintenance_ratio": round(owner_earnings_ratio, 2),
            "owner_earnings_ratio_source": owner_earnings_ratio_source,
            "confidence": confidence_label,
            "confidence_score": confidence_score,
            "interest_coverage": round(interest_coverage, 2) if interest_coverage is not None else None,
            "defensive": defensive,
            "optimistic": optimistic,
            "warnings": [warning for warning in (
                "远期 EPS 为模型估计" if forward_source == "模型估计远期EPS" else "",
                "缺乏分析师远期 EPS，已自动降级为保守模型" if forward_eps_missing_warning else "",
                "市场处于 AI 寒冬状态，已禁用远期 EPS 模型，强制回退到周期中值 + P/B 底线" if ai_winter else "",
                "市场增速放缓，远期 EPS 已按 7 折处理，估值倍数上限已压缩" if regime == MarketRegime.SLOWDOWN and ai_exposed else "",
                "现金流为负，安全边际已提高" if negative_fcf else "",
                "利息覆盖率低于 2 倍，乐观估值已收紧" if low_interest_coverage else "",
                model_warning,
                "行业专用估值模型已启用，仍需结合行业指标与安全边际判断" if industry_model else "",
                "AI 存储结构性增长仍有周期回撤风险，采用远期 EPS × 35–55 倍 PE 与 35% 安全边际" if ai_storage_growth else "",
                "AI 存储需求与周期利润存在回撤风险，远期 EPS 已按七折并采用 40% 安全边际" if ai_storage_cycle else "",
                "估值高度依赖国防订单兑现，需关注订单节奏与季度收入波动" if defense_transition else "",
                "乐观估值依赖自动驾驶/机器人/能源等业务兑现，保守估值仍按汽车制造底线计算" if mobility_technology_growth else "",
                market_cap_warning or "",
                "营收时序包含非正值，数据质量评分已下调" if revenue_values and any(value <= 0 for value in revenue_values) else "",
                "分析师覆盖不足，数据质量评分已下调" if analyst_count is not None and analyst_count < 3 else "",
                data_source_warning or "",
            ) if warning],
            "source": FAIR_VALUE_SOURCE,
            "data_quality": "partial_upstream" if upstream_errors else "complete",
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

    def ensure_fair_value(self, symbol: str) -> dict[str, Any]:
        """把轻量估值触发转发给主行情适配器，避免切换时重新抓期权链。"""
        ensure = getattr(self.regular_provider, "ensure_fair_value", None)
        if callable(ensure):
            return ensure(symbol)
        return {"value": None, "source": None, "status": "unavailable"}

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
