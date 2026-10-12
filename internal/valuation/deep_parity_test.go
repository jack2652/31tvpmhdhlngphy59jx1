package valuation

import (
	"strings"
	"testing"
)

func TestStorageHardwareWithoutAnalystEPSFallsBackWithoutCyclicalModel(t *testing.T) {
	result := Calculate(Fundamentals{
		Sector: "Technology", Industry: "Computer Hardware", IndustryKey: "computer-hardware",
		LongBusinessSummary: "Makes hard disk enterprise storage systems.",
		TrailingEPS:         ptr(10), NormalizedEPS: ptr(10), GrowthRate: ptr(.20),
		FreeCashFlow: ptr(200), FreeCashFlowPerShare: ptr(20), OwnerEarnings: ptr(120),
		SharesOutstanding: ptr(10), Revenue: ptr(1000),
	})

	if result.Status != StatusReady {
		t.Fatalf("status=%q, want a ready generic fallback: %#v", result.Status, result)
	}
	if result.Method == "周期中值 EPS × PE" {
		t.Fatalf("method=%q, Python clears storage-cycle classification without analyst EPS", result.Method)
	}
	if result.ForwardEPS != nil {
		t.Fatalf("forward EPS=%v, want nil without an analyst estimate", result.ForwardEPS)
	}
	if !strings.Contains(result.Warning, "缺乏分析师远期 EPS") {
		t.Fatalf("warning=%q, want the missing analyst EPS downgrade warning", result.Warning)
	}
}
