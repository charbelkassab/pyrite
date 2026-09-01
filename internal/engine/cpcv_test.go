package engine

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// binomial is the expected split and path count, computed independently of the
// code under test so the assertion is not the implementation restated.
func binomial(n, k int) int {
	if k < 0 || k > n {
		return 0
	}
	out := 1
	for i := 0; i < k; i++ {
		out = out * (n - i) / (i + 1)
	}
	return out
}

func TestPlanGroupsPartitionsTheCalendar(t *testing.T) {
	// A remainder that does not divide evenly is the interesting case: every
	// session must land in exactly one group and no group may be empty.
	for _, n := range []int{600, 601, 3521} {
		for _, g := range []int{3, 6, 7} {
			bounds := planGroups(n, g)
			if len(bounds) != g {
				t.Fatalf("n=%d g=%d: got %d groups", n, g, len(bounds))
			}
			if bounds[0].from != 0 || bounds[g-1].to != n-1 {
				t.Errorf("n=%d g=%d: groups span %d..%d, want 0..%d",
					n, g, bounds[0].from, bounds[g-1].to, n-1)
			}
			total := 0
			for i, b := range bounds {
				if b.to < b.from {
					t.Errorf("n=%d g=%d: group %d is empty", n, g, i)
				}
				if i > 0 && b.from != bounds[i-1].to+1 {
					t.Errorf("n=%d g=%d: group %d does not follow group %d", n, g, i, i-1)
				}
				total += b.to - b.from + 1
			}
			if total != n {
				t.Errorf("n=%d g=%d: groups cover %d sessions", n, g, total)
			}
		}
	}
}

func TestSplitCountIsTheBinomial(t *testing.T) {
	for _, c := range []struct{ groups, test int }{{6, 2}, {6, 1}, {8, 3}, {5, 2}} {
		if got, want := len(combinations(c.groups, c.test)), binomial(c.groups, c.test); got != want {
			t.Errorf("C(%d,%d): %d splits, want %d", c.groups, c.test, got, want)
		}
	}
}

func TestEverySessionIsHeldOutTheSameNumberOfTimes(t *testing.T) {
	// Each group is tested by C(groups-1, test-1) splits, and a session is
	// tested exactly as often as its group. That is the arithmetic the path
	// reconstruction rests on: if it were uneven, some path would be short a
	// segment and some session would be counted twice.
	const n, groups, test = 1200, 6, 2
	bounds := planGroups(n, groups)
	counts := make([]int, n)
	for _, split := range combinations(groups, test) {
		for _, g := range split {
			for i := bounds[g].from; i <= bounds[g].to; i++ {
				counts[i]++
			}
		}
	}
	want := binomial(groups-1, test-1)
	for i, c := range counts {
		if c != want {
			t.Fatalf("session %d held out %d times, want %d", i, c, want)
		}
	}
}

func TestPurgeMaskRemovesExactlyTheBoundarySessions(t *testing.T) {
	// 600 sessions, six groups of 100, holding out groups 1 and 4 with a
	// 10-session embargo. The purged indices are known by hand: the ten
	// sessions on each side of each held-out group, and nothing else.
	const n, embargo = 600, 10
	bounds := planGroups(n, 6)
	mask := purgeMask(n, bounds, []int{1, 4}, embargo)

	want := map[int]bool{}
	for _, g := range []int{1, 4} {
		for i := bounds[g].from - embargo; i < bounds[g].from; i++ {
			want[i] = true
		}
		for i := bounds[g].to + 1; i <= bounds[g].to+embargo; i++ {
			want[i] = true
		}
	}
	// Spelled out, so the test fails if the ranges above ever drift: groups 1
	// and 4 run 100..199 and 400..499.
	for _, i := range []int{90, 99, 200, 209, 390, 399, 500, 509} {
		if !mask[i] {
			t.Errorf("session %d sits against a held-out group and was not purged", i)
		}
	}
	for _, i := range []int{0, 89, 210, 300, 389, 510, 599} {
		if mask[i] {
			t.Errorf("session %d is nowhere near a held-out group and was purged", i)
		}
	}
	for i := 0; i < n; i++ {
		// A held-out session is not "purged" — it is the test set — so the
		// mask must not claim it either way.
		inTest := (i >= 100 && i <= 199) || (i >= 400 && i <= 499)
		if inTest {
			continue
		}
		if mask[i] != want[i] {
			t.Fatalf("session %d: mask %v, want %v", i, mask[i], want[i])
		}
	}
	if got := len(want); got != 40 {
		t.Errorf("expected 40 purged sessions, computed %d", got)
	}
}

func TestPurgeMaskWithNoEmbargoRemovesNothing(t *testing.T) {
	mask := purgeMask(600, planGroups(600, 6), []int{1, 4}, 0)
	for i, m := range mask {
		if m {
			t.Fatalf("session %d purged with no embargo asked for", i)
		}
	}
}

func TestResolveEmbargoDefaultsToTheWarmup(t *testing.T) {
	if got := resolveEmbargo(0, 200); got != 200 {
		t.Errorf("unset embargo should fall back to the warm-up, got %d", got)
	}
	if got := resolveEmbargo(-1, 200); got != 0 {
		t.Errorf("a negative embargo means none, got %d", got)
	}
	if got := resolveEmbargo(30, 200); got != 30 {
		t.Errorf("an explicit embargo should stand, got %d", got)
	}
}

// cpcvTestSpec is the shared setup: four years of synthetic data and a
// strategy with six combinations, which is enough to have something to select
// between without making the test slow.
func cpcvTestSpec() CPCVSpec {
	return CPCVSpec{
		Base: sweepSpec(), Groups: 6, TestGroups: 2, Workers: 4,
		SkipWalkForward: true,
	}
}

func TestCPCVReconstructsFullLengthPaths(t *testing.T) {
	cs := cpcvTestSpec()
	res, err := RunCPCV(context.Background(), cs, newTestStore(t), nil)
	if err != nil {
		t.Fatalf("cpcv: %v", err)
	}

	if got, want := len(res.Splits), binomial(6, 2); got != want {
		t.Fatalf("%d splits, want C(6,2)=%d", got, want)
	}
	if got, want := len(res.Paths), binomial(5, 1); got != want {
		t.Fatalf("%d paths, want C(5,1)=%d", got, want)
	}
	if res.ValidPaths != len(res.Paths) {
		t.Errorf("%d of %d paths failed", len(res.Paths)-res.ValidPaths, len(res.Paths))
	}

	// Every path must cover the whole period, and every group in it must come
	// from a split that held that group out.
	for _, p := range res.Paths {
		if p.Error != "" {
			t.Fatalf("path %d: %s", p.Index, p.Error)
		}
		if len(p.Curve) != res.Sessions {
			t.Errorf("path %d covers %d sessions, want %d", p.Index, len(p.Curve), res.Sessions)
		}
		if p.Start != res.GroupBounds[0].Start || p.End != res.GroupBounds[len(res.GroupBounds)-1].End {
			t.Errorf("path %d runs %s..%s, want %s..%s", p.Index, p.Start, p.End,
				res.GroupBounds[0].Start, res.GroupBounds[len(res.GroupBounds)-1].End)
		}
		for g, s := range p.Splits {
			held := false
			for _, tg := range res.Splits[s].TestGroups {
				if tg == g {
					held = true
				}
			}
			if !held {
				t.Errorf("path %d took group %d from split %d, which trained on it", p.Index, g, s)
			}
		}
	}

	// Purging must have cost something: with an embargo equal to the 60-bar
	// warm-up, every split has to give up training sessions at the boundary.
	for _, sp := range res.Splits {
		if sp.Error != "" {
			t.Fatalf("split %d: %s", sp.Index, sp.Error)
		}
		if sp.PurgedSessions == 0 {
			t.Errorf("split %d purged nothing", sp.Index)
		}
		if sp.TrainSessions+sp.PurgedSessions+sp.TestSessions != res.Sessions {
			t.Errorf("split %d: %d train + %d purged + %d test does not account for %d sessions",
				sp.Index, sp.TrainSessions, sp.PurgedSessions, sp.TestSessions, res.Sessions)
		}
	}
	// The spec carries 30 bars and setup() asks for 60. The embargo has to
	// cover the larger, or an indicator computed on the first test session is
	// still made of training data.
	if res.Embargo != 60 {
		t.Errorf("embargo defaulted to %d, want the 60 bars the strategy needs", res.Embargo)
	}
	if res.Verdict == "" {
		t.Error("a verdict should always be written")
	}
	if res.ProfitablePaths > res.ValidPaths {
		t.Errorf("more profitable paths than paths: %d of %d", res.ProfitablePaths, res.ValidPaths)
	}
}

func TestEmbargoFindsAWarmupDeclaredInSetup(t *testing.T) {
	// A strategy loaded from a file carries no warm-up in its spec: it raises
	// one with ctx.warmup() inside setup(). Sizing the embargo from the spec
	// alone purged nothing at all for exactly the strategies most likely to
	// need it.
	cs := cpcvTestSpec()
	cs.Base.Warmup = 0
	res, err := RunCPCV(context.Background(), cs, newTestStore(t), nil)
	if err != nil {
		t.Fatalf("cpcv: %v", err)
	}
	if res.Embargo != 60 {
		t.Errorf("embargo %d, want the 60 bars setup() declared", res.Embargo)
	}
	for _, sp := range res.Splits {
		if sp.Error == "" && sp.PurgedSessions == 0 {
			t.Fatalf("split %d purged nothing despite a declared warm-up", sp.Index)
		}
	}
}

func TestCPCVPathMatchesTheGroupRunsItIsMadeOf(t *testing.T) {
	// The path is chained from per-group return series rather than from one
	// continuous backtest, which is the load-bearing approximation in this
	// file. Pinned against the runs it claims to be made of: one
	// configuration, so every split picks it and the path is simply the six
	// group backtests multiplied together.
	cs := cpcvTestSpec()
	cs.Grids = map[string][]any{"fast": {10.0}, "slow": {50.0}}
	store := newTestStore(t)

	res, err := RunCPCV(context.Background(), cs, store, nil)
	if err != nil {
		t.Fatalf("cpcv: %v", err)
	}
	if res.Combos != 1 {
		t.Fatalf("want a single combination, got %d", res.Combos)
	}
	if len(res.Paths) == 0 || res.Paths[0].Error != "" {
		t.Fatal("no usable path")
	}

	spec := cs.Base
	spec.ApplyDefaults()
	chained := 1.0
	for _, g := range res.GroupBounds {
		s := spec
		s.Start, s.End = g.Start, g.End
		s.Params = map[string]any{"fast": 10.0, "slow": 50.0}
		s.OmitDayRecords = true
		r, err := New(s, store).Run(context.Background())
		if err != nil {
			t.Fatalf("group %d: %v", g.Index, err)
		}
		// Measured from the run's opening cash, which is where the chained
		// series starts each group from.
		chained *= r.Metrics.EndValue / s.InitialCash
	}
	want := chained - 1
	got := res.Paths[0].Metrics.TotalReturn
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("path returned %v, but the six group runs multiply to %v", got, want)
	}

	// With nothing to select between, every path has to be the same path.
	for _, p := range res.Paths[1:] {
		if p.Metrics.TotalReturn != got {
			t.Errorf("path %d returned %v against %v with only one configuration to choose",
				p.Index, p.Metrics.TotalReturn, got)
		}
	}
	if res.PBO.Defined() {
		t.Error("one combination is not a selection, so there is no overfitting to measure")
	}
}

func TestCPCVIsDeterministic(t *testing.T) {
	// Two non-determinism bugs have shipped in this package, both from map
	// iteration order reaching the output. The evaluation runs across workers
	// and reduces over splits, paths and combinations, so it is exactly the
	// shape that goes wrong.
	cs := cpcvTestSpec()
	cs.SkipWalkForward = false
	cs.TrainDays, cs.TestDays = 300, 100

	store := newTestStore(t)
	first, err := RunCPCV(context.Background(), cs, store, nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := RunCPCV(context.Background(), cs, store, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	// Elapsed is wall-clock and is expected to differ.
	first.Elapsed, second.Elapsed = 0, 0
	a, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if string(a) != string(b) {
		t.Error("two runs over identical inputs produced different output")
	}
	if first.WalkForward == nil {
		t.Fatal("the walk-forward comparison should have run")
	}
	if !first.WalkForward.CAGRPercentile.Defined() {
		t.Error("the walk-forward path should have been placed inside the distribution")
	}
}

func TestCPCVMarshalsUndefinedStatisticsAsNull(t *testing.T) {
	// A bare NaN refuses to encode and truncates the whole response, which is
	// a bug this repo has shipped. Every statistic that can be undefined has
	// to be a Ratio.
	res := &CPCVResult{
		PBO: Ratio(math.NaN()), BlockPBO: Ratio(math.Inf(1)),
		Return: spreadOf(nil), CAGR: spreadOf(nil), Sharpe: spreadOf(nil),
		MaxDrawdown: spreadOf(nil), NoSelection: spreadOf(nil),
		Splits: []CPCVSplit{{
			TrainScore: Ratio(math.NaN()), TestScore: Ratio(math.NaN()),
			TrainReturn: Ratio(math.NaN()), TestReturn: Ratio(math.NaN()),
		}},
		WalkForward: &CPCVWalkForward{
			TotalReturn: Ratio(math.NaN()), CAGR: Ratio(math.NaN()), Sharpe: Ratio(math.NaN()),
			CAGRPercentile: Ratio(math.NaN()), SharpePercentile: Ratio(math.NaN()),
		},
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("undefined statistics broke the encoder: %v", err)
	}
	if !strings.Contains(string(b), `"pbo":null`) {
		t.Errorf("an undefined PBO should marshal to null: %s", b)
	}
}

func TestCPCVExplainsTooLittleHistory(t *testing.T) {
	cs := cpcvTestSpec()
	cs.Base.Start = "2023-10-02"
	cs.Base.End = "2023-12-29"
	cs.Groups = 20
	_, err := RunCPCV(context.Background(), cs, newTestStore(t), nil)
	if err == nil {
		t.Fatal("expected an error for too little history")
	}
	if !strings.Contains(err.Error(), "--groups") {
		t.Errorf("the error should say what to change: %v", err)
	}
}

func TestCPCVRefusesAnImpossiblePartition(t *testing.T) {
	cs := cpcvTestSpec()
	cs.TestGroups = 6
	if _, err := RunCPCV(context.Background(), cs, newTestStore(t), nil); err == nil {
		t.Error("holding out every group leaves nothing to train on and should be refused")
	}
	cs = cpcvTestSpec()
	cs.Groups = 2
	if _, err := RunCPCV(context.Background(), cs, newTestStore(t), nil); err == nil {
		t.Error("two groups admit no combination and should be refused")
	}
}

func TestSpreadOfIsUndefinedWithoutData(t *testing.T) {
	s := spreadOf(nil)
	for name, r := range map[string]Ratio{
		"mean": s.Mean, "median": s.Median, "stdev": s.Stdev,
		"p05": s.P05, "p95": s.P95, "worst": s.Worst, "best": s.Best,
	} {
		if r.Defined() {
			t.Errorf("%s of an empty sample should be undefined, got %v", name, float64(r))
		}
	}
	// One observation has a median but no deviation, and reporting zero there
	// would claim a spread that was never measured.
	one := spreadOf([]float64{0.4})
	if !one.Median.Defined() || float64(one.Median) != 0.4 {
		t.Errorf("median of one observation should be that observation, got %v", float64(one.Median))
	}
	if one.Stdev.Defined() {
		t.Error("a single observation has no standard deviation")
	}
}

func TestBelowMedianRankMatchesTheRankItReplaces(t *testing.T) {
	// The purged cross-validation and the sweep's block partition must answer
	// the overfitting question by the same arithmetic.
	scores := []float64{-1, 0, 1, 2, 3}
	if belowMedianRank(scores, 0) != true {
		t.Error("the worst trial ranks below median")
	}
	if belowMedianRank(scores, 4) != false {
		t.Error("the best trial does not rank below median")
	}
	if belowMedianRank(scores, -1) {
		t.Error("an absent pick cannot rank below median")
	}
}

func TestVerdictComparesTheTwoOverfittingEstimates(t *testing.T) {
	// The bar for "these disagree" is two standard errors of the less precise
	// estimate, not a round number, so the same six-point gap has to read as
	// noise over fifteen splits and the same fifty-point gap as a finding.
	base := func(pbo, block float64, pboSplits, blockSplits int) *CPCVResult {
		return &CPCVResult{
			Combos: 10, ValidPaths: 5, ProfitablePaths: 3, WorstPath: 0,
			Return: spreadOf([]float64{-0.1, 0.05, 0.2, 0.3, 0.4}),
			CAGR:   spreadOf([]float64{-0.02, 0.01, 0.03, 0.04, 0.05}),
			Sharpe: spreadOf([]float64{-0.2, 0.1, 0.3, 0.4, 0.5}),
			// No selection edge in this fixture, so the clause is silent and
			// the assertions below are about the PBO comparison alone.
			NoSelection: spreadOf(nil),
			PBO:         Ratio(pbo), PBOSplits: pboSplits,
			BlockPBO: Ratio(block), BlockPBOSplits: blockSplits,
		}
	}
	if v := cpcvVerdict(base(0.55, 0.49, 15, 70)); !strings.Contains(v, "within the noise of 15 splits") {
		t.Errorf("six points over fifteen splits is noise: %q", v)
	}
	if v := cpcvVerdict(base(0.80, 0.30, 15, 70)); !strings.Contains(v, "disagrees") {
		t.Errorf("fifty points is wider than any of these estimates' noise: %q", v)
	}
}

func TestVerdictNamesWhatTheSelectionWasWorth(t *testing.T) {
	r := &CPCVResult{
		Combos: 10, ValidPaths: 5, ProfitablePaths: 5, WorstPath: 0,
		Return:      spreadOf([]float64{0.6, 0.7, 0.7, 0.7, 0.8}),
		CAGR:        spreadOf([]float64{0.03, 0.04, 0.04, 0.04, 0.05}),
		Sharpe:      spreadOf([]float64{0.5, 0.5, 0.5, 0.6, 0.6}),
		NoSelection: spreadOf([]float64{0.9, 0.95, 1.0}),
		PBO:         Ratio(math.NaN()), BlockPBO: Ratio(math.NaN()),
	}
	v := cpcvVerdict(r)
	// Five profitable paths is the most flattering sentence this command can
	// produce, and it is worthless without the control beside it.
	if !strings.Contains(v, "subtracted") {
		t.Errorf("a selection that underperformed doing nothing should be named: %q", v)
	}
	if i, j := strings.Index(v, "subtracted"), strings.Index(v, "percentile"); i > j && j >= 0 {
		t.Errorf("the control belongs before the spread, not after it: %q", v)
	}
}

func TestPercentileOfPlacesAValue(t *testing.T) {
	xs := []float64{0, 1, 2, 3}
	if got := float64(percentileOf(xs, 1.5)); got != 0.5 {
		t.Errorf("half the sample is below 1.5, got %v", got)
	}
	if got := float64(percentileOf(xs, -1)); got != 0 {
		t.Errorf("nothing is below -1, got %v", got)
	}
	if percentileOf(nil, 1).Defined() {
		t.Error("an empty sample places nothing")
	}
	if percentileOf(xs, math.NaN()).Defined() {
		t.Error("an undefined value has no position")
	}
}
