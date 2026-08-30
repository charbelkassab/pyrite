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
type Scale struct {
	// PeriodsPerYear is how many bars of this run's size fit in a year.
	PeriodsPerYear float64
	// RiskFree is the annual risk-free rate, as a fraction.
	RiskFree float64
}

// DailyScale is the conventional daily setting.
func DailyScale(riskFree float64) Scale {
	return Scale{PeriodsPerYear: TradingDaysPerYear, RiskFree: riskFree}
}

// ScaleFor builds a Scale for a bar size.
func ScaleFor(iv market.Interval, riskFree float64) Scale {
	ppy := TradingDaysPerYear
	if iv.Valid() {
		ppy = iv.PeriodsPerYear()
	}
	return Scale{PeriodsPerYear: ppy, RiskFree: riskFree}
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
