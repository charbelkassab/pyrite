// Command natural-quant runs the natural-quant web application and CLI.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
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
	if prompt == "" {
		return fmt.Errorf("describe a strategy, for example:\n  natural-quant run \"buy SPY when the 50 day average crosses above the 200 day\"")
	}

	a, err := newApp(fs, offline)
	if err != nil {
		return err
	}
	if !a.Cfg.AnyProviderEnabled() {
		return fmt.Errorf("no model API key found. Set OPENAI_API_KEY, CEREBRAS_API_KEY or KIMI_API_KEY")
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

	fmt.Fprintf(os.Stderr, "compiling strategy with %s...\n", a.DescribeRoutes())
	started := time.Now()
	plan, err := a.Compiler.Compile(ctx, strategy.Request{
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

	if *showCode {
		fmt.Println(plan.Code)
		fmt.Println()
	}

	spec := app.BuildSpec(plan, prompt, opts)
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
