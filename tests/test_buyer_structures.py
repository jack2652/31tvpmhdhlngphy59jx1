import unittest
from datetime import datetime, timezone

from app.buyer_structures import build_buyer_structures


NOW = datetime(2026, 9, 21, 12, 0, tzinfo=timezone.utc)


def option_rows():
    rows = []
    for expiration in ("2026-10-02", "2026-10-30"):
        for strike, bid, ask in (
            (98, 3.1, 3.3),
            (100, 1.9, 2.1),
            (102, 1.0, 1.2),
            (105, 0.39, 0.41),
        ):
            rows.append(
                {
                    "expiration": expiration,
                    "contract_type": "call",
                    "strike": strike,
                    "bid": bid,
                    "ask": ask,
                    # 故意设置成与 Mid 不同的值，防止实现偷偷使用 Last。
                    "last_price": 0.01,
                    "volume": 1000,
                    "open_interest": 5000,
                    "implied_volatility": 0.3,
                }
            )
            put_quotes = {
                98: (0.39, 0.41),
                100: (0.98, 1.02),
                102: (1.88, 1.92),
                105: (3.08, 3.12),
            }
            put_bid, put_ask = put_quotes[strike]
            rows.append(
                {
                    "expiration": expiration,
                    "contract_type": "put",
                    "strike": strike,
                    "bid": put_bid,
                    "ask": put_ask,
                    "last_price": 0.01,
                    "volume": 900,
                    "open_interest": 4500,
                    "implied_volatility": 0.3,
                }
            )
    return rows


class BuyerStructuresTest(unittest.TestCase):
    def test_builds_directional_structures_from_bid_ask_mid(self):
        result = build_buyer_structures(
            option_rows(),
            100,
            {"direction": "up", "upper": 105, "lower": 95},
            {"action": "buy", "reason": "上行趋势"},
            [{"price": 95}],
            [{"price": 105}],
            now=NOW,
        )

        self.assertTrue(result["available"])
        self.assertEqual(len(result["items"]), 10)
        self.assertEqual(sum(item["direction"] == "call" for item in result["items"]), 5)
        self.assertEqual(sum(item["direction"] == "put" for item in result["items"]), 5)
        self.assertTrue(all(item["direction"] == "call" for item in result["items"][:5]))
        self.assertTrue(any(item["kind"] == "vertical" for item in result["items"]))
        call_singles = [item for item in result["items"] if item["kind"] == "single" and item["direction"] == "call"]
        self.assertTrue(any(item["cost"] == 320.0 for item in call_singles))
        self.assertTrue(all("scenario_profitable" in item for item in result["items"]))
        single = call_singles[0]
        self.assertEqual(single["quote_method"], "买卖价中间价")
        self.assertIn("对比买入看跌", result["direction_label"])

    def test_does_not_force_a_structure_when_direction_is_unclear(self):
        result = build_buyer_structures(
            option_rows(),
            100,
            {"direction": "range"},
            {"action": "hold"},
            [{"price": 95}],
            [{"price": 105}],
            now=NOW,
        )

        self.assertFalse(result["available"])
        self.assertEqual(result["items"], [])

    def test_put_is_primary_when_recommendation_is_sell(self):
        result = build_buyer_structures(
            option_rows(),
            100,
            {"direction": "down", "upper": 105, "lower": 95},
            {"action": "sell", "reason": "下行趋势"},
            [{"price": 95}],
            [{"price": 105}],
            now=NOW,
        )

        self.assertTrue(result["available"])
        self.assertEqual([item["direction"] for item in result["items"][:5]], ["put"] * 5)
        self.assertEqual([item["direction"] for item in result["items"][5:]], ["call"] * 5)
        self.assertTrue(all(item["is_primary"] for item in result["items"][:5]))
        self.assertTrue(all(not item["is_primary"] for item in result["items"][5:]))
        self.assertIn("对比买入看涨", result["direction_label"])

    def test_marks_negative_scenario_and_keeps_profitable_comparison_first(self):
        result = build_buyer_structures(
            option_rows(),
            100,
            {"direction": "up"},
            {"action": "buy"},
            [{"price": 99}],
            [{"price": 100.1}],
            now=NOW,
        )

        primary = [item for item in result["items"] if item["direction"] == "call"]
        comparison = [item for item in result["items"] if item["direction"] == "put"]
        self.assertTrue(primary)
        self.assertTrue(comparison)
        self.assertTrue(all(not item["scenario_profitable"] for item in primary))
        self.assertTrue(all(item["scenario_profitable"] for item in comparison))
        self.assertEqual(result["items"][0]["direction"], "call")

    def test_rejects_contracts_without_valid_two_sided_quote(self):
        rows = [{**row, "bid": None, "ask": None} for row in option_rows()]
        result = build_buyer_structures(
            rows,
            100,
            {"direction": "up"},
            {"action": "buy"},
            [{"price": 95}],
            [{"price": 105}],
            now=NOW,
        )

        self.assertFalse(result["available"])
        self.assertIn("买卖", result["reason"])

    def test_rejects_wide_quotes_from_recommendations(self):
        rows = [{**row, "bid": 1.0, "ask": 1.3} for row in option_rows()]
        result = build_buyer_structures(
            rows,
            100,
            {"direction": "up"},
            {"action": "buy"},
            [{"price": 95}],
            [{"price": 105}],
            now=NOW,
        )

        self.assertFalse(result["available"])
        self.assertIn("流动性", result["reason"])


if __name__ == "__main__":
    unittest.main()
