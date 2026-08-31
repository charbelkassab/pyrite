package app

import (
	"testing"

	"github.com/charbelkassab/pyrite/internal/config"
	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
	"github.com/charbelkassab/pyrite/internal/strategy"
)

func TestApplyDefaults(t *testing.T) {
	var o RunOptions
	o.ApplyDefaults()

	if o.End == "" {
		t.Error("End was left unset")
	}
	// Start stays empty on purpose: the engine reads that as "as early as the
	// data allows", which is what someone comparing strategies wants.
	if o.Start != "" {
		t.Errorf("Start = %q, want it left empty", o.Start)
	}
	if o.InitialCash != 100000 {
		t.Errorf("InitialCash = %v, want 100000", o.InitialCash)
	}
	if o.Fill != engine.FillNextOpen {
		t.Errorf("Fill = %q, want next-open — filling at a close the strategy "+
			"already saw is lookahead", o.Fill)
	}
	if o.AITier != config.TierFast {
		t.Errorf("AITier = %q", o.AITier)
	}

	// Values already set are left alone.
	set := RunOptions{Start: "2020-01-02", End: "2021-01-02", InitialCash: 5, Fill: engine.FillClose}
	set.ApplyDefaults()
	if set.Start != "2020-01-02" || set.End != "2021-01-02" || set.InitialCash != 5 {
		t.Errorf("ApplyDefaults overwrote explicit values: %+v", set)
	}
	if set.Fill != engine.FillClose {
		t.Error("an explicit fill model was overwritten")
	}
}

// A model-reading backtest costs one API call per simulated day, so an
// unbounded start date turns "try this idea" into a bill measured in
// thousands of calls. The window is clamped, and only when the caller did not
// choose one.
func TestClampForAI(t *testing.T) {
	opts := RunOptions{End: "2024-01-02"}
	if !ClampForAI(&opts, true) {
		t.Fatal("an open-ended AI run was not clamped")
	}
	if opts.Start == "" {
		t.Fatal("clamping left Start empty")
	}
	if opts.Start >= opts.End {
		t.Errorf("Start %q is not before End %q", opts.Start, opts.End)
	}

	// An explicit start is the caller's decision and stands.
	chosen := RunOptions{Start: "1990-01-02", End: "2024-01-02"}
	if ClampForAI(&chosen, true) || chosen.Start != "1990-01-02" {
		t.Error("an explicit start date was overwritten")
	}

	// A strategy that never calls a model is not clamped at all.
	plain := RunOptions{End: "2024-01-02"}
	if ClampForAI(&plain, false) || plain.Start != "" {
		t.Error("a run with no model calls was clamped")
	}
}

func TestBuildSpecPrefersCallerOptionsOverThePlan(t *testing.T) {
	plan := &strategy.Plan{
		Name:       "Test",
		Code:       "function onDay(ctx){}",
		Universe:   []string{"AAPL"},
		Benchmarks: []string{"QQQ"},
		Warmup:     40,
	}

	spec := BuildSpec(plan, "a prompt", RunOptions{End: "2023-12-29"})
	if len(spec.Universe) != 1 || spec.Universe[0] != "AAPL" {
		t.Errorf("universe = %v, want the plan's", spec.Universe)
	}

	// An explicit universe overrides whatever the model chose.
	spec = BuildSpec(plan, "a prompt", RunOptions{
		End:      "2023-12-29",
		Universe: []string{"msft", "MSFT", "tsla"},
	})
	if len(spec.Universe) != 2 || spec.Universe[0] != "MSFT" || spec.Universe[1] != "TSLA" {
		t.Errorf("universe = %v, want [MSFT TSLA] normalised and deduped", spec.Universe)
	}
}

// A point-in-time index cannot be flattened to a fixed symbol list, because
// membership depends on the day. It has to travel on the spec so the engine
// can resolve it per session — flattening it here would reintroduce exactly
// the survivorship bias the index table exists to remove.
func TestBuildSpecKeepsAnIndexUnflattened(t *testing.T) {
	plan := &strategy.Plan{Name: "T", Code: "function onDay(ctx){}"}

	spec := BuildSpec(plan, "", RunOptions{End: "2023-12-29", Universe: []string{"sp500"}})
	if spec.Index == "" {
		t.Fatal("the index was not carried on the spec")
	}
	for _, s := range spec.Universe {
		if market.IndexUniverse(s) != "" {
			t.Errorf("the index name %q was left in the symbol list", s)
		}
	}

	// Symbols named alongside the index survive as extras, so "sp500 plus
	// TLT" is expressible.
	spec = BuildSpec(plan, "", RunOptions{End: "2023-12-29", Universe: []string{"sp500", "TLT"}})
	if spec.Index == "" {
		t.Error("the index was dropped when another symbol was present")
	}
	var hasTLT bool
	for _, s := range spec.Universe {
		if s == "TLT" {
			hasTLT = true
		}
	}
	if !hasTLT {
		t.Errorf("TLT was dropped alongside the index: %v", spec.Universe)
	}
}

// Offline mode must reach no network at all, including for macro series.
// Returning null and saying so is honest; inventing an unemployment rate is
// not, and a strategy branching on fake data produces a fake result.
func TestOfflineModeUsesSyntheticDataAndNoEconProvider(t *testing.T) {
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.OfflineMode = true

	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Econ != nil {
		t.Error("offline mode built an economic-data provider, which reaches the network")
	}

	// The cache is namespaced by provider so one offline run cannot poison
	// the real market cache with synthetic prices.
	h := a.Health(t.Context(), false)
	if !h.OfflineMode {
		t.Error("health did not report offline mode")
	}
	if h.DataProvider == "" {
		t.Error("health reported no data provider")
	}
	if h.DataDir != cfg.DataDir {
		t.Errorf("health reported data dir %q, want %q", h.DataDir, cfg.DataDir)
	}
}

func TestRunsDirLivesUnderTheDataDir(t *testing.T) {
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.OfflineMode = true

	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.RunsDir(); len(got) <= len(cfg.DataDir) || got[:len(cfg.DataDir)] != cfg.DataDir {
		t.Errorf("RunsDir = %q, want it under %q", got, cfg.DataDir)
	}
}
