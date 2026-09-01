package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

// reportFor builds a report the way the CLI does, so the test exercises the
// same assembly a user gets.
func reportFor(t *testing.T) *Report {
	t.Helper()
	store := newTestStore(t)
	spec := sweepSpec()
	spec.Start, spec.End = "2016-01-05", "2023-12-29"
	spec.Benchmarks = []string{"SPY"}

	run, err := New(spec, store).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	sw, err := RunSweep(context.Background(), SweepSpec{Base: spec, KeepBest: 1}, store, nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	wf, err := RunWalkForward(context.Background(), WalkForwardSpec{
		Base: spec, TrainDays: 300, TestDays: 150,
	}, store, nil)
	if err != nil {
		t.Fatalf("walk-forward: %v", err)
	}
	costs, err := RunCostScan(context.Background(), spec, store, nil)
	if err != nil {
		t.Fatalf("cost scan: %v", err)
	}
	factors, err := AnalyseFactors(context.Background(), run.Curve, store,
		spec.Interval, ScaleFor(spec.Interval, spec.RiskFreeRate), nil)
	if err != nil {
		t.Fatalf("factors: %v", err)
	}

	return &Report{
		Title: "Test strategy", Prompt: "a moving average cross",
		Generated: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Run:       run, Sweep: sw, WalkForward: wf, Costs: costs,
		Factors:     factors,
		Bootstrap:   Bootstrap(run.Curve, 500, 21, 42),
		Assumptions: []string{"trades at the next open"},
		Limitations: []string{"no intraday stops"},
	}
}

func TestReportContainsEverySection(t *testing.T) {
	doc := reportFor(t).Markdown()
	for _, want := range []string{
		"# Test strategy",
		"## Verdict",
		"How much should you believe this",
		"## Results,",
		"## Out of sample",
		"never fitted to",
		"## How much of this is the search?",
		"Expected best from luck alone",
		"## Where the return came from",
		"## Which rules made the money",
		"## How much survives friction",
		"## Is any of this alpha?",
		"Alpha, annualised",
		"Newey-West standard",
		"ETF spreads, not the Fama-French",
		"## One path is not the distribution",
		"## Objections",
		"## How this was produced",
		"## The strategy",
		"```javascript",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the report is missing %q", want)
		}
	}
	// It must stand on its own without a model.
	if strings.Contains(doc, "%!") {
		t.Error("a formatting verb went unfilled")
	}
	if strings.Contains(doc, "NaN") || strings.Contains(doc, "+Inf") {
		t.Error("a non-finite number reached the document")
	}
}

func TestReportLeadsWithTheVerdict(t *testing.T) {
	doc := reportFor(t).Markdown()
	verdict := strings.Index(doc, "## Verdict")
	results := strings.Index(doc, "## Results,")
	code := strings.Index(doc, "## The strategy")
	if !(verdict > 0 && verdict < results && results < code) {
		t.Errorf("sections are out of order: verdict %d, results %d, code %d",
			verdict, results, code)
	}
	// A reader who stops after the first section should have the conclusion.
	head := doc[:results]
	if !strings.Contains(head, "believe this") {
		t.Error("the trust score should appear before the numbers it qualifies")
	}
}

func TestReportRendersWithoutOptionalSections(t *testing.T) {
	// A report with only a backtest must still be a coherent document.
	store := newTestStore(t)
	spec := sweepSpec()
	spec.Start, spec.End = "2020-01-06", "2021-12-31"
	run, err := New(spec, store).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	rep := &Report{Title: "Minimal", Run: run, Generated: time.Now()}
	doc := rep.Markdown()

	if !strings.Contains(doc, "# Minimal") || !strings.Contains(doc, "## Results,") {
		t.Fatalf("the minimal report is not coherent:\n%s", doc)
	}
	for _, absent := range []string{"## Out of sample", "## How much survives friction",
		"## Is any of this alpha?", "## One path is not the distribution",
		"How much of this is the search?"} {
		if strings.Contains(doc, absent) {
			t.Errorf("%q should be omitted when its analysis was not run", absent)
		}
	}
}

func TestReportHandlesAnEmptyRun(t *testing.T) {
	rep := &Report{Title: "Nothing", Generated: time.Now()}
	doc := rep.Markdown()
	if !strings.Contains(doc, "# Nothing") {
		t.Error("even an empty report should render its title")
	}
}

func TestReportNamesCloseFillsAsOptimistic(t *testing.T) {
	if got := fillText(FillClose); !strings.Contains(got, "already seen") {
		t.Errorf("close fills should be labelled: %q", got)
	}
	if got := fillText(FillNextOpen); !strings.Contains(got, "next session") {
		t.Errorf("next-open fills should be described: %q", got)
	}
}

func TestPluralIsGrammatical(t *testing.T) {
	if got := plural(1, "symbol"); got != "1 symbol" {
		t.Errorf("got %q", got)
	}
	if got := plural(43, "symbol"); got != "43 symbols" {
		t.Errorf("got %q", got)
	}
	if got := plural(0, "symbol"); got != "0 symbols" {
		t.Errorf("got %q", got)
	}
}

func TestReportAttributesPnLToTheRules(t *testing.T) {
	spec := baseSpec(`
		function onDay(ctx) {
			const f = ctx.sma("AAPL", 10), s = ctx.sma("AAPL", 50);
			if (f === null || s === null) return;
			if (f > s && !ctx.hasPosition("AAPL")) {
				ctx.buy("AAPL", { pctCash: 0.95 }, "10 day crossed above 50 day");
			} else if (f < s && ctx.hasPosition("AAPL")) {
				ctx.close("AAPL", "the trend broke");
			}
		}
	`)
	spec.Universe = []string{"AAPL"}
	spec.Start, spec.End = "2016-01-05", "2023-12-29"
	run, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	doc := (&Report{Title: "Rules", Run: run, Generated: time.Now()}).Markdown()

	for _, want := range []string{
		"## Which rules made the money",
		"Reasons are grouped",
		"| Entry rule | Net P&L |",
		"| Exit rule | Net P&L |",
		"10 day crossed above 50 day",
		"the trend broke",
		"both sum to",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the rule section is missing %q", want)
		}
	}
	if strings.Contains(doc, "%!") {
		t.Error("a formatting verb went unfilled")
	}
}

func TestReportSaysWhenNoOrderGaveAReason(t *testing.T) {
	// ctx.buy() and ctx.sell() take a reason and default to none, so a
	// strategy can be entirely silent about itself. The section then has to
	// explain its own emptiness rather than print two rows of the bucket.
	spec := baseSpec(`
		function onDay(ctx) {
			const f = ctx.sma("AAPL", 10), s = ctx.sma("AAPL", 50);
			if (f === null || s === null) return;
			if (f > s && !ctx.hasPosition("AAPL")) ctx.buy("AAPL", { pctCash: 0.95 });
			else if (f < s && ctx.hasPosition("AAPL")) ctx.sell("AAPL", { all: true });
		}
	`)
	spec.Universe = []string{"AAPL"}
	spec.Start, spec.End = "2016-01-05", "2023-12-29"
	run, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	doc := (&Report{Title: "Silent", Run: run, Generated: time.Now()}).Markdown()
	if !strings.Contains(doc, "No order in this run gave a reason") {
		t.Errorf("an unattributed run needs the explanation:\n%s", doc)
	}
	if strings.Contains(doc, NoReasonLabel) {
		t.Error("a table of one empty bucket is not worth printing")
	}
}
