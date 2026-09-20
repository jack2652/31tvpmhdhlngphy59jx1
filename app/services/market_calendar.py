"""NYSE/Nasdaq 交易日历适配。"""

from __future__ import annotations

from datetime import date, datetime
from functools import lru_cache
from zoneinfo import ZoneInfo

import exchange_calendars


MARKET_TIMEZONE = ZoneInfo("America/New_York")


def localize(moment: datetime) -> datetime:
    """把时间转换为美东时区；无时区时间按美东时间解释。"""
    if moment.tzinfo is None:
        return moment.replace(tzinfo=MARKET_TIMEZONE)
    return moment.astimezone(MARKET_TIMEZONE)


@lru_cache(maxsize=1)
def nyse_calendar():
    """返回共享的 XNYS 日历实例；NYSE 与 Nasdaq 的常规交易日历一致。"""
    return exchange_calendars.get_calendar("XNYS")


def is_trading_day(value: date | datetime) -> bool:
    """判断美东日期是否为交易所交易日，包含节假日和临时休市规则。"""
    day = localize(value).date() if isinstance(value, datetime) else value
    return bool(nyse_calendar().is_session(day.isoformat()))


def regular_session_bounds(moment: datetime) -> tuple[datetime, datetime] | None:
    """返回当天实际盘中开收盘时间；完整休市日返回 None。"""
    local = localize(moment)
    calendar = nyse_calendar()
    day = local.date().isoformat()
    if not calendar.is_session(day):
        return None
    opened = calendar.session_open(day).to_pydatetime().astimezone(MARKET_TIMEZONE)
    closed = calendar.session_close(day).to_pydatetime().astimezone(MARKET_TIMEZONE)
    return opened, closed


def is_regular_session(moment: datetime) -> bool:
    """判断当前时刻是否处于交易所实际盘中，自动处理提前收盘。"""
    local = localize(moment)
    bounds = regular_session_bounds(local)
    return bounds is not None and bounds[0] <= local < bounds[1]
