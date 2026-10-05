"""压力位/支撑位多因子合成。

参与合成的四类因子：

- 斐波那契回撤：用收盘价确认的摆动高低点，按 23.6% / 38.2% / 50% / 61.8% / 78.6% 计算回撤位。单根插针不会重画整组价位。
- 筹码分布（VPVR 简化）：把每根日线的成交量按当日价格区间做三角分配后汇总成价格剖面，取成交最密集的价位。
- 承接位：近期被砸下去又被买回、且之后没有被跌破的摆动低点。反弹和跌破距离按 ATR 缩放。
- 期权持仓：多个近期期限按执行价聚合的绝对 GEX（整链没有有效未平仓量时退回成交量）。行权价在现价上方视为压力侧磁吸，下方视为支撑侧磁吸，不再把看涨一律当压力、看跌一律当支撑。

合成规则：每类因子输出 (价位, 权重, 标签) 候选，按现价的 MERGE_TOLERANCE 归并同一段价位并累加权重，
候选先按稳定综合强度 ÷ (1 + 距现价%) 排序；公共面板在固定名额内同时保留近端价位与远端强价位，展示时再按离现价的距离从近到远排序。
每条价位额外给出「触及概率」：按所选到期日的隐含波动率、零漂移的几何布朗运动首次触及概率估算。

此外给出趋势通道（最近日线线性回归得到的方向与上下轨）和交易计划价位：
现价下方的支撑候选平均分为买入位和加仓位，两侧各最多 PLAN_COUNT 条；上方最近的 PLAN_COUNT 个压力作为卖出位。
"""

from __future__ import annotations

import math
from collections import deque
from datetime import date, timedelta
from statistics import median
from typing import Any, Iterable

from app.gamma import annotate_model_greeks, contract_gex, norm_cdf, years_to_expiry
from app.buyer_structures import build_buyer_structures

from app.levels_config import *  # noqa: F401,F403
from app.level_candidates import (
    _closing_pivots, _median_true_range, _percentile, _absorption_distance,
    absorption_levels, aggregate_strikes, average_true_range, average_true_ranges,
    chip_peaks, fibonacci_levels, option_levels,
)
from app.level_indicators import (
    _bar_day, _extreme_point, _number, price_extremes, relative_strength,
    stop_loss_level, trade_recommendation, trend_channel,
)

# 每侧展示的条数
# 展示名额中优先保留的近端价位条数；剩余名额用于保留远端强支撑/强压力。
# 综合强度达到此阈值时，前端使用强化色标记。
# 相距 1.8% 内的价位视为同一支撑/压力簇，簇内强位会给相邻价位稳定加成。
# 期权墙必须同时满足相对突出和绝对集中，避免每侧总会有一个“最大值”被误标成墙。
# 交易计划（买入 / 加仓 / 卖出）各自最多展示的条数
# 同一段价位的归并容差（现价的比例）
# 52 周高低点的回看窗口（天）
# 斐波那契：回看窗口与回撤比例（比例, 权重；50% 与 61.8% 视为最关键的黄金分割位）
# 收盘价反向运行达到该距离才承认摆动端点。用真实波幅中位数，避免一根插针把确认距离放大。
# 筹码分布：回看窗口、价格桶数量、密集区数量上限、低于峰值该比例的桶忽略
# 承接位：回看窗口、数量上限、反弹/跌破按 ATR 缩放，波动数据不足时退回固定比例。
# ATR 区域：使用最近 14 根日线，区域宽度取 ATR 的一部分并设置最小百分比。
# 用 ATR 估计候选聚类距离，固定比例仅作为上下限，避免高低波动标的使用同一距离。
# 历史回踩验证：样本不足时只展示模型候选，不标记为强位；近期多次反弹的多因子位可标为重点位。
# 强位需要比“重点位”更多的历史触及样本，避免少量样本把颜色直接推到最强等级。
# 近期反应窗口约 2 个月；至少出现两次反弹才给“重点”提示，不升级为“强”位。
# 当前候选主要由最近 120 根日线形成；验证时留出这段数据，避免用形成候选的样本验证候选。
# 历史比例使用轻量 Beta 先验收缩，避免 3 次触及全部守住就显示成确定性结果。
# 期权持仓：每侧取分量最大的执行价作为候选（不设阈值，GEX 高度集中在墙位时仍能凑齐价位）
# 取值略大于 LEVEL_COUNT，保证「无历史行情、只按期权持仓」时也能凑齐每侧 10 条。
# 趋势通道：回看窗口（日线根数）、通道宽度（残差标准差倍数）、判定有方向的日均斜率阈值（%）
# 反转确认：最近窗口同时满足斜率、累计涨跌幅和从局部极值的反弹/回撤幅度。
# 相对强弱：Wilder RSI(14)。70 及以上视为超买，30 及以下视为超卖。
# 操作建议：价位距离使用现价比例，避免不同价格规模的标的使用同一绝对距离。
# 最佳买卖点定义为短线机会，使用未来 5 个交易日的触及概率。

Level = tuple[float, float, str]

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
        candidates: list[tuple[float, dict[str, Any], float, float]] = []
        for row in rows:
            level_price = _number(row.get("price"))
            if level_price is None or level_price <= 0:
                continue
            if side == "buy" and level_price >= price:
                continue
            if side == "sell" and level_price <= price:
                continue
            # merge_candidates 保留未经历史校准的 model_score；最佳点在这里单独融合历史表现，避免历史数据被重复加权。
            strength = min(1.0, max(0.0, _number(row.get("model_score")) or _number(row.get("score")) or 0.0))
            factors = row.get("factors") if isinstance(row.get("factors"), list) else []
            factor_groups = {_factor_group(str(factor)) for factor in factors}
            factor_agreement = min(1.0, len(factor_groups) / 3.0)
            short_touch_probability = touch_probability(level_price, price, volatility, horizon_years)
            fallback_probability = _number(row.get("probability"))
            touch_score = short_touch_probability if short_touch_probability is not None else (fallback_probability if fallback_probability is not None else 0.5)
            distance_ratio = abs(level_price / price - 1)
            proximity = max(0.0, 1.0 - min(distance_ratio / 0.2, 1.0))
            trend_alignment = 1.0 if (side == "buy" and direction == "up") or (side == "sell" and direction == "down") else (0.78 if direction == "range" else 0.62)
            model_confidence = (
                strength * 0.4
                + factor_agreement * 0.2
                + touch_score * 0.15
                + proximity * 0.1
                + trend_alignment * 0.15
            )
            adjusted_hold_rate = _number(row.get("history_adjusted_hold_rate"))
            hold_rate = adjusted_hold_rate if adjusted_hold_rate is not None else _number(row.get("history_hold_rate"))
            sample_confidence = min(1.0, max(0.0, _number(row.get("history_confidence")) or 0.0))
            history_weight = min(0.25, sample_confidence * 0.25) if hold_rate is not None else 0.0
            # 样本越多，历史守住率对模型分数的影响越大；样本不足时只做轻量修正。
            historical_score = hold_rate if hold_rate is not None else 0.5
            confidence = model_confidence * (1.0 - history_weight) + historical_score * history_weight
            candidates.append((confidence, row, model_confidence, history_weight))
        if not candidates:
            return None
        confidence, row, model_confidence, history_weight = max(candidates, key=lambda item: item[0])
        low = _number(row.get("zone_low")) or _number(row.get("price")) or price
        high = _number(row.get("zone_high")) or _number(row.get("price")) or price
        adjusted_hold_rate = _number(row.get("history_adjusted_hold_rate"))
        hold_rate = adjusted_hold_rate if adjusted_hold_rate is not None else _number(row.get("history_hold_rate"))
        adjusted_break_rate = _number(row.get("history_adjusted_break_rate"))
        break_rate = adjusted_break_rate if adjusted_break_rate is not None else _number(row.get("history_break_rate"))
        history_samples = int(_number(row.get("history_samples")) or 0)
        return {
            "price": _number(row.get("price")),
            "zone_low": round(min(low, high), 4),
            "zone_high": round(max(low, high), 4),
            "confidence": round(min(1.0, max(0.0, confidence)), 4),
            "model_confidence": round(min(1.0, max(0.0, model_confidence)), 4),
            "history_weight": round(history_weight, 4),
            "history_samples": history_samples,
            "history_hold_rate": round(hold_rate, 4) if hold_rate is not None else None,
            "history_break_rate": round(break_rate, 4) if break_rate is not None else None,
            "history_sample_confidence": round(min(1.0, max(0.0, _number(row.get("history_confidence")) or 0.0)), 4),
            "factors": list(row.get("factors") or []),
            "reason": "多因子共振" if len(row.get("factors") or []) >= 2 else "单一主因子，需结合行情确认",
        }

    # 买入区和卖出区允许保留各自完整的技术/期权区域；不再用两个代表价的中点
    # 动态裁剪边界，避免最佳买入点变化时把卖出区间下沿一起推来推去。
    return {"buy": select(support, "buy"), "sell": select(resistance, "sell")}

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
            # 区域边界由候选价位和 ATR 宽度决定；不能用现价裁剪，否则现价靠近压力位时下沿会随报价跳动。
            item["zone_low"] = round(max(0.0, item["raw_low"] - width), 4)
            item["zone_high"] = round(item["raw_high"] + width, 4)
        else:
            item["zone_low"] = round(max(0.0, item["raw_low"] - width), 4)
            # 支撑区域同样保留自身的上沿，避免现价变化造成区域边界漂移。
            item["zone_high"] = round(item["raw_high"] + width, 4)
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
    adjusted_hold_rate = _number(level.get("history_adjusted_hold_rate"))
    adjusted_break_rate = _number(level.get("history_adjusted_break_rate"))
    if adjusted_hold_rate is None:
        adjusted_hold_rate = _number(level.get("history_hold_rate"))
    if adjusted_break_rate is None:
        adjusted_break_rate = _number(level.get("history_break_rate"))
    confidence = _number(level.get("history_confidence"))
    if confidence is None:
        prior_total = LEVEL_HISTORY_PRIOR_HOLD + LEVEL_HISTORY_PRIOR_BREAK
        confidence = samples / (samples + prior_total) if samples else 0.0
    if samples < LEVEL_HISTORY_MIN_SAMPLES:
        # 深层价位往往尚未被当前日线再次回踩；筹码密集、承接位或多因子共振仍保留重点色，
        # 但使用“重点”标签，明确它不是已经完成历史命中率验证的强位。
        factor_set = set(factors)
        groups = {_factor_group(factor) for factor in factor_set}
        if "承接位" in factor_set or {"chips", "absorption"}.issubset(groups) or len(groups) >= 2:
            return "reinforced"
        return "normal"
    if score >= STRONG_LEVEL_SCORE:
        hold_rate = adjusted_hold_rate or 0.0
        break_rate = adjusted_break_rate or 0.0
        if (
            samples >= LEVEL_HISTORY_STRONG_MIN_SAMPLES
            and confidence >= LEVEL_HISTORY_STRONG_MIN_CONFIDENCE
            and hold_rate >= LEVEL_HISTORY_MIN_HOLD_RATE
            and break_rate <= LEVEL_HISTORY_MAX_BREAK_RATE
        ):
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
    exclude_recent: int = 0,
    atr_by_index: list[float | None] | None = None,
) -> list[dict[str, Any]]:
    """用历史日线评估候选区域被触及后的守住/跌破结果。

    这是当前数据条件下的历史反应统计，不是未来信息训练出的保证概率；
    同一轮连续触及只计一次，避免横盘时重复放大样本。调用方可以排除最近的候选形成窗口，
    让验证只使用更早的留出数据。
    """
    items = list(levels)
    history = list(bars)
    horizon = max(int(lookahead), 1)
    excluded = max(int(exclude_recent), 0)
    validation_end = max(0, len(history) - excluded)

    def set_empty_history(item: dict[str, Any], method: str) -> None:
        item["history_samples"] = 0
        item["history_hold_rate"] = None
        item["history_break_rate"] = None
        item["history_reaction_rate"] = None
        item["history_adjusted_hold_rate"] = None
        item["history_adjusted_break_rate"] = None
        item["history_confidence"] = 0.0
        item["validation_method"] = method
        item["recent_samples"] = 0
        item["recent_reactions"] = 0
        item["recent_reaction_rate"] = None
        item["strength_tier"] = level_strength_tier(item)

    atr_values = atr_by_index if atr_by_index is not None else average_true_ranges(history)
    atr = average_true_range(history)
    if atr is None or atr <= 0 or validation_end <= horizon:
        for item in items:
            set_empty_history(item, "insufficient_history")
        return items
    # 每个历史触及点只使用当日及之前的波动率，避免当前 ATR 把未来波动信息带回旧样本。

    for item in items:
        zone_low = _number(item.get("zone_low"))
        zone_high = _number(item.get("zone_high"))
        center = _number(item.get("price"))
        if zone_low is None or zone_high is None or center is None or center <= 0:
            set_empty_history(item, "invalid_level")
            continue

        samples = holds = breaks = 0
        reactions = 0
        sample_results: list[tuple[int, bool, bool]] = []
        last_touch = -horizon
        for index in range(0, validation_end - horizon):
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
            sample_atr = (atr_values[index] if index < len(atr_values) else None) or atr
            sample_break_buffer = max(sample_atr * LEVEL_HISTORY_BREAK_ATR, center * MIN_ZONE_RATIO)
            sample_rebound_buffer = max(sample_atr * LEVEL_HISTORY_REBOUND_ATR, center * MIN_ZONE_RATIO)
            if side == "support":
                broken = min(future_lows) < zone_low - sample_break_buffer
                reacted = max(future_closes) >= center + sample_rebound_buffer
            else:
                broken = max(future_highs) > zone_high + sample_break_buffer
                reacted = min(future_closes) <= center - sample_rebound_buffer
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
        prior_total = LEVEL_HISTORY_PRIOR_HOLD + LEVEL_HISTORY_PRIOR_BREAK
        adjusted_hold_rate = (
            (holds + LEVEL_HISTORY_PRIOR_HOLD) / (samples + prior_total)
            if samples else None
        )
        adjusted_break_rate = (
            (breaks + LEVEL_HISTORY_PRIOR_BREAK) / (samples + prior_total)
            if samples else None
        )
        item["history_adjusted_hold_rate"] = round(adjusted_hold_rate, 4) if adjusted_hold_rate is not None else None
        item["history_adjusted_break_rate"] = round(adjusted_break_rate, 4) if adjusted_break_rate is not None else None
        item["history_confidence"] = round(samples / (samples + prior_total), 4) if samples else 0.0
        item["validation_method"] = "holdout_price_action" if excluded else "in_sample_price_action"
        recent_cutoff = max(0, validation_end - LEVEL_RECENT_WINDOW_BARS)
        recent_results = [result for result in sample_results if result[0] >= recent_cutoff]
        recent_samples = len(recent_results)
        recent_reactions = sum(1 for _, _, reacted in recent_results if reacted)
        item["recent_samples"] = recent_samples
        item["recent_reactions"] = recent_reactions
        item["recent_reaction_rate"] = round(recent_reactions / recent_samples, 4) if recent_samples else None
        if samples:
            # 样本少时只做有限修正，并使用收缩后的比例，避免三次历史触及就完全覆盖多因子模型分数。
            historical_weight = min(0.25, samples / 20.0)
            base_score = _number(item.get("score")) or 0.0
            empirical_score = max(
                0.0,
                min(1.0, (adjusted_hold_rate or 0.0) * (1.0 - (adjusted_break_rate or 0.0))),
            )
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
    candidate_spot: Any = None,
) -> dict[str, Any]:
    """综合技术面与多期限期权因子，生成带动态区域的压力位、支撑位和交易计划。

    ``spot`` 是当前展示口径的价格；``candidate_spot`` 是同一快照内稳定的候选锚点。
    两者分开后，实时价与盘后价切换只会改变距离和概率，不会重复生成两套候选池。
    """
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
            "stop_loss": None,
            "trade_points_horizon": {"trading_days": TRADE_POINT_TRADING_DAYS, "label": "未来 5 个交易日"},
            "buyer_structures": {
                "available": False,
                "status": "unavailable",
                "items": [],
                "horizon_trading_days": TRADE_POINT_TRADING_DAYS,
                "horizon_label": "未来 5 个交易日",
                "reason": "缺少有效价格数据",
            },
            "extremes": extremes,
            "plan": {"buy": [], "add": [], "sell": []},
        }
    candidate_price = _number(candidate_spot)
    if candidate_price is None or candidate_price <= 0:
        candidate_price = price
    # 模型 IV 由合约价格反解，随后才能按与页面一致的公式计算每张合约的 GEX。
    iv_model = annotate_model_greeks(row_list, candidate_price)
    points = aggregate_strikes(row_list, candidate_price)
    above_levels, below_levels, metric = option_levels(points, candidate_price)
    # 触及概率继续使用页面选中的期限；多期限只参与墙位强度，不改变图表和概率口径。
    volatility = (iv_model.get(expiration) or {}).get("iv") if expiration else None
    years = years_to_expiry(expiration) if expiration else None
    # 技术面和期权候选都按价位落在现价哪一侧归入压力或支撑。期权是磁吸位，不按看涨/看跌预设方向。
    technical: list[Level] = []
    technical += fibonacci_levels(bar_list, candidate_price)
    technical += chip_peaks(bar_list, candidate_price)
    technical += absorption_levels(bar_list, candidate_price)
    trend = trend_channel(bar_list)
    if trend and trend["direction"] == "up":
        resistance_weight, support_weight = 0.9, 1.15
    elif trend and trend["direction"] == "down":
        resistance_weight, support_weight = 1.15, 0.9
    else:
        resistance_weight = support_weight = 1.0
    atr_by_index = average_true_ranges(bar_list)
    atr = average_true_range(bar_list)
    zone_width = max((atr or 0.0) * ATR_ZONE_RATIO, candidate_price * MIN_ZONE_RATIO)
    # 公共面板最终展示 10 条，但内部候选池保留 30 条，避免远端强支撑/强压力在选强位前被截掉。
    # 交易计划仍从完整候选池中分出买入和加仓两档。
    candidate_limit = LEVEL_COUNT * 3
    merge_tolerance = adaptive_merge_tolerance(candidate_price, atr)
    resistance_all = merge_candidates(
        technical + above_levels, candidate_price, "above", volatility, years, resistance_weight, zone_width,
        limit=candidate_limit, merge_tolerance=merge_tolerance,
    )
    support_all = merge_candidates(
        technical + below_levels, candidate_price, "below", volatility, years, support_weight, zone_width,
        limit=candidate_limit, merge_tolerance=merge_tolerance,
    )
    # 候选池以稳定锚点生成；当前口径只负责重新分侧和更新触及概率。
    resistance_all = [item for item in resistance_all if (_number(item.get("price")) or 0.0) > price]
    support_all = [item for item in support_all if (_number(item.get("price")) or 0.0) < price]
    resistance_all.sort(key=lambda item: abs((_number(item.get("price")) or price) - price))
    support_all.sort(key=lambda item: abs((_number(item.get("price")) or price) - price))
    for item in resistance_all + support_all:
        item["probability"] = touch_probability(item["price"], price, volatility, years)
    resistance_all = annotate_level_history(
        resistance_all,
        bar_list,
        "resistance",
        exclude_recent=LEVEL_HISTORY_FORMATION_BARS,
        atr_by_index=atr_by_index,
    )
    support_all = annotate_level_history(
        support_all,
        bar_list,
        "support",
        exclude_recent=LEVEL_HISTORY_FORMATION_BARS,
        atr_by_index=atr_by_index,
    )
    resistance = select_visible_levels(resistance_all, price, LEVEL_COUNT)
    support = select_visible_levels(support_all, price, LEVEL_COUNT)
    buy_levels, add_levels = split_support_plan(support_all, price)
    recommendation = trade_recommendation(trend, support, resistance, price)
    trade_points = best_trade_points(trend, support_all, resistance_all, price, volatility, TRADE_POINT_TRADING_DAYS)
    stop_loss = stop_loss_level(trend, trade_points.get("buy"), support_all, price, atr)
    buyer_structures = build_buyer_structures(
        row_list,
        price,
        trend,
        recommendation,
        support_all,
        resistance_all,
        TRADE_POINT_TRADING_DAYS,
        iv_model=iv_model,
    )
    return {
        "spot": price,
        "candidate_spot": candidate_price,
        "expiration": expiration,
        "history_bars": len(bar_list),
        "options_metric": metric,
        "resistance": resistance,
        "support": support,
        "trend": trend,
        "recommendation": recommendation,
        "trade_points": trade_points,
        "stop_loss": stop_loss,
        "trade_points_horizon": {"trading_days": TRADE_POINT_TRADING_DAYS, "label": "未来 5 个交易日"},
        "buyer_structures": buyer_structures,
        "history_validation": {
            "method": "holdout_price_action",
            "lookahead_bars": LEVEL_HISTORY_LOOKAHEAD,
            "excluded_recent_bars": LEVEL_HISTORY_FORMATION_BARS,
        },
        # 52 周与历史高低点来自全量日线（由 HistoryService 抓取并缓存后传入）。
        "extremes": extremes,
        # 交易计划：支撑候选平均分为买入位和加仓位，两侧各最多 10 条；压力位最多 10 条。
        "plan": {
            "buy": buy_levels,
            "add": add_levels,
            "sell": resistance_all[:PLAN_COUNT],
        },
    }
