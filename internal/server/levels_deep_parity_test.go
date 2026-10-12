package server

import "testing"

func TestLevelsSelectionRetainsDistinctRoundedPriceStrongCandidateLikePython(t *testing.T) {
	values := make([]map[string]any, 0, 11)
	for index := 0; index < 5; index++ {
		values = append(values, map[string]any{
			"price": 101.0, "score": .56, "strength_tier": "normal", "marker": "near-" + string(rune('0'+index)),
		})
	}
	values = append(values, map[string]any{
		"price": 101.0, "score": .95, "strength_tier": "strong", "marker": "strong-duplicate-price",
	})
	for index := 0; index < 5; index++ {
		values = append(values, map[string]any{
			"price": float64(105 + index), "score": .56, "strength_tier": "normal", "marker": "far",
		})
	}

	selected := selectVisibleCachedLevels(values, 100, 10)
	if len(selected) != 10 {
		t.Fatalf("selected %d levels, want the Python limit of 10", len(selected))
	}
	for _, item := range selected {
		if item["marker"] == "strong-duplicate-price" {
			return
		}
	}
	t.Fatalf("strong candidate with an equal rounded price was dropped: %#v", selected)
}
