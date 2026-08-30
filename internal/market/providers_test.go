package market

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubProvider serves a fixed answer, so chain behaviour can be tested without
// touching the network.
type stubProvider struct {
	name    string
	series  *Series
	err     error
	calls   int
	failFor map[string]bool
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Fetch(ctx context.Context, symbol string, from, to Day) (*Series, error) {
	s.calls++
	if s.failFor != nil && s.failFor[symbol] {
		return nil, errors.New("deliberate failure for " + symbol)
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.series, nil
}
func (s *stubProvider) Search(ctx context.Context, q string) ([]Quote, error) { return nil, nil }

func oneBarSeries(sym string) *Series {
	return NewSeries(sym, []Bar{{Date: "2024-01-02", Open: 1, High: 1, Low: 1, Close: 1, AdjClose: 1}})
}

func TestChainFallsThroughPerSymbol(t *testing.T) {
	// The realistic case: a vendor that works for most names and fails for
	// one. The chain must recover exactly that name, not refetch everything.
	first := &stubProvider{name: "first", series: oneBarSeries("AAPL"),
		failFor: map[string]bool{"MSFT": true}}
	second := &stubProvider{name: "second", series: oneBarSeries("MSFT")}

	var fellBack []string
	c := NewChain(first, second)
	c.OnFallback = func(symbol, failed, next string, err error) {
		fellBack = append(fellBack, symbol+":"+failed+"->"+next)
	}

	if _, err := c.Fetch(context.Background(), "AAPL", "", ""); err != nil {
		t.Fatalf("AAPL should come from the first provider: %v", err)
	}
	if second.calls != 0 {
		t.Error("the second provider should not be consulted when the first succeeds")
	}
	if _, err := c.Fetch(context.Background(), "MSFT", "", ""); err != nil {
		t.Fatalf("MSFT should fall through to the second provider: %v", err)
	}
	if len(fellBack) != 1 || !strings.HasPrefix(fellBack[0], "MSFT:first->second") {
		t.Errorf("fallback not reported correctly: %v", fellBack)
	}
}

func TestChainReportsTheFirstErrorWhenAllFail(t *testing.T) {
	a := &stubProvider{name: "a", err: errors.New("a is down")}
	b := &stubProvider{name: "b", err: errors.New("b is down")}
	_, err := NewChain(a, b).Fetch(context.Background(), "X", "", "")
	if err == nil {
		t.Fatal("expected an error when every provider fails")
	}
	if !strings.Contains(err.Error(), "a is down") {
		t.Errorf("should surface the first error: %v", err)
	}
}

func TestChainStopsOnCancelledContext(t *testing.T) {
	a := &stubProvider{name: "a", err: errors.New("down")}
	b := &stubProvider{name: "b", series: oneBarSeries("X")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewChain(a, b).Fetch(ctx, "X", "", ""); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context should stop the chain, got %v", err)
	}
	if b.calls != 0 {
		t.Error("a cancelled chain should not keep trying providers")
	}
}

func TestChainIgnoresNilProviders(t *testing.T) {
	c := NewChain(nil, &stubProvider{name: "real", series: oneBarSeries("X")}, nil)
	if len(c.Providers) != 1 {
		t.Fatalf("nil providers should be dropped, got %d", len(c.Providers))
	}
	if c.Name() != "real" {
		t.Errorf("name: got %q", c.Name())
	}
}

func TestChainNameListsMembers(t *testing.T) {
	c := NewChain(&stubProvider{name: "yahoo"}, &stubProvider{name: "stooq"})
	if c.Name() != "yahoo+stooq" {
		t.Errorf("got %q, want yahoo+stooq", c.Name())
	}
	if NewChain().Name() != "none" {
		t.Error("an empty chain should say so")
	}
}

func TestCSVProviderReadsAVendorExport(t *testing.T) {
	dir := t.TempDir()
	// Deliberately awkward: mixed case header, a US date format, an adjusted
	// column under a different name, and a trailing blank line.
	os.WriteFile(filepath.Join(dir, "TEST.csv"), []byte(
		"Date,Open,High,Low,Close,Adj Close,Volume\n"+
			"01/02/2024,10,12,9,11,10.5,1000\n"+
			"01/03/2024,11,13,10,12,11.5,2000\n"+
			"\n"), 0o644)

	p := NewCSVProvider(dir)
	ser, err := p.Fetch(context.Background(), "TEST", "", "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(ser.Bars) != 2 {
		t.Fatalf("want 2 bars, got %d", len(ser.Bars))
	}
	b := ser.Bars[0]
	if b.Date != "2024-01-02" {
		t.Errorf("date: got %v, want 2024-01-02", b.Date)
	}
	if b.Close != 11 || b.AdjClose != 10.5 || b.Volume != 1000 {
		t.Errorf("bar parsed wrong: %+v", b)
	}
	if ser.Bars[1].Date <= ser.Bars[0].Date {
		t.Error("bars should be sorted oldest first")
	}
}

func TestCSVProviderFallsBackToRawCloseWithoutAnAdjustedColumn(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "NOADJ.csv"), []byte(
		"date,close\n2024-01-02,50\n2024-01-03,55\n"), 0o644)

	ser, err := NewCSVProvider(dir).Fetch(context.Background(), "NOADJ", "", "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// Without an adjusted close, raw stands in — so the split factor is 1 and
	// nothing downstream misprices.
	for _, b := range ser.Bars {
		if b.AdjClose != b.Close {
			t.Errorf("adjusted should mirror raw: %+v", b)
		}
		if b.SplitFactor() != 1 {
			t.Errorf("split factor should be 1: %v", b.SplitFactor())
		}
		if b.Open != b.Close || b.High != b.Close || b.Low != b.Close {
			t.Errorf("missing OHLC should fall back to close: %+v", b)
		}
	}
}

func TestCSVProviderMatchesFilenameVariants(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "brk_b.csv"), []byte("date,close\n2024-01-02,10\n"), 0o644)
	if _, err := NewCSVProvider(dir).Fetch(context.Background(), "BRK-B", "", ""); err != nil {
		t.Errorf("should match brk_b.csv for BRK-B: %v", err)
	}
	if _, err := NewCSVProvider(dir).Fetch(context.Background(), "NOPE", "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing symbol should be ErrNotFound, got %v", err)
	}
}

func TestCSVProviderSearchListsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"AAPL.csv", "MSFT.csv", "notes.txt"} {
		os.WriteFile(filepath.Join(dir, n), []byte("date,close\n2024-01-02,1\n"), 0o644)
	}
	res, err := NewCSVProvider(dir).Search(context.Background(), "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("only CSV files should be listed, got %d: %+v", len(res), res)
	}
	res, _ = NewCSVProvider(dir).Search(context.Background(), "aap")
	if len(res) != 1 || res[0].Symbol != "AAPL" {
		t.Errorf("search should filter: %+v", res)
	}
}

func TestParseFlexibleDayAcceptsVendorFormats(t *testing.T) {
	cases := map[string]Day{
		"2024-01-02":          "2024-01-02",
		"2024/01/02":          "2024-01-02",
		"20240102":            "2024-01-02",
		"2024-01-02T00:00:00": "2024-01-02",
		"2024-01-02 09:30:00": "2024-01-02",
		"1704153600":          "2024-01-02", // unix seconds
	}
	for in, want := range cases {
		got, err := parseFlexibleDay(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
	if _, err := parseFlexibleDay("not a date"); err == nil {
		t.Error("garbage should be rejected")
	}
}

func TestStooqSymbolMapping(t *testing.T) {
	cases := map[string]string{
		"AAPL":    "aapl.us",
		"^GSPC":   "^spx",
		"^IXIC":   "^ndq",
		"BTC-USD": "btcusd",
	}
	for in, want := range cases {
		if got := stooqSymbol(in); got != want {
			t.Errorf("stooqSymbol(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStooqParsesItsCSV(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "aapl.us") {
			t.Errorf("symbol not mapped in the request: %s", r.URL.RawQuery)
		}
		w.Write([]byte("Date,Open,High,Low,Close,Volume\n" +
			"2024-01-02,10,12,9,11,1000\n2024-01-03,11,13,10,12,2000\n"))
	}))
	defer srv.Close()

	p := NewStooqProvider()
	p.BaseURL = srv.URL
	ser, err := p.Fetch(context.Background(), "AAPL", "2024-01-01", "2024-02-01")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(ser.Bars) != 2 {
		t.Fatalf("want 2 bars, got %d", len(ser.Bars))
	}
	// Stooq has no adjusted close, so it mirrors raw.
	if ser.Bars[0].AdjClose != ser.Bars[0].Close {
		t.Errorf("adjusted should mirror raw: %+v", ser.Bars[0])
	}
}

func TestStooqReportsNotFoundForAnEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Date,Open,High,Low,Close,Volume\n"))
	}))
	defer srv.Close()
	p := NewStooqProvider()
	p.BaseURL = srv.URL
	if _, err := p.Fetch(context.Background(), "NOPE", "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}
