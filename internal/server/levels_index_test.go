package server

import (
	"math"
	"testing"
	"time"

	"option-scope-go/internal/model"
)

func TestIndexLevelsUseDailySwingsWithoutOptionRows(t *testing.T) {
	const barCount = 180
	bars := make([]map[string]any, 0, barCount)
	for index := 0; index < barCount; index++ {
		close := 100 + float64(index)*0.04 + 5*math.Sin(float64(index)*math.Pi/8)
		bars = append(bars, map[string]any{
			"date":   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, index).Format("2006-01-02"),
			"open":   close,
			"high":   close + .8,
			"low":    close - .8,
			"close":  close,
			"volume": 0,
		})
	}
	spot := 110.0
	quote := model.Quote{Symbol: "^GSPC", Sessions: map[string]any{}}
	levels := buildCachedLevelsAt("^GSPC", "", bars, nil, quote, &spot, &spot, time.Now(), map[string]any{}, map[string]any{}, nil, time.Now(), map[string]any{})
	if levels == nil {
		t.Fatal("index levels are nil without option rows")
	}

	resistance, _ := levels["resistance"].([]map[string]any)
	support, _ := levels["support"].([]map[string]any)
	if len(resistance) == 0 || len(support) < 2 {
		t.Fatalf("resistance/support counts = %d/%d, want resistance and at least two supports for buy/add", len(resistance), len(support))
	}
	for _, side := range []struct {
		name   string
		values []map[string]any
		above  bool
	}{{"resistance", resistance, true}, {"support", support, false}} {
		for _, level := range side.values {
			price := numberValue(level["price"])
			if price == nil || (side.above && *price <= spot) || (!side.above && *price >= spot) {
				t.Errorf("%s level has invalid side/price: %#v", side.name, level)
			}
			if level["probability"] != nil {
				t.Errorf("%s level fabricated option probability: %#v", side.name, level["probability"])
			}
			factors, _ := level["factors"].([]string)
			if len(factors) == 0 {
				t.Errorf("%s level has no technical factor: %#v", side.name, level)
			}
		}
	}

	plan, _ := levels["plan"].(map[string]any)
	add, _ := plan["add"].([]map[string]any)
	if len(add) == 0 {
		t.Fatal("index add plan is empty despite multiple technical support candidates")
	}
	for _, level := range add {
		if level["probability"] != nil {
			t.Errorf("add level fabricated option probability: %#v", level["probability"])
		}
	}
}

func TestIndexSwingCandidatesAreNotAddedToStocks(t *testing.T) {
	bars := make([]map[string]any, 15)
	for index := range bars {
		close := 100 + 4*math.Sin(float64(index)*math.Pi/3)
		bars[index] = map[string]any{"high": close + 1, "low": close - 1, "close": close, "volume": 0}
	}
	spot := 100.0
	containsSwing := func(levels []cachedLevel) bool {
		for _, level := range levels {
			if level.factor == "历史摆动高点" || level.factor == "历史摆动低点" {
				return true
			}
		}
		return false
	}
	indexLevels := buildCachedLevelsAt("^NDX", "", bars, nil, model.Quote{Symbol: "^NDX"}, &spot, &spot, time.Now(), map[string]any{}, map[string]any{}, nil, time.Now(), map[string]any{})
	stockLevels := buildCachedLevelsAt("QQQ", "", bars, nil, model.Quote{Symbol: "QQQ"}, &spot, &spot, time.Now(), map[string]any{}, map[string]any{}, nil, time.Now(), map[string]any{})
	indexCandidates := make([]cachedLevel, 0)
	for _, side := range []string{"resistance", "support"} {
		items, _ := indexLevels[side].([]map[string]any)
		for _, item := range items {
			for _, factor := range cachedFactorStrings(item["factors"]) {
				indexCandidates = append(indexCandidates, cachedLevel{factor: factor})
			}
		}
	}
	stockCandidates := make([]cachedLevel, 0)
	for _, side := range []string{"resistance", "support"} {
		items, _ := stockLevels[side].([]map[string]any)
		for _, item := range items {
			for _, factor := range cachedFactorStrings(item["factors"]) {
				stockCandidates = append(stockCandidates, cachedLevel{factor: factor})
			}
		}
	}
	if !containsSwing(indexCandidates) {
		t.Fatal("index candidates do not include swing levels")
	}
	if containsSwing(stockCandidates) {
		t.Fatal("stock candidates unexpectedly include index-only swing levels")
	}
}
