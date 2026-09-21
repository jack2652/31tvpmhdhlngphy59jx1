"""期权买方结构分析。

首版只使用当前已缓存的期权链和项目统一的 Black-Scholes 模型，输出少量可执行的候选结构。
这里的评分是结构质量评分，不是历史胜率；所有成本都使用有效买卖价中间价。
"""

from __future__ import annotations

import math
from datetime import date, datetime, timezone
from typing import Any, Iterable
from zoneinfo import ZoneInfo

from app.gamma import (
    RISK_FREE,
    annotate_model_greeks,
    black_scholes_price,
    norm_cdf,
    years_to_expiry,
)

MARKET_TIMEZONE = ZoneInfo("America/New_York")
HORIZON_TRADING_DAYS = 5
# 用约一周的自然日覆盖未来 5 个交易日，避免推荐临近到期、情景窗口已经超过合约寿命的结构。
MIN_CANDIDATE_DTE = 7
PREFERRED_SHORT_DTE = 14
TIME_TOLERANCE_DTE = 20
MAX_QUOTE_SPREAD_RATIO = 0.10
GOOD_QUOTE_SPREAD_RATIO = 0.05
TARGET_FALLBACK_RATIO = 0.05
DIRECTION_STRUCTURE_LIMIT = 5
STRUCTURE_LIMIT = DIRECTION_STRUCTURE_LIMIT * 2


def _number(value: Any) -> float | None:
    try:
        result = float(value)
    except (TypeError, ValueError):
        return None
    return result if math.isfinite(result) else None


def _mid_quote(row: dict[str, Any]) -> tuple[float, float] | None:
    """返回 (中间价, 买卖价差占中间价比例)，无有效双边报价时返回 None。"""
    bid = _number(row.get("bid"))
    ask = _number(row.get("ask"))
    if bid is None or ask is None or bid <= 0 or ask < bid:
        return None
    mid = (bid + ask) / 2
    if mid <= 0:
        return None
    return mid, (ask - bid) / mid


def _dte(expiration: str, now: datetime | None = None) -> int | None:
    try:
        expiry = date.fromisoformat(expiration)
    except (TypeError, ValueError):
        return None
    current = now or datetime.now(timezone.utc)
    if current.tzinfo is None:
        current = current.replace(tzinfo=timezone.utc)
    return max((expiry - current.astimezone(MARKET_TIMEZONE).date()).days, 0)


def _normal_pdf(value: float) -> float:
    return math.exp(-0.5 * value * value) / math.sqrt(2 * math.pi)


def _greeks(spot: float, strike: float, volatility: float, years: float, contract_type: str) -> dict[str, float] | None:
    if spot <= 0 or strike <= 0 or volatility <= 0 or years <= 0:
        return None
    volatility_time = volatility * math.sqrt(years)
    if volatility_time <= 0:
        return None
    d1 = (math.log(spot / strike) + (RISK_FREE + 0.5 * volatility**2) * years) / volatility_time
    d2 = d1 - volatility_time
    if contract_type == "call":
        delta = norm_cdf(d1)
        theta = -spot * _normal_pdf(d1) * volatility / (2 * math.sqrt(years)) - RISK_FREE * strike * math.exp(-RISK_FREE * years) * norm_cdf(d2)
    else:
        delta = norm_cdf(d1) - 1
        theta = -spot * _normal_pdf(d1) * volatility / (2 * math.sqrt(years)) + RISK_FREE * strike * math.exp(-RISK_FREE * years) * norm_cdf(-d2)
    vega = spot * _normal_pdf(d1) * math.sqrt(years)
    return {"delta": delta, "theta_per_day": theta / 365, "vega": vega}


def _target_price(
    direction: str,
    spot: float,
    support: Iterable[dict[str, Any]],
    resistance: Iterable[dict[str, Any]],
    trend: dict[str, Any] | None,
) -> tuple[float | None, str]:
    rows = resistance if direction == "call" else support
    candidates = []
    for row in rows:
        value = _number(row.get("price"))
        if value is None or value <= 0:
            continue
        if (direction == "call" and value > spot) or (direction == "put" and value < spot):
            candidates.append(value)
    if candidates:
        value = min(candidates, key=lambda item: abs(item - spot))
        return value, "综合压力位" if direction == "call" else "综合支撑位"
    channel_value = _number((trend or {}).get("upper" if direction == "call" else "lower"))
    if channel_value is not None and ((direction == "call" and channel_value > spot) or (direction == "put" and channel_value < spot)):
        return channel_value, "趋势通道"
    return spot * (1 + TARGET_FALLBACK_RATIO if direction == "call" else 1 - TARGET_FALLBACK_RATIO), "情景参考位"


def _spread_score(spread_ratio: float) -> float:
    if spread_ratio <= 0.02:
        return 1.0
    if spread_ratio >= MAX_QUOTE_SPREAD_RATIO:
        return 0.0
    return max(0.0, 1 - (spread_ratio - 0.02) / 0.08)


def _liquidity_label(spread_ratio: float, activity: float) -> str:
    if spread_ratio <= 0.02 and activity >= 0.6:
        return "良好"
    if spread_ratio <= GOOD_QUOTE_SPREAD_RATIO and activity >= 0.35:
        return "可接受"
    return "偏弱"


def _activity_score(row: dict[str, Any], maximum: float) -> float:
    activity = max(_number(row.get("volume")) or 0, 0) + max(_number(row.get("open_interest")) or 0, 0)
    if maximum <= 0:
        return 0.0
    return min(1.0, math.log1p(activity) / math.log1p(maximum))


def _prepare_metrics(
    rows: list[dict[str, Any]],
    spot: float,
    now: datetime | None = None,
    iv_model: dict[str, dict[str, Any]] | None = None,
) -> dict[str, list[dict[str, Any]]]:
    # levels.py 已经为同一快照完成过 IV/Gamma 标注时直接复用，避免首屏重复反解隐含波动率。
    if iv_model is None:
        iv_model = annotate_model_greeks(rows, spot, now)
    grouped: dict[str, list[dict[str, Any]]] = {}
    for row in rows:
        expiration = str(row.get("expiration") or "")
        strike = _number(row.get("strike"))
        quote = _mid_quote(row)
        if not expiration or strike is None or strike <= 0 or quote is None:
            continue
        dte = _dte(expiration, now)
        years = years_to_expiry(expiration, now)
        volatility = _number(row.get("model_iv")) or _number((iv_model.get(expiration) or {}).get("iv"))
        if dte is None or years is None or volatility is None or volatility <= 0:
            continue
        greeks = _greeks(spot, strike, volatility, years, str(row.get("contract_type") or ""))
        if greeks is None:
            continue
        mid, spread_ratio = quote
        row_copy = dict(row)
        row_copy.update({
            "mid": mid,
            "spread_ratio": spread_ratio,
            "dte": dte,
            "years": years,
            "iv": volatility,
            **greeks,
        })
        grouped.setdefault(expiration, []).append(row_copy)
    for group in grouped.values():
        maximum = max(
            (max(_number(row.get("volume")) or 0, 0) + max(_number(row.get("open_interest")) or 0, 0) for row in group),
            default=0,
        )
        for row in group:
            row["activity_score"] = _activity_score(row, maximum)
    return grouped


def _scenario_value(row: dict[str, Any], target: float, horizon_days: int, is_call: bool) -> float:
    remaining = max((_number(row.get("years")) or 0) - horizon_days / 252, 1 / 365)
    value = black_scholes_price(target, row["strike"], row["iv"], remaining, is_call)
    return max(value, 0.0)


def _scenario_return(row: dict[str, Any], target: float, horizon_days: int, is_call: bool) -> float | None:
    """计算目标价在分析窗口内触及时，单腿相对当前中间价的模型收益。"""
    if row["mid"] <= 0:
        return None
    return (_scenario_value(row, target, horizon_days, is_call) - row["mid"]) / row["mid"]


def _scenario_priority(value: float | None) -> tuple[int, float]:
    """正收益优先；同为负收益时优先选择亏损幅度较小的方案。"""
    if value is None or not math.isfinite(value):
        return 0, -math.inf
    return (1 if value >= 0 else 0), value


def _single_quality(row: dict[str, Any], target: float, spot: float) -> float:
    delta_quality = max(0.0, 1 - abs(abs(row["delta"]) - 0.58) / 0.58)
    theta_ratio = abs(row["theta_per_day"]) / max(row["mid"], 0.01)
    theta_quality = max(0.0, 1 - min(theta_ratio / 0.08, 1))
    distance = abs(row["strike"] / spot - target / spot)
    target_quality = max(0.0, 1 - min(distance / 0.12, 1))
    return min(1.0, max(0.0, (
        _spread_score(row["spread_ratio"]) * 0.30
        + row["activity_score"] * 0.20
        + delta_quality * 0.20
        + theta_quality * 0.15
        + target_quality * 0.15
    )))


def _single_structure(row: dict[str, Any], style: str, direction: str, target: float, target_source: str, spot: float, horizon_days: int) -> dict[str, Any]:
    is_call = direction == "call"
    cost = row["mid"] * 100
    scenario_return = _scenario_return(row, target, horizon_days, is_call)
    breakeven = row["strike"] + row["mid"] if is_call else row["strike"] - row["mid"]
    exposure = spot * 100 * abs(row["delta"])
    leverage = exposure / cost if cost > 0 else 0
    quality = _single_quality(row, target, spot)
    return {
        "kind": "single",
        "style": style,
        "direction": direction,
        "label": f"买入{'看涨' if is_call else '看跌'}",
        "expiration": row["expiration"],
        "dte": row["dte"],
        "strikes": [round(row["strike"], 4)],
        "legs": [{"action": "buy", "contract_type": direction, "strike": round(row["strike"], 4)}],
        "iv": row["iv"],
        "delta": row["delta"],
        "delta_exposure": exposure,
        "effective_leverage": leverage,
        "max_loss": cost,
        "cost": cost,
        "breakeven": breakeven,
        "target_price": target,
        "target_source": target_source,
        "scenario_return": scenario_return,
        "scenario_profitable": scenario_return is not None and scenario_return >= 0,
        "spread_ratio": row["spread_ratio"],
        "liquidity": _liquidity_label(row["spread_ratio"], row["activity_score"]),
        "score": quality,
        "advantage": "伽马弹性较高，适合预期行情较快兑现" if style == "激进单腿" else "到期时间更长，时间衰减压力相对较小",
        "risk": "时间衰减较快" if style == "激进单腿" else "权利金占用较高",
        "model_iv_source": "统一模型估算",
        "quote_method": "买卖价中间价",
    }


def _preferred_long(group: list[dict[str, Any]], direction: str, spot: float) -> dict[str, Any] | None:
    side = [row for row in group if row.get("contract_type") == direction]
    if not side:
        return None
    is_call = direction == "call"
    preferred = spot * (0.98 if is_call else 1.02)
    in_band = [row for row in side if spot * (0.85 if is_call else 1.0) <= row["strike"] <= spot * (1.02 if is_call else 1.15)]
    return min(in_band or side, key=lambda row: abs(row["strike"] - preferred))


def _vertical_structure(
    group: list[dict[str, Any]],
    direction: str,
    target: float,
    target_source: str,
    spot: float,
    horizon_days: int,
    style: str = "目标位价差",
) -> dict[str, Any] | None:
    long = _preferred_long(group, direction, spot)
    if long is None:
        return None
    is_call = direction == "call"
    short_candidates = [
        row for row in group
        if row.get("contract_type") == direction
        and ((row["strike"] > long["strike"]) if is_call else (row["strike"] < long["strike"]))
    ]
    if not short_candidates:
        return None
    short = min(short_candidates, key=lambda row: abs(row["strike"] - target))
    debit = long["mid"] - short["mid"]
    width = abs(short["strike"] - long["strike"])
    if debit <= 0 or debit >= width or long["spread_ratio"] >= MAX_QUOTE_SPREAD_RATIO or short["spread_ratio"] >= MAX_QUOTE_SPREAD_RATIO:
        return None
    net_delta = long["delta"] - short["delta"]
    net_theta = long["theta_per_day"] - short["theta_per_day"]
    net_vega = long["vega"] - short["vega"]
    max_loss = debit * 100
    max_profit = max(0.0, width - debit) * 100
    scenario = max(_scenario_value(long, target, horizon_days, is_call) - _scenario_value(short, target, horizon_days, is_call), 0.0)
    scenario_return = (scenario - debit) / debit if debit > 0 else None
    exposure = spot * 100 * abs(net_delta)
    leverage = exposure / max_loss if max_loss > 0 else 0
    target_quality = max(0.0, 1 - min(abs(short["strike"] - target) / max(spot * 0.12, 0.01), 1))
    activity = min(long["activity_score"], short["activity_score"])
    spread_quality = (_spread_score(long["spread_ratio"]) + _spread_score(short["spread_ratio"])) / 2
    quality = min(1.0, max(0.0, spread_quality * 0.30 + activity * 0.20 + target_quality * 0.25 + min(abs(net_delta) / 0.55, 1) * 0.15 + min(max_profit / max_loss, 1) * 0.10))
    return {
        "kind": "vertical",
        "style": style,
        "direction": direction,
        "label": "看涨价差" if is_call else "看跌价差",
        "expiration": long["expiration"],
        "dte": long["dte"],
        "strikes": [round(long["strike"], 4), round(short["strike"], 4)],
        "legs": [
            {"action": "buy", "contract_type": direction, "strike": round(long["strike"], 4)},
            {"action": "sell", "contract_type": direction, "strike": round(short["strike"], 4)},
        ],
        "iv": (long["iv"] + short["iv"]) / 2,
        "delta": net_delta,
        "delta_exposure": exposure,
        "effective_leverage": leverage,
        "max_loss": max_loss,
        "max_profit": max_profit,
        "cost": max_loss,
        "breakeven": long["strike"] + debit if is_call else long["strike"] - debit,
        "target_price": target,
        "target_source": target_source,
        "scenario_return": scenario_return,
        "scenario_profitable": scenario_return is not None and scenario_return >= 0,
        "spread_ratio": max(long["spread_ratio"], short["spread_ratio"]),
        "liquidity": _liquidity_label(max(long["spread_ratio"], short["spread_ratio"]), activity),
        "score": quality,
        "advantage": "空头腿靠近目标位，降低权利金和波动率敏感度",
        "risk": "收益封顶，且两条腿都需要有可成交报价",
        "model_iv_source": "统一模型估算",
        "quote_method": "双腿买卖价中间价",
        "net_theta_per_day": net_theta,
        "net_vega": net_vega,
    }


def _empty(reason: str) -> dict[str, Any]:
    return {
        "available": False,
        "status": "unavailable",
        "direction": None,
        "horizon_trading_days": HORIZON_TRADING_DAYS,
        "horizon_label": "未来 5 个交易日",
        "items": [],
        "reason": reason,
        "method": "布莱克-斯科尔斯模型估算",
        "quote_method": "仅使用有效买卖价中间价",
    }


def build_buyer_structures(
    chain_rows: Iterable[dict[str, Any]] | None,
    spot: Any,
    trend: dict[str, Any] | None,
    recommendation: dict[str, Any] | None,
    support: Iterable[dict[str, Any]],
    resistance: Iterable[dict[str, Any]],
    horizon_trading_days: int = HORIZON_TRADING_DAYS,
    now: datetime | None = None,
    iv_model: dict[str, dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """同时比较主方向与反向方向，各最多给出五条买方候选结构。"""
    price = _number(spot)
    if price is None or price <= 0:
        return _empty("缺少有效现价")
    action = str((recommendation or {}).get("action") or "hold")
    if action not in {"buy", "sell"}:
        return _empty("当前方向信号不足，不强行推荐单方向期权")
    primary_direction = "call" if action == "buy" else "put"
    comparison_direction = "put" if primary_direction == "call" else "call"
    rows = [dict(row) for row in (chain_rows or [])]
    groups = _prepare_metrics(rows, price, now, iv_model)
    eligible = [(expiration, group) for expiration, group in groups.items() if any(row["dte"] >= MIN_CANDIDATE_DTE for row in group)]
    if not eligible:
        return _empty("当前缓存缺少有效买卖报价，暂不生成买方结构")
    eligible.sort(key=lambda item: (min(row["dte"] for row in item[1]), item[0]))
    def build_direction_candidates(direction: str) -> list[dict[str, Any]]:
        """为单个期权方向生成短线、价差、时间容错和备用候选。"""
        target, target_source = _target_price(direction, price, support, resistance, trend)
        candidates: list[dict[str, Any]] = []
        used_single_contracts: set[tuple[str, float]] = set()
        used_verticals: set[tuple[str, tuple[float, ...]]] = set()

        def add_single(style: str, preferred_group: list[dict[str, Any]] | None, preferred_strike: float, predicate) -> None:
            pool = [
                row for row in (preferred_group or [])
                if row.get("contract_type") == direction
                and row["dte"] >= MIN_CANDIDATE_DTE
                and row["spread_ratio"] < MAX_QUOTE_SPREAD_RATIO
                and predicate(row)
            ]
            pool = [row for row in pool if (row["expiration"], row["strike"]) not in used_single_contracts]
            if not pool:
                return
            chosen = max(
                pool,
                key=lambda row: (
                    _single_quality(row, target, price)
                    + max(0.0, 1 - abs(row["strike"] - preferred_strike) / max(price * 0.08, 0.01)) * 0.45
                    - abs(row["dte"] - (7 if "短线" in style else TIME_TOLERANCE_DTE)) / 300
                ),
            )
            used_single_contracts.add((chosen["expiration"], chosen["strike"]))
            candidates.append(_single_structure(chosen, style, direction, target, target_source, price, horizon_trading_days))

        def add_vertical(group: list[dict[str, Any]], style: str) -> None:
            structure = _vertical_structure(group, direction, target, target_source, price, horizon_trading_days, style)
            if not structure:
                return
            key = (structure["expiration"], tuple(structure["strikes"]))
            if key in used_verticals:
                return
            used_verticals.add(key)
            candidates.append(structure)

        short_groups = [group for _, group in eligible if MIN_CANDIDATE_DTE <= min(row["dte"] for row in group) <= PREFERRED_SHORT_DTE]
        short_group = short_groups[0] if short_groups else eligible[0][1]
        near_itm = price * (0.98 if direction == "call" else 1.02)
        add_single("短线价内单腿", short_group, near_itm, lambda row: row["strike"] <= price if direction == "call" else row["strike"] >= price)

        spread_groups = [group for _, group in eligible if min(row["dte"] for row in group) >= MIN_CANDIDATE_DTE]
        if spread_groups:
            add_vertical(spread_groups[0], "近期目标位价差")
            if len(spread_groups) > 1:
                add_vertical(spread_groups[-1], "远期目标位价差")

        long_groups = [group for _, group in eligible if max(row["dte"] for row in group) >= TIME_TOLERANCE_DTE]
        long_group = long_groups[-1] if long_groups else eligible[-1][1]
        add_single("时间容错单腿", long_group, near_itm, lambda row: row["strike"] <= price * 1.08 if direction == "call" else row["strike"] >= price * 0.92)
        # 在已有候选合约之外再选一张备用单腿，保留不同执行价的替代方案。
        add_single("备用价内单腿", long_group, price, lambda row: row["strike"] <= price * 1.08 if direction == "call" else row["strike"] >= price * 0.92)
        candidates.sort(
            key=lambda item: (
                _scenario_priority(item.get("scenario_return")),
                item.get("score", 0),
            ),
            reverse=True,
        )
        return candidates[:DIRECTION_STRUCTURE_LIMIT]

    primary_candidates = build_direction_candidates(primary_direction)
    comparison_candidates = build_direction_candidates(comparison_direction)
    unique = primary_candidates + comparison_candidates
    if not unique:
        return _empty("当前期权链没有满足报价和流动性条件的候选结构")
    primary_target, primary_target_source = _target_price(primary_direction, price, support, resistance, trend)
    comparison_target, comparison_target_source = _target_price(comparison_direction, price, support, resistance, trend)
    for item in unique:
        item["is_primary"] = item["direction"] == primary_direction
        item["direction_label"] = "看涨方案" if item["direction"] == "call" else "看跌方案"
    return {
        "available": True,
        "status": "ok",
        "direction": primary_direction,
        "primary_direction": primary_direction,
        "direction_label": "主方向：买入看涨 · 同时对比买入看跌" if primary_direction == "call" else "主方向：买入看跌 · 同时对比买入看涨",
        "recommendation": (recommendation or {}).get("reason") or "结合趋势与支撑/压力位",
        "horizon_trading_days": horizon_trading_days,
        "horizon_label": f"未来 {horizon_trading_days} 个交易日",
        "targets": {
            "call": {"price": primary_target if primary_direction == "call" else comparison_target, "source": primary_target_source if primary_direction == "call" else comparison_target_source},
            "put": {"price": primary_target if primary_direction == "put" else comparison_target, "source": primary_target_source if primary_direction == "put" else comparison_target_source},
        },
        "items": unique[:STRUCTURE_LIMIT],
        "method": "布莱克-斯科尔斯模型估算",
        "quote_method": "仅使用有效买卖价中间价，不使用最新成交价代替成本",
        "disclaimer": "综合评分不是历史胜率；情景收益是假设目标价在窗口内触及时的模型估算",
    }
