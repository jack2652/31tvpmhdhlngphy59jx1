"""共享的行情适配器工具、市场时段和基础类型。"""
from __future__ import annotations
import logging
import json
import math
import re
from datetime import date, datetime, timedelta
from urllib.error import HTTPError, URLError
from zoneinfo import ZoneInfo
from app.services.market_calendar import is_regular_session, is_trading_day, localize, regular_session_bounds
from app.providers.valuation_config import VALUATION_CONFIG
logger = logging.getLogger("app.providers.market")

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

MARKET_TIMEZONE = ZoneInfo("America/New_York")

SESSION_STATES = {"pre": "PRE", "regular": "REGULAR", "post": "POST", "overnight": "OVERNIGHT"}

SESSION_FRESH_SECONDS = 30 * 60
PREVIOUS_CLOSE_AUCTION_AGREEMENT = 0.001

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
