// Command pyrite runs the pyrite web application and CLI.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/charbelkassab/pyrite/examples"
	"github.com/charbelkassab/pyrite/internal/app"
	"github.com/charbelkassab/pyrite/internal/config"
	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/ledger"
	"github.com/charbelkassab/pyrite/internal/market"
	"github.com/charbelkassab/pyrite/internal/mcp"
	"github.com/charbelkassab/pyrite/internal/server"
	"github.com/charbelkassab/pyrite/internal/strategy"
)

// version is overridden at build time with -ldflags "-X main.version=..."
var version = "dev"

const usage = `pyrite — describe a trading strategy in plain language, then find out
whether the result means anything.

Usage:
  pyrite serve [flags]              start the web app (default)
  pyrite run "<strategy>"           one backtest, with its own critique
  pyrite run --example NAME         run a bundled strategy, no key needed
  pyrite examples                   list the bundled strategies
  pyrite report "<strategy>"        the full battery, as one document
  pyrite scenarios "<strategy>"     replay it through named historical crises
  pyrite diff --example A --example B
                                    run two strategies over one setup and test
                                    whether the gap between them is real

Searching, because one backtest is one point in a space:
  pyrite sweep "<strategy>"         every combination, plus a heatmap and
                                           the overfitting statistics
  pyrite walkforward "<strategy>"   choose on one period, report on the next
  pyrite cpcv "<strategy>"          hold out every combination of periods, and
                                           report the spread of the out-of-sample
                                           paths rather than one of them
  pyrite improve "<strategy>"       guided search against a blind holdout
  pyrite ledger                     how much searching each dataset has
                                           already absorbed, across sessions

The one test nobody can iterate against, because the future has not happened:
  pyrite forward record             write down what the strategy wants to hold
                                    next session, sealed in a hash chain
  pyrite forward score              measure the records whose sessions have
                                    since happened
  pyrite forward verify             check that nothing recorded was altered
  pyrite forward list               what has been written down so far

Handing a result to somebody else, so they can check it:
  pyrite bundle export --out run.pyrite    the strategy, the spec and every
                                    bar the run read, in one file
  pyrite bundle run run.pyrite      re-run it with no network and no keys,
                                    and get an exact match or the session
                                    the two runs parted on
  pyrite bundle show run.pyrite     what is inside, without running it

Before you trust any of it, check the data it rests on:
  pyrite audit AAPL MSFT SPY        unadjusted splits, stale prices, missing
                                    sessions, impossible bars. Exits 2 on a
                                    critical finding, so a pipeline can stop.
  pyrite selftest                   run the critique against strategies built
                                    to be caught, and see which findings land.
                                    Offline, no key. Exits 1 if one is missed.

Reference data:
  pyrite ingest edgar               point-in-time share counts, from SEC filings
  pyrite ingest index               point-in-time S&P 500 membership

Driving it from an agent:
  pyrite mcp                        serve the Model Context Protocol on stdio

Everything else:
  pyrite doctor                     check data, model providers and caches
  pyrite api                        print the strategy API reference
  pyrite cache clear [--ai]         clear cached market data and replies
  pyrite version

Common flags:
  --from        backtest start date, YYYY-MM-DD         (default 5 years ago)
  --to          backtest end date, YYYY-MM-DD           (default today)
  --cash        starting capital                        (default 100000)
  --benchmark   comma separated comparison symbols      (default SPY)
  --universe    tradable symbols, a universe name, or sp500 for
                point-in-time index membership
  --code-file   run a strategy you already have, skipping the compiler
  --impact      market impact coefficient; 1 is the usual estimate
  --interval    bar size: 1m, 5m, 15m, 30m, 1h, 1d, 1wk, 1mo   (default 1d)
  --offline     use synthetic data, no network, no keys
  --json        print machine readable output

  ledger:       --dataset <key>  --reset [--yes]  --all
  forward:      --example/--code-file  --name <label>  --horizon <sessions>
                --as-of <date>   for backfilling a missed session or testing
                                 the loop; a record written after its outcome
                                 existed is marked and kept out of the figures
  audit:        --csv-dir ./export   audit your own vendor CSVs
  diff:         two of --example/--code-file, in either combination; the
                first is A and the second is B. --seed fixes the bootstrap.
  run:          --cost-scan      re-run at 0, 5, 20 and 50 bps of slippage
                --capacity       re-run from $100k to $1bn with market impact
                                 on, and report the size the edge dies at
                --decay          the average trade's cumulative return 1 to
                                 40 bars after entry, and where it peaks
                --factors        regress the returns on market, size, value,
                                 momentum and low volatility, and report what
                                 alpha is left
                --null-strategy  compare against random trading with the
                                 same trade count, holding period and exposure
  sweep:        --param fast=10,20,50   --objective sharpe   --csv out.csv
  walkforward:  --train 504  --test 126  --embargo 200  --anchored
  cpcv:         --groups 6   --test-groups 2   --embargo 200
                --no-walkforward
  improve:      --budget 6   --holdout 0.3   --goal "..."
  report:       --out report.md  --html report.html  --no-sweep
                --no-walkforward  --no-scenarios  --cpcv
  scenarios:    --list        print the windows and their dates, run nothing
                --from/--to   consider only windows inside that range

Model provider keys are read from the environment:
  OPENAI_API_KEY, CEREBRAS_API_KEY, KIMI_API_KEY (or MOONSHOT_API_KEY)
A key is needed only to compile plain language. Everything else — including
every search above — runs on --code-file with no key at all.

Every run and sweep is counted in the research ledger, so the trial count
outlives the session that produced it. PYRITE_NO_LEDGER=1 turns that off.

pyrite forward is the only claim here that cannot be reached by iterating:
record every session, and score measures decisions that were written down
before the prices they are scored against existed.

Examples:
  pyrite serve --offline --open
  pyrite run --example golden-cross
  pyrite run "buy $100 of the biggest company by market cap each day, sell when it is no longer number one"
  pyrite sweep "golden cross on SPY" --from 2015-01-01
  pyrite walkforward "each month hold the 20 strongest S&P 500 names" --universe sp500
  pyrite cpcv --example golden-cross --from 2010-01-05
  pyrite report "a 60/40 portfolio rebalanced quarterly" --html report.html
  pyrite scenarios --example sixty-forty
  pyrite diff --example golden-cross --example mean-reversion
`

func main() {
	// The engine stamps every result with the build identity, so a saved run
	// records which binary produced it.
	engine.Version = version
	if err := run(); err != nil {
		// A command may want to fail without having failed: `audit` exits
		// non-zero on a critical finding so a data pipeline can stop, and
		// printing "error:" over a report it just rendered would be a lie.
		var code exitCode
		if errors.As(err, &code) {
			os.Exit(int(code))
		}
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
}

// exitCode is a status a command asks for, rather than a failure to report.
type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit status %d", int(c)) }

func run() error {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "serve":
		return cmdServe(args)
	case "mcp":
		return cmdMCP(args)
	case "run":
		return cmdRun(args)
	case "diff":
		return cmdDiff(args)
	case "audit":
		return cmdAudit(args)
	case "selftest":
		return cmdSelfTest(args)
	case "doctor", "health":
		return cmdDoctor(args)
	case "api":
		fmt.Print(strategy.APIReference())
		return nil
	case "cache":
		return cmdCache(args)
	case "ingest":
		return cmdIngest(args)
	case "sweep":
		return cmdSweep(args)
	case "walkforward", "wf":
		return cmdWalkForward(args)
	case "cpcv":
		return cmdCPCV(args)
	case "improve":
		return cmdImprove(args)
	case "report":
		return cmdReport(args)
	case "scenarios":
		return cmdScenarios(args)
	case "examples":
		return cmdExamples(args)
	case "bundle":
		return cmdBundle(args)
	case "ledger":
		return cmdLedger(args)
	case "forward":
		return cmdForward(args)
	case "version", "-v", "--version":
		fmt.Printf("pyrite %s\n", version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// newApp loads configuration and constructs the application.
func newApp(fs *flag.FlagSet, offline *bool) (*app.App, error) {
	cfg, err := config.Load(os.Getenv("PYRITE_CONFIG"))
	if err != nil {
		return nil, err
	}
	if offline != nil && *offline {
		cfg.OfflineMode = true
	}
	return app.New(cfg)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "", "host:port to listen on")
	offline := fs.Bool("offline", false, "use synthetic data and disable network access")
	open := fs.Bool("open", false, "open the app in a browser after starting")
	dev := fs.String("dev", "", "serve the front end from this directory instead of the embedded copy")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := newApp(fs, offline)
	if err != nil {
		return err
	}
	if *addr != "" {
		a.Cfg.Addr = *addr
	}

	srv := server.New(a)
	if *dev != "" {
		if err := srv.UseDevAssets(*dev); err != nil {
			return err
		}
		fmt.Printf("  assets     %s (live, no rebuild needed)\n", *dev)
	}

	fmt.Printf("pyrite %s\n", version)
	fmt.Printf("  data       %s\n", a.Store.ProviderName())
	fmt.Printf("  models     %s\n", a.DescribeRoutes())
	fmt.Printf("  cache      %s\n", a.Cfg.DataDir)
	if !a.Cfg.AnyProviderEnabled() {
		fmt.Printf("\n  No model found, so plain-English strategies cannot be compiled yet.\n")
		fmt.Printf("  Everything else works: paste code in the Code tab, or run a bundled\n")
		fmt.Printf("  strategy with `pyrite run --example golden-cross`.\n\n")
		fmt.Printf("  To turn compilation on:\n")
		fmt.Printf("    free   — ollama pull qwen2.5-coder:7b, then restart\n")
		fmt.Printf("    hosted — export OPENAI_API_KEY, CEREBRAS_API_KEY or KIMI_API_KEY\n")
	} else if !a.Cfg.AnyCloudProviderEnabled() {
		fmt.Printf("\n  Served by a local model. Free, and slower than a hosted one.\n")
	}
	if a.Cfg.OfflineMode {
		fmt.Printf("\n  Offline: prices are deterministic synthetic data, not the market.\n")
	}
	fmt.Printf("\n  ready on http://%s\n\n", a.Cfg.Addr)

	if *open {
		go openBrowser("http://" + a.Cfg.Addr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.ListenAndServe(ctx, a.Cfg.Addr)
}

// cmdMCP serves the Model Context Protocol on stdio, so an agent can drive
// pyrite as a tool.
//
// Nothing in this command may write to stdout: it carries protocol frames and
// a single stray line desynchronises the stream, which the client reports as a
// parse error with no indication of where it came from. The startup line goes
// to stderr, which is where Claude Desktop and Claude Code put a server's log.
func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	offline := fs.Bool("offline", false, "use synthetic data and disable network access")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := newApp(fs, offline)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "pyrite %s serving MCP on stdio: data %s, cache %s\n",
		version, a.Store.ProviderName(), a.Cfg.DataDir)
	if a.Cfg.OfflineMode {
		fmt.Fprintf(os.Stderr, "offline: prices are synthetic, not the market\n")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return mcp.New(a, version).Serve(ctx, os.Stdin, os.Stdout)
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	from := fs.String("from", "", "start date YYYY-MM-DD")
	to := fs.String("to", "", "end date YYYY-MM-DD")
	cash := fs.Float64("cash", 100000, "starting capital")
	benchmark := fs.String("benchmark", "SPY", "comma separated comparison symbols")
	universe := fs.String("universe", "", "override the tradable symbols")
	offline := fs.Bool("offline", false, "use synthetic data and disable network access")
	asJSON := fs.Bool("json", false, "print the full result as JSON")
	showCode := fs.Bool("code", false, "print the generated strategy code")
	fillClose := fs.Bool("fill-close", false, "fill at the same day's close instead of the next open")
	codeFile := fs.String("code-file", "", "run this JavaScript strategy instead of compiling a prompt")
	example := fs.String("example", "",
		"run a bundled example; `pyrite examples` lists them")
	costScan := fs.Bool("cost-scan", false, "also re-run at 0, 5, 20 and 50 bps of slippage")
	capacity := fs.Bool("capacity", false,
		"also re-run at $100k, $1m, $10m, $100m and $1bn with market impact on, and report "+
			"the size at which the edge disappears")
	decay := fs.Bool("decay", false,
		"also report the average trade's cumulative return at fixed horizons after entry")
	factors := fs.Bool("factors", false,
		"also decompose the returns against ETF factor proxies and report the residual alpha")
	nullStrategy := fs.Bool("null-strategy", false,
		"also compare against random strategies matched on trade count, holding period and exposure")
	impact := fs.Float64("impact", 0,
		"market impact coefficient; 1 is the usual estimate, 0 disables the model")
	interval := fs.String("interval", "1d",
		"bar size: "+strings.Join(market.IntervalNames(), ", "))
	calendar := fs.String("calendar", "",
		"trading calendar to annualise by ("+strings.Join(market.CalendarNames(), ", ")+
			"); the default infers it from the data")
	borrowFile := fs.String("borrow-file", "",
		"CSV of per-symbol short borrow rates: symbol,annual_pct[,available]")
	// A bundled example and a compiled strategy both declare this for
	// themselves; a hand-written --code-file has nowhere to say it, so
	// without the flag its shorts are silently clamped to flat.
	allowShort := fs.Bool("allow-short", false, "permit short positions")
	warmupFlag := fs.Int("warmup", 0, "bars of history to load before the start date")
	// Separate the prompt from the flags before parsing. Go's flag package
	// stops at the first positional argument, so without this a command like
	//   pyrite run "buy SPY" --from 2020-01-01
	// would silently ignore every flag and fold them into the prompt.
	prompt, flagArgs := splitPromptAndFlags(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	// Anything still positional belongs to the prompt too.
	prompt = strings.TrimSpace(strings.Join(append([]string{prompt}, fs.Args()...), " "))
	if prompt == "" && *codeFile == "" && *example == "" {
		return fmt.Errorf("describe a strategy, for example:\n" +
			"  pyrite run \"buy SPY when the 50 day average crosses above the 200 day\"\n\n" +
			"No API key yet? Try a bundled one instead — it needs nothing:\n" +
			"  pyrite run --example golden-cross\n" +
			"  pyrite examples")
	}

	a, err := newApp(fs, offline)
	if err != nil {
		return err
	}
	if *codeFile == "" && *example == "" && !a.Cfg.AnyProviderEnabled() {
		return fmt.Errorf("compiling plain English needs a model, and none is configured.\n\n" +
			"  Try a bundled strategy instead — it needs nothing:\n" +
			"    pyrite run --example golden-cross\n" +
			"    pyrite examples\n\n" +
			"  Or turn compilation on:\n" +
			"    free   — install Ollama, then: ollama pull qwen2.5-coder:7b\n" +
			"    hosted — export OPENAI_API_KEY, CEREBRAS_API_KEY or KIMI_API_KEY\n" +
			"    then:    pyrite doctor")
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
		// A point-in-time index has no static expansion, so it travels as a
		// name and the engine resolves it per session.
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

	if *showCode {
		fmt.Println(plan.Code)
		fmt.Println()
	}

	spec := app.BuildSpec(plan, prompt, opts)
	if *warmupFlag > 0 {
		spec.Warmup = *warmupFlag
	}
	if *allowShort {
		spec.AllowShort = true
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
	cal, err := market.ParseCalendar(*calendar)
	if err != nil {
		return err
	}
	spec.Calendar = cal
	// The command line wins over the config file, because a borrow file is
	// the kind of thing that belongs to one experiment rather than to the
	// installation.
	if path := firstNonEmpty(*borrowFile, a.Cfg.BorrowCSV); path != "" {
		sched, err := engine.LoadBorrowCSV(path)
		if err != nil {
			return err
		}
		spec.Costs.Borrow = sched
	}
	lastPct := -1
	opts.Progress = func(done, total int, day market.Day) {
		pct := done * 100 / total
		if pct != lastPct {
			lastPct = pct
			fmt.Fprintf(os.Stderr, "\rbacktesting %3d%%  %s", pct, day)
		}
	}
	res, err := a.Backtest(ctx, spec, opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\r%40s\r", "")

	// One run is one trial against this dataset, and the ledger is the only
	// place that number survives to be added to the next one.
	note := recordInvocation(a.Cfg, ledger.Entry{
		DatasetKey: ledger.DatasetKey(datasetOf(res.Spec)),
		Strategy:   plan.Name,
		CodeSHA256: res.Manifest.CodeSHA256,
		Trials:     1,
		Objective:  "sharpe",
		BestScore:  res.Metrics.Sharpe,
		// One trial has no spread. Recording a zero would claim the run
		// found every configuration equally good, which it never looked.
		ScoreSpread: engine.Ratio(math.NaN()),
	})

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		printLedgerNote(note, true)
		return enc.Encode(map[string]any{"plan": plan, "result": res})
	}
	printReport(plan, res)
	printLedgerNote(note, false)

	// The cost scan runs after the report because it is a separate question:
	// not "how did this do" but "how much of that survives friction".
	if *costScan {
		scan, err := engine.RunCostScan(ctx, spec, a.Store, nil)
		if err != nil {
			return err
		}
		printCostScan(scan)
	}
	// Capacity asks the same question with the other variable moved: not how
	// much survives a friction level somebody chose, but how much survives the
	// friction the strategy causes itself once the account is large.
	if *capacity {
		ladder, err := engine.RunCapacity(ctx, spec, a.Store, nil, spec.Costs.ImpactCoefficient)
		if err != nil {
			return err
		}
		printCapacity(ladder)
	}
	// The decay curve needs no re-run: it is built from the round trips the
	// backtest already produced, so the flag governs whether it is printed
	// rather than whether it is computed.
	if *decay {
		printDecay(res.Decay)
	}
	// And the factor decomposition asks the third question: how much of it
	// was the strategy rather than an exposure that already has a name.
	if *factors {
		fx, err := engine.AnalyseFactors(ctx, res.Curve, a.Store,
			spec.Interval, res.Spec.Scale(), nil)
		if err != nil {
			return err
		}
		printFactors(fx)
	}
	// The null comparison is behind a flag for the same reason: it is another
	// question again — not "how much survives friction" but "how much of this
	// is just being in the market" — and it costs a thousand extra passes over
	// the return series to answer.
	if *nullStrategy {
		printNullComparison(engine.RunNullStrategy(ctx, res, a.Store, 0, spec.Seed))
	}
	return nil
}

// planSource is how a command was told which strategy to run: a bundled
// example, a file of JavaScript, or plain language to compile.
type planSource struct {
	prompt   string
	example  string
	codeFile string
	// benchmarkFlag is the raw --benchmark value, so an example's own
	// benchmarks can stand when the user did not ask for different ones.
	benchmarkFlag string
}

// loadPlan resolves a strategy from the command line, filling in the run
// options an example declares for itself.
//
// Shared by `run` and `bundle export` so the two cannot drift: a bundle that
// ran a different strategy from the one the same flags give `pyrite run`
// would be worse than no bundle at all.
func loadPlan(ctx context.Context, a *app.App, src planSource, opts *app.RunOptions) (*strategy.Plan, error) {
	switch {
	case src.example != "":
		ex, err := examples.Get(src.example)
		if err != nil {
			return nil, err
		}
		plan := &strategy.Plan{
			Name: ex.Label, Description: firstNonEmpty(ex.Title, ex.Summary), Code: ex.Code,
			Universe: ex.Universe, Benchmarks: ex.Benchmarks,
			Warmup: ex.Warmup, AllowShort: ex.AllowShort,
		}
		if plan.Name == "" {
			plan.Name = ex.Name
		}
		// The example's own declarations stand unless the caller overrode
		// them on the command line.
		if len(opts.Universe) == 0 && len(ex.Universe) > 0 {
			opts.Universe = market.ResolveUniverse(strings.Join(ex.Universe, ","))
			opts.Index = market.IndexUniverse(strings.Join(ex.Universe, ","))
		}
		if src.benchmarkFlag == "SPY" && len(ex.Benchmarks) > 0 {
			opts.Benchmarks = market.ResolveUniverse(strings.Join(ex.Benchmarks, ","))
		}
		if ex.NeedsModel && !a.Cfg.AnyProviderEnabled() {
			return nil, fmt.Errorf("the %q example calls a model inside the backtest, so it needs one.\n"+
				"  free   — install Ollama, then: ollama pull qwen2.5-coder:7b\n"+
				"  hosted — export OPENAI_API_KEY, CEREBRAS_API_KEY or KIMI_API_KEY\n"+
				"Every other example runs with nothing: pyrite examples", ex.Name)
		}
		return plan, nil

	case src.codeFile != "":
		code, err := os.ReadFile(src.codeFile)
		if err != nil {
			return nil, err
		}
		return &strategy.Plan{Name: filepath.Base(src.codeFile), Code: string(code)}, nil

	default:
		fmt.Fprintf(os.Stderr, "compiling strategy with %s...\n", a.DescribeRoutes())
		started := time.Now()
		plan, err := a.Compiler.Compile(ctx, strategy.Request{
			Prompt:   src.prompt,
			Universe: opts.Universe,
			Start:    opts.Start,
			End:      opts.End,
		})
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "compiled in %s using %s/%s (attempt %d)\n\n",
			time.Since(started).Round(time.Millisecond), plan.Provider, plan.Model, plan.Attempts)
		return plan, nil
	}
}

// printFactors reports what is left of the strategy once known risk premia
// are taken out of it.
func printFactors(f *engine.FactorExposure) {
	fmt.Printf("\nWhat is left after known factors?\n")
	fmt.Printf("  %-15s %-12s %10s %10s %10s\n", "Factor", "Proxy", "Beta", "Std err", "t-stat")
	for _, l := range f.Factors {
		fmt.Printf("  %-15s %-12s %10s %10s %10s\n",
			l.Name, l.Proxy, ratio(l.Beta), ratio(l.StdErr), ratio(l.TStat))
	}
	fmt.Printf("  %-28s %10s %10s %10s\n", "Alpha, annualised",
		pctOrNA(f.Alpha), pctOrNA(f.AlphaStdErr), ratio(f.AlphaTStat))

	fmt.Printf("\n  %-30s %14s\n", "R² (adjusted)",
		fmt.Sprintf("%s (%s)", ratio(f.RSquared), ratio(f.AdjRSquared)))
	fmt.Printf("  %-30s %14d\n", "Observations", f.Observations)
	fmt.Printf("  %-30s %14d\n", "Newey-West lag", f.NeweyWestLag)
	if f.AvgExposure.Defined() {
		// Printed beside the market beta because the two only mean anything
		// together: a low beta from a fully invested book and a low beta
		// from a strategy that was flat half the time are different facts.
		fmt.Printf("  %-30s %14s\n", "Average gross exposure", ratio(f.AvgExposure))
	}

	for _, d := range f.Dropped {
		fmt.Printf("  %-30s %s\n", "Dropped: "+d.Name,
			wrapIndent(d.Reason, 44, strings.Repeat(" ", 33)))
	}
	if f.Verdict != "" {
		fmt.Printf("\n  %s\n", wrapIndent(f.Verdict, 74, "  "))
	}
	fmt.Printf("\n  %s\n", wrapIndent(f.ProxyNote, 74, "  "))
}

// pctOrNA renders a percentage that may legitimately be undefined.
func pctOrNA(r engine.Ratio) string {
	if !r.Defined() {
		return "n/a"
	}
	return pct(float64(r))
}

// printNullComparison reports one run against random trading with the same
// habits.
func printNullComparison(ns engine.NullStrategy) {
	fmt.Printf("\nOr is it just being in the market?\n")
	if !ns.Percentile.Defined() {
		reason := ns.Verdict
		if reason == "" {
			reason = "the price series for what this strategy traded could not be rebuilt"
		}
		fmt.Printf("  %s\n", wrapIndent(reason, 74, "  "))
		return
	}
	fmt.Printf("  %-30s %14d\n", "Random strategies generated", ns.Trials)
	fmt.Printf("  %-30s %14d\n", "Matched on holds", ns.Episodes)
	fmt.Printf("  %-30s %14s\n", "Matched on median hold",
		fmt.Sprintf("%d bars", ns.MedianHoldBars))
	fmt.Printf("  %-30s %14s\n", "Matched on average exposure", pct(ns.AvgExposure))
	fmt.Printf("  %-30s %14s\n", "Strategy, timing only", ratio(ns.Score))
	fmt.Printf("  %-30s %14s\n", "Random, median", ratio(ns.NullMedian))
	fmt.Printf("  %-30s %14s\n", "Random, 95th percentile", ratio(ns.NullP95))
	fmt.Printf("  %-30s %14s\n", "Percentile", pct(float64(ns.Percentile)))
	if ns.ReportedSharpe.Defined() && ns.Score.Defined() {
		fmt.Printf("\n  Against a reported Sharpe of %s. The two differ by whatever the\n"+
			"  strategy did other than choose when to be invested.\n",
			ratio(ns.ReportedSharpe))
	}
	if ns.Verdict != "" {
		fmt.Printf("\n  %s\n", wrapIndent(ns.Verdict, 74, "  "))
	}

}

// printCostScan reports the same strategy at several friction levels.
func printCostScan(s *engine.CostScan) {
	fmt.Printf("\nHow much survives friction?\n")
	fmt.Printf("  %-12s %14s %12s %12s %10s\n", "Slippage", "Return", "CAGR", "Sharpe", "Costs")
	for _, p := range s.Points {
		if p.Error != "" {
			fmt.Printf("  %-12s %s\n", fmt.Sprintf("%.0f bps", p.SlippageBps), truncate(p.Error, 50))
			continue
		}
		fmt.Printf("  %-12s %14s %12s %12s %10s\n",
			fmt.Sprintf("%.0f bps", p.SlippageBps),
			pct(p.TotalReturn), pct(p.CAGR), ratio(p.Sharpe), money(p.TotalCosts))
	}
	if s.BreakEvenBps.Defined() {
		fmt.Printf("\n  %-30s %14s\n", "Break-even slippage", fmt.Sprintf("%.1f bps", float64(s.BreakEvenBps)))
	}
	if s.Verdict != "" {
		fmt.Printf("\n  %s\n", wrapIndent(s.Verdict, 74, "  "))
	}
}

// printCapacity reports the same strategy at several account sizes.
func printCapacity(c *engine.Capacity) {
	fmt.Printf("\nHow much money can this take?\n")
	fmt.Printf("  %-12s %14s %12s %12s %10s\n",
		"Capital", "Return", "CAGR", "Sharpe", "Friction")
	for _, p := range c.Points {
		if p.Error != "" {
			fmt.Printf("  %-12s %s\n", compactMoney(p.Capital), truncate(p.Error, 50))
			continue
		}
		fmt.Printf("  %-12s %14s %12s %12s %10s\n", compactMoney(p.Capital),
			pct(p.TotalReturn), pct(p.CAGR), ratio(p.Sharpe),
			fmt.Sprintf("%.0f bps", p.CostBps))
	}
	fmt.Printf("\n  %-30s %14.2f\n", "Impact coefficient", c.ImpactCoefficient)
	if c.ZeroReturnCapital.Defined() {
		fmt.Printf("  %-30s %14s\n", "Largest size above zero",
			compactMoney(float64(c.ZeroReturnCapital)))
	}
	if c.BenchmarkCapital.Defined() {
		fmt.Printf("  %-30s %14s\n", "Largest size beating "+c.BenchmarkLabel,
			compactMoney(float64(c.BenchmarkCapital)))
	}
	if c.Verdict != "" {
		fmt.Printf("\n  %s\n", wrapIndent(c.Verdict, 74, "  "))
	}
}

// printDecay reports when the average trade's edge arrived and when it went.
func printDecay(d engine.SignalDecay) {
	fmt.Printf("\nWhen does the edge arrive, and when does it go?\n")
	if len(d.Points) == 0 {
		reason := d.Verdict
		if reason == "" {
			reason = "no closed round trip had enough price history to measure"
		}
		fmt.Printf("  %s\n", wrapIndent(reason, 74, "  "))
		return
	}
	fmt.Printf("  %-14s %14s %14s\n", "Bars", "Mean return", "Still open")
	for _, p := range d.Points {
		fmt.Printf("  %-14d %14s %14s\n", p.Bars, pct(p.MeanReturn),
			fmt.Sprintf("%d of %d", p.StillOpen, d.Trades))
	}
	fmt.Printf("\n  %-30s %14s\n", "Peak",
		fmt.Sprintf("%s at bar %d", pctOrNA(d.PeakReturn), d.PeakBars))
	fmt.Printf("  %-30s %14s\n", "At the exit", pctOrNA(d.ExitReturn))
	fmt.Printf("  %-30s %14s\n", "Average hold", fmt.Sprintf("%.1f bars", d.MeanBarsHeld))
	if d.GivenBack.Defined() {
		fmt.Printf("  %-30s %14s\n", "Given back by the exit", pct(float64(d.GivenBack)))
	}
	if d.Verdict != "" {
		fmt.Printf("\n  %s\n", wrapIndent(d.Verdict, 74, "  "))
	}
}

// printReport renders a human-readable summary to stdout.
func printReport(plan *strategy.Plan, res *engine.Result) {
	m := res.Metrics
	fmt.Printf("%s\n", plan.Name)
	if plan.Description != "" {
		fmt.Printf("%s\n", wrap(plan.Description, 76))
	}
	unit := "trading days"
	if res.Spec.Interval.Intraday() {
		unit = string(res.Spec.Interval) + " bars"
	}
	fmt.Printf("\n%s to %s   %d %s   universe of %d\n",
		res.Spec.Start, res.Spec.End, m.TradingDays, unit, len(res.Spec.Universe))
	// The annualisation factor is stated rather than left to be inferred:
	// the same trades scored on 252 and on 365 give Sharpes 20% apart, and
	// nothing else on this page says which was used.
	fmt.Printf("Annualised on the %s calendar: %s bars a year\n\n",
		res.Manifest.TradingCalendar.Label(), trimTrailingZeros(res.Manifest.PeriodsPerYear))

	fmt.Printf("  %-22s %14s\n", "Starting capital", money(m.StartValue))
	fmt.Printf("  %-22s %14s\n", "Final value", money(m.EndValue))
	fmt.Printf("  %-22s %14s\n", "Total return", pct(m.TotalReturn))
	fmt.Printf("  %-22s %14s\n", "Annualised (CAGR)", pct(m.CAGR))
	fmt.Printf("  %-22s %14s\n", "Volatility", pct(m.Volatility))
	fmt.Printf("  %-22s %14s\n", "Sharpe ratio", ratio(m.Sharpe))
	fmt.Printf("  %-22s %14s\n", "Sortino ratio", ratio(m.Sortino))
	fmt.Printf("  %-22s %14s\n", "Max drawdown", pct(m.MaxDrawdown))
	fmt.Printf("  %-22s %14d\n", "Trades", m.TotalTrades)
	if m.TotalTrades > 0 {
		fmt.Printf("  %-22s %14s\n", "Trade win rate", pct(m.TradeWinRate))
		fmt.Printf("  %-22s %14s\n", "Profit factor", ratio(m.ProfitFactor))
	}
	fmt.Printf("  %-22s %14s\n", "Costs paid", money(m.TotalCosts))

	printRoundTrips(res)
	printBorrow(res)
	printRisk(res)
	printByYear(res)
	printRegimes(res)
	printStress(res)
	printSymbols(res)
	printReasons(res)
	printCritique(res)

	if len(res.Benchmarks) > 0 {
		fmt.Printf("\n  %-22s %14s %14s\n", "Comparison", "Total return", "Max drawdown")
		fmt.Printf("  %-22s %14s %14s\n", truncate(plan.Name, 22), pct(m.TotalReturn), pct(m.MaxDrawdown))
		for _, b := range res.Benchmarks {
			fmt.Printf("  %-22s %14s %14s\n", truncate(b.Label, 22), pct(b.Metric.TotalReturn), pct(b.Metric.MaxDrawdown))
		}
	}

	if len(plan.Assumptions) > 0 {
		fmt.Printf("\nAssumptions made:\n")
		for _, a := range plan.Assumptions {
			fmt.Printf("  - %s\n", wrapIndent(a, 74, "    "))
		}
	}
	if len(plan.Limitations) > 0 {
		fmt.Printf("\nLimitations:\n")
		for _, l := range plan.Limitations {
			fmt.Printf("  - %s\n", wrapIndent(l, 74, "    "))
		}
	}
	if len(res.Warnings) > 0 {
		fmt.Printf("\nWarnings:\n")
		for i, w := range res.Warnings {
			if i >= 5 {
				fmt.Printf("  ... and %d more\n", len(res.Warnings)-5)
				break
			}
			fmt.Printf("  - %s\n", wrapIndent(w, 74, "    "))
		}
	}
	if res.AICallCount > 0 {
		fmt.Printf("\n%d AI or web calls were made during the run.\n", res.AICallCount)
	}
	fmt.Printf("\nCompleted in %s.\n", time.Duration(res.Elapsed)*time.Millisecond)
}

// splitPromptAndFlags divides "prompt words" from "-flags", so the prompt may
// be written before the flags, which is the order people naturally type.
func splitPromptAndFlags(args []string) (string, []string) {
	var prompt []string
	for i, a := range args {
		if len(a) > 1 && strings.HasPrefix(a, "-") {
			return strings.Join(prompt, " "), args[i:]
		}
		prompt = append(prompt, a)
	}
	return strings.Join(prompt, " "), nil
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	offline := fs.Bool("offline", false, "use synthetic data")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	a, err := newApp(fs, offline)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	h := a.Health(ctx, true)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(h)
	}

	fmt.Printf("pyrite %s\n\n", version)
	fmt.Printf("data provider      %s\n", h.DataProvider)
	fmt.Printf("offline mode       %v\n", h.OfflineMode)
	fmt.Printf("web search         %v\n", h.SearchOK)
	fmt.Printf("fundamentals       %s\n", h.Fundamentals)
	fmt.Printf("index membership   %s\n", membershipSummary(a))
	fmt.Printf("data directory     %s\n", h.DataDir)
	fmt.Printf("cached symbols     %d (%.1f MB)\n", h.CachedSymbols, float64(h.CacheBytes)/(1<<20))

	fmt.Printf("\nmodel providers\n")
	for _, p := range h.Providers {
		status := "no API key"
		if p.Local {
			status = "not running at " + p.BaseURL
		}
		switch {
		case p.Reachable != nil && *p.Reachable:
			status = fmt.Sprintf("ok, %d models available", len(p.Models))
		case p.Reachable != nil:
			status = "unreachable: " + truncate(p.Error, 44)
		case p.Enabled:
			status = "detected"
			if !p.Local {
				status = "key present"
			}
		}
		model := p.Model
		if model == "" {
			model = "—"
		}
		fmt.Printf("  %-10s %-28s %s\n", p.Name, truncate(model, 28), status)
	}
	fmt.Printf("\nrouting            %s\n", a.DescribeRoutes())

	// The point of this command is to tell someone what to do next, so it
	// ends with that rather than with a status dump.
	fmt.Printf("\nWhat you can do right now\n")
	if a.Cfg.AnyProviderEnabled() {
		fmt.Printf("  ✓ everything, including plain-English strategies\n")
		if !a.Cfg.AnyCloudProviderEnabled() {
			fmt.Printf("    (served by a local model — free, and slower than a hosted one)\n")
		}
	} else {
		fmt.Printf("  ✓ run, sweep, walkforward and report, with --code-file\n")
		fmt.Printf("  ✓ everything offline on synthetic data, with --offline\n")
		fmt.Printf("  ✗ compiling plain English into a strategy\n")
		fmt.Printf("\n  To turn the last one on, either:\n")
		fmt.Printf("    free   — install Ollama and run:  ollama pull qwen2.5-coder:7b\n")
		fmt.Printf("             pyrite finds it automatically on 127.0.0.1:11434\n")
		fmt.Printf("    hosted — export OPENAI_API_KEY, CEREBRAS_API_KEY or KIMI_API_KEY\n")
	}
	if h.OfflineMode {
		fmt.Printf("\n  Offline mode is on, so prices are deterministic synthetic data,\n")
		fmt.Printf("  not the market. Good for demos and tests, not for conclusions.\n")
	}
	return nil
}

// membershipSummary reports what the point-in-time index table covers.
func membershipSummary(a *app.App) string {
	m, err := a.Store.Membership("sp500")
	if err != nil {
		return "unavailable"
	}
	total := len(m.Symbols())
	current := len(m.MembersOn(market.NewDay(time.Now())))
	return fmt.Sprintf("sp500: %d names, %d current, from %s", total, current, m.Earliest)
}

func cmdCache(args []string) error {
	fs := flag.NewFlagSet("cache", flag.ExitOnError)
	includeAI := fs.Bool("ai", false, "also clear cached model replies")
	onlyAI := fs.Bool("ai-only", false, "clear only cached model replies")
	sub := "clear"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if sub != "clear" {
		return fmt.Errorf("unknown cache command %q (try: cache clear)", sub)
	}
	a, err := newApp(fs, nil)
	if err != nil {
		return err
	}
	if err := a.ClearCaches(!*onlyAI, *includeAI || *onlyAI); err != nil {
		return err
	}
	fmt.Println("cache cleared")
	return nil
}

func openBrowser(url string) {
	time.Sleep(300 * time.Millisecond)
	for _, cmd := range [][]string{{"xdg-open", url}, {"open", url}} {
		if err := execCmd(cmd[0], cmd[1]); err == nil {
			return
		}
	}
}

// ---- small formatting helpers -------------------------------------------

func money(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.2f", v)
	// Insert thousands separators.
	dot := strings.Index(s, ".")
	intPart, frac := s[:dot], s[dot:]
	var out []byte
	for i, c := range []byte(intPart) {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	res := "$" + string(out) + frac
	if neg {
		return "-" + res
	}
	return res
}

// compactMoney renders an account size to two significant figures and a
// suffix.
//
// A capacity ladder spans four orders of magnitude, and $1,000,000,000.00 in a
// column beside $100,000.00 is read by counting digits. The interpolated
// thresholds are estimates off five rungs, so the lost precision was never
// there to begin with.
func compactMoney(v float64) string {
	switch a := math.Abs(v); {
	case a >= 1e9:
		return fmt.Sprintf("$%.1fbn", v/1e9)
	case a >= 1e6:
		return fmt.Sprintf("$%.1fm", v/1e6)
	case a >= 1e3:
		return fmt.Sprintf("$%.0fk", v/1e3)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}

func pct(v float64) string { return fmt.Sprintf("%.2f%%", v*100) }

// pctRatio renders a percentage that may legitimately be undefined — a share
// of a P&L that netted to nothing.
func pctRatio(r engine.Ratio) string {
	if !r.Defined() {
		return "n/a"
	}
	return pct(float64(r))
}

// ratio renders a metric that may legitimately be undefined — a Sortino with
// no losing days, a profit factor with no losing trades.
func ratio(r engine.Ratio) string {
	if !r.Defined() {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", float64(r))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func wrap(s string, width int) string { return wrapIndent(s, width, "") }

func wrapIndent(s string, width int, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var lines []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			lines = append(lines, line)
			line = w
		} else {
			line += " " + w
		}
	}
	lines = append(lines, line)
	return strings.Join(lines, "\n"+indent)
}

// printBorrow reports what the short side cost, per name, and what it was
// refused.
//
// Only shown when the run actually went short. A long-only book has nothing
// to say here and an empty section reads as a missing number.
func printBorrow(res *engine.Result) {
	b := res.Borrow
	if !b.Charged() {
		return
	}
	fmt.Printf("\nShort borrow\n")
	fmt.Printf("  %-22s %14s\n", "Total paid", money(b.TotalCost))
	if len(b.Names) > 0 {
		fmt.Printf("  %-22s %8s %6s %8s\n", "Name", "Rate", "Days", "Cost")
		for i, n := range b.Names {
			if i >= 8 {
				fmt.Printf("  ... and %d more\n", len(b.Names)-8)
				break
			}
			label := n.Symbol
			if n.HardToBorrow {
				label += " *"
			}
			fmt.Printf("  %-22s %7.2f%% %6d %8s\n",
				truncate(label, 22), n.AnnualPct*100, n.Sessions, compactMoney(n.Cost))
		}
	}
	for _, r := range b.Refused {
		fmt.Printf("  %-22s %14s\n", "No locate: "+r.Symbol,
			fmt.Sprintf("%d refused", r.Orders))
	}
	if !b.PerName {
		fmt.Printf("  %s\n", wrapIndent("Every name was charged the same rate; no borrow "+
			"file was supplied. Pass --borrow-file to price the hard ones.", 74, "  "))
	}
}

// trimTrailingZeros prints a bar count without a pointless decimal tail: 252,
// not 252.00, and 98280 rather than 98280.0.
func trimTrailingZeros(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

// printRoundTrips reports the entry-to-exit view of the run.
func printRoundTrips(res *engine.Result) {
	t := res.TradeStats
	if t.Closed == 0 {
		return
	}
	fmt.Printf("\nRound trips\n")
	fmt.Printf("  %-22s %14d", "Closed", t.Closed)
	if t.Open > 0 {
		fmt.Printf("   (%d still open)", t.Open)
	}
	fmt.Println()
	fmt.Printf("  %-22s %14s\n", "Win rate", pct(t.WinRate))
	fmt.Printf("  %-22s %14s\n", "Average win", pct(t.AvgWinPct))
	fmt.Printf("  %-22s %14s\n", "Average loss", pct(t.AvgLossPct))
	fmt.Printf("  %-22s %14s\n", "Payoff ratio", ratio(t.PayoffRatio))
	fmt.Printf("  %-22s %14s\n", "Expectancy / trade", money(t.Expectancy))
	fmt.Printf("  %-22s %14.1f\n", "Average bars held", t.AvgBarsHeld)
	fmt.Printf("  %-22s %11d / %2d\n", "Longest win/loss run", t.MaxConsecWins, t.MaxConsecLosses)

	// Excursion analysis only means something when price history was
	// available to measure it against.
	if t.AvgMFEPct != 0 || t.AvgMAEPct != 0 {
		fmt.Printf("  %-22s %14s\n", "Avg worst excursion", pct(t.AvgMAEPct))
		fmt.Printf("  %-22s %14s\n", "Avg best excursion", pct(t.AvgMFEPct))
		fmt.Printf("  %-22s %14s\n", "Edge ratio", ratio(t.EdgeRatio))
		if t.GiveBackTrades > 0 {
			fmt.Printf("  %-22s %14s   (%d losers)\n", "Losers' give-back",
				pct(t.GiveBack), t.GiveBackTrades)
		}
		if t.WinnerMAEPct < 0 {
			fmt.Printf("  %-22s %14s\n", "Winners' worst dip", pct(t.WinnerMAEPct))
		}
	}
}

// printRisk reports the distribution and drawdown statistics.
func printRisk(res *engine.Result) {
	r := res.Risk
	fmt.Printf("\nRisk\n")
	fmt.Printf("  %-22s %14s\n", "Ulcer index", pct(r.UlcerIndex))
	fmt.Printf("  %-22s %14s\n", "Daily VaR (95%)", pct(r.VaR95))
	fmt.Printf("  %-22s %14s\n", "Daily CVaR (95%)", pct(r.CVaR95))
	fmt.Printf("  %-22s %14.2f\n", "Return skew", r.Skew)
	fmt.Printf("  %-22s %14.2f\n", "Excess kurtosis", r.ExcessKurtosis)
	fmt.Printf("  %-22s %14s\n", "Tail ratio", ratio(r.TailRatio))
	fmt.Printf("  %-22s %14s\n", "Omega", ratio(r.Omega))
	fmt.Printf("  %-22s %14s\n", "Gain to pain", ratio(r.GainToPain))
	fmt.Printf("  %-22s %14.2f\n", "Equity curve R²", r.EquityR2)
	if r.UpCapture.Defined() && r.DownCapture.Defined() {
		fmt.Printf("  %-22s %8s / %5s\n", "Up / down capture", ratio(r.UpCapture), ratio(r.DownCapture))
	}
}

// printByYear shows the calendar breakdown, which is where a strategy that
// worked twice usually gives itself away.
func printByYear(res *engine.Result) {
	years := res.Attribution.ByYear
	if len(years) < 2 {
		return
	}
	hasBench := false
	for _, y := range years {
		if y.BenchmarkReturn != 0 {
			hasBench = true
			break
		}
	}
	fmt.Printf("\nBy year\n")
	if hasBench {
		fmt.Printf("  %-8s %12s %12s %12s %12s\n", "", "Return", "Benchmark", "Excess", "Drawdown")
	} else {
		fmt.Printf("  %-8s %12s %12s\n", "", "Return", "Drawdown")
	}
	for _, y := range years {
		if hasBench {
			fmt.Printf("  %-8s %12s %12s %12s %12s\n", y.Label,
				pct(y.Return), pct(y.BenchmarkReturn), pct(y.Excess), pct(y.MaxDrawdown))
		} else {
			fmt.Printf("  %-8s %12s %12s\n", y.Label, pct(y.Return), pct(y.MaxDrawdown))
		}
	}
}

// printRegimes shows behaviour by market condition rather than by calendar.
func printRegimes(res *engine.Result) {
	rs := res.Attribution.ByRegime
	if len(rs) == 0 {
		return
	}
	fmt.Printf("\nBy market regime\n")
	fmt.Printf("  %-22s %12s %12s %8s\n", "", "Return", "Drawdown", "Days")
	for _, r := range rs {
		fmt.Printf("  %-22s %12s %12s %8d\n", r.Label, pct(r.Return), pct(r.MaxDrawdown), r.TradingDays)
	}
}

// printStress shows what is left once the best episodes are removed.
func printStress(res *engine.Result) {
	ss := res.Attribution.Stress
	if len(ss) == 0 {
		return
	}
	fmt.Printf("\nHow concentrated is the edge?\n")
	for _, s := range ss {
		fmt.Printf("  %-30s %12s   (%s of the gain)\n",
			s.Label, pct(s.Return), pct(s.ShareOfTotal))
	}
}

// printSymbols ranks holdings by what they actually contributed.
func printSymbols(res *engine.Result) {
	syms := res.Attribution.BySymbol
	if len(syms) < 2 {
		return
	}
	fmt.Printf("\nWhere the money came from\n")
	fmt.Printf("  %-10s %14s %10s %8s %8s\n", "", "Net P&L", "Share", "Trades", "Win %")
	n := len(syms)
	if n > 10 {
		n = 10
	}
	for _, s := range syms[:n] {
		fmt.Printf("  %-10s %14s %10s %8d %8s\n", s.Symbol,
			money(s.NetPnL), pct(s.Contribution), s.Trades, pct(s.WinRate))
	}
	if len(syms) > n {
		fmt.Printf("  ... and %d more\n", len(syms)-n)
	}
}

// printReasons attributes the P&L to the rule behind each trade.
//
// The symbol table above says which holding paid. This says which rule did,
// which is the version of the question with an action attached to it.
func printReasons(res *engine.Result) {
	tbl := res.Reasons.Table()
	if len(tbl.ByEntry) == 0 && len(tbl.ByExit) == 0 {
		return
	}
	// A run whose orders never said why is worth a line rather than two
	// tables of one row each. There is nothing to compare, and the reader is
	// better told why the section is empty than left to wonder where it went.
	if tbl.Unattributed() {
		fmt.Printf("\nWhich rules made the money\n")
		fmt.Printf("  No order in this run gave a reason, so there is nothing to attribute.\n")
		fmt.Printf("  Pass one to fill this in: ctx.buy(sym, { ... }, \"why\").\n")
		return
	}
	fmt.Printf("\nWhich rules made the money   (reasons grouped %s)\n", tbl.Grouping)
	printReasonRows("Entry rule", tbl.ByEntry)
	if len(tbl.ByEntry) > 0 && len(tbl.ByExit) > 0 {
		fmt.Println()
	}
	printReasonRows("Exit rule", tbl.ByExit)
}

func printReasonRows(header string, rows []engine.ReasonStats) {
	if len(rows) == 0 {
		return
	}
	fmt.Printf("  %-28s %12s %8s %6s %7s %5s\n", header, "Net P&L", "Share", "Trades", "Win %", "Days")
	row := func(r engine.ReasonStats) {
		fmt.Printf("  %-28s %12s %8s %6d %7s %5.0f\n", truncate(r.Reason, 28),
			money(r.NetPnL), pctRatio(r.Share), r.Trades, pct(r.WinRate), r.MeanDaysHeld)
	}
	head, dropped, tail := engine.TopAndBottom(rows, 10)
	for _, r := range head {
		row(r)
	}
	if dropped > 0 {
		fmt.Printf("  ... %d more rules between\n", dropped)
	}
	for _, r := range tail {
		row(r)
	}
}

// cmdIngest builds reference data tables from public sources.
func cmdIngest(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: pyrite ingest edgar [flags]")
	}
	switch args[0] {
	case "edgar":
		return cmdIngestEDGAR(args[1:])
	case "index":
		return cmdIngestIndex(args[1:])
	default:
		return fmt.Errorf("unknown ingest source %q (available: edgar, index)", args[0])
	}
}

// cmdIngestEDGAR rebuilds the point-in-time share-count table from SEC filings.
func cmdIngestEDGAR(args []string) error {
	fs := flag.NewFlagSet("ingest edgar", flag.ContinueOnError)
	symbols := fs.String("symbols", "", "comma-separated tickers to ingest")
	universe := fs.String("universe", "", "a named universe to ingest (megacap, tech, dow, ...)")
	out := fs.String("out", "", "write here instead of stdout")
	agent := fs.String("user-agent", os.Getenv("PYRITE_SEC_USER_AGENT"),
		"identify yourself to the SEC, e.g. \"Jane Doe jane@example.com\" (or set PYRITE_SEC_USER_AGENT)")
	threshold := fs.Float64("threshold", 0.005,
		"drop a filing whose share count moved less than this fraction from the last kept row")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var syms []string
	if *symbols != "" {
		for _, s := range strings.Split(*symbols, ",") {
			if s = strings.TrimSpace(s); s != "" {
				syms = append(syms, s)
			}
		}
	}
	if *universe != "" {
		syms = append(syms, market.ResolveUniverse(*universe)...)
	}
	if len(syms) == 0 {
		return fmt.Errorf("nothing to ingest: pass --symbols or --universe")
	}
	if strings.TrimSpace(*agent) == "" {
		return fmt.Errorf("the SEC requires a User-Agent identifying you.\n" +
			"  Pass --user-agent \"Your Name you@example.com\" or set PYRITE_SEC_USER_AGENT.\n" +
			"  Requests without one are refused, and a generic string risks a block.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var buf bytes.Buffer
	e := market.NewEDGAR(*agent)
	rep, err := e.BuildSharesTable(ctx, syms, *threshold, &buf, func(sym string, i, n int) {
		fmt.Fprintf(os.Stderr, "\r  %-8s %d/%d", sym, i+1, n)
	})
	fmt.Fprintf(os.Stderr, "\r%40s\r", "")
	if err != nil {
		return err
	}

	if *out == "" {
		fmt.Print(buf.String())
	} else {
		if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", *out)
	}

	fmt.Fprintf(os.Stderr, "\n%d rows for %d symbols\n", rep.Rows, len(rep.Symbols))
	if len(rep.Approximate) > 0 {
		fmt.Fprintf(os.Stderr, "\nApproximate (no point-in-time count is published in a machine-readable form):\n")
		approx := make([]string, 0, len(rep.Approximate))
		for sym := range rep.Approximate {
			approx = append(approx, sym)
		}
		sort.Strings(approx)
		for _, sym := range approx {
			fmt.Fprintf(os.Stderr, "  %-8s %s\n", sym, rep.Approximate[sym])
		}
	}
	if len(rep.Skipped) > 0 {
		fmt.Fprintf(os.Stderr, "\nSkipped:\n")
		skipped := make([]string, 0, len(rep.Skipped))
		for sym := range rep.Skipped {
			skipped = append(skipped, sym)
		}
		sort.Strings(skipped)
		for _, sym := range skipped {
			fmt.Fprintf(os.Stderr, "  %-8s %s\n", sym, rep.Skipped[sym])
		}
	}
	if *out != "" {
		fmt.Fprintf(os.Stderr, "\nTo use it, copy to $PYRITE_DATA_DIR/shares_outstanding.csv,\n"+
			"or replace internal/market/assets/shares_outstanding.csv and rebuild.\n")
	}
	return nil
}

// printCritique reports what is wrong with the result just printed.
func printCritique(res *engine.Result) {
	c := res.Critique
	if len(c.Findings) == 0 {
		return
	}
	fmt.Printf("\nHow much should you believe this?  %d/100\n", c.TrustScore)
	for _, f := range c.Findings {
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
}

// cmdIngestIndex rebuilds the point-in-time index membership table.
func cmdIngestIndex(args []string) error {
	fs := flag.NewFlagSet("ingest index", flag.ContinueOnError)
	index := fs.String("index", "sp500", "which index to rebuild")
	out := fs.String("out", "", "write here instead of stdout")
	agent := fs.String("user-agent", os.Getenv("PYRITE_WIKI_USER_AGENT"),
		"identify yourself to Wikipedia (optional but polite)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *index != "sp500" {
		return fmt.Errorf("only \"sp500\" is supported so far, not %q", *index)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	w := market.NewWikipediaIndex(*agent)
	fmt.Fprintf(os.Stderr, "fetching current constituents...\n")
	current, err := w.CurrentMembers(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "fetching the change log...\n")
	changes, err := w.Changes(ctx)
	if err != nil {
		return err
	}

	tenures := market.BuildMembership(current, changes)

	var buf bytes.Buffer
	if err := market.WriteMembershipCSV(&buf, *index, tenures, len(changes)); err != nil {
		return err
	}
	if *out == "" {
		fmt.Print(buf.String())
	} else {
		if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", *out)
	}

	// Count how many names the table holds that are no longer members: those
	// are exactly the ones survivorship bias would have removed.
	var dropped int
	for _, t := range tenures {
		if t.To != "" {
			dropped++
		}
	}
	fmt.Fprintf(os.Stderr, "\n%d current constituents, %d recorded changes\n", len(current), len(changes))
	fmt.Fprintf(os.Stderr, "%d tenures, of which %d have ended — those are the names a\n", len(tenures), dropped)
	fmt.Fprintf(os.Stderr, "survivorship-biased universe silently drops.\n")
	return nil
}

// cmdExamples lists the strategies bundled into the binary.
func cmdExamples(args []string) error {
	fs := flag.NewFlagSet("examples", flag.ContinueOnError)
	show := fs.String("show", "", "print one example's source instead of listing")
	asJSON := fs.Bool("json", false, "print the list as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// `pyrite examples golden-cross` should do the obvious thing.
	if *show == "" && len(fs.Args()) > 0 {
		*show = fs.Arg(0)
	}

	if *show != "" {
		ex, err := examples.Get(*show)
		if err != nil {
			return err
		}
		fmt.Print(ex.Code)
		return nil
	}

	all := examples.All()
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(all)
	}

	fmt.Printf("Bundled strategies. Each one runs with no API key and no setup:\n\n")
	const indent = "      "
	for _, ex := range all {
		fmt.Printf("  %s\n", ex.Name)
		fmt.Printf("%s%s\n", indent, wrapIndent(ex.Title, 70, indent))
		if ex.NeedsModel {
			fmt.Printf("%s(needs a model: it calls one inside the backtest)\n", indent)
		}
		fmt.Println()
	}
	fmt.Printf("Run one          pyrite run --example golden-cross\n")
	fmt.Printf("Read the code    pyrite examples golden-cross\n")
	fmt.Printf("Search its space pyrite sweep --example golden-cross\n")
	fmt.Printf("Full report      pyrite report --example golden-cross --html report.html\n")
	return nil
}
