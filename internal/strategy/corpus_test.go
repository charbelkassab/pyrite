package strategy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charbelkassab/pyrite/internal/app"
	"github.com/charbelkassab/pyrite/internal/config"
	"github.com/charbelkassab/pyrite/internal/market"
	"github.com/charbelkassab/pyrite/internal/strategy"
)

// The corpus test compiles and runs a broad set of natural language strategy
// prompts against live models and live market data. It is the regression
// suite for prompt coverage: when someone reports "pyrite could not
// handle X", X belongs in testdata/corpus.json.
//
// It costs real money and needs network access, so it only runs when asked:
//
//	PYRITE_LIVE_TESTS=1 go test ./internal/strategy/ -run TestPromptCorpus -v -timeout 45m
//
// Set PYRITE_CORPUS_FILTER to a family or id substring to run a subset.

type corpusCase struct {
	ID     string `json:"id"`
	Family string `json:"family"`
	Prompt string `json:"prompt"`
	Expect struct {
		Trades         bool `json:"trades"`
		PositionsAtEnd int  `json:"positions_at_end"`
		AllowShort     bool `json:"allow_short"`
		NeedsAI        bool `json:"needs_ai"`
	} `json:"expect"`
}

type corpusOutcome struct {
	Case       corpusCase
	Compiled   bool
	Ran        bool
	Attempts   int
	Provider   string
	Model      string
	CompileMS  int64
	RunMS      int64
	Trades     int
	Return     float64
	Positions  int
	AllowShort bool
	Failure    string
	Warnings   []string
	Code       string
}

func TestPromptCorpus(t *testing.T) {
	if os.Getenv("PYRITE_LIVE_TESTS") != "1" {
		t.Skip("set PYRITE_LIVE_TESTS=1 to run the live prompt corpus (uses real API calls)")
	}

	raw, err := os.ReadFile("testdata/corpus.json")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var cases []corpusCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}

	if filter := os.Getenv("PYRITE_CORPUS_FILTER"); filter != "" {
		var kept []corpusCase
		for _, c := range cases {
			if strings.Contains(c.ID, filter) || strings.Contains(c.Family, filter) {
				kept = append(kept, c)
			}
		}
		cases = kept
	}
	if len(cases) == 0 {
		t.Fatal("no cases selected")
	}

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if !cfg.AnyProviderEnabled() {
		t.Skip("no model API key configured")
	}
	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app: %v", err)
	}

	// A fixed window keeps results comparable between corpus runs and lets
	// the market data cache serve every case after the first pass.
	start := market.Day("2021-01-04")
	end := market.Day("2024-12-31")

	outcomes := make([]corpusOutcome, len(cases))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // bounded: providers rate limit

	for i, c := range cases {
		wg.Add(1)
		go func(i int, c corpusCase) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outcomes[i] = runCorpusCase(a, c, start, end)
		}(i, c)
	}
	wg.Wait()

	report := buildCorpusReport(outcomes)
	t.Log("\n" + report)
	if path := os.Getenv("PYRITE_CORPUS_REPORT"); path != "" {
		if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
			t.Logf("could not write report: %v", err)
		}
	}

	var failed []string
	for _, o := range outcomes {
		if o.Failure != "" {
			failed = append(failed, fmt.Sprintf("%s: %s", o.Case.ID, o.Failure))
		}
	}
	if len(failed) > 0 {
		t.Errorf("%d of %d prompts failed:\n  %s",
			len(failed), len(outcomes), strings.Join(failed, "\n  "))
	}
}

func runCorpusCase(a *app.App, c corpusCase, start, end market.Day) corpusOutcome {
	out := corpusOutcome{Case: c}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	t0 := time.Now()
	plan, err := a.Compiler.Compile(ctx, strategy.Request{
		Prompt: c.Prompt, Start: start, End: end,
	})
	out.CompileMS = time.Since(t0).Milliseconds()
	if err != nil {
		out.Failure = "compile: " + oneLine(err.Error())
		return out
	}
	out.Compiled = true
	out.Attempts, out.Provider, out.Model = plan.Attempts, plan.Provider, plan.Model
	out.AllowShort = plan.AllowShort
	out.Code = plan.Code

	opts := app.RunOptions{Start: start, End: end, InitialCash: 100000}
	opts.ApplyDefaults()
	spec := app.BuildSpec(plan, c.Prompt, opts)

	t1 := time.Now()
	res, err := a.Backtest(ctx, spec, opts)
	out.RunMS = time.Since(t1).Milliseconds()
	if err != nil {
		out.Failure = "run: " + oneLine(err.Error())
		return out
	}
	out.Ran = true
	out.Trades = res.Metrics.TotalTrades
	out.Return = res.Metrics.TotalReturn
	out.Warnings = res.Warnings
	if n := len(res.Days); n > 0 {
		out.Positions = len(res.Days[n-1].Positions)
	}

	// A strategy that throws on most days is broken even if it completes.
	if res.StrategyErrors > len(res.Days)/10 && res.StrategyErrors > 3 {
		for _, d := range res.Days {
			if d.Error != "" {
				out.Failure = fmt.Sprintf("threw on %d days, first: %s",
					res.StrategyErrors, oneLine(d.Error))
				return out
			}
		}
	}

	// Expectation checks.
	if c.Expect.Trades && res.Metrics.TotalTrades == 0 && len(res.Fills) == 0 {
		out.Failure = "expected the strategy to trade, but it never placed a fill"
		return out
	}
	if c.Expect.AllowShort && !plan.AllowShort {
		out.Failure = "expected allow_short to be set for a strategy that shorts"
		return out
	}
	if c.Expect.NeedsAI && res.AICallCount == 0 {
		out.Failure = "expected the strategy to call ctx.ai() or ctx.news(), but it made no calls"
		return out
	}
	if n := c.Expect.PositionsAtEnd; n > 0 && out.Positions != n {
		out.Failure = fmt.Sprintf("expected %d positions at the end, found %d", n, out.Positions)
		return out
	}
	return out
}

func buildCorpusReport(outcomes []corpusOutcome) string {
	var b strings.Builder
	byFamily := map[string][]corpusOutcome{}
	for _, o := range outcomes {
		byFamily[o.Case.Family] = append(byFamily[o.Case.Family], o)
	}
	families := make([]string, 0, len(byFamily))
	for f := range byFamily {
		families = append(families, f)
	}
	sort.Strings(families)

	var pass, fail int
	fmt.Fprintf(&b, "%-28s %-8s %5s %7s %7s %7s  %s\n",
		"CASE", "STATUS", "TRY", "COMP_S", "RUN_S", "TRADES", "DETAIL")
	fmt.Fprintln(&b, strings.Repeat("-", 110))

	for _, f := range families {
		fmt.Fprintf(&b, "\n[%s]\n", f)
		for _, o := range byFamily[f] {
			status := "ok"
			detail := fmt.Sprintf("return %+.1f%%", o.Return*100)
			if o.Failure != "" {
				status = "FAIL"
				detail = o.Failure
				fail++
			} else {
				pass++
			}
			fmt.Fprintf(&b, "%-28s %-8s %5d %7.1f %7.1f %7d  %s\n",
				truncateStr(o.Case.ID, 28), status, o.Attempts,
				float64(o.CompileMS)/1000, float64(o.RunMS)/1000,
				o.Trades, truncateStr(detail, 60))
		}
	}
	fmt.Fprintf(&b, "\n%d passed, %d failed, %d total\n", pass, fail, len(outcomes))
	return b.String()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
