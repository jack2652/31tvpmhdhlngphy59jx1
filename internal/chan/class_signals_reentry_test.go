package chananalysis

import (
	"strings"
	"testing"
)

// 中枢首次向上离开后若重新跌穿中枢，须评估新一次向下离开及首次回抽。
func TestClass3RescansOppositeDepartureAfterReturningThroughCenter(t *testing.T) {
	for _, side := range []string{"sell", "buy"} {
		t.Run(side, func(t *testing.T) {
			level := classThreeFixture()
			last := level.Segments[len(level.Segments)-1]
			level.Segments = append(level.Segments,
				Segment{Level: 0, Chain: 0, Direction: "up", Start: last.End, End: "07", StartPrice: last.EndPrice, EndPrice: 16, Low: last.EndPrice, High: 16, Confirmed: true, ConfirmedAt: "08"},
				Segment{Level: 0, Chain: 0, Direction: "down", Start: "07", End: "09", StartPrice: 16, EndPrice: 4, Low: 4, High: 16, Confirmed: true, ConfirmedAt: "10"},
				Segment{Level: 0, Chain: 0, Direction: "up", Start: "09", End: "11", StartPrice: 4, EndPrice: 4.5, Low: 4, High: 4.5, Confirmed: true, ConfirmedAt: "12"},
			)
			if side == "buy" {
				level, _ = mirrorSignalFixture(level, nil)
			}
			got := class3Signals(Frame{AsOf: "12"}, []LevelAnalysis{level})
			want := findV4Signal(got, "class3_"+side)
			if want == nil || want.Status != "confirmed" || want.Date != "11" || want.ConfirmedAt != "12" {
				t.Fatalf("重新穿越中枢后的反方向三类点遗漏：want class3_%s at 11 confirmed 12; got=%+v", side, got)
			}
		})
	}
}

// 一类点还须比较同一中枢前的进入段与中枢后的离开段，不能只比较两次离开。
func TestClassOneComparesEntryAndExitAroundSameCenter(t *testing.T) {
	for _, side := range []string{"sell", "buy"} {
		t.Run(side, func(t *testing.T) {
			level, strengths := centerEntryExitFixture()
			if side == "buy" {
				level, strengths = mirrorSignalFixture(level, strengths)
			}
			got := buildV4Signals(Frame{AsOf: "13"}, []LevelAnalysis{level}, strengths)
			signal := findV4Signal(got, "class1_"+side)
			if signal == nil || signal.Date != "10" || signal.ObservedAt != "11" || signal.StrengthRatio == nil || !priceEqual(*signal.StrengthRatio, 0.7) {
				t.Fatalf("同中枢进入/离开段背驰遗漏：side=%s signals=%+v", side, got)
			}
		})
	}
}

func centerEntryExitFixture() (LevelAnalysis, []Strength) {
	segments := []Segment{
		{Level: 0, Chain: 0, Direction: "up", Start: "01", End: "02", StartPrice: 4, EndPrice: 12, Low: 4, High: 12, Confirmed: true, ConfirmedAt: "03"},
		{Level: 0, Chain: 0, Direction: "down", Start: "02", End: "04", StartPrice: 12, EndPrice: 7, Low: 7, High: 12, Confirmed: true, ConfirmedAt: "05"},
		{Level: 0, Chain: 0, Direction: "up", Start: "04", End: "06", StartPrice: 7, EndPrice: 10, Low: 7, High: 10, Confirmed: true, ConfirmedAt: "07"},
		{Level: 0, Chain: 0, Direction: "down", Start: "06", End: "08", StartPrice: 10, EndPrice: 6, Low: 6, High: 10, Confirmed: true, ConfirmedAt: "09"},
		{Level: 0, Chain: 0, Direction: "up", Start: "08", End: "10", StartPrice: 6, EndPrice: 14, Low: 6, High: 14, Confirmed: true, ConfirmedAt: "11"},
		{Level: 0, Chain: 0, Direction: "down", Start: "10", End: "12", StartPrice: 14, EndPrice: 11, Low: 11, High: 14, Confirmed: true, ConfirmedAt: "13"},
	}
	center := RecursiveCenter{Level: 0, Chain: 0, CenterID: "entry-exit-center", Start: "02", End: "10", Lower: 7, Upper: 10, StartSegment: 1, EndSegment: 4, FormedAt: "09", ConfirmedAt: "13"}
	strengths := []Strength{
		{SourceKind: "pen", Level: 0, Direction: "up", Start: "01", End: "02", MACDArea: 10, WarmupComplete: true},
		{SourceKind: "pen", Level: 0, Direction: "up", Start: "08", End: "10", MACDArea: 7, WarmupComplete: true},
	}
	return LevelAnalysis{Level: 0, SourceKind: "pen", Status: "ready", Segments: segments, Centers: []RecursiveCenter{center}}, strengths
}

// 端点分型可观察与整笔固定是两个时点，未来的确认不能提前进入状态。
func TestCenterEntryExitObservationRequiresVisibleFractal(t *testing.T) {
	for _, side := range []string{"sell", "buy"} {
		t.Run(side, func(t *testing.T) {
			level, strengths := centerEntryExitFixture()
			if side == "buy" {
				level, strengths = mirrorSignalFixture(level, strengths)
			}
			current := &level.Segments[4]
			current.Confirmed, current.Forming, current.ConfirmedAt = false, true, ""
			frame := Frame{AsOf: "11", StrokesV4: make([]Stroke, len(level.Segments))}
			if got := buildCenterEntryExitSignals(frame, []LevelAnalysis{level}, strengths, .8); len(got) != 0 {
				t.Fatalf("未确认分型不应生成观察信号：%+v", got)
			}
			frame.StrokesV4[4] = Stroke{Chain: current.Chain, End: Fractal{Date: current.End, Price: current.EndPrice, ConfirmedAt: "11"}}
			assertPhase := func(wantStatus, wantConfirmed string) {
				t.Helper()
				got := buildCenterEntryExitSignals(frame, []LevelAnalysis{level}, strengths, .8)
				if len(got) != 1 || got[0].Kind != "class1_"+side || got[0].Status != wantStatus || got[0].ObservedAt != "11" || got[0].ConfirmedAt != wantConfirmed {
					t.Fatalf("观察/确认时间错误：asOf=%s got=%+v", frame.AsOf, got)
				}
			}
			assertPhase("observing", "")
			current.Confirmed, current.Forming, current.ConfirmedAt = true, false, "13"
			assertPhase("observing", "")
			frame.AsOf = "13"
			assertPhase("divergence_confirmed", "13")
			frame.AsOf = "10"
			if got := buildCenterEntryExitSignals(frame, []LevelAnalysis{level}, strengths, .8); len(got) != 0 {
				t.Fatalf("未来分型被提前使用：%+v", got)
			}
		})
	}
}

func TestCenterEntryExitRejectsInvalidComparisonBackground(t *testing.T) {
	tests := []struct {
		name   string
		change func(*LevelAnalysis, []Strength)
	}{
		{"equal extreme", func(l *LevelAnalysis, _ []Strength) { l.Segments[4].EndPrice = l.Segments[0].EndPrice }},
		{"threshold equality", func(_ *LevelAnalysis, s []Strength) { s[1].MACDArea = 8 }},
		{"warmup incomplete", func(_ *LevelAnalysis, s []Strength) { s[0].WarmupComplete = false }},
		{"zero entry area", func(_ *LevelAnalysis, s []Strength) { s[0].MACDArea = 0 }},
		{"different chain", func(l *LevelAnalysis, _ []Strength) { l.Segments[4].Chain = 1 }},
		{"price gap", func(l *LevelAnalysis, _ []Strength) { l.Segments[4].StartPrice++ }},
		{"future center", func(l *LevelAnalysis, _ []Strength) { l.Centers[0].FormedAt = "12" }},
		{"future center component", func(l *LevelAnalysis, _ []Strength) { l.Segments[3].ConfirmedAt = "12" }},
		{"unconfirmed entry", func(l *LevelAnalysis, _ []Strength) { l.Segments[0].Confirmed = false }},
		{"superseded center", func(l *LevelAnalysis, _ []Strength) {
			l.Centers = append(l.Centers, RecursiveCenter{Level: 0, Chain: 0, StartSegment: 3, FormedAt: "11"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			level, strengths := centerEntryExitFixture()
			tt.change(&level, strengths)
			if got := buildCenterEntryExitSignals(Frame{AsOf: "13"}, []LevelAnalysis{level}, strengths, .8); len(got) != 0 {
				t.Fatalf("无效背景产生背驰信号：%+v", got)
			}
		})
	}
	level, strengths := centerEntryExitFixture()
	level.Centers = append(level.Centers, RecursiveCenter{Level: 0, Chain: 0, StartSegment: 3, FormedAt: "12"})
	if got := buildCenterEntryExitSignals(Frame{AsOf: "13"}, []LevelAnalysis{level}, strengths, .8); len(got) != 1 {
		t.Fatalf("未来形成的新中枢不能抹去先前可观察信号：%+v", got)
	}
}

func TestCenterEntryExitInvalidationKeepsOnlyEarlierConfirmation(t *testing.T) {
	for _, invalidatedAt := range []string{"12", "13", "14"} {
		t.Run(invalidatedAt, func(t *testing.T) {
			level, strengths := centerEntryExitFixture()
			level.Segments[4].ConfirmedAt = "13"
			frame := Frame{AsOf: "14", StrokesV4: make([]Stroke, 5), Candles: []Bar{{Date: invalidatedAt, Low: 11, High: 14}}}
			frame.StrokesV4[4] = Stroke{End: Fractal{Date: "10", Price: 14, ConfirmedAt: "11"}}
			got := buildCenterEntryExitSignals(frame, []LevelAnalysis{level}, strengths, .8)
			wantConfirmed := ""
			if invalidatedAt == "14" {
				wantConfirmed = "13"
			}
			if len(got) != 1 || got[0].Status != "invalidated" || got[0].PhaseAt != invalidatedAt || got[0].ConfirmedAt != wantConfirmed {
				t.Fatalf("失效先后顺序错误：%+v", got)
			}
			for _, phase := range got[0].PhaseHistory[:len(got[0].PhaseHistory)-1] {
				if phase.At >= invalidatedAt {
					t.Fatalf("失效后的确认未移除：%+v", got[0])
				}
			}
		})
	}
}

func TestClass3DoesNotRepeatOutsideContinuation(t *testing.T) {
	level := classThreeFixture()
	level.Segments = append(level.Segments,
		Segment{Level: 0, Direction: "up", Start: "06", End: "07", StartPrice: 12, EndPrice: 16, Low: 12, High: 16, Confirmed: true, ConfirmedAt: "08"},
		Segment{Level: 0, Direction: "down", Start: "07", End: "09", StartPrice: 16, EndPrice: 13, Low: 13, High: 16, Confirmed: true, ConfirmedAt: "10"},
	)
	if got := class3Signals(Frame{AsOf: "10"}, []LevelAnalysis{level}); len(got) != 1 || got[0].Date != "06" {
		t.Fatalf("中枢外延续重复生成三买：%+v", got)
	}
}

func TestClass3RejectsFutureRetestAndLabelsFormingDeparture(t *testing.T) {
	level := classThreeFixture()
	level.Segments[4].Confirmed, level.Segments[4].ConfirmedAt = false, ""
	if got := class3Signals(Frame{AsOf: "05"}, []LevelAnalysis{level}); len(got) != 0 {
		t.Fatalf("未来回试不可用：%+v", got)
	}
	level.Segments = level.Segments[:4]
	level.Segments[3].Confirmed, level.Segments[3].ConfirmedAt = false, ""
	got := class3Signals(Frame{AsOf: "05"}, []LevelAnalysis{level})
	if len(got) != 1 || got[0].ObservedAt != "05" || got[0].ConfirmedAt != "" || !strings.Contains(got[0].Detail, "尚未确认") || len(got[0].Missing) != 2 {
		t.Fatalf("形成中离开误称已确认：%+v", got)
	}
}

// 高阶中枢可属于同一条长走势，新中枢形成后不沿用数年前的中枢预警。
func TestClass3SupersededRecursiveCenterDoesNotEmitStaleWaitingSignal(t *testing.T) {
	level := LevelAnalysis{Level: 1, Segments: []Segment{{Level: 1, Chain: 0, Direction: "up", Start: "01", End: "09", StartPrice: 9, EndPrice: 16, Low: 9, High: 16}}, Centers: []RecursiveCenter{
		{Level: 1, Chain: 0, Lower: 5, Upper: 10, End: "02", ConfirmedAt: "03", OwnerMoveIndex: 0},
		{Level: 1, Chain: 0, Lower: 12, Upper: 14, End: "08", FormedAt: "08", Forming: true, StartSegment: 7, OwnerMoveIndex: 0},
	}}
	if got := class3Signals(Frame{AsOf: "10"}, []LevelAnalysis{level}); len(got) != 0 {
		t.Fatalf("形成新中枢后仍使用旧中枢：%+v", got)
	}
	level.Segments[0].End, level.Segments[0].Confirmed, level.Segments[0].ConfirmedAt = "04", true, "05"
	if got := class3Signals(Frame{AsOf: "10"}, []LevelAnalysis{level}); len(got) != 1 || got[0].ObservedAt != "05" {
		t.Fatalf("后来的新中枢抹去更早历史事件：%+v", got)
	}
}
