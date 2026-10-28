package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type cboeBenchmarkTransport []byte

func (t cboeBenchmarkTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(t)),
		Request:    request,
	}, nil
}

func BenchmarkCboeLargeResponseDecode(b *testing.B) {
	const row = `{"option":"QQQ300118C00450000","last_trade_price":12.5,"bid":12,"ask":13,"volume":100,"open_interest":250,"iv":0.32,"gamma":0.04}`
	options := strings.TrimSuffix(strings.Repeat(row+",", 80000), ",")
	body := []byte(`{"data":{"options":[` + options + `]}}`)
	client := NewCboeOptionsClient(&http.Client{Transport: cboeBenchmarkTransport(body)}, "https://cboe.invalid/{symbol}")
	gate := make(chan struct{}, 1)
	ctx := context.Background()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		payload, err := client.fetchUncached(ctx, "QQQ", gate)
		if err != nil {
			b.Fatal(err)
		}
		_ = payload
	}
}
