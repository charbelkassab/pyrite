package market

import (
	"testing"
	"time"
)

func TestParseIntervalAcceptsWhatPeopleWrite(t *testing.T) {
	cases := map[string]Interval{
		"1m": Interval1m, "1min": Interval1m, "minute": Interval1m,
		"5m": Interval5m, "15m": Interval15m, "30m": Interval30m,
		"1h": Interval1h, "60m": Interval1h, "hourly": Interval1h,
		"1d": Interval1d, "daily": Interval1d, "D": Interval1d,
		"1wk": Interval1wk, "weekly": Interval1wk,
		"1mo": Interval1mo, "monthly": Interval1mo,
		"": DefaultInterval,
	}
	for in, want := range cases {
		got, err := ParseInterval(in)
		if err != nil {
			t.Errorf("ParseInterval(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseInterval(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseInterval("fortnightly"); err == nil {
		t.Error("an unknown size should be refused")
	}
}

// Annualising 1-minute bars as though they were daily overstates a Sharpe
// ratio by about twenty times, in the flattering direction. These factors are
// the thing that stops that.
func TestPeriodsPerYearScalesWithBarSize(t *testing.T) {
	if got := Interval1d.PeriodsPerYear(); got != 252 {
		t.Errorf("daily: got %v, want 252", got)
	}
	if got := Interval1wk.PeriodsPerYear(); got != 52 {
		t.Errorf("weekly: got %v, want 52", got)
	}
	if got := Interval1mo.PeriodsPerYear(); got != 12 {
		t.Errorf("monthly: got %v, want 12", got)
	}
	// Intraday counts trading sessions times bars per session, not calendar
	// hours: a strategy is not exposed overnight the way it is intraday.
	if got := Interval1m.PeriodsPerYear(); got != 252*390 {
		t.Errorf("1m: got %v, want %v", got, 252*390.0)
	}
	if got := Interval1h.PeriodsPerYear(); got != 252*6.5 {
		t.Errorf("1h: got %v, want %v", got, 252*6.5)
	}
	// And they must be strictly ordered.
	prev := 0.0
	for _, name := range []Interval{Interval1mo, Interval1wk, Interval1d, Interval1h,
		Interval30m, Interval15m, Interval5m, Interval1m} {
		if p := name.PeriodsPerYear(); p <= prev {
			t.Errorf("%s has %v periods, not more than the coarser size's %v", name, p, prev)
		} else {
			prev = p
		}
	}
}

func TestIntradayClassification(t *testing.T) {
	for _, i := range []Interval{Interval1m, Interval5m, Interval15m, Interval30m, Interval1h} {
		if !i.Intraday() {
			t.Errorf("%s should be intraday", i)
		}
	}
	for _, i := range []Interval{Interval1d, Interval1wk, Interval1mo} {
		if i.Intraday() {
			t.Errorf("%s is not intraday", i)
		}
	}
}

func TestDayCarriesATimeWithoutBreakingOrder(t *testing.T) {
	// The whole reason Day is still a string: adding a time component must
	// not disturb any existing comparison.
	ordered := []Day{
		"2024-01-02",
		"2024-01-02T09:30",
		"2024-01-02T09:31",
		"2024-01-02T16:00",
		"2024-01-03",
		"2024-01-03T09:30",
	}
	for i := 1; i < len(ordered); i++ {
		if !(ordered[i-1] < ordered[i]) {
			t.Errorf("%q should sort before %q", ordered[i-1], ordered[i])
		}
	}
}

func TestDayDateAndIntraday(t *testing.T) {
	d := Day("2024-03-05T14:30")
	if !d.Intraday() {
		t.Error("a stamped Day is intraday")
	}
	if d.Date() != "2024-03-05" {
		t.Errorf("Date(): got %v", d.Date())
	}
	plain := Day("2024-03-05")
	if plain.Intraday() {
		t.Error("a bare date is not intraday")
	}
	if plain.Date() != plain {
		t.Errorf("Date() on a bare date should be itself, got %v", plain.Date())
	}
	if got := d.Time(); got.Hour() != 14 || got.Minute() != 30 {
		t.Errorf("Time(): got %v", got)
	}
	if got := plain.Time(); got.Hour() != 0 {
		t.Errorf("a bare date should be midnight, got %v", got)
	}
}

// An inclusive upper bound of a bare date excludes that day's own intraday
// bars, because the timestamp sorts after the date. The symptom is a range
// that silently drops its last session.
func TestEndOfDayCoversTheSessionsWithinIt(t *testing.T) {
	end := Day("2024-01-05")
	bar := Day("2024-01-05T15:59")
	if bar <= end {
		t.Fatal("this test is pointless if the bare date already covers the bar")
	}
	if !(bar <= end.EndOfDay()) {
		t.Errorf("EndOfDay should cover %v, got bound %v", bar, end.EndOfDay())
	}
	// It must not reach into the next day.
	if Day("2024-01-06T00:00") <= end.EndOfDay() {
		t.Error("EndOfDay reached into the following day")
	}
	// Already-stamped bounds and empty bounds are left alone.
	if got := Day("2024-01-05T10:00").EndOfDay(); got != "2024-01-05T10:00" {
		t.Errorf("a stamped bound should be unchanged, got %v", got)
	}
	if got := Day("").EndOfDay(); got != "" {
		t.Errorf("an empty bound should stay empty, got %v", got)
	}
}

func TestParseDayAcceptsStampsAndNormalises(t *testing.T) {
	cases := map[string]Day{
		"2024-01-02":                "2024-01-02",
		"2024-01-02T09:30":          "2024-01-02T09:30",
		"2024-01-02T09:30:00":       "2024-01-02T09:30",
		"2024-01-02T09:30:00Z":      "2024-01-02T09:30",
		"2024-01-02T09:30:00+00:00": "2024-01-02T09:30",
	}
	for in, want := range cases {
		got, err := ParseDay(in)
		if err != nil {
			t.Errorf("ParseDay(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseDay(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseDay("not a date"); err == nil {
		t.Error("garbage should be refused")
	}
}

func TestAddPreservesTheShape(t *testing.T) {
	if got := Day("2024-01-02").Add(1); got != "2024-01-03" {
		t.Errorf("a bare date should stay bare: %v", got)
	}
	if got := Day("2024-01-02T09:30").Add(1); got != "2024-01-03T09:30" {
		t.Errorf("a stamp should keep its time: %v", got)
	}
}

func TestAddInterval(t *testing.T) {
	if got := Day("2024-01-02T09:30").AddInterval(2, Interval15m); got != "2024-01-02T10:00" {
		t.Errorf("two 15m bars from 09:30 is 10:00, got %v", got)
	}
	if got := Day("2024-01-02T15:30").AddInterval(1, Interval1h); got != "2024-01-02T16:30" {
		t.Errorf("got %v", got)
	}
	if got := Day("2024-01-02").AddInterval(3, Interval1d); got != "2024-01-05" {
		t.Errorf("got %v", got)
	}
	// Backwards too, which is what warm-up needs.
	if got := Day("2024-01-02T10:00").AddInterval(-2, Interval30m); got != "2024-01-02T09:00" {
		t.Errorf("got %v", got)
	}
}

func TestNewStampIsUTC(t *testing.T) {
	loc := time.FixedZone("test", 5*3600)
	got := NewStamp(time.Date(2024, 1, 2, 14, 30, 0, 0, loc))
	if got != "2024-01-02T09:30" {
		t.Errorf("NewStamp should normalise to UTC, got %v", got)
	}
}

func TestResampleAggregatesCorrectly(t *testing.T) {
	// Buckets are clock-aligned: an hourly bar covers 14:00-15:00, which is
	// how every vendor defines one. The US session opening at 14:30 UTC
	// therefore produces a half-hour stub as its first hourly bar, and that
	// is correct rather than something to paper over.
	bars := []Bar{
		{Date: "2024-01-02T14:30", Open: 100, High: 102, Low: 99, Close: 101, AdjClose: 101, Volume: 10},
		{Date: "2024-01-02T14:45", Open: 101, High: 105, Low: 100, Close: 104, AdjClose: 104, Volume: 20},
		{Date: "2024-01-02T15:00", Open: 104, High: 106, Low: 98, Close: 99, AdjClose: 99, Volume: 30},
		{Date: "2024-01-02T15:15", Open: 99, High: 103, Low: 97, Close: 102, AdjClose: 102, Volume: 40},
		{Date: "2024-01-02T15:30", Open: 102, High: 110, Low: 101, Close: 109, AdjClose: 109, Volume: 50},
	}
	got := Resample(NewSeries("X", bars), Interval1h)
	if len(got.Bars) != 2 {
		t.Fatalf("two clock hours, got %d bars", len(got.Bars))
	}

	// 14:00 hour: the 14:30 and 14:45 bars.
	h := got.Bars[0]
	if h.Date != "2024-01-02T14:00" {
		t.Errorf("bucket stamp should be the hour's start: got %v", h.Date)
	}
	if h.Open != 100 || h.Close != 104 {
		t.Errorf("open/close should be the bucket's first and last: got %v/%v", h.Open, h.Close)
	}
	if h.High != 105 || h.Low != 99 {
		t.Errorf("extremes should span the bucket: got high %v low %v", h.High, h.Low)
	}
	if h.Volume != 30 {
		t.Errorf("volume should sum within the bucket: got %v", h.Volume)
	}

	// 15:00 hour: the 15:00, 15:15 and 15:30 bars.
	h2 := got.Bars[1]
	if h2.Date != "2024-01-02T15:00" {
		t.Errorf("second bucket stamp: got %v", h2.Date)
	}
	if h2.Open != 104 || h2.Close != 109 {
		t.Errorf("second bucket open/close: got %v/%v", h2.Open, h2.Close)
	}
	if h2.High != 110 || h2.Low != 97 {
		t.Errorf("second bucket extremes: got high %v low %v", h2.High, h2.Low)
	}
	if h2.Volume != 120 {
		t.Errorf("second bucket volume: got %v, want 120", h2.Volume)
	}

	// Buckets must stay ordered and never be dated later than their contents.
	if !(got.Bars[0].Date < got.Bars[1].Date) {
		t.Error("resampled bars are out of order")
	}
	if got.Bars[0].Date > bars[0].Date {
		t.Errorf("a bucket is dated after the first bar it summarises: %v > %v",
			got.Bars[0].Date, bars[0].Date)
	}
}

func TestResampleToDailyAndWeekly(t *testing.T) {
	var bars []Bar
	// Two sessions of four hourly bars each.
	for _, day := range []string{"2024-01-02", "2024-01-03"} {
		for i, hr := range []string{"14:30", "15:30", "16:30", "17:30"} {
			p := 100 + float64(i)
			bars = append(bars, Bar{
				Date: Day(day + "T" + hr), Open: p, High: p + 1, Low: p - 1,
				Close: p, AdjClose: p, Volume: 10,
			})
		}
	}
	daily := Resample(NewSeries("X", bars), Interval1d)
	if len(daily.Bars) != 2 {
		t.Fatalf("two sessions should give 2 daily bars, got %d", len(daily.Bars))
	}
	if daily.Bars[0].Date != "2024-01-02" {
		t.Errorf("a daily bar should be dated, not stamped: %v", daily.Bars[0].Date)
	}
	if daily.Bars[0].Volume != 40 {
		t.Errorf("daily volume: got %v, want 40", daily.Bars[0].Volume)
	}

	// Both days fall in the same ISO week.
	weekly := Resample(NewSeries("X", bars), Interval1wk)
	if len(weekly.Bars) != 1 {
		t.Fatalf("both sessions are in one week, got %d bars", len(weekly.Bars))
	}
	if weekly.Bars[0].Volume != 80 {
		t.Errorf("weekly volume: got %v, want 80", weekly.Bars[0].Volume)
	}
	// Weeks are stamped at their Monday.
	if weekly.Bars[0].Date != "2024-01-01" {
		t.Errorf("a week should be stamped at its Monday: got %v", weekly.Bars[0].Date)
	}
}

func TestResampleHandlesDegenerateInput(t *testing.T) {
	if got := Resample(nil, Interval1d); got != nil {
		t.Error("nil in, nil out")
	}
	empty := NewSeries("X", nil)
	if got := Resample(empty, Interval1d); len(got.Bars) != 0 {
		t.Error("empty in, empty out")
	}
	one := NewSeries("X", []Bar{{Date: "2024-01-02", Close: 10, AdjClose: 10}})
	if got := Resample(one, Interval1wk); len(got.Bars) != 1 {
		t.Errorf("a single bar should survive: got %d", len(got.Bars))
	}
}
