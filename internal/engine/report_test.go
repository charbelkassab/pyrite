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

	return &Report{
		Title: "Test strategy", Prompt: "a moving average cross",
		Generated: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Run:       run, Sweep: sw, WalkForward: wf, Costs: costs,
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
		"## How much survives friction",
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
		"## One path is not the distribution", "How much of this is the search?"} {
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
