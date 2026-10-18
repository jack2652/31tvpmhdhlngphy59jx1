package chananalysis

import (
	"testing"
	"time"
)

func TestClassifyMarketEnvironmentUsesOnlyAvailableHistory(t *testing.T) {
	input := make([]Bar, 0, 250)
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		input = append(input, Bar{Date: start.AddDate(0, 0, i).Format(time.DateOnly), Close: 100})
	}
	series := ClassifyMarketEnvironment(input)
	if len(series) != 200 || series[198].Status != "unfiltered" || series[199].Regime != "range" || series[199].SMA50 == nil || series[199].SMA200 == nil {
		t.Fatalf("environment warmup/classification = %#v %#v", series[198], series[199])
	}
	first := series[199]
	for i := 0; i < 50; i++ {
		input = append(input, Bar{Date: start.AddDate(0, 0, 200+i).Format(time.DateOnly), Close: float64(101 + i)})
	}
	withFuture := ClassifyMarketEnvironment(input)
	prior := withFuture[199]
	if prior.Date != first.Date || prior.Status != first.Status || prior.Regime != first.Regime || prior.Samples != first.Samples || *prior.Close != *first.Close || *prior.SMA50 != *first.SMA50 || *prior.SMA200 != *first.SMA200 {
		t.Fatalf("future bars changed prior classification: before=%+v after=%+v", first, prior)
	}
	if got := withFuture[249].Regime; got != "bull" {
		t.Fatalf("rising benchmark regime=%q, want bull", got)
	}
}

func TestClassifyMarketEnvironmentBearAndInvalidSamples(t *testing.T) {
	input := make([]Bar, 0, 250)
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		input = append(input, Bar{Date: start.AddDate(0, 0, i).Format(time.DateOnly), Close: 200})
	}
	for i := 0; i < 50; i++ {
		input = append(input, Bar{Date: start.AddDate(0, 0, 200+i).Format(time.DateOnly), Close: float64(199 - i)})
	}
	input = append(input, Bar{Date: "2021-01-01", Close: 0}, Bar{Date: "invalid", Close: 500})
	series := ClassifyMarketEnvironment(input)
	if got := series[len(series)-1].Regime; got != "bear" {
		t.Fatalf("falling benchmark regime=%q, want bear", got)
	}
	if got := MarketEnvironmentAt(series, series[100].Date); got.Regime != "unfiltered" || got.Date != series[100].Date {
		t.Fatalf("early point-in-time regime=%+v", got)
	}
	if got := MarketEnvironmentAt(series, "1900-01-01"); got.Status != "unfiltered" || got.Reason == "" {
		t.Fatalf("missing history environment=%+v", got)
	}
}

func TestMarketEnvironmentSMAWindowIsTrailing200Bars(t *testing.T) {
	input := make([]Bar, 0, 201)
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		close := 100.0
		if i == 0 {
			close = 10000
		}
		input = append(input, Bar{Date: start.AddDate(0, 0, i).Format(time.DateOnly), Close: close})
	}
	input = append(input, Bar{Date: start.AddDate(0, 0, 200).Format(time.DateOnly), Close: 100})
	series := ClassifyMarketEnvironment(input)
	last := series[len(series)-1]
	if last.SMA200 == nil || *last.SMA200 >= 200 {
		t.Fatalf("SMA200 should exclude close older than trailing window: got=%v", last.SMA200)
	}
}
