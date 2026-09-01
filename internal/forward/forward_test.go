package forward

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
)

func openTestLog(t *testing.T) *Log {
	t.Helper()
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return l
}

// decision builds a record whose timestamp is before the session it would
// trade in, which is what makes it a forward record rather than a backfill.
func decision(strategy string, asOf market.Day, weight float64) Entry {
	return Entry{
		At:         asOf.Time().Add(18 * time.Hour),
		Strategy:   strategy,
		CodeSHA256: "abc123",
		AsOf:       asOf,
		Reference:  "SPY",
		Horizon:    1,
		Positions:  []Position{{Symbol: "SPY", Weight: weight}},
	}
}

func mustRecord(t *testing.T, l *Log, e Entry) Entry {
	t.Helper()
	stored, appended, err := l.Record(e)
	if err != nil {
		t.Fatalf("record %s: %v", e.AsOf, err)
	}
	if !appended {
		t.Fatalf("record %s: nothing was appended", e.AsOf)
	}
	return stored
}

func readLines(t *testing.T, l *Log) []string {
	t.Helper()
	raw, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func writeLines(t *testing.T, l *Log, lines []string) {
	t.Helper()
	if err := os.WriteFile(l.Path(), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("rewrite the log: %v", err)
	}
}

func TestChainVerifiesWhenIntact(t *testing.T) {
	l := openTestLog(t)
	for i, day := range []market.Day{"2024-01-02", "2024-01-03", "2024-01-04"} {
		mustRecord(t, l, decision("golden cross", day, 0.5+float64(i)/10))
	}
	v, err := l.Verify()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !v.Intact || v.BreakAt != -1 {
		t.Fatalf("an untouched chain should verify, got break at %d: %s", v.BreakAt, v.Reason)
	}
	if v.Entries != 3 {
		t.Errorf("want 3 records, got %d", v.Entries)
	}
	// Verification reads the records back off disk, so this also asserts that
	// a record survives the JSON round trip byte for byte. If it did not, an
	// honest log would fail to verify and the feature would be worthless.
	if len(v.Skipped) != 0 {
		t.Errorf("nothing should have been skipped, got %v", v.Skipped)
	}
}

func TestEditingAMiddleRecordIsReportedAtItsIndex(t *testing.T) {
	l := openTestLog(t)
	for _, day := range []market.Day{"2024-01-02", "2024-01-03", "2024-01-04", "2024-01-05"} {
		mustRecord(t, l, decision("golden cross", day, 0.5))
	}

	// Improve the second record's book, leaving its seal alone — which is
	// what somebody tidying up their own history would do.
	lines := readLines(t, l)
	var edited Entry
	if err := json.Unmarshal([]byte(lines[1]), &edited); err != nil {
		t.Fatalf("parse record 1: %v", err)
	}
	edited.Positions[0].Weight = 0.95
	raw, err := json.Marshal(edited)
	if err != nil {
		t.Fatalf("encode record 1: %v", err)
	}
	lines[1] = string(raw)
	writeLines(t, l, lines)

	v, err := l.Verify()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Intact {
		t.Fatal("an edited record should not verify")
	}
	if v.BreakAt != 1 {
		t.Errorf("want the break reported at record 1, got %d (%s)", v.BreakAt, v.Reason)
	}
	if !strings.Contains(v.Reason, "altered") {
		t.Errorf("the reason should say what is wrong in words, got %q", v.Reason)
	}
}

func TestRemovingARecordIsReportedAtTheGap(t *testing.T) {
	l := openTestLog(t)
	for _, day := range []market.Day{"2024-01-02", "2024-01-03", "2024-01-04"} {
		mustRecord(t, l, decision("golden cross", day, 0.5))
	}
	lines := readLines(t, l)
	writeLines(t, l, []string{lines[0], lines[2]})

	v, err := l.Verify()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Intact {
		t.Fatal("a log with a record removed should not verify")
	}
	// The gap shows up at the record that should have followed the missing
	// one, because that is the record whose link no longer resolves.
	if v.BreakAt != 1 {
		t.Errorf("want the break reported at record 1, got %d (%s)", v.BreakAt, v.Reason)
	}
}

func TestRecordingTwiceOnOneDayIsIdempotent(t *testing.T) {
	l := openTestLog(t)
	e := decision("golden cross", "2024-01-02", 0.5)
	mustRecord(t, l, e)

	// A second run the same evening produces the same decision with a later
	// wall clock. That is one decision, not two observations.
	again := e
	again.At = e.At.Add(20 * time.Minute)
	stored, appended, err := l.Record(again)
	if err != nil {
		t.Fatalf("re-record: %v", err)
	}
	if appended {
		t.Error("recording the same decision twice should not append a second record")
	}
	if !stored.At.Equal(e.At.UTC()) {
		t.Errorf("the first record should stand, got one written at %s", stored.At)
	}
	if lines := readLines(t, l); len(lines) != 1 {
		t.Errorf("want one line in the log, got %d", len(lines))
	}
}

func TestAConflictingReRecordIsRefused(t *testing.T) {
	l := openTestLog(t)
	mustRecord(t, l, decision("golden cross", "2024-01-02", 0.5))

	changed := decision("golden cross", "2024-01-02", 0.9)
	_, appended, err := l.Record(changed)
	if appended {
		t.Fatal("a differing decision for a day already recorded must not be appended")
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want a ConflictError, got %v", err)
	}
	if !strings.Contains(err.Error(), "2024-01-02") {
		t.Errorf("the error should name the day in dispute, got %q", err)
	}
	if lines := readLines(t, l); len(lines) != 1 {
		t.Errorf("the log should be unchanged, got %d lines", len(lines))
	}
	if v, _ := l.Verify(); !v.Intact {
		t.Error("a refused record should leave the chain intact")
	}
}

func TestADifferentCodeHashIsADifferentDecision(t *testing.T) {
	l := openTestLog(t)
	mustRecord(t, l, decision("golden cross", "2024-01-02", 0.5))

	// The book is identical and the code is not, which is the case worth
	// catching: an edited strategy recording under the old name.
	rewritten := decision("golden cross", "2024-01-02", 0.5)
	rewritten.CodeSHA256 = "def456"
	if _, _, err := l.Record(rewritten); err == nil {
		t.Fatal("an edited strategy recording under the same name should be refused")
	}
}

func TestATornFinalLineIsSkippedNotFatal(t *testing.T) {
	l := openTestLog(t)
	mustRecord(t, l, decision("golden cross", "2024-01-02", 0.5))
	mustRecord(t, l, decision("golden cross", "2024-01-03", 0.5))

	// What a crash part-way through a write leaves behind.
	f, err := os.OpenFile(l.Path(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	if _, err := f.WriteString(`{"at":"2024-01-04T18:00:00Z","strategy":"golden cro`); err != nil {
		t.Fatalf("write a torn line: %v", err)
	}
	f.Close()

	r, err := l.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(r.Entries) != 2 {
		t.Fatalf("want the two whole records, got %d", len(r.Entries))
	}
	if len(r.Skipped) != 1 {
		t.Fatalf("the torn line should be reported, got %v", r.Skipped)
	}
	if v := VerifyEntries(r); !v.Intact {
		t.Errorf("a torn line is a hole, not a broken link: %s", v.Reason)
	}
	// And the log stays usable: the next record chains from the last whole
	// one rather than refusing to write.
	mustRecord(t, l, decision("golden cross", "2024-01-05", 0.5))
	v, err := l.Verify()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !v.Intact || v.Entries != 3 {
		t.Errorf("want 3 verified records after recovering, got %d intact=%v", v.Entries, v.Intact)
	}
}

func TestRecordSurvivesTheProcessThatWroteIt(t *testing.T) {
	dir := t.TempDir()
	for i, day := range []market.Day{"2024-01-02", "2024-01-03"} {
		l, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		mustRecord(t, l, decision("golden cross", day, 0.5))
	}
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	v, err := l.Verify()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !v.Intact || v.Entries != 2 {
		t.Errorf("want 2 records chained across instances, got %d intact=%v", v.Entries, v.Intact)
	}
}

func TestRecordRefusesAnUnnamedOrUndatedDecision(t *testing.T) {
	l := openTestLog(t)
	if _, _, err := l.Record(Entry{AsOf: "2024-01-02"}); err == nil {
		t.Error("a record with no strategy name should be refused")
	}
	if _, _, err := l.Record(Entry{Strategy: "x"}); err == nil {
		t.Error("a record with no as-of date should be refused")
	}
}

// --- scoring ---------------------------------------------------------------

type fakePrices struct{ series map[string][]market.Bar }

func (f fakePrices) Bars(_ context.Context, symbol string, from, to market.Day) ([]market.Bar, error) {
	var out []market.Bar
	for _, b := range f.series[symbol] {
		if b.Date >= from && b.Date <= to {
			out = append(out, b)
		}
	}
	return out, nil
}

// bar builds a session that opens at open and closes at close, with no split.
func bar(day market.Day, open, close float64) market.Bar {
	return market.Bar{Date: day, Open: open, High: close, Low: open, Close: close, AdjClose: close}
}

func TestScoreMeasuresTheSessionAfterTheAsOfDate(t *testing.T) {
	px := fakePrices{series: map[string][]market.Bar{
		"SPY": {
			bar("2024-01-02", 90, 95), // the as-of session, which must not be scored
			bar("2024-01-03", 100, 102),
		},
	}}
	card, err := Score(context.Background(), []Entry{decision("golden cross", "2024-01-02", 1)}, px)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if card.Forward.Count != 1 {
		t.Fatalf("want one forward decision scored, got %+v", card.Forward)
	}
	got := float64(card.Entries[0].Return)
	if diff := got - 0.02; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("want the next session's open-to-close return of 2%%, got %.6f", got)
	}
	if card.Entries[0].Entered != "2024-01-03" || card.Entries[0].Exited != "2024-01-03" {
		t.Errorf("want the window to be the session after the as-of date, got %s to %s",
			card.Entries[0].Entered, card.Entries[0].Exited)
	}
}

func TestADecisionWithNoElapsedOutcomeIsSkippedNotScoredAsZero(t *testing.T) {
	px := fakePrices{series: map[string][]market.Bar{
		"SPY": {bar("2024-01-02", 90, 95)},
	}}
	card, err := Score(context.Background(), []Entry{decision("golden cross", "2024-01-02", 1)}, px)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if card.Pending != 1 {
		t.Fatalf("want the decision left pending, got %+v", card)
	}
	if card.Forward.Count != 0 || card.Backfill.Count != 0 {
		t.Error("a decision with no outcome must not reach any aggregate")
	}
	if card.Entries[0].Return.Defined() {
		t.Errorf("an unscored decision must not carry a return, got %v", card.Entries[0].Return)
	}
	if card.Entries[0].Pending == "" {
		t.Error("the decision should say in words why it was not scored")
	}
	if !strings.Contains(card.Verdict, "not yet") && !strings.Contains(card.Verdict, "not old enough") &&
		!strings.Contains(card.Verdict, "none of them old enough") {
		t.Errorf("the verdict should say nothing is scoreable yet, got %q", card.Verdict)
	}
}

func TestABarDatedAfterTodayIsNotAnOutcome(t *testing.T) {
	// The synthetic provider behind --offline generates whatever date it is
	// asked for, including tomorrow's. Scoring against a price that has not
	// been printed yet would turn this feature into the thing it exists to
	// refuse, so the scorer trims the window at today rather than trusting
	// what a provider hands back.
	px := fakePrices{series: map[string][]market.Bar{
		"SPY": {bar("2024-01-02", 90, 95), bar("2024-01-03", 100, 102)},
	}}
	entries := []Entry{decision("golden cross", "2024-01-02", 1)}

	card, err := scoreAsOf(context.Background(), entries, px, "2024-01-02")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if card.Pending != 1 {
		t.Fatalf("a session that has not happened must not be scored, got %+v", card)
	}

	card, err = scoreAsOf(context.Background(), entries, px, "2024-01-03")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if card.Forward.Count != 1 {
		t.Errorf("once the session has happened it should score, got %+v", card.Forward)
	}
}

func TestAFlatDecisionWaitsForItsSessionToo(t *testing.T) {
	// Without a reference calendar a flat book has no way of knowing whether
	// its session has happened, and scoring it as zero would invent the
	// outcome. This is the case the Reference field exists for.
	flat := decision("golden cross", "2024-01-02", 0)
	flat.Positions = nil
	px := fakePrices{series: map[string][]market.Bar{"SPY": {bar("2024-01-02", 90, 95)}}}
	card, err := Score(context.Background(), []Entry{flat}, px)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if card.Pending != 1 || card.Forward.Count != 0 {
		t.Fatalf("want the flat decision left pending, got %+v", card)
	}

	px.series["SPY"] = append(px.series["SPY"], bar("2024-01-03", 100, 102))
	card, err = Score(context.Background(), []Entry{flat}, px)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if card.Forward.Count != 1 || card.Flat != 1 {
		t.Fatalf("want the flat decision scored once its session happened, got %+v", card)
	}
	if float64(card.Entries[0].Return) != 0 {
		t.Errorf("holding nothing earns nothing, got %v", card.Entries[0].Return)
	}
}

func TestABackfilledRecordIsKeptOutOfTheForwardAggregate(t *testing.T) {
	px := fakePrices{series: map[string][]market.Bar{
		"SPY": {bar("2024-01-02", 90, 95), bar("2024-01-03", 100, 102)},
	}}
	late := decision("golden cross", "2024-01-02", 1)
	// Written a month after the session it claims to be deciding about.
	late.At = time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)

	card, err := Score(context.Background(), []Entry{late}, px)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if card.Forward.Count != 0 {
		t.Error("a record written after its outcome existed is not forward evidence")
	}
	if card.Backfill.Count != 1 {
		t.Fatalf("want it counted as a backfill, got %+v", card.Backfill)
	}
	if !card.Entries[0].Backfilled {
		t.Error("the record should be marked as backfilled")
	}
	if !strings.Contains(card.Verdict, "backfill") {
		t.Errorf("the verdict should say the backfill is not evidence, got %q", card.Verdict)
	}
}

func TestTheVerdictRefusesToBoastOverNineSamples(t *testing.T) {
	series := []market.Bar{bar("2024-01-01", 100, 100)}
	var entries []Entry
	day := market.Day("2024-01-01")
	for i := 0; i < 9; i++ {
		next := day.Add(1)
		// Every decision wins, which is exactly the shape that produces an
		// impressive-looking hit rate over nothing.
		series = append(series, bar(next, 100, 101))
		e := decision("lucky", day, 1)
		entries = append(entries, e)
		day = next
	}
	card, err := Score(context.Background(), entries, fakePrices{series: map[string][]market.Bar{"SPY": series}})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if card.Forward.Count != 9 {
		t.Fatalf("want nine scored decisions, got %+v", card.Forward)
	}
	if float64(card.Forward.HitRate) != 1 {
		t.Fatalf("want a 100%% hit rate to argue against, got %v", card.Forward.HitRate)
	}
	if !strings.Contains(card.Verdict, "far too few") {
		t.Errorf("nine perfect decisions must be called what they are, got %q", card.Verdict)
	}
	// Nine identical returns have no spread, so there is no t-statistic to
	// print and printing a zero one would be worse than printing none.
	if card.Forward.TStat.Defined() {
		t.Errorf("want an undefined t-statistic with no spread, got %v", card.Forward.TStat)
	}
	if body, err := json.Marshal(card.Forward); err != nil {
		t.Errorf("an undefined ratio must still encode: %v", err)
	} else if !strings.Contains(string(body), `"t_stat":null`) {
		t.Errorf("an undefined t-statistic should marshal to null, got %s", body)
	}
}

func TestTStatAndTheHorizonToReachIt(t *testing.T) {
	// Two sessions held, so the exit is the close of the second one.
	px := fakePrices{series: map[string][]market.Bar{
		"SPY": {
			bar("2024-01-02", 90, 95),
			bar("2024-01-03", 100, 102),
			bar("2024-01-04", 103, 110),
		},
	}}
	e := decision("golden cross", "2024-01-02", 1)
	e.Horizon = 2
	card, err := Score(context.Background(), []Entry{e}, px)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	got := float64(card.Entries[0].Return)
	if diff := got - 0.1; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("want the two-session return of 10%%, got %.6f", got)
	}
	if card.Entries[0].Exited != "2024-01-04" {
		t.Errorf("want the exit on the second session, got %s", card.Entries[0].Exited)
	}
	if card.Forward.TStat.Defined() {
		t.Error("one observation cannot have a t-statistic")
	}
}

func TestASymbolWithNoPricesIsUnscorableRatherThanZero(t *testing.T) {
	px := fakePrices{series: map[string][]market.Bar{
		"SPY": {bar("2024-01-02", 90, 95), bar("2024-01-03", 100, 102)},
	}}
	e := decision("golden cross", "2024-01-02", 0.5)
	e.Positions = append(e.Positions, Position{Symbol: "DELISTED", Weight: 0.5})
	card, err := Score(context.Background(), []Entry{e}, px)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if card.Unscorable != 1 || card.Forward.Count != 0 {
		t.Fatalf("want the decision reported as unscorable, got %+v", card)
	}
	if card.Entries[0].Return.Defined() {
		t.Error("an unmeasurable decision must not carry a return")
	}
}

// --- projecting the book ---------------------------------------------------

func TestIntendAppliesTheFinalSessionsOrders(t *testing.T) {
	last := engine.DayRecord{
		Date:      "2024-01-02",
		Equity:    100000,
		Positions: []engine.PositionSnapshot{{Symbol: "SPY", Weight: 0.6, Price: 400}},
		Orders: []engine.Order{
			{Symbol: "SPY", Kind: engine.KindWeight, Weight: 0.8, IsTarget: true},
			{Symbol: "TLT", Kind: engine.KindNotional, Notional: 20000},
		},
	}
	book := Intend(last, map[string]float64{"SPY": 400, "TLT": 100}, 1)
	if len(book) != 2 || book[0].Symbol != "SPY" || book[1].Symbol != "TLT" {
		t.Fatalf("want SPY and TLT in symbol order, got %+v", book)
	}
	if book[0].Weight != 0.8 {
		t.Errorf("a target-weight order should set the weight, got %v", book[0].Weight)
	}
	if book[1].Weight != 0.2 {
		t.Errorf("a notional order should become its share of equity, got %v", book[1].Weight)
	}
}

func TestIntendScalesBackWhatTheRunWouldNotAllow(t *testing.T) {
	last := engine.DayRecord{
		Date:   "2024-01-02",
		Equity: 100000,
		Orders: []engine.Order{
			{Symbol: "AAA", Kind: engine.KindWeight, Weight: 1.0, IsTarget: true},
			{Symbol: "BBB", Kind: engine.KindWeight, Weight: 1.0, IsTarget: true},
		},
	}
	book := Intend(last, nil, 1)
	var gross float64
	for _, p := range book {
		gross += p.Weight
	}
	if diff := gross - 1.0; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("want the book scaled to the leverage limit, got gross %v", gross)
	}
}

func TestIntendDropsAClosedPosition(t *testing.T) {
	last := engine.DayRecord{
		Date:      "2024-01-02",
		Equity:    100000,
		Positions: []engine.PositionSnapshot{{Symbol: "SPY", Weight: 0.6, Price: 400}},
		Orders:    []engine.Order{{Symbol: "SPY", Kind: engine.KindWeight, Weight: 0, IsTarget: true}},
	}
	if book := Intend(last, map[string]float64{"SPY": 400}, 1); len(book) != 0 {
		t.Errorf("a position sold to nothing should not be recorded, got %+v", book)
	}
}

func TestConcurrentRecordsStillChain(t *testing.T) {
	// Two schedules firing at once must not interleave into a chain that
	// cannot be verified, because an unverifiable chain is indistinguishable
	// from a tampered one.
	l := openTestLog(t)
	days := []market.Day{"2024-01-02", "2024-01-03", "2024-01-04", "2024-01-05",
		"2024-01-08", "2024-01-09", "2024-01-10", "2024-01-11"}
	var wg sync.WaitGroup
	for _, day := range days {
		wg.Add(1)
		go func(d market.Day) {
			defer wg.Done()
			if _, _, err := l.Record(decision("golden cross", d, 0.5)); err != nil {
				t.Errorf("record %s: %v", d, err)
			}
		}(day)
	}
	wg.Wait()

	v, err := l.Verify()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !v.Intact || v.Entries != len(days) {
		t.Errorf("want %d chained records, got %d intact=%v (%s)", len(days), v.Entries, v.Intact, v.Reason)
	}
}
