package market

import (
	"embed"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed assets/shares_outstanding.csv
var fundamentalsFS embed.FS

// sharesRow is one piecewise-constant share count observation.
type sharesRow struct {
	From   Day
	Shares float64
}

// Fundamentals holds the point-in-time share counts used for market-cap
// ranking. See data/fundamentals/shares_outstanding.csv for the substantial
// caveats that come with this data.
type Fundamentals struct {
	mu     sync.RWMutex
	shares map[string][]sharesRow // sorted ascending by From
	names  map[string]string
	// source records where the table was loaded from, for display in the UI.
	source string
}

// LoadFundamentals reads the embedded share-count table, then overlays a
// user-supplied override file at dir/shares_outstanding.csv if present.
func LoadFundamentals(dir string) (*Fundamentals, error) {
	f := &Fundamentals{
		shares: map[string][]sharesRow{},
		names:  map[string]string{},
		source: "bundled",
	}

	b, err := fundamentalsFS.ReadFile("assets/shares_outstanding.csv")
	if err != nil {
		return nil, fmt.Errorf("read bundled fundamentals: %w", err)
	}
	if err := f.parse(strings.NewReader(string(b))); err != nil {
		return nil, fmt.Errorf("parse bundled fundamentals: %w", err)
	}

	if dir != "" {
		override := filepath.Join(dir, "shares_outstanding.csv")
		if ob, err := os.ReadFile(override); err == nil {
			// A user override replaces the bundled table entirely, rather
			// than merging, so results stay explainable.
			g := &Fundamentals{shares: map[string][]sharesRow{}, names: map[string]string{}}
			if err := g.parse(strings.NewReader(string(ob))); err != nil {
				return nil, fmt.Errorf("parse override %s: %w", override, err)
			}
			g.source = override
			return g, nil
		}
	}
	return f, nil
}

func (f *Fundamentals) parse(r io.Reader) error {
	// Strip comment lines before handing to encoding/csv so that quoted
	// company names still parse correctly.
	cr := csv.NewReader(r)
	cr.Comment = '#'
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true

	records, err := cr.ReadAll()
	if err != nil {
		return err
	}
	for i, rec := range records {
		if len(rec) < 3 {
			continue
		}
		sym := strings.ToUpper(strings.TrimSpace(rec[0]))
		if sym == "" || strings.EqualFold(sym, "symbol") {
			continue // header
		}
		day, err := ParseDay(rec[1])
		if err != nil {
			return fmt.Errorf("row %d (%s): %w", i+1, sym, err)
		}
		shares, err := strconv.ParseFloat(strings.TrimSpace(rec[2]), 64)
		if err != nil {
			return fmt.Errorf("row %d (%s): bad share count %q", i+1, sym, rec[2])
		}
		if shares <= 0 {
			continue
		}
		f.shares[sym] = append(f.shares[sym], sharesRow{From: day, Shares: shares})
		if len(rec) >= 4 && strings.TrimSpace(rec[3]) != "" {
			f.names[sym] = strings.TrimSpace(rec[3])
		}
	}
	for sym := range f.shares {
		rows := f.shares[sym]
		sort.Slice(rows, func(i, j int) bool { return rows[i].From < rows[j].From })
		f.shares[sym] = rows
	}
	return nil
}

// Source reports where the table came from.
func (f *Fundamentals) Source() string { return f.source }

// Symbols lists every ticker with share-count data, sorted.
func (f *Fundamentals) Symbols() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]string, 0, len(f.shares))
	for s := range f.shares {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Name returns the company name for a ticker, if known.
func (f *Fundamentals) Name(symbol string) string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.names[strings.ToUpper(symbol)]
}

// SharesOutstanding returns the share count in effect on day d.
//
// If d precedes the earliest row for the symbol, the earliest row is used —
// extrapolating backwards is less wrong than reporting zero, but it is one
// more reason ranking accuracy decays as you test further back.
func (f *Fundamentals) SharesOutstanding(symbol string, d Day) (float64, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	rows := f.shares[strings.ToUpper(symbol)]
	if len(rows) == 0 {
		return 0, false
	}
	i := sort.Search(len(rows), func(i int) bool { return rows[i].From > d })
	if i == 0 {
		return rows[0].Shares, true
	}
	return rows[i-1].Shares, true
}

// MarketCap computes shares outstanding times the raw close on day d.
//
// The raw close is required, not the adjusted close: share counts are stated
// in actual shares, and pairing them with a split-adjusted price would
// understate the cap of every company that has ever split.
func (f *Fundamentals) MarketCap(symbol string, d Day, series *Series) (float64, bool) {
	if series == nil {
		return 0, false
	}
	shares, ok := f.SharesOutstanding(symbol, d)
	if !ok {
		return 0, false
	}
	bar, ok := series.AsOf(d)
	if !ok || bar.Close <= 0 {
		return 0, false
	}
	return shares * bar.Close, true
}

// Ranking is one entry in a market-cap-ordered list.
type Ranking struct {
	Rank      int     `json:"rank"`
	Symbol    string  `json:"symbol"`
	Name      string  `json:"name,omitempty"`
	MarketCap float64 `json:"market_cap"`
	Price     float64 `json:"price"`
	Shares    float64 `json:"shares"`
}

// RankByMarketCap orders the supplied symbols by market cap on day d,
// largest first. Symbols with no share data or no price are omitted.
func (f *Fundamentals) RankByMarketCap(d Day, series map[string]*Series) []Ranking {
	out := make([]Ranking, 0, len(series))
	for sym, s := range series {
		shares, ok := f.SharesOutstanding(sym, d)
		if !ok {
			continue
		}
		bar, ok := s.AsOf(d)
		if !ok || bar.Close <= 0 {
			continue
		}
		// Skip symbols whose data has not started yet: AsOf would otherwise
		// return a stale bar from before the requested day.
		if first, ok := s.First(); ok && first.Date > d {
			continue
		}
		out = append(out, Ranking{
			Symbol:    sym,
			Name:      f.Name(sym),
			MarketCap: shares * bar.Close,
			Price:     bar.Close,
			Shares:    shares,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MarketCap != out[j].MarketCap {
			return out[i].MarketCap > out[j].MarketCap
		}
		return out[i].Symbol < out[j].Symbol // deterministic tie-break
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}
