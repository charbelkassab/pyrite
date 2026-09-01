package engine

import (
	"fmt"
	"math"
)

// RealityCheck is White's Reality Check and Hansen's Superior Predictive
// Ability test over the trials of a search.
//
// Both answer the same question and disagree usefully about it: taking the
// whole set of variants that were tried as the object of study rather than
// only the winner, can the null that the best of them has no positive expected
// performance be rejected? The deflated Sharpe asks this parametrically, from
// the spread of the trial scores. These ask it by resampling the trials' actual
// return series, which makes no assumption about the shape of the return
// distribution and — more to the point — none about the trials being
// independent. In a parameter sweep they never are: two adjacent cells of a
// grid trade almost the same days, and treating them as two independent
// attempts overstates how much searching was really done.
type RealityCheck struct {
	// Trials and Periods are the shape of the matrix the test ran on. Zero
	// trials means it was not computed.
	Trials  int `json:"trials"`
	Periods int `json:"periods"`
	// Bootstraps is how many resamples the p-values were counted over, so the
	// resolution of a small p-value is visible: nothing below 1/Bootstraps is
	// distinguishable from zero.
	Bootstraps int `json:"bootstraps"`
	// BlockLength is the mean block of the stationary bootstrap, in bars.
	BlockLength int `json:"block_length"`

	// BestExcess is the annualised mean return, over the risk-free rate, of
	// the trial the test is built around. It is the quantity being tested,
	// reported so the p-values below have something to be about.
	BestExcess float64 `json:"best_excess"`
	// BestStatistic is that trial's studentised statistic, sqrt(T) times its
	// mean over its bootstrap standard error. Read as a t-statistic for a
	// single strategy, before any correction for the number tried.
	BestStatistic Ratio `json:"best_statistic"`

	// RealityCheckP is White's p-value. It recentres every trial at its own
	// sample mean, so every trial in the set — including the hopeless ones —
	// contributes a fresh draw to the maximum the critical value is built
	// from. That makes it conservative, and a sweep is mostly hopeless
	// trials.
	RealityCheckP Ratio `json:"reality_check_p"`
	// SPAP is Hansen's p-value: studentised, and with trials that are poor
	// beyond doubt recentred at zero instead of at their own mean, so they no
	// longer inflate the bar the winner has to clear.
	//
	// It is the more powerful of the two exactly when there is something for
	// the recentring to discard. In a sweep where every cell is respectable
	// there is not, and the two then differ only by the studentising, which
	// can fall either way. So neither is "the" answer: read the larger of the
	// pair, because rejecting on one test and not the other is not a
	// rejection.
	SPAP Ratio `json:"spa_p"`

	Verdict string `json:"verdict"`
}

// newRealityCheck returns the undefined value, with every Ratio NaN rather
// than a finite zero. A p-value of 0 is the strongest claim this struct can
// make, and it is not one to arrive at by having computed nothing.
func newRealityCheck() RealityCheck {
	return RealityCheck{
		BestStatistic: Ratio(math.NaN()),
		RealityCheckP: Ratio(math.NaN()),
		SPAP:          Ratio(math.NaN()),
	}
}

const (
	// defaultBootstraps is the number of stationary-bootstrap resamples.
	// A thousand puts the standard error of a p-value near 0.05 at about
	// 0.007, which is finer than the decisions anybody makes from it.
	defaultBootstraps = 1000
	// defaultRCBlock is the mean block length, in bars. The same month the
	// equity-curve bootstrap uses, and for the same reason: it is long enough
	// to carry the volatility clustering in daily returns through the
	// resample. Erring long is the safe direction — it widens the bootstrap
	// distribution and so raises the p-values.
	defaultRCBlock = 21
	// maxRCCells caps the arithmetic. Cost is bootstraps × trials × periods,
	// and beyond this the resample count is cut rather than the search being
	// made to wait minutes for a third decimal place.
	maxRCCells = 600 << 20
	// minBootstraps is the floor that cut respects. Below this the p-value is
	// too coarse to report.
	minBootstraps = 200
)

// AddRealityCheck runs White's Reality Check and Hansen's SPA over the sweep's
// per-trial return series.
//
// returns is the same aligned matrix AddPBO consumes — one row per trial, one
// column per period. No backtests are re-run.
//
// The benchmark both tests are stated against is holding cash at the run's
// risk-free rate, so the null is "the best variant does not beat sitting it
// out". That bar is a low one for anything long-biased in a rising market, and
// the verdict says so rather than letting a small p-value be read as an edge.
func (r *Robustness) AddRealityCheck(returns [][]float64, sc Scale, bootstraps, blockLen int, seed int64) {
	// As with PBO, every path out of here leaves the statistic explicitly
	// undefined. A p-value that defaulted to zero would be the most flattering
	// possible answer, arrived at by having computed nothing.
	r.RealityCheck = newRealityCheck()

	k := len(returns)
	if k == 0 {
		return
	}
	t := len(returns[0])
	// Below a couple of hundred bars the mean of a return series is too noisy
	// for a bootstrap around it to say anything, and the block length would
	// be a large fraction of the sample.
	if t < 200 {
		return
	}
	for _, row := range returns {
		if len(row) != t {
			return // ragged input: refuse rather than compare unlike things
		}
	}
	if bootstraps <= 0 {
		bootstraps = defaultBootstraps
	}
	if blockLen <= 0 {
		blockLen = defaultRCBlock
	}
	if cells := k * t; cells > 0 && bootstraps*cells > maxRCCells {
		bootstraps = maxRCCells / cells
	}
	if bootstraps < minBootstraps {
		return
	}

	// Performance relative to the benchmark. The risk-free rate is per bar,
	// through Scale, so an intraday sweep is not silently compared against an
	// annual rate.
	rf := sc.PerPeriodRF()
	mean := make([]float64, k)
	for i, row := range returns {
		var s float64
		for _, x := range row {
			s += x
		}
		mean[i] = s/float64(t) - rf
	}

	// The bootstrap means. Every trial is resampled on the *same* draw of time
	// indices within a replication — resampling each trial independently would
	// destroy the cross-sectional dependence between them, which is the only
	// thing standing between this test and the deflated Sharpe.
	sqrtT := math.Sqrt(float64(t))
	rng := newLCG(seed)
	idx := make([]int, t)
	boot := make([][]float64, bootstraps)
	for b := range boot {
		stationaryIndices(idx, float64(blockLen), rng)
		row := make([]float64, k)
		for i, series := range returns {
			var s float64
			for _, j := range idx {
				s += series[j]
			}
			row[i] = s/float64(t) - rf
		}
		boot[b] = row
	}

	// The standard error of sqrt(T) times each trial's mean, taken from the
	// bootstrap itself. Doing it this way rather than with a separate kernel
	// estimator means the studentisation and the critical value are built from
	// exactly the same dependence structure.
	omega := make([]float64, k)
	for i := 0; i < k; i++ {
		var ss float64
		for b := range boot {
			d := sqrtT * (boot[b][i] - mean[i])
			ss += d * d
		}
		omega[i] = math.Sqrt(ss / float64(bootstraps))
	}

	// White's statistic: the largest mean in the set, unstudentised.
	v := math.Inf(-1)
	best := 0
	for i := 0; i < k; i++ {
		if s := sqrtT * mean[i]; s > v {
			v, best = s, i
		}
	}

	// Hansen's: the same maximum, studentised, and floored at zero because a
	// set in which nothing beats the benchmark cannot reject the null.
	tSPA := 0.0
	for i := 0; i < k; i++ {
		if omega[i] <= 0 {
			continue
		}
		if s := sqrtT * mean[i] / omega[i]; s > tSPA {
			tSPA = s
		}
	}

	// Hansen's consistent recentring. A trial whose mean is worse than
	// -sqrt(2 log log T) standard errors is poor beyond reasonable doubt, so
	// it is recentred at zero: its bootstrap draws then sit far below the
	// maximum and stop contributing noise to the critical value. Recentring
	// every trial at its own mean instead is exactly White's choice, and is
	// why the Reality Check loses power as a sweep adds bad cells.
	threshold := -math.Sqrt(2 * math.Log(math.Log(float64(t))))
	g := make([]float64, k)
	for i := 0; i < k; i++ {
		if omega[i] > 0 && sqrtT*mean[i]/omega[i] < threshold {
			continue // stays at zero
		}
		g[i] = mean[i]
	}

	var rcExceed, spaExceed int
	for b := range boot {
		vb, zb := math.Inf(-1), 0.0
		for i := 0; i < k; i++ {
			if s := sqrtT * (boot[b][i] - mean[i]); s > vb {
				vb = s
			}
			if omega[i] > 0 {
				if s := sqrtT * (boot[b][i] - g[i]) / omega[i]; s > zb {
					zb = s
				}
			}
		}
		if vb > v {
			rcExceed++
		}
		if zb > tSPA {
			spaExceed++
		}
	}

	r.RealityCheck = RealityCheck{
		Trials:        k,
		Periods:       t,
		Bootstraps:    bootstraps,
		BlockLength:   blockLen,
		BestExcess:    sc.Annualise(mean[best]),
		BestStatistic: Ratio(math.NaN()),
		RealityCheckP: Ratio(float64(rcExceed) / float64(bootstraps)),
		SPAP:          Ratio(float64(spaExceed) / float64(bootstraps)),
	}
	if omega[best] > 0 {
		r.RealityCheck.BestStatistic = Ratio(sqrtT * mean[best] / omega[best])
	}
	r.RealityCheck.Verdict = realityCheckVerdict(r.RealityCheck)
}

// realityCheckVerdict states what the two p-values mean together.
//
// They are reported as a pair because their disagreement is informative, and
// the conclusion is taken from the weaker of the two: a result that rejects on
// one test and not the other has not rejected anything.
func realityCheckVerdict(rc RealityCheck) string {
	if !rc.RealityCheckP.Defined() || !rc.SPAP.Defined() {
		return ""
	}
	white, spa := float64(rc.RealityCheckP), float64(rc.SPAP)
	res := fmt.Sprintf("reality check p=%s, SPA p=%s over %d trials",
		FormatPValue(white, rc.Bootstraps), FormatPValue(spa, rc.Bootstraps), rc.Trials)

	switch {
	case white > 0.1 && spa <= 0.05:
		// The case Hansen's correction exists for.
		res += ": the winner survives once the search's dead cells stop counting against it, and does not survive when they do"
	case math.Max(white, spa) <= 0.05:
		res += ": the best of them does beat holding cash, though beating cash is a low bar for anything that is long the market most of the time"
	case math.Min(white, spa) > 0.1:
		res += ": the best of them does not beat holding cash by more than the search itself explains"
	default:
		res += ": borderline, which for a test this blunt means no"
	}
	return res
}

// FormatPValue renders a p-value, and refuses to print a zero the bootstrap
// cannot support. With B resamples nothing below 1/B is measurable, and
// "0.000" claims a precision that was never there.
func FormatPValue(p float64, bootstraps int) string {
	if bootstraps > 0 && p < 1/float64(bootstraps) {
		return fmt.Sprintf("<%.3f", 1/float64(bootstraps))
	}
	return fmt.Sprintf("%.3f", p)
}
