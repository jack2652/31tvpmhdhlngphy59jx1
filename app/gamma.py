"""Zero Gamma / GEX 估算。"""

from __future__ import annotations

import math
from datetime import date, datetime, time, timedelta, timezone
from functools import lru_cache
from typing import Any, Iterable
from zoneinfo import ZoneInfo


NEW_YORK = ZoneInfo("America/New_York")
FLIP_HORIZON_DAYS = 7
CHART_HORIZON_DAYS = 45
MIN_MINUTES = 30
SCAN_BAND = 0.15
SCAN_STEPS = 161
BISECTION_STEPS = 40
MAX_ACTIONABLE_DISTANCE = 0.08
RISK_FREE = 0.005
# 隐含波动率估计参数：上游的 IV 字段在盘前/收盘后常是占位值（1e-5、1/32 之类），
# 因此改用合约价格反解，样本只取近 ATM 且价格可信的合约。
IV_MIN = 0.03
IV_MAX = 5.0
IV_SAMPLE_BAND = 0.12
IV_ATM_BAND = 0.03
IV_FLOOR = 0.05
IV_CEILING = 3.0
IV_MIN_PRICE = 0.02
IV_FALLBACK = 0.25


def _cache_number(value: Any) -> float | None:
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    return number if math.isfinite(number) else None


def option_expiry(expiration: str) -> datetime | None:
    """美股股票期权按到期日 16:00 America/New_York 截止。"""
    try:
        day = date.fromisoformat(expiration)
    except (TypeError, ValueError):
        return None
    return datetime.combine(day, time(16, 0), tzinfo=NEW_YORK)


def years_to_expiry(expiration: str, now: datetime | None = None) -> float | None:
    expiry = option_expiry(expiration)
    if expiry is None:
        return None
    current = now or datetime.now(timezone.utc)
    if current.tzinfo is None:
        current = current.replace(tzinfo=timezone.utc)
    seconds = (expiry.astimezone(timezone.utc) - current.astimezone(timezone.utc)).total_seconds()
    return max(seconds / (365 * 24 * 3600), MIN_MINUTES / (365 * 24 * 60))


def black_scholes_gamma(spot: float, strike: float, implied_volatility: float, time_years: float, rate: float = RISK_FREE) -> float | None:
    if spot <= 0 or strike <= 0 or implied_volatility <= 0 or time_years <= 0:
        return None
    volatility_time = implied_volatility * math.sqrt(time_years)
    if volatility_time <= 0:
        return None
    d1 = (math.log(spot / strike) + (rate + 0.5 * implied_volatility ** 2) * time_years) / volatility_time
    return math.exp(-0.5 * d1 ** 2) / (spot * volatility_time * math.sqrt(2 * math.pi))


def _as_now(now: datetime | None) -> datetime:
    current = now or datetime.now(timezone.utc)
    if current.tzinfo is None:
        return current.replace(tzinfo=timezone.utc)
    return current.astimezone(timezone.utc)


def within_horizon(row: dict[str, Any], horizon_days: int, now: datetime | None = None) -> bool:
    current = _as_now(now).astimezone(NEW_YORK).date()
    try:
        expiration = date.fromisoformat(str(row.get("expiration")))
    except (TypeError, ValueError):
        return False
    if expiration < current or expiration > current + timedelta(days=horizon_days):
        return False
    expiry = option_expiry(str(row.get("expiration")))
    if expiry is None:
        return False
    remain_seconds = (expiry.astimezone(timezone.utc) - _as_now(now)).total_seconds()
    return remain_seconds >= -MIN_MINUTES * 60


@lru_cache(maxsize=32)
def _scan_zero_gamma(
    spot_value: float,
    horizon_days: int,
    prepared: tuple[tuple[float, float, float, float, float], ...],
    expirations: tuple[str, ...],
    lower: float,
    upper: float,
) -> tuple[float, float, tuple[str, ...]] | None:
    """缓存固定快照的 Zero Gamma 扫描；prepared 已包含所有与时间有关的参数。"""
    if not upper > lower:
        return None

    def gex_at(price: float) -> float:
        total = 0.0
        for strike, implied_volatility, open_interest, sign, time_years in prepared:
            if implied_volatility <= 0 or open_interest <= 0:
                continue
            gamma = black_scholes_gamma(price, strike, implied_volatility, time_years)
            if gamma is not None:
                total += sign * gamma * open_interest * 100 * price ** 2 * 0.01
        return total

    roots: list[float] = []
    previous_spot = lower
    previous_value = gex_at(previous_spot)
    for index in range(1, SCAN_STEPS + 1):
        next_spot = lower + (upper - lower) * index / SCAN_STEPS
        next_value = gex_at(next_spot)
        if previous_value == 0:
            roots.append(previous_spot)
        if previous_value * next_value < 0:
            left, right, left_value = previous_spot, next_spot, previous_value
            for _ in range(BISECTION_STEPS):
                middle = (left + right) / 2
                middle_value = gex_at(middle)
                if left_value * middle_value <= 0:
                    right = middle
                else:
                    left, left_value = middle, middle_value
            roots.append((left + right) / 2)
        previous_spot, previous_value = next_spot, next_value
    if not roots:
        return None
    net_gex = gex_at(spot_value)
    actionable = [root for root in roots if abs(root - spot_value) / spot_value <= MAX_ACTIONABLE_DISTANCE]
    candidates = actionable or roots
    if net_gex >= 0:
        below = [root for root in candidates if root <= spot_value]
        chosen = max(below) if below else min(candidates, key=lambda root: abs(root - spot_value))
    else:
        above = [root for root in candidates if root >= spot_value]
        chosen = min(above) if above else min(candidates, key=lambda root: abs(root - spot_value))
    return chosen, net_gex, expirations


def contract_gex(row: dict[str, Any], spot: float, now: datetime | None = None) -> float:
    try:
        strike = float(row["strike"])
        # 模型 IV 由合约价格反解得到，优先于数据源自带的 IV 字段。
        implied_volatility = float(row.get("model_iv") or row.get("implied_volatility") or 0)
        open_interest = float(row.get("open_interest") or 0)
    except (TypeError, ValueError, KeyError):
        return 0.0
    if open_interest <= 0 or implied_volatility <= 0:
        return 0.0
    time_years = years_to_expiry(str(row.get("expiration")), now)
    gamma = black_scholes_gamma(spot, strike, implied_volatility, time_years or 0)
    if gamma is None:
        return 0.0
    sign = 1.0 if row.get("contract_type") == "call" else -1.0
    return sign * gamma * open_interest * 100 * spot ** 2 * 0.01


def total_gex(rows: Iterable[dict[str, Any]], spot: float, now: datetime | None = None) -> float:
    return sum(contract_gex(row, spot, now) for row in rows)


def find_zero_gamma(
    rows: Iterable[dict[str, Any]],
    spot: float | None,
    horizon_days: int = FLIP_HORIZON_DAYS,
    now: datetime | None = None,
) -> dict[str, Any] | None:
    """在近 7 日未到期合约上做 spot-shift GEX 曲线，并取制度一致的最近零点。"""
    try:
        spot_value = float(spot)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return None
    if spot_value <= 0:
        return None
    selected = [row for row in rows if within_horizon(row, horizon_days, now)]
    strikes: list[float] = []
    for row in selected:
        try:
            strike = float(row["strike"])
        except (TypeError, ValueError, KeyError):
            continue
        if strike > 0:
            strikes.append(strike)
    if not selected or not strikes:
        return None
    # 扫描过程中每个价格点都会重复使用这些合约参数；到期时间和方向与扫描价格无关，
    # 先固定下来，避免 161 个扫描点加二分步骤反复解析日期和转换时区。
    current = _as_now(now)
    prepared: list[tuple[float, float, float, float, float]] = []
    for row in selected:
        try:
            strike = float(row["strike"])
            implied_volatility = float(row.get("model_iv") or row.get("implied_volatility") or 0)
            open_interest = float(row.get("open_interest") or 0)
        except (TypeError, ValueError, KeyError):
            continue
        if strike <= 0:
            continue
        time_years = years_to_expiry(str(row.get("expiration")), current)
        if time_years is None or time_years <= 0:
            continue
        sign = 1.0 if row.get("contract_type") == "call" else -1.0
        prepared.append((strike, implied_volatility, open_interest, sign, time_years))
    if not prepared:
        return None

    expirations = tuple(sorted({str(row["expiration"]) for row in selected if row.get("expiration")}))
    strike_values = [strike for row in selected for strike in [_cache_number(row.get("strike"))] if strike and strike > 0]
    if not strike_values:
        return None
    lower = max(min(strike_values), spot_value * (1 - SCAN_BAND))
    upper = min(max(strike_values), spot_value * (1 + SCAN_BAND))
    result = _scan_zero_gamma(spot_value, horizon_days, tuple(prepared), expirations, lower, upper)
    if result is None:
        return None
    chosen, net_gex, expirations = result
    return {
        "price": chosen,
        "net_gex": net_gex,
        "expirations": list(expirations),
        "horizon_days": horizon_days,
        "method": "spot-shift-7d-et-close",
    }


def norm_cdf(value: float) -> float:
    return 0.5 * (1.0 + math.erf(value / math.sqrt(2)))


def black_scholes_price(spot: float, strike: float, implied_volatility: float, time_years: float, is_call: bool, rate: float = RISK_FREE) -> float:
    """Black-Scholes 欧式期权理论价，用于从市场价格反解隐含波动率。"""
    volatility_time = implied_volatility * math.sqrt(time_years)
    if spot <= 0 or strike <= 0 or volatility_time <= 0:
        return 0.0
    d1 = (math.log(spot / strike) + (rate + 0.5 * implied_volatility ** 2) * time_years) / volatility_time
    d2 = d1 - volatility_time
    discount = math.exp(-rate * time_years)
    if is_call:
        return spot * norm_cdf(d1) - strike * discount * norm_cdf(d2)
    return strike * discount * norm_cdf(-d2) - spot * norm_cdf(-d1)


def implied_volatility_from_price(price: float, spot: float, strike: float, time_years: float, is_call: bool) -> float | None:
    """二分反解隐含波动率；价格低于理论下限或超出可行区间时返回 None。"""
    if price <= 0 or time_years <= 0 or spot <= 0 or strike <= 0:
        return None
    if black_scholes_price(spot, strike, IV_MAX, time_years, is_call) < price:
        return None
    # 价格低于最小波动率对应的理论价时反解没有意义（多为陈旧或异常报价）。
    if black_scholes_price(spot, strike, IV_MIN, time_years, is_call) > price:
        return None
    low, high = IV_MIN, IV_MAX
    for _ in range(60):
        middle = (low + high) / 2
        if black_scholes_price(spot, strike, middle, time_years, is_call) < price:
            low = middle
        else:
            high = middle
    return (low + high) / 2


def _quote_price(row: dict[str, Any]) -> float:
    """取合约报价：优先买卖中间价，盘前没有报价时退回最新成交价。"""
    try:
        bid = float(row.get("bid") or 0)
        ask = float(row.get("ask") or 0)
    except (TypeError, ValueError):
        return 0.0
    if bid > 0 and ask >= bid:
        return (bid + ask) / 2
    try:
        return float(row.get("last_price") or 0)
    except (TypeError, ValueError):
        return 0.0


def _contract_sample_iv(row: dict[str, Any], spot: float, years: float) -> tuple[float, float] | None:
    """反解单张合约的 IV；返回 (对数偏离度, IV)，价格不可信时返回 None。"""
    try:
        strike = float(row["strike"])
    except (TypeError, ValueError, KeyError):
        return None
    price = _quote_price(row)
    if strike <= 0 or price < IV_MIN_PRICE or years <= 0:
        return None
    distance = abs(math.log(strike / spot))
    if distance > IV_SAMPLE_BAND:
        return None
    is_call = row.get("contract_type") == "call"
    intrinsic = max(0.0, spot - strike) if is_call else max(0.0, strike - spot)
    if price < intrinsic * 0.98:
        return None
    volatility = implied_volatility_from_price(price, spot, strike, years, is_call)
    return (distance, volatility) if volatility else None


def estimate_expiration_iv(samples: list[tuple[float, float]], rows: Iterable[dict[str, Any]]) -> tuple[float, int, str]:
    """用近 ATM 样本的中位数代表该到期日的波动率水平。"""
    near = sorted(volatility for distance, volatility in samples if distance <= IV_ATM_BAND)
    if len(near) >= 3:
        return min(max(near[len(near) // 2], IV_FLOOR), IV_CEILING), len(near), "price"
    if samples:
        values = sorted(volatility for _, volatility in samples)
        return min(max(values[len(values) // 2], IV_FLOOR), IV_CEILING), len(values), "price"
    # 价格不可用时退回数据源自带的 IV，仅在合理区间内取中位数。
    provided = sorted(
        float(row["implied_volatility"])
        for row in rows
        if row.get("implied_volatility")
    )
    provided = [value for value in provided if IV_FLOOR <= value <= IV_CEILING]
    if provided:
        return provided[len(provided) // 2], len(provided), "provider"
    return IV_FALLBACK, 0, "default"


@lru_cache(maxsize=64)
def _cached_expiration_greeks(
    spot: float,
    expiration: str,
    years: float,
    inputs: tuple[tuple[Any, ...], ...],
) -> tuple[float, int, str, tuple[float | None, ...]]:
    """缓存同一快照期限的 IV 反解与 Gamma，避免多个接口重复计算。"""
    group = [
        {
            "strike": row[0],
            "bid": row[1],
            "ask": row[2],
            "last_price": row[3],
            "implied_volatility": row[4],
            "contract_type": row[5],
        }
        for row in inputs
    ]
    samples = [sample for sample in (_contract_sample_iv(row, spot, years) for row in group) if sample]
    volatility, count, source = estimate_expiration_iv(samples, group)
    gammas = tuple(
        black_scholes_gamma(spot, _cache_number(row[0]) or 0.0, volatility, years)
        for row in inputs
    )
    return volatility, count, source, gammas


def annotate_model_greeks(rows: Iterable[dict[str, Any]], spot: float | None, now: datetime | None = None) -> dict[str, dict[str, Any]]:
    """按到期日用价格反解的 IV 写回 model_iv / model_gamma，并返回每个到期日的估计摘要。"""
    grouped: dict[str, list[dict[str, Any]]] = {}
    for row in rows:
        expiration = str(row.get("expiration") or "")
        if expiration:
            grouped.setdefault(expiration, []).append(row)
    summary: dict[str, dict[str, Any]] = {}
    try:
        spot_value = float(spot)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return summary
    if spot_value <= 0:
        return summary
    for expiration, group in grouped.items():
        years = years_to_expiry(expiration, now) or 0
        inputs = tuple(
            (
                _cache_number(row.get("strike")),
                _cache_number(row.get("bid")),
                _cache_number(row.get("ask")),
                _cache_number(row.get("last_price")),
                _cache_number(row.get("implied_volatility")),
                row.get("contract_type"),
            )
            for row in group
        )
        volatility, count, source, gammas = _cached_expiration_greeks(
            spot_value, expiration, years, inputs
        )
        for row, gamma in zip(group, gammas):
            row["model_iv"] = volatility
            row["model_gamma"] = gamma
        summary[expiration] = {"iv": round(volatility, 6), "samples": count, "source": source}
    return summary
