package market

import (
	"context"
	"hash/fnv"
	"math"
	"math/rand"
	"sort"
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

	// A generated series has to keep the calendar its ticker implies, or the
	// offline path manufactures a bitcoin that shuts at weekends — a market
	// that does not exist, audited and annualised as though it did. The
	// annual drift and volatility are spread over that calendar's sessions
	// for the same reason: the figures above are per year, and how many bars
	// a year holds is the thing that varies.
	cal := CalendarForSymbol(symbol)
	sessions := cal.SessionsPerYear()
	muDaily := annDrift / sessions
	sigDaily := annVol / math.Sqrt(sessions)

	var bars []Bar
	i := 0
	for d := start; d <= end; d = d.Add(1) {
		if cal != CalendarContinuous {
			if IsWeekend(d) {
				continue
			}
			// FX runs through the exchange holidays and stops only at the
			// weekend.
			if cal != CalendarFX && isSyntheticHoliday(d) {
				continue
			}
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
	// Sorted, so the same query returns the same list in the same order.
	// Ranging the map directly reshuffled the suggestions under the search
	// box on every keystroke, which reads as the search being broken. Not a
	// numeric bug like the four this class has already produced in the
	// engine, but the same mistake.
	rest := make([]string, 0, len(syntheticNames))
	for sym := range syntheticNames {
		if sym != q && strings.Contains(strings.ToUpper(syntheticNames[sym]), q) {
			rest = append(rest, sym)
		}
	}
	sort.Strings(rest)
	for _, sym := range rest {
		out = append(out, Quote{Symbol: sym, Name: syntheticNames[sym], Type: "EQUITY"})
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

// SupportedIntervals reports every bar size the generator can produce, which
// is all of them: it is producing the bars rather than fetching them. That matters for more than
// demos — it is what lets the intraday path be tested with no network.
func (s *SyntheticProvider) SupportedIntervals() []Interval {
	out := make([]Interval, 0, 8)
	for _, n := range IntervalNames() {
		out = append(out, Interval(n))
	}
	return out
}

// FetchInterval generates bars at the requested size.
//
// Intraday bars are built by subdividing the day's range rather than by
// running the daily generator faster, so the day's open, high, low and close
// still agree with what Fetch would return for the same date. A backtest that
// switches bar size should be looking at the same market.
func (s *SyntheticProvider) FetchInterval(ctx context.Context, symbol string, from, to Day, iv Interval) (*Series, error) {
	if iv == "" || !iv.Intraday() {
		return s.Fetch(ctx, symbol, from, to)
	}
	daily, err := s.Fetch(ctx, symbol, from, to)
	if err != nil {
		return nil, err
	}

	cal := CalendarForSymbol(symbol)
	perSession := int(iv.BarsPerSession(cal))
	if perSession < 1 {
		perSession = 1
	}
	// The US regular session, in UTC, which is what NewStamp records. A
	// market that never closes starts its day at midnight instead.
	openHour, openMinute := 14, 30
	if cal != CalendarUSEquity {
		openHour, openMinute = 0, 0
	}

	bars := make([]Bar, 0, len(daily.Bars)*perSession)
	for _, d := range daily.Bars {
		day := d.Date.Date().Time()
		prev := d.Open
		for i := 0; i < perSession; i++ {
			t := day.Add(time.Duration(openHour)*time.Hour +
				time.Duration(openMinute)*time.Minute +
				time.Duration(i)*iv.Duration())

			// Walk from the day's open to its close in equal steps, with the
			// extremes placed on real bars so the session's high and low
			// survive the subdivision.
			frac := float64(i+1) / float64(perSession)
			close := d.Open + (d.Close-d.Open)*frac
			high, low := math.Max(prev, close), math.Min(prev, close)
			if i == perSession/3 {
				high = math.Max(high, d.High)
			}
			if i == perSession*2/3 {
				low = math.Min(low, d.Low)
			}
			bars = append(bars, Bar{
				Date: NewStamp(t), Open: prev, High: high, Low: low,
				Close: close, AdjClose: close,
				Volume: d.Volume / float64(perSession),
			})
			prev = close
		}
	}
	out := NewSeries(NormalizeSymbol(symbol), bars)
	out.Name = daily.Name
	return out, nil
}
