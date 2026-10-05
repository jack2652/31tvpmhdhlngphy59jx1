"""Price action and technical market indicators used by level synthesis."""
from __future__ import annotations
import math
from datetime import date, timedelta
from typing import Any, Iterable
from app.levels_config import *  # stable shared constants

def _number(value: Any) -> float | None:
    """把输入转换成有限浮点数；无法转换或非有限值时返回 None。"""
    try:
        result = float(value)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return None
    return result if math.isfinite(result) else None

def _bar_day(value: Any) -> date | None:
    """把日线日期解析成 date；格式异常时返回 None。"""
    try:
        return date.fromisoformat(str(value)[:10])
    except (TypeError, ValueError):
        return None

def _extreme_point(items: list[tuple[date, float | None, float | None]], want_high: bool) -> dict[str, Any] | None:
    """在给定日线序列里取最高 / 最低价及其发生日期。"""
    best: tuple[date, float] | None = None
    for day, high, low in items:
        value = high if want_high else low
        if value is None:
            continue
        if best is None or (value > best[1] if want_high else value < best[1]):
            best = (day, value)
    if best is None:
        return None
    return {"price": round(best[1], 4), "date": best[0].isoformat()}

def price_extremes(bars: Iterable[dict[str, Any]], window_days: int = EXTREME_WINDOW_DAYS) -> dict[str, Any] | None:
    """按日线的最高/最低价算出 52 周与历史极值（含发生日期）。

    52 周窗口以序列最后一根日线为基准向前推 window_days 天（而不是系统当天），
    这样周末、停牌或数据延迟时结果依然稳定。缺少可用日线时返回 None（页面显示占位符）。
    """
    items: list[tuple[date, float | None, float | None]] = []
    for bar in bars or []:
        day = _bar_day(bar.get("date"))
        if day is None:
            continue
        items.append((day, _number(bar.get("high")), _number(bar.get("low"))))
    if not items:
        return None
    items.sort(key=lambda item: item[0])
    reference = items[-1][0]
    cutoff = reference - timedelta(days=window_days)
    window = [item for item in items if item[0] >= cutoff]
    return {
        "week52": {"high": _extreme_point(window, True), "low": _extreme_point(window, False)},
        "all_time": {"high": _extreme_point(items, True), "low": _extreme_point(items, False)},
        "window_days": window_days,
        # 便于前端与日志核对口径：基准日、窗口内/全部日线根数。
        "reference_date": reference.isoformat(),
        "bars": len(items),
        "window_bars": len(window),
    }

def relative_strength(values: list[float], period: int = RSI_PERIOD) -> dict[str, Any] | None:
    """按日线收盘价计算 Wilder RSI，并给出超买 / 超卖 / 中性。"""
    if period < 1 or len(values) < period + 1:
        return None
    gains: list[float] = []
    losses: list[float] = []
    for previous, current in zip(values, values[1:]):
        delta = current - previous
        gains.append(max(delta, 0.0))
        losses.append(max(-delta, 0.0))
    average_gain = sum(gains[:period]) / period
    average_loss = sum(losses[:period]) / period
    for gain, loss in zip(gains[period:], losses[period:]):
        average_gain = (average_gain * (period - 1) + gain) / period
        average_loss = (average_loss * (period - 1) + loss) / period
    if average_gain == 0 and average_loss == 0:
        reading = 50.0
    elif average_loss == 0:
        reading = 100.0
    else:
        reading = 100.0 - 100.0 / (1.0 + average_gain / average_loss)
    if reading >= RSI_OVERBOUGHT:
        state, label = "overbought", "超买"
    elif reading <= RSI_OVERSOLD:
        state, label = "oversold", "超卖"
    else:
        state, label = "neutral", "中性"
    return {"value": round(reading, 1), "period": period, "state": state, "label": label}

def trend_channel(bars: Iterable[dict[str, Any]]) -> dict[str, Any] | None:
    """用中期通道识别背景，并用最近窗口确认触底反弹或短线转弱。"""
    all_bars = list(bars)
    closes = [_number(bar.get("close")) for bar in all_bars]
    values = [value for value in closes if value is not None and value > 0]
    if len(values) < TREND_RECENT_WINDOW_BARS:
        return None

    def fit_channel(series: list[float]) -> dict[str, Any] | None:
        if len(series) < 20:
            return None
        count = len(series)
        mean_x = (count - 1) / 2
        mean_y = sum(series) / count
        variance = sum((index - mean_x) ** 2 for index in range(count))
        if variance <= 0 or mean_y <= 0:
            return None
        slope = sum((index - mean_x) * (value - mean_y) for index, value in enumerate(series)) / variance
        intercept = mean_y - slope * mean_x
        residuals = [value - (intercept + slope * index) for index, value in enumerate(series)]
        deviation = math.sqrt(sum(residual * residual for residual in residuals) / count)
        fitted = intercept + slope * (count - 1)
        return {
            "slope_percent": slope / mean_y * 100,
            "upper": round(fitted + TREND_CHANNEL_SIGMA * deviation, 4),
            "lower": round(fitted - TREND_CHANNEL_SIGMA * deviation, 4),
            "bars": count,
        }

    long_values = values[-TREND_WINDOW_BARS:]
    long_fit = fit_channel(long_values)
    recent_values = values[-TREND_RECENT_WINDOW_BARS:]
    recent_fit = fit_channel(recent_values)
    if long_fit is None or recent_fit is None:
        return None

    long_slope = long_fit["slope_percent"]
    recent_slope = recent_fit["slope_percent"]
    recent_change = recent_values[-1] / recent_values[0] - 1
    rebound = recent_values[-1] / min(recent_values) - 1
    pullback = max(recent_values) / recent_values[-1] - 1
    long_direction = "up" if long_slope > TREND_SLOPE_THRESHOLD else ("down" if long_slope < -TREND_SLOPE_THRESHOLD else "range")
    recent_up = recent_slope > TREND_SLOPE_THRESHOLD
    recent_down = recent_slope < -TREND_SLOPE_THRESHOLD
    reversal_up = recent_up and recent_change >= TREND_REVERSAL_MIN_CHANGE and rebound >= TREND_REVERSAL_MIN_REBOUND
    reversal_down = recent_down and recent_change <= -TREND_REVERSAL_MIN_CHANGE and pullback >= TREND_REVERSAL_MIN_REBOUND

    if reversal_up and long_direction != "up":
        selected, direction, label = recent_fit, "up", "反弹上行 · 上涨趋势"
    elif reversal_down and long_direction != "down":
        selected, direction, label = recent_fit, "down", "短线转弱 · 下跌趋势"
    else:
        selected = long_fit
        direction = long_direction
        label = "上行通道 · 上涨趋势" if direction == "up" else ("下行通道 · 下跌趋势" if direction == "down" else "区间震荡 · 方向待定")
    return {
        "direction": direction,
        "label": label,
        "slope_percent": round(selected["slope_percent"], 3),
        "upper": selected["upper"],
        "lower": selected["lower"],
        "bars": selected["bars"],
        "background_direction": long_direction,
        "reversal_confirmed": selected is recent_fit,
        "rsi": relative_strength(values),
    }

def trade_recommendation(
    trend: dict[str, Any] | None,
    support: Iterable[dict[str, Any]],
    resistance: Iterable[dict[str, Any]],
    spot: float | None,
) -> dict[str, str]:
    """综合趋势通道与最近支撑/压力，给出买入、卖出或持有建议。"""
    price = _number(spot)
    if not trend or price is None or price <= 0:
        return {"action": "hold", "label": "继续持有", "reason": "历史趋势或价位数据不足，暂不形成明确买卖信号"}

    direction = str(trend.get("direction") or "range")
    lower = _number(trend.get("lower"))
    upper = _number(trend.get("upper"))
    support_rows = list(support)
    resistance_rows = list(resistance)

    def distance(rows: list[dict[str, Any]], side: str) -> float | None:
        if not rows:
            return None
        level = _number(rows[0].get("price"))
        if level is None or level <= 0:
            return None
        gap = (price - level) / price if side == "support" else (level - price) / price
        return gap if gap >= 0 else None

    support_gap = distance(support_rows, "support")
    resistance_gap = distance(resistance_rows, "resistance")
    channel_position: float | None = None
    if lower is not None and upper is not None and upper > lower:
        channel_position = min(1.0, max(0.0, (price - lower) / (upper - lower)))

    if direction == "up":
        if (support_gap is not None and support_gap <= RECOMMENDATION_LEVEL_RATIO and (channel_position is None or channel_position <= 0.6)):
            return {"action": "buy", "label": "推荐买入", "reason": "上行趋势，现价接近支撑位"}
        if (resistance_gap is not None and resistance_gap <= RECOMMENDATION_RANGE_RATIO and channel_position is not None and channel_position >= 0.75):
            return {"action": "sell", "label": "推荐卖出", "reason": "上行通道上沿附近，现价接近压力位"}
    elif direction == "down":
        if (resistance_gap is not None and resistance_gap <= RECOMMENDATION_LEVEL_RATIO and (channel_position is None or channel_position >= 0.4)):
            return {"action": "sell", "label": "推荐卖出", "reason": "下行趋势，现价接近压力位"}
    else:
        if support_gap is not None and support_gap <= RECOMMENDATION_RANGE_RATIO and (resistance_gap is None or support_gap <= resistance_gap):
            return {"action": "buy", "label": "推荐买入", "reason": "震荡区间下沿，现价接近支撑位"}
        if resistance_gap is not None and resistance_gap <= RECOMMENDATION_RANGE_RATIO and (support_gap is None or resistance_gap < support_gap):
            return {"action": "sell", "label": "推荐卖出", "reason": "震荡区间上沿，现价接近压力位"}

    return {"action": "hold", "label": "继续持有", "reason": "趋势方向与支撑/压力信号未形成明确共振"}

def stop_loss_level(
    trend: dict[str, Any] | None,
    buy_point: dict[str, Any] | None,
    support: Iterable[dict[str, Any]],
    spot: float | None,
    atr: float | None,
) -> dict[str, Any] | None:
    """在最近有效支撑下方留出波动缓冲，生成参考止损价。"""
    price = _number(spot)
    if price is None or price <= 0:
        return None

    # 先考虑近期最佳买入区间下沿，再考虑通道下轨和普通支撑，最终取离现价最近的有效锚点。
    anchors: list[tuple[float, str]] = []
    if isinstance(buy_point, dict):
        buy_low = _number(buy_point.get("zone_low")) or _number(buy_point.get("price"))
        if buy_low is not None and 0 < buy_low < price:
            anchors.append((buy_low, "近期最佳买入点下沿"))

    channel_lower = _number((trend or {}).get("lower"))
    if channel_lower is not None and 0 < channel_lower < price:
        anchors.append((channel_lower, "趋势通道下轨"))

    for level in support:
        level_price = _number(level.get("zone_low")) or _number(level.get("price"))
        if level_price is not None and 0 < level_price < price:
            anchors.append((level_price, "最近支撑位"))

    if not anchors:
        return None
    anchor, source = max(anchors, key=lambda item: item[0])
    buffer = max((_number(atr) or 0.0) * 0.25, price * 0.005)
    stop_price = anchor - buffer
    if not math.isfinite(stop_price) or stop_price <= 0 or stop_price >= price:
        return None
    return {
        "price": round(stop_price, 4),
        "anchor_price": round(anchor, 4),
        "source": source,
        "buffer": round(buffer, 4),
        "reason": f"低于{source}并预留波动缓冲",
    }
