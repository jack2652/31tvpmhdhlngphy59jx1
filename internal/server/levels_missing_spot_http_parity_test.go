package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestLevelsHTTPMissingSpotMatchesPythonBuildLevelsFixture(t *testing.T) {
	_, ts, _ := newAPIRemainingParityService(t, &testMarketProvider{})
	response, err := http.Get(ts.URL + "/api/levels/QQQ?expiration=2099-01-16")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	var got map[string]any
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("missing-spot levels response status=%d decode=%v body=%#v", response.StatusCode, err, got)
	}

	// Python app/levels.py returns this unavailable-analysis payload when neither
	// the requested spot nor the last history close is a valid positive price.
	pythonBuildLevelsFixture := map[string]any{
		"spot":                 nil,
		"resistance":           []any{},
		"support":              []any{},
		"history_bars":         float64(0),
		"options_metric":       "none",
		"trend":                nil,
		"recommendation":       map[string]any{"action": "hold", "label": "继续持有", "reason": "缺少有效价格数据"},
		"trade_points":         map[string]any{"buy": nil, "sell": nil},
		"stop_loss":            nil,
		"trade_points_horizon": map[string]any{"trading_days": float64(5), "label": "未来 5 个交易日"},
		"buyer_structures": map[string]any{
			"available": false, "status": "unavailable", "items": []any{},
			"horizon_trading_days": float64(5), "horizon_label": "未来 5 个交易日",
			"reason": "缺少有效价格数据",
		},
		"extremes": nil,
		"plan":     map[string]any{"buy": []any{}, "add": []any{}, "sell": []any{}},
	}
	for field, want := range pythonBuildLevelsFixture {
		if !reflect.DeepEqual(got[field], want) {
			t.Errorf("Python build_levels field %q = %#v, want %#v", field, got[field], want)
		}
	}
	for _, field := range []string{"candidate_spot", "history_validation"} {
		if _, exists := got[field]; exists {
			t.Errorf("Python early build_levels result does not include %q: %#v", field, got)
		}
	}
	if got["symbol"] != "QQQ" || got["expiration"] != "2099-01-16" {
		t.Errorf("unavailable levels identity = (%v, %v), want (QQQ, 2099-01-16)", got["symbol"], got["expiration"])
	}
}
