package market

import (
	"testing"
	"time"
)

func mkSeries(t *testing.T, dates ...string) *Series {
	t.Helper()
	bars := make([]Bar, 0, len(dates))
	for i, d := range dates {
		p := 100 + float64(i)
		bars = append(bars, Bar{
			Date: Day(d), Open: p, High: p + 1, Low: p - 1,
			Close: p, AdjClose: p, Volume: 1000,
		})
	}
	return NewSeries("TEST", bars)
}

func TestSeriesAsOfReturnsLastBarOnOrBefore(t *testing.T) {
	s := mkSeries(t, "2024-01-02", "2024-01-03", "2024-01-08")

	// An exact hit.
	if b, ok := s.AsOf("2024-01-03"); !ok || b.Date != "2024-01-03" {
		t.Errorf("exact lookup failed: %+v ok=%v", b, ok)
	}
	// A market holiday between bars must resolve backwards, not forwards.
	if b, ok := s.AsOf("2024-01-05"); !ok || b.Date != "2024-01-03" {
		t.Errorf("gap lookup should return 2024-01-03, got %+v", b)
	}
	// Before the first bar there is nothing to return.
	if _, ok := s.AsOf("2023-12-31"); ok {
		t.Error("a date before the first bar must not resolve")
	}
	// After the last bar the last bar stands.
	if b, ok := s.AsOf("2025-06-01"); !ok || b.Date != "2024-01-08" {
		t.Errorf("post-end lookup should return the last bar, got %+v", b)
	}
}

func TestSeriesHistoryExcludesFutureBars(t *testing.T) {
	s := mkSeries(t, "2024-01-02", "2024-01-03", "2024-01-04", "2024-01-05")
	h := s.History("2024-01-03", 10)
	if len(h) != 2 {
		t.Fatalf("history on 2024-01-03 should hold 2 bars, got %d", len(h))
	}
	for _, b := range h {
		if b.Date > "2024-01-03" {
			t.Errorf("history leaked a future bar: %s", b.Date)
		}
	}
	// A short window is truncated from the oldest end.
	if h := s.History("2024-01-05", 2); len(h) != 2 || h[0].Date != "2024-01-04" {
		t.Errorf("expected the two most recent bars, got %+v", h)
	}
}

func TestNewSeriesSortsAndDeduplicates(t *testing.T) {
	s := NewSeries("X", []Bar{
		{Date: "2024-01-03", Close: 3, AdjClose: 3},
		{Date: "2024-01-01", Close: 1, AdjClose: 1},
		{Date: "2024-01-03", Close: 99, AdjClose: 99}, // later wins
		{Date: "2024-01-02", Close: 2, AdjClose: 2},
	})
	if len(s.Bars) != 3 {
		t.Fatalf("expected 3 unique bars, got %d", len(s.Bars))
	}
	for i := 1; i < len(s.Bars); i++ {
		if s.Bars[i].Date <= s.Bars[i-1].Date {
			t.Fatalf("bars are not in ascending date order: %+v", s.Bars)
		}
	}
	if b, _ := s.At("2024-01-03"); b.Close != 99 {
		t.Errorf("the later duplicate should win, got close %v", b.Close)
	}
}

func TestTradingCalendarIsTheUnionNotTheIntersection(t *testing.T) {
	// A symbol that starts late must not truncate the calendar: the union
	// keeps every day either symbol traded.
	series := map[string]*Series{
		"OLD": mkSeries(t, "2024-01-02", "2024-01-03", "2024-01-04"),
		"NEW": mkSeries(t, "2024-01-04", "2024-01-05"),
	}
	days := TradingCalendar(series, "2024-01-01", "2024-01-31")
	want := []Day{"2024-01-02", "2024-01-03", "2024-01-04", "2024-01-05"}
	if len(days) != len(want) {
		t.Fatalf("expected %d days, got %d (%v)", len(want), len(days), days)
	}
	for i := range want {
		if days[i] != want[i] {
			t.Errorf("day %d: got %s, want %s", i, days[i], want[i])
		}
	}
}

func TestDayArithmeticAndParsing(t *testing.T) {
	d := Day("2024-02-28")
	if got := d.Add(1); got != "2024-02-29" {
		t.Errorf("leap day: got %s", got) // 2024 is a leap year
	}
	if got := d.Add(-59); got != "2023-12-31" {
		t.Errorf("year boundary: got %s", got)
	}
	if _, err := ParseDay("not-a-date"); err == nil {
		t.Error("expected an error for an unparseable date")
	}
	if got, err := ParseDay(" 2024-03-01 "); err != nil || got != "2024-03-01" {
		t.Errorf("surrounding whitespace should be tolerated, got %q err=%v", got, err)
	}
	if got := NewDay(time.Date(2024, 7, 4, 23, 30, 0, 0, time.UTC)); got != "2024-07-04" {
		t.Errorf("NewDay dropped the wrong component: %s", got)
	}
}

func TestRealTickersWinOverFriendlyAliases(t *testing.T) {
	// DOW is Dow Inc and GOLD is Barrick Gold. Aliasing either to an index or
	// a futures contract would silently trade the wrong instrument.
	for _, sym := range []string{"DOW", "GOLD", "BTC", "ETH"} {
		if got := NormalizeSymbol(sym); got != sym {
			t.Errorf("%s is a real ticker and must not be rewritten, got %q", sym, got)
		}
	}
	// The spelled-out forms are not tickers, so they may alias.
	for in, want := range map[string]string{
		"dow jones": "^DJI", "djia": "^DJI", "bitcoin": "BTC-USD", "gold price": "GC=F",
	} {
		if got := NormalizeSymbol(in); got != want {
			t.Errorf("NormalizeSymbol(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeSymbolMapsFriendlyAliases(t *testing.T) {
	cases := map[string]string{
		"aapl":    "AAPL",
		" msft ":  "MSFT",
		"S&P 500": "^GSPC",
		"nasdaq":  "^IXIC",
		"bitcoin": "BTC-USD",
	}
	for in, want := range cases {
		if got := NormalizeSymbol(in); got != want {
			t.Errorf("NormalizeSymbol(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFundamentalsUsePointInTimeShareCounts(t *testing.T) {
	f, err := LoadFundamentals("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Apple's 4:1 split in August 2020 must show as a step change, otherwise
	// every pre-split market cap is understated by a factor of four.
	before, ok1 := f.SharesOutstanding("AAPL", "2020-06-01")
	after, ok2 := f.SharesOutstanding("AAPL", "2020-12-01")
	if !ok1 || !ok2 {
		t.Fatal("expected AAPL share counts on both sides of the split")
	}
	if ratio := after / before; ratio < 3.5 || ratio > 4.5 {
		t.Errorf("expected roughly a 4x step across the split, got %.2fx", ratio)
	}

	// A date before the earliest row falls back to that row rather than zero.
	if v, ok := f.SharesOutstanding("AAPL", "1990-01-01"); !ok || v <= 0 {
		t.Errorf("pre-history lookup should extrapolate backwards, got %v ok=%v", v, ok)
	}
	// An unknown symbol reports absence rather than guessing.
	if _, ok := f.SharesOutstanding("NOTREAL", "2024-01-01"); ok {
		t.Error("an unknown symbol must not report a share count")
	}
}

func TestRankByMarketCapOrdersLargestFirst(t *testing.T) {
	f, err := LoadFundamentals("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Construct prices so the ordering is known: AAPL has ~15.4bn shares and
	// MSFT ~7.4bn in 2024, so equal prices must put AAPL ahead.
	series := map[string]*Series{
		"AAPL": NewSeries("AAPL", []Bar{{Date: "2024-06-03", Close: 200, AdjClose: 200}}),
		"MSFT": NewSeries("MSFT", []Bar{{Date: "2024-06-03", Close: 200, AdjClose: 200}}),
	}
	ranks := f.RankByMarketCap("2024-06-03", series)
	if len(ranks) != 2 {
		t.Fatalf("expected 2 ranked symbols, got %d", len(ranks))
	}
	if ranks[0].Symbol != "AAPL" {
		t.Errorf("AAPL has more shares outstanding so should rank first, got %s", ranks[0].Symbol)
	}
	if ranks[0].Rank != 1 || ranks[1].Rank != 2 {
		t.Errorf("ranks should be 1 and 2, got %d and %d", ranks[0].Rank, ranks[1].Rank)
	}
	if ranks[0].MarketCap <= ranks[1].MarketCap {
		t.Error("market caps are not in descending order")
	}
}

func TestRankByMarketCapSkipsSymbolsBeforeTheirFirstBar(t *testing.T) {
	f, _ := LoadFundamentals("")
	series := map[string]*Series{
		// Listed after the ranking date: must not appear at all.
		"AAPL": NewSeries("AAPL", []Bar{{Date: "2024-06-03", Close: 200, AdjClose: 200}}),
	}
	if ranks := f.RankByMarketCap("2020-01-02", series); len(ranks) != 0 {
		t.Errorf("a symbol with no bar on or before the date must be skipped, got %+v", ranks)
	}
}

func TestResolveUniverseAcceptsKeysAndLists(t *testing.T) {
	if syms := ResolveUniverse("megacap"); len(syms) < 20 {
		t.Errorf("megacap should expand to a large list, got %d", len(syms))
	}
	got := ResolveUniverse("aapl, msft ,aapl")
	if len(got) != 3 || got[0] != "AAPL" {
		t.Errorf("comma list should normalise but preserve order, got %v", got)
	}
	if deduped := DedupeSymbols(got); len(deduped) != 2 {
		t.Errorf("DedupeSymbols should collapse the repeat, got %v", deduped)
	}
	if syms := ResolveUniverse(""); syms != nil {
		t.Errorf("empty input should yield nothing, got %v", syms)
	}
}

func TestSyntheticProviderIsDeterministic(t *testing.T) {
	p := NewSyntheticProvider()
	a, err := p.Fetch(t.Context(), "AAPL", "2022-01-03", "2022-06-30")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	b, err := p.Fetch(t.Context(), "AAPL", "2022-01-03", "2022-06-30")
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if len(a.Bars) != len(b.Bars) {
		t.Fatalf("bar counts differ between runs: %d vs %d", len(a.Bars), len(b.Bars))
	}
	for i := range a.Bars {
		if a.Bars[i] != b.Bars[i] {
			t.Fatalf("synthetic data is not reproducible at bar %d", i)
		}
	}
	// Weekends must never appear.
	for _, bar := range a.Bars {
		if wd := bar.Date.Time().Weekday(); wd == time.Saturday || wd == time.Sunday {
			t.Fatalf("weekend bar generated: %s", bar.Date)
		}
	}
}

func TestBuiltInUniversesAreWellFormed(t *testing.T) {
	// A malformed universe is silent: a lower-case or duplicated entry costs a
	// wasted fetch and, for ranking strategies, a double-counted symbol.
	for key, u := range Universes {
		if len(u.Symbols) == 0 {
			t.Errorf("universe %q is empty", key)
		}
		seen := map[string]bool{}
		for _, s := range u.Symbols {
			if s != NormalizeSymbol(s) {
				t.Errorf("universe %q: %q is not in normalised form (want %q)", key, s, NormalizeSymbol(s))
			}
			if seen[s] {
				t.Errorf("universe %q: %q appears twice", key, s)
			}
			seen[s] = true
		}
		if u.Key != key {
			t.Errorf("universe %q has mismatched Key %q", key, u.Key)
		}
	}
}

func TestUSLargeIsBroadAndCoversTheMegaCaps(t *testing.T) {
	broad := ResolveUniverse("us-large")
	if len(broad) < 200 {
		t.Errorf("us-large should span the wider market, got %d symbols", len(broad))
	}
	have := map[string]bool{}
	for _, s := range broad {
		have[s] = true
	}
	for _, s := range ResolveUniverse("megacap") {
		if !have[s] {
			t.Errorf("us-large is missing mega cap %s", s)
		}
	}
}
