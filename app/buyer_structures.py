"""期权买方结构分析。

只使用当前已缓存的期权链和项目统一的 Black-Scholes 模型，给出少量买方候选。
成本展示仍用买卖价中间价。排序则按「触及目标的盈亏 × 触及概率 + 未触及时的时间损耗 × 剩余概率」
并先扣掉半档价差。综合评分只描述报价和结构质量，不是历史胜率，也不单独决定名次。
"""

from __future__ import annotations

import math
from datetime import datetime, timedelta, timezone
from typing import Any, Iterable
from zoneinfo import ZoneInfo

from app.gamma import (
    RISK_FREE,
    annotate_model_greeks,
    black_scholes_price,
    implied_volatility_from_price,
    norm_cdf,
    option_expiry,
)

MARKET_TIMEZONE = ZoneInfo("America/New_York")
HORIZON_TRADING_DAYS = 5
# 5 个交易日大约跨过 7 个自然日。短于这个窗口的合约直接排除，不再把剩余期限托底成 1 天。
SWEET_MIN_DTE = 10
SWEET_MAX_DTE = 45
MAX_QUOTE_SPREAD_RATIO = 0.10
GOOD_QUOTE_SPREAD_RATIO = 0.05
TARGET_FALLBACK_RATIO = 0.05
# 5 个交易日内几乎碰不到的价位不拿来做情景，除非它是唯一可用的目标。
MIN_TARGET_TOUCH = 0.15
MAX_VERTICAL_WIDTH_RATIO = 0.15
DIRECTION_STRUCTURE_LIMIT = 5
STRUCTURE_LIMIT = DIRECTION_STRUCTURE_LIMIT * 2
YEAR_SECONDS = 365 * 24 * 3600
# 低于内在价值 2% 的报价视为失真，避免用不可成交的价格反解波动率。
INTRINSIC_SLACK = 0.98


def _number(value: Any) -> float | None:
    try:
        result = float(value)
    except (TypeError, ValueError):
        return None
    return result if math.isfinite(result) else None


def _as_now(now: datetime | None) -> datetime:
    current = now or datetime.now(timezone.utc)
    if current.tzinfo is None:
        current = current.replace(tzinfo=timezone.utc)
    return current.astimezone(timezone.utc)


def _horizon_end(now: datetime, trading_days: int) -> datetime:
    """从当前时刻起跳过周末，得到未来 N 个交易日的同一钟点。"""
    cursor = now.astimezone(MARKET_TIMEZONE)
    remaining = max(int(trading_days), 1)
    while remaining > 0:
        cursor += timedelta(days=1)
        if cursor.weekday() < 5:
            remaining -= 1
    return cursor


def _years_between(start: datetime, end: datetime) -> float:
    return (end - start).total_seconds() / YEAR_SECONDS


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


def _dte(expiration: str, now: datetime) -> int | None:
    expiry = option_expiry(expiration)
    if expiry is None:
        return None
    return max((expiry.astimezone(MARKET_TIMEZONE).date() - now.astimezone(MARKET_TIMEZONE).date()).days, 0)


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
        theta = (
            -spot * _normal_pdf(d1) * volatility / (2 * math.sqrt(years))
            - RISK_FREE * strike * math.exp(-RISK_FREE * years) * norm_cdf(d2)
        )
    else:
        delta = norm_cdf(d1) - 1
        theta = (
            -spot * _normal_pdf(d1) * volatility / (2 * math.sqrt(years))
            + RISK_FREE * strike * math.exp(-RISK_FREE * years) * norm_cdf(-d2)
        )
    vega = spot * _normal_pdf(d1) * math.sqrt(years)
    return {"delta": delta, "theta_per_day": theta / 365, "vega": vega}


def _touch_probability(price: float, spot: float, volatility: float | None, years: float | None) -> float | None:
    """与价位模块相同的零漂移首次触及概率，保留完整精度供排序。"""
    if spot <= 0 or price <= 0 or not volatility or not years or volatility <= 0 or years <= 0:
        return None
    sigma_time = volatility * math.sqrt(years)
    if sigma_time <= 0:
        return None
    distance = abs(math.log(price / spot))
    return min(1.0, 2.0 * norm_cdf(-distance / sigma_time))


def _level_strength(row: dict[str, Any]) -> float | None:
    for key in ("score", "model_score"):
        value = _number(row.get(key))
        if value is not None:
            return min(1.0, max(0.0, value))
    return None


def _target_price(
    direction: str,
    spot: float,
    support: Iterable[dict[str, Any]],
    resistance: Iterable[dict[str, Any]],
    trend: dict[str, Any] | None,
    volatility: float | None,
    horizon_years: float,
) -> tuple[float, str, float | None]:
    """在正确的一侧选择目标价：强度 × 窗口内触及概率，而不是离现价最近的一档。"""
    rows = resistance if direction == "call" else support
    levels: list[dict[str, Any]] = []
    for row in rows:
        value = _number(row.get("price"))
        if value is None or value <= 0:
            continue
        if direction == "call" and value <= spot:
            continue
        if direction == "put" and value >= spot:
            continue
        touch = _touch_probability(value, spot, volatility, horizon_years)
        strength = _level_strength(row)
        weight = 0.5 if strength is None else strength
        utility = weight * (touch if touch is not None else 0.5)
        levels.append({"price": value, "touch": touch, "utility": utility})
    if levels:
        reachable = [item for item in levels if item["touch"] is not None and item["touch"] >= MIN_TARGET_TOUCH]
        pool = reachable or levels
        chosen = max(
            pool,
            key=lambda item: (
                item["utility"],
                item["touch"] if item["touch"] is not None else -1.0,
                -abs(item["price"] - spot),
            ),
        )
        source = "综合压力位" if direction == "call" else "综合支撑位"
        return chosen["price"], source, chosen["touch"]
    channel_value = _number((trend or {}).get("upper" if direction == "call" else "lower"))
    if channel_value is not None and (
        (direction == "call" and channel_value > spot) or (direction == "put" and channel_value < spot)
    ):
        return channel_value, "趋势通道", _touch_probability(channel_value, spot, volatility, horizon_years)
    fallback = spot * (1 + TARGET_FALLBACK_RATIO if direction == "call" else 1 - TARGET_FALLBACK_RATIO)
    return fallback, "情景参考位", _touch_probability(fallback, spot, volatility, horizon_years)


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


def _fallback_iv(row: dict[str, Any], iv_model: dict[str, dict[str, Any]]) -> float | None:
    value = _number(row.get("model_iv"))
    if value is None or value <= 0:
        value = _number((iv_model.get(str(row.get("expiration") or "")) or {}).get("iv"))
    if value is None or value <= 0:
        return None
    return value


def _below_intrinsic(mid: float, spot: float, strike: float, is_call: bool) -> bool:
    intrinsic = max(spot - strike, 0.0) if is_call else max(strike - spot, 0.0)
    return mid < intrinsic * INTRINSIC_SLACK


def _reference_iv(groups: list[list[dict[str, Any]]], spot: float) -> float | None:
    """用最近到期日、最靠近现价且双边都有报价的执行价代表短线波动。

    只对所有近值合约取中位数时，一边报价偏便宜会把波动率拉低，远处的目标会被误判成碰不到。
    """
    ordered = sorted(groups, key=lambda group: (group[0]["dte"], group[0]["expiration"]))
    for group in ordered:
        by_strike: dict[float, list[float]] = {}
        for row in group:
            if row.get("iv_source") != "contract" or row["strike"] <= 0:
                continue
            if abs(math.log(row["strike"] / spot)) > 0.03:
                continue
            by_strike.setdefault(row["strike"], []).append(row["iv"])
        if not by_strike:
            continue
        paired = {strike: values for strike, values in by_strike.items() if len(values) >= 2}
        pool = paired or by_strike
        strike = min(pool, key=lambda value: abs(value - spot))
        values = pool[strike]
        return sum(values) / len(values)
    samples = [row["iv"] for group in ordered for row in group if _number(row.get("iv"))]
    if not samples:
        return None
    samples.sort()
    return samples[len(samples) // 2]


def _future_mark(row: dict[str, Any], spot: float, price: float, horizon_years: float, is_call: bool) -> float:
    """用这条腿自己的波动率做情景价，再平移到当前中间价，避免模型价和市价的落差混进盈亏。"""
    strike = row["strike"]
    remaining = row["years"] - horizon_years
    if remaining <= 1 / (365 * 24 * 60):
        return max(price - strike, 0.0) if is_call else max(strike - price, 0.0)
    future = black_scholes_price(price, strike, row["iv"], remaining, is_call)
    today = black_scholes_price(spot, strike, row["iv"], row["years"], is_call)
    return max(0.0, row["mid"] + future - today)


def _half_spread(row: dict[str, Any]) -> float:
    return row["mid"] * row["spread_ratio"] / 2


def _expected_return(target_return: float, unchanged_return: float, touch: float | None) -> float:
    probability = touch if touch is not None else 0.5
    return probability * target_return + (1.0 - probability) * unchanged_return


def _iv_source_label(*rows: dict[str, Any]) -> str:
    if rows and all(row.get("iv_source") == "contract" for row in rows):
        return "合约中间价反解"
    return "到期日模型估算"


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


def _single_style(row: dict[str, Any], direction: str, spot: float) -> tuple[str, str, str]:
    strike = row["strike"]
    if abs(strike / spot - 1) <= 0.01:
        moneyness = "平值"
    elif (direction == "call" and strike < spot) or (direction == "put" and strike > spot):
        moneyness = "价内"
    else:
        moneyness = "价外"
    if row["dte"] <= 21:
        return (
            f"短线{moneyness}单腿",
            "期限覆盖分析窗口，方向弹性更高",
            "越接近到期，时间价值损耗越快",
        )
    return (
        f"时间容错{moneyness}单腿",
        "到期更远，时间衰减压力更小",
        "权利金更高，同样行情下收益率更低",
    )


def _single_structure(
    row: dict[str, Any],
    direction: str,
    target: float,
    target_source: str,
    spot: float,
    horizon_years: float,
    touch: float | None,
) -> dict[str, Any] | None:
    is_call = direction == "call"
    cost = row["mid"] * 100
    if row["mid"] <= 0 or cost <= 0:
        return None
    target_exit = _future_mark(row, spot, target, horizon_years, is_call)
    unchanged_exit = _future_mark(row, spot, spot, horizon_years, is_call)
    half = _half_spread(row)
    scenario_return = (target_exit - row["mid"]) / row["mid"]
    expected = _expected_return(
        (target_exit - row["mid"] - half) / row["mid"],
        (unchanged_exit - row["mid"] - half) / row["mid"],
        touch,
    )
    if not math.isfinite(scenario_return) or not math.isfinite(expected):
        return None
    style, advantage, risk = _single_style(row, direction, spot)
    exposure = spot * 100 * abs(row["delta"])
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
        "effective_leverage": exposure / cost if cost > 0 else 0,
        "max_loss": cost,
        "cost": cost,
        "breakeven": row["strike"] + row["mid"] if is_call else row["strike"] - row["mid"],
        "target_price": target,
        "target_source": target_source,
        "scenario_return": scenario_return,
        "scenario_profitable": scenario_return >= 0,
        "touch_probability": None if touch is None else round(touch, 4),
        "expected_return": expected,
        "spread_ratio": row["spread_ratio"],
        "liquidity": _liquidity_label(row["spread_ratio"], row["activity_score"]),
        "score": _single_quality(row, target, spot),
        "advantage": advantage,
        "risk": risk,
        "model_iv_source": _iv_source_label(row),
        "quote_method": "买卖价中间价",
    }


def _long_rows(group: list[dict[str, Any]], direction: str, spot: float) -> list[dict[str, Any]]:
    is_call = direction == "call"
    low, high = (spot * 0.90, spot * 1.08) if is_call else (spot * 0.92, spot * 1.10)
    rows = [row for row in group if row["contract_type"] == direction and low <= row["strike"] <= high]
    if rows:
        return rows
    side = [row for row in group if row["contract_type"] == direction]
    if not side:
        return []
    nearest = min(side, key=lambda row: abs(row["strike"] - spot))
    if abs(nearest["strike"] - spot) <= spot * 0.15:
        return [nearest]
    return []


def _short_rows(
    group: list[dict[str, Any]],
    long: dict[str, Any],
    direction: str,
    target: float,
    spot: float,
) -> list[dict[str, Any]]:
    is_call = direction == "call"
    further = [
        row for row in group
        if row["contract_type"] == direction
        and (row["strike"] > long["strike"] if is_call else row["strike"] < long["strike"])
    ]
    within = [row for row in further if abs(row["strike"] - long["strike"]) <= spot * MAX_VERTICAL_WIDTH_RATIO]
    if within:
        return within
    if not further:
        return []
    nearest = min(further, key=lambda row: abs(row["strike"] - target))
    if abs(nearest["strike"] - long["strike"]) <= spot * 0.25:
        return [nearest]
    return []


def _vertical_structure(
    long: dict[str, Any],
    short: dict[str, Any],
    direction: str,
    target: float,
    target_source: str,
    spot: float,
    horizon_years: float,
    touch: float | None,
) -> dict[str, Any] | None:
    is_call = direction == "call"
    debit = long["mid"] - short["mid"]
    width = abs(short["strike"] - long["strike"])
    half = _half_spread(long) + _half_spread(short)
    if debit <= half or debit >= width or width <= 0:
        return None
    target_exit = min(
        max(
            _future_mark(long, spot, target, horizon_years, is_call)
            - _future_mark(short, spot, target, horizon_years, is_call),
            0.0,
        ),
        width,
    )
    unchanged_exit = min(
        max(
            _future_mark(long, spot, spot, horizon_years, is_call)
            - _future_mark(short, spot, spot, horizon_years, is_call),
            0.0,
        ),
        width,
    )
    scenario_return = (target_exit - debit) / debit
    expected = _expected_return(
        (target_exit - debit - half) / debit,
        (unchanged_exit - debit - half) / debit,
        touch,
    )
    if not math.isfinite(scenario_return) or not math.isfinite(expected):
        return None
    net_delta = long["delta"] - short["delta"]
    max_loss = debit * 100
    max_profit = max(0.0, width - debit) * 100
    exposure = spot * 100 * abs(net_delta)
    activity = min(long["activity_score"], short["activity_score"])
    spread_quality = (_spread_score(long["spread_ratio"]) + _spread_score(short["spread_ratio"])) / 2
    target_quality = max(0.0, 1 - min(abs(short["strike"] - target) / max(spot * 0.12, 0.01), 1))
    quality = min(1.0, max(0.0, (
        spread_quality * 0.30
        + activity * 0.20
        + target_quality * 0.25
        + min(abs(net_delta) / 0.55, 1) * 0.15
        + min(max_profit / max_loss, 1) * 0.10
    )))
    short_dte = long["dte"] <= 21
    return {
        "kind": "vertical",
        "style": "近期目标位价差" if short_dte else "远期目标位价差",
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
        "effective_leverage": exposure / max_loss if max_loss > 0 else 0,
        "max_loss": max_loss,
        "max_profit": max_profit,
        "cost": max_loss,
        "breakeven": long["strike"] + debit if is_call else long["strike"] - debit,
        "target_price": target,
        "target_source": target_source,
        "scenario_return": scenario_return,
        "scenario_profitable": scenario_return >= 0,
        "touch_probability": None if touch is None else round(touch, 4),
        "expected_return": expected,
        "spread_ratio": max(long["spread_ratio"], short["spread_ratio"]),
        "liquidity": _liquidity_label(max(long["spread_ratio"], short["spread_ratio"]), activity),
        "score": quality,
        "advantage": "卖出腿压低权利金，并降低波动率暴露",
        "risk": "收益封顶，两条腿都要有可成交报价",
        "model_iv_source": _iv_source_label(long, short),
        "quote_method": "双腿买卖价中间价",
        "net_theta_per_day": long["theta_per_day"] - short["theta_per_day"],
        "net_vega": long["vega"] - short["vega"],
    }


def _rank_key(item: dict[str, Any]) -> tuple[float, float]:
    expected = item.get("expected_return")
    if expected is None or not math.isfinite(expected):
        expected = -math.inf
    return expected, item.get("score") or 0.0


def _too_similar(left: dict[str, Any], right: dict[str, Any], spot: float) -> bool:
    if left["kind"] != right["kind"] or left["expiration"] != right["expiration"]:
        return False
    if left["strikes"] == right["strikes"]:
        return True
    tolerance = max(spot * 0.015, 0.01)
    if abs(left["strikes"][0] - right["strikes"][0]) > tolerance:
        return False
    if left["kind"] == "single":
        return True
    return abs(left["strikes"][-1] - right["strikes"][-1]) <= tolerance


def _select_diverse(ranked: list[dict[str, Any]], limit: int, spot: float) -> list[dict[str, Any]]:
    """先按期望收益取不同到期和执行价，再保证单腿和价差至少各留一条。"""
    selected: list[dict[str, Any]] = []
    seen: set[int] = set()

    def add(item: dict[str, Any]) -> bool:
        marker = id(item)
        if marker in seen:
            return False
        seen.add(marker)
        selected.append(item)
        return True

    for item in ranked:
        if len(selected) >= limit:
            break
        if any(_too_similar(item, kept, spot) for kept in selected):
            continue
        add(item)

    def ensure(kind: str) -> None:
        if any(item["kind"] == kind for item in selected):
            return
        candidate = next((item for item in ranked if item["kind"] == kind), None)
        if candidate is None:
            return
        if len(selected) < limit:
            add(candidate)
            return
        replaceable = [item for item in selected if item["kind"] != kind]
        if not replaceable:
            return
        worst = min(replaceable, key=_rank_key)
        selected.remove(worst)
        seen.discard(id(worst))
        add(candidate)

    ensure("single")
    ensure("vertical")
    if len(selected) < limit:
        for item in ranked:
            if len(selected) >= limit:
                break
            add(item)
    selected.sort(key=_rank_key, reverse=True)
    return selected[:limit]


def _empty(reason: str, horizon_trading_days: int = HORIZON_TRADING_DAYS) -> dict[str, Any]:
    return {
        "available": False,
        "status": "unavailable",
        "direction": None,
        "horizon_trading_days": horizon_trading_days,
        "horizon_label": f"未来 {horizon_trading_days} 个交易日",
        "items": [],
        "reason": reason,
        "method": "布莱克-斯科尔斯模型估算",
        "quote_method": "仅使用有效买卖价中间价",
    }


def _quoted_contracts(
    rows: Iterable[dict[str, Any]],
    spot: float,
    now: datetime,
) -> list[dict[str, Any]]:
    quoted: dict[tuple[str, str, float], dict[str, Any]] = {}
    for source in rows:
        expiration = str(source.get("expiration") or "")
        contract_type = str(source.get("contract_type") or "")
        strike = _number(source.get("strike"))
        quote = _mid_quote(source)
        expiry = option_expiry(expiration)
        if contract_type not in {"call", "put"} or not expiration or strike is None or strike <= 0 or quote is None or expiry is None:
            continue
        years = _years_between(now, expiry)
        dte = _dte(expiration, now)
        if dte is None or years <= 0:
            continue
        mid, spread_ratio = quote
        key = (expiration, contract_type, strike)
        current = quoted.get(key)
        if current is not None and current["spread_ratio"] <= spread_ratio:
            continue
        quoted[key] = {
            "expiration": expiration,
            "contract_type": contract_type,
            "strike": strike,
            "mid": mid,
            "spread_ratio": spread_ratio,
            "dte": dte,
            "years": years,
            "expiry": expiry,
            "volume": source.get("volume"),
            "open_interest": source.get("open_interest"),
            "model_iv": source.get("model_iv"),
            "below_intrinsic": _below_intrinsic(mid, spot, strike, contract_type == "call"),
        }
    return list(quoted.values())


def _enrich(
    rows: list[dict[str, Any]],
    spot: float,
    iv_model: dict[str, dict[str, Any]],
) -> list[dict[str, Any]]:
    enriched: list[dict[str, Any]] = []
    for row in rows:
        if row["spread_ratio"] >= MAX_QUOTE_SPREAD_RATIO or row["below_intrinsic"]:
            continue
        is_call = row["contract_type"] == "call"
        solved = implied_volatility_from_price(row["mid"], spot, row["strike"], row["years"], is_call)
        if solved is not None and solved > 0:
            volatility, source = solved, "contract"
        else:
            volatility = _fallback_iv(row, iv_model)
            source = "expiration"
        if volatility is None or volatility <= 0:
            continue
        greeks = _greeks(spot, row["strike"], volatility, row["years"], row["contract_type"])
        if greeks is None:
            continue
        enriched.append({**row, "iv": volatility, "iv_source": source, **greeks})
    grouped: dict[str, list[dict[str, Any]]] = {}
    for row in enriched:
        grouped.setdefault(row["expiration"], []).append(row)
    for group in grouped.values():
        maximum = max(
            (max(_number(row.get("volume")) or 0, 0) + max(_number(row.get("open_interest")) or 0, 0) for row in group),
            default=0,
        )
        for row in group:
            row["activity_score"] = _activity_score(row, maximum)
    return enriched


def _search_groups(rows: list[dict[str, Any]]) -> list[list[dict[str, Any]]]:
    grouped: dict[str, list[dict[str, Any]]] = {}
    for row in rows:
        grouped.setdefault(row["expiration"], []).append(row)
    groups = list(grouped.values())
    sweet = [group for group in groups if SWEET_MIN_DTE <= group[0]["dte"] <= SWEET_MAX_DTE]
    chosen = sweet or groups
    chosen.sort(key=lambda group: (group[0]["dte"], group[0]["expiration"]))
    return chosen


def _direction_candidates(
    groups: list[list[dict[str, Any]]],
    direction: str,
    target: float,
    target_source: str,
    spot: float,
    horizon_years: float,
    touch: float | None,
) -> list[dict[str, Any]]:
    candidates: list[dict[str, Any]] = []
    seen_singles: set[tuple[str, float]] = set()
    seen_verticals: set[tuple[str, float, float]] = set()
    for group in groups:
        for row in _long_rows(group, direction, spot):
            key = (row["expiration"], row["strike"])
            if key in seen_singles:
                continue
            structure = _single_structure(row, direction, target, target_source, spot, horizon_years, touch)
            if structure is None:
                continue
            seen_singles.add(key)
            candidates.append(structure)
        for long in _long_rows(group, direction, spot):
            for short in _short_rows(group, long, direction, target, spot):
                key = (long["expiration"], long["strike"], short["strike"])
                if key in seen_verticals:
                    continue
                structure = _vertical_structure(long, short, direction, target, target_source, spot, horizon_years, touch)
                if structure is None:
                    continue
                seen_verticals.add(key)
                candidates.append(structure)
    candidates.sort(key=_rank_key, reverse=True)
    return _select_diverse(candidates, DIRECTION_STRUCTURE_LIMIT, spot)


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
    horizon_days = max(int(horizon_trading_days or HORIZON_TRADING_DAYS), 1)
    price = _number(spot)
    if price is None or price <= 0:
        return _empty("缺少有效现价", horizon_days)
    current = _as_now(now)
    horizon_end = _horizon_end(current, horizon_days)
    horizon_years = max(_years_between(current, horizon_end), 0.0)
    rows = [dict(row) for row in (chain_rows or [])]
    if iv_model is None:
        iv_model = annotate_model_greeks(rows, price, current)
    quoted = _quoted_contracts(rows, price, current)
    if not quoted:
        return _empty("当前缓存缺少有效买卖报价，暂不生成买方结构", horizon_days)
    surviving = [row for row in quoted if row["expiry"] > horizon_end and row["years"] > horizon_years]
    if not surviving:
        return _empty(f"可用到期日都落在未来 {horizon_days} 个交易日窗口内，暂不生成买方结构", horizon_days)
    # 有 10 到 45 天的到期日时，不为了凑数去定价更远的合约。
    sweet_expirations = {
        row["expiration"]
        for row in surviving
        if not row["below_intrinsic"]
        and row["spread_ratio"] < MAX_QUOTE_SPREAD_RATIO
        and SWEET_MIN_DTE <= row["dte"] <= SWEET_MAX_DTE
    }
    if sweet_expirations:
        pricing_rows = [row for row in surviving if row["expiration"] in sweet_expirations]
    else:
        pricing_rows = surviving
    enriched = _enrich(pricing_rows, price, iv_model)
    groups = _search_groups(enriched)
    if not groups:
        return _empty("当前期权链没有满足报价和流动性条件的候选结构", horizon_days)
    volatility = _reference_iv(groups, price)
    action = str((recommendation or {}).get("action") or "hold")
    primary_direction = "call" if action == "buy" else ("put" if action == "sell" else None)
    # 中性行情也提供双向对比，但不把对比方案标成交易建议。
    directions = [primary_direction, "put" if primary_direction == "call" else "call"] if primary_direction else ["call", "put"]
    built: dict[str, list[dict[str, Any]]] = {}
    targets: dict[str, tuple[float, str, float | None]] = {}
    for direction in ("call", "put"):
        target, target_source, touch = _target_price(
            direction, price, support, resistance, trend, volatility, horizon_years,
        )
        targets[direction] = (target, target_source, touch)
        if direction in directions:
            built[direction] = _direction_candidates(
                groups, direction, target, target_source, price, horizon_years, touch,
            )
    unique = [item for direction in directions for item in built.get(direction, [])]
    if not unique:
        return _empty("当前期权链没有满足报价和流动性条件的候选结构", horizon_days)
    for item in unique:
        item["is_primary"] = primary_direction is not None and item["direction"] == primary_direction
        item["direction_label"] = "看涨方案" if item["direction"] == "call" else "看跌方案"
    return {
        "available": True,
        "status": "ok",
        "direction": primary_direction or "neutral",
        "primary_direction": primary_direction,
        "direction_label": (
            "主方向：买入看涨 · 同时对比买入看跌"
            if primary_direction == "call"
            else "主方向：买入看跌 · 同时对比买入看涨"
            if primary_direction == "put"
            else "中性对比：同时观察买入看涨 / 买入看跌"
        ),
        "recommendation": (recommendation or {}).get("reason") or (
            "结合趋势与支撑/压力位" if primary_direction else "当前未形成明确方向，仅作双向结构对比"
        ),
        "horizon_trading_days": horizon_days,
        "horizon_label": f"未来 {horizon_days} 个交易日",
        "targets": {
            direction: {
                "price": targets[direction][0],
                "source": targets[direction][1],
                "touch_probability": None if targets[direction][2] is None else round(targets[direction][2], 4),
            }
            for direction in ("call", "put")
        },
        "reference_iv": volatility,
        "items": unique[:STRUCTURE_LIMIT],
        "method": "布莱克-斯科尔斯模型估算",
        "quote_method": "仅使用有效买卖价中间价，不使用最新成交价代替成本",
        "disclaimer": "综合评分不是历史胜率。排序按触及概率加权，并扣掉半档价差；展示的预计盈亏仍以中间价计算，不扣价差。",
    }
