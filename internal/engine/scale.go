package engine

import (
	"math"

	"github.com/charbelkassab/pyrite/internal/market"
)

// TradingDaysPerYear is the annualisation factor for daily bars.
//
// Kept as a named constant because it is the conventional figure and appears
// in the documentation, but nothing should reach for it directly any more:
// use a Scale, which knows the run's actual bar size.
const TradingDaysPerYear = 252.0

// Scale converts per-bar statistics into annual ones.
//
// It exists as a type rather than two loose float64 parameters because those
// two are always passed together and are trivially swappable at a call site —
// ComputeMetrics(curve, 0.02, 252) and ComputeMetrics(curve, 252, 0.02) both
// compile, and one of them silently produces a Sharpe ratio out by orders of
// magnitude. In a project whose entire argument is about not generating
// plausible wrong numbers, that is not an acceptable shape for an API.
//
// The periods figure matters more than it looks. A Sharpe ratio computed on
// 1-minute bars and annualised as though they were daily is out by roughly a
// factor of twenty, in the flattering direction.
// The bar size is only half of it. How many bars of that size a year holds
// depends on the market: 252 daily bars for a US equity, 365 for bitcoin, 261
// for spot FX. Annualising a crypto series at 252 leaves its volatility short
// by a sixth and its Sharpe scaled by the wrong root, and nothing in the
// output says so. Hence Calendar: the same trades, scored on the calendar
// that actually produced the bars.
type Scale struct {
	// PeriodsPerYear is how many bars of this run's size fit in a year.
	PeriodsPerYear float64
	// RiskFree is the annual risk-free rate, as a fraction.
	RiskFree float64
	// Calendar records which market's sessions PeriodsPerYear was counted
	// from, so a result can say what it annualised by rather than leaving a
	// reader to infer it from the number.
	Calendar market.Calendar
}

// DailyScale is the conventional daily setting.
func DailyScale(riskFree float64) Scale {
	return Scale{PeriodsPerYear: TradingDaysPerYear, RiskFree: riskFree,
		Calendar: market.CalendarUSEquity}
}

// ScaleFor builds a Scale for a bar size on the US equity calendar.
func ScaleFor(iv market.Interval, riskFree float64) Scale {
	return ScaleOn(iv, market.CalendarUSEquity, riskFree)
}

// ScaleOn builds a Scale for a bar size on a given trading calendar.
//
// An unset or unrecognised calendar means US equities, so every caller that
// has never heard of a calendar keeps the numbers it always had.
func ScaleOn(iv market.Interval, cal market.Calendar, riskFree float64) Scale {
	if !cal.Valid() {
		cal = market.CalendarUSEquity
	}
	ppy := cal.SessionsPerYear()
	if iv.Valid() {
		ppy = iv.PeriodsPerYearOn(cal)
	}
	return Scale{PeriodsPerYear: ppy, RiskFree: riskFree, Calendar: cal}
}

// Periods is the annualisation factor, never zero.
func (s Scale) Periods() float64 {
	if s.PeriodsPerYear <= 0 {
		return TradingDaysPerYear
	}
	return s.PeriodsPerYear
}

// Root is sqrt(periods), the factor a standard deviation scales by.
func (s Scale) Root() float64 { return math.Sqrt(s.Periods()) }

// PerPeriodRF is the risk-free rate over one bar.
func (s Scale) PerPeriodRF() float64 { return s.RiskFree / s.Periods() }

// Annualise scales a per-bar mean return to a year.
func (s Scale) Annualise(perPeriod float64) float64 { return perPeriod * s.Periods() }

// Sharpe computes an annualised Sharpe from a per-bar mean and deviation.
func (s Scale) Sharpe(mean, sd float64) float64 {
	if sd <= 0 {
		return math.NaN()
	}
	return (mean - s.PerPeriodRF()) / sd * s.Root()
}

// Vol annualises a per-bar standard deviation.
func (s Scale) Vol(sd float64) float64 { return sd * s.Root() }

// Market is the calendar this scale counted its sessions from.
func (s Scale) Market() market.Calendar {
	if !s.Calendar.Valid() {
		return market.CalendarUSEquity
	}
	return s.Calendar
}
