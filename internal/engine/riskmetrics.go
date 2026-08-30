package engine

import (
	"math"
	"sort"
)

// RiskMetrics are the second-order statistics: the shape of the return
// distribution, the depth and persistence of losses, and how the strategy
// behaves relative to a benchmark in up and down markets specifically.
//
// The headline table answers "how much did it make". These answer "what did it
// have to do to make it", which is the question that decides whether anyone
// could actually have held the position.
type RiskMetrics struct {
	// Omega is the ratio of probability-weighted gains to losses above and
	// below the risk-free threshold. Unlike Sharpe it makes no normality
	// assumption, so it survives fat tails.
	Omega Ratio `json:"omega"`
	// UlcerIndex is the root-mean-square drawdown across the whole curve. Max
	// drawdown reports the single worst moment; the ulcer index reports how
	// much of the run was spent underwater and how deep, which is much closer
	// to what makes a position hard to hold.
	UlcerIndex float64 `json:"ulcer_index"`
	// MartinRatio is CAGR over the ulcer index.
	MartinRatio Ratio `json:"martin_ratio"`

	// TailRatio compares the size of the right tail to the left: the 95th
	// percentile daily return over the absolute 5th percentile. Below 1 means
	// the losses are bigger than the gains even if the average is positive.
	TailRatio Ratio `json:"tail_ratio"`
	// VaR is the loss that a given fraction of days does not exceed; CVaR is
	// the average loss on the days that do exceed it. CVaR is the honest one:
	// VaR tells you the edge of the cliff, CVaR tells you how far the fall is.
	VaR95  float64 `json:"var_95"`
	CVaR95 float64 `json:"cvar_95"`
	VaR99  float64 `json:"var_99"`
	CVaR99 float64 `json:"cvar_99"`

	// Skew and ExcessKurtosis describe the distribution's asymmetry and how
	// fat its tails are. A strategy with strong Sharpe, negative skew and
	// high kurtosis is selling insurance, whatever its description says.
	Skew           float64 `json:"skew"`
	ExcessKurtosis float64 `json:"excess_kurtosis"`

	// GainToPain is total return over the sum of all losing days.
	GainToPain Ratio `json:"gain_to_pain"`
	// KellyFraction is the growth-optimal leverage implied by the return
	// distribution. Reported because a Kelly far above 1 usually means the
	// sample is too small or too kind, not that leverage is free.
	KellyFraction float64 `json:"kelly_fraction"`
	// EquityR2 is how well the log equity curve fits a straight line. Near 1
	// is steady compounding; a low value with a good total return means the
	// result came from a few episodes.
	EquityR2 float64 `json:"equity_r2"`

	// Capture ratios, populated when a benchmark is supplied. Up capture is
	// the share of the benchmark's gains the strategy caught on days the
	// benchmark rose; down capture is the share of its losses taken. The pair
	// is more informative than beta because it is allowed to be asymmetric.
	UpCapture   Ratio `json:"up_capture,omitempty"`
	DownCapture Ratio `json:"down_capture,omitempty"`
}

// ComputeRiskMetrics derives distribution and drawdown statistics from an
// equity curve.
func ComputeRiskMetrics(curve []EquityPoint, cagr float64, sc Scale) RiskMetrics {
	nan := Ratio(math.NaN())
	r := RiskMetrics{Omega: nan, MartinRatio: nan, TailRatio: nan, GainToPain: nan,
		UpCapture: nan, DownCapture: nan}
	if len(curve) < 3 {
		return r
	}

	rets := dailyReturns(curve)
	if len(rets) < 2 {
		return r
	}
	thr := sc.PerPeriodRF()

	// Omega and gain-to-pain share a pass over the returns.
	var above, below, total, painSum float64
	for _, x := range rets {
		if x > thr {
			above += x - thr
		} else {
			below += thr - x
		}
		total += x
		if x < 0 {
			painSum += -x
		}
	}
	if below > 0 {
		r.Omega = Ratio(above / below)
	} else if above > 0 {
		r.Omega = Ratio(math.Inf(1))
	}
	if painSum > 0 {
		r.GainToPain = Ratio(total / painSum)
	}

	// Ulcer index over the drawdown series already carried on the curve.
	var ddSS float64
	for _, p := range curve {
		ddSS += p.Drawdown * p.Drawdown
	}
	r.UlcerIndex = math.Sqrt(ddSS / float64(len(curve)))
	if r.UlcerIndex > 0 {
		r.MartinRatio = Ratio(cagr / r.UlcerIndex)
	}

	sorted := append([]float64(nil), rets...)
	sort.Float64s(sorted)
	p05 := percentileSorted(sorted, 0.05)
	p95 := percentileSorted(sorted, 0.95)
	if l := math.Abs(p05); l > 0 {
		r.TailRatio = Ratio(math.Abs(p95) / l)
	}
	r.VaR95, r.CVaR95 = p05, conditionalTail(sorted, 0.05)
	r.VaR99, r.CVaR99 = percentileSorted(sorted, 0.01), conditionalTail(sorted, 0.01)

	mean, sd := meanStdev(rets)
	if sd > 0 {
		n := float64(len(rets))
		var m3, m4 float64
		for _, x := range rets {
			z := (x - mean) / sd
			m3 += z * z * z
			m4 += z * z * z * z
		}
		r.Skew = m3 / n
		r.ExcessKurtosis = m4/n - 3
		// Continuous Kelly: mean over variance, in daily units.
		r.KellyFraction = mean / (sd * sd)
	}

	r.EquityR2 = logEquityR2(curve)
	return r
}

// AddCapture computes up and down capture against an aligned benchmark curve.
func (r *RiskMetrics) AddCapture(strategy, benchmark []EquityPoint) {
	sr, br := alignedReturns(strategy, benchmark)
	if len(sr) < 3 {
		return
	}
	var sUp, bUp, sDown, bDown float64
	var nUp, nDown int
	for i := range sr {
		if br[i] > 0 {
			sUp += sr[i]
			bUp += br[i]
			nUp++
		} else if br[i] < 0 {
			sDown += sr[i]
			bDown += br[i]
			nDown++
		}
	}
	if nUp > 0 && bUp != 0 {
		r.UpCapture = Ratio((sUp / float64(nUp)) / (bUp / float64(nUp)))
	}
	if nDown > 0 && bDown != 0 {
		r.DownCapture = Ratio((sDown / float64(nDown)) / (bDown / float64(nDown)))
	}
}

// RollingPoint is one observation of a rolling statistic.
type RollingPoint struct {
	Date   string  `json:"date"`
	Sharpe Ratio   `json:"sharpe"`
	Vol    float64 `json:"volatility"`
	Beta   Ratio   `json:"beta,omitempty"`
}

// RollingStats computes a trailing window of Sharpe, volatility and — when a
// benchmark is supplied — beta.
//
// A single Sharpe for a ten-year run hides the fact that it was 2.4 for three
// years and 0.1 for seven. The rolling series is what makes that visible, and
// it is the cheapest possible defence against a headline number.
func RollingStats(curve, benchmark []EquityPoint, window int, sc Scale) []RollingPoint {
	if window < 2 || len(curve) <= window {
		return nil
	}
	rets := dailyReturns(curve)
	var bench []float64
	if len(benchmark) > 0 {
		if sr, br := alignedReturns(curve, benchmark); len(sr) == len(rets) {
			bench = br
		}
	}

	out := make([]RollingPoint, 0, len(rets)-window+1)
	for i := window; i <= len(rets); i++ {
		w := rets[i-window : i]
		mean, sd := meanStdev(w)
		p := RollingPoint{
			Date:   string(curve[i].Date),
			Sharpe: Ratio(math.NaN()),
			Beta:   Ratio(math.NaN()),
		}
		if sd > 0 {
			p.Sharpe = Ratio(sc.Sharpe(mean, sd))
			p.Vol = sc.Vol(sd)
		}
		if bench != nil {
			p.Beta = Ratio(Beta(w, bench[i-window:i]))
		}
		out = append(out, p)
	}
	return out
}

// dailyReturns converts an equity curve to simple daily returns.
func dailyReturns(curve []EquityPoint) []float64 {
	if len(curve) < 2 {
		return nil
	}
	out := make([]float64, 0, len(curve)-1)
	for i := 1; i < len(curve); i++ {
		if curve[i-1].Value <= 0 {
			out = append(out, 0)
			continue
		}
		out = append(out, curve[i].Value/curve[i-1].Value-1)
	}
	return out
}

func meanStdev(xs []float64) (mean, sd float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	if len(xs) < 2 {
		return mean, 0
	}
	var ss float64
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	return mean, math.Sqrt(ss / float64(len(xs)-1))
}

// percentileSorted interpolates a percentile from an already-sorted slice.
func percentileSorted(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := p * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo < 0 {
		lo = 0
	}
	if hi >= len(sorted) {
		hi = len(sorted) - 1
	}
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// conditionalTail averages the worst p of the distribution.
func conditionalTail(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	n := int(math.Ceil(p * float64(len(sorted))))
	if n < 1 {
		n = 1
	}
	if n > len(sorted) {
		n = len(sorted)
	}
	var sum float64
	for _, x := range sorted[:n] {
		sum += x
	}
	return sum / float64(n)
}

// logEquityR2 regresses log equity on the bar index and returns the R².
func logEquityR2(curve []EquityPoint) float64 {
	n := 0
	var sx, sy, sxx, sxy, syy float64
	for i, p := range curve {
		if p.Value <= 0 {
			continue
		}
		x := float64(i)
		y := math.Log(p.Value)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
		syy += y * y
		n++
	}
	if n < 3 {
		return 0
	}
	fn := float64(n)
	num := fn*sxy - sx*sy
	den := (fn*sxx - sx*sx) * (fn*syy - sy*sy)
	if den <= 0 {
		return 0
	}
	r := num / math.Sqrt(den)
	return r * r
}
