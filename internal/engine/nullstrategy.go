package engine

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/charbelkassab/pyrite/internal/market"
)

// NullStrategy is where a strategy lands against random trading with the same
// habits.
//
// The deflated Sharpe and the probability of backtest overfitting both answer
// "is the winner of my search real?". This answers a blunter question that
// comes first: is the strategy better than being in the market at arbitrary
// times, with the same number of trades, the same holding periods and the same
// capital at risk? A strategy that cannot beat that has no edge, however good
// its Sharpe looks.
type NullStrategy struct {
	// Trials is how many random strategies were generated. Zero means the
	// comparison was not made.
	Trials int `json:"trials"`

	// Score is the strategy measured on the same footing as the random ones:
	// its own exposure path applied to the market it traded. It is not the
	// headline Sharpe, and CompareToNullStrategies explains why it must not
	// be.
	Score Ratio `json:"score"`
	// ReportedSharpe is the run's headline Sharpe, carried here so the two
	// can be compared. They should be close. A wide gap means most of the
	// return came from something other than when the strategy was in the
	// market — symbol selection, position sizing, friction — and this test
	// then speaks only to the timing.
	ReportedSharpe Ratio `json:"reported_sharpe"`

	// Percentile is the fraction of random strategies the real one beat.
	Percentile Ratio `json:"percentile"`
	// NullMedian and NullP95 frame the distribution it was placed in.
	NullMedian Ratio `json:"null_median"`
	NullP95    Ratio `json:"null_p95"`

	// The properties the random strategies were matched on, reported so a
	// reader can check the match is the one they would have asked for.
	//
	// Episodes counts unbroken stretches in the market, which is the trade
	// count for a strategy that holds one position at a time and is close to
	// it otherwise. It is read off the exposure path rather than the round
	// trips because the exposure path is what the null rearranges, and a
	// number that described something else would be describing a different
	// test.
	Episodes       int     `json:"episodes"`
	AvgExposure    float64 `json:"avg_exposure"`
	MedianHoldBars int     `json:"median_hold_bars"`

	Verdict string `json:"verdict"`
}

// newNullStrategy returns the undefined value.
//
// Every Ratio starts NaN for the same reason the rest of this package does:
// the zero value of a Ratio is a finite 0, and a percentile of 0 reads as
// "beaten by every random strategy" when what happened is that nothing was
// computed.
func newNullStrategy() NullStrategy {
	nan := Ratio(math.NaN())
	return NullStrategy{Score: nan, ReportedSharpe: nan, Percentile: nan,
		NullMedian: nan, NullP95: nan}
}

// defaultNullTrials is how many random strategies are generated.
//
// The statistic reported is a percentile, so the resolution that matters is
// 1/trials. A thousand gives it to a tenth of a percent, which is finer than
// the question deserves and still costs a few milliseconds.
const defaultNullTrials = 1000

// minSideShare is how much of the run has to fall on each side of "in the
// market" before randomising the entries means anything.
//
// Too little time out, and there is nowhere to move the holds to: every random
// arrangement is a near-copy of the strategy and the percentile is noise
// dressed as a test. Too little time in, and the score on both sides is
// decided by a handful of bars, so the comparison cannot separate a strategy
// from a coin. Five per cent of a nine-year daily backtest is about a hundred
// bars, which is already thin in either direction.
const minSideShare = 0.05

// CompareToNullStrategies places a strategy in the distribution of random
// strategies matched to it.
//
// # What is matched, and why it is the whole statistic
//
// A random strategy that is fully invested will beat a real one that is 40%
// invested in a rising market, for reasons that have nothing to do with skill.
// So the random strategies are not "buy at random times" — they are the real
// strategy's own exposure path, cut into the contiguous stretches where it
// held something, and laid back down over the same price series at random
// non-overlapping positions.
//
// Relocating the stretches rather than resampling exposure levels is what
// makes the comparison fair, and it is exact rather than approximate:
//
//   - the number of stretches is the trade count, preserved by construction;
//   - the multiset of stretch lengths is the holding-period distribution,
//     preserved by construction;
//   - the sum of exposure over the run is unchanged, so the expected return
//     contributed by the market's drift is identical — this is the term that
//     would otherwise hand the null a free win in a bull market;
//   - the sum of squared exposure is unchanged too, so the volatility the
//     Sharpe divides by is on the same scale. A comparison that matched the
//     mean but not the second moment would flatter whichever side happened to
//     concentrate its exposure.
//
// Only the timing is randomised. The prices are not touched, which is the
// point: resampling the prices would test whether the market's own path was
// lucky, and that is a different question the block bootstrap already answers.
//
// # Why Score is not the headline Sharpe
//
// A percentile is only a test if the observed value and the null values come
// out of the same measurement. The strategy's headline Sharpe carries its
// fills, its slippage and its choice of symbol; a reconstructed random path
// carries none of those. Comparing one against the other would measure the
// reconstruction error as much as the timing. So the strategy is scored the
// same way its rivals are — its real exposure path, unshuffled, over the same
// market returns — and under the null that the timing carries no information
// the real path is exchangeable with the shuffled ones, which is exactly what
// makes the percentile uniform. ReportedSharpe is carried alongside so the
// gap between the two is visible rather than hidden.
//
// marketRets is the per-bar return of the market the strategy traded, one
// shorter than curve, aligned so that marketRets[i] is the return earned
// between curve[i] and curve[i+1]. totalCostFraction is what the run paid in
// commission and slippage as a fraction of its average equity; the random
// strategies pay the same total, split evenly across their entries, so the
// null is not given a free ride on friction the real strategy paid.
func CompareToNullStrategies(curve []EquityPoint, marketRets []float64, totalCostFraction float64, sc Scale, trials int, seed int64) NullStrategy {
	ns := newNullStrategy()
	if trials <= 0 {
		trials = defaultNullTrials
	}
	if len(curve) < 2 || len(marketRets) != len(curve)-1 {
		return ns
	}
	n := len(marketRets)
	// Sixty bars is about a quarter of a trading year. Below that the Sharpe
	// of any one arrangement is decided by a handful of returns, so ranking
	// arrangements against each other is ranking noise.
	if n < 60 {
		return ns
	}

	// Exposure at curve[i] is what was held going into bar i+1, so it is the
	// exposure that earned marketRets[i]. Off-by-one here would shift every
	// episode by a bar and quietly bias the reconstruction.
	expo := make([]float64, n)
	var held int
	var sum float64
	for i := 0; i < n; i++ {
		e := curve[i].Exposure
		if e < 0 {
			// Exposure is gross, so this should not happen; treat it as flat
			// rather than inventing a short the rest of the code cannot model.
			e = 0
		}
		expo[i] = e
		sum += e
		if e > 0 {
			held++
		}
	}
	ns.AvgExposure = sum / float64(n)

	episodes := exposureEpisodes(expo)
	free := n - held
	// The cases this test has nothing to say about. Always-invested and
	// barely-invested strategies are not exempt from scrutiny — the question
	// for the first is which assets it picked rather than when it held them —
	// but this is not the statistic that answers either one, and reporting a
	// number anyway is how a meaningless percentile ends up in a report.
	if len(episodes) == 0 {
		ns.Verdict = "the strategy never held a position, so there is nothing to compare"
		return ns
	}
	if float64(free) < minSideShare*float64(n) {
		ns.Verdict = fmt.Sprintf("the strategy was invested on %.0f%% of bars, so there is no "+
			"entry timing left to randomise — for a strategy that is always in the market the "+
			"question is what it held, not when", 100*float64(held)/float64(n))
		return ns
	}
	if float64(held) < minSideShare*float64(n) {
		ns.Verdict = fmt.Sprintf("the strategy was in the market on only %.0f%% of bars, too "+
			"little for this comparison to separate it from any other few days",
			100*float64(held)/float64(n))
		return ns
	}
	ns.Episodes = len(episodes)
	ns.MedianHoldBars = medianLength(episodes)

	costPerEpisode := 0.0
	if totalCostFraction > 0 {
		costPerEpisode = totalCostFraction / float64(len(episodes))
	}

	// The strategy's own arrangement, scored by the same code path as its
	// rivals so that nothing but the ordering differs.
	path := make([]float64, n)
	rets := make([]float64, n)
	entries := layout(path, episodes, identityOrder(len(episodes)), leadingGaps(expo, episodes))
	observed := scoreExposurePath(rets, path, marketRets, entries, costPerEpisode, sc)
	if math.IsNaN(observed) {
		return ns
	}
	ns.Score = Ratio(observed)

	rng := newLCG(seed)
	order := make([]int, len(episodes))
	gaps := make([]int, len(episodes)+1)
	scores := make([]float64, 0, trials)
	for t := 0; t < trials; t++ {
		randomOrder(order, rng)
		randomGaps(gaps, free, rng)
		entries = layout(path, episodes, order, gaps)
		if s := scoreExposurePath(rets, path, marketRets, entries, costPerEpisode, sc); !math.IsNaN(s) {
			scores = append(scores, s)
		}
	}
	if len(scores) < 20 {
		return ns
	}

	sort.Float64s(scores)
	var below int
	for _, s := range scores {
		if s < observed {
			below++
		}
	}
	ns.Trials = len(scores)
	ns.Percentile = Ratio(float64(below) / float64(len(scores)))
	ns.NullMedian = Ratio(percentileSorted(scores, 0.5))
	ns.NullP95 = Ratio(percentileSorted(scores, 0.95))
	ns.Verdict = nullVerdict(ns)
	return ns
}

// RunNullStrategy compares a completed run to random trading with the same
// habits, loading the price series it needs from the store.
//
// It costs one cached data read and a few thousand arithmetic passes over the
// return series — no backtests are re-run.
func RunNullStrategy(ctx context.Context, res *Result, store *market.Store, trials int, seed int64) NullStrategy {
	if res == nil {
		return newNullStrategy()
	}
	mkt := TradedMarketReturns(ctx, res, store)
	if mkt == nil {
		return newNullStrategy()
	}
	sc := res.Spec.Scale()
	ns := CompareToNullStrategies(res.Curve, mkt, costFraction(res), sc, trials, seed)
	ns.ReportedSharpe = res.Metrics.Sharpe
	return ns
}

// costFraction is what the run paid in friction, as a fraction of the equity
// it had to pay it out of.
func costFraction(res *Result) float64 {
	if res.Metrics.TotalCosts <= 0 || len(res.Curve) == 0 {
		return 0
	}
	var sum float64
	for _, p := range res.Curve {
		sum += p.Value
	}
	avg := sum / float64(len(res.Curve))
	if avg <= 0 {
		return 0
	}
	return res.Metrics.TotalCosts / avg
}

// TradedMarketReturns builds the per-bar return of an equally weighted basket
// of the symbols a run actually traded, aligned to its equity curve.
//
// Equal weight rather than the weights the strategy chose, because a strategy
// with no skill has no reason to prefer one name over another: the basket is
// what a random entry gets in expectation. Adjusted closes are used so the
// basket is a total return, matching the strategy's own accounting.
//
// The symbols come from the round trips rather than the declared universe. A
// strategy that declares fifty names and trades three should be compared
// against the three it can actually be in.
func TradedMarketReturns(ctx context.Context, res *Result, store *market.Store) []float64 {
	if res == nil || store == nil || len(res.Curve) < 2 {
		return nil
	}
	seen := map[string]bool{}
	var symbols []string
	for _, t := range res.Trades {
		if t.Symbol != "" && !seen[t.Symbol] {
			seen[t.Symbol] = true
			symbols = append(symbols, t.Symbol)
		}
	}
	if len(symbols) == 0 {
		symbols = append(symbols, res.Spec.Universe...)
	}
	if len(symbols) == 0 {
		return nil
	}
	sort.Strings(symbols)

	from, to := res.Curve[0].Date, res.Curve[len(res.Curve)-1].Date
	iv := res.Spec.Interval
	if !iv.Valid() {
		iv = market.DefaultInterval
	}
	series, _ := store.GetManyInterval(ctx, symbols, from, to, iv)
	if len(series) == 0 {
		return nil
	}

	out := make([]float64, len(res.Curve)-1)
	var usable int
	for i := 1; i < len(res.Curve); i++ {
		var sum float64
		var n int
		for _, s := range series {
			prev, ok := s.At(res.Curve[i-1].Date)
			if !ok || prev.AdjClose <= 0 {
				continue
			}
			cur, ok := s.At(res.Curve[i].Date)
			if !ok || cur.AdjClose <= 0 {
				continue
			}
			sum += cur.AdjClose/prev.AdjClose - 1
			n++
		}
		if n > 0 {
			out[i-1] = sum / float64(n)
			usable++
		}
	}
	// A basket that only lines up on a handful of bars is not a market, and
	// scoring against it would produce a confident number from nothing.
	if usable < len(out)*9/10 {
		return nil
	}
	return out
}

// exposureEpisodes cuts an exposure path into the maximal stretches where the
// strategy held something.
//
// The values inside each stretch are copied verbatim rather than flattened to
// an average, so a position that was scaled in and out keeps its shape when it
// is relocated.
func exposureEpisodes(expo []float64) [][]float64 {
	var out [][]float64
	start := -1
	for i, e := range expo {
		switch {
		case e > 0 && start < 0:
			start = i
		case e <= 0 && start >= 0:
			out = append(out, append([]float64(nil), expo[start:i]...))
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, append([]float64(nil), expo[start:]...))
	}
	return out
}

// medianLength is the median holding period in bars.
func medianLength(episodes [][]float64) int {
	lens := make([]int, len(episodes))
	for i, e := range episodes {
		lens[i] = len(e)
	}
	sort.Ints(lens)
	return lens[len(lens)/2]
}

// leadingGaps recovers the flat stretches of the strategy's own path, so its
// real arrangement can be rebuilt by the same code that builds the random
// ones. Scoring the observed value through a different code path from the null
// values is how a percentile ends up measuring the difference between two
// implementations instead of the difference between two strategies.
func leadingGaps(expo []float64, episodes [][]float64) []int {
	gaps := make([]int, len(episodes)+1)
	ep, run := 0, 0
	for i := 0; i < len(expo); i++ {
		if expo[i] <= 0 {
			run++
			continue
		}
		if ep < len(episodes) {
			gaps[ep] = run
			i += len(episodes[ep]) - 1
			ep++
			run = 0
		}
	}
	gaps[len(episodes)] = run
	return gaps
}

func identityOrder(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// layout writes the episodes into dst in the given order, separated by the
// given gaps, and returns the bar index each episode started on.
//
// dst is reused across trials: a thousand allocations of a two-thousand-bar
// path is the whole cost of this statistic if it is not.
func layout(dst []float64, episodes [][]float64, order, gaps []int) []int {
	for i := range dst {
		dst[i] = 0
	}
	entries := make([]int, 0, len(episodes))
	at := 0
	for i, idx := range order {
		at += gaps[i]
		if at >= len(dst) {
			break
		}
		ep := episodes[idx]
		entries = append(entries, at)
		copy(dst[at:], ep)
		at += len(ep)
	}
	return entries
}

// randomOrder shuffles the episode order in place, so the sequence of holding
// periods is randomised as well as where they land.
func randomOrder(order []int, rng *lcg) {
	for i := range order {
		order[i] = i
	}
	for i := len(order) - 1; i > 0; i-- {
		j := rng.intn(i + 1)
		order[i], order[j] = order[j], order[i]
	}
}

// randomGaps splits free bars into len(gaps) flat stretches, uniformly.
//
// Sorted cut points are the standard construction: it gives every arrangement
// of the episodes along the series the same probability, which is what "random
// entry timing" has to mean for the percentile to be interpretable.
func randomGaps(gaps []int, free int, rng *lcg) {
	k := len(gaps) - 1
	if k == 0 {
		gaps[0] = free
		return
	}
	cuts := make([]int, k)
	for i := range cuts {
		cuts[i] = rng.intn(free + 1)
	}
	sort.Ints(cuts)
	gaps[0] = cuts[0]
	for i := 1; i < k; i++ {
		gaps[i] = cuts[i] - cuts[i-1]
	}
	gaps[k] = free - cuts[k-1]
}

// scoreExposurePath is the annualised Sharpe of holding an exposure path
// through the market's returns, paying costPerEpisode on entry. rets is
// scratch space the caller reuses across trials.
func scoreExposurePath(rets, expo, mkt []float64, entries []int, costPerEpisode float64, sc Scale) float64 {
	for i := range expo {
		rets[i] = expo[i] * mkt[i]
	}
	for _, i := range entries {
		rets[i] -= costPerEpisode
	}
	mean, sd := meanStdev(rets)
	if sd <= 0 {
		return math.NaN()
	}
	return sc.Sharpe(mean, sd)
}

// nullVerdict states the comparison in the terms it was made in.
//
// The thresholds are the conventional ones and are worth naming: below the
// median the timing is worse than arbitrary, and 95% is the bar a one-sided
// test would ask for. Everything in between is the interesting case, because
// it is where a strategy looks good and is not.
func nullVerdict(ns NullStrategy) string {
	if !ns.Percentile.Defined() {
		return ""
	}
	// Three significant figures, not two decimal places and not none. A
	// percentile of 0.949 printed as "95%" alongside the sentence "short of
	// 95%" reads as a contradiction, and rounding it up to the threshold it
	// failed is the wrong direction to round in.
	p := float64(ns.Percentile) * 100
	matched := fmt.Sprintf("%d random strategies with the same trade count, holding periods and exposure",
		ns.Trials)
	switch {
	case p < 50:
		return fmt.Sprintf("the timing loses to the median of %s — entering at arbitrary moments would have done better", matched)
	case p < 90:
		return fmt.Sprintf("beats %.3g%% of %s — that is not evidence of an edge", p, matched)
	case p < 95:
		return fmt.Sprintf("beats %.3g%% of %s, short of the 95%% a one-sided test would ask for", p, matched)
	default:
		return fmt.Sprintf("beats %.3g%% of %s, which is the one comparison here it passes", p, matched)
	}
}
