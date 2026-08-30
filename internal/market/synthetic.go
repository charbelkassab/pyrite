package market

import (
	"context"
	"hash/fnv"
	"math"
	"math/rand"
	"strings"
	"time"
)

// SyntheticProvider generates deterministic pseudo-random price histories.
//
// It exists so pyrite can be demoed, developed against and tested in
// CI with no network access and no API keys. Every symbol produces a stable
// series derived from a hash of its ticker, so runs are reproducible and
// tests can assert exact numbers.
//
// Prices follow a geometric Brownian motion with a symbol-specific drift and
// volatility, plus a slow cyclical component so that trend-following and
// mean-reversion strategies both find something to trade.
type SyntheticProvider struct {
	// Start is the earliest date generated.
	Start Day
	// End is the latest date generated; zero means "today".
	End Day
}

// NewSyntheticProvider returns a provider covering a generous date range.
func NewSyntheticProvider() *SyntheticProvider {
	return &SyntheticProvider{Start: "2000-01-03"}
}

func (s *SyntheticProvider) Name() string { return "synthetic" }

// syntheticNames gives a handful of well-known tickers plausible descriptions
// so the offline demo reads naturally.
var syntheticNames = map[string]string{
	"AAPL":  "Apple Inc. (synthetic)",
	"MSFT":  "Microsoft Corporation (synthetic)",
	"NVDA":  "NVIDIA Corporation (synthetic)",
	"GOOGL": "Alphabet Inc. (synthetic)",
	"AMZN":  "Amazon.com, Inc. (synthetic)",
	"META":  "Meta Platforms, Inc. (synthetic)",
	"TSLA":  "Tesla, Inc. (synthetic)",
	"SPY":   "S&P 500 ETF (synthetic)",
	"QQQ":   "Nasdaq 100 ETF (synthetic)",
	"^GSPC": "S&P 500 Index (synthetic)",
	"^IXIC": "Nasdaq Composite (synthetic)",
}

// Fetch generates a deterministic series for the symbol.
func (s *SyntheticProvider) Fetch(ctx context.Context, symbol string, from, to Day) (*Series, error) {
	start := s.Start
	if start == "" {
		start = "2000-01-03"
	}
	if from != "" && from < start {
		start = from
	}
	end := s.End
	if end == "" {
		end = NewDay(time.Now())
	}
	if to > end {
		end = to
	}

	seed := hashSymbol(symbol)
	rng := rand.New(rand.NewSource(int64(seed)))

	// Derive per-symbol characteristics from the hash so each ticker has a
	// distinct but stable personality.
	price := 20 + float64(seed%400)
	annDrift := 0.02 + float64(seed%18)/100.0    // 2%..20% per year
	annVol := 0.14 + float64((seed>>8)%40)/100.0 // 14%..54% per year
	cycleLen := 90 + float64((seed>>16)%400)     // trading days
	cycleAmp := 0.04 + float64((seed>>20)%12)/100.0

	// Broad market proxies get lower volatility and steadier drift.
	if isIndexLike(symbol) {
		annDrift = 0.08
		annVol = 0.16
		cycleAmp = 0.02
	}

	const tradingDaysPerYear = 252.0
	muDaily := annDrift / tradingDaysPerYear
	sigDaily := annVol / math.Sqrt(tradingDaysPerYear)

	var bars []Bar
	i := 0
	for d := start; d <= end; d = d.Add(1) {
		wd := d.Time().Weekday()
		if wd == time.Saturday || wd == time.Sunday {
			continue
		}
		if isSyntheticHoliday(d) {
			continue
		}

		cycle := cycleAmp * math.Sin(2*math.Pi*float64(i)/cycleLen)
		shock := rng.NormFloat64() * sigDaily
		ret := muDaily + shock + cycle/cycleLen

		price *= math.Exp(ret)
		if price < 0.5 {
			price = 0.5
		}

		intraday := math.Abs(rng.NormFloat64()) * sigDaily * 0.6
		open := price * math.Exp(rng.NormFloat64()*sigDaily*0.3)
		high := math.Max(open, price) * (1 + intraday)
		low := math.Min(open, price) * (1 - intraday)
		vol := math.Round(1e6 + math.Abs(rng.NormFloat64())*5e6)

		bars = append(bars, Bar{
			Date:     d,
			Open:     round2(open),
			High:     round2(high),
			Low:      round2(low),
			Close:    round2(price),
			AdjClose: round2(price),
			Volume:   vol,
		})
		i++
	}

	if len(bars) == 0 {
		return nil, ErrNotFound
	}
	ser := NewSeries(symbol, bars)
	if n, ok := syntheticNames[strings.ToUpper(symbol)]; ok {
		ser.Name = n
	} else {
		ser.Name = symbol + " (synthetic)"
	}
	return ser, nil
}

// Search returns any symbol the caller asks for; the synthetic provider can
// manufacture data for every ticker.
func (s *SyntheticProvider) Search(ctx context.Context, query string) ([]Quote, error) {
	q := strings.ToUpper(strings.TrimSpace(query))
	if q == "" {
		return nil, nil
	}
	out := []Quote{{Symbol: q, Name: syntheticNames[q], Type: "EQUITY"}}
	if out[0].Name == "" {
		out[0].Name = q + " (synthetic)"
	}
	for sym, name := range syntheticNames {
		if sym != q && strings.Contains(strings.ToUpper(name), q) {
			out = append(out, Quote{Symbol: sym, Name: name, Type: "EQUITY"})
		}
	}
	return out, nil
}

func hashSymbol(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToUpper(s)))
	return h.Sum32()
}

func isIndexLike(symbol string) bool {
	u := strings.ToUpper(symbol)
	if strings.HasPrefix(u, "^") {
		return true
	}
	switch u {
	case "SPY", "QQQ", "VOO", "VTI", "IWM", "DIA":
		return true
	}
	return false
}

// isSyntheticHoliday marks the fixed-date US market holidays. Floating
// holidays are ignored: the synthetic calendar only needs to be plausible,
// not exact.
func isSyntheticHoliday(d Day) bool {
	t := d.Time()
	switch {
	case t.Month() == time.January && t.Day() == 1:
		return true
	case t.Month() == time.July && t.Day() == 4:
		return true
	case t.Month() == time.December && t.Day() == 25:
		return true
	}
	return false
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
