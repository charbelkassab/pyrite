package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charbelkassab/pyrite/examples"
	"github.com/charbelkassab/pyrite/internal/app"
	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
	"github.com/charbelkassab/pyrite/internal/strategy"
)

// strategySource is one side of the comparison, as named on the command line.
type strategySource struct {
	// kind is "example" or "code-file"; value is the name or the path.
	kind, value string
}

// sourceList collects --example and --code-file across both flag names, in
// command-line order.
//
// The flag shape is deliberately repetition rather than a pair of prefixed
// flags (--a-example, --b-code-file) or two positional arguments. Repetition
// keeps the two sides symmetric, lets them be of different kinds —
//
//	pyrite diff --example golden-cross --code-file tweaked.js
//
// which is the common case, since the usual comparison is a bundled or saved
// strategy against the version you just edited — and it means the flag names
// are the same ones `pyrite run` already uses, so nothing new has to be
// learned. The cost is that order carries meaning: the first is A, the second
// is B. That is stated in the header of every report so it cannot be misread.
type sourceList struct{ items []strategySource }

// sourceFlag binds one flag name into the shared list.
type sourceFlag struct {
	kind string
	list *sourceList
}

func (f sourceFlag) String() string { return "" }

func (f sourceFlag) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("--%s needs a value", f.kind)
	}
	f.list.items = append(f.list.items, strategySource{f.kind, v})
	return nil
}

// cmdDiff runs two strategies over one setup and tests whether the difference
// between them is real.
func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	var sources sourceList
	fs.Var(sourceFlag{"example", &sources}, "example",
		"a bundled example to compare; give the flag twice, or once alongside --code-file")
	fs.Var(sourceFlag{"code-file", &sources}, "code-file",
		"a JavaScript strategy file to compare; give the flag twice, or once alongside --example")

	from := fs.String("from", "", "start date YYYY-MM-DD")
	to := fs.String("to", "", "end date YYYY-MM-DD")
	cash := fs.Float64("cash", 100000, "starting capital")
	universe := fs.String("universe", "", "override the tradable symbols for both")
	interval := fs.String("interval", "1d", "bar size: "+strings.Join(market.IntervalNames(), ", "))
	fillClose := fs.Bool("fill-close", false, "fill at the same day's close instead of the next open")
	impact := fs.Float64("impact", 0, "market impact coefficient; 1 is the usual estimate")
	warmup := fs.Int("warmup", 0, "bars of history to load before the start date")
	offline := fs.Bool("offline", false, "use synthetic data and disable network access")
	asJSON := fs.Bool("json", false, "print the comparison as JSON")
	seed := fs.Int64("seed", 1, "seed for the bootstrap interval on the Sharpe difference")
	bootstraps := fs.Int("bootstraps", 0, "resamples behind that interval (default 1000)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(sources.items) != 2 {
		return fmt.Errorf("diff compares exactly two strategies; %d given — name them with "+
			"--example or --code-file, in either combination:\n"+
			"  pyrite diff --example golden-cross --example mean-reversion\n"+
			"  pyrite diff --example golden-cross --code-file tweaked.js\n"+
			"  pyrite diff --code-file before.js --code-file after.js\n"+
			"The first is A and the second is B.", len(sources.items))
	}

	a, err := newApp(fs, offline)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// One set of run options, built once and used for both sides. Building
	// each side from its own options would let a typo change the experiment
	// rather than the strategy, which is the mistake the whole command is
	// about.
	opts := app.RunOptions{InitialCash: *cash}
	if *from != "" {
		d, err := market.ParseDay(*from)
		if err != nil {
			return err
		}
		opts.Start = d
	}
	if *to != "" {
		d, err := market.ParseDay(*to)
		if err != nil {
			return err
		}
		opts.End = d
	}
	if *universe != "" {
		opts.Index = market.IndexUniverse(*universe)
		opts.Universe = market.ResolveUniverse(*universe)
	}
	if *fillClose {
		opts.Fill = engine.FillClose
	}
	opts.ApplyDefaults()

	iv, err := market.ParseInterval(*interval)
	if err != nil {
		return err
	}
	if !a.Store.SupportsInterval(iv) {
		return fmt.Errorf("the configured data provider serves daily bars only, so %s is unavailable.\n"+
			"  Yahoo serves intraday: set PYRITE_DATA_PROVIDERS=yahoo", iv)
	}

	results := make([]*engine.Result, 2)
	for i, src := range sources.items {
		plan, err := planForSource(a, src)
		if err != nil {
			return err
		}

		spec := app.BuildSpec(plan, "", opts)
		if *warmup > 0 {
			spec.Warmup = *warmup
		}
		spec.Costs.ImpactCoefficient = *impact
		spec.Interval = iv
		// Benchmarks are not part of the comparison and each example declares
		// its own, so they are dropped: nothing here reads them, and loading
		// them would be one more way the two runs could differ.
		spec.Benchmarks = nil

		side := "A"
		if i == 1 {
			side = "B"
		}
		lastPct := -1
		opts.Progress = func(done, total int, day market.Day) {
			if pct := done * 100 / total; pct != lastPct {
				lastPct = pct
				fmt.Fprintf(os.Stderr, "\rbacktesting %s %3d%%  %s", side, pct, day)
			}
		}
		res, err := a.Backtest(ctx, spec, opts)
		if err != nil {
			return fmt.Errorf("running %s (%s): %w", side, src.value, err)
		}
		fmt.Fprintf(os.Stderr, "\r%44s\r", "")
		results[i] = res
	}

	d, err := engine.CompareRuns(results[0], results[1], engine.DiffOptions{
		Bootstraps: *bootstraps, Seed: *seed,
	})
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(d)
	}
	printDiff(d)
	return nil
}

// planForSource turns one --example or --code-file into a runnable plan.
//
// An example's own universe is adopted only when the caller named none, which
// is the same precedence `pyrite run` uses. With two examples that declare
// different universes, both keep their own — see setupMismatch for why that is
// allowed and what is said about it.
func planForSource(a *app.App, src strategySource) (*strategy.Plan, error) {
	if src.kind == "code-file" {
		code, err := os.ReadFile(src.value)
		if err != nil {
			return nil, err
		}
		return &strategy.Plan{Name: filepath.Base(src.value), Code: string(code)}, nil
	}
	ex, err := examples.Get(src.value)
	if err != nil {
		return nil, err
	}
	if ex.NeedsModel && !a.Cfg.AnyProviderEnabled() {
		return nil, fmt.Errorf("the %q example calls a model inside the backtest, so it needs one.\n"+
			"Every other example runs with nothing: pyrite examples", ex.Name)
	}
	name := ex.Label
	if name == "" {
		name = ex.Name
	}
	return &strategy.Plan{
		Name: name, Description: firstNonEmpty(ex.Title, ex.Summary), Code: ex.Code,
		Universe: expandUniverse(ex.Universe), Benchmarks: ex.Benchmarks,
		Warmup: ex.Warmup, AllowShort: ex.AllowShort,
	}, nil
}

// expandUniverse turns a declared universe into the symbols the engine loads.
//
// A named universe such as "megacap" has to be expanded here or the engine
// goes looking for a ticker of that name. A point-in-time index name must not
// be expanded — it has no static answer, and travels on the spec for
// BuildSpec to resolve per session — so those are passed through untouched.
//
// The expansion happens on the plan rather than on the shared run options,
// which is where `pyrite run` does it. With one strategy the two are the same
// thing; with two, writing into the shared options would let the first side's
// universe become the second side's, and the comparison would then be of two
// strategies over one strategy's symbols.
func expandUniverse(declared []string) []string {
	out := make([]string, 0, len(declared))
	for _, u := range declared {
		if market.IndexUniverse(u) != "" {
			out = append(out, u)
			continue
		}
		out = append(out, market.ResolveUniverse(u)...)
	}
	return out
}

// printDiff renders the comparison to stdout.
func printDiff(d *engine.Diff) {
	fmt.Printf("A  %s\nB  %s\n", d.A.Name, d.B.Name)
	fmt.Printf("\n%s to %s   %d paired sessions", d.From, d.To, d.Sessions)
	if u := d.UnpairedA + d.UnpairedB; u > 0 {
		fmt.Printf("   %d unpaired, dropped", u)
	}
	fmt.Printf("\n\n")

	fmt.Printf("  %-22s %14s %14s %14s\n", "", "A", "B", "B - A")
	fmt.Printf("  %-22s %14s %14s %14s\n", "Total return",
		pct(d.A.TotalReturn), pct(d.B.TotalReturn), pct(d.B.TotalReturn-d.A.TotalReturn))
	fmt.Printf("  %-22s %14s %14s %14s\n", "Annualised (CAGR)",
		pct(d.A.CAGR), pct(d.B.CAGR), pct(d.B.CAGR-d.A.CAGR))
	fmt.Printf("  %-22s %14s %14s %14s\n", "Sharpe ratio",
		ratio(d.A.Sharpe), ratio(d.B.Sharpe), ratio(d.Sharpe.Difference))
	fmt.Printf("  %-22s %14s %14s %14s\n", "Max drawdown",
		pct(d.A.MaxDrawdown), pct(d.B.MaxDrawdown), pct(d.B.MaxDrawdown-d.A.MaxDrawdown))
	fmt.Printf("  %-22s %14d %14d %14d\n", "Trades", d.A.Trades, d.B.Trades, d.B.Trades-d.A.Trades)
	fmt.Printf("  %-22s %14s %14s %14s\n", "Turnover",
		fmt.Sprintf("%.2fx", d.A.Turnover), fmt.Sprintf("%.2fx", d.B.Turnover),
		fmt.Sprintf("%.2fx", d.B.Turnover-d.A.Turnover))

	p := d.Paired
	fmt.Printf("\nIs the difference real?\n")
	fmt.Printf("  %-30s %14s\n", "Mean difference, annualised", pct(p.AnnualDifference))
	fmt.Printf("  %-30s %14s\n", "Newey-West standard error", pctRatio(p.AnnualStdErr))
	fmt.Printf("  %-30s %14s\n", "t-statistic (paired)", ratio(p.TStat))
	fmt.Printf("  %-30s %14s\n", "  same, sessions independent", ratio(p.NaiveTStat))
	fmt.Printf("  %-30s %14s\n", "p-value", pvalue(p.PValue))
	fmt.Printf("  %-30s %14d\n", "Newey-West lag, bars", p.NeweyWestLag)
	fmt.Printf("  %-30s %14s\n", "Sessions B beat A", pctRatio(p.WinShare))

	s := d.Sharpe
	fmt.Printf("\n  %-30s %14s\n", "Sharpe difference", ratio(s.Difference))
	if s.CILow.Defined() && s.CIHigh.Defined() {
		fmt.Printf("  %-30s %14s\n", "  95% bootstrap interval",
			fmt.Sprintf("%s to %s", ratio(s.CILow), ratio(s.CIHigh)))
		fmt.Printf("  %-30s %14s\n", "  resamples, block, seed",
			fmt.Sprintf("%d / %d / %d", s.Bootstraps, s.BlockLength, s.Seed))
	}

	fmt.Printf("\nOr are they the same strategy?\n")
	fmt.Printf("  %-30s %14s\n", "Return correlation", ratio(d.Overlap.Correlation))
	if d.Overlap.SameHoldings.Defined() {
		fmt.Printf("  %-30s %14s\n", "Same book at the close", pctRatio(d.Overlap.SameHoldings))
	}

	if d.Verdict != "" {
		fmt.Printf("\n  %s\n", wrapIndent(d.Verdict, 74, "  "))
	}
	if len(d.Findings) > 0 {
		fmt.Printf("\nWhat is wrong with this comparison:\n")
		for _, f := range d.Findings {
			fmt.Printf("\n  [%s] %s\n", f.Severity, f.Title)
			fmt.Printf("  %s\n", wrapIndent(f.Detail, 74, "  "))
		}
	}
	fmt.Println()
}

// pvalue prints a p-value, or n/a when there is none.
func pvalue(r engine.Ratio) string {
	if !r.Defined() {
		return "n/a"
	}
	return fmt.Sprintf("%.3f", float64(r))
}
