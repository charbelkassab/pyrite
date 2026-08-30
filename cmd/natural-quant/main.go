// Command natural-quant runs the natural-quant web application and CLI.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/charbelkassab/natural-quant/internal/app"
	"github.com/charbelkassab/natural-quant/internal/config"
	"github.com/charbelkassab/natural-quant/internal/engine"
	"github.com/charbelkassab/natural-quant/internal/market"
	"github.com/charbelkassab/natural-quant/internal/server"
	"github.com/charbelkassab/natural-quant/internal/strategy"
)

// version is overridden at build time with -ldflags "-X main.version=..."
var version = "dev"

const usage = `natural-quant — describe a trading strategy in plain language, then backtest it.

Usage:
  natural-quant serve [flags]            start the web app (default)
  natural-quant run "<strategy>" [flags] run one backtest and print the result
  natural-quant doctor                   check data, model providers and caches
  natural-quant api                      print the strategy API reference
  natural-quant cache clear [--ai]       clear cached market data and replies
  natural-quant sweep "<strategy>" [--param fast=10,20,50] [--objective sharpe]
                                   [--csv out.csv] [--top 20]
  natural-quant walkforward "<strategy>" [--train 504] [--test 126]
                                         [--embargo 200] [--anchored]
  natural-quant ingest edgar [--symbols A,B] [--universe megacap] [--out FILE]
  natural-quant version

Common flags:
  --addr        host:port for the web server            (default 127.0.0.1:8080)
  --from        backtest start date, YYYY-MM-DD         (default 5 years ago)
  --to          backtest end date, YYYY-MM-DD           (default today)
  --cash        starting capital                        (default 100000)
  --benchmark   comma separated comparison symbols      (default SPY)
  --universe    override the tradable symbols
  --offline     use synthetic data, no network, no keys
  --json        print machine readable output

Model provider keys are read from the environment:
  OPENAI_API_KEY, CEREBRAS_API_KEY, KIMI_API_KEY (or MOONSHOT_API_KEY)

Examples:
  natural-quant serve
  natural-quant run "buy $100 of the biggest company by market cap each day, sell when it is no longer number one"
  natural-quant run "golden cross on SPY with a 10% trailing stop" --from 2015-01-01
`

func main() {
	// The engine stamps every result with the build identity, so a saved run
	// records which binary produced it.
	engine.Version = version
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "serve":
		return cmdServe(args)
	case "run":
		return cmdRun(args)
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
	case "version", "-v", "--version":
		fmt.Printf("natural-quant %s\n", version)
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
	cfg, err := config.Load(os.Getenv("NQ_CONFIG"))
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

	fmt.Printf("natural-quant %s\n", version)
	fmt.Printf("  data       %s\n", a.Store.ProviderName())
	fmt.Printf("  models     %s\n", a.DescribeRoutes())
	fmt.Printf("  cache      %s\n", a.Cfg.DataDir)
	if !a.Cfg.AnyProviderEnabled() {
		fmt.Printf("\n  No model API key found, so strategies cannot be compiled from plain language.\n")
		fmt.Printf("  Set OPENAI_API_KEY, CEREBRAS_API_KEY or KIMI_API_KEY and restart.\n")
		fmt.Printf("  You can still run the bundled example strategies.\n")
	}
	fmt.Printf("\n  ready on http://%s\n\n", a.Cfg.Addr)

	if *open {
		go openBrowser("http://" + a.Cfg.Addr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.ListenAndServe(ctx, a.Cfg.Addr)
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
	warmupFlag := fs.Int("warmup", 0, "bars of history to load before the start date")
	// Separate the prompt from the flags before parsing. Go's flag package
	// stops at the first positional argument, so without this a command like
	//   natural-quant run "buy SPY" --from 2020-01-01
	// would silently ignore every flag and fold them into the prompt.
	prompt, flagArgs := splitPromptAndFlags(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	// Anything still positional belongs to the prompt too.
	prompt = strings.TrimSpace(strings.Join(append([]string{prompt}, fs.Args()...), " "))
	if prompt == "" && *codeFile == "" {
		return fmt.Errorf("describe a strategy, for example:\n" +
			"  natural-quant run \"buy SPY when the 50 day average crosses above the 200 day\"\n" +
			"  or pass --code-file strategy.js to run code you already have")
	}

	a, err := newApp(fs, offline)
	if err != nil {
		return err
	}
	if *codeFile == "" && !a.Cfg.AnyProviderEnabled() {
		return fmt.Errorf("no model API key found. Set OPENAI_API_KEY, CEREBRAS_API_KEY or KIMI_API_KEY,\n" +
			"  or pass --code-file to run a strategy you already have")
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
		opts.Universe = market.ResolveUniverse(*universe)
	}
	if *fillClose {
		opts.Fill = engine.FillClose
	}
	opts.ApplyDefaults()

	var plan *strategy.Plan
	if *codeFile != "" {
		code, err := os.ReadFile(*codeFile)
		if err != nil {
			return err
		}
		plan = &strategy.Plan{Name: filepath.Base(*codeFile), Code: string(code)}
	} else {
		fmt.Fprintf(os.Stderr, "compiling strategy with %s...\n", a.DescribeRoutes())
		started := time.Now()
		plan, err = a.Compiler.Compile(ctx, strategy.Request{
			Prompt:   prompt,
			Universe: opts.Universe,
			Start:    opts.Start,
			End:      opts.End,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "compiled in %s using %s/%s (attempt %d)\n\n",
			time.Since(started).Round(time.Millisecond), plan.Provider, plan.Model, plan.Attempts)
	}

	if *showCode {
		fmt.Println(plan.Code)
		fmt.Println()
	}

	spec := app.BuildSpec(plan, prompt, opts)
	if *warmupFlag > 0 {
		spec.Warmup = *warmupFlag
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

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"plan": plan, "result": res})
	}
	printReport(plan, res)
	return nil
}

// printReport renders a human-readable summary to stdout.
func printReport(plan *strategy.Plan, res *engine.Result) {
	m := res.Metrics
	fmt.Printf("%s\n", plan.Name)
	if plan.Description != "" {
		fmt.Printf("%s\n", wrap(plan.Description, 76))
	}
	fmt.Printf("\n%s to %s   %d trading days   universe of %d\n\n",
		res.Spec.Start, res.Spec.End, m.TradingDays, len(res.Spec.Universe))

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
	printRisk(res)
	printByYear(res)
	printRegimes(res)
	printStress(res)
	printSymbols(res)
	printCritique(res)

	if len(res.Benchmarks) > 0 {
		fmt.Printf("\n  %-22s %14s %14s\n", "Comparison", "Total return", "Max drawdown")
		fmt.Printf("  %-22s %14s %14s\n", plan.Name, pct(m.TotalReturn), pct(m.MaxDrawdown))
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

	fmt.Printf("natural-quant %s\n\n", version)
	fmt.Printf("data provider      %s\n", h.DataProvider)
	fmt.Printf("offline mode       %v\n", h.OfflineMode)
	fmt.Printf("web search         %v\n", h.SearchOK)
	fmt.Printf("fundamentals       %s\n", h.Fundamentals)
	fmt.Printf("data directory     %s\n", h.DataDir)
	fmt.Printf("cached symbols     %d (%.1f MB)\n", h.CachedSymbols, float64(h.CacheBytes)/(1<<20))
	fmt.Printf("\nmodel providers\n")
	for _, p := range h.Providers {
		status := "no API key"
		switch {
		case p.Reachable != nil && *p.Reachable:
			status = fmt.Sprintf("ok, %d models available", len(p.Models))
		case p.Reachable != nil:
			status = "unreachable: " + p.Error
		case p.Enabled:
			status = "key present"
		}
		fmt.Printf("  %-10s %-28s %s\n", p.Name, p.Model, status)
	}
	fmt.Printf("\nrouting            %s\n", a.DescribeRoutes())
	if !a.Cfg.AnyProviderEnabled() {
		fmt.Printf("\nNo model API key is configured. Set one of OPENAI_API_KEY,\nCEREBRAS_API_KEY or KIMI_API_KEY to compile strategies.\n")
	}
	return nil
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

func pct(v float64) string { return fmt.Sprintf("%.2f%%", v*100) }

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

// cmdIngest builds reference data tables from public sources.
func cmdIngest(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: natural-quant ingest edgar [flags]")
	}
	switch args[0] {
	case "edgar":
		return cmdIngestEDGAR(args[1:])
	default:
		return fmt.Errorf("unknown ingest source %q (only \"edgar\" is available)", args[0])
	}
}

// cmdIngestEDGAR rebuilds the point-in-time share-count table from SEC filings.
func cmdIngestEDGAR(args []string) error {
	fs := flag.NewFlagSet("ingest edgar", flag.ContinueOnError)
	symbols := fs.String("symbols", "", "comma-separated tickers to ingest")
	universe := fs.String("universe", "", "a named universe to ingest (megacap, tech, dow, ...)")
	out := fs.String("out", "", "write here instead of stdout")
	agent := fs.String("user-agent", os.Getenv("NQ_SEC_USER_AGENT"),
		"identify yourself to the SEC, e.g. \"Jane Doe jane@example.com\" (or set NQ_SEC_USER_AGENT)")
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
			"  Pass --user-agent \"Your Name you@example.com\" or set NQ_SEC_USER_AGENT.\n" +
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
		fmt.Fprintf(os.Stderr, "\nTo use it, copy to $NQ_DATA_DIR/shares_outstanding.csv,\n"+
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
