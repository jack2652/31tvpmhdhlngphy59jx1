"""Database concern mixin extracted from the legacy Database facade."""
from __future__ import annotations
import logging
import sqlite3
import time
from datetime import datetime, timedelta
from zoneinfo import ZoneInfo
from typing import Any
from app.db_constants import (
    ANALYSIS_JOB_STALE_SECONDS, NO_FLOOR, ORPHANED_ANALYSIS_MESSAGE,
    SIZE_CLEANUP_PROTECT_HOURS, SIZE_CLEANUP_TARGET_RATIO, SIZE_CLEANUP_TIME_BUDGET_SECONDS,
)
MARKET_TIMEZONE = ZoneInfo("America/New_York")
logger = logging.getLogger("app.db")

class DatabaseOptionsMixin:
    def option_flow(self, symbol: str, expiration: str) -> dict[str, Any]:
        """比较同一期限最近两次快照，估算 Call/Put 的新增买卖流向。"""
        def empty_side() -> dict[str, Any]:
            return {
                "buy_volume": 0,
                "sell_volume": 0,
                "unknown_volume": 0,
                "buy_premium": 0.0,
                "sell_premium": 0.0,
                "unknown_premium": 0.0,
                "buy_contracts": 0,
                "sell_contracts": 0,
                "unknown_contracts": 0,
                "net_volume": 0,
                "net_premium": 0.0,
                "top_strikes": [],
                "concentration": [],
            }

        def empty_result(reason: str, current_fetched_at: str | None = None) -> dict[str, Any]:
            return {
                "symbol": symbol,
                "expiration": expiration,
                "available": False,
                "source": "sqlite",
                "reason": reason,
                "current_fetched_at": current_fetched_at,
                "previous_fetched_at": None,
                "interval_seconds": None,
                "reset_count": 0,
                "call": empty_side(),
                "put": empty_side(),
                "signal": {"label": "等待下一快照", "class_name": "waiting", "detail": reason},
            }

        with self.connect() as connection:
            batches = connection.execute(
                """SELECT fetched_at
                     FROM option_snapshots
                    WHERE symbol=? AND expiration=?
                    GROUP BY fetched_at
                    ORDER BY fetched_at DESC
                    LIMIT 2""",
                (symbol, expiration),
            ).fetchall()
            if not batches:
                return empty_result("暂无该期限的期权快照")
            current_fetched_at = str(batches[0][0])
            if len(batches) < 2:
                return empty_result("等待下一份相邻快照", current_fetched_at)
            previous_fetched_at = str(batches[1][0])
            columns = "contract_symbol, contract_type, strike, last_price, bid, ask, volume"
            current_rows = connection.execute(
                f"SELECT {columns} FROM option_snapshots WHERE symbol=? AND expiration=? AND fetched_at=?",
                (symbol, expiration, current_fetched_at),
            ).fetchall()
            previous_rows = connection.execute(
                f"SELECT {columns} FROM option_snapshots WHERE symbol=? AND expiration=? AND fetched_at=?",
                (symbol, expiration, previous_fetched_at),
            ).fetchall()

        def number(value: Any) -> float | None:
            try:
                parsed = float(value)
            except (TypeError, ValueError):
                return None
            return parsed if parsed == parsed else None

        def volume(value: Any) -> int:
            parsed = number(value)
            return max(0, int(parsed or 0))

        def classify(row: dict[str, Any]) -> str:
            last = number(row.get("last_price"))
            bid = number(row.get("bid"))
            ask = number(row.get("ask"))
            if last is None or bid is None or ask is None or bid <= 0 or ask < bid or last <= 0:
                return "unknown"
            spread = ask - bid
            if spread <= 0:
                return "unknown"
            edge = spread * 0.25
            if last >= ask - edge:
                return "buy"
            if last <= bid + edge:
                return "sell"
            return "unknown"

        current_by_contract = {str(row[0]): dict(row) for row in current_rows if row[0]}
        previous_by_contract = {str(row[0]): dict(row) for row in previous_rows if row[0]}
        sides = {"call": empty_side(), "put": empty_side()}
        strike_totals: dict[str, dict[tuple[float, str], dict[str, Any]]] = {"call": {}, "put": {}}
        strike_concentration: dict[str, dict[float, dict[str, Any]]] = {"call": {}, "put": {}}
        reset_count = 0
        for contract_symbol, row in current_by_contract.items():
            contract_type = str(row.get("contract_type") or "").lower()
            if contract_type not in sides:
                continue
            current_volume = volume(row.get("volume"))
            previous_volume = volume(previous_by_contract.get(contract_symbol, {}).get("volume"))
            delta = current_volume - previous_volume
            if delta < 0:
                reset_count += 1
                delta = current_volume
            if delta <= 0:
                continue
            direction = classify(row)
            premium = delta * max(number(row.get("last_price")) or 0.0, 0.0) * 100
            side = sides[contract_type]
            side[f"{direction}_volume"] += delta
            side[f"{direction}_premium"] += premium
            side[f"{direction}_contracts"] += 1
            strike = number(row.get("strike"))
            if strike is not None:
                key = (strike, direction)
                grouped = strike_totals[contract_type].setdefault(key, {"volume": 0, "premium": 0.0})
                grouped["volume"] += delta
                grouped["premium"] += premium
                concentrated = strike_concentration[contract_type].setdefault(
                    strike,
                    {"volume": 0, "buy_volume": 0, "sell_volume": 0, "unknown_volume": 0, "premium": 0.0},
                )
                concentrated["volume"] += delta
                concentrated[f"{direction}_volume"] += delta
                concentrated["premium"] += premium

        for contract_type, side in sides.items():
            side["net_volume"] = side["buy_volume"] - side["sell_volume"]
            side["net_premium"] = round(side["buy_premium"] - side["sell_premium"], 2)
            side["buy_premium"] = round(side["buy_premium"], 2)
            side["sell_premium"] = round(side["sell_premium"], 2)
            side["unknown_premium"] = round(side["unknown_premium"], 2)
            side["top_strikes"] = [
                {"strike": strike, "direction": direction, "volume": values["volume"], "premium": round(values["premium"], 2)}
                for (strike, direction), values in sorted(
                    strike_totals[contract_type].items(), key=lambda item: item[1]["volume"], reverse=True
                )[:5]
            ]
            side["concentration"] = []
            for strike, values in sorted(
                strike_concentration[contract_type].items(), key=lambda item: item[1]["volume"], reverse=True
            )[:5]:
                if values["buy_volume"] > values["sell_volume"]:
                    dominant_direction = "buy"
                elif values["sell_volume"] > values["buy_volume"]:
                    dominant_direction = "sell"
                else:
                    dominant_direction = "unknown"
                side["concentration"].append(
                    {
                        "strike": strike,
                        "volume": values["volume"],
                        "buy_volume": values["buy_volume"],
                        "sell_volume": values["sell_volume"],
                        "unknown_volume": values["unknown_volume"],
                        "premium": round(values["premium"], 2),
                        "dominant_direction": dominant_direction,
                    }
                )

        try:
            current_time = datetime.fromisoformat(current_fetched_at.replace("Z", "+00:00"))
            previous_time = datetime.fromisoformat(previous_fetched_at.replace("Z", "+00:00"))
            interval_seconds = max(0, round((current_time - previous_time).total_seconds()))
        except ValueError:
            interval_seconds = None
        bullish_premium = sides["call"]["buy_premium"] + sides["put"]["sell_premium"]
        bearish_premium = sides["call"]["sell_premium"] + sides["put"]["buy_premium"]
        if bullish_premium <= 0 and bearish_premium <= 0:
            bullish_score = sides["call"]["buy_volume"] + sides["put"]["sell_volume"]
            bearish_score = sides["call"]["sell_volume"] + sides["put"]["buy_volume"]
        else:
            bullish_score = bullish_premium
            bearish_score = bearish_premium
        if bullish_score <= 0 and bearish_score <= 0:
            signal = {"label": "暂无新增流向", "class_name": "neutral", "detail": "相邻快照没有新增成交量"}
        elif bullish_score >= bearish_score * 1.15:
            signal = {"label": "偏多流向", "class_name": "bullish", "detail": "Call 买入与 Put 卖出占优"}
        elif bearish_score >= bullish_score * 1.15:
            signal = {"label": "偏空流向", "class_name": "bearish", "detail": "Call 卖出与 Put 买入占优"}
        else:
            signal = {"label": "多空分歧", "class_name": "neutral", "detail": "Call/Put 新增流向接近"}
        result = {
            "symbol": symbol,
            "expiration": expiration,
            "available": True,
            "source": "sqlite",
            "reason": "相邻快照成交量增量，买卖方向按成交价接近 Bid/Ask 估算",
            "current_fetched_at": current_fetched_at,
            "previous_fetched_at": previous_fetched_at,
            "interval_seconds": interval_seconds,
            "reset_count": reset_count,
            "call": sides["call"],
            "put": sides["put"],
            "signal": signal,
        }
        if reset_count:
            result["warning"] = f"{reset_count} 个合约成交量出现回落，按当前量作为重置后增量估算"
        return result


    @staticmethod
    def _open_interest_fallback(connection: sqlite3.Connection, symbol: str, start: str, end: str) -> dict[tuple[str, str], tuple[int, str]]:
        """读取每个合约最近一次非零的未平仓量，供盘前空值兜底使用。"""
        rows = connection.execute(
            """SELECT expiration, contract_symbol, open_interest, MAX(fetched_at) AS fetched_at
                 FROM option_snapshots
                WHERE symbol=? AND expiration BETWEEN ? AND ? AND open_interest > 0
                GROUP BY expiration, contract_symbol""",
            (symbol, start, end),
        ).fetchall()
        # SQLite 在 GROUP BY 中搭配 MAX() 时，裸列取自最大值所在的那一行，因此这里拿到的是最近一次有效的未平仓量。
        return {(str(row["expiration"]), str(row["contract_symbol"])): (int(row["open_interest"]), str(row["fetched_at"])) for row in rows}


    @staticmethod
    def _apply_open_interest_fallback(rows: list[dict[str, Any]], fallback: dict[tuple[str, str], tuple[int, str]]) -> dict[str, Any]:
        """把为 0 的未平仓量替换成同一合约最近一次有效值，并返回回溯统计。"""
        restored = 0
        as_of: str | None = None
        for row in rows:
            if row.get("open_interest"):
                continue
            entry = fallback.get((str(row.get("expiration")), str(row.get("contract_symbol"))))
            if entry is None:
                continue
            row["open_interest"] = entry[0]
            restored += 1
            if as_of is None or entry[1] > as_of:
                as_of = entry[1]
        return {"restored": restored, "as_of": as_of}


    def latest_chains(self, symbol: str, horizon_days: int = 45) -> dict[str, Any]:
        """返回近期期限的最新期权链，用于跨到期日 Gamma 分析。"""
        # 到期日是美东日历日；服务在 UTC 晚间运行时，不能提前跳过仍在交易中的美东当天合约。
        market_date = datetime.now(MARKET_TIMEZONE).date()
        start = market_date.isoformat()
        end = (market_date + timedelta(days=horizon_days)).isoformat()
        with self.connect() as connection:
            rows = connection.execute(
                """SELECT current.contract_symbol, current.expiration, current.contract_type,
                          current.strike, current.last_price, current.bid, current.ask,
                          current.volume, current.open_interest, current.implied_volatility,
                          current.gamma, current.in_the_money, current.change_percent,
                          current.fetched_at
                     FROM option_latest_batches AS latest
                     JOIN option_snapshots AS current
                       ON current.symbol=latest.symbol
                      AND current.expiration=latest.expiration
                      AND latest.fetched_at=current.fetched_at
                    WHERE latest.symbol=? AND latest.expiration BETWEEN ? AND ?
                   ORDER BY current.expiration, current.strike, current.contract_type""",
                (symbol, start, end),
            ).fetchall()
            fallback = self._open_interest_fallback(connection, symbol, start, end)
        data = [dict(row) for row in rows]
        expirations = sorted({str(row["expiration"]) for row in data})
        fetched_values = [row["fetched_at"] for row in data if row.get("fetched_at")]
        oi_fallback = self._apply_open_interest_fallback(data, fallback)
        for row in data:
            row.pop("fetched_at", None)
        return {
            "fetched_at": max(fetched_values) if fetched_values else None,
            "data": data,
            "expirations": expirations,
            "horizon_days": horizon_days,
            "oi_fallback": oi_fallback,
        }
