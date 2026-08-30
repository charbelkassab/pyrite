package engine

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestCostScanRunsEveryLevel(t *testing.T) {
	// A strategy that trades every day is exactly the case costs decide.
	spec := baseSpec(`
		function onDay(ctx) {
			if (ctx.dayIndex % 2 === 0) ctx.buy("AAPL", { pctCash: 0.9 });
			else ctx.close("AAPL");
		}
	`)
	spec.Universe = []string{"AAPL"}

	scan, err := RunCostScan(context.Background(), spec, newTestStore(t), nil)
	if err != nil {
		t.Fatalf("cost scan: %v", err)
	}
	if len(scan.Points) != len(DefaultCostLevels) {
		t.Fatalf("want %d points, got %d", len(DefaultCostLevels), len(scan.Points))
	}
	for i, p := range scan.Points {
		if p.Error != "" {
			t.Fatalf("level %v failed: %s", p.SlippageBps, p.Error)
		}
		if p.SlippageBps != DefaultCostLevels[i] {
			t.Errorf("points must stay in the order requested: %v at index %d", p.SlippageBps, i)
		}
	}

	// Higher friction must cost more and return less. A daily-turnover
	// strategy that did not degrade would mean the cost model is not wired in.
	zero, fifty := scan.Points[0], scan.Points[len(scan.Points)-1]
	if !(fifty.TotalCosts > zero.TotalCosts) {
		t.Errorf("costs did not rise with slippage: %v then %v", zero.TotalCosts, fifty.TotalCosts)
	}
	if !(fifty.TotalReturn < zero.TotalReturn) {
		t.Errorf("return did not fall with slippage: %v then %v", zero.TotalReturn, fifty.TotalReturn)
	}
	if scan.Verdict == "" {
		t.Error("a verdict should always be written")
	}
}

func TestCostScanZeroFrictionIsFree(t *testing.T) {
	spec := baseSpec(`
		function onDay(ctx) {
			if (ctx.dayIndex === 0) ctx.buy("AAPL", { pctCash: 0.9 });
		}
	`)
	spec.Universe = []string{"AAPL"}
	scan, err := RunCostScan(context.Background(), spec, newTestStore(t), []float64{0})
	if err != nil {
		t.Fatalf("cost scan: %v", err)
	}
	if got := scan.Points[0].TotalCosts; math.Abs(got) > 1e-9 {
		t.Errorf("zero slippage and no commission should cost nothing, got %v", got)
	}
}

func TestBreakEvenInterpolates(t *testing.T) {
	// Return crosses zero halfway between 10 and 20 bps.
	pts := []CostPoint{
		{SlippageBps: 0, TotalReturn: 0.20},
		{SlippageBps: 10, TotalReturn: 0.05},
		{SlippageBps: 20, TotalReturn: -0.05},
	}
	got := breakEven(pts)
	if !got.Defined() {
		t.Fatal("break-even should be defined when return crosses zero")
	}
	if math.Abs(float64(got)-15) > 1e-9 {
		t.Errorf("break-even: got %v, want 15", float64(got))
	}
}

func TestBreakEvenUndefinedWhenNeverCrossed(t *testing.T) {
	pts := []CostPoint{
		{SlippageBps: 0, TotalReturn: 0.20},
		{SlippageBps: 50, TotalReturn: 0.15},
	}
	if breakEven(pts).Defined() {
		t.Error("a strategy that stays profitable has no break-even inside the range")
	}
	// And one that never made money at all.
	pts = []CostPoint{
		{SlippageBps: 0, TotalReturn: -0.1},
		{SlippageBps: 50, TotalReturn: -0.3},
	}
	if breakEven(pts).Defined() {
		t.Error("a strategy that never made money has no break-even to find")
	}
}

func TestCostVerdictNamesTheArtefact(t *testing.T) {
	s := &CostScan{
		Points: []CostPoint{
			{SlippageBps: 0, TotalReturn: 0.8},
			{SlippageBps: 5, TotalReturn: -0.1},
			{SlippageBps: 20, TotalReturn: -0.5},
		},
	}
	s.BreakEvenBps = breakEven(s.Points)
	v := costVerdict(s)
	if !strings.Contains(v, "artefact") {
		t.Errorf("an edge that dies below the default charge should be named: %q", v)
	}
}

func TestCostVerdictOnAStrategyThatNeverWorked(t *testing.T) {
	s := &CostScan{Points: []CostPoint{
		{SlippageBps: 0, TotalReturn: -0.2},
		{SlippageBps: 50, TotalReturn: -0.4},
	}}
	s.BreakEvenBps = breakEven(s.Points)
	if !strings.Contains(costVerdict(s), "costs are not the problem") {
		t.Errorf("should say costs are not the issue: %q", costVerdict(s))
	}
}

func TestNumberHelpers(t *testing.T) {
	if got := fmtInt(0); got != "0" {
		t.Errorf("fmtInt(0) = %q", got)
	}
	if got := fmtInt(-1234); got != "-1234" {
		t.Errorf("fmtInt(-1234) = %q", got)
	}
	if got := fmtFloat1(12.34); got != "12.3" {
		t.Errorf("fmtFloat1(12.34) = %q", got)
	}
	// Rounding up must carry into the whole part.
	if got := fmtFloat1(9.98); got != "10.0" {
		t.Errorf("fmtFloat1(9.98) = %q, want 10.0", got)
	}
	if got := trimFloat(15); got != "15" {
		t.Errorf("trimFloat(15) = %q, want a bare integer", got)
	}
	if got := fmtPercent(0.567); got != "57%" {
		t.Errorf("fmtPercent(0.567) = %q", got)
	}
}
