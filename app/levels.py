"""压力位/支撑位多因子合成。

参与合成的四类因子：

- 斐波那契回撤：取回看窗口内的摆动高低点，按 23.6% / 38.2% / 50% / 61.8% / 78.6% 计算回撤位。
- 筹码分布（VPVR 简化）：把每根日线的成交量按当日价格区间做三角分配后汇总成价格剖面，取成交最密集的价位。
- 承接位：近期被砸下去又被买回、且之后没有被跌破的摆动低点。
- 期权持仓：所选到期日按执行价聚合的 GEX（整链没有有效未平仓量时退回成交量），看涨对应压力、看跌对应支撑。

合成规则：每类因子输出 (价位, 权重, 标签) 候选，按现价的 MERGE_TOLERANCE 归并同一段价位并累加权重，
每侧按「强度 ÷ (1 + 距现价%)」取前 LEVEL_COUNT 条，展示时再按离现价的距离从近到远排序。
每条价位额外给出「到达概率」：按所选到期日的隐含波动率、零漂移的几何布朗运动首次触及概率估算。

此外给出趋势通道（最近日线线性回归得到的方向与上下轨）和交易计划价位：
现价下方最近的 PLAN_COUNT 个支撑作为买入位、更深的 PLAN_COUNT 个作为加仓位，上方最近的 PLAN_COUNT 个压力作为卖出位。
"""

from __future__ import annotations

import math
from datetime import date, timedelta
from typing import Any, Iterable

from app.gamma import annotate_model_greeks, contract_gex, norm_cdf, years_to_expiry

# 每侧展示的条数
LEVEL_COUNT = 10
# 交易计划（买入 / 加仓 / 卖出）各自展示的条数
PLAN_COUNT = 5
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
# 期权持仓：每侧取分量最大的执行价作为候选（不设阈值，GEX 高度集中在墙位时仍能凑齐价位）
# 取值略大于 LEVEL_COUNT，保证「无历史行情、只按期权持仓」时也能凑齐每侧 10 条。
OPTION_LIMIT = 12
# 趋势通道：回看窗口（日线根数）、通道宽度（残差标准差倍数）、判定有方向的日均斜率阈值（%）
TREND_WINDOW_BARS = 60
TREND_CHANNEL_SIGMA = 1.5
TREND_SLOPE_THRESHOLD = 0.05

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
    """按最近若干根日线拟合线性回归通道，给出趋势方向与当前上下轨。

    以收盘价对序号做最小二乘拟合：日均斜率超过阈值视为上行/下行通道，否则视为区间震荡；
    上下轨取拟合值 ± TREND_CHANNEL_SIGMA 倍残差标准差。日线不足时返回 None（页面显示占位符）。
    """
    closes = [_number(bar.get("close")) for bar in list(bars)[-TREND_WINDOW_BARS:]]
    values = [value for value in closes if value is not None and value > 0]
    if len(values) < 20:
        return None
    count = len(values)
    mean_x = (count - 1) / 2
    mean_y = sum(values) / count
    variance = sum((index - mean_x) ** 2 for index in range(count))
    if variance <= 0 or mean_y <= 0:
        return None
    slope = sum((index - mean_x) * (value - mean_y) for index, value in enumerate(values)) / variance
    intercept = mean_y - slope * mean_x
    residuals = [value - (intercept + slope * index) for index, value in enumerate(values)]
    deviation = math.sqrt(sum(residual * residual for residual in residuals) / count)
    fitted = intercept + slope * (count - 1)
    slope_percent = slope / mean_y * 100
    if slope_percent > TREND_SLOPE_THRESHOLD:
        direction, label = "up", "上行通道 · 上涨趋势"
    elif slope_percent < -TREND_SLOPE_THRESHOLD:
        direction, label = "down", "下行通道 · 下跌趋势"
    else:
        direction, label = "range", "区间震荡 · 方向待定"
    return {
        "direction": direction,
        "label": label,
        "slope_percent": round(slope_percent, 3),
        "upper": round(fitted + TREND_CHANNEL_SIGMA * deviation, 4),
        "lower": round(fitted - TREND_CHANNEL_SIGMA * deviation, 4),
        "bars": count,
    }


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


def option_levels(points: list[dict[str, float]], spot: float) -> tuple[list[Level], list[Level], str]:
    """期权持仓因子：未平仓量有效时按 GEX 排序，否则退回成交量；返回 (看涨候选, 看跌候选, 口径)。

    候选按现价分侧选取：看涨只取现价上方的执行价、看跌只取现价下方的执行价，
    避免名额被另一侧的行权价占掉；「墙」标签只给该类型（看涨/看跌）分量最大的执行价。
    """
    open_interest = sum(point["callOi"] + point["putOi"] for point in points)
    volume = sum(point["callVolume"] + point["putVolume"] for point in points)
    # 未平仓量合计不足成交量 20% 时视为持仓数据不完整（Yahoo 盘前会整链返回 0），改用成交量口径。
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
        peak_value, peak_strike = max(values)
        on_side = [item for item in values if (item[1] > spot if side == "call" else item[1] < spot)]
        selected = sorted(on_side, reverse=True)[:OPTION_LIMIT]
        for value, strike in selected:
            wall = "看涨墙" if side == "call" else "看跌墙"
            holding = "看涨持仓" if side == "call" else "看跌持仓"
            result[side].append((strike, value / peak_value, wall if strike == peak_strike else holding))
    return result["call"], result["put"], ("gex" if by_gex else "volume")


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


def merge_candidates(candidates: Iterable[Level], spot: float, side: str, volatility: float | None = None, years: float | None = None) -> list[dict[str, Any]]:
    """把候选价位归并成最终列表：同段合并、按「强度÷(1+距现价%)」取前 LEVEL_COUNT 条，再按距离排序。"""
    if spot <= 0:
        return []
    tolerance = spot * MERGE_TOLERANCE
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
        tags: list[str] = []
        for _, _, tag in sorted(group, key=lambda member: -member[1]):
            if tag not in tags:
                tags.append(tag)
        results.append({
            "price": round(representative, 4),
            "score": sum(member[1] for member in group),
            "factors": tags,
        })
    if not results:
        return []
    # 综合优先级 = 强度 ÷ (1 + 距现价%)：同样强度时离现价越近越重要。
    # 否则十几美元以外的多因子共振位会把近端关键位（例如刚被承接住的低点）挤出名单。
    for item in results:
        distance = abs(item["price"] / spot - 1) * 100
        item["priority"] = item["score"] / (1 + distance)
    results.sort(key=lambda item: item["priority"], reverse=True)
    results = results[:LEVEL_COUNT]
    peak = max(item["score"] for item in results) or 1.0
    for item in results:
        item["score"] = round(item["score"] / peak, 2)
        item["probability"] = touch_probability(item["price"], spot, volatility, years)
        item.pop("priority", None)
    results.sort(key=lambda item: abs(item["price"] - spot))
    return results


def build_levels(
    bars: Iterable[dict[str, Any]] | None,
    chain_rows: Iterable[dict[str, Any]] | None,
    spot: Any,
    expiration: str | None = None,
    extremes: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """综合四类因子生成压力位与支撑位；缺少历史行情时自动退化为纯期权口径。"""
    bar_list = list(bars or [])
    row_list = list(chain_rows or [])
    price = _number(spot)
    if price is None or price <= 0:
        price = _number(bar_list[-1].get("close")) if bar_list else None
    if price is None or price <= 0:
        return {
            "spot": None, "resistance": [], "support": [],
            "history_bars": len(bar_list), "options_metric": "none",
            "trend": None, "extremes": extremes, "plan": {"buy": [], "add": [], "sell": []},
        }
    # 模型 IV 由合约价格反解，随后才能按与页面一致的公式计算每张合约的 GEX。
    iv_model = annotate_model_greeks(row_list, price)
    points = aggregate_strikes(row_list, price)
    call_levels, put_levels, metric = option_levels(points, price)
    # 到达概率用所选到期日的模型 IV 与剩余期限；缺任一（无期权数据/无到期日）时页面显示占位符。
    volatility = (iv_model.get(expiration) or {}).get("iv") if expiration else None
    years = years_to_expiry(expiration) if expiration else None
    # 技术面候选（斐波那契 / 筹码 / 承接位）按价位落在现价哪一侧归入压力或支撑；
    # 期权候选按惯例对应：看涨持仓计入压力、看跌持仓计入支撑。
    technical: list[Level] = []
    technical += fibonacci_levels(bar_list, price)
    technical += chip_peaks(bar_list, price)
    technical += absorption_levels(bar_list, price)
    resistance = merge_candidates(technical + call_levels, price, "above", volatility, years)
    support = merge_candidates(technical + put_levels, price, "below", volatility, years)
    return {
        "spot": price,
        "expiration": expiration,
        "history_bars": len(bar_list),
        "options_metric": metric,
        "resistance": resistance,
        "support": support,
        "trend": trend_channel(bar_list),
        # 52 周与历史高低点来自全量日线（由 HistoryService 抓取并缓存后传入）。
        "extremes": extremes,
        # 交易计划：现价下方最近的 5 个支撑为买入位、更深的 5 个为加仓位，上方最近的 5 个压力为卖出位。
        "plan": {
            "buy": support[:PLAN_COUNT],
            "add": support[PLAN_COUNT:PLAN_COUNT * 2],
            "sell": resistance[:PLAN_COUNT],
        },
    }
