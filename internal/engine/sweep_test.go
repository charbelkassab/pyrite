package engine

import (
	"context"
	"math"
	"strings"
	"testing"
)

// paramStrategy declares two tunables and uses them, so a sweep has something
// real to search.
const paramStrategy = `
	function setup(ctx) {
		ctx.warmup(60);
		ctx.param("fast", 10, { grid: [5, 10, 20] });
		ctx.param("slow", 50, { grid: [30, 50] });
	}
	function onDay(ctx) {
		const f = ctx.sma("AAPL", ctx.params.fast);
		const s = ctx.sma("AAPL", ctx.params.slow);
		if (f === null || s === null) return;
		if (f > s && !ctx.hasPosition("AAPL")) ctx.buy("AAPL", { pctCash: 0.95 });
		else if (f < s && ctx.hasPosition("AAPL")) ctx.close("AAPL");
	}
`

func sweepSpec() Spec {
	spec := baseSpec(paramStrategy)
	spec.Universe = []string{"AAPL"}
	spec.Start = "2020-01-06"
	spec.End = "2023-12-29"
	return spec
}

func TestDeclaredParamsFindsWhatSetupDeclares(t *testing.T) {
	decls, err := DeclaredParams(context.Background(), sweepSpec(), newTestStore(t))
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	if len(decls) != 2 {
		t.Fatalf("want 2 declarations, got %d: %+v", len(decls), decls)
	}
	byName := map[string]ParamDecl{}
	for _, d := range decls {
		byName[d.Name] = d
	}
	if got := len(byName["fast"].Values()); got != 3 {
		t.Errorf("fast grid: got %d values, want 3", got)
	}
	if got := len(byName["slow"].Values()); got != 2 {
		t.Errorf("slow grid: got %d values, want 2", got)
	}
}

func TestParamReturnsOverrideWhenSupplied(t *testing.T) {
	spec := sweepSpec()
	spec.Params = map[string]any{"fast": 20}
	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.ParamValues["fast"]; !sameParam(got, 20) {
		t.Errorf("override ignored: got %v, want 20", got)
	}
	// The undisturbed parameter keeps its declared default.
	if got := res.ParamValues["slow"]; !sameParam(got, 50) {
		t.Errorf("default lost: got %v, want 50", got)
	}
}

func TestSweepCoversTheCartesianProduct(t *testing.T) {
	res, err := RunSweep(context.Background(), SweepSpec{
		Base: sweepSpec(), Workers: 4, Objective: "sharpe",
	}, newTestStore(t), nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Combos != 6 {
		t.Fatalf("3 x 2 grid should give 6 combinations, got %d", res.Combos)
	}
	if len(res.Rows) != 6 {
		t.Fatalf("want 6 rows, got %d", len(res.Rows))
	}
	if res.Failed != 0 {
		t.Errorf("%d combinations failed", res.Failed)
	}
	if len(res.Axes) != 2 || res.Axes[0] != "fast" || res.Axes[1] != "slow" {
		t.Errorf("axes should be the varying parameters, sorted: %v", res.Axes)
	}

	// Every combination must be distinct and complete.
	seen := map[string]bool{}
	for _, row := range res.Rows {
		if row.Label == "" {
			t.Fatalf("row without a label: %+v", row)
		}
		if seen[row.Label] {
			t.Fatalf("duplicate combination %q", row.Label)
		}
		seen[row.Label] = true
		if len(row.Params) != 2 {
			t.Errorf("row %q should carry both parameters: %v", row.Label, row.Params)
		}
	}
	if len(res.Best) != 1 {
		t.Fatalf("the winner's full result should be retained, got %d", len(res.Best))
	}
	// The retained winner must be the top-scoring row.
	best := res.Sorted()[0]
	if !sameParam(res.Best[0].ParamValues["fast"], best.Params["fast"]) {
		t.Errorf("retained result is not the winner: %v vs %v",
			res.Best[0].ParamValues, best.Params)
	}
}

func TestSweepIsDeterministic(t *testing.T) {
	// Two identical searches must produce identical tables. A sweep whose
	// row order depends on goroutine scheduling makes every heatmap a
	// different picture of the same data.
	store := newTestStore(t)
	a, err := RunSweep(context.Background(), SweepSpec{Base: sweepSpec(), Workers: 8}, store, nil)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	b, err := RunSweep(context.Background(), SweepSpec{Base: sweepSpec(), Workers: 3}, store, nil)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	for i := range a.Rows {
		if a.Rows[i].Label != b.Rows[i].Label {
			t.Fatalf("row %d differs between runs: %q vs %q", i, a.Rows[i].Label, b.Rows[i].Label)
		}
		if math.Abs(a.Rows[i].TotalReturn-b.Rows[i].TotalReturn) > 1e-9 {
			t.Fatalf("row %d returned a different result on re-run", i)
		}
	}
}

func TestSweepRejectsOversizedSearch(t *testing.T) {
	_, err := RunSweep(context.Background(), SweepSpec{
		Base: sweepSpec(), MaxCombos: 4,
	}, newTestStore(t), nil)
	if err == nil {
		t.Fatal("a 6-combination search should be refused under a limit of 4")
	}
	if !strings.Contains(err.Error(), "max-combos") {
		t.Errorf("the error should say how to fix it: %v", err)
	}
}

func TestSweepExplainsAnUnsweepableStrategy(t *testing.T) {
	spec := baseSpec(`function onDay(ctx) {}`)
	_, err := RunSweep(context.Background(), SweepSpec{Base: spec}, newTestStore(t), nil)
	if err == nil {
		t.Fatal("a strategy with no parameters cannot be swept")
	}
	if !strings.Contains(err.Error(), "ctx.param") {
		t.Errorf("the error should show how to declare one: %v", err)
	}
}

func TestSweepAcceptsCallerSuppliedGrids(t *testing.T) {
	res, err := RunSweep(context.Background(), SweepSpec{
		Base:  sweepSpec(),
		Grids: map[string][]any{"fast": {5, 8}},
	}, newTestStore(t), nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// fast overridden to 2 values, slow keeps its declared 2.
	if res.Combos != 4 {
		t.Errorf("caller grid should replace the declared one: got %d combos", res.Combos)
	}
}

func TestSurfaceProjectsOntoTwoAxes(t *testing.T) {
	res, err := RunSweep(context.Background(), SweepSpec{Base: sweepSpec()}, newTestStore(t), nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	xs, ys, z := res.Surface("fast", "slow")
	if len(xs) != 3 || len(ys) != 2 {
		t.Fatalf("surface dimensions: %d x %d", len(xs), len(ys))
	}
	if len(z) != 2 || len(z[0]) != 3 {
		t.Fatalf("z matrix should be rows=y, cols=x: %d x %d", len(z), len(z[0]))
	}
	filled := 0
	for _, rowz := range z {
		for _, v := range rowz {
			if !math.IsNaN(v) {
				filled++
			}
		}
	}
	if filled != 6 {
		t.Errorf("a two-parameter sweep should fill the whole surface: %d of 6", filled)
	}
}

func TestRobustnessAssessesTheSearch(t *testing.T) {
	res, err := RunSweep(context.Background(), SweepSpec{Base: sweepSpec()}, newTestStore(t), nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	r := res.Robustness
	if r.Trials != 6 {
		t.Errorf("trials: got %d, want 6", r.Trials)
	}
	if r.BestScore < r.MedianScore {
		t.Errorf("best should be at least the median: %v < %v", r.BestScore, r.MedianScore)
	}
	if r.WorstScore > r.MedianScore {
		t.Errorf("worst should be at most the median: %v > %v", r.WorstScore, r.MedianScore)
	}
	if r.Verdict == "" {
		t.Error("a verdict should always be written")
	}
	if r.PlateauRatio.Defined() {
		if p := float64(r.PlateauRatio); p < 0 || p > 1 {
			t.Errorf("plateau ratio out of range: %v", p)
		}
		if r.Neighbours == 0 {
			t.Error("a defined plateau ratio implies at least one neighbour")
		}
	}
}

// Caller-supplied grids arrive in a map, and appending them to the parameter
// declarations in map order made the expansion order vary between runs. The
// set of combinations is the same either way, but their order decides how
// ties break when the table is ranked, so the same search could name a
// different winner twice over identical data.
func TestCallerGridsExpandInAStableOrder(t *testing.T) {
	var first []string
	for run := 0; run < 8; run++ {
		ss := SweepSpec{
			Grids: map[string][]any{
				"zeta":  {1.0, 2.0},
				"alpha": {3.0, 4.0},
				"mid":   {5.0},
			},
		}
		decls := mergeGrids(nil, ss.Grids)
		got := make([]string, len(decls))
		for i, d := range decls {
			got[i] = d.Name
		}
		if run == 0 {
			first = got
			continue
		}
		for i := range first {
			if got[i] != first[i] {
				t.Fatalf("run %d ordered parameters %v, run 0 ordered them %v",
					run, got, first)
			}
		}
	}
	want := []string{"alpha", "mid", "zeta"}
	for i := range want {
		if first[i] != want[i] {
			t.Fatalf("parameters were %v, want them sorted as %v", first, want)
		}
	}
}
