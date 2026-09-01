package market

import (
	"math"
	"strings"
	"testing"
)

// cleanBars builds a plausible daily series over the real US session
// calendar: every trading day between from and to, no weekends, no holidays,
// a small deterministic drift and a normal intraday range.
func cleanBars(from, to Day, start float64) []Bar {
	var bars []Bar
	price := start
	for i, d := range TradingDays(from, to) {
		// A deterministic wobble, so no two closes repeat and nothing looks
		// like a split.
		price *= 1 + 0.004*math.Sin(float64(i)/3.0) + 0.0002
		bars = append(bars, Bar{
			Date:     d,
			Open:     price * 0.998,
			High:     price * 1.006,
			Low:      price * 0.993,
			Close:    price,
			AdjClose: price,
			Volume:   1e6 + float64(i),
		})
	}
	return bars
}

// freeze repeats one close over n bars, moving the whole bar with it so the
// fixture stays internally consistent and only the staleness is under test.
func freeze(bars []Bar, at, n int) {
	c := bars[at].Close
	for i := at; i < at+n && i < len(bars); i++ {
		scale := c / bars[i].Close
		bars[i].Open *= scale
		bars[i].High *= scale
		bars[i].Low *= scale
		bars[i].Close = c
		bars[i].AdjClose = c
	}
}

func findingOf(r Report, kind string) (Finding, bool) {
	for _, f := range r.Findings {
		if f.Kind == kind {
			return f, true
		}
	}
	return Finding{}, false
}

// A clean series must produce nothing at all. A noisy auditor is ignored, and
// an ignored auditor is worse than no auditor, so this is the test that
// matters most.
func TestAuditCleanSeriesHasNoFindings(t *testing.T) {
	s := NewSeries("CLEAN", cleanBars("2015-01-02", "2023-12-29", 100))
	r := Audit(s)
	if len(r.Findings) != 0 {
		for _, f := range r.Findings {
			t.Errorf("false positive: %s %s — %s", f.Severity, f.Kind, f.Detail)
		}
		t.Fatalf("a clean series produced %d findings", len(r.Findings))
	}
	if r.Bars < 2000 {
		t.Fatalf("the fixture is too short to prove anything: %d bars", r.Bars)
	}
}

func TestAuditNamesTheSplitRatio(t *testing.T) {
	bars := cleanBars("2020-01-02", "2020-12-31", 400)
	// Halve every close from the 100th bar on, which is what an unadjusted
	// 2:1 split looks like: a step down that never comes back.
	split := bars[100].Date
	for i := 100; i < len(bars); i++ {
		bars[i].Open /= 2
		bars[i].High /= 2
		bars[i].Low /= 2
		bars[i].Close /= 2
		bars[i].AdjClose /= 2
	}
	r := Audit(NewSeries("SPLIT", bars))

	f, ok := findingOf(r, KindSplit)
	if !ok {
		t.Fatalf("an unadjusted 2:1 split was not detected; findings: %+v", r.Findings)
	}
	if f.Severity != SeverityCritical {
		t.Errorf("an unadjusted split is critical, got %s", f.Severity)
	}
	if !strings.Contains(f.Detail, "2:1") {
		t.Errorf("the finding must name the ratio it resembles: %s", f.Detail)
	}
	if len(f.Dates) != 1 || f.Dates[0] != split {
		t.Errorf("dates: got %v, want [%s]", f.Dates, split)
	}
	// The same day must not be reported twice under a weaker heading.
	if _, ok := findingOf(r, KindExtreme); ok {
		t.Errorf("the split day was also reported as an extreme return")
	}
}

func TestAuditNamesAReverseSplit(t *testing.T) {
	bars := cleanBars("2020-01-02", "2020-12-31", 3)
	for i := 60; i < len(bars); i++ {
		bars[i].Open *= 10
		bars[i].High *= 10
		bars[i].Low *= 10
		bars[i].Close *= 10
		bars[i].AdjClose *= 10
	}
	r := Audit(NewSeries("REV", bars))
	f, ok := findingOf(r, KindSplit)
	if !ok {
		t.Fatalf("a 1:10 reverse split was not detected; findings: %+v", r.Findings)
	}
	if !strings.Contains(f.Detail, "1:10 reverse") {
		t.Errorf("the finding must name the ratio: %s", f.Detail)
	}
}

// A share that halves on news and stays down is not a split, and calling it
// one would put a critical finding on real data.
func TestAuditDoesNotCallACollapseASplit(t *testing.T) {
	bars := cleanBars("2020-01-02", "2020-12-31", 100)
	i := 80
	prev := bars[i-1].Close
	// It traded the whole way down: opened near yesterday, closed at half.
	bars[i].Open = prev
	bars[i].High = prev
	bars[i].Low = prev * 0.49
	bars[i].Close = prev * 0.5
	bars[i].AdjClose = bars[i].Close
	for j := i + 1; j < len(bars); j++ {
		bars[j].Open /= 2
		bars[j].High /= 2
		bars[j].Low /= 2
		bars[j].Close /= 2
		bars[j].AdjClose /= 2
	}
	r := Audit(NewSeries("CRASH", bars))
	if f, ok := findingOf(r, KindSplit); ok {
		t.Errorf("a -50%% day with a 104%% intraday range was called a split: %s", f.Detail)
	}
	if _, ok := findingOf(r, KindExtreme); !ok {
		t.Errorf("the collapse should still be listed as a large move")
	}
}

// A single bad print that corrects the next day is a bad print, not a split.
func TestAuditIgnoresAHalvedPriceThatComesBack(t *testing.T) {
	bars := cleanBars("2020-01-02", "2020-12-31", 100)
	i := 50
	bars[i].Open /= 2
	bars[i].High /= 2
	bars[i].Low /= 2
	bars[i].Close /= 2
	bars[i].AdjClose /= 2
	r := Audit(NewSeries("PRINT", bars))
	if f, ok := findingOf(r, KindSplit); ok {
		t.Errorf("a one-day dip that reverted was called a split: %s", f.Detail)
	}
}

func TestAuditFindsStaleRuns(t *testing.T) {
	bars := cleanBars("2020-01-02", "2020-12-31", 100)
	freeze(bars, 29, 9)
	r := Audit(NewSeries("STALE", bars))
	f, ok := findingOf(r, KindStale)
	if !ok {
		t.Fatalf("nine identical closes were not flagged; findings: %+v", r.Findings)
	}
	if !strings.Contains(f.Detail, "9 sessions") {
		t.Errorf("the finding must say how long the run was: %s", f.Detail)
	}
	if f.Severity != SeverityWarning {
		t.Errorf("a nine-session run is a warning, got %s", f.Severity)
	}
}

// Four identical closes happen. Flagging them would fire on real data.
func TestAuditIgnoresAShortRunOfIdenticalCloses(t *testing.T) {
	bars := cleanBars("2020-01-02", "2020-12-31", 100)
	freeze(bars, 29, 4)
	if f, ok := findingOf(Audit(NewSeries("QUIET", bars)), KindStale); ok {
		t.Errorf("four identical closes should not be a finding: %s", f.Detail)
	}
}

func TestAuditDoesNotReportHolidaysAsGaps(t *testing.T) {
	// cleanBars is built from the calendar itself, so any gap finding here
	// is the calendar disagreeing with itself about a holiday.
	r := Audit(NewSeries("HOL", cleanBars("2015-01-02", "2023-12-29", 100)))
	if f, ok := findingOf(r, KindGap); ok {
		t.Fatalf("holidays reported as gaps: %s (%v)", f.Detail, f.Dates)
	}
	// And the specific ones people get wrong.
	for _, d := range []Day{
		"2015-07-03", // Independence Day observed on the Friday
		"2018-12-05", // the Bush funeral, an unscheduled closure
		"2021-12-24", // Christmas observed on the Friday
		"2022-06-20", // the first Juneteenth
		"2023-04-07", // Good Friday
	} {
		if IsTradingDay(d) {
			t.Errorf("%s was a market holiday, the calendar says it was a session", d)
		}
	}
	// 31 December 2021 is the trap: New Year's Day fell on a Saturday and the
	// exchange traded that Friday anyway.
	if !IsTradingDay("2021-12-31") {
		t.Errorf("2021-12-31 was a full session and the calendar closed it")
	}
}

func TestAuditFindsRealGaps(t *testing.T) {
	bars := cleanBars("2020-01-02", "2020-12-31", 100)
	// Remove a fortnight of sessions.
	cut := append([]Bar(nil), bars[:60]...)
	cut = append(cut, bars[70:]...)
	r := Audit(NewSeries("GAP", NewSeries("GAP", cut).Bars))
	f, ok := findingOf(r, KindGap)
	if !ok {
		t.Fatalf("ten missing sessions were not detected; findings: %+v", r.Findings)
	}
	if f.Count != 10 {
		t.Errorf("missing sessions: got %d, want 10", f.Count)
	}
	if f.Severity != SeverityCritical {
		t.Errorf("a ten-session hole is critical, got %s", f.Severity)
	}
}

func TestAuditFindsBarsOnClosedDays(t *testing.T) {
	bars := cleanBars("2020-01-02", "2020-03-31", 100)
	bars = append(bars,
		Bar{Date: "2020-01-04", Open: 100, High: 101, Low: 99, Close: 100, AdjClose: 100, Volume: 1e6},
		Bar{Date: "2020-02-17", Open: 100, High: 101, Low: 99, Close: 100, AdjClose: 100, Volume: 1e6},
	)
	r := Audit(NewSeries("SHUT", bars))

	var weekend, holiday bool
	for _, f := range r.Findings {
		if f.Kind != KindClosedDay {
			continue
		}
		switch {
		case strings.Contains(f.Title, "weekend"):
			weekend = true
			if f.Severity != SeverityCritical {
				t.Errorf("a Saturday bar is critical, got %s", f.Severity)
			}
		case strings.Contains(f.Title, "holiday"):
			holiday = true
		}
	}
	if !weekend {
		t.Errorf("a Saturday bar was not reported; findings: %+v", r.Findings)
	}
	if !holiday {
		t.Errorf("a Presidents' Day bar was not reported; findings: %+v", r.Findings)
	}
}

func TestAuditFindsOHLCViolations(t *testing.T) {
	bars := []Bar{
		{Date: "2024-01-02", Open: 100, High: 101, Low: 99, Close: 100, AdjClose: 100, Volume: 1e6},
		{Date: "2024-01-03", Open: 100, High: 95, Low: 99, Close: 100, AdjClose: 100, Volume: 1e6},
		{Date: "2024-01-04", Open: 100, High: 101, Low: 99, Close: 140, AdjClose: 140, Volume: 1e6},
		{Date: "2024-01-05", Open: 100, High: 101, Low: 99, Close: 100, AdjClose: 100, Volume: -5},
	}
	r := Audit(NewSeries("BAD", bars))
	f, ok := findingOf(r, KindOHLC)
	if !ok {
		t.Fatalf("impossible bars were not detected; findings: %+v", r.Findings)
	}
	if f.Severity != SeverityCritical {
		t.Errorf("impossible bars are critical, got %s", f.Severity)
	}
	if f.Count != 3 {
		t.Errorf("bad bars: got %d, want 3", f.Count)
	}
	for _, want := range []string{"high below the low", "close outside", "negative volume"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("the finding must say what was wrong (%q): %s", want, f.Detail)
		}
	}
}

// A vendor file with only a date and a close is thin, not broken: the loader
// backfills the other three, and reporting that as an arithmetic error would
// fire on every such file.
func TestAuditAcceptsACloseOnlySeries(t *testing.T) {
	bars := cleanBars("2020-01-02", "2020-06-30", 100)
	for i := range bars {
		bars[i].Open, bars[i].High, bars[i].Low = 0, 0, 0
	}
	if f, ok := findingOf(Audit(NewSeries("THIN", bars)), KindOHLC); ok {
		t.Errorf("a close-only series was reported as impossible: %s", f.Detail)
	}
}

func TestAuditFindsDuplicateDates(t *testing.T) {
	// Built without NewSeries on purpose: NewSeries de-duplicates, so a
	// duplicate can only be seen before it builds one.
	s := &Series{Symbol: "DUP", Bars: []Bar{
		{Date: "2024-01-02", Open: 100, High: 101, Low: 99, Close: 100, AdjClose: 100, Volume: 1e6},
		{Date: "2024-01-03", Open: 100, High: 101, Low: 99, Close: 100.5, AdjClose: 100.5, Volume: 1e6},
		{Date: "2024-01-03", Open: 100, High: 101, Low: 99, Close: 100.7, AdjClose: 100.7, Volume: 1e6},
		{Date: "2024-01-04", Open: 100, High: 102, Low: 99, Close: 101, AdjClose: 101, Volume: 1e6},
	}}
	f, ok := findingOf(Audit(s), KindDuplicate)
	if !ok {
		t.Fatal("a repeated date was not detected")
	}
	if f.Severity != SeverityCritical || f.Count != 1 || len(f.Dates) != 1 || f.Dates[0] != "2024-01-03" {
		t.Errorf("got %s count=%d dates=%v", f.Severity, f.Count, f.Dates)
	}
}

func TestAuditFindsZeroVolumeSessions(t *testing.T) {
	bars := cleanBars("2020-01-02", "2020-12-31", 100)
	bars[10].Volume = 0
	bars[11].Volume = 0
	r := Audit(NewSeries("Q", bars))
	f, ok := findingOf(r, KindZeroVolume)
	if !ok {
		t.Fatalf("zero-volume sessions were not detected; findings: %+v", r.Findings)
	}
	if f.Count != 2 {
		t.Errorf("count: got %d, want 2", f.Count)
	}
}

// An index reports no volume at all. That is what an index is, not a defect.
func TestAuditIgnoresASeriesWithNoVolumeAnywhere(t *testing.T) {
	bars := cleanBars("2020-01-02", "2020-12-31", 100)
	for i := range bars {
		bars[i].Volume = 0
	}
	if f, ok := findingOf(Audit(NewSeries("^IDX", bars)), KindZeroVolume); ok {
		t.Errorf("an index was reported for having no volume: %s", f.Detail)
	}
}

func TestAuditReportsExtremeMovesWithoutCallingThemErrors(t *testing.T) {
	bars := cleanBars("2020-01-02", "2020-12-31", 100)
	i := 40
	bars[i].Close = bars[i-1].Close * 0.75
	bars[i].AdjClose = bars[i].Close
	bars[i].Low = bars[i].Close * 0.99
	bars[i].High = bars[i-1].Close
	for j := i + 1; j < len(bars); j++ {
		bars[j].Close *= 0.75
		bars[j].AdjClose = bars[j].Close
		bars[j].Open *= 0.75
		bars[j].High *= 0.75
		bars[j].Low *= 0.75
	}
	f, ok := findingOf(Audit(NewSeries("NEWS", bars)), KindExtreme)
	if !ok {
		t.Fatal("a -25% day was not listed")
	}
	if f.Severity != SeverityNote {
		t.Errorf("one large move is a note, got %s", f.Severity)
	}
	if !strings.Contains(f.Detail, "Most moves this size are real") {
		t.Errorf("the finding must not imply every outlier is an error: %s", f.Detail)
	}
}

// A symbol that trades at weekends is not on the exchange calendar, so the
// calendar checks have to stand down rather than report every Saturday.
func TestAuditSkipsTheCalendarForContinuousMarkets(t *testing.T) {
	var bars []Bar
	price := 30000.0
	for i, d := 0, Day("2022-01-01"); d <= "2022-12-31"; d, i = d.Add(1), i+1 {
		price *= 1 + 0.005*math.Sin(float64(i)/4.0)
		bars = append(bars, Bar{
			Date: d, Open: price * 0.99, High: price * 1.01, Low: price * 0.98,
			Close: price, AdjClose: price, Volume: 1e6,
		})
	}
	r := Audit(NewSeries("BTC-USD", bars))
	for _, f := range r.Findings {
		if f.Kind == KindClosedDay || f.Kind == KindGap {
			t.Errorf("calendar check fired on a continuously traded symbol: %s", f.Detail)
		}
	}
	if _, ok := findingOf(r, KindContinuous); !ok {
		t.Errorf("the report should say why the calendar checks were skipped")
	}
}

func TestAuditCriticalKeepsOnlyDisqualifyingFindings(t *testing.T) {
	bars := cleanBars("2020-01-02", "2020-12-31", 100)
	// A warning-grade stale run and nothing critical.
	freeze(bars, 29, 9)
	if got := AuditCritical(NewSeries("S", bars)); len(got) != 0 {
		t.Errorf("a warning reached the critical scan: %+v", got)
	}

	bars[50].High = bars[50].Low / 2
	got := AuditCritical(NewSeries("S", bars))
	if len(got) != 1 || got[0].Kind != KindOHLC {
		t.Fatalf("critical scan: got %+v", got)
	}
	if got[0].Symbol != "S" {
		t.Errorf("a collected finding must name its symbol, got %q", got[0].Symbol)
	}
}

func TestAuditEmptySeries(t *testing.T) {
	if r := Audit(nil); len(r.Findings) != 0 {
		t.Errorf("a nil series should not produce findings")
	}
	r := Audit(NewSeries("EMPTY", nil))
	if !strings.Contains(r.Verdict, "nothing to audit") {
		t.Errorf("verdict: %q", r.Verdict)
	}
}
