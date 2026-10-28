package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	jsoniter "github.com/json-iterator/go"
)

func BenchmarkYahooLargeOptionsDecode(b *testing.B) {
	const contract = `{"contractSymbol":"QQQ300118C00450000","strike":450,"lastPrice":12.5,"bid":12,"ask":13,"percentChange":2.5,"volume":100,"openInterest":250,"impliedVolatility":0.32,"inTheMoney":true,"gamma":0.04}`
	var contracts strings.Builder
	contracts.Grow(len(contract)*4000 + 3999)
	for i := 0; i < 4000; i++ {
		if i > 0 {
			contracts.WriteByte(',')
		}
		contracts.WriteString(contract)
	}
	body := fmt.Sprintf(`{"optionChain":{"result":[{"expirationDates":[1895011200],"options":[{"expirationDate":1895011200,"calls":[%s],"puts":[%s]}]}],"error":null}}`, contracts.String(), contracts.String())
	decodeStreaming := func() (yahooOptionExtensions, error) {
		var payload yahooOptionExtensions
		decoder := json.NewDecoder(strings.NewReader(body))
		if err := decoder.Decode(&payload); err != nil {
			return payload, err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				return payload, fmt.Errorf("unexpected trailing JSON")
			}
			return payload, err
		}
		return payload, nil
	}
	decodeBuffered := func() (yahooOptionExtensions, error) {
		var payload yahooOptionExtensions
		err := json.Unmarshal([]byte(body), &payload)
		return payload, err
	}
	decodeJSONIter := func() (yahooOptionExtensions, error) {
		var payload yahooOptionExtensions
		err := jsoniter.ConfigCompatibleWithStandardLibrary.UnmarshalFromString(body, &payload)
		return payload, err
	}
	for _, decoder := range []struct {
		name string
		fn   func() (yahooOptionExtensions, error)
	}{{"decoder", decodeStreaming}, {"unmarshal", decodeBuffered}, {"jsoniter", decodeJSONIter}} {
		for _, includeRows := range []bool{false, true} {
			name := decoder.name + "/decode"
			if includeRows {
				name = decoder.name + "/decode_and_rows"
			}
			b.Run(name, func(b *testing.B) {
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					payload, err := decoder.fn()
					if err != nil {
						b.Fatal(err)
					}
					if includeRows {
						rows := yahooOptionRows(payload.OptionChain.Result[0].Options[0], "2030-01-18")
						if len(rows) != 8000 {
							b.Fatalf("decoded rows=%d, want 8000", len(rows))
						}
					}
				}
			})
		}
	}
}
