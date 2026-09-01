package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/strategy"
)

// cmdScenarios replays a strategy through named historical crises.
func cmdScenarios(args []string) error {
	fs := flag.NewFlagSet("scenarios", flag.ContinueOnError)
	cash := fs.Float64("cash", 100000, "starting capital")
	from := fs.String("from", "", "ignore windows that open before this date")
	to := fs.String("to", "", "ignore windows that close after this date")
	universe := fs.String("universe", "", "override the tradable symbols")
	benchmark := fs.String("benchmark", "SPY", "comma separated comparison symbols")
	offline := fs.Bool("offline", false, "use synthetic data and disable network access")
	asJSON := fs.Bool("json", false, "print the full result as JSON")
	codeFile, warmup, example := addCodeFileFlags(fs)
	interval := addIntervalFlag(fs)
	impact := fs.Float64("impact", 0,
		"market impact coefficient; 1 is the usual estimate, 0 disables the model")
	list := fs.Bool("list", false, "print the scenario table and exit, without running anything")

	prompt, flagArgs := splitPromptAndFlags(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	prompt = strings.TrimSpace(strings.Join(append([]string{prompt}, fs.Args()...), " "))

	if *list {
		return printScenarioTable(*asJSON)
	}
	if prompt == "" && *codeFile == "" && *example == "" {
		return fmt.Errorf("describe a strategy, for example:\n" +
			"  pyrite scenarios \"buy SPY and hold it\"\n" +
			"  or --code-file strategy.js, or --example golden-cross\n" +
			"  pyrite scenarios --list  prints the windows without running anything")
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
	s.spec.Costs.ImpactCoefficient = *impact

	rep, err := engine.RunScenarios(s.ctx, engine.ScenarioSpec{Base: s.spec}, s.app.Store,
		func(done, total int, name string) {
			fmt.Fprintf(os.Stderr, "\r%40s\rreplaying %d/%d  %s", "", done, total, name)
		})
	fmt.Fprintf(os.Stderr, "\r%60s\r", "")
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printScenarios(s.plan, rep)
	return nil
}

// printScenarioTable prints the windows themselves, which is worth having
// without a strategy: the dates are the tool's claim about history and should
// be checkable without running a backtest to see them.
func printScenarioTable(asJSON bool) error {
	list := engine.Scenarios()
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(list)
	}
	fmt.Printf("%d named windows. The index column is the S&P 500's own peak-to-trough\n", len(list))
	fmt.Printf("decline inside each window, from published closes.\n\n")
	for _, sc := range list {
		fmt.Printf("  %-20s %s..%s %8s\n", sc.Name, sc.Start, sc.End, pct(sc.IndexDrawdown))
		fmt.Printf("    %s\n\n", wrapIndent(sc.Description, 70, "    "))
	}
	return nil
}

// printScenarios renders the replay.
func printScenarios(plan *strategy.Plan, rep *engine.ScenarioReport) {
	fmt.Printf("%s\n", plan.Name)
	fmt.Printf("%d named windows, %d measured. Data runs %s to %s. Each window is\n",
		len(rep.Runs), rep.Covered, rep.DataFrom, rep.DataTo)
	fmt.Printf("preceded by %d bars of warm-up and %d sessions of trading, so the strategy\n",
		rep.Warmup, rep.LeadIn)
	fmt.Printf("enters it holding whatever it would have been holding. Only the window\n")
	fmt.Printf("itself is measured.\n\n")

	// The benchmark column is headed by the symbol rather than the fund's
	// full name, which does not fit and is not what anybody calls it.
	bench := "bench"
	for _, r := range rep.Runs {
		if r.Measured() && r.Benchmark != "" {
			bench = r.Benchmark
			break
		}
	}
	fmt.Printf("  %-20s %-22s %8s %8s %8s %8s\n",
		"", "window", "return", "drawdown", truncate(bench, 8), "excess")

	// Skipped windows keep their row. A missing row would read as a table of
	// every crisis, when it is a table of the ones this data can reach.
	reasons := map[string][]string{}
	var order []string
	for _, r := range rep.Runs {
		window := fmt.Sprintf("%s..%s", r.Start, r.End)
		if !r.Measured() {
			reason := r.SkipReason
			if reason == "" {
				reason = "the run failed: " + r.Error
			}
			if _, seen := reasons[reason]; !seen {
				order = append(order, reason)
			}
			reasons[reason] = append(reasons[reason], r.Name)
			fmt.Printf("  %-20s %-22s %8s\n", r.Name, window, "—")
			continue
		}
		fmt.Printf("  %-20s %-22s %8s %8s %8s %8s\n", r.Name, window,
			pctOrNA(r.Return), pctOrNA(r.MaxDrawdown),
			pctOrNA(r.BenchmarkReturn), pctOrNA(r.Excess))
	}

	if len(order) > 0 {
		fmt.Printf("\nNot measured (%d of %d)\n", rep.Skipped, len(rep.Runs))
		for _, reason := range order {
			fmt.Printf("  %s\n", wrapIndent(strings.Join(reasons[reason], ", "), 74, "  "))
			fmt.Printf("      %s\n", wrapIndent(reason, 70, "      "))
		}
	}

	// Exposure is printed only where it changes the reading of a row, which
	// is when the row is flat because nothing was held.
	var idle []string
	for _, r := range rep.Runs {
		if r.Measured() && r.Fills == 0 && float64(r.Exposure) < 0.01 {
			idle = append(idle, r.Name)
		}
	}
	if len(idle) > 0 {
		fmt.Printf("\nHeld nothing at all in: %s\n", wrapIndent(strings.Join(idle, ", "), 74, "  "))
	}

	for _, f := range rep.Findings {
		marker := "note "
		switch f.Severity {
		case engine.SeverityCritical:
			marker = "STOP "
		case engine.SeverityWarning:
			marker = "warn "
		}
		fmt.Printf("\n  %s%s\n", marker, f.Title)
		fmt.Printf("        %s\n", wrapIndent(f.Detail, 68, "        "))
	}

	if rep.Verdict != "" {
		fmt.Printf("\n  %s\n", wrapIndent(rep.Verdict, 74, "  "))
	}
}
