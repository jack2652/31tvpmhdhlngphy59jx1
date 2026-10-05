"""Ranking and diversification of buyer structures."""
from __future__ import annotations
import math
from typing import Any

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
