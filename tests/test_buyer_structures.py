import unittest
from datetime import datetime, timezone

from app.buyer_structures import build_buyer_structures


NOW = datetime(2026, 9, 21, 12, 0, tzinfo=timezone.utc)


def option_rows(expirations=("2026-10-02", "2026-10-30")):
    rows = []
    for expiration in expirations:
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
        self.assertTrue(all(item["cost"] in {40.0, 320.0} for item in call_singles))
        self.assertTrue(all(item["dte"] >= 10 for item in result["items"]))
        self.assertTrue(all("scenario_profitable" in item for item in result["items"]))
        self.assertTrue(all(item["expected_return"] is not None for item in result["items"]))
        for direction in ("call", "put"):
            block = [item["expected_return"] for item in result["items"] if item["direction"] == direction]
            self.assertEqual(block, sorted(block, reverse=True))
        single = call_singles[0]
        self.assertEqual(single["quote_method"], "买卖价中间价")
        self.assertIn("对比买入看跌", result["direction_label"])

    def test_compares_both_directions_when_direction_is_unclear(self):
        result = build_buyer_structures(
            option_rows(),
            100,
            {"direction": "range"},
            {"action": "hold"},
            [{"price": 95}],
            [{"price": 105}],
            now=NOW,
        )

        self.assertTrue(result["available"])
        self.assertEqual(result["direction"], "neutral")
        self.assertIsNone(result["primary_direction"])
        self.assertIn("中性对比", result["direction_label"])
        self.assertEqual({item["direction"] for item in result["items"]}, {"call", "put"})
        self.assertTrue(all(not item["is_primary"] for item in result["items"]))

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


    def test_skips_expirations_inside_the_horizon_and_outside_the_sweet_spot(self):
        inside = build_buyer_structures(
            option_rows(("2026-09-23", "2026-09-25")),
            100,
            {"direction": "up"},
            {"action": "buy"},
            [{"price": 95}],
            [{"price": 105}],
            now=NOW,
        )
        self.assertFalse(inside["available"])
        self.assertIn("窗口", inside["reason"])

        preferred = build_buyer_structures(
            option_rows(("2026-09-28", "2026-10-16")),
            100,
            {"direction": "up"},
            {"action": "buy"},
            [{"price": 95}],
            [{"price": 105}],
            now=NOW,
        )
        self.assertTrue(preferred["available"])
        self.assertTrue(preferred["items"])
        self.assertTrue(all(item["expiration"] == "2026-10-16" for item in preferred["items"]))

    def test_target_prefers_stronger_reachable_level_over_nearer_weak_level(self):
        result = build_buyer_structures(
            option_rows(),
            100,
            {"direction": "up", "upper": 110, "lower": 90},
            {"action": "buy"},
            [{"price": 96, "score": 0.2}],
            [{"price": 100.4, "score": 0.15}, {"price": 104, "score": 0.9}],
            now=NOW,
        )
        self.assertEqual(result["targets"]["call"]["price"], 104)
        self.assertGreater(result["targets"]["call"]["touch_probability"], 0.15)
        self.assertTrue(all(item["target_price"] == 104 for item in result["items"] if item["direction"] == "call"))

    def test_expensive_otm_uses_its_own_implied_volatility(self):
        rows = []
        for contract_type, strike, bid, ask in (
            ("call", 100, 3.9, 4.1),
            ("call", 105, 3.4, 3.6),
            ("put", 100, 3.9, 4.1),
            ("put", 95, 1.4, 1.5),
        ):
            rows.append({
                "expiration": "2026-10-16",
                "contract_type": contract_type,
                "strike": strike,
                "bid": bid,
                "ask": ask,
                "last_price": 0.01,
                "volume": 500,
                "open_interest": 800,
                "implied_volatility": 0.2,
            })
        result = build_buyer_structures(
            rows,
            100,
            {"direction": "up"},
            {"action": "buy"},
            [{"price": 95}],
            [{"price": 108}],
            now=NOW,
        )
        atm = next(item for item in result["items"] if item["direction"] == "call" and item["kind"] == "single" and item["strikes"] == [100])
        rich = next(item for item in result["items"] if item["direction"] == "call" and item["kind"] == "single" and item["strikes"] == [105])
        self.assertGreater(rich["iv"], atm["iv"] + 0.1)
        self.assertEqual(rich["model_iv_source"], "合约中间价反解")
        self.assertGreater(rich["scenario_return"], -0.35)




if __name__ == "__main__":
    unittest.main()
