package chananalysis

import "testing"

// 已形成中枢仍在延伸时，反弹失败与买方镜像都应产生独立预警。
func TestStructureWarningBreakoutRetestDirectionsAndConfirmation(t *testing.T) {
	for _, side := range []string{"sell", "buy"} {
		t.Run(side, func(t *testing.T) {
			level := warningRetestFixture()
			kind := "rebound_failure_sell"
			if side == "buy" {
				level, _ = mirrorSignalFixture(level, nil)
				kind = "pullback_hold_buy"
			}
			frame := warningFixtureFrame(level, 5, "13")
			get := func() *V4Signal {
				return findV4Signal(buildStructureWarnings(frame, []LevelAnalysis{level}, nil, .8), kind)
			}
			got := get()
			if got == nil || got.Status != "observing" || got.ObservedAt != "13" || got.ConfirmedAt != "" || got.Side != side {
				t.Fatalf("预警观察错误：%+v", got)
			}
			frame.AsOf = "15"
			got = get()
			if got == nil || got.Status != "warning_confirmed" || got.ConfirmedAt != "15" || got.ObservedAt != "13" {
				t.Fatalf("预警确认错误：%+v", got)
			}
			frame.Candles = []Bar{{Date: "16", High: level.Segments[5].EndPrice, Low: level.Segments[5].EndPrice}}
			frame.AsOf = "16"
			got = get()
			if got == nil || got.Status != "invalidated" || got.ConfirmedAt != "15" || got.PhaseAt != "16" {
				t.Fatalf("历史确认/失效错误：%+v", got)
			}
			frame.Candles[0].Date = "14"
			got = get()
			if got == nil || got.Status != "invalidated" || got.ConfirmedAt != "" || len(got.PhaseHistory) != 2 {
				t.Fatalf("失效后错误保留未来确认：%+v", got)
			}
		})
	}
}

func TestStructureWarningContinuedRepairUsesOriginalBreakoutAnchor(t *testing.T) {
	level := warningRetestFixture()
	level.Segments[5].ConfirmedAt = "13"
	level.Segments = append(level.Segments, Segment{Level: 0, Direction: "up", Start: "14", End: "16", StartPrice: 3, EndPrice: 9.5, Low: 3, High: 9.5, Confirmed: true, ConfirmedAt: "19"})
	frame := warningFixtureFrame(level, 7, "17")
	got := findV4Signal(buildStructureWarnings(frame, []LevelAnalysis{level}, nil, .8), "rebound_failure_sell")
	for _, warning := range buildStructureWarnings(frame, []LevelAnalysis{level}, nil, .8) {
		if warning.Date == "16" {
			got = &warning
		}
	}
	if got == nil || got.Date != "16" || len(got.StructureRefs) != 4 || got.StructureRefs[0] != 4 {
		t.Fatalf("第二次修复未沿用本轮跌破前高点：%+v", got)
	}
	// 前一次反弹触及上沿后旧轮次结束，不能以旧高点继续预警。
	level.Segments[5].High = 10
	for _, warning := range buildStructureWarnings(frame, []LevelAnalysis{level}, nil, .8) {
		if warning.Date == "16" {
			t.Fatalf("已收复上沿却继续沿用旧轮次：%+v", warning)
		}
	}
}

func TestStructureWarningRetestRejectsUnavailableAndInvalidStructure(t *testing.T) {
	tests := []struct {
		name   string
		change func(*LevelAnalysis, *Frame)
	}{
		{"unseen fractal", func(l *LevelAnalysis, f *Frame) {
			l.Segments[5].Confirmed = false
			l.Segments[5].ConfirmedAt = ""
			f.StrokesV4[5].End.ConfirmedAt = ""
		}},
		{"future fractal", func(_ *LevelAnalysis, f *Frame) { f.StrokesV4[5].End.ConfirmedAt = "14" }},
		{"future center", func(l *LevelAnalysis, _ *Frame) { l.Centers[0].FormedAt = "14" }},
		{"future retest confirmation", func(l *LevelAnalysis, _ *Frame) { l.Segments[4].ConfirmedAt = "14" }},
		{"chain gap", func(l *LevelAnalysis, _ *Frame) { l.Segments[4].Chain = 1 }},
		{"price gap", func(l *LevelAnalysis, _ *Frame) { l.Segments[5].StartPrice = 4.1 }},
		{"upper boundary touch", func(l *LevelAnalysis, _ *Frame) { l.Segments[5].High = 10 }},
		{"lower boundary touch", func(l *LevelAnalysis, _ *Frame) { l.Segments[4].EndPrice = 5; l.Segments[5].StartPrice = 5 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			level := warningRetestFixture()
			frame := warningFixtureFrame(level, 5, "13")
			tt.change(&level, &frame)
			if got := findV4Signal(buildStructureWarnings(frame, []LevelAnalysis{level}, nil, .8), "rebound_failure_sell"); got != nil {
				t.Fatalf("无效结构产生预警：%+v", got)
			}
		})
	}
}

func TestStructureWarningLocalDivergenceDirectionsAndThreshold(t *testing.T) {
	for _, side := range []string{"sell", "buy"} {
		t.Run(side, func(t *testing.T) {
			level, strengths := classOneFixture(0)
			level.SourceKind = "pen"
			level.Segments[5].Low, level.Segments[5].EndPrice, level.Segments[6].StartPrice = 9, 9, 9
			level.Segments[6].Low = 9
			level.Segments[6].Confirmed, level.Segments[6].ConfirmedAt = false, ""
			for index := range strengths {
				strengths[index].SourceKind = "pen"
			}
			if side == "buy" {
				level, strengths = mirrorSignalFixture(level, strengths)
			}
			frame := warningFixtureFrame(level, 6, "08")
			kind := "center_divergence_" + side
			if got := findV4Signal(buildStructureWarnings(frame, []LevelAnalysis{level}, strengths, .8), kind); got == nil || got.Status != "observing" || got.StrengthRatio == nil || !priceEqual(*got.StrengthRatio, .79) {
				t.Fatalf("中枢局部背驰预警遗漏：%+v", got)
			}
			strengths[1].MACDArea = 8
			if got := findV4Signal(buildStructureWarnings(frame, []LevelAnalysis{level}, strengths, .8), kind); got != nil {
				t.Fatalf("阈值相等仍产生预警：%+v", got)
			}
			strengths[1].MACDArea = 7.9
			strengths[0].WarmupComplete = false
			if got := findV4Signal(buildStructureWarnings(frame, []LevelAnalysis{level}, strengths, .8), kind); got != nil {
				t.Fatalf("未预热仍产生预警：%+v", got)
			}
		})
	}
}

func TestStructureWarningDoesNotBorrowFractalForRecursiveSegment(t *testing.T) {
	level := warningRetestFixture()
	frame := warningFixtureFrame(level, 5, "13")
	level.SourceKind = "recursive"
	if got := findV4Signal(buildStructureWarnings(frame, []LevelAnalysis{level}, nil, .8), "rebound_failure_sell"); got != nil {
		t.Fatalf("基础线段借笔分型过早观察：%+v", got)
	}
	frame.AsOf = "15"
	if got := findV4Signal(buildStructureWarnings(frame, []LevelAnalysis{level}, nil, .8), "rebound_failure_sell"); got == nil || got.ObservedAt != "15" {
		t.Fatalf("基础线段确认后预警遗漏：%+v", got)
	}
}

func TestStructureWarningStrictSignalPriorityIsPointInTime(t *testing.T) {
	level := warningRetestFixture()
	frame := warningFixtureFrame(level, 5, "13")
	frame.AsOf = "15"
	frame.SignalsV4 = []V4Signal{{SourceKind: "pen", Level: 0, Kind: "class1_sell", Side: "sell", Date: "12", ObservedAt: "13", Price: ptr(9)}}
	if got := findV4Signal(buildStructureWarnings(frame, []LevelAnalysis{level}, nil, .8), "rebound_failure_sell"); got != nil {
		t.Fatalf("同时可见的严格点重复预警：%+v", got)
	}
	frame.SignalsV4[0].ObservedAt = "15"
	if got := findV4Signal(buildStructureWarnings(frame, []LevelAnalysis{level}, nil, .8), "rebound_failure_sell"); got == nil {
		t.Fatal("后来严格点抹去了早期预警")
	}
	frame.SignalsV4[0].ObservedAt, frame.SignalsV4[0].Side = "13", "buy"
	if got := findV4Signal(buildStructureWarnings(frame, []LevelAnalysis{level}, nil, .8), "rebound_failure_sell"); got == nil {
		t.Fatal("相反方向的严格点抹去了预警")
	}
	frame.SignalsV4[0].Side, frame.SignalsV4[0].Price = "sell", ptr(8)
	if got := findV4Signal(buildStructureWarnings(frame, []LevelAnalysis{level}, nil, .8), "rebound_failure_sell"); got == nil {
		t.Fatal("不同价格的严格点抹去了预警")
	}
}

func warningFixtureFrame(level LevelAnalysis, index int, asOf string) Frame {
	frame := Frame{AsOf: asOf, StrokesV4: make([]Stroke, len(level.Segments))}
	current := level.Segments[index]
	frame.StrokesV4[index] = Stroke{Chain: current.Chain, End: Fractal{Date: current.End, Price: current.EndPrice, ConfirmedAt: asOf}}
	return frame
}

func warningRetestFixture() LevelAnalysis {
	return LevelAnalysis{Level: 0, SourceKind: "pen", Centers: []RecursiveCenter{{Level: 0, Chain: 0, CenterID: "warning-center", Lower: 5, Upper: 10, StartSegment: 0, EndSegment: 4, FormedAt: "07", Forming: true}}, Segments: []Segment{
		{Level: 0, Direction: "down", Start: "01", End: "02", StartPrice: 12, EndPrice: 4, Low: 4, High: 12, Confirmed: true, ConfirmedAt: "03"},
		{Level: 0, Direction: "up", Start: "02", End: "04", StartPrice: 4, EndPrice: 10, Low: 4, High: 10, Confirmed: true, ConfirmedAt: "05"},
		{Level: 0, Direction: "down", Start: "04", End: "06", StartPrice: 10, EndPrice: 5, Low: 5, High: 10, Confirmed: true, ConfirmedAt: "07"},
		{Level: 0, Direction: "up", Start: "06", End: "08", StartPrice: 5, EndPrice: 12, Low: 5, High: 12, Confirmed: true, ConfirmedAt: "09"},
		{Level: 0, Direction: "down", Start: "08", End: "10", StartPrice: 12, EndPrice: 4, Low: 4, High: 12, Confirmed: true, ConfirmedAt: "11"},
		{Level: 0, Direction: "up", Start: "10", End: "12", StartPrice: 4, EndPrice: 9, Low: 4, High: 9, Confirmed: true, ConfirmedAt: "15"},
		{Level: 0, Direction: "down", Start: "12", End: "14", StartPrice: 9, EndPrice: 3, Low: 3, High: 9, Confirmed: true, ConfirmedAt: "17"},
	}}
}
