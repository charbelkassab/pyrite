package engine

import (
	"math"
	"testing"
)

func TestNormalInvRoundTrips(t *testing.T) {
	for _, p := range []float64{0.01, 0.05, 0.25, 0.5, 0.75, 0.95, 0.99, 0.999} {
		x := normalInv(p)
		if got := normalCDF(x); math.Abs(got-p) > 1e-6 {
			t.Errorf("normalInv/normalCDF disagree at p=%v: %v", p, got)
		}
	}
	// The two standard reference points.
	if got := normalInv(0.975); math.Abs(got-1.959964) > 1e-4 {
		t.Errorf("normalInv(0.975): got %v, want 1.959964", got)
	}
	if got := normalCDF(0); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("normalCDF(0): got %v", got)
	}
}

func TestExpectedMaxGrowsWithTrials(t *testing.T) {
	// The more strategies you try, the better the best looks by luck alone.
	// If this were flat, the whole deflation would be pointless.
	a := expectedMax(1.0, 10)
	b := expectedMax(1.0, 1000)
	if !(b > a && a > 0) {
		t.Fatalf("expected max should rise with trial count: %v then %v", a, b)
	}
	// Ten trials of unit spread should peak somewhere near 1.5 sigma.
	if a < 1.0 || a > 2.5 {
		t.Errorf("expectedMax(1, 10) = %v, outside the plausible range", a)
	}
	if expectedMax(0, 100) != 0 {
		t.Error("no spread means no expected maximum")
	}
}

func TestPlateauRatioSeparatesSpikeFromRidge(t *testing.T) {
	// A ridge: neighbours score nearly as well as the peak.
	ridge := []SweepRow{
		{Params: map[string]any{"n": 10}, Score: 0.9},
		{Params: map[string]any{"n": 20}, Score: 1.0},
		{Params: map[string]any{"n": 30}, Score: 0.9},
	}
	r, n := plateauRatio(ridge)
	if n != 2 {
		t.Fatalf("want 2 neighbours, got %d", n)
	}
	if float64(r) < 0.8 {
		t.Errorf("a ridge should score high: %v", float64(r))
	}

	// A spike: the same peak, with nothing supporting it.
	spike := []SweepRow{
		{Params: map[string]any{"n": 10}, Score: 0.05},
		{Params: map[string]any{"n": 20}, Score: 1.0},
		{Params: map[string]any{"n": 30}, Score: 0.05},
	}
	r2, _ := plateauRatio(spike)
	if float64(r2) > 0.2 {
		t.Errorf("a spike should score low: %v", float64(r2))
	}
	if float64(r2) >= float64(r) {
		t.Errorf("the spike must score below the ridge: %v vs %v", float64(r2), float64(r))
	}
}

func TestPlateauRatioIgnoresDiagonalNeighbours(t *testing.T) {
	// Only combinations one step away on exactly one axis count.
	rows := []SweepRow{
		{Params: map[string]any{"a": 1, "b": 1}, Score: 0.5},
		{Params: map[string]any{"a": 2, "b": 1}, Score: 0.4},
		{Params: map[string]any{"a": 1, "b": 2}, Score: 0.6},
		{Params: map[string]any{"a": 2, "b": 2}, Score: 1.0}, // the winner
	}
	_, n := plateauRatio(rows)
	if n != 2 {
		t.Errorf("the winner has exactly 2 orthogonal neighbours, got %d", n)
	}
}

func TestPBOIsHalfForPureNoise(t *testing.T) {
	// Trials of independent noise carry no information, so choosing the best
	// in-sample should land below the out-of-sample median about half the
	// time. This is the calibration check for the whole statistic.
	const trials, periods = 24, 800
	rng := newLCG(42)
	returns := make([][]float64, trials)
	for i := range returns {
		row := make([]float64, periods)
		for j := range row {
			// Uniform noise centred on zero.
			row[j] = float64(rng.next()%20001)/1e6 - 0.01
		}
		returns[i] = row
	}

	var r Robustness
	r.AddPBO(returns, 8)
	if !r.PBO.Defined() {
		t.Fatal("PBO should be computable for aligned noise")
	}
	if r.PBOSplits != 70 {
		t.Errorf("8 blocks choose 4 gives 70 splits, got %d", r.PBOSplits)
	}
	if p := float64(r.PBO); p < 0.3 || p > 0.7 {
		t.Errorf("noise should give PBO near 0.5, got %v", p)
	}
}

func TestPBOIsLowWhenOneTrialGenuinelyDominates(t *testing.T) {
	// One trial has a real, persistent edge; the rest are noise. Selection
	// should then transfer out of sample, and PBO should be small.
	const trials, periods = 12, 800
	rng := newLCG(7)
	returns := make([][]float64, trials)
	for i := range returns {
		row := make([]float64, periods)
		for j := range row {
			row[j] = float64(rng.next()%20001)/1e6 - 0.01
			if i == 0 {
				row[j] += 0.004 // a steady edge in every period
			}
		}
		returns[i] = row
	}

	var r Robustness
	r.AddPBO(returns, 8)
	if p := float64(r.PBO); p > 0.1 {
		t.Errorf("a genuine edge should give a low PBO, got %v", p)
	}
}

func TestPBORefusesRaggedInput(t *testing.T) {
	var r Robustness
	r.AddPBO([][]float64{make([]float64, 200), make([]float64, 150)}, 8)
	if r.PBO.Defined() {
		t.Error("series of different lengths must not be compared")
	}
	// Odd block counts cannot be split in half.
	var r2 Robustness
	r2.AddPBO([][]float64{make([]float64, 400), make([]float64, 400)}, 7)
	if r2.PBO.Defined() {
		t.Error("an odd block count should be refused")
	}
}

func TestCombinationsEnumeratesCorrectly(t *testing.T) {
	got := combinations(4, 2)
	if len(got) != 6 {
		t.Fatalf("4 choose 2 is 6, got %d", len(got))
	}
	seen := map[string]bool{}
	for _, c := range got {
		if len(c) != 2 {
			t.Fatalf("bad combination %v", c)
		}
		key := string(rune('a'+c[0])) + string(rune('a'+c[1]))
		if seen[key] {
			t.Fatalf("duplicate combination %v", c)
		}
		seen[key] = true
	}
}

func TestBootstrapProducesOrderedBands(t *testing.T) {
	// A gently rising curve with real variation.
	rng := newLCG(3)
	curve := make([]EquityPoint, 500)
	v := 100000.0
	for i := range curve {
		v *= 1 + (float64(rng.next()%20001)/1e6 - 0.0095)
		curve[i] = EquityPoint{Date: "2020-01-01", Value: v}
	}
	b := Bootstrap(curve, 200, 21, 99)
	if b.Trials != 200 {
		t.Fatalf("trials: got %d", b.Trials)
	}
	if !(b.ReturnP05 <= b.ReturnMedian && b.ReturnMedian <= b.ReturnP95) {
		t.Errorf("return percentiles out of order: %v %v %v",
			b.ReturnP05, b.ReturnMedian, b.ReturnP95)
	}
	if b.DrawdownP05 > b.DrawdownMedian {
		t.Errorf("the 5th percentile drawdown should be the worse one: %v vs %v",
			b.DrawdownP05, b.DrawdownMedian)
	}
	if b.DrawdownMedian > 0 {
		t.Errorf("drawdowns must be negative: %v", b.DrawdownMedian)
	}
	if b.LossProbability < 0 || b.LossProbability > 1 {
		t.Errorf("loss probability out of range: %v", b.LossProbability)
	}
}

func TestBootstrapIsReproducibleFromItsSeed(t *testing.T) {
	curve := make([]EquityPoint, 300)
	v := 100000.0
	rng := newLCG(11)
	for i := range curve {
		v *= 1 + (float64(rng.next()%10001)/1e6 - 0.004)
		curve[i] = EquityPoint{Date: "2020-01-01", Value: v}
	}
	a := Bootstrap(curve, 100, 21, 5)
	b := Bootstrap(curve, 100, 21, 5)
	if a != b {
		t.Errorf("same seed produced different bands:\n%+v\n%+v", a, b)
	}
	if c := Bootstrap(curve, 100, 21, 6); c == a {
		t.Error("a different seed should produce a different resampling")
	}
}

func TestVerdictNamesAnIsolatedSpike(t *testing.T) {
	r := Robustness{
		Trials: 100, BestScore: 2.0, MedianScore: 0.1, ScoreStdev: 0.5,
		ExpectedMaxScore: 1.2, PositiveShare: 0.6,
		PlateauRatio: 0.1, Neighbours: 4,
		PBO: Ratio(math.NaN()), DeflatedSharpe: Ratio(math.NaN()),
	}
	v := verdict(r, "sharpe")
	if v == "" {
		t.Fatal("no verdict written")
	}
	if !contains(v, "isolated spike") {
		t.Errorf("a spike should be named as one: %q", v)
	}
}

func TestVerdictCallsOutASearchThatFoundNothing(t *testing.T) {
	r := Robustness{
		Trials: 500, BestScore: 0.8, MedianScore: 0.0, ScoreStdev: 0.6,
		ExpectedMaxScore: 1.9, PositiveShare: 0.5,
		PlateauRatio: Ratio(math.NaN()), PBO: Ratio(math.NaN()),
		DeflatedSharpe: Ratio(math.NaN()),
	}
	v := verdict(r, "sharpe")
	if !contains(v, "found nothing") {
		t.Errorf("a best below the luck threshold should be stated plainly: %q", v)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
