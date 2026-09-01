package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charbelkassab/pyrite/internal/app"
	"github.com/charbelkassab/pyrite/internal/bundle"
	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/ledger"
	"github.com/charbelkassab/pyrite/internal/market"
)

const bundleUsage = `pyrite bundle — package a backtest so somebody else can re-run it.

  pyrite bundle export --out run.pyrite --example golden-cross
  pyrite bundle run run.pyrite
  pyrite bundle show run.pyrite

A bundle carries the strategy, the resolved spec and every price bar the run
read. Re-running one needs no network, no keys and no cached data, and either
reproduces the numbers exactly or names the session they parted on.
`

func cmdBundle(args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "export":
		return cmdBundleExport(args)
	case "run":
		return cmdBundleRun(args)
	case "show":
		return cmdBundleShow(args)
	case "help", "":
		fmt.Print(bundleUsage)
		if sub == "" {
			return fmt.Errorf("say what to do: export, run or show")
		}
		return nil
	default:
		fmt.Print(bundleUsage)
		return fmt.Errorf("unknown bundle command %q", sub)
	}
}

func cmdBundleExport(args []string) error {
	fs := flag.NewFlagSet("bundle export", flag.ExitOnError)
	out := fs.String("out", "", "write the bundle here (default run.pyrite)")
	from := fs.String("from", "", "start date YYYY-MM-DD")
	to := fs.String("to", "", "end date YYYY-MM-DD")
	cash := fs.Float64("cash", 100000, "starting capital")
	benchmark := fs.String("benchmark", "SPY", "comma separated comparison symbols")
	universe := fs.String("universe", "", "override the tradable symbols")
	offline := fs.Bool("offline", false, "use synthetic data and disable network access")
	fillClose := fs.Bool("fill-close", false, "fill at the same day's close instead of the next open")
	codeFile := fs.String("code-file", "", "bundle this JavaScript strategy instead of compiling a prompt")
	example := fs.String("example", "", "bundle a bundled example; `pyrite examples` lists them")
	impact := fs.Float64("impact", 0, "market impact coefficient; 1 is the usual estimate")
	interval := fs.String("interval", "1d", "bar size: "+strings.Join(market.IntervalNames(), ", "))
	warmupFlag := fs.Int("warmup", 0, "bars of history to load before the start date")
	asJSON := fs.Bool("json", false, "print the manifest as JSON")

	prompt, flagArgs := splitPromptAndFlags(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	prompt = strings.TrimSpace(strings.Join(append([]string{prompt}, fs.Args()...), " "))
	if prompt == "" && *codeFile == "" && *example == "" {
		return fmt.Errorf("say what to bundle, for example:\n" +
			"  pyrite bundle export --out run.pyrite --example golden-cross\n" +
			"  pyrite bundle export --out run.pyrite --code-file mine.js --from 2018-01-02")
	}
	path := *out
	if path == "" {
		path = "run.pyrite"
	}

	a, err := newApp(fs, offline)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	if *benchmark != "" {
		opts.Benchmarks = market.ResolveUniverse(*benchmark)
	}
	if *universe != "" {
		opts.Index = market.IndexUniverse(*universe)
		opts.Universe = market.ResolveUniverse(*universe)
	}
	if *fillClose {
		opts.Fill = engine.FillClose
	}
	opts.ApplyDefaults()

	plan, err := loadPlan(ctx, a, planSource{
		prompt: prompt, example: *example, codeFile: *codeFile,
		benchmarkFlag: *benchmark,
	}, &opts)
	if err != nil {
		return err
	}

	spec := app.BuildSpec(plan, prompt, opts)
	if *warmupFlag > 0 {
		spec.Warmup = *warmupFlag
	}
	spec.Costs.ImpactCoefficient = *impact
	iv, err := market.ParseInterval(*interval)
	if err != nil {
		return err
	}
	if !a.Store.SupportsInterval(iv) {
		return fmt.Errorf("the configured data provider serves daily bars only, so %s is unavailable.\n"+
			"  Yahoo serves intraday: set PYRITE_DATA_PROVIDERS=yahoo", iv)
	}
	spec.Interval = iv

	lastPct := -1
	opts.Progress = func(done, total int, day market.Day) {
		pct := done * 100 / total
		if pct != lastPct {
			lastPct = pct
			fmt.Fprintf(os.Stderr, "\rbacktesting %3d%%  %s", pct, day)
		}
	}

	// The engine rather than app.Backtest, because the bars the run consumed
	// live on the engine and are the whole point of the exercise.
	eng := a.NewEngine(spec, opts)
	res, err := eng.Run(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\r%40s\r", "")

	var members *market.Membership
	if res.Spec.Index != "" {
		members, err = a.Store.Membership(res.Spec.Index)
		if err != nil {
			return fmt.Errorf("read the %s membership table to bundle it: %w", res.Spec.Index, err)
		}
	}

	man, err := bundle.Write(path, bundle.Input{
		Spec:         res.Spec,
		Result:       res,
		Series:       eng.LoadedSeries(),
		Fundamentals: a.Store.Fundamentals(),
		Membership:   members,
		Version:      version,
	})
	if err != nil {
		return err
	}

	// Exporting a bundle runs a real backtest, and a run is one trial against
	// the dataset whatever it was for.
	note := recordInvocation(a.Cfg, ledger.Entry{
		DatasetKey: ledger.DatasetKey(datasetOf(res.Spec)),
		Strategy:   plan.Name,
		CodeSHA256: res.Manifest.CodeSHA256,
		Trials:     1,
		Objective:  "sharpe",
		BestScore:  res.Metrics.Sharpe,
		// One trial has no spread, and a zero would claim the run found every
		// configuration equally good.
		ScoreSpread: engine.Ratio(math.NaN()),
	})

	size := fileSize(path)
	if *asJSON {
		printLedgerNote(note, true)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"path": path, "bytes": size, "manifest": man})
	}

	fmt.Printf("Wrote %s (%s)\n\n", path, bytesLabel(size))
	fmt.Printf("  %-16s %s\n", "strategy", firstNonEmpty(man.Strategy, plan.Name))
	fmt.Printf("  %-16s %s to %s, %s bars\n", "window", man.Start, man.End, man.Interval)
	fmt.Printf("  %-16s %s, %d bars\n", "carries", plural(len(man.Series), "symbol"), totalBars(man))
	fmt.Printf("  %-16s %s\n", "recorded from", firstNonEmpty(man.DataProvider, "an unnamed provider"))
	fmt.Printf("  %-16s %s\n", "content", man.ContentSHA256)
	if !man.Reproducible {
		fmt.Printf("\n  The run made %d model or web calls, so it was never reproducible and\n"+
			"  this bundle cannot make it so. The re-run will say the same.\n", man.AICallCount)
	}
	fmt.Printf("\nRe-run it anywhere, with no network and no keys:\n  pyrite bundle run %s\n", path)
	printLedgerNote(note, false)
	return nil
}

func cmdBundleRun(args []string) error {
	fs := flag.NewFlagSet("bundle run", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the comparison as JSON")
	path, flagArgs := splitPromptAndFlags(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	path = strings.TrimSpace(strings.Join(append([]string{path}, fs.Args()...), " "))
	if path == "" {
		return fmt.Errorf("name the bundle to run, for example:\n  pyrite bundle run run.pyrite")
	}

	b, err := bundle.Open(path)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmp, err := b.Rerun(ctx)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(cmp); err != nil {
			return err
		}
		if !cmp.Match {
			return exitCode(1)
		}
		return nil
	}

	fmt.Printf("%s — %s\n", path, firstNonEmpty(b.Manifest.Strategy, "unnamed strategy"))
	fmt.Printf("  %-16s pyrite %s, %s\n", "written by", b.Manifest.PyriteVersion,
		b.Manifest.CreatedAt.Format("2006-01-02 15:04 UTC"))
	fmt.Printf("  %-16s %s, %d bars, from %s\n", "data", plural(len(b.Manifest.Series), "symbol"),
		totalBars(&b.Manifest), firstNonEmpty(b.Manifest.DataProvider, "an unnamed provider"))
	fmt.Printf("  %-16s %s\n", "re-ran", fmt.Sprintf("%d sessions in %s",
		cmp.ReplaySessions, time.Duration(cmp.Elapsed)*time.Millisecond))

	fmt.Printf("\n%s\n", cmp.Summary())
	printComparisonTable(b, cmp)
	for _, n := range cmp.Notes {
		fmt.Printf("\n  Note: %s\n", wrapIndent(n, 74, "        "))
	}
	if !cmp.Match {
		// A bundle that does not reproduce is a failed check, and a pipeline
		// running this should be able to stop on it.
		return exitCode(1)
	}
	return nil
}

func cmdBundleShow(args []string) error {
	fs := flag.NewFlagSet("bundle show", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the manifest as JSON")
	path, flagArgs := splitPromptAndFlags(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	path = strings.TrimSpace(strings.Join(append([]string{path}, fs.Args()...), " "))
	if path == "" {
		return fmt.Errorf("name the bundle to show, for example:\n  pyrite bundle show run.pyrite")
	}

	b, err := bundle.Open(path)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"path": b.Path, "bytes": b.Bytes, "manifest": b.Manifest,
			"spec": b.Spec, "metrics": b.Recorded.Metrics,
			"modified": b.Modified, "computed_sha256": b.ComputedSHA256,
		})
	}

	m := b.Manifest
	fmt.Printf("%s (%s)\n\n", b.Path, bytesLabel(b.Bytes))
	fmt.Printf("  %-16s %s\n", "strategy", firstNonEmpty(m.Strategy, "unnamed"))
	fmt.Printf("  %-16s pyrite %s on %s, %s\n", "written by", m.PyriteVersion,
		m.CreatedAt.Format("2006-01-02"), m.GoVersion)
	fmt.Printf("  %-16s %s to %s, %s bars\n", "window", m.Start, m.End, m.Interval)
	fmt.Printf("  %-16s %s\n", "provider", firstNonEmpty(m.DataProvider, "unrecorded"))
	if m.Index != "" {
		fmt.Printf("  %-16s %s, point in time\n", "universe", m.Index)
	} else {
		fmt.Printf("  %-16s %s\n", "universe", plural(len(b.Spec.Universe), "symbol"))
	}
	fmt.Printf("  %-16s %s from %s, %g bps slippage, seed %d\n", "execution",
		b.Spec.Fill, money(b.Spec.InitialCash), b.Spec.Costs.SlippageBps, b.Spec.Seed)
	fmt.Printf("  %-16s %d bytes of JavaScript\n", "strategy code", len(b.Code))
	if b.Modified {
		fmt.Printf("  %-16s CHANGED — records %s, contents hash to %s\n", "content",
			m.ContentSHA256, b.ComputedSHA256)
	} else {
		fmt.Printf("  %-16s %s\n", "content", m.ContentSHA256)
	}
	if len(m.Reference) > 0 {
		fmt.Printf("  %-16s %s\n", "reference data", strings.Join(m.Reference, ", "))
	}
	if !m.Reproducible {
		fmt.Printf("  %-16s no — the run made %d model or web calls\n", "reproducible", m.AICallCount)
	}

	// An index universe is five hundred symbols, and a listing nobody can
	// read is not a listing. The manifest is there in --json for the rest.
	const maxRows = 30
	fmt.Printf("\n  %-10s %8s  %-12s %-12s %s\n", "Symbol", "Bars", "First", "Last", "Name")
	for i, s := range m.Series {
		if i >= maxRows {
			fmt.Printf("  ... and %s more; --json lists them all\n",
				plural(len(m.Series)-maxRows, "symbol"))
			break
		}
		fmt.Printf("  %-10s %8d  %-12s %-12s %s\n", s.Symbol, s.Bars, s.First, s.Last, truncate(s.Name, 34))
	}

	r := b.Recorded.Metrics
	fmt.Printf("\n  Recorded result over %d sessions\n", len(b.Recorded.Curve))
	fmt.Printf("    %-22s %14s\n", "Final value", money(r.EndValue))
	fmt.Printf("    %-22s %14s\n", "Total return", pct(r.TotalReturn))
	fmt.Printf("    %-22s %14s\n", "Sharpe ratio", ratio(r.Sharpe))
	fmt.Printf("    %-22s %14s\n", "Max drawdown", pct(r.MaxDrawdown))
	fmt.Printf("    %-22s %14d\n", "Trades", r.TotalTrades)
	fmt.Printf("\nRe-run it: pyrite bundle run %s\n", b.Path)
	return nil
}

// printComparisonTable shows the recorded numbers beside the ones the re-run
// produced, so a match is visible rather than asserted.
func printComparisonTable(b *bundle.Bundle, cmp *bundle.Comparison) {
	if cmp.Replayed == nil {
		return
	}
	rec, got := b.Recorded.Metrics, cmp.Replayed.Metrics
	fmt.Printf("\n  %-22s %16s %16s\n", "", "Recorded", "Re-run")
	fmt.Printf("  %-22s %16s %16s\n", "Final value", money(rec.EndValue), money(got.EndValue))
	fmt.Printf("  %-22s %16s %16s\n", "Total return", pct(rec.TotalReturn), pct(got.TotalReturn))
	fmt.Printf("  %-22s %16s %16s\n", "Sharpe ratio", ratio(rec.Sharpe), ratio(got.Sharpe))
	fmt.Printf("  %-22s %16s %16s\n", "Max drawdown", pct(rec.MaxDrawdown), pct(got.MaxDrawdown))
	fmt.Printf("  %-22s %16d %16d\n", "Trades", rec.TotalTrades, got.TotalTrades)
	fmt.Printf("  %-22s %16s %16s\n", "Costs paid", money(rec.TotalCosts), money(got.TotalCosts))
}

// plural renders a count with its noun, because "1 symbols" reads as a bug.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func totalBars(m *bundle.Manifest) int {
	n := 0
	for _, s := range m.Series {
		n += s.Bars
	}
	return n
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// bytesLabel renders a file size the way somebody about to send the file
// would want to read it.
func bytesLabel(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
