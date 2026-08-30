// Package market provides historical price data, corporate actions and the
// fundamental figures needed to rank a universe of symbols.
package market

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Day is a calendar date in ISO form, "2006-01-02".
//
// A plain string is used rather than time.Time because every consumer of this
// type — map keys, JSON payloads, the charting library on the front end and
// lexicographic sorting — works naturally with it, and daily bars have no
// meaningful intraday component.
type Day string

// Layout is the canonical date layout used throughout natural-quant.
const Layout = "2006-01-02"

// NewDay converts a time.Time to a Day in UTC.
func NewDay(t time.Time) Day { return Day(t.UTC().Format(Layout)) }

// ParseDay parses an ISO date.
func ParseDay(s string) (Day, error) {
	t, err := time.Parse(Layout, strings.TrimSpace(s))
	if err != nil {
		return "", fmt.Errorf("invalid date %q (want YYYY-MM-DD): %w", s, err)
	}
	return NewDay(t), nil
}

// Time converts back to a time.Time at UTC midnight.
func (d Day) Time() time.Time {
	t, _ := time.Parse(Layout, string(d))
	return t
}

// Add returns the day shifted by n calendar days.
func (d Day) Add(n int) Day { return NewDay(d.Time().AddDate(0, 0, n)) }

// Before reports whether d is earlier than other.
func (d Day) Before(other Day) bool { return d < other }

// After reports whether d is later than other.
func (d Day) After(other Day) bool { return d > other }

// Bar is a single daily OHLCV observation.
//
// Close is the raw close as printed on the day. AdjClose is adjusted for
// splits and dividends. Both are kept: raw prices are what a trader would
// have seen and what share counts refer to, while adjusted prices are what
// return calculations must use.
type Bar struct {
	Date     Day     `json:"date"`
	Open     float64 `json:"open"`
	High     float64 `json:"high"`
	Low      float64 `json:"low"`
	Close    float64 `json:"close"`
	AdjClose float64 `json:"adj_close"`
	Volume   float64 `json:"volume"`
}

// SplitFactor is the cumulative split adjustment implied by the difference
// between the raw and adjusted close. It is 1.0 when no adjustment applies.
func (b Bar) SplitFactor() float64 {
	if b.Close == 0 || b.AdjClose == 0 {
		return 1
	}
	return b.AdjClose / b.Close
}

// Series is a symbol's ordered history plus a date index for O(1) lookup.
type Series struct {
	Symbol string `json:"symbol"`
	Name   string `json:"name,omitempty"`
	Bars   []Bar  `json:"bars"`

	idx map[Day]int
}

// NewSeries builds a Series, sorting bars by date and de-duplicating.
func NewSeries(symbol string, bars []Bar) *Series {
	sort.Slice(bars, func(i, j int) bool { return bars[i].Date < bars[j].Date })
	out := bars[:0]
	var last Day
	for _, b := range bars {
		if b.Date == last {
			out[len(out)-1] = b // later observation wins
			continue
		}
		out = append(out, b)
		last = b.Date
	}
	s := &Series{Symbol: symbol, Bars: out}
	s.reindex()
	return s
}

func (s *Series) reindex() {
	s.idx = make(map[Day]int, len(s.Bars))
	for i, b := range s.Bars {
		s.idx[b.Date] = i
	}
}

// Index returns the position of the bar on day d, or -1.
func (s *Series) Index(d Day) int {
	if s.idx == nil {
		s.reindex()
	}
	if i, ok := s.idx[d]; ok {
		return i
	}
	return -1
}

// At returns the bar exactly on day d.
func (s *Series) At(d Day) (Bar, bool) {
	i := s.Index(d)
	if i < 0 {
		return Bar{}, false
	}
	return s.Bars[i], true
}

// AsOf returns the most recent bar on or before day d. This is the correct
// lookup for a strategy asking "what is the price?" on a day the symbol did
// not trade (a holiday, a halt, or a symbol with a shorter calendar).
func (s *Series) AsOf(d Day) (Bar, bool) {
	if i := s.Index(d); i >= 0 {
		return s.Bars[i], true
	}
	// Binary search for the last bar <= d.
	i := sort.Search(len(s.Bars), func(i int) bool { return s.Bars[i].Date > d })
	if i == 0 {
		return Bar{}, false
	}
	return s.Bars[i-1], true
}

// History returns up to n bars ending on or before day d, oldest first.
func (s *Series) History(d Day, n int) []Bar {
	if n <= 0 {
		return nil
	}
	end := sort.Search(len(s.Bars), func(i int) bool { return s.Bars[i].Date > d })
	start := end - n
	if start < 0 {
		start = 0
	}
	return s.Bars[start:end]
}

// Range returns the bars within [from, to] inclusive.
func (s *Series) Range(from, to Day) []Bar {
	lo := sort.Search(len(s.Bars), func(i int) bool { return s.Bars[i].Date >= from })
	hi := sort.Search(len(s.Bars), func(i int) bool { return s.Bars[i].Date > to })
	if lo > hi {
		return nil
	}
	return s.Bars[lo:hi]
}

// First and Last return the boundary bars.
func (s *Series) First() (Bar, bool) {
	if len(s.Bars) == 0 {
		return Bar{}, false
	}
	return s.Bars[0], true
}

func (s *Series) Last() (Bar, bool) {
	if len(s.Bars) == 0 {
		return Bar{}, false
	}
	return s.Bars[len(s.Bars)-1], true
}

// Quote is a lightweight symbol description returned by search.
type Quote struct {
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Exchange string `json:"exchange,omitempty"`
	Type     string `json:"type,omitempty"`
}

// NormalizeSymbol upper-cases and trims a ticker, and maps a few common
// aliases people type for indices onto tradable proxies or index tickers.
func NormalizeSymbol(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if alias, ok := symbolAliases[s]; ok {
		return alias
	}
	return s
}

// symbolAliases maps friendly names onto the tickers the data provider uses.
//
// A real ticker always wins over a friendly name. "DOW" is Dow Inc and "GOLD"
// is Barrick Gold, so neither may alias to an index or a futures contract —
// doing so would silently trade something other than what the strategy named,
// which is far worse than failing to recognise a nickname. Spelled-out forms
// that are not themselves tickers are safe.
var symbolAliases = map[string]string{
	"S&P500":      "^GSPC",
	"S&P 500":     "^GSPC",
	"SP500":       "^GSPC",
	"SPX":         "^GSPC",
	"NASDAQ":      "^IXIC",
	"NASDAQ100":   "^NDX",
	"NDX":         "^NDX",
	"DOW JONES":   "^DJI",
	"DOWJONES":    "^DJI",
	"DJIA":        "^DJI",
	"RUSSELL":     "^RUT",
	"RUSSELL2000": "^RUT",
	"VIX":         "^VIX",
	"GOLD PRICE":  "GC=F",
	"SPOT GOLD":   "GC=F",
	"CRUDE OIL":   "CL=F",
	"OIL":         "CL=F",
	"BITCOIN":     "BTC-USD",
	"ETHEREUM":    "ETH-USD",
}
