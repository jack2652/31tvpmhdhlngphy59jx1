package server

import (
	"testing"

	"option-scope-go/internal/model"
)

func TestQuoteAPIValueUsesNullForMissingValuationSource(t *testing.T) {
	payload, ok := quoteAPIValue(model.Quote{Symbol: "QQQ"}).(map[string]any)
	if !ok {
		t.Fatalf("quoteAPIValue() type = %T, want map[string]any", quoteAPIValue(model.Quote{Symbol: "QQQ"}))
	}
	if got := payload["fair_value_source"]; got != nil {
		t.Fatalf("fair_value_source = %#v, want null", got)
	}
}
