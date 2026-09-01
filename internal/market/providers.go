package market

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Chain tries several providers in order, falling through on a per-symbol
// failure.
//
// This is not redundancy for its own sake. Free endpoints are unreliable in a
// specific way: they work for most symbols and quietly 401 or 404 on some, and
// the failure is per symbol rather than global. Dropping that name from a
// forty-symbol universe silently changes the backtest. Trying the next vendor
// for exactly the names that failed is the honest response.
type Chain struct {
	Providers []Provider
	// OnFallback, when set, is called each time a provider is skipped, so the
	// run can record which vendor actually supplied which symbol.
	OnFallback func(symbol, failed, next string, err error)
}

// NewChain builds a fallback chain. Nil entries are ignored so callers can
// pass optional providers without guarding each one.
func NewChain(providers ...Provider) *Chain {
	out := make([]Provider, 0, len(providers))
	for _, p := range providers {
		if p != nil {
			out = append(out, p)
		}
	}
	return &Chain{Providers: out}
}

// Name lists the chain members, so the manifest records what could have
// served a run rather than only what did.
func (c *Chain) Name() string {
	if len(c.Providers) == 0 {
		return "none"
	}
	names := make([]string, 0, len(c.Providers))
	for _, p := range c.Providers {
		names = append(names, p.Name())
	}
	return strings.Join(names, "+")
}

// Fetch tries each provider until one returns data.
func (c *Chain) Fetch(ctx context.Context, symbol string, from, to Day) (*Series, error) {
	if len(c.Providers) == 0 {
		return nil, fmt.Errorf("no data provider is configured")
	}
	var firstErr error
	for i, p := range c.Providers {
		ser, err := p.Fetch(ctx, symbol, from, to)
		if err == nil && ser != nil && len(ser.Bars) > 0 {
			return ser, nil
		}
		if err == nil {
			err = fmt.Errorf("%w: %s", ErrNotFound, symbol)
		}
		if firstErr == nil {
			firstErr = err
		}
		// A cancelled context is not a provider failure, and retrying every
		// vendor against a dead context wastes the user's time twice over.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if c.OnFallback != nil && i+1 < len(c.Providers) {
			c.OnFallback(symbol, p.Name(), c.Providers[i+1].Name(), err)
		}
	}
	return nil, firstErr
}

// SupportedIntervals is the union of what the chain's members can serve.
func (c *Chain) SupportedIntervals() []Interval {
	seen := map[Interval]bool{Interval1d: true}
	for _, p := range c.Providers {
		ip, ok := p.(IntervalProvider)
		if !ok {
			continue
		}
		for _, iv := range ip.SupportedIntervals() {
			seen[iv] = true
		}
	}
	out := make([]Interval, 0, len(seen))
	for _, iv := range IntervalNames() {
		if seen[Interval(iv)] {
			out = append(out, Interval(iv))
		}
	}
	return out
}

// FetchInterval tries each member that can serve the requested bar size.
//
// Members that cannot are skipped rather than counted as failures: a
// daily-only vendor sitting behind an intraday one in the chain is a normal
// configuration, not an error, and reporting it as one would bury the real
// failure when there is one.
func (c *Chain) FetchInterval(ctx context.Context, symbol string, from, to Day, iv Interval) (*Series, error) {
	if iv == "" || iv == Interval1d {
		return c.Fetch(ctx, symbol, from, to)
	}

	var capable []Provider
	for _, p := range c.Providers {
		if SupportsInterval(p, iv) {
			capable = append(capable, p)
		}
	}
	if len(capable) == 0 {
		return nil, fmt.Errorf("none of %s serves %s bars", c.Name(), iv)
	}

	var firstErr error
	for i, p := range capable {
		ser, err := fetchAt(ctx, p, symbol, from, to, iv)
		if err == nil && ser != nil && len(ser.Bars) > 0 {
			return ser, nil
		}
		if err == nil {
			err = fmt.Errorf("%w: %s at %s", ErrNotFound, symbol, iv)
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if c.OnFallback != nil && i+1 < len(capable) {
			c.OnFallback(symbol, p.Name(), capable[i+1].Name(), err)
		}
	}
	return nil, firstErr
}

// Search asks the first provider that can answer.
func (c *Chain) Search(ctx context.Context, query string) ([]Quote, error) {
	var firstErr error
	for _, p := range c.Providers {
		res, err := p.Search(ctx, query)
		if err == nil && len(res) > 0 {
			return res, nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// ---------------------------------------------------------------------------

// StooqProvider reads daily bars from Stooq's CSV endpoint.
//
// Keyless like Yahoo, and useful precisely because it fails differently: when
// Yahoo rate-limits or drops a symbol, Stooq usually still has it. It carries
// no adjusted close, so adjusted and raw are the same series here — dividends
// are not reinvested for symbols served this way, which the chain records.
type StooqProvider struct {
	HTTP    *http.Client
	BaseURL string
}

// NewStooqProvider builds a Stooq client.
func NewStooqProvider() *StooqProvider {
	return &StooqProvider{
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		BaseURL: "https://stooq.com",
	}
}

func (s *StooqProvider) Name() string { return "stooq" }

// stooqSymbol maps a ticker to Stooq's naming, which suffixes US listings and
// spells indices differently.
func stooqSymbol(symbol string) string {
	sym := strings.ToLower(strings.TrimSpace(symbol))
	switch sym {
	case "^gspc":
		return "^spx"
	case "^ixic":
		return "^ndq"
	case "^dji":
		return "^dji"
	}
	if strings.HasPrefix(sym, "^") || strings.Contains(sym, ".") {
		return sym
	}
	if strings.HasSuffix(sym, "-usd") {
		// Crypto pairs: BTC-USD becomes btcusd.
		return strings.ReplaceAll(sym, "-", "")
	}
	return sym + ".us"
}

// Fetch retrieves daily bars.
func (s *StooqProvider) Fetch(ctx context.Context, symbol string, from, to Day) (*Series, error) {
	if from == "" {
		from = "1970-01-01"
	}
	if to == "" {
		to = NewDay(time.Now())
	}
	url := fmt.Sprintf("%s/q/d/l/?s=%s&d1=%s&d2=%s&i=d",
		s.BaseURL, stooqSymbol(symbol),
		strings.ReplaceAll(string(from), "-", ""),
		strings.ReplaceAll(string(to), "-", ""))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stooq returned %s for %s", resp.Status, symbol)
	}

	bars, err := parseOHLCVCSV(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("stooq %s: %w", symbol, err)
	}
	if len(bars) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, symbol)
	}
	return NewSeries(NormalizeSymbol(symbol), bars), nil
}

// Search is not offered by Stooq's public endpoints.
func (s *StooqProvider) Search(ctx context.Context, query string) ([]Quote, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------

// CSVProvider reads bars from a directory of CSV files, one per symbol.
//
// This is the escape hatch that makes every other data question answerable:
// point it at a vendor export, a database dump, or a hand-built file, and the
// rest of the tool works unchanged. It is also the only provider that can hold
// delisted securities, which no free live endpoint will ever serve.
type CSVProvider struct {
	Dir string
}

// NewCSVProvider reads from dir. Files are matched case-insensitively as
// SYMBOL.csv.
func NewCSVProvider(dir string) *CSVProvider { return &CSVProvider{Dir: dir} }

func (c *CSVProvider) Name() string { return "csv" }

// Fetch loads and parses one symbol's file.
func (c *CSVProvider) Fetch(ctx context.Context, symbol string, from, to Day) (*Series, error) {
	path, err := c.pathFor(symbol)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, symbol)
	}
	defer f.Close()

	bars, err := parseOHLCVCSV(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(bars) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, symbol)
	}
	return NewSeries(NormalizeSymbol(symbol), bars), nil
}

// pathFor resolves a symbol to a file, tolerating case and the "-" / "_"
// substitution filesystems and tickers disagree about.
func (c *CSVProvider) pathFor(symbol string) (string, error) {
	if c.Dir == "" {
		return "", fmt.Errorf("no CSV directory configured")
	}
	sym := NormalizeSymbol(symbol)
	candidates := []string{
		sym + ".csv",
		strings.ToLower(sym) + ".csv",
		strings.ReplaceAll(sym, "-", "_") + ".csv",
		strings.ReplaceAll(strings.ToLower(sym), "-", "_") + ".csv",
	}
	for _, name := range candidates {
		p := filepath.Join(c.Dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: no file for %s in %s", ErrNotFound, sym, c.Dir)
}

// Search lists the symbols the directory holds.
func (c *CSVProvider) Search(ctx context.Context, query string) ([]Quote, error) {
	if c.Dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(c.Dir)
	if err != nil {
		return nil, nil
	}
	q := strings.ToUpper(strings.TrimSpace(query))
	var out []Quote
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".csv") {
			continue
		}
		sym := strings.ToUpper(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
		if q == "" || strings.Contains(sym, q) {
			out = append(out, Quote{Symbol: sym, Name: sym})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	if len(out) > 20 {
		out = out[:20]
	}
	return out, nil
}

// ---------------------------------------------------------------------------

// ReadBarsCSV parses a vendor CSV into bars, in date order and with nothing
// removed.
//
// It exists for the auditor, which has to see the rows as they were written.
// NewSeries de-duplicates as it builds, so by the time a file has become a
// Series the one defect nobody can recover from — two contradictory prices
// for the same session — has already been tidied silently away.
func ReadBarsCSV(r io.Reader) ([]Bar, error) { return parseOHLCVCSV(r) }

// parseOHLCVCSV reads a header row and then bars, tolerating the column names
// and orders that different vendors use.
//
// Being liberal here is the whole value of the CSV provider: the point is that
// someone can drop in a file from wherever their data actually lives, and
// insisting on one exact schema would defeat that.
func parseOHLCVCSV(r io.Reader) ([]Bar, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true

	records, err := cr.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) < 2 {
		return nil, nil
	}

	idx := map[string]int{}
	for i, name := range records[0] {
		key := strings.ToLower(strings.TrimSpace(name))
		key = strings.ReplaceAll(key, " ", "")
		key = strings.ReplaceAll(key, "_", "")
		switch key {
		case "date", "timestamp", "time", "datetime":
			idx["date"] = i
		case "open":
			idx["open"] = i
		case "high":
			idx["high"] = i
		case "low":
			idx["low"] = i
		case "close":
			idx["close"] = i
		case "adjclose", "adjustedclose", "adjustedclosing", "closeadj":
			idx["adjclose"] = i
		case "volume", "vol":
			idx["volume"] = i
		}
	}
	if _, ok := idx["date"]; !ok {
		return nil, errors.New("no date column: expected one of date, timestamp, time")
	}
	if _, ok := idx["close"]; !ok {
		return nil, errors.New("no close column")
	}

	get := func(rec []string, key string) float64 {
		i, ok := idx[key]
		if !ok || i >= len(rec) {
			return 0
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(rec[i]), 64)
		if err != nil {
			return 0
		}
		return v
	}

	bars := make([]Bar, 0, len(records)-1)
	for _, rec := range records[1:] {
		di := idx["date"]
		if di >= len(rec) {
			continue
		}
		day, err := parseFlexibleDay(strings.TrimSpace(rec[di]))
		if err != nil {
			continue // a footer row or a blank line, not a fatal error
		}
		b := Bar{
			Date:   day,
			Open:   get(rec, "open"),
			High:   get(rec, "high"),
			Low:    get(rec, "low"),
			Close:  get(rec, "close"),
			Volume: get(rec, "volume"),
		}
		if b.Close <= 0 {
			continue
		}
		b.AdjClose = get(rec, "adjclose")
		if b.AdjClose <= 0 {
			// No adjusted column: the raw close stands in. Dividends are then
			// not reinvested for this symbol, which understates total return
			// for anything that pays one.
			b.AdjClose = b.Close
		}
		if b.Open <= 0 {
			b.Open = b.Close
		}
		if b.High <= 0 {
			b.High = b.Close
		}
		if b.Low <= 0 {
			b.Low = b.Close
		}
		bars = append(bars, b)
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].Date < bars[j].Date })
	return bars, nil
}

// parseFlexibleDay accepts the date formats vendors actually emit.
func parseFlexibleDay(s string) (Day, error) {
	if s == "" {
		return "", errors.New("empty date")
	}
	// A bare unix timestamp.
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 100000000 {
		return NewDay(time.Unix(n, 0).UTC()), nil
	}
	// Trim a time component if one is present.
	if i := strings.IndexAny(s, "T "); i > 0 {
		s = s[:i]
	}
	for _, layout := range []string{"2006-01-02", "2006/01/02", "01/02/2006", "02/01/2006", "20060102"} {
		if t, err := time.Parse(layout, s); err == nil {
			return NewDay(t), nil
		}
	}
	return "", fmt.Errorf("unrecognised date %q", s)
}
