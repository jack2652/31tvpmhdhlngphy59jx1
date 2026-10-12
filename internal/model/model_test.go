package model

import (
	"encoding/json"
	"testing"
)

func TestOptionRowJSONPreservesNullableInTheMoney(t *testing.T) {
	tests := []struct {
		name string
		row  OptionRow
		want any
	}{
		{name: "unknown", row: OptionRow{ContractSymbol: "UNKNOWN"}, want: nil},
		{name: "false", row: OptionRow{ContractSymbol: "OUT", InTheMoneyKnown: true}, want: false},
		{name: "true", row: OptionRow{ContractSymbol: "IN", InTheMoney: true, InTheMoneyKnown: true}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(test.row)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(payload, &decoded); err != nil {
				t.Fatal(err)
			}
			if got, present := decoded["in_the_money"]; !present || got != test.want {
				t.Fatalf("in_the_money = %#v (present=%v), want %#v; JSON=%s", got, present, test.want, payload)
			}
			if decoded["contract_symbol"] != test.row.ContractSymbol {
				t.Fatalf("contract_symbol = %#v, JSON=%s", decoded["contract_symbol"], payload)
			}
		})
	}
}

func TestQuoteSQLiteMetadataDoesNotAppearInModelJSON(t *testing.T) {
	id := "{\"price\":100}"
	methods := "{\"owner_earnings\":120}"
	historical := "{\"p50\":95}"
	payload, err := json.Marshal(Quote{
		SnapshotID: 17, RawJSON: &id, FairValueMethodsJSON: &methods,
		FairValueHistoricalValuationJSON: &historical, Symbol: "QQQ",
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "raw_json", "fair_value_methods_json", "fair_value_historical_valuation_percentiles_json"} {
		if _, present := decoded[field]; present {
			t.Errorf("storage-only field %q leaked from model JSON: %s", field, payload)
		}
	}
}

func TestQuoteJSONOmitsUnsetValuationBreakdowns(t *testing.T) {
	quote := Quote{Symbol: "QQQ"}
	payload, err := json.Marshal(quote)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"fair_value_methods", "fair_value_historical_valuation_percentiles"} {
		if _, present := decoded[field]; present {
			t.Errorf("unset field %q should be omitted to match Python quote responses: %s", field, payload)
		}
	}

	quote.FairValueMethods = map[string]any{"owner_earnings": 120.0}
	quote.FairValueHistoricalValuationPct = map[string]any{"p50": 95.0}
	payload, err = json.Marshal(quote)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"fair_value_methods", "fair_value_historical_valuation_percentiles"} {
		if _, present := decoded[field]; !present {
			t.Errorf("populated field %q was omitted: %s", field, payload)
		}
	}
}
