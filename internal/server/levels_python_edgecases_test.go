package server

import "testing"

func TestLevelsPlanSelectionUsesPythonPriceCoercion(t *testing.T) {
	t.Run("invalid and zero prices fall back to spot", func(t *testing.T) {
		values := []map[string]any{
			{"price": 99.0, "score": .5, "marker": "valid"},
			{"score": .5, "marker": "missing"},
			{"price": 0.0, "score": .5, "marker": "zero"},
			{"price": "bad", "score": .5, "marker": "invalid"},
		}
		buy, _ := splitCachedSupport(values, 100)
		if len(buy) != 2 || buy[0]["marker"] != "missing" || buy[1]["marker"] != "zero" {
			t.Fatalf("buy plan=%#v, want Python fallback-price tie order [missing zero]", buy)
		}
	})

	t.Run("numeric strings are prices", func(t *testing.T) {
		values := []map[string]any{
			{"price": 99.0, "score": .5, "marker": "99"},
			{"price": "99.5", "score": .5, "marker": "99.5"},
			{"price": 98.0, "score": .5, "marker": "98"},
			{"price": 97.0, "score": .5, "marker": "97"},
		}
		buy, _ := splitCachedSupport(values, 100)
		if len(buy) != 2 || buy[0]["marker"] != "99.5" || buy[1]["marker"] != "99" {
			t.Fatalf("buy plan=%#v, want Python numeric-string order [99.5 99]", buy)
		}
	})
}

func TestLevelsPlanRanksNumericStringScoresLikePython(t *testing.T) {
	values := make([]map[string]any, 22)
	for index := 0; index < 10; index++ {
		values[index] = map[string]any{"price": float64(99 - index), "score": .1, "marker": "buy"}
	}
	values[10] = map[string]any{"price": 89.0, "score": "100", "marker": "string-score"}
	for index := 11; index < 21; index++ {
		values[index] = map[string]any{"price": float64(100 - index), "score": 1.0, "marker": "numeric-score"}
	}
	values[21] = map[string]any{"price": 78.0, "score": .01, "marker": "low-score"}

	_, add := splitCachedSupport(values, 100)
	for _, item := range add {
		if item["marker"] == "string-score" {
			return
		}
	}
	t.Fatalf("add plan=%#v, want numeric-string score ranked among Python's top ten", add)
}

func TestLevelsSideFilterUsesPythonPriceCoercionAndSortFallback(t *testing.T) {
	values := []map[string]any{
		{"price": "101", "marker": "numeric-string"},
		{"price": 102.0, "marker": "numeric"},
		{"price": "invalid", "marker": "invalid"},
		{"price": 0.0, "marker": "zero"},
		{"price": 99.0, "marker": "below"},
	}

	above := filterCachedSide(values, 100, true)
	if len(above) != 2 || above[0]["marker"] != "numeric-string" || above[1]["marker"] != "numeric" {
		t.Fatalf("above candidates=%#v, want numeric-string then numeric price", above)
	}
	below := filterCachedSide(values, 100, false)
	if len(below) != 3 || below[0]["marker"] != "invalid" || below[1]["marker"] != "zero" || below[2]["marker"] != "below" {
		t.Fatalf("below candidates=%#v, want invalid and zero fallback at spot before price 99", below)
	}
}
