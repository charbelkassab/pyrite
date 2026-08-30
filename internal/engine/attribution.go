package engine

import (
	"fmt"
	"math"
	"sort"

	"github.com/charbelkassab/pyrite/internal/market"
)

// PeriodStats is the performance of one slice of the run.
type PeriodStats struct {
	Label string     `json:"label"`
	Start market.Day `json:"start"`
	End   market.Day `json:"end"`

	Return          float64 `json:"return"`
	BenchmarkReturn float64 `json:"benchmark_return,omitempty"`
	// Excess is Return minus BenchmarkReturn, present only when a benchmark
	// covered the same slice.
	Excess      float64 `json:"excess,omitempty"`
	MaxDrawdown float64 `json:"max_drawdown"`
	Volatility  float64 `json:"volatility"`
	Sharpe      Ratio   `json:"sharpe"`
	TradingDays int     `json:"trading_days"`
	Trades      int     `json:"trades"`
	// Exposure is the average gross exposure over the slice: a period with a
	// flat return and zero exposure is a different fact from a flat return
	// while fully invested.
	Exposure float64 `json:"exposure"`
}

// SymbolStats is one symbol's contribution to the result.
type SymbolStats struct {
	Symbol string `json:"symbol"`
	Trades int    `json:"trades"`
	Wins   int    `json:"wins"`
	// NetPnL is realised, after costs.
	NetPnL float64 `json:"net_pnl"`
	// Contribution is this symbol's share of total realised P&L. It is
	// signed, and a losing symbol in a winning strategy gets a negative
	// share — so the column does not sum tidily to 1, which is the honest
	// presentation.
	Contribution float64 `json:"contribution"`
	WinRate      float64 `json:"win_rate"`
	AvgReturnPct float64 `json:"avg_return_pct"`
	Costs        float64 `json:"costs"`
	AvgBarsHeld  float64 `json:"avg_bars_held"`
}

// StressResult reports what the result looks like with its best episodes
// removed.
//
// The purpose is blunt: most backtests are one or two good stretches wearing a
// trench coat. If the whole edge lives in a single month, the strategy is a bet
// on that month recurring, and it should be described that way.
type StressResult struct {
	Label string `json:"label"`
	// Return is the total return with the named episodes excluded.
	Return float64 `json:"return"`
	// Removed is what was taken out, for display.
	Removed []string `json:"removed,omitempty"`
	// ShareOfTotal is the fraction of the full-period gain that the removed
	// episodes accounted for.
	ShareOfTotal float64 `json:"share_of_total"`
}

// Attribution decomposes a result along time, market regime and holding.
type Attribution struct {
	ByYear        []PeriodStats  `json:"by_year"`
	ByMonth       []PeriodStats  `json:"by_month"`
	ByMonthOfYear []PeriodStats  `json:"by_month_of_year"`
	ByRegime      []PeriodStats  `json:"by_regime"`
	BySymbol      []SymbolStats  `json:"by_symbol"`
	Stress        []StressResult `json:"stress"`
}

// ComputeAttribution builds the full decomposition. benchmark may be nil.
func ComputeAttribution(curve []EquityPoint, trades []Trade, benchmark []EquityPoint, sc Scale) Attribution {
	var a Attribution
	if len(curve) < 2 {
		return a
	}
	benchByDay := map[market.Day]float64{}
	for _, p := range benchmark {
		benchByDay[p.Date] = p.Value
	}

	a.ByYear = slicePeriods(curve, trades, benchByDay, sc, func(d market.Day) string {
		return string(d.Date())[:4]
	})
	a.ByMonth = slicePeriods(curve, trades, benchByDay, sc, func(d market.Day) string {
		return string(d.Date())[:7]
	})
	a.ByMonthOfYear = seasonalMonths(curve, sc)
	a.ByRegime = regimePeriods(curve, trades, benchmark, sc)
	a.BySymbol = symbolAttribution(trades)
	a.Stress = stressTests(curve, a.ByMonth)
	return a
}

// slicePeriods groups the curve by a key derived from the date and measures
// each group.
//
// Returns are computed from the equity value at the boundary of the previous
// group, not from the first value inside the group, so the first day's move is
// not silently discarded from every period.
func slicePeriods(curve []EquityPoint, trades []Trade, bench map[market.Day]float64, sc Scale, key func(market.Day) string) []PeriodStats {
	if len(curve) == 0 {
		return nil
	}
	type group struct {
		label    string
		startIdx int
		endIdx   int
	}
	var groups []group
	cur := group{label: key(curve[0].Date), startIdx: 0, endIdx: 0}
	for i := 1; i < len(curve); i++ {
		k := key(curve[i].Date)
		if k != cur.label {
			groups = append(groups, cur)
			cur = group{label: k, startIdx: i, endIdx: i}
			continue
		}
		cur.endIdx = i
	}
	groups = append(groups, cur)

	out := make([]PeriodStats, 0, len(groups))
	for _, g := range groups {
		seg := curve[g.startIdx : g.endIdx+1]
		// Base the return on the close of the last bar before the period.
		base := seg[0].Value
		baseDate := seg[0].Date
		if g.startIdx > 0 {
			base = curve[g.startIdx-1].Value
			baseDate = curve[g.startIdx-1].Date
		}
		ps := periodStats(g.label, seg, base, sc)
		if bv, ok := bench[baseDate]; ok && bv > 0 {
			if ev, ok2 := bench[seg[len(seg)-1].Date]; ok2 && ev > 0 {
				ps.BenchmarkReturn = ev/bv - 1
				ps.Excess = ps.Return - ps.BenchmarkReturn
			}
		}
		ps.Trades = countTradesIn(trades, seg[0].Date, seg[len(seg)-1].Date)
		out = append(out, ps)
	}
	return out
}

// periodStats measures one contiguous slice against a starting value.
func periodStats(label string, seg []EquityPoint, base float64, sc Scale) PeriodStats {
	ps := PeriodStats{
		Label:       label,
		Start:       seg[0].Date,
		End:         seg[len(seg)-1].Date,
		TradingDays: len(seg),
		Sharpe:      Ratio(math.NaN()),
	}
	if base > 0 {
		ps.Return = seg[len(seg)-1].Value/base - 1
	}

	rets := make([]float64, 0, len(seg))
	prev := base
	peak := base
	var expSum float64
	for _, p := range seg {
		if prev > 0 {
			rets = append(rets, p.Value/prev-1)
		}
		prev = p.Value
		if p.Value > peak {
			peak = p.Value
		}
		if peak > 0 {
			if dd := p.Value/peak - 1; dd < ps.MaxDrawdown {
				ps.MaxDrawdown = dd
			}
		}
		expSum += p.Exposure
	}
	if len(seg) > 0 {
		ps.Exposure = expSum / float64(len(seg))
	}
	if len(rets) > 1 {
		mean, sd := meanStdev(rets)
		if sd > 0 {
			ps.Volatility = sc.Vol(sd)
			ps.Sharpe = Ratio(sc.Sharpe(mean, sd))
		}
	}
	return ps
}

// seasonalMonths aggregates every January together, every February together,
// and so on — the calendar-effect view.
func seasonalMonths(curve []EquityPoint, sc Scale) []PeriodStats {
	names := []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun",
		"Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	sums := make([][]float64, 12)
	rets := dailyReturns(curve)
	for i, r := range rets {
		d := curve[i+1].Date
		mo := int(d.Time().Month()) - 1
		if mo < 0 || mo > 11 {
			continue
		}
		sums[mo] = append(sums[mo], r)
	}
	out := make([]PeriodStats, 0, 12)
	for i, rs := range sums {
		ps := PeriodStats{Label: names[i], TradingDays: len(rs), Sharpe: Ratio(math.NaN())}
		if len(rs) == 0 {
			out = append(out, ps)
			continue
		}
		// Compound rather than sum: a month's return is what an investor got,
		// not the arithmetic mean of its days.
		compound := 1.0
		for _, r := range rs {
			compound *= 1 + r
		}
		ps.Return = compound - 1
		if len(rs) > 1 {
			mean, sd := meanStdev(rs)
			if sd > 0 {
				ps.Volatility = sc.Vol(sd)
				ps.Sharpe = Ratio(mean / sd * sc.Root())
			}
		}
		out = append(out, ps)
	}
	return out
}

// regimePeriods splits the run by market conditions rather than by calendar.
//
// The regime is defined from the benchmark when one exists, and from the
// strategy's own curve otherwise. Using the benchmark is the meaningful choice:
// "how does this behave when the market is falling" is a question about the
// market, not about the strategy.
func regimePeriods(curve []EquityPoint, trades []Trade, benchmark []EquityPoint, sc Scale) []PeriodStats {
	ref := benchmark
	if len(ref) < 60 {
		ref = curve
	}
	if len(ref) < 60 {
		return nil
	}
	refByDay := make(map[market.Day]float64, len(ref))
	for _, p := range ref {
		refByDay[p.Date] = p.Value
	}

	// Trailing 63-day (one quarter) realised volatility of the reference, and
	// its drawdown from a running peak.
	const volWindow = 63
	labels := make(map[market.Day]string, len(curve))
	var vols []float64
	volAt := make(map[market.Day]float64, len(ref))
	ddAt := make(map[market.Day]float64, len(ref))
	peak := ref[0].Value
	refRets := dailyReturns(ref)
	for i := 1; i < len(ref); i++ {
		if ref[i].Value > peak {
			peak = ref[i].Value
		}
		if peak > 0 {
			ddAt[ref[i].Date] = ref[i].Value/peak - 1
		}
		if i >= volWindow {
			_, sd := meanStdev(refRets[i-volWindow : i])
			v := sc.Vol(sd)
			volAt[ref[i].Date] = v
			vols = append(vols, v)
		}
	}
	if len(vols) < 10 {
		return nil
	}
	sorted := append([]float64(nil), vols...)
	sort.Float64s(sorted)
	loCut := percentileSorted(sorted, 1.0/3.0)
	hiCut := percentileSorted(sorted, 2.0/3.0)

	for _, p := range curve {
		// Bear beats volatility: a 20% decline is the regime, whatever the
		// realised vol happens to print that week.
		if dd, ok := ddAt[p.Date]; ok && dd <= -0.10 {
			labels[p.Date] = "bear (>10% off peak)"
			continue
		}
		v, ok := volAt[p.Date]
		if !ok {
			continue
		}
		switch {
		case v <= loCut:
			labels[p.Date] = "calm"
		case v >= hiCut:
			labels[p.Date] = "high volatility"
		default:
			labels[p.Date] = "normal"
		}
	}

	// Group by label. These slices are not contiguous, so the return is the
	// compounded product of the daily returns that carry the label.
	order := []string{"calm", "normal", "high volatility", "bear (>10% off peak)"}
	acc := map[string][]float64{}
	count := map[string]int{}
	expo := map[string]float64{}
	rets := dailyReturns(curve)
	for i, r := range rets {
		d := curve[i+1].Date
		l, ok := labels[d]
		if !ok {
			continue
		}
		acc[l] = append(acc[l], r)
		count[l]++
		expo[l] += curve[i+1].Exposure
	}

	out := make([]PeriodStats, 0, len(order))
	for _, l := range order {
		rs := acc[l]
		if len(rs) == 0 {
			continue
		}
		ps := PeriodStats{Label: l, TradingDays: len(rs), Sharpe: Ratio(math.NaN())}
		compound := 1.0
		var worst, run float64
		for _, r := range rs {
			compound *= 1 + r
			// A drawdown within a non-contiguous regime is only meaningful as
			// the worst cumulative run of that regime's own days.
			run = math.Min(0, (1+run)*(1+r)-1)
			worst = math.Min(worst, run)
		}
		ps.Return = compound - 1
		ps.MaxDrawdown = worst
		ps.Exposure = expo[l] / float64(len(rs))
		if len(rs) > 1 {
			mean, sd := meanStdev(rs)
			if sd > 0 {
				ps.Volatility = sc.Vol(sd)
				ps.Sharpe = Ratio(sc.Sharpe(mean, sd))
			}
		}
		out = append(out, ps)
	}
	return out
}

// symbolAttribution ranks holdings by realised contribution.
func symbolAttribution(trades []Trade) []SymbolStats {
	if len(trades) == 0 {
		return nil
	}
	agg := map[string]*SymbolStats{}
	bars := map[string]float64{}
	var totalAbs float64
	for _, t := range trades {
		if t.Open {
			continue
		}
		s, ok := agg[t.Symbol]
		if !ok {
			s = &SymbolStats{Symbol: t.Symbol}
			agg[t.Symbol] = s
		}
		s.Trades++
		s.NetPnL += t.NetPnL
		s.Costs += t.Costs
		s.AvgReturnPct += t.ReturnPct
		bars[t.Symbol] += float64(t.BarsHeld)
		if t.NetPnL > 0 {
			s.Wins++
		}
		totalAbs += math.Abs(t.NetPnL)
	}
	out := make([]SymbolStats, 0, len(agg))
	for _, s := range agg {
		if s.Trades > 0 {
			s.WinRate = float64(s.Wins) / float64(s.Trades)
			s.AvgReturnPct /= float64(s.Trades)
			s.AvgBarsHeld = bars[s.Symbol] / float64(s.Trades)
		}
		if totalAbs > 0 {
			s.Contribution = s.NetPnL / totalAbs
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NetPnL != out[j].NetPnL {
			return out[i].NetPnL > out[j].NetPnL
		}
		return out[i].Symbol < out[j].Symbol
	})
	return out
}

// stressTests re-compounds the run with its best episodes removed.
func stressTests(curve []EquityPoint, byMonth []PeriodStats) []StressResult {
	rets := dailyReturns(curve)
	if len(rets) < 20 {
		return nil
	}
	full := 1.0
	for _, r := range rets {
		full *= 1 + r
	}
	fullReturn := full - 1

	var out []StressResult

	// Drop the single best calendar month.
	if len(byMonth) > 1 {
		best := 0
		for i := range byMonth {
			if byMonth[i].Return > byMonth[best].Return {
				best = i
			}
		}
		bestLabel := byMonth[best].Label
		compound := 1.0
		for i, r := range rets {
			if string(curve[i+1].Date)[:7] == bestLabel {
				continue
			}
			compound *= 1 + r
		}
		out = append(out, StressResult{
			Label:        "excluding the best month",
			Return:       compound - 1,
			Removed:      []string{bestLabel},
			ShareOfTotal: shareOf(fullReturn, compound-1),
		})
	}

	// Drop the five best days.
	type dayRet struct {
		i int
		r float64
	}
	drs := make([]dayRet, len(rets))
	for i, r := range rets {
		drs[i] = dayRet{i, r}
	}
	sort.Slice(drs, func(i, j int) bool { return drs[i].r > drs[j].r })
	n := 5
	if n > len(drs) {
		n = len(drs)
	}
	drop := map[int]bool{}
	removed := make([]string, 0, n)
	for _, d := range drs[:n] {
		drop[d.i] = true
		removed = append(removed, fmt.Sprintf("%s (%+.1f%%)", curve[d.i+1].Date, d.r*100))
	}
	compound := 1.0
	for i, r := range rets {
		if drop[i] {
			continue
		}
		compound *= 1 + r
	}
	out = append(out, StressResult{
		Label:        "excluding the 5 best days",
		Return:       compound - 1,
		Removed:      removed,
		ShareOfTotal: shareOf(fullReturn, compound-1),
	})
	return out
}

// shareOf reports how much of the full gain the removed episodes carried.
func shareOf(full, without float64) float64 {
	if full == 0 {
		return 0
	}
	return (full - without) / math.Abs(full)
}

func countTradesIn(trades []Trade, from, to market.Day) int {
	n := 0
	for _, t := range trades {
		d := t.ExitDate
		if d == "" {
			d = t.EntryDate
		}
		if d >= from && d <= to {
			n++
		}
	}
	return n
}
