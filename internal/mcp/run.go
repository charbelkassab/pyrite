package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charbelkassab/pyrite/examples"
	"github.com/charbelkassab/pyrite/internal/app"
	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
	"github.com/charbelkassab/pyrite/internal/strategy"
)

// strategyInput is what every backtest-shaped tool accepts. It mirrors the
// HTTP API's run request, because the two are the same question asked over
// different transports and a field that means one thing in the web app and
// another here would be worse than a missing one.
type strategyInput struct {
	Code    string `json:"code,omitempty"`
	Example string `json:"example,omitempty"`
	Name    string `json:"name,omitempty"`

	Universe   []string `json:"universe,omitempty"`
	Benchmarks []string `json:"benchmarks,omitempty"`
	Start      string   `json:"start,omitempty"`
	End        string   `json:"end,omitempty"`

	InitialCash   float64        `json:"initial_cash,omitempty"`
	Interval      string         `json:"interval,omitempty"`
	Fill          string         `json:"fill,omitempty"`
	SlippageBps   float64        `json:"slippage_bps,omitempty"`
	CommissionPct float64        `json:"commission_pct,omitempty"`
	Impact        float64        `json:"impact,omitempty"`
	AllowShort    bool           `json:"allow_short,omitempty"`
	Warmup        int            `json:"warmup,omitempty"`
	RiskFree      float64        `json:"risk_free_rate,omitempty"`
	Params        map[string]any `json:"params,omitempty"`
}

type sweepInput struct {
	strategyInput
	Grids     map[string][]any `json:"grids,omitempty"`
	Objective string           `json:"objective,omitempty"`
	MaxCombos int              `json:"max_combos,omitempty"`
	Top       int              `json:"top,omitempty"`
}

type walkForwardInput struct {
	strategyInput
	Grids     map[string][]any `json:"grids,omitempty"`
	Objective string           `json:"objective,omitempty"`
	TrainDays int              `json:"train_days,omitempty"`
	TestDays  int              `json:"test_days,omitempty"`
	Embargo   int              `json:"embargo,omitempty"`
	Anchored  bool             `json:"anchored,omitempty"`
	MaxCombos int              `json:"max_combos,omitempty"`
}

// prepare turns tool arguments into the plan, spec and options the app layer
// already knows how to run. Everything below this line is the existing
// orchestration; this package only translates.
func (s *Server) prepare(in strategyInput) (*strategy.Plan, engine.Spec, app.RunOptions, error) {
	var spec engine.Spec

	opts := app.RunOptions{
		InitialCash: in.InitialCash,
		RiskFree:    in.RiskFree,
		Params:      in.Params,
	}
	if in.Start != "" {
		d, err := market.ParseDay(in.Start)
		if err != nil {
			return nil, spec, opts, invalidParams("start is not a date: %v", err)
		}
		opts.Start = d
	}
	if in.End != "" {
		d, err := market.ParseDay(in.End)
		if err != nil {
			return nil, spec, opts, invalidParams("end is not a date: %v", err)
		}
		opts.End = d
	}
	if len(in.Benchmarks) > 0 {
		opts.Benchmarks = resolveSymbols(in.Benchmarks)
	} else {
		opts.Benchmarks = []string{"SPY"}
	}
	opts.Universe, opts.Index = resolveUniverse(in.Universe)
	switch in.Fill {
	case "", string(engine.FillNextOpen):
	case string(engine.FillClose):
		opts.Fill = engine.FillClose
	default:
		return nil, spec, opts, invalidParams("fill must be %q or %q, got %q",
			engine.FillNextOpen, engine.FillClose, in.Fill)
	}
	opts.ApplyDefaults()

	plan, err := s.plan(in, &opts)
	if err != nil {
		return nil, spec, opts, err
	}

	spec = app.BuildSpec(plan, "", opts)
	if in.Warmup > 0 {
		spec.Warmup = in.Warmup
	}
	if in.AllowShort {
		spec.AllowShort = true
	}
	if in.SlippageBps > 0 {
		spec.Costs.SlippageBps = in.SlippageBps
	}
	if in.CommissionPct > 0 {
		spec.Costs.CommissionPct = in.CommissionPct
	}
	spec.Costs.ImpactCoefficient = in.Impact
	if in.Interval != "" {
		iv, err := market.ParseInterval(in.Interval)
		if err != nil {
			return nil, spec, opts, invalidParams("%v", err)
		}
		if !s.app.Store.SupportsInterval(iv) {
			return nil, spec, opts, invalidParams(
				"the configured data provider serves daily bars only, so %s is unavailable. "+
					"Set PYRITE_DATA_PROVIDERS=yahoo for intraday.", iv)
		}
		spec.Interval = iv
	}
	return plan, spec, opts, nil
}

// plan produces the strategy to run. There is no compiler path here on
// purpose: an agent writes the JavaScript itself, so asking a second model to
// turn its English back into code would add a translation nobody needs.
func (s *Server) plan(in strategyInput, opts *app.RunOptions) (*strategy.Plan, error) {
	code := strings.TrimSpace(in.Code)
	name := strings.TrimSpace(in.Example)

	switch {
	case code != "" && name != "":
		return nil, invalidParams("pass either code or example, not both")
	case code == "" && name == "":
		return nil, invalidParams(
			"supply the strategy as code, or name a bundled one with example. " +
				"list_examples has the names and strategy_api has the functions a strategy may call")
	}

	if code != "" {
		return &strategy.Plan{
			Name:       defaultString(in.Name, "strategy"),
			Code:       code,
			Universe:   opts.Universe,
			Benchmarks: opts.Benchmarks,
			Warmup:     in.Warmup,
			AllowShort: in.AllowShort,
		}, nil
	}

	ex, err := examples.Get(name)
	if err != nil {
		return nil, invalidParams("%v", err)
	}
	if ex.NeedsModel && !s.app.Cfg.AnyProviderEnabled() {
		return nil, fmt.Errorf("the %q example calls a model inside the backtest and none is "+
			"configured, so it cannot run. Set OPENAI_API_KEY, CEREBRAS_API_KEY or KIMI_API_KEY, "+
			"or pick another example: every other one runs with nothing", ex.Name)
	}
	// The example's own declarations stand unless the caller overrode them.
	if len(opts.Universe) == 0 && opts.Index == "" && len(ex.Universe) > 0 {
		opts.Universe, opts.Index = resolveUniverse(ex.Universe)
	}
	if len(in.Benchmarks) == 0 && len(ex.Benchmarks) > 0 {
		opts.Benchmarks = resolveSymbols(ex.Benchmarks)
	}
	return &strategy.Plan{
		Name:       defaultString(ex.Label, ex.Name),
		Code:       ex.Code,
		Universe:   ex.Universe,
		Benchmarks: ex.Benchmarks,
		Warmup:     ex.Warmup,
		AllowShort: ex.AllowShort,
		NeedsAI:    ex.NeedsModel,
	}, nil
}

func (s *Server) runBacktest(ctx context.Context, in strategyInput) (any, error) {
	plan, spec, opts, err := s.prepare(in)
	if err != nil {
		return nil, err
	}
	// A strategy that consults a model on a schedule cannot be run over the
	// full history without exhausting the budget, so the window is shortened
	// and the result says so rather than quietly returning nothing.
	clamped := app.ClampForAI(&opts, plan.NeedsAI || plan.NeedsWeb)
	if clamped {
		spec.Start = opts.Start
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout(1))
	defer cancel()

	res, err := s.app.Backtest(ctx, spec, opts)
	if err != nil {
		return nil, fmt.Errorf("the backtest did not run: %w", err)
	}
	out := s.runPayload(plan, res)
	if clamped {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"This strategy calls a model or the web, so it was run over the last %d years "+
				"rather than the full history.", app.MaxAIYears))
	}
	return out, nil
}

func (s *Server) runSweep(ctx context.Context, in sweepInput) (any, error) {
	plan, spec, _, err := s.prepare(in.strategyInput)
	if err != nil {
		return nil, err
	}
	if plan.NeedsAI || plan.NeedsWeb {
		return nil, fmt.Errorf("%q calls a model or the web inside the backtest, so it cannot be "+
			"swept: a search would multiply those calls by the number of combinations. "+
			"Run it once with backtest instead", plan.Name)
	}

	// A search is many backtests, so it gets a proportionally larger budget
	// than a single run rather than timing out halfway through the grid.
	ctx, cancel := context.WithTimeout(ctx, s.timeout(10))
	defer cancel()

	res, err := engine.RunSweep(ctx, engine.SweepSpec{
		Base: spec, Grids: in.Grids, Objective: in.Objective,
		MaxCombos: in.MaxCombos, KeepBest: 1,
	}, s.app.Store, nil)
	if err != nil {
		return nil, fmt.Errorf("the search did not run: %w", err)
	}
	return s.sweepPayload(plan, res, in.Top), nil
}

func (s *Server) runWalkForward(ctx context.Context, in walkForwardInput) (any, error) {
	plan, spec, _, err := s.prepare(in.strategyInput)
	if err != nil {
		return nil, err
	}
	if plan.NeedsAI || plan.NeedsWeb {
		return nil, fmt.Errorf("%q calls a model or the web inside the backtest, so it cannot be "+
			"walked forward: each fold searches the grid and would multiply those calls. "+
			"Run it once with backtest instead", plan.Name)
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout(10))
	defer cancel()

	// Embargo zero and embargo unset are different instructions, and JSON
	// cannot tell them apart. Unset must mean "use the strategy's warm-up",
	// which the engine spells -1, or a 200-day average would carry training
	// data into the first test day and the out-of-sample number would quietly
	// inherit the fit.
	embargo := in.Embargo
	if embargo == 0 {
		embargo = -1
	}
	started := time.Now()
	res, err := engine.RunWalkForward(ctx, engine.WalkForwardSpec{
		Base: spec, Grids: in.Grids, TrainDays: in.TrainDays, TestDays: in.TestDays,
		Embargo: embargo, Anchored: in.Anchored, Objective: in.Objective,
		MaxCombos: in.MaxCombos,
	}, s.app.Store, nil)
	if err != nil {
		return nil, fmt.Errorf("the walk-forward evaluation did not run: %w", err)
	}
	// The engine leaves WalkForwardResult.Elapsed unset, and reporting a zero
	// for a job that ran for minutes is worse than reporting nothing, so it is
	// measured here.
	return s.walkForwardPayload(plan, res, time.Since(started).Milliseconds()), nil
}

// timeout scales the configured per-run budget for jobs that are many runs.
func (s *Server) timeout(factor int) time.Duration {
	secs := s.app.Cfg.StrategyTimeoutSec
	if secs <= 0 {
		secs = 3600
	}
	return time.Duration(secs) * time.Second * time.Duration(factor)
}

// resolveUniverse expands each entry in turn rather than joining the list
// first, so ["megacap", "TLT"] means the megacap names plus TLT and ["sp500"]
// travels as an index name the engine resolves per session.
func resolveUniverse(entries []string) ([]string, string) {
	var symbols []string
	var index string
	for _, e := range entries {
		if idx := market.IndexUniverse(e); idx != "" {
			index = idx
			continue
		}
		symbols = append(symbols, market.ResolveUniverse(e)...)
	}
	return market.DedupeSymbols(symbols), index
}

func resolveSymbols(entries []string) []string {
	syms, _ := resolveUniverse(entries)
	return syms
}

func defaultString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
