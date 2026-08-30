package engine

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestPlanFoldsLaysOutNonOverlappingTests(t *testing.T) {
	// 1000 sessions, 400 train, 100 test, no embargo.
	w := planFolds(1000, 400, 100, 0, false)
	if len(w) == 0 {
		t.Fatal("no folds planned")
	}
	for i, f := range w {
		if f.trainTo-f.trainFrom+1 != 400 {
			t.Errorf("fold %d training window is %d sessions, want 400", i, f.trainTo-f.trainFrom+1)
		}
		if f.testTo-f.testFrom+1 != 100 {
			t.Errorf("fold %d test window is %d sessions, want 100", i, f.testTo-f.testFrom+1)
		}
		if f.testFrom <= f.trainTo {
			t.Errorf("fold %d tests on data it trained on", i)
		}
		if f.testTo >= 1000 {
			t.Errorf("fold %d runs past the end of the data", i)
		}
		if i > 0 && f.testFrom <= w[i-1].testTo {
			t.Errorf("fold %d test window overlaps the previous one", i)
		}
	}
}

func TestPlanFoldsHonoursTheEmbargo(t *testing.T) {
	const embargo = 50
	w := planFolds(1000, 400, 100, embargo, false)
	if len(w) == 0 {
		t.Fatal("no folds planned")
	}
	for i, f := range w {
		if gap := f.testFrom - f.trainTo - 1; gap != embargo {
			t.Errorf("fold %d embargo is %d sessions, want %d", i, gap, embargo)
		}
	}
}

func TestPlanFoldsAnchoredExpands(t *testing.T) {
	w := planFolds(1200, 300, 100, 0, true)
	if len(w) < 2 {
		t.Fatalf("want several folds, got %d", len(w))
	}
	for i, f := range w {
		if f.trainFrom != 0 {
			t.Errorf("fold %d anchored training should start at 0, got %d", i, f.trainFrom)
		}
		if i > 0 && f.trainTo <= w[i-1].trainTo {
			t.Errorf("fold %d training window did not expand", i)
		}
	}
}

func TestPlanFoldsRefusesInsufficientHistory(t *testing.T) {
	if w := planFolds(300, 400, 100, 0, false); w != nil {
		t.Errorf("300 sessions cannot hold a 400-session training window: %v", w)
	}
	if w := planFolds(1000, 400, 100, 600, false); w != nil {
		t.Errorf("an embargo that consumes the data should yield no folds: %v", w)
	}
}

func TestWalkForwardReportsOutOfSample(t *testing.T) {
	spec := sweepSpec()
	spec.Start = "2018-01-03"
	spec.End = "2023-12-29"

	res, err := RunWalkForward(context.Background(), WalkForwardSpec{
		Base: spec, TrainDays: 300, TestDays: 120, Embargo: 10, Workers: 4,
	}, newTestStore(t), nil)
	if err != nil {
		t.Fatalf("walk-forward: %v", err)
	}
	if len(res.Folds) < 2 {
		t.Fatalf("want several folds, got %d", len(res.Folds))
	}

	for i, f := range res.Folds {
		if f.Error != "" {
			t.Fatalf("fold %d failed: %s", i, f.Error)
		}
		if f.TestStart <= f.TrainEnd {
			t.Errorf("fold %d test window starts inside training data", i)
		}
		if len(f.BestParams) == 0 {
			t.Errorf("fold %d chose no parameters", i)
		}
		if f.Combos != 6 {
			t.Errorf("fold %d searched %d combinations, want 6", i, f.Combos)
		}
	}

	// Consecutive folds must join: each starts where the last one finished.
	for i := 1; i < len(res.Folds); i++ {
		prev := res.Folds[i-1].TestCurve
		cur := res.Folds[i].TestCurve
		if len(prev) == 0 || len(cur) == 0 {
			continue
		}
		end := prev[len(prev)-1].Value
		// The next fold starts from the previous close, less a session of
		// friction at most.
		if cur[0].Value > end*1.5 || cur[0].Value < end*0.5 {
			t.Errorf("fold %d does not continue from fold %d: %v then %v",
				i, i-1, end, cur[0].Value)
		}
	}

	if len(res.Stitched) == 0 {
		t.Fatal("no stitched out-of-sample curve")
	}
	if res.StitchedMetrics.TradingDays != len(res.Stitched) {
		t.Errorf("stitched metrics cover %d days but the curve has %d",
			res.StitchedMetrics.TradingDays, len(res.Stitched))
	}
	// Drawdowns are recomputed across the join, so no point may claim a
	// positive drawdown and the first must be zero or negative.
	for _, p := range res.Stitched {
		if p.Drawdown > 0 {
			t.Fatalf("positive drawdown at %s: %v", p.Date, p.Drawdown)
		}
	}
	if res.Verdict == "" {
		t.Error("a verdict should always be written")
	}
	if res.ConsistentFolds > len(res.Folds) {
		t.Errorf("more positive folds than folds: %d of %d", res.ConsistentFolds, len(res.Folds))
	}
	if res.ParamStability < 0 || res.ParamStability > 1 {
		t.Errorf("stability out of range: %v", res.ParamStability)
	}
}

func TestWalkForwardExplainsInsufficientHistory(t *testing.T) {
	spec := sweepSpec()
	spec.Start = "2022-01-04"
	spec.End = "2022-06-30"
	_, err := RunWalkForward(context.Background(), WalkForwardSpec{
		Base: spec, TrainDays: 5000, TestDays: 500,
	}, newTestStore(t), nil)
	if err == nil {
		t.Fatal("expected an error for too little history")
	}
	if !strings.Contains(err.Error(), "--train") {
		t.Errorf("the error should say what to change: %v", err)
	}
}

func TestWalkForwardEfficiencyIsDefined(t *testing.T) {
	spec := sweepSpec()
	spec.Start = "2018-01-03"
	spec.End = "2023-12-29"
	res, err := RunWalkForward(context.Background(), WalkForwardSpec{
		Base: spec, TrainDays: 400, TestDays: 150, Workers: 2,
	}, newTestStore(t), nil)
	if err != nil {
		t.Fatalf("walk-forward: %v", err)
	}
	if res.InSampleReturn == 0 && res.OutOfSampleMean == 0 {
		t.Skip("degenerate synthetic data produced no return either way")
	}
	if !res.Efficiency.Defined() && res.InSampleReturn != 0 {
		t.Error("efficiency should be defined when in-sample return is non-zero")
	}
	if e := float64(res.Efficiency); res.Efficiency.Defined() && math.IsNaN(e) {
		t.Error("a defined efficiency must not be NaN")
	}
}
