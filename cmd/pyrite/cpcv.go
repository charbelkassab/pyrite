package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/strategy"
)

// cmdCPCV holds out every combination of groups and reports the distribution
// of out-of-sample paths that reconstructs.
func cmdCPCV(args []string) error {
	fs := flag.NewFlagSet("cpcv", flag.ContinueOnError)
	params, objective, csvPath, workers, maxCombos, cash, from, to, universe, benchmark, offline, asJSON :=
		addCommonSearchFlags(fs)
	codeFile, warmup, example := addCodeFileFlags(fs)
	interval := addIntervalFlag(fs)
	groups := fs.Int("groups", 6, "contiguous blocks the period is cut into")
	testGroups := fs.Int("test-groups", 2, "blocks held out per split")
	// Zero rather than -1 is the default so that the documented behaviour is
	// the one that actually happens: the engine reads zero as "use the
	// strategy's warm-up", which is the horizon the leakage can occur over.
	embargo := fs.Int("embargo", 0,
		"sessions withheld from training either side of each test group (0: the strategy's warm-up, negative: none)")
	train := fs.Int("train", 0, "training window of the walk-forward path compared against (default 504)")
	test := fs.Int("test", 0, "test window of the walk-forward path compared against (default 126)")
	skipWF := fs.Bool("no-walkforward", false, "skip the walk-forward path the distribution is compared against")

	prompt, flagArgs := splitPromptAndFlags(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	prompt = strings.TrimSpace(strings.Join(append([]string{prompt}, fs.Args()...), " "))
	if prompt == "" && *codeFile == "" && *example == "" {
		return fmt.Errorf("describe a strategy, for example:\n" +
			"  pyrite cpcv \"a golden cross on SPY\"\n" +
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

	lastPct := -1
	res, err := engine.RunCPCV(s.ctx, engine.CPCVSpec{
		Base: s.spec, Grids: params.grids, Groups: *groups, TestGroups: *testGroups,
		Embargo: *embargo, Objective: *objective, Workers: *workers, MaxCombos: *maxCombos,
		TrainDays: *train, TestDays: *test, SkipWalkForward: *skipWF,
	}, s.app.Store, func(done, total int) {
		if pct := done * 100 / total; pct != lastPct {
			lastPct = pct
			fmt.Fprintf(os.Stderr, "\rcross-validating %3d%%  %d/%d", pct, done, total)
		}
	})
	fmt.Fprintf(os.Stderr, "\r%50s\r", "")
	if err != nil {
		return err
	}

	if *csvPath != "" {
		if err := writeCPCVCSV(*csvPath, res); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *csvPath)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	printCPCV(s.plan, res)
	return nil
}

// printCPCV renders the splits, the paths and the spread across them.
func printCPCV(plan *strategy.Plan, res *engine.CPCVResult) {
	fmt.Printf("%s\n", plan.Name)
	per := 0
	if res.Groups > 0 {
		per = res.Sessions / res.Groups
	}
	fmt.Printf("%d groups of about %d sessions, %d held out per split: %d splits, %d paths\n",
		res.Groups, per, res.TestGroups, len(res.Splits), res.ValidPaths)
	fmt.Printf("%d combinations in %s, ranked by %s in training, %d sessions embargoed either "+
		"side of every test group\n\n", res.Combos,
		(time.Duration(res.Elapsed) * time.Millisecond).Round(time.Millisecond),
		res.Objective, res.Embargo)

	fmt.Printf("  %-6s %-9s %8s %8s %-24s %10s %10s\n",
		"split", "held out", "train", "purged", "chosen", "in-sample", "out")
	for _, sp := range res.Splits {
		if sp.Error != "" {
			fmt.Printf("  %-6d %-9s %s\n", sp.Index, groupList(sp.TestGroups), truncate(sp.Error, 55))
			continue
		}
		fmt.Printf("  %-6d %-9s %8d %8d %-24s %10s %10s\n",
			sp.Index, groupList(sp.TestGroups), sp.TrainSessions, sp.PurgedSessions,
			truncate(engine.FormatParams(sp.BestParams), 24),
			pctRatio(sp.TrainReturn), pctRatio(sp.TestReturn))
	}

	if res.Failed > 0 {
		fmt.Printf("\n  %d of %d group backtests failed\n", res.Failed, res.Combos*res.Groups)
	}

	fmt.Printf("\nEvery path below covers the whole period and none of it was fitted to\n")
	fmt.Printf("  %-5s %-11s %-11s %10s %10s %8s %10s\n",
		"path", "from", "to", "return", "CAGR", "Sharpe", "drawdown")
	for _, p := range res.Paths {
		if p.Error != "" {
			fmt.Printf("  %-5d %s\n", p.Index, truncate(p.Error, 60))
			continue
		}
		fmt.Printf("  %-5d %-11s %-11s %10s %10s %8s %10s\n",
			p.Index, p.Start, p.End, pct(p.Metrics.TotalReturn), pct(p.Metrics.CAGR),
			ratio(p.Metrics.Sharpe), pct(p.Metrics.MaxDrawdown))
	}

	// The distribution is the reason the command exists, so it gets its own
	// block rather than a footnote under the table it is computed from.
	fmt.Printf("\nWhat the spread says, across %d out-of-sample paths\n", res.ValidPaths)
	fmt.Printf("  %-24s %12s %12s %10s\n", "", "return", "CAGR", "Sharpe")
	spreadRow := func(label string, get func(engine.PathSpread) engine.Ratio) {
		fmt.Printf("  %-24s %12s %12s %10s\n", label,
			pctRatio(get(res.Return)), pctRatio(get(res.CAGR)), ratio(get(res.Sharpe)))
	}
	spreadRow("Mean", func(s engine.PathSpread) engine.Ratio { return s.Mean })
	spreadRow("Median", func(s engine.PathSpread) engine.Ratio { return s.Median })
	spreadRow("Standard deviation", func(s engine.PathSpread) engine.Ratio { return s.Stdev })
	spreadRow("5th percentile", func(s engine.PathSpread) engine.Ratio { return s.P05 })
	spreadRow("95th percentile", func(s engine.PathSpread) engine.Ratio { return s.P95 })
	spreadRow("Worst path", func(s engine.PathSpread) engine.Ratio { return s.Worst })
	spreadRow("Best path", func(s engine.PathSpread) engine.Ratio { return s.Best })
	fmt.Printf("  %-24s %12s\n", "Paths profitable",
		fmt.Sprintf("%d of %d", res.ProfitablePaths, res.ValidPaths))
	if res.MaxDrawdown.Worst.Defined() {
		fmt.Printf("  %-24s %12s\n", "Deepest drawdown", pctRatio(res.MaxDrawdown.Worst))
	}

	// The control the distribution above has to beat. Printed beside it
	// rather than in a later section, because a reader who sees the paths
	// alone has no way to tell a working search from a rising market.
	if res.NoSelection.Median.Defined() {
		fmt.Printf("\n  Choosing nothing: the same groups, one configuration held throughout\n")
		fmt.Printf("  %-24s %12s\n", "Median configuration", pctRatio(res.NoSelection.Median))
		fmt.Printf("  %-24s %12s\n", "Worst configuration", pctRatio(res.NoSelection.Worst))
		fmt.Printf("  %-24s %12s\n", "Best configuration", pctRatio(res.NoSelection.Best))
	}

	fmt.Printf("\nHow much of this is the selection?\n")
	if res.PBO.Defined() {
		fmt.Printf("  %-34s %10s   (%d splits)\n", "Overfitting prob., purged splits",
			pctRatio(res.PBO), res.PBOSplits)
	}
	if res.BlockPBO.Defined() {
		fmt.Printf("  %-34s %10s   (%d splits)\n", "  the sweep's unpurged partition",
			pctRatio(res.BlockPBO), res.BlockPBOSplits)
	}
	if res.AvgBarsHeld > 0 {
		fmt.Printf("  %-34s %10.0f   (%d sessions per group)\n", "Average round trip, bars",
			res.AvgBarsHeld, res.GroupSessions)
	}

	if wf := res.WalkForward; wf != nil {
		fmt.Printf("\nThe single walk-forward path, for comparison\n")
		fmt.Printf("  out of sample %s to %s over %d folds, %d of them positive\n",
			wf.Start, wf.End, wf.Folds, wf.PositiveFolds)
		fmt.Printf("  %-24s %12s   %s\n", "Annualised", pctRatio(wf.CAGR),
			placement(wf.CAGRPercentile))
		fmt.Printf("  %-24s %12s   %s\n", "Sharpe", ratio(wf.Sharpe),
			placement(wf.SharpePercentile))
	}

	if res.Verdict != "" {
		fmt.Printf("\n  %s\n", wrapIndent(res.Verdict, 74, "  "))
	}
}

// placement says where one number sits inside the distribution.
func placement(r engine.Ratio) string {
	if !r.Defined() {
		return ""
	}
	return fmt.Sprintf("%.0fth percentile of the paths", float64(r)*100)
}

// groupList renders a split's held-out groups compactly.
func groupList(gs []int) string {
	parts := make([]string, 0, len(gs))
	for _, g := range gs {
		parts = append(parts, strconv.Itoa(g))
	}
	return strings.Join(parts, ",")
}

// writeCPCVCSV exports the splits, which carry the chosen parameters the
// terminal table has to truncate.
func writeCPCVCSV(path string, res *engine.CPCVResult) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		// A close failure on a file being written means the export was
		// truncated, so it has to reach the caller rather than be dropped —
		// otherwise the CSV looks written and is short.
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	w := csv.NewWriter(f)
	if err := w.Write([]string{"split", "test_groups", "train_sessions", "test_sessions",
		"purged_sessions", "params", "train_score", "test_score", "train_return",
		"test_return", "error"}); err != nil {
		return err
	}
	for _, sp := range res.Splits {
		if err := w.Write([]string{
			strconv.Itoa(sp.Index), groupList(sp.TestGroups),
			strconv.Itoa(sp.TrainSessions), strconv.Itoa(sp.TestSessions),
			strconv.Itoa(sp.PurgedSessions), engine.FormatParams(sp.BestParams),
			num(float64(sp.TrainScore)), num(float64(sp.TestScore)),
			num(float64(sp.TrainReturn)), num(float64(sp.TestReturn)), sp.Error,
		}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}
