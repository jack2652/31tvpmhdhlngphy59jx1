"""Pure response, quote and history helpers shared with the API router."""
from __future__ import annotations
import hashlib
import json
import math
from datetime import date
from typing import Any
from app.services.snapshots import market_today, snapshot_age_seconds

def snapshot_signature(value: Any) -> str:
    """为缓存键生成稳定的输入摘要，避免仅依赖时间戳漏掉同批次数据变化。"""
    encoded = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), default=str).encode("utf-8")
    return hashlib.blake2b(encoded, digest_size=16).hexdigest()

def spot_cache_bucket(value: Any) -> float | None:
    """把现价收成稳定格子，供价位缓存复用。

    格子宽度大约是价格数量级的 1/500：200 元附近约 0.2 元。
    半入规则与前端 Math.round 一致。未命中时仍用本次精确现价重算。
    """
    try:
        price = float(value)
    except (TypeError, ValueError):
        return None
    if not math.isfinite(price) or price <= 0:
        return None
    magnitude = 10 ** math.floor(math.log10(price))
    step = magnitude / 500
    units = math.floor(price / step + 0.5)
    return round(units * step, 6)

def _positive_price(value: Any) -> float | None:
    """把行情数值收成正的有限价格；缺失或无效时返回 None。"""
    if isinstance(value, bool):
        return None
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    if not math.isfinite(number) or number <= 0:
        return None
    return number

def _open_copied_from_previous(current: Any, previous: Any) -> bool:
    """今开和前一根日线开盘价几乎相同，视为未完成日线把昨开抄了进来。"""
    current_number = _positive_price(current)
    previous_number = _positive_price(previous)
    if current_number is None or previous_number is None:
        return False
    tolerance = max(0.01, abs(previous_number) * 1e-4)
    return abs(current_number - previous_number) <= tolerance

def trend_market_data(bars: list[dict[str, Any]], quote: dict[str, Any], *, today=None) -> dict[str, Any]:
    today = today or market_today
    """整理趋势面板的今开/昨收；非交易时段缺少当日 K 线时回退最近交易日。"""
    valid = []
    for bar in bars:
        try:
            day = str(bar.get("date"))[:10]
            opening = float(bar.get("open")) if bar.get("open") is not None else None
            close = float(bar.get("close")) if bar.get("close") is not None else None
        except (TypeError, ValueError):
            continue
        valid.append({"date": day, "open": opening, "close": close})
    valid.sort(key=lambda item: item["date"])
    latest = valid[-1] if valid else {}
    previous = valid[-2] if len(valid) > 1 else {}
    # 只有盘中当日日线仍可能继续变化；盘后、夜盘和休市时，最新日线已经是最近一个完整交易日。
    # 原先只比较日期，导致周一收盘后的夜盘仍把周一当成「今日」，错误返回周五收盘。
    market_state = str(quote.get("market_state") or "").upper()
    latest_is_today = (
        latest.get("date") == today().isoformat()
        and market_state == "REGULAR"
    )
    today_open = latest.get("open")
    today_open_date = latest.get("date")
    calendar_today = today().isoformat()
    # “昨开”始终指最近一个已完成交易日的常规时段开盘价。
    previous_open_row = previous if latest_is_today else latest
    previous_open = previous_open_row.get("open")
    previous_open_date = previous_open_row.get("date")
    previous_close = previous.get("close") if latest_is_today else latest.get("close")
    previous_close_date = previous.get("date") if latest_is_today else latest.get("date")
    # Yahoo 在盘后/夜盘的 fast_info.previous_close 可能仍停留在前一个交易日。
    # 扩展时段摘要中的 reference_close 是同一批分钟线对应的最近正常盘收盘价，
    # 优先使用它，避免日线缓存或上游 previous_close 落后时显示上周五数据。
    sessions = quote.get("sessions") or {}
    # 盘前的基准来自上一交易日正式收盘；盘后/夜盘才使用盘后摘要。
    # 不能固定先取 post：它可能保留 15:59 的 132.63，而 quote 已校准为正式收盘 132.60。
    if market_state == "PRE":
        regular_summary = sessions.get("pre") or {}
    else:
        regular_summary = sessions.get("post") or sessions.get("overnight") or {}
    try:
        reference_close = float(regular_summary.get("reference_close"))
    except (TypeError, ValueError):
        reference_close = None
    as_of = str(regular_summary.get("as_of") or "")[:10]
    history_is_behind_session = bool(as_of and (not latest.get("date") or latest.get("date") < as_of))
    if market_state != "REGULAR" and history_is_behind_session and reference_close is not None and reference_close > 0:
        previous_close = reference_close
        previous_close_date = as_of
    quote_open = _positive_price(quote.get("today_open"))
    # 日线还没滚到今天，或今天的开盘价和前一根日线开盘价相同，都说明今开还不可信。
    daily_missing_today = latest.get("date") != calendar_today
    daily_open_copied = (not daily_missing_today) and _open_copied_from_previous(today_open, previous.get("open"))
    if market_state in {"REGULAR", "POST", "OVERNIGHT"} and quote_open is not None and (daily_missing_today or daily_open_copied):
        today_open = quote_open
        today_open_date = calendar_today
    elif today_open is None:
        today_open = quote.get("today_open")
    if previous_close is None:
        previous_close = quote.get("previous_close")
    return {
        "today_open": today_open,
        "previous_open": previous_open,
        "previous_close": previous_close,
        "today_open_date": today_open_date,
        "previous_open_date": previous_open_date,
        "previous_close_date": previous_close_date,
        "market_state": quote.get("market_state"),
    }

def _align_quote_with_history(
    quote: dict[str, Any],
    history_payload: dict[str, Any] | None,
    max_age_seconds: int,
    *,
    today=None,
) -> dict[str, Any]:
    """用新鲜日线校准缓存行情，避免首屏 quote 与趋势通道使用不同昨收。"""
    if not quote or not isinstance(history_payload, dict):
        return quote
    age = snapshot_age_seconds(history_payload.get("fetched_at"))
    if age is None or age > max_age_seconds:
        return quote
    bars = history_payload.get("bars")
    if not isinstance(bars, list) or not bars:
        return quote
    recent_dates = sorted(str(item.get("date"))[:10] for item in bars if isinstance(item, dict) and item.get("date"))
    try:
        latest_day = date.fromisoformat(recent_dates[-1])
    except (IndexError, ValueError):
        return quote
    # 防止上游刚返回一份“抓取时间新、实际交易日很旧”的降级历史覆盖可靠 quote。
    # 正常周末/节假日最多相隔几天，超过一周说明它不能作为首屏昨收依据。
    if (market_today() - latest_day).days > 7:
        return quote
    market = trend_market_data(bars, quote, today=today)
    previous_close = _positive_price(market.get("previous_close"))
    if previous_close is None:
        return quote
    aligned = dict(quote)
    aligned["previous_close"] = previous_close
    price = _positive_price(aligned.get("price"))
    if price is not None:
        aligned["change_percent"] = (price - previous_close) / previous_close * 100
    return aligned
