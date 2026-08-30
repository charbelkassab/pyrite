package engine

import (
	"encoding/json"
	"math"
	"sort"

	"github.com/charbelkassab/pyrite/internal/market"
)

// TradingDaysPerYear is the annualisation factor for daily data.
const TradingDaysPerYear = 252.0

// Ratio is a metric that may legitimately be undefined — a Sortino ratio with
// no losing days, a profit factor with no losing trades, a beta against a
// benchmark that never moved.
//
// It marshals to JSON null rather than a number. This matters twice over:
// encoding/json refuses to encode NaN and ±Inf at all, so without this a
// single flawless strategy would truncate the whole API response; and
// reporting an undefined ratio as 0 reads as the worst possible score when it
// is closer to the best.
type Ratio float64

// MarshalJSON emits null for non-finite values.
func (r Ratio) MarshalJSON() ([]byte, error) {
	v := float64(r)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return []byte("null"), nil
	}
	return json.Marshal(v)
}

// UnmarshalJSON accepts null and restores it as NaN.
func (r *Ratio) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*r = Ratio(math.NaN())
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*r = Ratio(v)
	return nil
}

// Defined reports whether the ratio holds a usable number.
func (r Ratio) Defined() bool {
	v := float64(r)
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// Metrics summarises the risk and return profile of an equity curve.
//
// Every field is computed from the daily equity series, so a strategy and a
// buy-and-hold benchmark are measured identically and are directly
// comparable.
type Metrics struct {
	StartValue float64 `json:"start_value"`
	EndValue   float64 `json:"end_value"`
	// TotalReturn is the cumulative fraction, e.g. 0.42 for +42%.
	TotalReturn float64 `json:"total_return"`
	// CAGR is the compound annual growth rate.
	CAGR float64 `json:"cagr"`
	// Volatility is the annualised standard deviation of daily returns.
	Volatility float64 `json:"volatility"`
	// Sharpe uses the configured risk-free rate.
	Sharpe Ratio `json:"sharpe"`
	// Sortino penalises only downside deviation.
	Sortino Ratio `json:"sortino"`
	// Calmar is CAGR divided by the absolute maximum drawdown.
	Calmar Ratio `json:"calmar"`
	// MaxDrawdown is the worst peak-to-trough decline, negative.
	MaxDrawdown float64 `json:"max_drawdown"`
	// MaxDrawdownStart/End/Recovery describe when it happened.
	MaxDrawdownStart market.Day `json:"max_drawdown_start,omitempty"`
	MaxDrawdownEnd   market.Day `json:"max_drawdown_end,omitempty"`
	// LongestDrawdownDays is the longest stretch below a prior peak.
	LongestDrawdownDays int `json:"longest_drawdown_days"`

	BestDay      float64    `json:"best_day"`
	BestDayDate  market.Day `json:"best_day_date,omitempty"`
	WorstDay     float64    `json:"worst_day"`
	WorstDayDate market.Day `json:"worst_day_date,omitempty"`

	// WinRate is the fraction of days with a positive return.
	WinRate float64 `json:"win_rate"`
	// TradingDays is the number of daily observations.
	TradingDays int `json:"trading_days"`
	// Years is the elapsed calendar span.
	Years float64 `json:"years"`

	// Trade statistics, populated when fills are supplied.
	TotalTrades   int     `json:"total_trades"`
	WinningTrades int     `json:"winning_trades"`
	LosingTrades  int     `json:"losing_trades"`
	TradeWinRate  float64 `json:"trade_win_rate"`
	AvgWin        float64 `json:"avg_win"`
	AvgLoss       float64 `json:"avg_loss"`
	ProfitFactor  Ratio   `json:"profit_factor"`
	TotalCosts    float64 `json:"total_costs"`
	Turnover      float64 `json:"turnover"`

	// Benchmark-relative statistics, populated when a benchmark is given.
	Alpha              Ratio   `json:"alpha,omitempty"`
	BetaVsBenchmark    Ratio   `json:"beta,omitempty"`
	CorrelationVsBench Ratio   `json:"correlation,omitempty"`
	TrackingError      float64 `json:"tracking_error,omitempty"`
	InformationRatio   Ratio   `json:"information_ratio,omitempty"`
}

// EquityPoint is one observation on an equity curve.
type EquityPoint struct {
	Date   market.Day `json:"date"`
	Value  float64    `json:"value"`
	Cash   float64    `json:"cash"`
	Return float64    `json:"return"`
	// Drawdown is the decline from the running peak, negative.
	Drawdown float64 `json:"drawdown"`
	// Exposure is gross position value divided by equity.
	Exposure float64 `json:"exposure"`
}

// ComputeMetrics derives statistics from an equity curve.
func ComputeMetrics(curve []EquityPoint, riskFreeRate float64) Metrics {
	// Ratios start undefined. Their zero value would otherwise claim a real
	// score of zero for a statistic that was never computed.
	nan := Ratio(math.NaN())
	m := Metrics{Sharpe: nan, Sortino: nan, Calmar: nan, ProfitFactor: nan,
		Alpha: nan, BetaVsBenchmark: nan, CorrelationVsBench: nan, InformationRatio: nan}
	if len(curve) == 0 {
		return m
	}
	m.StartValue = curve[0].Value
	m.EndValue = curve[len(curve)-1].Value
	m.TradingDays = len(curve)

	if m.StartValue > 0 {
		m.TotalReturn = m.EndValue/m.StartValue - 1
	}

	// Elapsed time from the actual calendar span, not the bar count, so that
	// a sparse or partial year annualises correctly.
	days := curve[len(curve)-1].Date.Time().Sub(curve[0].Date.Time()).Hours() / 24
	m.Years = days / 365.25
	if m.Years > 0 && m.StartValue > 0 && m.EndValue > 0 {
		m.CAGR = math.Pow(m.EndValue/m.StartValue, 1/m.Years) - 1
	}

	rets := make([]float64, 0, len(curve))
	var wins int
	for i := 1; i < len(curve); i++ {
		prev := curve[i-1].Value
		if prev <= 0 {
			rets = append(rets, 0)
			continue
		}
		r := curve[i].Value/prev - 1
		rets = append(rets, r)
		if r > 0 {
			wins++
		}
		if r > m.BestDay {
			m.BestDay, m.BestDayDate = r, curve[i].Date
		}
		if r < m.WorstDay {
			m.WorstDay, m.WorstDayDate = r, curve[i].Date
		}
	}
	if len(rets) > 0 {
		m.WinRate = float64(wins) / float64(len(rets))
	}

	if len(rets) > 1 {
		sd := Stdev(rets, len(rets))
		if !math.IsNaN(sd) {
			m.Volatility = sd * math.Sqrt(TradingDaysPerYear)
		}
		mean := 0.0
		for _, r := range rets {
			mean += r
		}
		mean /= float64(len(rets))

		rfDaily := riskFreeRate / TradingDaysPerYear
		if sd > 0 {
			m.Sharpe = Ratio((mean - rfDaily) / sd * math.Sqrt(TradingDaysPerYear))
		}

		// Sortino: deviation of returns below the risk-free rate only.
		var downSS float64
		var downN int
		for _, r := range rets {
			if r < rfDaily {
				d := r - rfDaily
				downSS += d * d
				downN++
			}
		}
		if downN > 0 {
			dd := math.Sqrt(downSS / float64(downN))
			if dd > 0 {
				m.Sortino = Ratio((mean - rfDaily) / dd * math.Sqrt(TradingDaysPerYear))
			}
		}
	}

	// Maximum drawdown and its dating.
	peak := curve[0].Value
	peakDate := curve[0].Date
	var ddStart, ddEnd market.Day
	longest, current := 0, 0
	for _, p := range curve {
		if p.Value > peak {
			peak = p.Value
			peakDate = p.Date
			if current > longest {
				longest = current
			}
			current = 0
		} else {
			current++
		}
		if peak > 0 {
			dd := p.Value/peak - 1
			if dd < m.MaxDrawdown {
				m.MaxDrawdown = dd
				ddStart, ddEnd = peakDate, p.Date
			}
		}
	}
	if current > longest {
		longest = current
	}
	m.LongestDrawdownDays = longest
	m.MaxDrawdownStart, m.MaxDrawdownEnd = ddStart, ddEnd

	if m.MaxDrawdown < 0 {
		m.Calmar = Ratio(m.CAGR / math.Abs(m.MaxDrawdown))
	}
	return m
}

// AddTradeStats folds fill-level statistics into a Metrics value.
//
// A "trade" here is a realising fill — one that reduced or closed a position —
// because that is the point at which a profit or loss becomes known.
func (m *Metrics) AddTradeStats(fills []Fill, avgEquity float64) {
	var grossWin, grossLoss, costs, traded float64
	for _, f := range fills {
		// Slippage is a real cost even when commission is zero, so both are
		// reported rather than only the visible fee.
		costs += f.Commission + f.Slippage
		traded += f.Value
		if f.RealizedPnL == 0 {
			continue
		}
		m.TotalTrades++
		if f.RealizedPnL > 0 {
			m.WinningTrades++
			grossWin += f.RealizedPnL
		} else {
			m.LosingTrades++
			grossLoss += -f.RealizedPnL
		}
	}
	if m.TotalTrades > 0 {
		m.TradeWinRate = float64(m.WinningTrades) / float64(m.TotalTrades)
	}
	if m.WinningTrades > 0 {
		m.AvgWin = grossWin / float64(m.WinningTrades)
	}
	if m.LosingTrades > 0 {
		m.AvgLoss = grossLoss / float64(m.LosingTrades)
	}
	if grossLoss > 0 {
		m.ProfitFactor = Ratio(grossWin / grossLoss)
	} else if grossWin > 0 {
		m.ProfitFactor = Ratio(math.Inf(1))
	}
	m.TotalCosts = costs
	if avgEquity > 0 && m.Years > 0 {
		// Annualised turnover: dollars traded per dollar of equity per year.
		m.Turnover = traded / avgEquity / m.Years
	}
}

// AddBenchmarkStats computes benchmark-relative statistics. The two curves are
// aligned on date; days present in only one are skipped.
func (m *Metrics) AddBenchmarkStats(strategy, benchmark []EquityPoint, riskFreeRate float64) {
	sr, br := alignedReturns(strategy, benchmark)
	if len(sr) < 3 {
		return
	}
	m.BetaVsBenchmark = Ratio(Beta(sr, br))
	m.CorrelationVsBench = Ratio(Correlation(sr, br))

	diff := make([]float64, len(sr))
	for i := range sr {
		diff[i] = sr[i] - br[i]
	}
	te := Stdev(diff, len(diff))
	if !math.IsNaN(te) {
		m.TrackingError = te * math.Sqrt(TradingDaysPerYear)
		var mean float64
		for _, d := range diff {
			mean += d
		}
		mean /= float64(len(diff))
		if te > 0 {
			m.InformationRatio = Ratio(mean / te * math.Sqrt(TradingDaysPerYear))
		}
	}

	// Jensen's alpha, annualised.
	if m.BetaVsBenchmark.Defined() {
		var ms, mb float64
		for i := range sr {
			ms += sr[i]
			mb += br[i]
		}
		ms /= float64(len(sr))
		mb /= float64(len(br))
		rf := riskFreeRate / TradingDaysPerYear
		m.Alpha = Ratio(((ms - rf) - float64(m.BetaVsBenchmark)*(mb-rf)) * TradingDaysPerYear)
	}
}

// alignedReturns pairs two equity curves by date and returns their daily
// returns over the common dates.
func alignedReturns(a, b []EquityPoint) ([]float64, []float64) {
	bv := make(map[market.Day]float64, len(b))
	for _, p := range b {
		bv[p.Date] = p.Value
	}
	type pair struct {
		d    market.Day
		x, y float64
	}
	var common []pair
	for _, p := range a {
		if v, ok := bv[p.Date]; ok {
			common = append(common, pair{p.Date, p.Value, v})
		}
	}
	sort.Slice(common, func(i, j int) bool { return common[i].d < common[j].d })
	if len(common) < 2 {
		return nil, nil
	}
	ra := make([]float64, 0, len(common)-1)
	rb := make([]float64, 0, len(common)-1)
	for i := 1; i < len(common); i++ {
		if common[i-1].x <= 0 || common[i-1].y <= 0 {
			continue
		}
		ra = append(ra, common[i].x/common[i-1].x-1)
		rb = append(rb, common[i].y/common[i-1].y-1)
	}
	return ra, rb
}

// BuyAndHoldCurve builds the equity curve for holding a symbol from the first
// available bar, used for benchmark comparisons. Adjusted closes are used so
// that dividends are reinvested.
func BuyAndHoldCurve(s *market.Series, days []market.Day, startCash float64) []EquityPoint {
	if s == nil || len(days) == 0 {
		return nil
	}
	var base float64
	out := make([]EquityPoint, 0, len(days))
	peak := 0.0
	for _, d := range days {
		bar, ok := s.AsOf(d)
		if !ok || bar.AdjClose <= 0 {
			continue
		}
		if base == 0 {
			base = bar.AdjClose
		}
		v := startCash * bar.AdjClose / base
		if v > peak {
			peak = v
		}
		dd := 0.0
		if peak > 0 {
			dd = v/peak - 1
		}
		ret := 0.0
		if n := len(out); n > 0 && out[n-1].Value > 0 {
			ret = v/out[n-1].Value - 1
		}
		out = append(out, EquityPoint{
			Date: d, Value: v, Return: ret, Drawdown: dd, Exposure: 1,
		})
	}
	return out
}
