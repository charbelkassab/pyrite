package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charbelkassab/pyrite/examples"
	"github.com/charbelkassab/pyrite/internal/config"
	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/ledger"
	"github.com/charbelkassab/pyrite/internal/market"
)

// openLedger prepares the research history, or returns nil when the user has
// turned it off.
func openLedger(cfg *config.Config) (*ledger.Ledger, error) {
	if ledger.Disabled() {
		return nil, nil
	}
	dir, err := cfg.CacheDir("ledger")
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	return ledger.Open(dir)
}

// datasetOf identifies the research problem a spec was run against.
func datasetOf(spec engine.Spec) ledger.Dataset {
	return ledger.Dataset{
		Symbols:  spec.Universe,
		Index:    spec.Index,
		Start:    spec.Start,
		End:      spec.End,
		Interval: spec.Interval,
	}
}

// recordInvocation adds one run or sweep to the history and returns the line
// to print when the dataset has absorbed materially more searching than this
// invocation alone accounted for.
//
// A ledger that cannot be written is reported and then ignored. The backtest
// has already succeeded by this point, and losing a result to a bookkeeping
// failure would be a poor trade.
func recordInvocation(cfg *config.Config, e ledger.Entry) string {
	l, err := openLedger(cfg)
	if l == nil || err != nil {
		if err != nil {
			fmt.Fprintf(os.Stderr, "ledger not updated: %v\n", err)
		}
		return ""
	}
	if err := l.Record(e); err != nil {
		fmt.Fprintf(os.Stderr, "ledger not updated: %v\n", err)
		return ""
	}
	s, err := l.Query(e.DatasetKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ledger not readable: %v\n", err)
		return ""
	}
	return s.Warning(e.Trials)
}

// printLedgerNote writes the multiple-testing reminder. It goes to stderr
// alongside machine output, so a JSON consumer gets valid JSON and the person
// watching still gets the warning.
func printLedgerNote(note string, machineOutput bool) {
	if note == "" {
		return
	}
	w := os.Stdout
	if machineOutput {
		w = os.Stderr
	}
	fmt.Fprintf(w, "\n  %s\n", wrapIndent(note, 74, "  "))
}

// cmdLedger reports what a dataset has already been put through.
func cmdLedger(args []string) error {
	fs := flag.NewFlagSet("ledger", flag.ContinueOnError)
	dataset := fs.String("dataset", "", "show one dataset by the key `pyrite ledger` lists")
	universe := fs.String("universe", "", "identify the dataset by its symbols instead of its key")
	example := fs.String("example", "", "identify the dataset by a bundled example's universe")
	from := fs.String("from", "", "start date YYYY-MM-DD")
	to := fs.String("to", "", "end date YYYY-MM-DD")
	interval := fs.String("interval", "1d", "bar size: "+strings.Join(market.IntervalNames(), ", "))
	reset := fs.Bool("reset", false, "forget the selected dataset")
	all := fs.Bool("all", false, "with --reset, forget every dataset")
	yes := fs.Bool("yes", false, "answer the confirmation prompt in advance")
	asJSON := fs.Bool("json", false, "print as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// `pyrite ledger SPY:2019-01-02:2023-12-29:1d` should do the obvious thing.
	if *dataset == "" && fs.NArg() > 0 {
		*dataset = fs.Arg(0)
	}

	cfg, err := config.Load(os.Getenv("PYRITE_CONFIG"))
	if err != nil {
		return err
	}
	if ledger.Disabled() {
		return fmt.Errorf("the ledger is turned off by PYRITE_NO_LEDGER, so there is nothing to show.\n" +
			"  Unset it and future runs will be counted again")
	}
	l, err := openLedger(cfg)
	if err != nil {
		return err
	}

	key := *dataset
	if key == "" && (*universe != "" || *example != "" || *from != "" || *to != "") {
		key, err = datasetKeyFromFlags(*example, *universe, *from, *to, *interval)
		if err != nil {
			return err
		}
	}

	if *reset {
		return resetLedger(l, key, *all, *yes)
	}
	if key == "" {
		return listDatasets(l, *asJSON)
	}
	return showDataset(l, key, *asJSON)
}

// datasetKeyFromFlags rebuilds the key a run with these flags would have
// recorded.
//
// An unset --from is the one case this cannot reproduce: a run resolves it
// against the data actually available, which needs the data. The list output
// prints every key in full, so --dataset is always available as the exact
// answer.
func datasetKeyFromFlags(example, universe, from, to, interval string) (string, error) {
	names := universe
	if example != "" {
		ex, err := examples.Get(example)
		if err != nil {
			return "", err
		}
		if names == "" {
			names = strings.Join(ex.Universe, ",")
		}
	}
	d := ledger.Dataset{
		Symbols: market.ResolveUniverse(names),
		Index:   market.IndexUniverse(names),
		End:     market.NewDay(time.Now()),
	}
	if from != "" {
		start, err := market.ParseDay(from)
		if err != nil {
			return "", err
		}
		d.Start = start
	}
	if to != "" {
		end, err := market.ParseDay(to)
		if err != nil {
			return "", err
		}
		d.End = end
	}
	iv, err := market.ParseInterval(interval)
	if err != nil {
		return "", err
	}
	d.Interval = iv
	return ledger.DatasetKey(d), nil
}

func listDatasets(l *ledger.Ledger, asJSON bool) error {
	all, err := l.Datasets()
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(all)
	}
	if len(all) == 0 {
		fmt.Printf("Nothing recorded yet. Every run and sweep adds to this, so that the\n")
		fmt.Printf("next one can tell you how much searching a dataset has already had.\n")
		return nil
	}

	fmt.Printf("What you have already tried, and how often\n\n")
	fmt.Printf("  %-40s %8s %9s %8s %8s  %-10s %s\n",
		"dataset", "trials", "sessions", "best", "luck", "first", "last")
	for _, s := range all {
		fmt.Printf("  %-40s %8d %9d %8s %8s  %-10s %s\n",
			truncate(s.DatasetKey, 40), s.Trials, s.Invocations,
			ratio(s.BestScore), ratio(s.LuckThreshold),
			shortDate(s.First), shortDate(s.Last))
	}
	fmt.Printf("\n  \"luck\" is the score the best of that many tries reaches by chance alone.\n")
	fmt.Printf("  For one dataset in full:  pyrite ledger --dataset <key>\n")
	return nil
}

func showDataset(l *ledger.Ledger, key string, asJSON bool) error {
	s, err := l.Query(key)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	if s.Empty() {
		fmt.Printf("Nothing recorded against %s.\n\n", key)
		fmt.Printf("  `pyrite ledger` lists the datasets that do have a history.\n")
		return nil
	}

	fmt.Printf("%s\n\n", s.DatasetKey)
	fmt.Printf("  %-30s %14d\n", "Sessions", s.Invocations)
	fmt.Printf("  %-30s %14d\n", "Cumulative trials", s.Trials)
	fmt.Printf("  %-30s %14s\n", "First searched", shortDate(s.First))
	fmt.Printf("  %-30s %14s\n", "Most recent", shortDate(s.Last))
	fmt.Printf("  %-30s %14d\n", "Distinct strategy versions", len(s.CodeHashes))
	fmt.Printf("  %-30s %14s\n",
		"Best "+ledger.ObjectiveLabel(s.Objective)+" seen", ratio(s.BestScore))
	fmt.Printf("  %-30s %14s\n", "Spread of scores", ratio(s.ScoreSpread))
	fmt.Printf("  %-30s %14s\n", "Best from luck alone", ratio(s.LuckThreshold))

	if len(s.Strategies) > 0 {
		fmt.Printf("\n  Strategies: %s\n", wrapIndent(strings.Join(s.Strategies, ", "), 62, "    "))
	}
	if len(s.Objectives) > 1 {
		fmt.Printf("  Ranked by: %s\n", strings.Join(s.Objectives, ", "))
	}
	if s.Verdict != "" {
		fmt.Printf("\n  %s\n", wrapIndent(s.Verdict, 74, "  "))
	}
	fmt.Printf("\n  To start this dataset's count again:\n")
	fmt.Printf("    pyrite ledger --dataset %s --reset\n", s.DatasetKey)
	return nil
}

func resetLedger(l *ledger.Ledger, key string, all, yes bool) error {
	if !all && key == "" {
		return fmt.Errorf("say what to forget: --dataset <key>, the run flags that identify it,\n" +
			"  or --all to clear the whole ledger")
	}
	what := fmt.Sprintf("the history of %s", key)
	if all {
		what = "every dataset in the ledger"
	}
	// Forgetting is the one operation here that destroys evidence, and the
	// evidence is the point of the feature.
	if !yes && !confirm("Forget "+what+"?") {
		fmt.Println("nothing changed")
		return nil
	}
	if all {
		if err := l.ResetAll(); err != nil {
			return err
		}
		fmt.Println("the ledger is empty; the trial count starts again from the next run")
		return nil
	}
	if err := l.Reset(key); err != nil {
		return err
	}
	fmt.Printf("forgot %s\n", key)
	return nil
}

func confirm(question string) bool {
	fmt.Printf("%s [y/N] ", question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// shortDate renders a timestamp as a date, or as a dash when nothing was recorded.
func shortDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format(market.Layout)
}
