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
	"github.com/charbelkassab/pyrite/internal/config"
	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/forward"
	"github.com/charbelkassab/pyrite/internal/market"
	"github.com/charbelkassab/pyrite/internal/strategy"
)

// cmdForward dispatches the paper-forward subcommands.
func cmdForward(args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "record":
		return cmdForwardRecord(args)
	case "score":
		return cmdForwardScore(args)
	case "verify":
		return cmdForwardVerify(args)
	case "list":
		return cmdForwardList(args)
	default:
		return fmt.Errorf("unknown forward command %q. There is:\n"+
			"  pyrite forward record   write down what the strategy wants to hold next session\n"+
			"  pyrite forward score    measure the records old enough to have an outcome\n"+
			"  pyrite forward verify   check that nothing recorded has been altered since\n"+
			"  pyrite forward list     what has been recorded so far", sub)
	}
}

// openForwardLog prepares the decision log under the data directory.
func openForwardLog(cfg *config.Config) (*forward.Log, error) {
	dir, err := cfg.CacheDir("forward")
	if err != nil {
		return nil, fmt.Errorf("open the forward log: %w", err)
	}
	return forward.Open(dir)
}

// cmdForwardRecord runs the strategy up to today and writes down what it
// wants to hold when the market next opens.
func cmdForwardRecord(args []string) error {
	fs := flag.NewFlagSet("forward record", flag.ExitOnError)
	example := fs.String("example", "", "record a bundled example; `pyrite examples` lists them")
	codeFile := fs.String("code-file", "", "record this JavaScript strategy instead of compiling a prompt")
	name := fs.String("name", "", "the label the decisions accumulate under (default: the strategy's own name)")
	asOf := fs.String("as-of", "",
		"pretend the last session was this date, YYYY-MM-DD. It exists so this loop can be "+
			"tested and so an interrupted schedule can be backfilled, not so a decision can be "+
			"passed off as having been made earlier: anything recorded after its outcome already "+
			"existed is marked as a backfill and kept out of the forward statistics")
	horizon := fs.Int("horizon", 1, "how many sessions the recorded book is held for")
	reference := fs.String("reference", "",
		"the symbol whose sessions define the holding window (default: the first benchmark)")
	universe := fs.String("universe", "", "override the tradable symbols")
	from := fs.String("from", "", "how far back to simulate before the as-of date (default 5 years)")
	cash := fs.Float64("cash", 100000, "starting capital for the simulation behind the decision")
	note := fs.String("note", "", "free text stored with the record")
	offline := fs.Bool("offline", false, "use synthetic data and disable network access")
	asJSON := fs.Bool("json", false, "print the record as JSON")
	prompt, flagArgs := splitPromptAndFlags(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	prompt = strings.TrimSpace(strings.Join(append([]string{prompt}, fs.Args()...), " "))
	if prompt == "" && *codeFile == "" && *example == "" {
		return fmt.Errorf("say which strategy to record, for example:\n" +
			"  pyrite forward record --example golden-cross\n" +
			"  pyrite forward record --code-file ./mine.js --name mine\n\n" +
			"Whatever it is, record the same one every session: the point is a run of decisions\n" +
			"under one name, not one decision under many.")
	}
	if *horizon < 1 {
		return fmt.Errorf("--horizon is how many sessions the book is held for, so it must be at least 1")
	}

	a, err := newApp(fs, offline)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := app.RunOptions{InitialCash: *cash}
	if *universe != "" {
		opts.Index = market.IndexUniverse(*universe)
		opts.Universe = market.ResolveUniverse(*universe)
	}
	if *asOf != "" {
		d, err := market.ParseDay(*asOf)
		if err != nil {
			return err
		}
		opts.End = d
	}
	opts.ApplyDefaults()
	if *from != "" {
		d, err := market.ParseDay(*from)
		if err != nil {
			return err
		}
		opts.Start = d
	} else {
		// A forward record needs enough history for the indicators to be
		// warm and no more. Simulating thirty years every session to learn
		// what to hold tomorrow would make a daily schedule unusable.
		opts.Start = opts.End.Add(-365 * 5)
	}

	plan, err := forwardPlan(ctx, a, prompt, *example, *codeFile, &opts)
	if err != nil {
		return err
	}
	spec := app.BuildSpec(plan, prompt, opts)
	res, err := a.Backtest(ctx, spec, opts)
	if err != nil {
		return err
	}
	if len(res.Days) == 0 {
		return fmt.Errorf("the strategy ran no sessions up to %s, so it has said nothing to record.\n"+
			"  Usually the warm-up exceeds the history loaded: try --from with an earlier date", opts.End)
	}
	last := res.Days[len(res.Days)-1]

	prices, err := lastPrices(ctx, a, last, spec)
	if err != nil {
		return err
	}
	book := forward.Intend(last, prices, spec.MaxLeverage)

	ref := *reference
	if ref == "" {
		ref = referenceSymbol(spec, book)
	}
	if ref == "" {
		return fmt.Errorf("this strategy holds nothing and names no benchmark, so there is no\n" +
			"  calendar to say when its decision's outcome arrives. Name one: --reference SPY")
	}

	label := *name
	if label == "" {
		label = plan.Name
	}
	l, err := openForwardLog(a.Cfg)
	if err != nil {
		return err
	}
	entry, appended, err := l.Record(forward.Entry{
		Strategy:   label,
		CodeSHA256: res.Manifest.CodeSHA256,
		AsOf:       last.Date.Date(),
		Reference:  market.NormalizeSymbol(ref),
		Horizon:    *horizon,
		Positions:  book,
		Note:       *note,
	})
	if err != nil {
		return err
	}
	v, err := l.Verify()
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"entry": entry, "appended": appended, "chain": v,
		})
	}
	printForwardRecord(l, entry, appended, v)
	return nil
}

// forwardPlan resolves the strategy to record, the same three ways `pyrite
// run` does.
func forwardPlan(ctx context.Context, a *app.App, prompt, example, codeFile string, opts *app.RunOptions) (*strategy.Plan, error) {
	switch {
	case example != "":
		ex, err := examples.Get(example)
		if err != nil {
			return nil, err
		}
		plan := &strategy.Plan{
			Name: firstNonEmpty(ex.Label, ex.Name), Description: firstNonEmpty(ex.Title, ex.Summary),
			Code: ex.Code, Universe: ex.Universe, Benchmarks: ex.Benchmarks,
			Warmup: ex.Warmup, AllowShort: ex.AllowShort,
		}
		if len(opts.Universe) == 0 && len(ex.Universe) > 0 {
			opts.Universe = market.ResolveUniverse(strings.Join(ex.Universe, ","))
			opts.Index = market.IndexUniverse(strings.Join(ex.Universe, ","))
		}
		if len(opts.Benchmarks) == 0 && len(ex.Benchmarks) > 0 {
			opts.Benchmarks = market.ResolveUniverse(strings.Join(ex.Benchmarks, ","))
		}
		if ex.NeedsModel && !a.Cfg.AnyProviderEnabled() {
			return nil, fmt.Errorf("the %q example calls a model inside the backtest, so it needs one configured", ex.Name)
		}
		return plan, nil
	case codeFile != "":
		code, err := os.ReadFile(codeFile)
		if err != nil {
			return nil, err
		}
		return &strategy.Plan{
			Name: strings.TrimSuffix(filepath.Base(codeFile), filepath.Ext(codeFile)),
			Code: string(code),
		}, nil
	}
	// Compiling the same prompt twice can produce different code, and a
	// forward record is only worth anything if every entry under one name
	// came from the same strategy. The code hash on each record is what
	// catches it, and the warning is what stops it happening by accident.
	if !a.Cfg.AnyProviderEnabled() {
		return nil, fmt.Errorf("compiling plain English needs a model, and none is configured.\n" +
			"  For a forward record, prefer --code-file or --example anyway: the same prompt\n" +
			"  compiled again tomorrow may not be the same strategy")
	}
	fmt.Fprintf(os.Stderr, "compiling strategy with %s...\n", a.DescribeRoutes())
	fmt.Fprintf(os.Stderr, "note: recording a compiled prompt records whatever it compiled to today.\n"+
		"  Save the code and use --code-file if this is to run every session.\n")
	return a.Compiler.Compile(ctx, strategy.Request{
		Prompt: prompt, Universe: opts.Universe, Start: opts.Start, End: opts.End,
	})
}

// lastPrices gives Intend a price for every symbol the final session's orders
// name, so an order expressed in shares can be turned into a weight.
func lastPrices(ctx context.Context, a *app.App, last engine.DayRecord, spec engine.Spec) (map[string]float64, error) {
	prices := map[string]float64{}
	for _, p := range last.Positions {
		if p.Price > 0 {
			prices[p.Symbol] = p.Price
		}
	}
	var missing []string
	for _, o := range last.Orders {
		if o.Kind == engine.KindShares && prices[o.Symbol] == 0 {
			missing = append(missing, o.Symbol)
		}
	}
	if len(missing) == 0 {
		return prices, nil
	}
	series, _ := a.Store.GetMany(ctx, market.DedupeSymbols(missing), last.Date.Add(-14), last.Date)
	for sym, s := range series {
		if b, ok := s.AsOf(last.Date); ok && b.AdjClose > 0 {
			prices[sym] = b.AdjClose
		}
	}
	return prices, nil
}

// referenceSymbol picks the calendar the holding window is measured on.
//
// The benchmark comes first because it is the one symbol guaranteed to trade
// on every session the strategy could have traded on, including the sessions
// where the strategy chose to hold nothing.
func referenceSymbol(spec engine.Spec, book []forward.Position) string {
	if len(spec.Benchmarks) > 0 {
		return spec.Benchmarks[0]
	}
	if len(spec.Universe) > 0 {
		return spec.Universe[0]
	}
	if len(book) > 0 {
		return book[0].Symbol
	}
	return ""
}

func printForwardRecord(l *forward.Log, e forward.Entry, appended bool, v forward.Verification) {
	if appended {
		fmt.Printf("Recorded what %q wants to hold from the next session.\n\n", e.Strategy)
	} else {
		fmt.Printf("%q had already recorded this exact decision for %s; nothing was added.\n\n", e.Strategy, e.AsOf)
	}
	fmt.Printf("  %-28s %s\n", "As of", e.AsOf)
	fmt.Printf("  %-28s %s\n", "Written at", e.At.Local().Format("2006-01-02 15:04"))
	fmt.Printf("  %-28s %s\n", "Strategy code", shortSHA(e.CodeSHA256))
	fmt.Printf("  %-28s %d\n", "Sessions held", e.Horizon)
	fmt.Printf("  %-28s %s\n", "Reference calendar", e.Reference)
	fmt.Printf("  %-28s %.0f%%\n", "Gross exposure", e.Gross()*100)
	fmt.Printf("  %-28s %s\n", "Book", wrapIndent(e.Describe(), 44, strings.Repeat(" ", 31)))
	fmt.Printf("  %-28s %s\n", "Sealed as", shortSHA(e.Hash))
	fmt.Printf("\n  %-28s %s\n", "Chain", chainLine(v))
	fmt.Printf("  %-28s %s\n", "Log", l.Path())
	fmt.Printf("\n  Nothing here is evidence until the sessions it refers to have happened:\n")
	fmt.Printf("    pyrite forward score\n")
}

// cmdForwardScore measures every record old enough to have an outcome.
func cmdForwardScore(args []string) error {
	fs := flag.NewFlagSet("forward score", flag.ExitOnError)
	strategyName := fs.String("strategy", "", "score only the records under this name")
	offline := fs.Bool("offline", false, "use synthetic data and disable network access")
	asJSON := fs.Bool("json", false, "print the scorecard as JSON")
	verbose := fs.Bool("all", false, "list every record, including the ones with no outcome yet")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := newApp(fs, offline)
	if err != nil {
		return err
	}
	l, err := openForwardLog(a.Cfg)
	if err != nil {
		return err
	}
	r, err := l.Read()
	if err != nil {
		return err
	}
	// The chain is walked before anything is scored. A scorecard computed
	// over records that may have been edited is exactly the impressive,
	// meaningless number this whole feature exists to refuse.
	v := forward.VerifyEntries(r)
	if !v.Intact {
		return fmt.Errorf("the recorded decisions do not verify, so scoring them would prove nothing.\n"+
			"  %s\n"+
			"  Run `pyrite forward verify` for the full picture", v.Reason)
	}

	entries := r.Entries
	if *strategyName != "" {
		var kept []forward.Entry
		for _, e := range entries {
			if strings.EqualFold(e.Strategy, *strategyName) {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			return fmt.Errorf("nothing has been recorded under %q. `pyrite forward list` shows the names that have", *strategyName)
		}
		entries = kept
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	card, err := forward.Score(ctx, entries, storePrices{a.Store})
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(card)
	}
	printForwardScore(card, r.Skipped, *verbose)
	return nil
}

// storePrices adapts the market store to what the scorer needs.
type storePrices struct{ store *market.Store }

func (p storePrices) Bars(ctx context.Context, symbol string, from, to market.Day) ([]market.Bar, error) {
	s, err := p.store.Get(ctx, symbol, from, to)
	if err != nil {
		return nil, err
	}
	return s.Bars, nil
}

func printForwardScore(card forward.Scorecard, skipped []string, verbose bool) {
	fmt.Printf("What the recorded decisions actually did\n\n")

	shown := 0
	for _, e := range card.Entries {
		if e.Pending != "" || e.Problem != "" {
			continue
		}
		if shown == 0 {
			fmt.Printf("  %-10s %-22s %-10s %-10s %9s\n",
				"as of", "strategy", "entered", "exited", "return")
		}
		mark := ""
		if e.Backfilled {
			mark = "  backfilled"
		}
		fmt.Printf("  %-10s %-22s %-10s %-10s %9s%s\n",
			e.AsOf, truncate(e.Strategy, 22), e.Entered, e.Exited, pctRatio(e.Return), mark)
		shown++
	}
	if shown == 0 {
		fmt.Printf("  Nothing has an outcome yet.\n")
	}
	if verbose {
		for _, e := range card.Entries {
			switch {
			case e.Pending != "":
				fmt.Printf("  %-10s %-22s waiting: %s\n", e.AsOf, truncate(e.Strategy, 22), e.Pending)
			case e.Problem != "":
				fmt.Printf("  %-10s %-22s cannot be measured: %s\n", e.AsOf, truncate(e.Strategy, 22), e.Problem)
			}
		}
	}

	printAggregate("Written before the outcome existed", card.Forward)
	if card.Backfill.Count > 0 {
		printAggregate("Backfilled, and therefore not evidence", card.Backfill)
	}
	if card.Pending > 0 {
		fmt.Printf("\n  %d %s recorded and not yet scoreable.\n", card.Pending, plural(card.Pending, "decision"))
	}
	for _, s := range skipped {
		fmt.Printf("\n  %s\n", s)
	}
	if card.Verdict != "" {
		fmt.Printf("\n  %s\n", wrapIndent(card.Verdict, 74, "  "))
	}
}

func printAggregate(title string, a forward.Aggregate) {
	fmt.Printf("\n%s\n\n", title)
	if a.Count == 0 {
		fmt.Printf("  none\n")
		return
	}
	fmt.Printf("  %-28s %14d\n", "Decisions scored", a.Count)
	fmt.Printf("  %-28s %14s\n", "Hit rate", pctRatio(a.HitRate))
	fmt.Printf("  %-28s %14s\n", "Mean return per decision", pctRatio(a.MeanReturn))
	fmt.Printf("  %-28s %14s\n", "Spread of returns", pctRatio(a.StdDev))
	fmt.Printf("  %-28s %14s\n", "t-statistic on the mean", ratio(a.TStat))
	if a.DecisionsForT2 > 0 {
		fmt.Printf("  %-28s %14d\n", "Decisions needed for |t| = 2", a.DecisionsForT2)
	}
}

// cmdForwardVerify walks the chain and says whether it holds.
func cmdForwardVerify(args []string) error {
	fs := flag.NewFlagSet("forward verify", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(os.Getenv("PYRITE_CONFIG"))
	if err != nil {
		return err
	}
	l, err := openForwardLog(cfg)
	if err != nil {
		return err
	}
	v, err := l.Verify()
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return err
		}
	} else {
		printForwardVerify(l, v)
	}
	// A broken chain exits non-zero so a schedule that records every session
	// stops instead of quietly carrying on writing into a log nobody can
	// believe any more.
	if !v.Intact {
		return exitCode(2)
	}
	return nil
}

func printForwardVerify(l *forward.Log, v forward.Verification) {
	if v.Entries == 0 && v.Intact {
		fmt.Printf("Nothing recorded yet, so there is nothing to verify.\n\n")
		fmt.Printf("  %s\n", l.Path())
		return
	}
	if v.Intact {
		fmt.Printf("The chain is intact.\n\n")
		fmt.Printf("  %-28s %d\n", "Records", v.Entries)
		fmt.Printf("  %-28s %s to %s\n", "Covering", v.FirstAs, v.LastAs)
		fmt.Printf("  %-28s %s\n", "First written", v.First.Local().Format("2006-01-02 15:04"))
		fmt.Printf("  %-28s %s\n", "Last written", v.Last.Local().Format("2006-01-02 15:04"))
		fmt.Printf("  %-28s %s\n", "Log", l.Path())
		fmt.Printf("\n  %s\n", wrapIndent("Every record still hashes to the value it was sealed with, and each one "+
			"still names the record before it. Nothing has been edited, removed or reordered since it was written. "+
			"That is what makes the scorecard worth reading; it is not proof against someone rewriting the file "+
			"from the first record onwards, which nothing kept on this disk could be.", 74, "  "))
	} else {
		fmt.Printf("The chain is broken at record %d.\n\n", v.BreakAt)
		fmt.Printf("  %s\n", wrapIndent(v.Reason, 74, "  "))
		fmt.Printf("\n  %-28s %d\n", "Records read", v.Entries)
		fmt.Printf("  %-28s %d\n", "Records still trustworthy", v.BreakAt)
		fmt.Printf("  %-28s %s\n", "Log", l.Path())
		fmt.Printf("\n  %s\n", wrapIndent("Everything before that record still verifies and can be scored. "+
			"From it onwards the log says only what someone wanted it to say, which is worth nothing as evidence. "+
			"There is no repair for this: a rewritten record cannot be restored, and a chain that could be "+
			"mended by the tool would not have been worth having.", 74, "  "))
	}
	for _, s := range v.Skipped {
		fmt.Printf("\n  %s\n", s)
	}
}

// cmdForwardList shows what has been recorded.
func cmdForwardList(args []string) error {
	fs := flag.NewFlagSet("forward list", flag.ExitOnError)
	strategyName := fs.String("strategy", "", "list only the records under this name")
	asJSON := fs.Bool("json", false, "print the records as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(os.Getenv("PYRITE_CONFIG"))
	if err != nil {
		return err
	}
	l, err := openForwardLog(cfg)
	if err != nil {
		return err
	}
	r, err := l.Read()
	if err != nil {
		return err
	}
	entries := r.Entries
	if *strategyName != "" {
		var kept []forward.Entry
		for _, e := range entries {
			if strings.EqualFold(e.Strategy, *strategyName) {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(forward.Reading{Entries: entries, Skipped: r.Skipped})
	}

	if len(entries) == 0 {
		fmt.Printf("Nothing recorded yet.\n\n")
		fmt.Printf("  pyrite forward record --example golden-cross\n\n")
		fmt.Printf("  %s\n", wrapIndent("That writes down what the strategy wants to hold when the "+
			"market next opens, sealed so it cannot be changed afterwards. Run it every session; "+
			"`pyrite forward score` measures the records once their sessions have happened.", 74, "  "))
		return nil
	}

	fmt.Printf("What has been written down, before the fact\n\n")
	fmt.Printf("  %-10s %-16s %-20s %5s  %s\n", "as of", "written", "strategy", "held", "book")
	for _, e := range entries {
		fmt.Printf("  %-10s %-16s %-20s %5d  %s\n",
			e.AsOf, e.At.Local().Format("2006-01-02 15:04"),
			truncate(e.Strategy, 20), e.Horizon, truncate(e.Describe(), 30))
	}
	fmt.Printf("\n  %d %s under %d %s.\n",
		len(entries), plural(len(entries), "record"),
		countStrategies(entries), plural(countStrategies(entries), "name"))
	for _, s := range r.Skipped {
		fmt.Printf("  %s\n", s)
	}
	fmt.Printf("\n  pyrite forward verify    check nothing has been altered since\n")
	fmt.Printf("  pyrite forward score     measure the ones with an outcome\n")
	return nil
}

func countStrategies(entries []forward.Entry) int {
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Strategy] = true
	}
	return len(seen)
}

func chainLine(v forward.Verification) string {
	if v.Intact {
		return fmt.Sprintf("%d %s, intact", v.Entries, plural(v.Entries, "record"))
	}
	return fmt.Sprintf("%d records, BROKEN at %d", v.Entries, v.BreakAt)
}

func shortSHA(h string) string {
	if h == "" {
		return "—"
	}
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// plural is the smallest English the CLI needs; anything cleverer would be a
// dependency for the sake of an "s".
func plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}
