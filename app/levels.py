"""压力位/支撑位多因子合成。

参与合成的四类因子：

- 斐波那契回撤：取回看窗口内的摆动高低点，按 23.6% / 38.2% / 50% / 61.8% / 78.6% 计算回撤位。
- 筹码分布（VPVR 简化）：把每根日线的成交量按当日价格区间做三角分配后汇总成价格剖面，取成交最密集的价位。
- 承接位：近期被砸下去又被买回、且之后没有被跌破的摆动低点。
- 期权持仓：多个近期期限按执行价聚合的 GEX（整链没有有效未平仓量时退回成交量），看涨对应压力、看跌对应支撑。

合成规则：每类因子输出 (价位, 权重, 标签) 候选，按现价的 MERGE_TOLERANCE 归并同一段价位并累加权重，
候选先按稳定综合强度 ÷ (1 + 距现价%) 排序；公共面板在固定名额内同时保留近端价位与远端强价位，展示时再按离现价的距离从近到远排序。
每条价位额外给出「触及概率」：按所选到期日的隐含波动率、零漂移的几何布朗运动首次触及概率估算。

此外给出趋势通道（最近日线线性回归得到的方向与上下轨）和交易计划价位：
现价下方的支撑候选平均分为买入位和加仓位，两侧各最多 PLAN_COUNT 条；上方最近的 PLAN_COUNT 个压力作为卖出位。
"""

from __future__ import annotations

import math
from datetime import date, timedelta
from statistics import median
from typing import Any, Iterable

from app.gamma import annotate_model_greeks, contract_gex, norm_cdf, years_to_expiry

# 每侧展示的条数
LEVEL_COUNT = 10
# 展示名额中优先保留的近端价位条数；剩余名额用于保留远端强支撑/强压力。
NEAR_LEVEL_COUNT = 5
# 综合强度达到此阈值时，前端使用强化色标记。
STRONG_LEVEL_SCORE = 0.7
# 相距 1.8% 内的价位视为同一支撑/压力簇，簇内强位会给相邻价位稳定加成。
STRONG_CLUSTER_DISTANCE_RATIO = 0.018
STRONG_CLUSTER_PROPAGATION = 0.8
# 期权墙必须同时满足相对突出和绝对集中，避免每侧总会有一个“最大值”被误标成墙。
OPTION_WALL_MIN_PROMINENCE = 1.8
OPTION_WALL_MIN_SHARE = 0.08
# 交易计划（买入 / 加仓 / 卖出）各自最多展示的条数
PLAN_COUNT = 10
# 同一段价位的归并容差（现价的比例）
MERGE_TOLERANCE = 0.005
# 52 周高低点的回看窗口（天）
EXTREME_WINDOW_DAYS = 365
# 斐波那契：回看窗口与回撤比例（比例, 权重；50% 与 61.8% 视为最关键的黄金分割位）
FIB_WINDOW_BARS = 120
FIB_RATIOS = ((0.236, 0.6), (0.382, 0.9), (0.5, 1.0), (0.618, 1.0), (0.786, 0.6))
# 筹码分布：回看窗口、价格桶数量、密集区数量上限、低于峰值该比例的桶忽略
CHIP_WINDOW_BARS = 120
CHIP_BUCKETS = 240
CHIP_PEAK_LIMIT = 6
CHIP_MIN_SHARE = 0.12
# 承接位：回看窗口、数量上限、反弹幅度、未被跌破的判定容差、放量加成阈值
ABSORPTION_WINDOW_BARS = 60
ABSORPTION_LIMIT = 4
ABSORPTION_REBOUND = 1.01
ABSORPTION_BREAK_TOLERANCE = 0.998
ABSORPTION_VOLUME_RATIO = 1.2
# ATR 区域：使用最近 14 根日线，区域宽度取 ATR 的一部分并设置最小百分比。
ATR_PERIOD = 14
ATR_ZONE_RATIO = 0.3
MIN_ZONE_RATIO = 0.0025
# 用 ATR 估计候选聚类距离，固定比例仅作为上下限，避免高低波动标的使用同一距离。
ATR_MERGE_RATIO = 0.35
MIN_MERGE_RATIO = 0.0025
MAX_MERGE_RATIO = 0.012
# 历史回踩验证：样本不足时只展示模型候选，不标记为强位；近期多次反弹的多因子位可标为重点位。
LEVEL_HISTORY_LOOKAHEAD = 5
LEVEL_HISTORY_MIN_SAMPLES = 3
LEVEL_HISTORY_MIN_HOLD_RATE = 0.65
LEVEL_HISTORY_MAX_BREAK_RATE = 0.25
LEVEL_HISTORY_BREAK_ATR = 0.25
LEVEL_HISTORY_REBOUND_ATR = 0.25
# 近期反应窗口约 2 个月；至少出现两次反弹才给“重点”提示，不升级为“强”位。
LEVEL_RECENT_WINDOW_BARS = 45
LEVEL_RECENT_MIN_SAMPLES = 2
LEVEL_RECENT_MIN_REACTIONS = 2
# 期权持仓：每侧取分量最大的执行价作为候选（不设阈值，GEX 高度集中在墙位时仍能凑齐价位）
# 取值略大于 LEVEL_COUNT，保证「无历史行情、只按期权持仓」时也能凑齐每侧 10 条。
OPTION_LIMIT = 12
# 趋势通道：回看窗口（日线根数）、通道宽度（残差标准差倍数）、判定有方向的日均斜率阈值（%）
TREND_WINDOW_BARS = 60
TREND_RECENT_WINDOW_BARS = 20
TREND_CHANNEL_SIGMA = 1.5
TREND_SLOPE_THRESHOLD = 0.05
# 反转确认：最近窗口同时满足斜率、累计涨跌幅和从局部极值的反弹/回撤幅度。
TREND_REVERSAL_MIN_CHANGE = 0.05
TREND_REVERSAL_MIN_REBOUND = 0.08
# 操作建议：价位距离使用现价比例，避免不同价格规模的标的使用同一绝对距离。
RECOMMENDATION_LEVEL_RATIO = 0.025
RECOMMENDATION_RANGE_RATIO = 0.015
# 最佳买卖点定义为短线机会，使用未来 5 个交易日的触及概率。
TRADE_POINT_TRADING_DAYS = 5

Level = tuple[float, float, str]


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


def best_trade_points(
    trend: dict[str, Any] | None,
    support: Iterable[dict[str, Any]],
    resistance: Iterable[dict[str, Any]],
    spot: float | None,
    volatility: float | None = None,
    horizon_trading_days: int = TRADE_POINT_TRADING_DAYS,
) -> dict[str, dict[str, Any] | None]:
    """从支撑/压力候选中选出未来短线窗口的最佳买卖区间。"""
    price = _number(spot)
    if price is None or price <= 0:
        return {"buy": None, "sell": None}
    direction = str((trend or {}).get("direction") or "range")
    horizon_years = max(int(horizon_trading_days), 1) / 252.0

    def select(rows: Iterable[dict[str, Any]], side: str) -> dict[str, Any] | None:
        candidates: list[tuple[float, dict[str, Any]]] = []
        for row in rows:
            level_price = _number(row.get("price"))
            if level_price is None or level_price <= 0:
                continue
            if side == "buy" and level_price >= price:
                continue
            if side == "sell" and level_price <= price:
                continue
            strength = min(1.0, max(0.0, _number(row.get("score")) or 0.0))
            factors = row.get("factors") if isinstance(row.get("factors"), list) else []
            factor_agreement = min(1.0, len(factors) / 3.0)
            short_touch_probability = touch_probability(level_price, price, volatility, horizon_years)
            fallback_probability = _number(row.get("probability"))
            touch_score = short_touch_probability if short_touch_probability is not None else (fallback_probability if fallback_probability is not None else 0.5)
            distance_ratio = abs(level_price / price - 1)
            proximity = max(0.0, 1.0 - min(distance_ratio / 0.2, 1.0))
            trend_alignment = 1.0 if (side == "buy" and direction == "up") or (side == "sell" and direction == "down") else (0.78 if direction == "range" else 0.62)
            confidence = (
                strength * 0.4
                + factor_agreement * 0.2
                + touch_score * 0.15
                + proximity * 0.1
                + trend_alignment * 0.15
            )
            candidates.append((confidence, row))
        if not candidates:
            return None
        confidence, row = max(candidates, key=lambda item: item[0])
        low = _number(row.get("zone_low")) or _number(row.get("price")) or price
        high = _number(row.get("zone_high")) or _number(row.get("price")) or price
        return {
            "price": _number(row.get("price")),
            "zone_low": round(min(low, high), 4),
            "zone_high": round(max(low, high), 4),
            "confidence": round(min(1.0, max(0.0, confidence)), 4),
            "factors": list(row.get("factors") or []),
            "reason": "多因子共振" if len(row.get("factors") or []) >= 2 else "单一主因子，需结合行情确认",
        }

    return {"buy": select(support, "buy"), "sell": select(resistance, "sell")}


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


def fibonacci_levels(bars: Iterable[dict[str, Any]], spot: float) -> list[Level]:
    """斐波那契回撤位：以回看窗口内的摆动高点/低点为区间，低点在前按高点向下回撤，反之按低点向上反弹。"""
    window = list(bars)[-FIB_WINDOW_BARS:]
    highs = [_number(bar.get("high")) for bar in window]
    lows = [_number(bar.get("low")) for bar in window]
    if any(value is None for value in highs) or any(value is None for value in lows):
        return []
    if len(window) < 5 or spot <= 0:
        return []
    high = max(highs)  # type: ignore[arg-type]
    low = min(lows)  # type: ignore[arg-type]
    if high <= low:
        return []
    # 取两个摆动端点的最后一次出现位置，判定当前处于上涨段还是下跌段。
    high_index = len(highs) - 1 - highs[::-1].index(high)  # type: ignore[arg-type]
    low_index = len(lows) - 1 - lows[::-1].index(low)  # type: ignore[arg-type]
    if high_index == low_index:
        return []
    span = high - low
    levels: list[Level] = []
    for ratio, weight in FIB_RATIOS:
        price = high - span * ratio if low_index < high_index else low + span * ratio
        levels.append((price, weight, f"斐波那契 {ratio * 100:g}%"))
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
        later = [value for value in lows[index + 1:] if value is not None]
        if later and min(later) < low * ABSORPTION_BREAK_TOLERANCE:
            continue
        high = highs[index]  # type: ignore[index]
        close = closes[index]  # type: ignore[index]
        recovered = close is not None and high is not None and close >= low + 0.5 * (high - low)
        if not recovered:
            later_closes = [value for value in closes[index + 1:index + 6] if value is not None]
            recovered = any(value >= low * ABSORPTION_REBOUND for value in later_closes)
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
    """期权持仓因子：未平仓量有效时按 GEX 排序，否则退回成交量；返回 (看涨候选, 看跌候选, 口径)。

    候选按现价分侧选取：看涨只取现价上方的执行价、看跌只取现价下方的执行价，
    避免名额被另一侧的行权价占掉。只有相对于本侧中位数明显突出、且占本侧总量达到最低比例的价位才标为「墙」。
    """
    open_interest = sum(point["callOi"] + point["putOi"] for point in points)
    volume = sum(point["callVolume"] + point["putVolume"] for point in points)
    # 未平仓量合计不足成交量 20% 时视为持仓数据不完整（上游盘前会整链返回 0），改用成交量口径。
    by_gex = open_interest > 0 and open_interest >= volume * 0.2
    result: dict[str, list[Level]] = {"call": [], "put": []}
    for side in ("call", "put"):
        values: list[tuple[float, float]] = []
        for point in points:
            strike = point["strike"]
            if by_gex:
                value = max(point["callGex"], 0.0) if side == "call" else max(-point["putGex"], 0.0)
            else:
                value = point["callVolume"] if side == "call" else point["putVolume"]
            if value > 0 and strike > 0:
                values.append((value, strike))
        if not values:
            continue
        on_side = [item for item in values if (item[1] > spot if side == "call" else item[1] < spot)]
        if not on_side:
            continue
        side_values = [item[0] for item in on_side]
        peak_value, peak_strike = max(on_side)
        baseline = median(side_values)
        upper_quartile = _percentile(side_values, 0.75) or baseline
        wall_threshold = max(baseline * OPTION_WALL_MIN_PROMINENCE, upper_quartile * 1.2)
        side_total = sum(side_values)
        selected = sorted(on_side, reverse=True)[:OPTION_LIMIT]
        for value, strike in selected:
            wall = "看涨墙" if side == "call" else "看跌墙"
            holding = "看涨持仓" if side == "call" else "看跌持仓"
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
    return result["call"], result["put"], ("gex" if by_gex else "volume")


def _factor_group(factor: str) -> str:
    """把展示标签归并为独立证据类别，避免同类期权档位重复计分。"""
    if "看涨" in factor or "看跌" in factor:
        return "options"
    if factor.startswith("斐波那契"):
        return "fibonacci"
    if factor == "筹码密集":
        return "chips"
    if factor == "承接位":
        return "absorption"
    return f"other:{factor}"


def _composite_strength(raw_score: float, factors: list[str]) -> float:
    """把因子原始权重转换为保守且稳定的 0~1 综合强度。

    原始权重已经按各自因子归一化，不能直接相加后把「单一普通期权持仓」当成强位。
    这里先压缩单因子贡献，再只对不同类别的独立证据加成；明确期权墙、关键斐波那契
    和承接位保留差异化基础分，避免现价小幅变化时强弱大幅跳变。
    """
    factor_set = set(factors)
    groups = {_factor_group(factor) for factor in factor_set}
    normalized_raw = min(1.0, max(0.0, raw_score))
    # 单一普通因子最高约 56%，不让相对峰值直接等价于强支撑/强压力。
    score = 0.25 + 0.31 * normalized_raw
    if any("看涨墙" in factor or "看跌墙" in factor for factor in factor_set):
        score = max(score, 0.72)
    elif any(factor in {"斐波那契 50%", "斐波那契 61.8%"} for factor in factor_set):
        score = max(score, 0.62)
    elif "承接位" in factor_set:
        score = max(score, 0.58)
    # 只有不同证据类别才形成共振；同类持仓多个行权价不重复奖励。
    score += min(0.28, max(0, len(groups) - 1) * 0.14)
    if "承接位" in factor_set and len(groups) >= 2:
        score += 0.04
    return min(1.0, max(0.0, score))


# 到达概率：几何布朗运动（零漂移）在剩余期限 T 内首次触及该价位的概率。
# 对数价格是漂移为 0 的布朗运动，由反射原理 P(max >= b) = 2*Phi(-b/(sigma*sqrt(T)))；上下两侧同式，故取 |ln(L/S)|。
def touch_probability(price: float, spot: float, volatility: float | None, years: float | None) -> float | None:
    """首次触及概率；波动率或期限缺失时返回 None（页面显示占位符）。"""
    if spot <= 0 or price <= 0 or not volatility or not years or volatility <= 0 or years <= 0:
        return None
    sigma_time = volatility * math.sqrt(years)
    if sigma_time <= 0:
        return None
    distance = abs(math.log(price / spot))
    return round(min(1.0, 2.0 * norm_cdf(-distance / sigma_time)), 4)


def merge_candidates(
    candidates: Iterable[Level],
    spot: float,
    side: str,
    volatility: float | None = None,
    years: float | None = None,
    side_weight: float = 1.0,
    zone_width: float | None = None,
    limit: int | None = None,
    merge_tolerance: float | None = None,
) -> list[dict[str, Any]]:
    """合并候选价位，并返回稳定综合分数、动态区域与触及概率。"""
    if spot <= 0:
        return []
    tolerance = max(0.0, _number(merge_tolerance) or spot * MERGE_TOLERANCE)
    filtered = [
        (price, weight, tag) for price, weight, tag in candidates
        if (price > spot if side == "above" else price < spot)
    ]
    ordered = sorted(filtered, key=lambda item: item[0])
    groups: list[list[tuple[float, float, str]]] = []
    for price, weight, tag in ordered:
        # 以组内首个价位为锚：组宽不超过容差，避免相邻档位逐级「接龙」把整段行权价串成一组。
        if groups and abs(price - groups[-1][0][0]) <= tolerance:
            groups[-1].append((price, weight, tag))
        else:
            groups.append([(price, weight, tag)])
    results: list[dict[str, Any]] = []
    for group in groups:
        representative = max(group, key=lambda member: member[1])[0]
        raw_low = min(member[0] for member in group)
        raw_high = max(member[0] for member in group)
        tags: list[str] = []
        for _, _, tag in sorted(group, key=lambda member: -member[1]):
            if tag not in tags:
                tags.append(tag)
        results.append({
            "price": round(representative, 4),
            "score": sum(member[1] for member in group) * max(side_weight, 0.0),
            "factors": tags,
            "raw_low": raw_low,
            "raw_high": raw_high,
        })
    if not results:
        return []
    # 综合强度使用固定规则，不再除以当前集合的峰值；同一价位在现价小幅变化时保持稳定。
    for item in results:
        composite = _composite_strength(item["score"], item["factors"])
        # 趋势只做小幅方向偏置，避免墙位的基础分把上行支撑与下行压力拉成同分。
        item["score"] = min(1.0, max(0.0, composite + (side_weight - 1.0) * 0.15))
        item["model_score"] = round(item["score"], 2)
    # 同一支撑/压力簇内的邻近价位共享一部分强度，避免 143-145 或 177-185
    # 被切成两段后只有其中一段有颜色。
    for item in results:
        nearby = [
            other["score"]
            for other in results
            if other is not item
            and abs(other["price"] / item["price"] - 1) <= STRONG_CLUSTER_DISTANCE_RATIO
            and other["score"] >= STRONG_LEVEL_SCORE
        ]
        if nearby:
            item["score"] = max(item["score"], max(nearby) * STRONG_CLUSTER_PROPAGATION)
    for item in results:
        distance = abs(item["price"] / spot - 1) * 100
        item["priority"] = item["score"] / (1 + distance)
    results.sort(key=lambda item: item["priority"], reverse=True)
    result_limit = LEVEL_COUNT if limit is None else max(int(limit), 0)
    results = results[:result_limit]
    width = max(_number(zone_width) or 0.0, spot * MIN_ZONE_RATIO)
    for item in results:
        item["score"] = round(item["score"], 2)
        item["probability"] = touch_probability(item["price"], spot, volatility, years)
        if side == "above":
            item["zone_low"] = round(max(spot, item["raw_low"] - width), 4)
            item["zone_high"] = round(item["raw_high"] + width, 4)
        else:
            item["zone_low"] = round(max(0.0, item["raw_low"] - width), 4)
            item["zone_high"] = round(min(spot, item["raw_high"] + width), 4)
        item.pop("raw_low", None)
        item.pop("raw_high", None)
        item.pop("priority", None)
    # ATR 区域可能覆盖相邻价位；用代表价中点切开重叠部分，保留所有候选但避免页面出现重复区间。
    price_order = sorted(results, key=lambda item: item["price"])
    for left, right in zip(price_order, price_order[1:]):
        if left["zone_high"] <= right["zone_low"]:
            continue
        boundary = round((left["price"] + right["price"]) / 2, 4)
        left["zone_high"] = min(left["zone_high"], boundary)
        right["zone_low"] = max(right["zone_low"], boundary)
    results.sort(key=lambda item: abs(item["price"] - spot))
    return results


def _has_independent_level_evidence(factors: Iterable[str]) -> bool:
    """判断价位是否至少有两个独立证据类别，或有经过门槛筛选的期权墙。"""
    groups: set[str] = set()
    has_wall = False
    has_absorption = False
    for factor in set(factors):
        if "看涨墙" in factor or "看跌墙" in factor:
            has_wall = True
        if factor == "承接位":
            has_absorption = True
        groups.add(_factor_group(factor))
    return has_wall or has_absorption or len(groups) >= 2


def level_strength_tier(level: dict[str, Any]) -> str:
    """根据模型分数、历史回踩和近期反应分级，区分重点位与强位。"""
    score = _number(level.get("score")) or 0.0
    model_score = _number(level.get("model_score")) or score
    factors = level.get("factors") if isinstance(level.get("factors"), list) else []
    if model_score < 0.55 or not _has_independent_level_evidence(factors):
        return "normal"
    samples = int(_number(level.get("history_samples")) or 0)
    if samples < LEVEL_HISTORY_MIN_SAMPLES:
        # 深层价位往往尚未被当前日线再次回踩；筹码密集、承接位或多因子共振仍保留重点色，
        # 但使用“重点”标签，明确它不是已经完成历史命中率验证的强位。
        factor_set = set(factors)
        groups = {_factor_group(factor) for factor in factor_set}
        if "承接位" in factor_set or {"chips", "absorption"}.issubset(groups) or len(groups) >= 2:
            return "reinforced"
        return "normal"
    if score >= STRONG_LEVEL_SCORE:
        hold_rate = _number(level.get("history_hold_rate")) or 0.0
        break_rate = _number(level.get("history_break_rate")) or 0.0
        if hold_rate >= LEVEL_HISTORY_MIN_HOLD_RATE and break_rate <= LEVEL_HISTORY_MAX_BREAK_RATE:
            return "strong"
    # 最近约两个月如果同一价位至少两次出现反弹，说明当前仍有承接/抛压反应；
    # 这只给“重点”提示，不替代长期守住率验证，也不把它误称为强位。
    recent_samples = int(_number(level.get("recent_samples")) or 0)
    recent_reactions = int(_number(level.get("recent_reactions")) or 0)
    if recent_samples >= LEVEL_RECENT_MIN_SAMPLES and recent_reactions >= LEVEL_RECENT_MIN_REACTIONS:
        return "reinforced"
    if score < STRONG_LEVEL_SCORE:
        return "normal"
    return "normal"


def annotate_level_history(
    levels: Iterable[dict[str, Any]],
    bars: Iterable[dict[str, Any]],
    side: str,
    lookahead: int = LEVEL_HISTORY_LOOKAHEAD,
) -> list[dict[str, Any]]:
    """用历史日线评估候选区域被触及后的守住/跌破结果。

    这是当前数据条件下的历史反应统计，不是未来信息训练出的保证概率；
    同一轮连续触及只计一次，避免横盘时重复放大样本。
    """
    items = list(levels)
    history = list(bars)
    horizon = max(int(lookahead), 1)
    atr = average_true_range(history)
    if atr is None or atr <= 0 or len(history) <= horizon:
        for item in items:
            item["history_samples"] = 0
            item["history_hold_rate"] = None
            item["history_break_rate"] = None
            item["history_reaction_rate"] = None
            item["recent_samples"] = 0
            item["recent_reactions"] = 0
            item["recent_reaction_rate"] = None
            item["strength_tier"] = level_strength_tier(item)
        return items

    for item in items:
        zone_low = _number(item.get("zone_low"))
        zone_high = _number(item.get("zone_high"))
        center = _number(item.get("price"))
        if zone_low is None or zone_high is None or center is None or center <= 0:
            item["history_samples"] = 0
            item["history_hold_rate"] = None
            item["history_break_rate"] = None
            item["history_reaction_rate"] = None
            item["recent_samples"] = 0
            item["recent_reactions"] = 0
            item["recent_reaction_rate"] = None
            item["strength_tier"] = level_strength_tier(item)
            continue

        samples = holds = breaks = 0
        reactions = 0
        sample_results: list[tuple[int, bool, bool]] = []
        last_touch = -horizon
        break_buffer = max(atr * LEVEL_HISTORY_BREAK_ATR, center * MIN_ZONE_RATIO)
        rebound_buffer = max(atr * LEVEL_HISTORY_REBOUND_ATR, center * MIN_ZONE_RATIO)
        for index in range(0, len(history) - horizon):
            if index - last_touch < horizon:
                continue
            bar_low = _number(history[index].get("low"))
            bar_high = _number(history[index].get("high"))
            if bar_low is None or bar_high is None or bar_high < zone_low or bar_low > zone_high:
                continue
            future = history[index + 1:index + 1 + horizon]
            future_lows = [_number(bar.get("low")) for bar in future]
            future_highs = [_number(bar.get("high")) for bar in future]
            future_closes = [_number(bar.get("close")) for bar in future]
            future_lows = [value for value in future_lows if value is not None]
            future_highs = [value for value in future_highs if value is not None]
            future_closes = [value for value in future_closes if value is not None]
            if not future_lows or not future_highs or not future_closes:
                continue
            last_touch = index
            samples += 1
            if side == "support":
                broken = min(future_lows) < zone_low - break_buffer
                reacted = max(future_closes) >= center + rebound_buffer
            else:
                broken = max(future_highs) > zone_high + break_buffer
                reacted = min(future_closes) <= center - rebound_buffer
            if broken:
                breaks += 1
            else:
                holds += 1
            if reacted:
                reactions += 1
            sample_results.append((index, broken, reacted))

        hold_rate = holds / samples if samples else None
        break_rate = breaks / samples if samples else None
        item["history_samples"] = samples
        item["history_hold_rate"] = round(hold_rate, 4) if hold_rate is not None else None
        item["history_break_rate"] = round(break_rate, 4) if break_rate is not None else None
        item["history_reaction_rate"] = round((reactions / samples), 4) if samples else None
        recent_cutoff = max(0, len(history) - LEVEL_RECENT_WINDOW_BARS)
        recent_results = [result for result in sample_results if result[0] >= recent_cutoff]
        recent_samples = len(recent_results)
        recent_reactions = sum(1 for _, _, reacted in recent_results if reacted)
        item["recent_samples"] = recent_samples
        item["recent_reactions"] = recent_reactions
        item["recent_reaction_rate"] = round(recent_reactions / recent_samples, 4) if recent_samples else None
        if samples:
            # 样本少时只做有限修正，避免三次历史触及就完全覆盖多因子模型分数。
            historical_weight = min(0.25, samples / 20.0)
            base_score = _number(item.get("score")) or 0.0
            empirical_score = max(0.0, min(1.0, (hold_rate or 0.0) * (1.0 - (break_rate or 0.0))))
            item["score"] = round(base_score * (1.0 - historical_weight) + empirical_score * historical_weight, 2)
        item["strength_tier"] = level_strength_tier(item)
    return items


def select_visible_levels(levels: Iterable[dict[str, Any]], spot: float, limit: int = LEVEL_COUNT) -> list[dict[str, Any]]:
    """在固定展示数量内同时保留近端价位和远端强价位。"""
    items = list(levels)
    if limit <= 0 or len(items) <= limit:
        return items[:max(limit, 0)]
    nearest = sorted(items, key=lambda item: abs(item["price"] / spot - 1))
    selected = nearest[:min(NEAR_LEVEL_COUNT, limit)]
    selected_ids = {id(item) for item in selected}
    strong = sorted(
        (item for item in items if (item.get("strength_tier", "strong" if item.get("score", 0) >= STRONG_LEVEL_SCORE else "") == "strong") and id(item) not in selected_ids),
        key=lambda item: (-item["score"], abs(item["price"] / spot - 1)),
    )
    for item in strong:
        if len(selected) >= limit:
            break
        selected.append(item)
        selected_ids.add(id(item))
    for item in nearest:
        if len(selected) >= limit:
            break
        if id(item) not in selected_ids:
            selected.append(item)
            selected_ids.add(id(item))
    return sorted(selected, key=lambda item: abs(item["price"] - spot))


def adaptive_merge_tolerance(spot: float, atr: float | None) -> float:
    """按 ATR 计算候选聚类距离，并限制在合理的价格比例范围内。"""
    if spot <= 0:
        return 0.0
    volatility_width = (atr or 0.0) * ATR_MERGE_RATIO
    return min(spot * MAX_MERGE_RATIO, max(spot * MIN_MERGE_RATIO, volatility_width))


def split_support_plan(levels: Iterable[dict[str, Any]], spot: float) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    """把支撑池拆成近端买入和更深加仓两档，并优先保留更深处的高质量区域。"""
    ordered = sorted(list(levels), key=lambda item: abs((_number(item.get("price")) or spot) - spot))
    if not ordered:
        return [], []
    split = min(PLAN_COUNT, max(1, (len(ordered) + 1) // 2))
    buy = ordered[:split]
    add_candidates = ordered[split:]
    add = sorted(
        add_candidates,
        key=lambda item: (-(_number(item.get("score")) or 0.0), abs((_number(item.get("price")) or spot) - spot)),
    )[:PLAN_COUNT]
    add.sort(key=lambda item: abs((_number(item.get("price")) or spot) - spot))
    return buy, add


def build_levels(
    bars: Iterable[dict[str, Any]] | None,
    chain_rows: Iterable[dict[str, Any]] | None,
    spot: Any,
    expiration: str | None = None,
    extremes: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """综合技术面与多期限期权因子，生成带动态区域的压力位、支撑位和交易计划。"""
    bar_list = list(bars or [])
    row_list = list(chain_rows or [])
    price = _number(spot)
    if price is None or price <= 0:
        price = _number(bar_list[-1].get("close")) if bar_list else None
    if price is None or price <= 0:
        return {
            "spot": None, "resistance": [], "support": [],
            "history_bars": len(bar_list), "options_metric": "none",
            "trend": None, "recommendation": {"action": "hold", "label": "继续持有", "reason": "缺少有效价格数据"},
            "trade_points": {"buy": None, "sell": None},
            "trade_points_horizon": {"trading_days": TRADE_POINT_TRADING_DAYS, "label": "未来 5 个交易日"},
            "extremes": extremes,
            "plan": {"buy": [], "add": [], "sell": []},
        }
    # 模型 IV 由合约价格反解，随后才能按与页面一致的公式计算每张合约的 GEX。
    iv_model = annotate_model_greeks(row_list, price)
    points = aggregate_strikes(row_list, price)
    call_levels, put_levels, metric = option_levels(points, price)
    # 触及概率继续使用页面选中的期限；多期限只参与墙位强度，不改变图表和概率口径。
    volatility = (iv_model.get(expiration) or {}).get("iv") if expiration else None
    years = years_to_expiry(expiration) if expiration else None
    # 技术面候选（斐波那契 / 筹码 / 承接位）按价位落在现价哪一侧归入压力或支撑；
    # 期权候选按惯例对应：看涨持仓计入压力、看跌持仓计入支撑。
    technical: list[Level] = []
    technical += fibonacci_levels(bar_list, price)
    technical += chip_peaks(bar_list, price)
    technical += absorption_levels(bar_list, price)
    trend = trend_channel(bar_list)
    if trend and trend["direction"] == "up":
        resistance_weight, support_weight = 0.9, 1.15
    elif trend and trend["direction"] == "down":
        resistance_weight, support_weight = 1.15, 0.9
    else:
        resistance_weight = support_weight = 1.0
    atr = average_true_range(bar_list)
    zone_width = max((atr or 0.0) * ATR_ZONE_RATIO, price * MIN_ZONE_RATIO)
    # 公共面板最终展示 10 条，但内部候选池保留 30 条，避免远端强支撑/强压力在选强位前被截掉。
    # 交易计划仍从完整候选池中分出买入和加仓两档。
    candidate_limit = LEVEL_COUNT * 3
    merge_tolerance = adaptive_merge_tolerance(price, atr)
    resistance_all = merge_candidates(
        technical + call_levels, price, "above", volatility, years, resistance_weight, zone_width,
        limit=candidate_limit, merge_tolerance=merge_tolerance,
    )
    support_all = merge_candidates(
        technical + put_levels, price, "below", volatility, years, support_weight, zone_width,
        limit=candidate_limit, merge_tolerance=merge_tolerance,
    )
    resistance_all = annotate_level_history(resistance_all, bar_list, "resistance")
    support_all = annotate_level_history(support_all, bar_list, "support")
    resistance = select_visible_levels(resistance_all, price, LEVEL_COUNT)
    support = select_visible_levels(support_all, price, LEVEL_COUNT)
    buy_levels, add_levels = split_support_plan(support_all, price)
    recommendation = trade_recommendation(trend, support, resistance, price)
    trade_points = best_trade_points(trend, support_all, resistance_all, price, volatility, TRADE_POINT_TRADING_DAYS)
    return {
        "spot": price,
        "expiration": expiration,
        "history_bars": len(bar_list),
        "options_metric": metric,
        "resistance": resistance,
        "support": support,
        "trend": trend,
        "recommendation": recommendation,
        "trade_points": trade_points,
        "trade_points_horizon": {"trading_days": TRADE_POINT_TRADING_DAYS, "label": "未来 5 个交易日"},
        # 52 周与历史高低点来自全量日线（由 HistoryService 抓取并缓存后传入）。
        "extremes": extremes,
        # 交易计划：支撑候选平均分为买入位和加仓位，两侧各最多 10 条；压力位最多 10 条。
        "plan": {
            "buy": buy_levels,
            "add": add_levels,
            "sell": resistance_all[:PLAN_COUNT],
        },
    }
