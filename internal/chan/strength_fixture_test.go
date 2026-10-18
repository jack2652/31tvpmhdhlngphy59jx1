package chananalysis

import (
	"fmt"
	"math"
	"testing"
)

func TestMACDStrengthFixturesCoverZeroAxisAndDirectionalAreas(t *testing.T) {
	bars := make([]Bar, 90)
	price := 100.0
	for index := range bars {
		switch {
		case index >= 35 && index < 55:
			price += 1
		case index >= 55:
			price -= 1
		}
		bars[index] = Bar{Date: fmt.Sprintf("%03d", index), Open: price, High: price + 1, Low: price - 1, Close: price}
	}
	points := macdSeries(bars)
	if points[10].Histogram != 0 || !points[10].Warmup {
		t.Fatalf("flat zero-axis fixture=%+v, want zero histogram during warmup", points[10])
	}
	positiveIndex, negativeIndex := -1, -1
	for index, point := range points {
		if point.Histogram > 0 && positiveIndex < 0 {
			positiveIndex = index
		}
		if point.Histogram < 0 && negativeIndex < 0 {
			negativeIndex = index
		}
	}
	if positiveIndex < macdWarmupBars || negativeIndex <= positiveIndex {
		t.Fatalf("MACD zero-axis crossings positive=%d negative=%d", positiveIndex, negativeIndex)
	}

	segments := []Segment{
		{Direction: "up", Start: bars[35].Date, End: bars[54].Date, StartPrice: bars[35].Close, EndPrice: bars[54].Close, Low: bars[35].Low, High: bars[54].High, Confirmed: true},
		{Direction: "down", Start: bars[55].Date, End: bars[89].Date, StartPrice: bars[55].Close, EndPrice: bars[89].Close, Low: bars[89].Low, High: bars[55].High, Confirmed: true},
	}
	strengths := buildStrengths(bars, []LevelAnalysis{{Level: 0, Segments: segments}})
	if len(strengths) != 2 || strengths[0].MACDArea <= 0 || strengths[1].MACDArea <= 0 {
		t.Fatalf("directional MACD areas=%+v, want positive up/down area on either side of zero", strengths)
	}
	upArea, downArea := 0.0, 0.0
	for index := 35; index <= 54; index++ {
		upArea += math.Max(0, points[index].Histogram)
	}
	for index := 55; index <= 89; index++ {
		downArea += math.Max(0, -points[index].Histogram)
	}
	if math.Abs(strengths[0].MACDArea-upArea) > 1e-12 || math.Abs(strengths[1].MACDArea-downArea) > 1e-12 {
		t.Fatalf("strength areas=%+v, want up=%g down=%g", strengths, upArea, downArea)
	}
}

func TestV4DivergenceRejectsPriceEndpointWithoutNewExtreme(t *testing.T) {
	segments := []Segment{
		{Direction: "up", Chain: 0, Level: 0, Start: "01", End: "02", StartPrice: 4, EndPrice: 11, Low: 4, High: 12, Confirmed: true, ConfirmedAt: "02"},
		{Direction: "down", Chain: 0, Level: 0, Start: "02", End: "03", StartPrice: 11, EndPrice: 7, Low: 5, High: 11, Confirmed: true, ConfirmedAt: "03"},
		{Direction: "up", Chain: 0, Level: 0, Start: "03", End: "04", StartPrice: 7, EndPrice: 9, Low: 6, High: 10, Confirmed: true, ConfirmedAt: "04"},
		{Direction: "down", Chain: 0, Level: 0, Start: "04", End: "05", StartPrice: 9, EndPrice: 6, Low: 4, High: 9, Confirmed: true, ConfirmedAt: "05"},
		{Direction: "up", Chain: 0, Level: 0, Start: "05", End: "06", StartPrice: 6, EndPrice: 14, Low: 6, High: 14, Confirmed: true, ConfirmedAt: "06"},
		{Direction: "down", Chain: 0, Level: 0, Start: "06", End: "07", StartPrice: 14, EndPrice: 12, Low: 11, High: 14, Confirmed: true, ConfirmedAt: "07"},
		{Direction: "up", Chain: 0, Level: 0, Start: "07", End: "08", StartPrice: 12, EndPrice: 14, Low: 12, High: 14, Confirmed: true, ConfirmedAt: "08"},
	}
	center := RecursiveCenter{Level: 0, Chain: 0, Lower: 5, Upper: 10, Start: "01", End: "04", FormedAt: "04", ConfirmedAt: "05", EndSegment: 2}
	strengths := []Strength{
		{Direction: "up", Level: 0, Start: "05", End: "06", MACDArea: 10, WarmupComplete: true},
		{Direction: "up", Level: 0, Start: "07", End: "08", MACDArea: 7, WarmupComplete: true},
	}
	levels := []LevelAnalysis{{Level: 0, Segments: segments, Centers: []RecursiveCenter{center}}}
	got := buildV4Signals(Frame{}, levels, strengths)
	for _, signal := range got {
		if signal.Kind == "class1_sell" {
			t.Fatalf("signals=%+v, want no divergence because the second push did not make a strict new high", got)
		}
	}
}

func TestV4DivergenceThresholdIsAnExplicitVersionedParameter(t *testing.T) {
	parameters := RecursiveParameters()
	if parameters["divergence_ratio"] != recursiveDivergenceRatio || parameters["divergence_threshold_source"] != "project-screening-parameter" {
		t.Fatalf("divergence parameters=%#v, want explicit project parameter and its source", parameters)
	}
	segments := []Segment{
		{Direction: "up", Chain: 0, Level: 0, Start: "01", End: "02", StartPrice: 4, EndPrice: 11, Low: 4, High: 12, Confirmed: true, ConfirmedAt: "02"},
		{Direction: "down", Chain: 0, Level: 0, Start: "02", End: "03", StartPrice: 11, EndPrice: 7, Low: 5, High: 11, Confirmed: true, ConfirmedAt: "03"},
		{Direction: "up", Chain: 0, Level: 0, Start: "03", End: "04", StartPrice: 7, EndPrice: 9, Low: 6, High: 10, Confirmed: true, ConfirmedAt: "04"},
		{Direction: "down", Chain: 0, Level: 0, Start: "04", End: "05", StartPrice: 9, EndPrice: 6, Low: 4, High: 9, Confirmed: true, ConfirmedAt: "05"},
		{Direction: "up", Chain: 0, Level: 0, Start: "05", End: "06", StartPrice: 6, EndPrice: 14, Low: 6, High: 14, Confirmed: true, ConfirmedAt: "06"},
		{Direction: "down", Chain: 0, Level: 0, Start: "06", End: "07", StartPrice: 14, EndPrice: 12, Low: 11, High: 14, Confirmed: true, ConfirmedAt: "07"},
		{Direction: "up", Chain: 0, Level: 0, Start: "07", End: "08", StartPrice: 12, EndPrice: 16, Low: 12, High: 16, Confirmed: true, ConfirmedAt: "08"},
	}
	center := RecursiveCenter{Level: 0, Chain: 0, Lower: 5, Upper: 10, Start: "01", End: "04", FormedAt: "04", ConfirmedAt: "05", EndSegment: 2}
	levels := []LevelAnalysis{{Level: 0, Status: "ready", Segments: segments, Centers: []RecursiveCenter{center}}}
	strengths := []Strength{{Direction: "up", Level: 0, Start: "05", End: "06", MACDArea: 10, WarmupComplete: true}, {Direction: "up", Level: 0, Start: "07", End: "08", MACDArea: 8, WarmupComplete: true}}
	if signals := buildV4SignalsWithDivergenceRatio(Frame{}, levels, strengths, 0.8); hasSignalKind(signals, "class1_sell") {
		t.Fatal("ratio exactly at the strict threshold must not trigger")
	}
	if signals := buildV4SignalsWithDivergenceRatio(Frame{}, levels, strengths, 0.81); !hasSignalKind(signals, "class1_sell") {
		t.Fatal("the same ratio should trigger when the configured threshold is 0.81")
	}
	if signals := buildV4SignalsWithDivergenceRatio(Frame{}, levels, strengths, 1); len(signals) != 0 {
		t.Fatalf("invalid thresholds must not emit signals: %+v", signals)
	}
}

func hasSignalKind(signals []V4Signal, kind string) bool {
	for _, signal := range signals {
		if signal.Kind == kind {
			return true
		}
	}
	return false
}
