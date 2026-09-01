package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charbelkassab/pyrite/examples"
	"github.com/charbelkassab/pyrite/internal/app"
	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/ledger"
	"github.com/charbelkassab/pyrite/internal/market"
	"github.com/charbelkassab/pyrite/internal/strategy"
)

// searchSetup is the shared front half of `sweep` and `walkforward`: parse the
// common flags, compile the prompt, and hand back a runnable base spec.
type searchSetup struct {
	app    *app.App
	plan   *strategy.Plan
	spec   engine.Spec
	grids  map[string][]any
	ctx    context.Context
	cancel func()

	objective string
	workers   int
	maxCombos int
	asJSON    bool
	csvPath   string
}

// addCommonSearchFlags registers the flags both search commands share.
func addCommonSearchFlags(fs *flag.FlagSet) (params *paramFlags, objective, csvPath *string, workers, maxCombos *int, cash *float64, from, to, universe, benchmark *string, offline, asJSON *bool) {
	params = &paramFlags{}
	fs.Var(params, "param", "override a grid, e.g. --param fast=10,20,50 (repeatable)")
	objective = fs.String("objective", "sharpe",
		"metric to rank by: "+strings.Join(engine.ObjectiveNames(), ", "))
	csvPath = fs.String("csv", "", "also write the full table to this file")
	workers = fs.Int("workers", 0, "parallel backtests (default: one per CPU)")
	maxCombos = fs.Int("max-combos", 5000, "refuse a search larger than this")
	cash = fs.Float64("cash", 100000, "starting capital")
	from = fs.String("from", "", "start date YYYY-MM-DD")
	to = fs.String("to", "", "end date YYYY-MM-DD")
	universe = fs.String("universe", "", "override the tradable symbols")
	benchmark = fs.String("benchmark", "SPY", "comma separated comparison symbols")
	offline = fs.Bool("offline", false, "use synthetic data and disable network access")
	asJSON = fs.Bool("json", false, "print the full result as JSON")
	return
}

// addIntervalFlag registers the bar-size flag shared by the search commands.
func addIntervalFlag(fs *flag.FlagSet) *string {
	return fs.String("interval", "1d", "bar size: "+strings.Join(market.IntervalNames(), ", "))
}

// addCodeFileFlags registers the compiler bypass shared by both commands.
func addCodeFileFlags(fs *flag.FlagSet) (codeFile *string, warmup *int, example *string) {
	codeFile = fs.String("code-file", "",
		"run this JavaScript strategy instead of compiling a prompt")
	warmup = fs.Int("warmup", 0, "bars of history to load before the start date")
	example = fs.String("example", "",
		"search a bundled example; `pyrite examples` lists them")
	return
}

// paramFlags collects repeated --param name=v1,v2,v3 arguments.
type paramFlags struct {
	grids map[string][]any
}

func (p *paramFlags) String() string { return "" }

func (p *paramFlags) Set(v string) error {
	name, list, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(name) == "" {
		return fmt.Errorf("expected --param name=v1,v2,v3, got %q", v)
	}
	var vals []any
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// Numbers stay numbers so grids compare and sort correctly; anything
		// else is passed through as a string.
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			vals = append(vals, f)
		} else {
			vals = append(vals, raw)
		}
	}
	if len(vals) == 0 {
		return fmt.Errorf("--param %s has no values", name)
	}
	if p.grids == nil {
		p.grids = map[string][]any{}
	}
	p.grids[strings.TrimSpace(name)] = vals
	return nil
}

// cmdSweep searches a strategy's parameter space.
func cmdSweep(args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	params, objective, csvPath, workers, maxCombos, cash, from, to, universe, benchmark, offline, asJSON :=
		addCommonSearchFlags(fs)
	codeFile, warmup, example := addCodeFileFlags(fs)
	interval := addIntervalFlag(fs)
	top := fs.Int("top", 15, "how many rows to print")
	heatmap := fs.Bool("heatmap", true, "draw the parameter surface when two or more vary")

	prompt, flagArgs := splitPromptAndFlags(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	prompt = strings.TrimSpace(strings.Join(append([]string{prompt}, fs.Args()...), " "))
	if prompt == "" && *codeFile == "" && *example == "" {
		return fmt.Errorf("describe a strategy, for example:\n" +
			"  pyrite sweep \"buy SPY when the fast average crosses the slow one\"\n" +
			"  or --code-file strategy.js, or --example golden-cross")
	}

	s, err := prepareSearch(fs, prompt, searchOpts{
		offline: offline, cash: *cash, from: *from, to: *to,
		universe: *universe, benchmark: *benchmark,
		codeFile: *codeFile, warmup: *warmup, example: *example,
		interval: *interval,
	})
	if err != nil {
		return err
	}
	defer s.cancel()

	var lastPct int = -1
	res, err := engine.RunSweep(s.ctx, engine.SweepSpec{
		Base: s.spec, Grids: params.grids, Workers: *workers,
		MaxCombos: *maxCombos, Objective: *objective, KeepBest: 1,
	}, s.app.Store, func(done, total int, row engine.SweepRow) {
		if pct := done * 100 / total; pct != lastPct {
			lastPct = pct
			fmt.Fprintf(os.Stderr, "\rsearching %3d%%  %d/%d", pct, done, total)
		}
	})
	fmt.Fprintf(os.Stderr, "\r%50s\r", "")
	if err != nil {
		return err
	}

	if *csvPath != "" {
		if err := writeSweepCSV(*csvPath, res); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *csvPath)
	}
	note := recordSweep(s, res)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		printLedgerNote(note, true)
		return enc.Encode(res)
	}
	printSweep(s.plan, res, *top, *heatmap)
	printLedgerNote(note, false)
	return nil
}

// recordSweep adds a whole search to the research ledger.
//
// The robustness statistics printed alongside it deflate for the combinations
// this one search tried. The ledger's job is to remember that they were not
// the first, and the spread recorded here is what lets a later plain run —
// which has no spread of its own — say what luck alone would have reached.
func recordSweep(s *searchSetup, res *engine.SweepResult) string {
	spec := s.spec
	if len(res.Best) > 0 {
		// The winner's spec carries the dates as the engine resolved them,
		// which is what a plain run over the same period also records.
		spec = res.Best[0].Spec
	}
	e := ledger.Entry{
		DatasetKey:  ledger.DatasetKey(datasetOf(spec)),
		Strategy:    s.plan.Name,
		Trials:      res.Combos,
		Objective:   res.Objective,
		BestScore:   engine.Ratio(math.NaN()),
		ScoreSpread: engine.Ratio(math.NaN()),
	}
	if len(res.Best) > 0 {
		e.CodeSHA256 = res.Best[0].Manifest.CodeSHA256
	}
	if res.Combos > res.Failed {
		e.BestScore = engine.Ratio(res.Robustness.BestScore)
		e.ScoreSpread = engine.Ratio(res.Robustness.ScoreStdev)
	}
	return recordInvocation(s.app.Cfg, e)
}

// cmdWalkForward optimises on rolling windows and reports out of sample.
func cmdWalkForward(args []string) error {
	fs := flag.NewFlagSet("walkforward", flag.ContinueOnError)
	params, objective, csvPath, workers, maxCombos, cash, from, to, universe, benchmark, offline, asJSON :=
		addCommonSearchFlags(fs)
	codeFile, warmup, example := addCodeFileFlags(fs)
	interval := addIntervalFlag(fs)
	train := fs.Int("train", 504, "training window in trading sessions")
	test := fs.Int("test", 126, "test window in trading sessions")
	embargo := fs.Int("embargo", -1, "sessions dropped between train and test (default: the strategy's warm-up)")
	anchored := fs.Bool("anchored", false, "grow the training window from the start instead of rolling it")

	prompt, flagArgs := splitPromptAndFlags(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	prompt = strings.TrimSpace(strings.Join(append([]string{prompt}, fs.Args()...), " "))
	if prompt == "" && *codeFile == "" && *example == "" {
		return fmt.Errorf("describe a strategy, for example:\n" +
			"  pyrite walkforward \"momentum rotation over the top 3 tech names\"\n" +
			"  or --code-file strategy.js, or --example golden-cross")
	}

	s, err := prepareSearch(fs, prompt, searchOpts{
		offline: offline, cash: *cash, from: *from, to: *to,
		universe: *universe, benchmark: *benchmark,
		codeFile: *codeFile, warmup: *warmup, example: *example,
		interval: *interval,
	})
	if err != nil {
		return err
	}
	defer s.cancel()

	res, err := engine.RunWalkForward(s.ctx, engine.WalkForwardSpec{
		Base: s.spec, Grids: params.grids, TrainDays: *train, TestDays: *test,
		Embargo: *embargo, Anchored: *anchored, Objective: *objective,
		Workers: *workers, MaxCombos: *maxCombos,
	}, s.app.Store, func(fold, total int) {
		fmt.Fprintf(os.Stderr, "\rfold %d/%d", fold, total)
	})
	fmt.Fprintf(os.Stderr, "\r%40s\r", "")
	if err != nil {
		return err
	}

	if *csvPath != "" {
		if err := writeFoldsCSV(*csvPath, res); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *csvPath)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	printWalkForward(s.plan, res)
	return nil
}

type searchOpts struct {
	offline                       *bool
	cash                          float64
	from, to, universe, benchmark string
	// codeFile bypasses the compiler and runs a strategy straight from disk.
	// Besides being how these commands are tested without a model key, it is
	// how you iterate on code you have already generated and hand-edited.
	codeFile string
	warmup   int
	example  string
	interval string
}

// prepareSearch compiles the prompt and builds the base spec both search
// commands run variations of.
func prepareSearch(fs *flag.FlagSet, prompt string, o searchOpts) (*searchSetup, error) {
	a, err := newApp(fs, o.offline)
	if err != nil {
		return nil, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	opts := app.RunOptions{InitialCash: o.cash}
	if o.from != "" {
		d, err := market.ParseDay(o.from)
		if err != nil {
			stop()
			return nil, err
		}
		opts.Start = d
	}
	if o.to != "" {
		d, err := market.ParseDay(o.to)
		if err != nil {
			stop()
			return nil, err
		}
		opts.End = d
	}
	if o.benchmark != "" {
		opts.Benchmarks = market.ResolveUniverse(o.benchmark)
	}
	if o.universe != "" {
		opts.Index = market.IndexUniverse(o.universe)
		opts.Universe = market.ResolveUniverse(o.universe)
	}
	opts.ApplyDefaults()

	if o.example != "" {
		ex, err := examples.Get(o.example)
		if err != nil {
			stop()
			return nil, err
		}
		if len(opts.Universe) == 0 && len(ex.Universe) > 0 {
			opts.Universe = market.ResolveUniverse(strings.Join(ex.Universe, ","))
			opts.Index = market.IndexUniverse(strings.Join(ex.Universe, ","))
		}
		plan := &strategy.Plan{
			Name: firstNonEmpty(ex.Label, ex.Name), Description: firstNonEmpty(ex.Title, ex.Summary),
			Code: ex.Code, Universe: ex.Universe, Benchmarks: ex.Benchmarks,
			Warmup: ex.Warmup, AllowShort: ex.AllowShort,
		}
		spec := app.BuildSpec(plan, "", opts)
		if o.warmup > 0 {
			spec.Warmup = o.warmup
		}
		if err := applyInterval(a, &spec, o.interval); err != nil {
			stop()
			return nil, err
		}
		return &searchSetup{app: a, plan: plan, spec: spec, ctx: ctx, cancel: stop}, nil
	}

	if o.codeFile != "" {
		code, err := os.ReadFile(o.codeFile)
		if err != nil {
			stop()
			return nil, err
		}
		plan := &strategy.Plan{
			Name: filepath.Base(o.codeFile),
			Code: string(code),
		}
		spec := app.BuildSpec(plan, "", opts)
		if o.warmup > 0 {
			spec.Warmup = o.warmup
		}
		if err := applyInterval(a, &spec, o.interval); err != nil {
			stop()
			return nil, err
		}
		return &searchSetup{app: a, plan: plan, spec: spec, ctx: ctx, cancel: stop}, nil
	}

	if !a.Cfg.AnyProviderEnabled() {
		stop()
		return nil, fmt.Errorf("compiling plain English needs a model, and none is configured.\n\n" +
			"  Search a bundled strategy instead — it needs nothing:\n" +
			"    pyrite sweep --example golden-cross\n\n" +
			"  Or turn compilation on:\n" +
			"    free   — install Ollama, then: ollama pull qwen2.5-coder:7b\n" +
			"    hosted — export OPENAI_API_KEY, CEREBRAS_API_KEY or KIMI_API_KEY")
	}

	fmt.Fprintf(os.Stderr, "compiling strategy with %s...\n", a.DescribeRoutes())
	started := time.Now()
	plan, err := a.Compiler.Compile(ctx, strategy.Request{
		Prompt: prompt, Universe: opts.Universe, Start: opts.Start, End: opts.End,
	})
	if err != nil {
		stop()
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "compiled in %s using %s/%s\n\n",
		time.Since(started).Round(time.Millisecond), plan.Provider, plan.Model)

	spec := app.BuildSpec(plan, prompt, opts)
	if err := applyInterval(a, &spec, o.interval); err != nil {
		stop()
		return nil, err
	}
	return &searchSetup{app: a, plan: plan, spec: spec, ctx: ctx, cancel: stop}, nil
}

// applyInterval resolves and validates a bar size onto a spec.
func applyInterval(a *app.App, spec *engine.Spec, name string) error {
	iv, err := market.ParseInterval(name)
	if err != nil {
		return err
	}
	if !a.Store.SupportsInterval(iv) {
		return fmt.Errorf("the configured data provider serves daily bars only, so %s is unavailable.\n"+
			"  Yahoo serves intraday: set PYRITE_DATA_PROVIDERS=yahoo", iv)
	}
	spec.Interval = iv
	return nil
}

// printSweep renders the search as a ranked table, a surface and a verdict.
func printSweep(plan *strategy.Plan, res *engine.SweepResult, top int, heatmap bool) {
	fmt.Printf("%s\n", plan.Name)
	fmt.Printf("%d combinations in %s, ranked by %s\n\n",
		res.Combos, time.Duration(res.Elapsed)*time.Millisecond, res.Objective)

	rows := res.Sorted()
	if top > len(rows) {
		top = len(rows)
	}
	fmt.Printf("  %-28s %9s %10s %10s %10s %7s\n",
		"", res.Objective, "return", "drawdown", "trades", "win%")
	for _, r := range rows[:top] {
		if r.Error != "" {
			fmt.Printf("  %-28s %9s   %s\n", truncate(r.Label, 28), "—", truncate(r.Error, 40))
			continue
		}
		fmt.Printf("  %-28s %9s %10s %10s %10d %7s\n",
			truncate(r.Label, 28), ratio(r.Score), pct(r.TotalReturn),
			pct(r.MaxDrawdown), r.Trades, pct(r.WinRate))
	}
	if len(rows) > top {
		fmt.Printf("  ... and %d more\n", len(rows)-top)
	}
	if res.Failed > 0 {
		fmt.Printf("\n  %d combinations failed\n", res.Failed)
	}

	if heatmap && len(res.Axes) >= 2 {
		printHeatmap(res, res.Axes[0], res.Axes[1])
	}
	printRobustness(res.Robustness, res.Objective)
}

// printHeatmap draws the parameter surface in the terminal.
//
// A heatmap is the fastest overfitting detector there is: one bright cell in a
// dark field is a fluke, a broad warm region is an edge, and the eye reads
// that distinction instantly in a way no summary statistic conveys.
func printHeatmap(res *engine.SweepResult, xAxis, yAxis string) {
	xs, ys, z := res.Surface(xAxis, yAxis)
	if len(xs) == 0 || len(ys) == 0 {
		return
	}

	lo, hi := math.Inf(1), math.Inf(-1)
	for _, row := range z {
		for _, v := range row {
			if math.IsNaN(v) {
				continue
			}
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
	}
	if math.IsInf(lo, 0) || hi <= lo {
		return
	}

	// A shading ramp rather than colour, so the output survives a pipe, a log
	// file and a terminal with no colour support. Indexed as runes: several
	// of these characters are multi-byte, and slicing the string by byte
	// emits mojibake.
	ramp := []rune(" .:-=+*#%@")

	// Column width is set by the widest label on either axis, so the grid
	// lines up whatever the parameter values happen to be.
	labels := make([]string, len(xs))
	cell := 2
	for i, x := range xs {
		labels[i] = formatAxisValue(x)
		if n := len(labels[i]) + 1; n > cell {
			cell = n
		}
	}
	rowLabels := make([]string, len(ys))
	gutter := 0
	for i, y := range ys {
		rowLabels[i] = formatAxisValue(y)
		if n := len(rowLabels[i]); n > gutter {
			gutter = n
		}
	}

	fmt.Printf("\n%s across, %s down, shaded by %s\n\n", xAxis, yAxis, res.Objective)
	for i := len(ys) - 1; i >= 0; i-- {
		fmt.Printf("  %*s │", gutter, rowLabels[i])
		for j := range xs {
			v := z[i][j]
			if math.IsNaN(v) {
				fmt.Printf("%*s", cell, "?")
				continue
			}
			idx := int((v - lo) / (hi - lo) * float64(len(ramp)-1))
			if idx < 0 {
				idx = 0
			}
			if idx >= len(ramp) {
				idx = len(ramp) - 1
			}
			fmt.Printf("%*c", cell, ramp[idx])
		}
		fmt.Println()
	}

	fmt.Printf("  %*s └", gutter, "")
	for range xs {
		fmt.Print(strings.Repeat("─", cell))
	}
	fmt.Println()
	fmt.Printf("  %*s  ", gutter, "")
	for _, l := range labels {
		fmt.Printf("%*s", cell, l)
	}
	fmt.Println()
	fmt.Printf("\n  %*s  %.3f %c   worst   %s   best   %.3f %c\n", gutter, "",
		lo, ramp[1], string(ramp[2:len(ramp)-1]), hi, ramp[len(ramp)-1])
	fmt.Printf("  %*s  A broad warm region is an edge. One bright cell in a dark\n", gutter, "")
	fmt.Printf("  %*s  field is a fluke, however good its number looks.\n", gutter, "")
}

// formatAxisValue renders a parameter value as a bare axis label.
func formatAxisValue(v any) string {
	// FormatParams writes "name=value"; with an empty name that is "=value".
	return strings.TrimPrefix(engine.FormatParams(map[string]any{"": v}), "=")
}

// printRobustness reports the overfitting assessment.
func printRobustness(r engine.Robustness, objective string) {
	fmt.Printf("\nHow much of this is real?\n")
	fmt.Printf("  %-30s %14.3f\n", "Best "+objective, r.BestScore)
	fmt.Printf("  %-30s %14.3f\n", "Median "+objective, r.MedianScore)
	if r.ExpectedMaxScore != 0 {
		fmt.Printf("  %-30s %14.3f\n", "Expected best from luck alone", r.ExpectedMaxScore)
	}
	fmt.Printf("  %-30s %14s\n", "Combinations above zero", pct(r.PositiveShare))
	if r.PlateauRatio.Defined() {
		fmt.Printf("  %-30s %14s\n", "Neighbour support", pct(float64(r.PlateauRatio)))
	}
	if r.PBO.Defined() {
		fmt.Printf("  %-30s %14s   (%d splits)\n", "Prob. of backtest overfitting",
			pct(float64(r.PBO)), r.PBOSplits)
	}
	if r.DeflatedSharpe.Defined() {
		fmt.Printf("  %-30s %14s\n", "Deflated Sharpe", pct(float64(r.DeflatedSharpe)))
	}
	if r.Verdict != "" {
		fmt.Printf("\n  %s\n", wrapIndent(r.Verdict, 74, "  "))
	}
}

// printWalkForward renders the fold-by-fold out-of-sample report.
func printWalkForward(plan *strategy.Plan, res *engine.WalkForwardResult) {
	fmt.Printf("%s\n", plan.Name)
	fmt.Printf("%d folds, ranked by %s in training\n\n", len(res.Folds), res.Objective)

	fmt.Printf("  %-4s %-11s %-11s %-24s %10s %10s\n",
		"fold", "test from", "to", "chosen", "in-sample", "out")
	for _, f := range res.Folds {
		if f.Error != "" {
			fmt.Printf("  %-4d %-11s %-11s %s\n", f.Index, f.TestStart, f.TestEnd, truncate(f.Error, 45))
			continue
		}
		fmt.Printf("  %-4d %-11s %-11s %-24s %10s %10s\n",
			f.Index, f.TestStart, f.TestEnd, truncate(engine.FormatParams(f.BestParams), 24),
			pct(f.TrainMetrics.TotalReturn), pct(f.TestMetrics.TotalReturn))
	}

	m := res.StitchedMetrics
	fmt.Printf("\nStitched out-of-sample equity — the only curve here that was never fitted to\n")
	fmt.Printf("  %-30s %14s\n", "Total return", pct(m.TotalReturn))
	fmt.Printf("  %-30s %14s\n", "Annualised (CAGR)", pct(m.CAGR))
	fmt.Printf("  %-30s %14s\n", "Sharpe ratio", ratio(m.Sharpe))
	fmt.Printf("  %-30s %14s\n", "Max drawdown", pct(m.MaxDrawdown))
	fmt.Printf("  %-30s %14s\n", "Ulcer index", pct(res.StitchedRisk.UlcerIndex))

	fmt.Printf("\n  %-30s %14s\n", "Mean in-sample return", pct(res.InSampleReturn))
	fmt.Printf("  %-30s %14s\n", "Mean out-of-sample return", pct(res.OutOfSampleMean))
	if res.Efficiency.Defined() {
		fmt.Printf("  %-30s %14s\n", "Walk-forward efficiency", pct(float64(res.Efficiency)))
	}
	fmt.Printf("  %-30s %11d / %2d\n", "Positive test windows", res.ConsistentFolds, len(res.Folds))
	fmt.Printf("  %-30s %14s\n", "Parameter stability", pct(res.ParamStability))

	if res.Verdict != "" {
		fmt.Printf("\n  %s\n", wrapIndent(res.Verdict, 74, "  "))
	}
}

// writeSweepCSV exports the full table.
func writeSweepCSV(path string, res *engine.SweepResult) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	header := append(append([]string{}, res.Axes...),
		"score", "total_return", "cagr", "sharpe", "sortino", "calmar",
		"max_drawdown", "volatility", "trades", "win_rate", "turnover",
		"ulcer_index", "expectancy", "error")
	if err := w.Write(header); err != nil {
		return err
	}
	for _, r := range res.Sorted() {
		rec := make([]string, 0, len(header))
		for _, ax := range res.Axes {
			rec = append(rec, engine.FormatParams(map[string]any{"": r.Params[ax]})[1:])
		}
		rec = append(rec,
			num(float64(r.Score)), num(r.TotalReturn), num(r.CAGR),
			num(float64(r.Sharpe)), num(float64(r.Sortino)), num(float64(r.Calmar)),
			num(r.MaxDrawdown), num(r.Volatility), strconv.Itoa(r.Trades),
			num(r.WinRate), num(r.Turnover), num(r.UlcerIndex), num(r.Expectancy), r.Error)
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// writeFoldsCSV exports the walk-forward folds.
func writeFoldsCSV(path string, res *engine.WalkForwardResult) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write([]string{"fold", "train_start", "train_end", "test_start", "test_end",
		"params", "train_return", "test_return", "train_score", "test_score", "error"}); err != nil {
		return err
	}
	for _, fold := range res.Folds {
		if err := w.Write([]string{
			strconv.Itoa(fold.Index), string(fold.TrainStart), string(fold.TrainEnd),
			string(fold.TestStart), string(fold.TestEnd),
			engine.FormatParams(fold.BestParams),
			num(fold.TrainMetrics.TotalReturn), num(fold.TestMetrics.TotalReturn),
			num(float64(fold.TrainScore)), num(float64(fold.TestScore)), fold.Error,
		}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// num formats a float for CSV, leaving undefined values empty rather than
// writing NaN, which most spreadsheets read as text.
func num(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return ""
	}
	return strconv.FormatFloat(v, 'f', 6, 64)
}

// cmdImprove runs a guided search under a withheld holdout period.
func cmdImprove(args []string) error {
	fs := flag.NewFlagSet("improve", flag.ContinueOnError)
	_, objective, csvPath, workers, maxCombos, cash, from, to, universe, benchmark, offline, asJSON :=
		addCommonSearchFlags(fs)
	codeFile, warmup, example := addCodeFileFlags(fs)
	interval := addIntervalFlag(fs)
	budget := fs.Int("budget", 6, "how many candidates to try")
	holdout := fs.Float64("holdout", 0.3, "fraction of the period withheld from the search")
	sweepParams := fs.Bool("sweep-params", true, "also search each candidate's declared parameters")
	goal := fs.String("goal", "", "what to improve, in your own words")

	prompt, flagArgs := splitPromptAndFlags(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	prompt = strings.TrimSpace(strings.Join(append([]string{prompt}, fs.Args()...), " "))
	if prompt == "" && *codeFile == "" && *example == "" {
		return fmt.Errorf("describe a strategy to improve, for example:\n" +
			"  pyrite improve \"a golden cross on SPY\"\n" +
			"  or --code-file strategy.js, or --example golden-cross")
	}
	_ = csvPath

	s, err := prepareSearch(fs, prompt, searchOpts{
		offline: offline, cash: *cash, from: *from, to: *to,
		universe: *universe, benchmark: *benchmark,
		codeFile: *codeFile, warmup: *warmup, example: *example,
		interval: *interval,
	})
	if err != nil {
		return err
	}
	defer s.cancel()

	if !s.app.Cfg.AnyProviderEnabled() {
		return fmt.Errorf("a guided search needs a model. Set OPENAI_API_KEY, CEREBRAS_API_KEY or KIMI_API_KEY")
	}

	proposer := &app.ModelProposer{App: s.app, Goal: *goal, Seed: s.plan}
	res, err := engine.RunAgent(s.ctx, engine.AgentSpec{
		Base: s.spec, Budget: *budget, HoldoutFraction: *holdout,
		Objective: *objective, SweepParams: *sweepParams,
		MaxCombos: *maxCombos, Workers: *workers,
	}, s.app.Store, proposer, func(n, budget int, c engine.Candidate) {
		if c.Error != "" {
			fmt.Fprintf(os.Stderr, "  %d/%d  failed: %s\n", n, budget, truncate(c.Error, 60))
			return
		}
		fmt.Fprintf(os.Stderr, "  %d/%d  training %s: %s   return %s\n",
			n, budget, *objective, ratio(c.TrainScore), pct(c.TrainMetrics.TotalReturn))
	})
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	printAgent(res)
	return nil
}

// printAgent renders the search and the single holdout measurement.
func printAgent(res *engine.AgentResult) {
	fmt.Printf("\nSearched %s to %s. Held back %s to %s.\n",
		res.TrainStart, res.TrainEnd, res.HoldoutStart, res.HoldoutEnd)
	fmt.Printf("The holdout was not visible during the search and was scored once, at the end.\n\n")

	fmt.Printf("  %-3s %10s %10s %10s %7s  %s\n", "#", "return", "CAGR", "drawdown", "trust", "what changed")
	for i, c := range res.Candidates {
		marker := " "
		if i == res.BestIndex {
			marker = "*"
		}
		if c.Error != "" {
			fmt.Printf("%s %-3d %s\n", marker, c.Iteration, truncate(c.Error, 60))
			continue
		}
		fmt.Printf("%s %-3d %10s %10s %10s %7d  %s\n", marker, c.Iteration,
			pct(c.TrainMetrics.TotalReturn), pct(c.TrainMetrics.CAGR),
			pct(c.TrainMetrics.MaxDrawdown), c.Critique.TrustScore,
			truncate(c.Rationale, 44))
	}

	if res.BestIndex < 0 || res.Holdout == nil {
		if res.Verdict != "" {
			fmt.Printf("\n  %s\n", wrapIndent(res.Verdict, 74, "  "))
		}
		return
	}

	best := res.Candidates[res.BestIndex]
	h := res.Holdout.Metrics
	fmt.Printf("\nThe winner, on data the search never saw\n")
	fmt.Printf("  %-24s %14s %14s\n", "", "training", "holdout")
	fmt.Printf("  %-24s %14s %14s\n", "Total return", pct(best.TrainMetrics.TotalReturn), pct(h.TotalReturn))
	fmt.Printf("  %-24s %14s %14s\n", "Annualised (CAGR)", pct(best.TrainMetrics.CAGR), pct(h.CAGR))
	fmt.Printf("  %-24s %14s %14s\n", "Sharpe ratio", ratio(best.TrainMetrics.Sharpe), ratio(h.Sharpe))
	fmt.Printf("  %-24s %14s %14s\n", "Max drawdown", pct(best.TrainMetrics.MaxDrawdown), pct(h.MaxDrawdown))
	if res.Degradation.Defined() {
		fmt.Printf("  %-24s %29s\n", "Surviving fraction", pct(float64(res.Degradation)))
	}

	if res.Verdict != "" {
		fmt.Printf("\n  %s\n", wrapIndent(res.Verdict, 74, "  "))
	}
	printCritique(res.Holdout)
}

// cmdReport runs the full battery and writes a document.
func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	params, objective, _, workers, maxCombos, cash, from, to, universe, benchmark, offline, asJSON :=
		addCommonSearchFlags(fs)
	codeFile, warmup, example := addCodeFileFlags(fs)
	interval := addIntervalFlag(fs)
	out := fs.String("out", "", "write the report here instead of stdout")
	skipSweep := fs.Bool("no-sweep", false, "skip the parameter search")
	skipWF := fs.Bool("no-walkforward", false, "skip the walk-forward evaluation")
	train := fs.Int("train", 504, "walk-forward training window in sessions")
	test := fs.Int("test", 126, "walk-forward test window in sessions")

	prompt, flagArgs := splitPromptAndFlags(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	prompt = strings.TrimSpace(strings.Join(append([]string{prompt}, fs.Args()...), " "))
	if prompt == "" && *codeFile == "" && *example == "" {
		return fmt.Errorf("describe a strategy to report on, or pass --code-file")
	}

	s, err := prepareSearch(fs, prompt, searchOpts{
		offline: offline, cash: *cash, from: *from, to: *to,
		universe: *universe, benchmark: *benchmark,
		codeFile: *codeFile, warmup: *warmup, example: *example,
		interval: *interval,
	})
	if err != nil {
		return err
	}
	defer s.cancel()

	rep := &engine.Report{
		Title:       s.plan.Name,
		Prompt:      prompt,
		Generated:   time.Now(),
		Assumptions: s.plan.Assumptions,
		Limitations: s.plan.Limitations,
	}

	// 1. The full-period backtest, which everything else contextualises.
	fmt.Fprintf(os.Stderr, "backtesting...\n")
	rep.Run, err = engine.New(s.spec, s.app.Store).Run(s.ctx)
	if err != nil {
		return err
	}

	// 2. The parameter space, if the strategy declares one.
	if !*skipSweep {
		fmt.Fprintf(os.Stderr, "searching the parameter space...\n")
		sw, err := engine.RunSweep(s.ctx, engine.SweepSpec{
			Base: s.spec, Grids: params.grids, Objective: *objective,
			MaxCombos: *maxCombos, Workers: *workers, KeepBest: 1,
		}, s.app.Store, nil)
		if err != nil {
			// A strategy with no declared parameters has no space to search.
			// That is a fact about the strategy, not a failure of the report.
			fmt.Fprintf(os.Stderr, "  skipped: %s\n", truncate(err.Error(), 70))
		} else {
			rep.Sweep = sw
		}
	}

	// 3. Out of sample.
	if !*skipWF {
		fmt.Fprintf(os.Stderr, "walking forward...\n")
		wf, err := engine.RunWalkForward(s.ctx, engine.WalkForwardSpec{
			Base: s.spec, Grids: params.grids, TrainDays: *train, TestDays: *test,
			Objective: *objective, Workers: *workers, MaxCombos: *maxCombos,
		}, s.app.Store, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  skipped: %s\n", truncate(err.Error(), 70))
		} else {
			rep.WalkForward = wf
		}
	}

	// 4. Cost sensitivity and the bootstrap, both cheap once the rest is done.
	fmt.Fprintf(os.Stderr, "scanning costs...\n")
	if scan, err := engine.RunCostScan(s.ctx, s.spec, s.app.Store, nil); err == nil {
		rep.Costs = scan
	}
	rep.Bootstrap = engine.Bootstrap(rep.Run.Curve, 2000, 21, s.spec.Seed)

	// 5. What is left once known risk premia are taken out. The proxies are
	//    ETFs, so this needs price data the run itself did not necessarily
	//    load; a period the funds do not cover costs the section, not the
	//    document.
	fmt.Fprintf(os.Stderr, "decomposing against factors...\n")
	if fx, err := engine.AnalyseFactors(s.ctx, rep.Run.Curve, s.app.Store, s.spec.Interval,
		engine.ScaleFor(s.spec.Interval, s.spec.RiskFreeRate), nil); err == nil {
		rep.Factors = fx
	} else {
		fmt.Fprintf(os.Stderr, "  skipped: %s\n", truncate(err.Error(), 70))
	}

	// 6. The prose, when a model is available. Everything above stands
	//    without it, so a missing key costs a paragraph, not the document.
	if s.app.Cfg.AnyProviderEnabled() {
		fmt.Fprintf(os.Stderr, "writing the summary...\n")
		if narrative, err := s.app.Narrate(s.ctx, rep); err == nil {
			rep.Narrative = narrative
		} else {
			fmt.Fprintf(os.Stderr, "  skipped: %s\n", truncate(err.Error(), 70))
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	doc := rep.Markdown()
	if *out == "" {
		fmt.Print(doc)
		return nil
	}
	if err := os.WriteFile(*out, []byte(doc), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", *out, len(doc))
	return nil
}
