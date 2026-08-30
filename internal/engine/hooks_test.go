package engine

import (
	"context"
	"strings"
	"testing"
)

// runWithLogs runs a strategy and returns every log line it wrote, in order.
func runWithLogs(t *testing.T, code string, mutate func(*Spec)) (*Result, []string) {
	t.Helper()
	spec := baseSpec(code)
	spec.Universe = []string{"AAPL"}
	spec.Start, spec.End = "2022-01-03", "2022-06-30"
	if mutate != nil {
		mutate(&spec)
	}
	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var logs []string
	for _, d := range res.Days {
		logs = append(logs, d.Logs...)
	}
	return res, logs
}

func TestOnFillReceivesTheExecution(t *testing.T) {
	res, logs := runWithLogs(t, `
		function onDay(ctx) {
			if (ctx.dayIndex === 0) ctx.buy("AAPL", { shares: 10 }, "opening");
		}
		function onFill(ctx, fill) {
			ctx.log("filled " + fill.side + " " + fill.shares + " " + fill.symbol
				+ " @ " + fill.price.toFixed(2) + " because " + fill.reason);
		}
	`, nil)
	if len(res.Fills) != 1 {
		t.Fatalf("want 1 fill, got %d", len(res.Fills))
	}
	if len(logs) != 1 {
		t.Fatalf("onFill should have fired once, got %d log lines: %v", len(logs), logs)
	}
	line := logs[0]
	for _, want := range []string{"filled buy", "10", "AAPL", "because opening"} {
		if !strings.Contains(line, want) {
			t.Errorf("the fill object is missing %q: %s", want, line)
		}
	}
}

func TestOnStopFiresSeparatelyFromOnFill(t *testing.T) {
	// A stop hit and an order filling are different events, and a strategy
	// should not have to parse a reason string to tell them apart.
	res, logs := runWithLogs(t, `
		function onDay(ctx) {
			if (!ctx.hasPosition("AAPL") && ctx.dayIndex < 40) {
				ctx.buy("AAPL", { pctCash: 0.9, stopLoss: 0.01 }, "entry");
			}
		}
		function onFill(ctx, fill) { ctx.log("FILL " + fill.symbol); }
		function onStop(ctx, fill) { ctx.log("STOP " + fill.symbol + " " + fill.reason); }
	`, nil)

	var fills, stops int
	for _, l := range logs {
		if strings.HasPrefix(l, "FILL ") {
			fills++
		}
		if strings.HasPrefix(l, "STOP ") {
			stops++
		}
	}
	if fills == 0 {
		t.Fatal("onFill never fired")
	}
	if stops == 0 {
		t.Fatal("a 1% stop on a daily-entry strategy should have triggered at least once")
	}
	// Every execution must be reported exactly once, by exactly one hook.
	if fills+stops != len(res.Fills) {
		t.Errorf("%d fills and %d stops reported, but %d executions happened",
			fills, stops, len(res.Fills))
	}
}

func TestPeriodHooksFireOncePerPeriod(t *testing.T) {
	_, logs := runWithLogs(t, `
		function onDay(ctx) {}
		function onMonth(ctx) { ctx.log("M " + ctx.date); }
		function onWeek(ctx) { ctx.log("W " + ctx.date); }
	`, nil)

	var months, weeks int
	seenMonth := map[string]bool{}
	for _, l := range logs {
		if strings.HasPrefix(l, "M ") {
			months++
			key := l[2:9] // YYYY-MM
			if seenMonth[key] {
				t.Errorf("onMonth fired twice in %s", key)
			}
			seenMonth[key] = true
		}
		if strings.HasPrefix(l, "W ") {
			weeks++
		}
	}
	// January to June inclusive.
	if months != 6 {
		t.Errorf("onMonth should fire six times over six months, got %d", months)
	}
	if weeks < 20 || weeks > 28 {
		t.Errorf("onWeek fired %d times over about six months, which is implausible", weeks)
	}
}

// The regression that motivated computing the flags once per session: a hook
// asking the same question earlier in the day used to consume the answer.
func TestPeriodHookDoesNotConsumeTheCalendarFlag(t *testing.T) {
	_, logs := runWithLogs(t, `
		function onDay(ctx) {
			if (ctx.isFirstTradingDayOfMonth()) ctx.log("onDay saw it: " + ctx.date);
		}
		function onMonth(ctx) {
			if (ctx.isFirstTradingDayOfMonth()) ctx.log("onMonth saw it: " + ctx.date);
		}
	`, nil)

	var inHook, inDay int
	for _, l := range logs {
		if strings.HasPrefix(l, "onMonth saw it") {
			inHook++
		}
		if strings.HasPrefix(l, "onDay saw it") {
			inDay++
		}
	}
	if inHook != 6 || inDay != 6 {
		t.Errorf("both callers should see the same six month starts, got hook=%d day=%d",
			inHook, inDay)
	}
}

func TestHooksAreOptional(t *testing.T) {
	// A strategy defining none of them must be unaffected.
	res, logs := runWithLogs(t, `
		function onDay(ctx) {
			if (ctx.dayIndex === 0) ctx.buy("AAPL", { shares: 5 });
		}
	`, nil)
	if len(res.Fills) != 1 {
		t.Errorf("want 1 fill, got %d", len(res.Fills))
	}
	if len(logs) != 0 {
		t.Errorf("no hooks defined, so nothing should have logged: %v", logs)
	}
	if res.StrategyErrors != 0 {
		t.Errorf("absent hooks are not errors, got %d", res.StrategyErrors)
	}
}

func TestAThrowingHookIsRecordedAndTheRunContinues(t *testing.T) {
	res, _ := runWithLogs(t, `
		function onDay(ctx) {
			if (!ctx.hasPosition("AAPL")) ctx.buy("AAPL", { shares: 1 });
		}
		function onFill(ctx, fill) { throw new Error("deliberate"); }
	`, nil)
	if res.StrategyErrors == 0 {
		t.Error("a throwing hook should be counted")
	}
	if len(res.Curve) < 100 {
		t.Errorf("the run should have continued to the end, got %d sessions", len(res.Curve))
	}
	var found bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "onFill error") {
			found = true
		}
	}
	if !found {
		t.Errorf("the warning should name the hook: %v", res.Warnings)
	}
}

func TestHooksFireUnderCloseFills(t *testing.T) {
	// Under close fills, execution happens after onDay in the same session,
	// so the hooks must still see it.
	_, logs := runWithLogs(t, `
		function onDay(ctx) {
			if (ctx.dayIndex === 0) ctx.buy("AAPL", { shares: 3 });
		}
		function onFill(ctx, fill) { ctx.log("filled " + fill.shares); }
	`, func(s *Spec) { s.Fill = FillClose })

	if len(logs) != 1 || !strings.Contains(logs[0], "filled 3") {
		t.Errorf("onFill did not fire under close fills: %v", logs)
	}
}
