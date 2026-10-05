"""Candidate generators for technical, volume and option level sources."""
from __future__ import annotations
import math
from collections import deque
from statistics import median
from typing import Any, Iterable
from app.gamma import contract_gex
from app.levels_config import *  # noqa: F401,F403
from app.level_indicators import _number

Level = tuple[float, float, str]

def average_true_ranges(bars: Iterable[dict[str, Any]], period: int = ATR_PERIOD) -> list[float | None]:
    """一次性计算每根日线对应的 ATR，保持逐前缀计算的原有口径。"""
    items = list(bars)
    if period <= 0:
        return [None] * len(items)
    window: deque[float] = deque(maxlen=period)
    window_total = 0.0
    values: list[float | None] = []
    previous_close: float | None = None
    for bar in items:
        high = _number(bar.get("high"))
        low = _number(bar.get("low"))
        close = _number(bar.get("close"))
        if high is None or low is None or high < low:
            previous_close = close if close is not None and close > 0 else previous_close
            values.append(sum(window) / len(window) if window else None)
            continue
        if previous_close is None or previous_close <= 0:
            true_range = high - low
        else:
            true_range = max(high - low, abs(high - previous_close), abs(low - previous_close))
        if true_range >= 0 and math.isfinite(true_range):
            if len(window) == period:
                window_total -= window[0]
            window.append(true_range)
            window_total += true_range
        previous_close = close if close is not None and close > 0 else previous_close
        # 使用窗口求和保持与历史 average_true_range 的浮点运算顺序一致。
        values.append(sum(window) / len(window) if window else None)
    return values

def average_true_range(bars: Iterable[dict[str, Any]], period: int = ATR_PERIOD) -> float | None:
    """计算日线 ATR；数据不足时用已有真实波幅的平均值，完全无效时返回 None。"""
    if period <= 0:
        return None
    items = list(bars)
    true_ranges: list[float] = []
    previous_close: float | None = None
    for bar in items:
        high = _number(bar.get("high"))
        low = _number(bar.get("low"))
        close = _number(bar.get("close"))
        if high is None or low is None or high < low:
            previous_close = close if close is not None and close > 0 else previous_close
            continue
        if previous_close is None or previous_close <= 0:
            true_range = high - low
        else:
            true_range = max(high - low, abs(high - previous_close), abs(low - previous_close))
        if true_range >= 0 and math.isfinite(true_range):
            true_ranges.append(true_range)
        previous_close = close if close is not None and close > 0 else previous_close
    if not true_ranges:
        return None
    return sum(true_ranges[-period:]) / min(len(true_ranges), period)

def _median_true_range(highs: list[float], lows: list[float], closes: list[float]) -> float | None:
    """真实波幅中位数。不用均值，是为了不让单根插针决定摆动确认距离。"""
    ranges: list[float] = []
    previous_close: float | None = None
    for high, low, close in zip(highs, lows, closes):
        if previous_close is None or previous_close <= 0:
            true_range = high - low
        else:
            true_range = max(high - low, abs(high - previous_close), abs(low - previous_close))
        if true_range >= 0 and math.isfinite(true_range):
            ranges.append(true_range)
        previous_close = close if close > 0 else previous_close
    if not ranges:
        return None
    ranges.sort()
    return ranges[len(ranges) // 2]

def _closing_pivots(closes: list[float], scale: float) -> list[tuple[int, str]]:
    """按收盘价做摆动确认。返回 (下标, high/low)，最后一项是尚未反向确认的当前端点。"""
    if len(closes) < 2 or scale <= 0:
        return []
    pivots: list[tuple[int, str]] = []
    direction: str | None = None
    extreme_index = 0
    extreme_close = closes[0]
    low_index, low_close = 0, closes[0]
    high_index, high_close = 0, closes[0]
    for index, close in enumerate(closes):
        if direction is None:
            if close <= low_close:
                low_index, low_close = index, close
            if close >= high_close:
                high_index, high_close = index, close
            up_threshold = max(scale * FIB_PIVOT_ATR, abs(low_close) * FIB_PIVOT_MIN_RATIO)
            down_threshold = max(scale * FIB_PIVOT_ATR, abs(high_close) * FIB_PIVOT_MIN_RATIO)
            # 单边行情也要留下起点，否则没有一对端点时会退回窗口极值，插针又会回来。
            if close - low_close >= up_threshold and close - low_close >= high_close - close:
                pivots.append((low_index, "low"))
                direction = "up"
                extreme_index, extreme_close = index, close
            elif high_close - close >= down_threshold:
                pivots.append((high_index, "high"))
                direction = "down"
                extreme_index, extreme_close = index, close
            continue
        threshold = max(scale * FIB_PIVOT_ATR, abs(extreme_close) * FIB_PIVOT_MIN_RATIO)
        if direction == "up":
            if close >= extreme_close:
                extreme_index, extreme_close = index, close
            elif extreme_close - close >= threshold:
                pivots.append((extreme_index, "high"))
                direction = "down"
                extreme_index, extreme_close = index, close
        elif close <= extreme_close:
            extreme_index, extreme_close = index, close
        elif close - extreme_close >= threshold:
            pivots.append((extreme_index, "low"))
            direction = "up"
            extreme_index, extreme_close = index, close
    if direction == "up":
        pivots.append((extreme_index, "high"))
    elif direction == "down":
        pivots.append((extreme_index, "low"))
    return pivots

def fibonacci_levels(bars: Iterable[dict[str, Any]], spot: float) -> list[Level]:
    """斐波那契回撤位：以确认过的摆动高点/低点为区间，低点在前按高点向下回撤，反之按低点向上反弹。"""
    window = list(bars)[-FIB_WINDOW_BARS:]
    highs = [_number(bar.get("high")) for bar in window]
    lows = [_number(bar.get("low")) for bar in window]
    closes = [_number(bar.get("close")) for bar in window]
    if any(value is None for value in highs) or any(value is None for value in lows) or any(value is None for value in closes):
        return []
    if len(window) < 5 or spot <= 0:
        return []
    high_values = [value for value in highs if value is not None]
    low_values = [value for value in lows if value is not None]
    close_values = [value for value in closes if value is not None]
    high_index: int | None = None
    low_index: int | None = None
    scale = _median_true_range(high_values, low_values, close_values)
    if scale is not None and scale > 0:
        for index, kind in _closing_pivots(close_values, scale):
            if kind == "high":
                high_index = index
            else:
                low_index = index
    # 走不出一对摆动端点时退回窗口极值，避免横盘标的直接失去斐波那契因子。
    if high_index is None or low_index is None or high_index == low_index:
        high = max(high_values)
        low = min(low_values)
        high_index = len(high_values) - 1 - high_values[::-1].index(high)
        low_index = len(low_values) - 1 - low_values[::-1].index(low)
    else:
        high = high_values[high_index]
        low = low_values[low_index]
    if high <= low or high_index == low_index:
        return []
    span = high - low
    levels: list[Level] = []
    for ratio, weight in FIB_RATIOS:
        price = high - span * ratio if low_index < high_index else low + span * ratio
        # 百分比只展示一位小数，避免浮点乘法把 23.6 显示成 23.599999999999998。
        percent_label = f"{ratio * 100:.1f}".rstrip("0").rstrip(".")
        levels.append((price, weight, f"斐波那契 {percent_label}%"))
    return levels

def chip_peaks(bars: Iterable[dict[str, Any]], spot: float) -> list[Level]:
    """筹码密集区：把每根日线的成交量按当日价格区间做三角分配写入价格桶，平滑后取成交最密集的价位。"""
    window = list(bars)[-CHIP_WINDOW_BARS:]
    prices: list[float] = []
    for bar in window:
        low = _number(bar.get("low"))
        high = _number(bar.get("high"))
        if low is not None:
            prices.append(low)
        if high is not None:
            prices.append(high)
    if len(window) < 5 or len(prices) < 2 or spot <= 0:
        return []
    low_price, high_price = min(prices), max(prices)
    if high_price <= low_price:
        return []
    bucket = (high_price - low_price) / CHIP_BUCKETS
    profile = [0.0] * CHIP_BUCKETS
    for bar in window:
        low = _number(bar.get("low"))
        high = _number(bar.get("high"))
        volume = _number(bar.get("volume")) or 0.0
        if low is None or high is None or volume <= 0:
            continue
        if high < low:
            low, high = high, low
        start = min(max(int((low - low_price) / bucket), 0), CHIP_BUCKETS - 1)
        end = min(max(int((high - low_price) / bucket), 0), CHIP_BUCKETS - 1)
        # 三角权重：当日成交量向典型价格（高低收均值）集中，越靠近当日常规成交区分配得越多。
        typical = _number(bar.get("close"))
        if typical is None:
            typical = (low + high) / 2
        center = min(max(((typical - low_price) / bucket) - start, 0.0), float(end - start))
        weights = [1.0 - 0.8 * (abs(index - center) / max(center, end - start - center, 1.0)) for index in range(end - start + 1)]
        total = sum(weights) or 1.0
        for offset, weight in enumerate(weights):
            profile[start + offset] += volume * weight / total
    # 3 桶滑动平均：避免把单日的价格缺口当作密集区。
    smoothed = [
        (profile[max(0, index - 1)] + profile[index] + profile[min(CHIP_BUCKETS - 1, index + 1)]) / 3
        for index in range(CHIP_BUCKETS)
    ]
    peak_value = max(smoothed)
    if peak_value <= 0:
        return []
    peaks: list[tuple[float, int]] = []
    for index in range(1, CHIP_BUCKETS - 1):
        value = smoothed[index]
        if value < peak_value * CHIP_MIN_SHARE:
            continue
        if value >= smoothed[index - 1] and value > smoothed[index + 1]:
            peaks.append((value, index))
    peaks.sort(reverse=True)
    picked: list[tuple[float, int]] = []
    for value, index in peaks:
        # 相距 3 个价格桶以内的峰合并成同一个密集区，只保留更高的那个。
        if any(abs(index - other) <= 3 for _, other in picked):
            continue
        picked.append((value, index))
        if len(picked) >= CHIP_PEAK_LIMIT:
            break
    return [(low_price + (index + 0.5) * bucket, value / peak_value, "筹码密集") for value, index in picked]

def _absorption_distance(price: float, atr: float | None, atr_ratio: float, fallback_ratio: float) -> float:
    """承接位的反弹或跌破距离。有波动率时按 ATR 缩放，否则退回价格比例。"""
    if atr is not None and atr > 0:
        return max(atr * atr_ratio, price * MIN_ZONE_RATIO)
    return price * fallback_ratio

def absorption_levels(bars: Iterable[dict[str, Any]], spot: float) -> list[Level]:
    """承接位：近期被打下去、当天收回或随后反弹，并且之后没有被跌破的摆动低点。"""
    window = list(bars)[-ABSORPTION_WINDOW_BARS:]
    if len(window) < 10 or spot <= 0:
        return []
    highs = [_number(bar.get("high")) for bar in window]
    lows = [_number(bar.get("low")) for bar in window]
    closes = [_number(bar.get("close")) for bar in window]
    volumes = [_number(bar.get("volume")) or 0.0 for bar in window]
    if any(value is None for value in highs) or any(value is None for value in lows) or any(value is None for value in closes):
        return []
    atr_values = average_true_ranges(window)
    average_volume = sum(volumes) / len(volumes) if volumes else 0.0
    candidates: list[tuple[int, float, float]] = []
    for index in range(2, len(window) - 2):
        low = lows[index]  # type: ignore[index]
        if low is None or low <= 0 or low >= spot:
            continue
        neighbourhood = lows[index - 2:index + 3]
        others = [value for value in (neighbourhood[:2] + neighbourhood[3:]) if value is not None]
        # 必须明显低于相邻低点：等低平台不算「被打下来」的摆动低点。
        if not others or low >= min(others) * 0.999:
            continue
        bar_atr = atr_values[index] if index < len(atr_values) else None
        break_distance = _absorption_distance(low, bar_atr, ABSORPTION_BREAK_ATR, ABSORPTION_BREAK_FALLBACK)
        later = [value for value in lows[index + 1:] if value is not None]
        if later and min(later) < low - break_distance:
            continue
        high = highs[index]  # type: ignore[index]
        close = closes[index]  # type: ignore[index]
        recovered = close is not None and high is not None and close >= low + 0.5 * (high - low)
        if not recovered:
            rebound_distance = _absorption_distance(low, bar_atr, ABSORPTION_REBOUND_ATR, ABSORPTION_REBOUND_FALLBACK)
            later_closes = [value for value in closes[index + 1:index + 6] if value is not None]
            recovered = any(value >= low + rebound_distance for value in later_closes)
        if not recovered:
            continue
        weight = 0.6
        if average_volume and volumes[index] >= average_volume * ABSORPTION_VOLUME_RATIO:
            weight += 0.2  # 放量承接的低点更可信
        candidates.append((index, low, weight))
    # 越近的承接位越重要：按时间倒序取最近几次，权重按 0.9 的步长递减。
    ordered = sorted(candidates, key=lambda item: item[0], reverse=True)[:ABSORPTION_LIMIT]
    return [(low, weight * (0.9 ** rank), "承接位") for rank, (_, low, weight) in enumerate(ordered)]

def aggregate_strikes(rows: Iterable[dict[str, Any]], spot: float) -> list[dict[str, float]]:
    """把期权链按执行价聚合成看涨/看跌的成交量、未平仓量与 GEX，口径与页面柱状图一致。"""
    grouped: dict[float, dict[str, float]] = {}
    for row in rows:
        strike = _number(row.get("strike"))
        if strike is None or strike <= 0:
            continue
        item = grouped.setdefault(strike, {
            "strike": strike, "callVolume": 0.0, "putVolume": 0.0,
            "callOi": 0.0, "putOi": 0.0, "callGex": 0.0, "putGex": 0.0,
        })
        volume = _number(row.get("volume")) or 0.0
        open_interest = _number(row.get("open_interest")) or 0.0
        gex = contract_gex(row, spot)
        if row.get("contract_type") == "call":
            item["callVolume"] += volume
            item["callOi"] += open_interest
            item["callGex"] += gex
        else:
            item["putVolume"] += volume
            item["putOi"] += open_interest
            item["putGex"] += gex
    return [item for _, item in sorted(grouped.items())]

def _percentile(values: Iterable[float], quantile: float) -> float | None:
    """返回有限数值的线性百分位；样本为空时返回 None。"""
    ordered = sorted(value for value in values if math.isfinite(value))
    if not ordered:
        return None
    position = (len(ordered) - 1) * min(1.0, max(0.0, quantile))
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return ordered[lower]
    fraction = position - lower
    return ordered[lower] + (ordered[upper] - ordered[lower]) * fraction

def option_levels(points: list[dict[str, float]], spot: float) -> tuple[list[Level], list[Level], str]:
    """期权持仓因子：未平仓量有效时按绝对 GEX 排序，否则退回成交量。

    返回 (现价上方候选, 现价下方候选, 口径)。持仓当作磁吸位：行权价在哪一侧，
    就进入哪一侧，不再预设看涨是压力、看跌是支撑。标签仍标明该价位主要是看涨还是看跌。
    只有相对于本侧中位数明显突出、且占本侧总量达到最低比例的价位才标为「墙」。
    """
    open_interest = sum(point["callOi"] + point["putOi"] for point in points)
    volume = sum(point["callVolume"] + point["putVolume"] for point in points)
    # 未平仓量合计不足成交量 20% 时视为持仓数据不完整（上游盘前会整链返回 0），改用成交量口径。
    by_gex = open_interest > 0 and open_interest >= volume * 0.2
    grouped: list[tuple[float, float, str, str]] = []
    for point in points:
        strike = point["strike"]
        if by_gex:
            call_value = max(point["callGex"], 0.0)
            put_value = max(-point["putGex"], 0.0)
        else:
            call_value = point["callVolume"]
            put_value = point["putVolume"]
        total = call_value + put_value
        if total <= 0 or strike <= 0 or strike == spot:
            continue
        side = "above" if strike > spot else "below"
        dominant = "call" if call_value >= put_value else "put"
        grouped.append((total, strike, side, dominant))
    result: dict[str, list[Level]] = {"above": [], "below": []}
    for side in ("above", "below"):
        on_side = [item for item in grouped if item[2] == side]
        if not on_side:
            continue
        side_values = [item[0] for item in on_side]
        peak_value, peak_strike, _, _ = max(on_side)
        baseline = median(side_values)
        upper_quartile = _percentile(side_values, 0.75) or baseline
        wall_threshold = max(baseline * OPTION_WALL_MIN_PROMINENCE, upper_quartile * 1.2)
        side_total = sum(side_values)
        selected = sorted(on_side, reverse=True)[:OPTION_LIMIT]
        for value, strike, _, dominant in selected:
            wall = "看涨墙" if dominant == "call" else "看跌墙"
            holding = "看涨持仓" if dominant == "call" else "看跌持仓"
            prominence = value / max(baseline, 1e-12)
            share = value / max(side_total, 1e-12)
            is_wall = (
                strike == peak_strike
                and len(side_values) >= 3
                and prominence >= OPTION_WALL_MIN_PROMINENCE
                and value >= wall_threshold
                and share >= OPTION_WALL_MIN_SHARE
            )
            # 权重同时考虑本侧峰值与本侧总量，避免仅凭单个相对最大值抬高综合分数。
            weight = min(1.0, 0.7 * value / max(peak_value, 1e-12) + 0.3 * share / max(OPTION_WALL_MIN_SHARE, 1e-12))
            result[side].append((strike, weight, wall if is_wall else holding))
    return result["above"], result["below"], ("gex" if by_gex else "volume")
