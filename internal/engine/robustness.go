package engine

import (
	"fmt"
	"math"
	"math/bits"
	"sort"
)

// eulerMascheroni appears in the expected-maximum-Sharpe correction.
const eulerMascheroni = 0.5772156649015329

// Robustness is the answer to the only question a parameter sweep is really
// for.
//
// The naive reading of a sweep is "the best cell is the strategy". That
// reading is wrong often enough to be dangerous: search hard enough over
// enough parameters and something will look excellent by chance alone. These
// statistics measure how much of the winner is signal and how much is the
// search itself.
type Robustness struct {
	Trials int `json:"trials"`
	// BestScore and MedianScore frame the search. A best far above the
	// median with nothing in between is a spike, not an edge.
	BestScore   float64 `json:"best_score"`
	MedianScore float64 `json:"median_score"`
	WorstScore  float64 `json:"worst_score"`
	ScoreStdev  float64 `json:"score_stdev"`
	// PositiveShare is the fraction of combinations that scored above zero.
	// A strategy whose edge is real usually works over much of its parameter
	// space; one that works at exactly one setting was fitted to the sample.
	PositiveShare float64 `json:"positive_share"`

	// ExpectedMaxScore is what the best of this many trials would score
	// under a null of no skill, given the spread actually observed. A best
	// score below it is not evidence of anything.
	ExpectedMaxScore float64 `json:"expected_max_score"`
	// DeflatedSharpe is the probability that the winner's Sharpe is
	// genuinely above zero once the number of trials, the skew and the fat
	// tails of its returns are all accounted for. Read it as a confidence:
	// above 0.95 is meaningful, below 0.5 means the search found noise.
	DeflatedSharpe Ratio `json:"deflated_sharpe"`

	// PBO is the probability of backtest overfitting, from combinatorially
	// symmetric cross-validation: across many train/test splits of the same
	// period, how often does the configuration that won in-sample land below
	// median out-of-sample. 0.5 is a coin flip, which is what pure
	// overfitting looks like.
	PBO Ratio `json:"pbo"`
	// PBOSplits is how many train/test partitions PBO was measured over,
	// and 0 means it could not be computed.
	PBOSplits int `json:"pbo_splits"`

	// PlateauRatio compares the winner to its immediate neighbours in the
	// parameter grid. Near 1 means the winner sits on a broad plateau and
	// a small mis-specification would not have destroyed it. Near 0 means
	// it is an isolated spike — the single most reliable visual sign of
	// overfitting, made numeric.
	PlateauRatio Ratio `json:"plateau_ratio"`
	// Neighbours is how many grid neighbours the ratio was computed over.
	Neighbours int `json:"neighbours"`

	// RealityCheck tests the whole trial set at once rather than the winner
	// alone: can the null that the best of them has no positive expected
	// performance be rejected, allowing for the fact that they were all tried?
	RealityCheck RealityCheck `json:"reality_check"`
	// NullStrategy asks the blunter question the statistics above skip past —
	// whether the winner beats trading at random with the same habits.
	NullStrategy NullStrategy `json:"null_strategy"`

	// Verdict is the plain-English reading of the numbers above.
	Verdict string `json:"verdict"`
}

// AssessRobustness measures a completed sweep.
//
// Rows must carry Score. PBO additionally needs per-combination return series
// and is computed by AddPBO when they were retained.
func AssessRobustness(rows []SweepRow, objective string) Robustness {
	r := Robustness{
		Trials:         len(rows),
		DeflatedSharpe: Ratio(math.NaN()),
		PBO:            Ratio(math.NaN()),
		PlateauRatio:   Ratio(math.NaN()),
		RealityCheck:   newRealityCheck(),
		NullStrategy:   newNullStrategy(),
	}
	scores := make([]float64, 0, len(rows))
	var positive int
	for _, row := range rows {
		if row.Error != "" || !row.Score.Defined() {
			continue
		}
		scores = append(scores, float64(row.Score))
		if row.Score > 0 {
			positive++
		}
	}
	if len(scores) == 0 {
		r.Verdict = "every combination failed, so there is nothing to assess"
		return r
	}

	sorted := append([]float64(nil), scores...)
	sort.Float64s(sorted)
	r.BestScore = sorted[len(sorted)-1]
	r.WorstScore = sorted[0]
	r.MedianScore = percentileSorted(sorted, 0.5)
	r.PositiveShare = float64(positive) / float64(len(scores))
	_, r.ScoreStdev = meanStdev(scores)

	// The expected maximum of N draws from the observed spread. This is the
	// bar the winner has to clear before it is worth discussing at all.
	if len(scores) > 1 && r.ScoreStdev > 0 {
		r.ExpectedMaxScore = expectedMax(r.ScoreStdev, len(scores))
	}
	r.PlateauRatio, r.Neighbours = plateauRatio(rows)

	// The deflated Sharpe and PBO both need inputs AssessRobustness does not
	// have — the winner's return series, and every trial's. Callers add them
	// and then call Finish, which is what writes the verdict.
	r.Finish(objective)
	return r
}

// Finish writes the verdict from whatever statistics have been added.
//
// It is separate from AssessRobustness because PBO and the deflated Sharpe
// arrive later, and a verdict written before them would silently omit the two
// most important numbers in the report.
func (r *Robustness) Finish(objective string) {
	r.Verdict = verdict(*r, objective)
}

// ExpectedMaxScore is the score the best of n trials reaches by luck alone,
// given the spread of scores observed.
//
// Exported so the research ledger can ask it of a whole history of searches
// rather than of one sweep. Two numbers on the same screen answering the same
// question by different arithmetic would be worse than either of them alone,
// so there is one formula and this is the way in.
func ExpectedMaxScore(spread float64, n int) float64 { return expectedMax(spread, n) }

// expectedMax is the expected maximum of N independent draws from a
// zero-mean normal with the given standard deviation.
//
// The two-term form is Bailey and López de Prado's: a single-term extreme
// value approximation is noticeably optimistic at the trial counts a sweep
// actually reaches.
func expectedMax(sd float64, n int) float64 {
	if n < 2 || sd <= 0 {
		return 0
	}
	fn := float64(n)
	a := normalInv(1 - 1/fn)
	b := normalInv(1 - 1/(fn*math.E))
	return sd * ((1-eulerMascheroni)*a + eulerMascheroni*b)
}

// AddDeflatedSharpe computes the probability that the winner's Sharpe is
// genuinely positive, given how many strategies were tried to find it.
//
// This is Bailey and López de Prado's deflated Sharpe ratio, and it needs
// three things the headline number does not: how many trials were run, how
// much the trials varied, and the higher moments of the winner's own returns.
// Negative skew and fat tails both inflate a Sharpe, so a strategy that sells
// tail risk scores well on the naive measure and poorly here — which is the
// entire point of the correction.
//
// bestCurve is the winning strategy's equity, and trialScores are the
// annualised Sharpes across the whole search.
func (r *Robustness) AddDeflatedSharpe(bestCurve []EquityPoint, trialScores []float64, sc Scale) {
	r.DeflatedSharpe = Ratio(math.NaN())

	rets := dailyReturns(bestCurve)
	if len(rets) < 30 || len(trialScores) < 2 {
		return
	}
	mean, sd := meanStdev(rets)
	if sd <= 0 {
		return
	}
	sr := mean / sd // per-period, which is what the formula is stated in

	// Skew and kurtosis of the winner's returns.
	n := float64(len(rets))
	var m3, m4 float64
	for _, x := range rets {
		z := (x - mean) / sd
		m3 += z * z * z
		m4 += z * z * z * z
	}
	skew := m3 / n
	kurt := m4 / n // non-excess, as the formula requires

	// The spread of trial Sharpes, de-annualised to match sr.
	_, spread := meanStdev(trialScores)
	spread /= sc.Root()
	if spread <= 0 {
		return
	}
	sr0 := expectedMax(spread, len(trialScores))

	denom := 1 - skew*sr + (kurt-1)/4*sr*sr
	if denom <= 0 {
		return
	}
	z := (sr - sr0) * math.Sqrt(n-1) / math.Sqrt(denom)
	r.DeflatedSharpe = Ratio(normalCDF(z))
}

// plateauRatio measures whether the best cell sits on a ridge or alone.
//
// Neighbours are combinations that differ in exactly one parameter, by exactly
// one step on that parameter's grid. The ratio is their mean score over the
// winner's — so 1.0 means the surface is flat around the peak and 0 means it
// falls straight off.
func plateauRatio(rows []SweepRow) (Ratio, int) {
	var best *SweepRow
	for i := range rows {
		if rows[i].Error != "" || !rows[i].Score.Defined() {
			continue
		}
		if best == nil || rows[i].Score > best.Score {
			best = &rows[i]
		}
	}
	if best == nil || len(best.Params) == 0 {
		return Ratio(math.NaN()), 0
	}

	// Grid positions per parameter, so "one step away" is well defined even
	// for non-uniform grids.
	axisValues := map[string][]float64{}
	for _, row := range rows {
		for k, v := range row.Params {
			f, ok := toFloatOK(v)
			if !ok {
				continue
			}
			vals := axisValues[k]
			found := false
			for _, x := range vals {
				if math.Abs(x-f) < 1e-9 {
					found = true
					break
				}
			}
			if !found {
				axisValues[k] = append(vals, f)
			}
		}
	}
	for k := range axisValues {
		sort.Float64s(axisValues[k])
	}
	pos := func(k string, v any) int {
		f, ok := toFloatOK(v)
		if !ok {
			return -1
		}
		for i, x := range axisValues[k] {
			if math.Abs(x-f) < 1e-9 {
				return i
			}
		}
		return -1
	}

	var sum float64
	var n int
	for _, row := range rows {
		if row.Error != "" || !row.Score.Defined() {
			continue
		}
		diffs := 0
		adjacent := true
		for k, v := range best.Params {
			if sameParam(row.Params[k], v) {
				continue
			}
			diffs++
			pi, pj := pos(k, row.Params[k]), pos(k, v)
			if pi < 0 || pj < 0 || abs(pi-pj) != 1 {
				adjacent = false
			}
		}
		if diffs == 1 && adjacent {
			sum += float64(row.Score)
			n++
		}
	}
	if n == 0 {
		return Ratio(math.NaN()), 0
	}
	mean := sum / float64(n)
	if best.Score == 0 {
		return Ratio(math.NaN()), n
	}
	bestScore := float64(best.Score)
	// Clamp: a neighbour scoring higher than the "best" cannot happen, and a
	// neighbour deep in the negative should read as zero support, not as a
	// large negative ratio nobody can interpret.
	ratio := mean / bestScore
	if bestScore < 0 {
		ratio = 0
	}
	return Ratio(math.Max(0, math.Min(1, ratio))), n
}

func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

// AddPBO computes the probability of backtest overfitting by combinatorially
// symmetric cross-validation.
//
// returns is one row per trial and one column per period, aligned. The period
// is cut into S equal blocks; every way of choosing S/2 blocks as training
// gives one split. In each split the best trial in-sample is located, and its
// rank among all trials out-of-sample is recorded. If selection carried no
// information, that rank is uniform and lands below median half the time —
// which is exactly what a PBO near 0.5 is saying.
func (r *Robustness) AddPBO(returns [][]float64, blocks int) {
	// Every path through this method must leave PBO explicitly set. The zero
	// value of a Ratio is a finite 0, which reads as "0% chance of
	// overfitting" — the most flattering possible answer, arrived at by
	// having computed nothing.
	r.PBO = Ratio(math.NaN())
	r.PBOSplits = 0

	if len(returns) < 2 || blocks < 2 || blocks%2 != 0 {
		return
	}
	n := len(returns[0])
	if n < blocks*10 {
		return
	}
	for _, row := range returns {
		if len(row) != n {
			return // ragged input: refuse rather than compare unlike things
		}
	}

	edges := make([][2]int, blocks)
	for i := 0; i < blocks; i++ {
		edges[i] = [2]int{i * n / blocks, (i + 1) * n / blocks}
	}

	var below, total int
	for _, train := range combinations(blocks, blocks/2) {
		inTrain := make([]bool, blocks)
		for _, b := range train {
			inTrain[b] = true
		}

		bestTrial, bestIS := -1, math.Inf(-1)
		oos := make([]float64, len(returns))
		for t, row := range returns {
			var isRet, oosRet []float64
			for b, e := range edges {
				if inTrain[b] {
					isRet = append(isRet, row[e[0]:e[1]]...)
				} else {
					oosRet = append(oosRet, row[e[0]:e[1]]...)
				}
			}
			if s := sharpeOf(isRet); s > bestIS {
				bestIS, bestTrial = s, t
			}
			oos[t] = sharpeOf(oosRet)
		}
		if bestTrial < 0 {
			continue
		}

		// Where did the in-sample winner rank out of sample?
		rank := 0
		for _, v := range oos {
			if v < oos[bestTrial] {
				rank++
			}
		}
		// Logit of the relative rank, thresholded at the median. Counting the
		// median crossings directly is equivalent and easier to explain.
		if float64(rank) < float64(len(oos)-1)/2 {
			below++
		}
		total++
	}
	if total > 0 {
		r.PBO = Ratio(float64(below) / float64(total))
		r.PBOSplits = total
	}
}

// sharpeOf is a per-period Sharpe with no risk-free adjustment, which is all
// the CSCV ranking needs.
func sharpeOf(rets []float64) float64 {
	if len(rets) < 2 {
		return math.Inf(-1)
	}
	mean, sd := meanStdev(rets)
	if sd <= 0 {
		return math.Inf(-1)
	}
	return mean / sd
}

// combinations enumerates every way of choosing k items from n.
func combinations(n, k int) [][]int {
	var out [][]int
	cur := make([]int, k)
	var rec func(start, depth int)
	rec = func(start, depth int) {
		if depth == k {
			out = append(out, append([]int(nil), cur...))
			return
		}
		for i := start; i < n; i++ {
			cur[depth] = i
			rec(i+1, depth+1)
		}
	}
	rec(0, 0)
	return out
}

// BootstrapBands is a distribution of outcomes from resampling a return
// series in blocks.
//
// One backtest gives one path. Resampling in blocks — long enough to preserve
// the autocorrelation and volatility clustering that make drawdowns what they
// are — gives the range of paths the same process could plausibly have
// produced. The fifth-percentile drawdown from this is a far more useful
// number to plan around than the single drawdown that happened to occur.
type BootstrapBands struct {
	Trials int `json:"trials"`
	// Return percentiles of total return.
	ReturnP05    float64 `json:"return_p05"`
	ReturnMedian float64 `json:"return_median"`
	ReturnP95    float64 `json:"return_p95"`
	// Drawdown percentiles, all negative; P05 is the bad tail.
	DrawdownP05    float64 `json:"drawdown_p05"`
	DrawdownMedian float64 `json:"drawdown_median"`
	// LossProbability is how often the resampled path finished down.
	LossProbability float64 `json:"loss_probability"`
}

// Bootstrap resamples an equity curve's returns in blocks.
func Bootstrap(curve []EquityPoint, trials, blockLen int, seed int64) BootstrapBands {
	rets := dailyReturns(curve)
	var b BootstrapBands
	if len(rets) < 30 || trials < 10 {
		return b
	}
	if blockLen < 2 {
		// A block of about a month preserves volatility clustering without
		// simply replaying the original path.
		blockLen = 21
	}
	if blockLen > len(rets)/3 {
		blockLen = len(rets) / 3
	}

	rng := newLCG(seed)
	totals := make([]float64, 0, trials)
	dds := make([]float64, 0, trials)
	idx := make([]int, len(rets))
	var losses int

	for t := 0; t < trials; t++ {
		equity, peak, worst := 1.0, 1.0, 0.0
		blockIndices(idx, blockLen, rng)
		for _, i := range idx {
			equity *= 1 + rets[i]
			if equity > peak {
				peak = equity
			}
			if peak > 0 {
				if dd := equity/peak - 1; dd < worst {
					worst = dd
				}
			}
		}
		totals = append(totals, equity-1)
		dds = append(dds, worst)
		if equity < 1 {
			losses++
		}
	}

	sort.Float64s(totals)
	sort.Float64s(dds)
	b.Trials = trials
	b.ReturnP05 = percentileSorted(totals, 0.05)
	b.ReturnMedian = percentileSorted(totals, 0.5)
	b.ReturnP95 = percentileSorted(totals, 0.95)
	b.DrawdownP05 = percentileSorted(dds, 0.05)
	b.DrawdownMedian = percentileSorted(dds, 0.5)
	b.LossProbability = float64(losses) / float64(trials)
	return b
}

// blockIndices fills idx with a moving-block resample of 0..len(idx)-1.
//
// This is the resampling scheme Bootstrap has always used, lifted out so that
// the reality check can share the generator and the seeding discipline rather
// than growing a second bootstrap alongside this one.
func blockIndices(idx []int, blockLen int, rng *lcg) {
	n := len(idx)
	if blockLen < 1 || blockLen >= n {
		blockLen = 1
	}
	for i := 0; i < n; i += blockLen {
		start := rng.intn(n - blockLen)
		for j := 0; j < blockLen && i+j < n; j++ {
			idx[i+j] = start + j
		}
	}
}

// stationaryIndices fills idx with a Politis–Romano stationary bootstrap
// resample: each step continues the previous block with probability
// 1-1/meanBlock and otherwise jumps to a fresh uniform position, wrapping at
// the end of the series.
//
// The moving-block scheme above is the right one for replaying an equity curve,
// where a fixed month-long block is easy to explain and the hard resets at
// block boundaries do not matter. It is the wrong one here: White's and
// Hansen's results assume the resampled series is stationary, and fixed blocks
// anchored to a grid are not — observations near a block boundary are sampled
// differently from observations in the middle. Geometric block lengths remove
// that, which is the entire reason Politis and Romano proposed them.
func stationaryIndices(idx []int, meanBlock float64, rng *lcg) {
	n := len(idx)
	if n == 0 {
		return
	}
	if meanBlock < 1 {
		meanBlock = 1
	}
	p := 1 / meanBlock
	at := rng.intn(n)
	for i := range idx {
		if i > 0 {
			if rng.float64() < p {
				at = rng.intn(n)
			} else {
				at = (at + 1) % n
			}
		}
		idx[i] = at
	}
}

// lcg is a small deterministic generator, so a bootstrap is reproducible from
// its seed without pulling in a global source.
type lcg struct{ state uint64 }

func newLCG(seed int64) *lcg {
	if seed == 0 {
		seed = 1
	}
	return &lcg{state: uint64(seed)}
}

func (l *lcg) next() uint64 {
	l.state = l.state*6364136223846793005 + 1442695040888963407
	return l.state >> 11
}

// intn is a draw from [0, n), taken from the high bits.
//
// The low bits of a linear congruential generator are its weakest: bit k
// repeats with period 2^(k+1). next() already discards the low eleven, so a
// modulus here would not have been reading the worst of them, and the effect
// is correspondingly small — measured against the known answer for an iid
// bootstrap, `next() % n` tracked the theoretical resample variance to within
// half a percent at most sizes and ran about 2.5% low at n=256, while this
// version stayed within half a percent throughout.
//
// Small, then, but free to fix and in a known direction: a resample that
// covers its series more evenly than chance understates variance, and every
// p-value built on it comes out smaller than it should. This package exists
// to argue against its own results, so an error that flatters them is the
// one kind worth removing even when it is this size.
func (l *lcg) intn(n int) int {
	if n <= 1 {
		return 0
	}
	hi, _ := bits.Mul64(l.next()<<11, uint64(n))
	return int(hi)
}

// float64 is a draw from [0, 1).
func (l *lcg) float64() float64 {
	const mantissa = 1 << 53
	return float64(l.next()%mantissa) / mantissa
}

// verdict turns the statistics into the sentence a reader actually needs.
func verdict(r Robustness, objective string) string {
	if r.Trials < 2 {
		return "a single combination is not a search; nothing here speaks to robustness"
	}
	var parts []string

	if r.BestScore <= r.ExpectedMaxScore && r.ExpectedMaxScore > 0 {
		parts = append(parts, fmt.Sprintf(
			"the best %s of %.2f is below the %.2f that pure luck would produce over %d trials — this search found nothing",
			objective, r.BestScore, r.ExpectedMaxScore, r.Trials))
	} else if r.ExpectedMaxScore > 0 {
		parts = append(parts, fmt.Sprintf(
			"best %s %.2f against %.2f expected from luck alone over %d trials",
			objective, r.BestScore, r.ExpectedMaxScore, r.Trials))
	}

	if r.PlateauRatio.Defined() {
		switch p := float64(r.PlateauRatio); {
		case p < 0.4:
			parts = append(parts, fmt.Sprintf(
				"the winner is an isolated spike: its immediate neighbours average only %.0f%% of its score, so a small change in any parameter destroys it", p*100))
		case p > 0.8:
			parts = append(parts, fmt.Sprintf(
				"the winner sits on a broad plateau (neighbours average %.0f%% of its score), which is what a real edge looks like", p*100))
		default:
			parts = append(parts, fmt.Sprintf(
				"the winner's neighbours average %.0f%% of its score — some support, but the peak is doing real work", p*100))
		}
	}

	if r.PositiveShare < 0.25 {
		parts = append(parts, fmt.Sprintf(
			"only %.0f%% of combinations scored above zero", r.PositiveShare*100))
	}

	if r.PBO.Defined() {
		switch p := float64(r.PBO); {
		case p >= 0.5:
			parts = append(parts, fmt.Sprintf(
				"probability of backtest overfitting is %.0f%% — selecting on this sample carries no information about the next one", p*100))
		case p <= 0.2:
			parts = append(parts, fmt.Sprintf(
				"probability of backtest overfitting is %.0f%%, low enough that the selection is doing something", p*100))
		default:
			parts = append(parts, fmt.Sprintf("probability of backtest overfitting is %.0f%%", p*100))
		}
	}

	if r.DeflatedSharpe.Defined() {
		if d := float64(r.DeflatedSharpe); d < 0.5 {
			parts = append(parts, fmt.Sprintf(
				"deflated Sharpe %.2f: once the number of trials is accounted for, this is indistinguishable from noise", d))
		} else if d > 0.95 {
			parts = append(parts, fmt.Sprintf("deflated Sharpe %.2f, which survives the trial count", d))
		}
	}

	if v := r.RealityCheck.Verdict; v != "" {
		parts = append(parts, v)
	}
	if v := r.NullStrategy.Verdict; v != "" {
		parts = append(parts, v)
	}
	// The two families of statistic answer different questions and a reader
	// who takes them for one will read a contradiction where there is none.
	// A search can fail to say which variant to use and still contain
	// variants that made money, and a strategy can make money purely by being
	// in a rising market at arbitrary times. Say so explicitly when both
	// happen, because that combination is the common one and it is the one
	// most often reported as a success.
	if r.PBO.Defined() && float64(r.PBO) >= 0.5 &&
		r.RealityCheck.SPAP.Defined() && float64(r.RealityCheck.SPAP) <= 0.05 {
		parts = append(parts, "those two are not in conflict: the set as a whole made money, "+
			"and the search still cannot tell you which member of it to run")
	}

	if len(parts) == 0 {
		return "not enough variation across the search to say anything useful"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "; " + p
	}
	return out
}

// normalCDF is the standard normal cumulative distribution.
func normalCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

// normalInv is the inverse standard normal CDF, via Acklam's rational
// approximation — accurate to about 1e-9, which is far beyond what any of
// these statistics need.
func normalInv(p float64) float64 {
	if p <= 0 {
		return math.Inf(-1)
	}
	if p >= 1 {
		return math.Inf(1)
	}
	a := []float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02,
		1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := []float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02,
		6.680131188771972e+01, -1.328068155288572e+01}
	c := []float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00,
		-2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := []float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00,
		3.754408661907416e+00}

	const plow, phigh = 0.02425, 1 - 0.02425
	switch {
	case p < plow:
		q := math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p > phigh:
		q := math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	default:
		q := p - 0.5
		r := q * q
		return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q /
			(((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
	}
}
