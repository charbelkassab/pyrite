package engine

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/charbelkassab/pyrite/internal/market"
)

// scenarioSeries builds a deterministic weekday series between two dates.
// price is asked for the close on each session, so a test can describe a
// crash as a function of the date rather than as a list of bars.
func scenarioSeries(sym string, from, to market.Day, price func(d market.Day) float64) *market.Series {
	var bars []market.Bar
	for d := from; d <= to; d = d.Add(1) {
		if wd := d.Time().Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		p := price(d)
		bars = append(bars, market.Bar{
			Date: d, Open: p, High: p, Low: p, Close: p, AdjClose: p, Volume: 1e6,
		})
	}
	return market.NewSeries(sym, bars)
}

// flatThen holds a price at 100 until start, then declines geometrically to
// end*100 by the last day of the fall, then holds.
func flatThen(start, finish market.Day, endLevel float64) func(market.Day) float64 {
	days := int(finish.Time().Sub(start.Time()).Hours() / 24)
	if days <= 0 {
		days = 1
	}
	return func(d market.Day) float64 {
		switch {
		case d <= start:
			return 100
		case d >= finish:
			return 100 * endLevel
		}
		elapsed := float64(int(d.Time().Sub(start.Time()).Hours()/24)) / float64(days)
		return 100 * math.Pow(endLevel, elapsed)
	}
}

func scenarioStore(t *testing.T, series map[string]*market.Series) *market.Store {
	t.Helper()
	return market.NewStore(&fixedProvider{series: series}, nil, mustFundamentals(t))
}

// The dates in the table are the tool's own claim about history, and a wrong
// one would be invisible in every other test in this file.
func TestScenarioTableIsInternallyConsistent(t *testing.T) {
	list := Scenarios()
	if len(list) < 13 {
		t.Fatalf("expected at least 13 named windows, got %d", len(list))
	}
	seen := map[string]bool{}
	var prevEnd market.Day
	var prevName string
	for _, sc := range list {
		if _, err := market.ParseDay(string(sc.Start)); err != nil {
			t.Errorf("%s: bad start %q: %v", sc.Name, sc.Start, err)
		}
		if _, err := market.ParseDay(string(sc.End)); err != nil {
			t.Errorf("%s: bad end %q: %v", sc.Name, sc.End, err)
		}
		if sc.Start >= sc.End {
			t.Errorf("%s: start %s is not before end %s", sc.Name, sc.Start, sc.End)
		}
		if seen[sc.Name] {
			t.Errorf("duplicate scenario name %q", sc.Name)
		}
		seen[sc.Name] = true
		if n := len(sc.Name); n == 0 || n > 20 {
			t.Errorf("%q is %d characters; the printed table allows 20", sc.Name, n)
		}
		if strings.TrimSpace(sc.Description) == "" {
			t.Errorf("%s has no description", sc.Name)
		}
		if sc.IndexDrawdown > 0 || sc.IndexDrawdown < -1 {
			t.Errorf("%s: index drawdown %.3f is not a fraction between -1 and 0",
				sc.Name, sc.IndexDrawdown)
		}
		if prevEnd != "" && sc.Start <= prevEnd {
			t.Errorf("%s (%s) overlaps %s, which runs to %s",
				sc.Name, sc.Start, prevName, prevEnd)
		}
		prevEnd, prevName = sc.End, sc.Name
	}
}

// A window the data cannot reach must be reported as unmeasured. Dropping the
// row would present partial coverage as full coverage, and reporting a zero
// return would claim the strategy sat out the financial crisis unharmed.
func TestScenarioOutsideTheDataIsSkippedNotZero(t *testing.T) {
	store := scenarioStore(t, map[string]*market.Series{
		"SPY": scenarioSeries("SPY", "2015-01-01", "2021-12-31", func(market.Day) float64 { return 100 }),
	})
	spec := Spec{
		Name: "hold", Universe: []string{"SPY"}, Benchmarks: []string{"SPY"},
		InitialCash: 100000, AllowFractional: true, Warmup: 5, Costs: Costs{},
		Code: `function onDay(ctx) { if (!ctx.hasPosition("SPY")) ctx.buy("SPY", { pctCash: 0.99 }); }`,
	}

	rep, err := RunScenarios(context.Background(), ScenarioSpec{Base: spec}, store, nil)
	if err != nil {
		t.Fatalf("run scenarios: %v", err)
	}
	if len(rep.Runs) != len(Scenarios()) {
		t.Fatalf("every window must keep its row: got %d of %d", len(rep.Runs), len(Scenarios()))
	}

	byName := map[string]ScenarioRun{}
	for _, r := range rep.Runs {
		byName[r.Name] = r
	}
	for _, name := range []string{"Black Monday", "Dot-com collapse", "Financial crisis"} {
		r, ok := byName[name]
		if !ok {
			t.Fatalf("%s is missing from the report entirely", name)
		}
		if !r.Skipped {
			t.Errorf("%s should be skipped: the data starts in 2015", name)
		}
		if r.SkipReason == "" {
			t.Errorf("%s was skipped without saying why", name)
		}
		if r.Return.Defined() || r.MaxDrawdown.Defined() || r.BenchmarkReturn.Defined() {
			t.Errorf("%s reported numbers for a window it never ran: return %v, benchmark %v",
				name, float64(r.Return), float64(r.BenchmarkReturn))
		}
		if r.Measured() {
			t.Errorf("%s counts as measured", name)
		}
	}
	// The far end of the data is the same failure in the other direction.
	if r := byName["Rate shock"]; !r.Skipped || !strings.Contains(r.SkipReason, "2021-12-31") {
		t.Errorf("a window past the end of the data should say so, got skipped=%v reason=%q",
			r.Skipped, r.SkipReason)
	}
	if rep.Covered+rep.Skipped != len(rep.Runs) {
		t.Errorf("covered %d plus skipped %d does not account for %d rows",
			rep.Covered, rep.Skipped, len(rep.Runs))
	}
	if rep.Covered == 0 {
		t.Fatal("2015 to 2021 covers several windows; none were measured")
	}
	var coverage bool
	for _, f := range rep.Findings {
		if strings.Contains(f.Title, "history") {
			coverage = true
		}
	}
	if !coverage {
		t.Errorf("skipping %d windows should produce a coverage finding, got %v",
			rep.Skipped, rep.Findings)
	}
}

// The flash crash is fifteen sessions long. A strategy with a 200 bar average
// can only trade inside it if the history in front of the window is loaded and
// run, and if it is not, every short window reports a flat line that reads as
// a defensive result and is really an empty one.
func TestWarmupIsHonouredInAShortWindow(t *testing.T) {
	flash := Scenarios()[3]
	if flash.Name != "Flash crash" {
		t.Fatalf("expected the flash crash at index 3, got %s", flash.Name)
	}
	store := scenarioStore(t, map[string]*market.Series{
		"SPY": scenarioSeries("SPY", "2006-01-02", "2010-12-31", func(d market.Day) float64 {
			// A slow rise so the 200 day average is defined and below price.
			days := d.Time().Sub(market.Day("2006-01-02").Time()).Hours() / 24
			return 100 * math.Pow(1.0002, days)
		}),
	})
	spec := Spec{
		Name: "slow average", Universe: []string{"SPY"}, Benchmarks: []string{"SPY"},
		InitialCash: 100000, AllowFractional: true, Warmup: 200, Costs: Costs{},
		Code: `
			function onDay(ctx) {
				var sma = ctx.sma("SPY", 200);
				if (sma === null) { ctx.log("no average yet"); return; }
				if (!ctx.hasPosition("SPY")) ctx.buy("SPY", { pctCash: 0.99 }, "average is valid");
			}
		`,
	}

	rep, err := RunScenarios(context.Background(),
		ScenarioSpec{Base: spec, Scenarios: []Scenario{flash}}, store, nil)
	if err != nil {
		t.Fatalf("run scenarios: %v", err)
	}
	r := rep.Runs[0]
	if !r.Measured() {
		t.Fatalf("the flash crash should be measurable with four years of history: %s%s",
			r.SkipReason, r.Error)
	}
	if r.Sessions < 10 {
		t.Errorf("expected the full fifteen session window, measured %d", r.Sessions)
	}
	if float64(r.Exposure) < 0.5 {
		t.Errorf("the strategy should hold a position through the window, exposure was %.2f",
			float64(r.Exposure))
	}
	if r.LeadInFrom >= flash.Start {
		t.Errorf("trading should begin before the window: lead-in %s, window opens %s",
			r.LeadInFrom, flash.Start)
	}
}

// The same strategy with barely any history must be told it cannot be
// measured, rather than reporting the flat line that produces.
func TestShortHistoryIsSkippedRatherThanRunFlat(t *testing.T) {
	flash := Scenarios()[3]
	store := scenarioStore(t, map[string]*market.Series{
		"SPY": scenarioSeries("SPY", "2010-01-04", "2010-12-31", func(market.Day) float64 { return 100 }),
	})
	spec := Spec{
		Name: "slow average", Universe: []string{"SPY"}, Benchmarks: []string{"SPY"},
		InitialCash: 100000, AllowFractional: true, Warmup: 200, Costs: Costs{},
		Code: `
			function onDay(ctx) {
				if (ctx.sma("SPY", 200) === null) return;
				if (!ctx.hasPosition("SPY")) ctx.buy("SPY", { pctCash: 0.99 });
			}
		`,
	}

	rep, err := RunScenarios(context.Background(),
		ScenarioSpec{Base: spec, Scenarios: []Scenario{flash}}, store, nil)
	if err != nil {
		t.Fatalf("run scenarios: %v", err)
	}
	r := rep.Runs[0]
	if !r.Skipped {
		t.Fatalf("87 sessions cannot warm up a 200 bar average and still enter the window "+
			"positioned; the row was measured instead: return %v", float64(r.Return))
	}
	if !strings.Contains(r.SkipReason, "warm up") {
		t.Errorf("the reason should name the warm-up, got %q", r.SkipReason)
	}
}

// Losing far more than the benchmark in a named crisis is exactly the thing
// this command exists to surface, so it has to reach the findings.
func TestLosingMoreThanTheBenchmarkIsAFinding(t *testing.T) {
	var covid Scenario
	for _, sc := range Scenarios() {
		if sc.Name == "COVID crash" {
			covid = sc
		}
	}
	if covid.Name == "" {
		t.Fatal("the COVID crash is missing from the table")
	}
	store := scenarioStore(t, map[string]*market.Series{
		// Halves over the window; the benchmark gives up a fifth.
		"FALL":  scenarioSeries("FALL", "2018-01-01", "2020-04-30", flatThen(covid.Start, covid.End, 0.5)),
		"BENCH": scenarioSeries("BENCH", "2018-01-01", "2020-04-30", flatThen(covid.Start, covid.End, 0.8)),
	})
	spec := Spec{
		Name: "long the wrong thing", Universe: []string{"FALL"}, Benchmarks: []string{"BENCH"},
		InitialCash: 100000, AllowFractional: true, Warmup: 5, Costs: Costs{},
		Code: `function onDay(ctx) { if (!ctx.hasPosition("FALL")) ctx.buy("FALL", { pctCash: 0.99 }); }`,
	}

	rep, err := RunScenarios(context.Background(),
		ScenarioSpec{Base: spec, Scenarios: []Scenario{covid}}, store, nil)
	if err != nil {
		t.Fatalf("run scenarios: %v", err)
	}
	r := rep.Runs[0]
	if !r.Measured() {
		t.Fatalf("the window should have run: %s%s", r.SkipReason, r.Error)
	}
	if got := float64(r.Return); got > -0.45 || got < -0.55 {
		t.Errorf("a held asset that halved should return about -50%%, got %.2f%%", got*100)
	}
	if got := float64(r.BenchmarkReturn); got > -0.15 || got < -0.25 {
		t.Errorf("the benchmark fell a fifth over the window, measured %.2f%%", got*100)
	}
	if got := float64(r.Excess); math.Abs(got-(float64(r.Return)-float64(r.BenchmarkReturn))) > 1e-9 {
		t.Errorf("excess %v is not return minus benchmark", got)
	}

	var found *Finding
	for i := range rep.Findings {
		if strings.Contains(rep.Findings[i].Title, "more than the benchmark") {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("losing 30 points more than the benchmark in a crisis produced no finding: %v",
			rep.Findings)
	}
	if found.Severity != SeverityCritical {
		t.Errorf("a 30 point gap should be critical, got %s", found.Severity)
	}
	if !strings.Contains(found.Detail, "COVID crash") {
		t.Errorf("the finding should name the window: %q", found.Detail)
	}
	if rep.Verdict == "" {
		t.Error("a measured replay should carry a verdict")
	}
}

// The base spec's dates bound which windows are considered, so a caller can
// ask for the recent crises without naming them — and the ones outside are
// still listed, with the reason.
func TestRequestedDateRangeExcludesWindowsOutLoud(t *testing.T) {
	store := scenarioStore(t, map[string]*market.Series{
		"SPY": scenarioSeries("SPY", "1995-01-02", "2024-12-31", func(market.Day) float64 { return 100 }),
	})
	spec := Spec{
		Name: "hold", Universe: []string{"SPY"}, Benchmarks: []string{"SPY"},
		Start: "2019-01-01", End: "2021-12-31",
		InitialCash: 100000, AllowFractional: true, Warmup: 5, Costs: Costs{},
		Code: `function onDay(ctx) { if (!ctx.hasPosition("SPY")) ctx.buy("SPY", { pctCash: 0.99 }); }`,
	}

	rep, err := RunScenarios(context.Background(), ScenarioSpec{Base: spec}, store, nil)
	if err != nil {
		t.Fatalf("run scenarios: %v", err)
	}
	for _, r := range rep.Runs {
		switch {
		case r.End < "2019-01-01":
			if !r.Skipped || !strings.Contains(r.SkipReason, "start date") {
				t.Errorf("%s closed before the requested range and should say so, got %q",
					r.Name, r.SkipReason)
			}
		case r.Start >= "2019-01-01" && r.End <= "2021-12-31":
			if r.Skipped {
				t.Errorf("%s is inside the requested range but was skipped: %s", r.Name, r.SkipReason)
			}
		}
	}
}

// Synthetic prices under a table of real crisis names is the most convincing
// wrong output this command could produce, so it has to say so itself.
func TestSyntheticPricesAreCalledOut(t *testing.T) {
	store := market.NewStore(market.NewSyntheticProvider(), nil, mustFundamentals(t))
	spec := Spec{
		Name: "hold", Universe: []string{"SPY"}, Benchmarks: []string{"SPY"},
		InitialCash: 100000, AllowFractional: true, Warmup: 5, Costs: Costs{},
		Code: `function onDay(ctx) { if (!ctx.hasPosition("SPY")) ctx.buy("SPY", { pctCash: 0.99 }); }`,
	}

	rep, err := RunScenarios(context.Background(),
		ScenarioSpec{Base: spec, Scenarios: []Scenario{Scenarios()[9]}}, store, nil)
	if err != nil {
		t.Fatalf("run scenarios: %v", err)
	}
	var found bool
	for _, f := range rep.Findings {
		if strings.Contains(f.Title, "not the real crises") && f.Severity == SeverityCritical {
			found = true
		}
	}
	if !found {
		t.Errorf("a synthetic run must be flagged as fiction, got %v", rep.Findings)
	}
}
