package market

import (
	"math"
	"testing"
)

// continuousBars builds a series that prints every calendar day, which is what
// a crypto feed looks like.
func continuousBars(from, to Day, start float64) []Bar {
	var bars []Bar
	price := start
	for i, d := 0, from; d <= to; d, i = d.Add(1), i+1 {
		price *= 1 + 0.004*math.Sin(float64(i)/3.0) + 0.0002
		bars = append(bars, Bar{
			Date: d, Open: price * 0.998, High: price * 1.006, Low: price * 0.993,
			Close: price, AdjClose: price, Volume: 1e6 + float64(i),
		})
	}
	return bars
}

func TestSessionsPerYearPerCalendar(t *testing.T) {
	for _, tc := range []struct {
		cal  Calendar
		want float64
	}{
		{CalendarUSEquity, 252},
		{CalendarContinuous, 365},
		{CalendarFX, 261},
		{"", 252},
		{"nonsense", 252},
	} {
		if got := tc.cal.SessionsPerYear(); got != tc.want {
			t.Errorf("%q: sessions per year is %v, want %v", tc.cal, got, tc.want)
		}
	}
}

// The bars are the evidence, not the ticker. A file that prints on Saturdays
// came from a market that trades on Saturdays whatever it is called.
func TestCalendarIsInferredFromTheBars(t *testing.T) {
	crypto := NewSeries("BTC-USD", continuousBars("2022-01-01", "2022-12-31", 30000))
	if got := CalendarOf(crypto); got != CalendarContinuous {
		t.Errorf("a series with weekend bars is %q, want %q", got, CalendarContinuous)
	}
	equity := NewSeries("SPY", cleanBars("2022-01-03", "2022-12-30", 400))
	if got := CalendarOf(equity); got != CalendarUSEquity {
		t.Errorf("a weekday-only series is %q, want %q", got, CalendarUSEquity)
	}

	// A crypto ticker over a weekday-only file cannot be annualised by 365:
	// there are not 365 bars a year in it.
	mislabelled := NewSeries("BTC-USD", cleanBars("2022-01-03", "2022-12-30", 30000))
	if got := CalendarOf(mislabelled); got != CalendarUSEquity {
		t.Errorf("the dates should overrule the ticker, got %q", got)
	}
	// And with too little history to judge, the ticker is all there is.
	short := NewSeries("BTC-USD", continuousBars("2022-01-01", "2022-01-05", 30000))
	if got := CalendarOf(short); got != CalendarContinuous {
		t.Errorf("a series too short to judge should fall back to the ticker, got %q", got)
	}
	if got := CalendarOf(nil); got != CalendarUSEquity {
		t.Errorf("a nil series is %q, want %q", got, CalendarUSEquity)
	}
}

func TestCalendarForSymbol(t *testing.T) {
	for sym, want := range map[string]Calendar{
		"BTC-USD":  CalendarContinuous,
		"eth-usd":  CalendarContinuous,
		"EURUSD=X": CalendarFX,
		"SPY":      CalendarUSEquity,
		"BRK-B":    CalendarUSEquity,
		"^GSPC":    CalendarUSEquity,
		"BITCOIN":  CalendarContinuous, // resolved through the alias table
	} {
		if got := CalendarForSymbol(sym); got != want {
			t.Errorf("%s is %q, want %q", sym, got, want)
		}
	}
}

// A mixed universe takes the widest calendar, because the equity curve is
// marked on the union of the sessions and the divisor describes the sampling.
func TestMixedUniverseTakesTheWidestCalendar(t *testing.T) {
	cal, seen := CalendarForSymbols([]string{"SPY", "BTC-USD", "AAPL"})
	if cal != CalendarContinuous {
		t.Errorf("mixed universe annualises on %q, want %q", cal, CalendarContinuous)
	}
	if len(seen) != 2 {
		t.Fatalf("expected both calendars to be reported, got %v", seen)
	}
	// Narrowest first, so two identical runs describe themselves the same way.
	if seen[0] != CalendarUSEquity || seen[1] != CalendarContinuous {
		t.Errorf("calendars listed as %v, want [us_equity continuous]", seen)
	}

	only, seenOne := CalendarForSymbols([]string{"SPY", "AAPL"})
	if only != CalendarUSEquity || len(seenOne) != 1 {
		t.Errorf("a single-calendar universe reported %q %v", only, seenOne)
	}
	// An empty universe still has to name something rather than nothing.
	empty, seenNone := CalendarForSymbols(nil)
	if empty != CalendarUSEquity || len(seenNone) != 1 {
		t.Errorf("an empty universe reported %q %v", empty, seenNone)
	}
}

func TestCalendarForSeriesReadsTheBars(t *testing.T) {
	series := map[string]*Series{
		"SPY":     NewSeries("SPY", cleanBars("2022-01-03", "2022-12-30", 400)),
		"BTC-USD": NewSeries("BTC-USD", continuousBars("2022-01-01", "2022-12-31", 30000)),
	}
	cal, seen := CalendarForSeries(series)
	if cal != CalendarContinuous || len(seen) != 2 {
		t.Errorf("mixed loaded universe reported %q %v", cal, seen)
	}
}

// The annualisation factor for daily bars is the session count, and for
// intraday bars the session count times how many fit in a session. Getting the
// second wrong is how an intraday Sharpe comes out five times too large.
func TestPeriodsPerYearOnEachCalendar(t *testing.T) {
	for _, tc := range []struct {
		iv   Interval
		cal  Calendar
		want float64
	}{
		{Interval1d, CalendarUSEquity, 252},
		{Interval1d, CalendarContinuous, 365},
		{Interval1d, CalendarFX, 261},
		{Interval1h, CalendarUSEquity, 252 * 390 / 60},
		{Interval1h, CalendarContinuous, 365 * 24},
		{Interval5m, CalendarUSEquity, 252 * 390 / 5},
		{Interval5m, CalendarContinuous, 365 * 1440 / 5},
		// A week is a week and a month is a month on every calendar.
		{Interval1wk, CalendarContinuous, 52},
		{Interval1mo, CalendarContinuous, 12},
	} {
		if got := tc.iv.PeriodsPerYearOn(tc.cal); got != tc.want {
			t.Errorf("%s on %s is %v, want %v", tc.iv, tc.cal, got, tc.want)
		}
	}
	// The old US-equity figures must be exactly what they always were.
	for iv, want := range map[Interval]float64{
		Interval1m: 252 * 390, Interval5m: 252 * 78, Interval1h: 252 * 6.5,
		Interval1d: 252, Interval1wk: 52, Interval1mo: 12,
	} {
		if got := iv.PeriodsPerYear(); got != want {
			t.Errorf("%s defaults to %v, want the unchanged %v", iv, got, want)
		}
	}
}

func TestParseCalendar(t *testing.T) {
	for in, want := range map[string]Calendar{
		"": "", "us_equity": CalendarUSEquity, "NYSE": CalendarUSEquity,
		"crypto": CalendarContinuous, "24/7": CalendarContinuous,
		"fx": CalendarFX, "Forex": CalendarFX,
	} {
		got, err := ParseCalendar(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q parsed to %q, want %q", in, got, want)
		}
	}
	if _, err := ParseCalendar("lunar"); err == nil {
		t.Error("an unknown calendar should be rejected rather than defaulted")
	}
}

// The auditor and the annualisation factor must agree about what continuous
// means. If they drift apart a series is audited as crypto and scored as
// equity, which is the worst of both.
func TestAuditAndCalendarAgreeOnContinuous(t *testing.T) {
	s := NewSeries("BTC-USD", continuousBars("2022-01-01", "2022-12-31", 30000))
	if CalendarOf(s) != CalendarContinuous {
		t.Fatal("the fixture is not classified as continuous")
	}
	if _, ok := findingOf(Audit(s), KindContinuous); !ok {
		t.Error("the auditor did not stand its calendar checks down on the same series")
	}
}

// A generated crypto series has to keep a crypto calendar, or the offline path
// manufactures a market that does not exist.
func TestSyntheticCryptoTradesEveryDay(t *testing.T) {
	p := NewSyntheticProvider()
	s, err := p.Fetch(t.Context(), "BTC-USD", "2022-01-01", "2022-12-31")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := CalendarOf(s); got != CalendarContinuous {
		t.Errorf("synthetic BTC-USD is on the %q calendar", got)
	}
	if n := len(s.Bars); n < 360 {
		t.Errorf("a year of continuous bars is %d, want 365 or so", n)
	}
	eq, err := p.Fetch(t.Context(), "SPY", "2022-01-01", "2022-12-31")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := CalendarOf(eq); got != CalendarUSEquity {
		t.Errorf("synthetic SPY is on the %q calendar", got)
	}
	if weekendBars(eq.Bars) != 0 {
		t.Error("a synthetic equity series should not print at weekends")
	}
}
