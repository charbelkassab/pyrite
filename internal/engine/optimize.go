package engine

import (
	"math"
	"sort"
)

// Portfolio construction, in pure Go.
//
// This is the one place where reaching for a library is tempting and wrong.
// Mean-variance, risk parity and hierarchical risk parity are a few hundred
// lines between them, and pulling in a linear algebra dependency to get them
// would cost the single-binary, no-toolchain property that makes the rest of
// the tool easy to run.

// Objective names a portfolio construction method.
type Objective string

const (
	// ObjMinVariance minimises portfolio variance.
	ObjMinVariance Objective = "min_variance"
	// ObjMaxSharpe is the tangency portfolio.
	ObjMaxSharpe Objective = "max_sharpe"
	// ObjRiskParity equalises each holding's contribution to total risk.
	ObjRiskParity Objective = "risk_parity"
	// ObjHRP is hierarchical risk parity, which never inverts the covariance
	// matrix at all.
	ObjHRP Objective = "hrp"
	// ObjInverseVol weights by 1/volatility, the crude version of risk parity.
	ObjInverseVol Objective = "inverse_vol"
	// ObjEqual is the baseline every method above has to beat.
	ObjEqual Objective = "equal"
)

// OptimizeOptions configures a weighting.
type OptimizeOptions struct {
	Objective Objective
	// Shrinkage blends the sample covariance toward a diagonal target, in
	// [0, 1]. Negative means "choose it from the data" by the Ledoit-Wolf
	// rule, which is the sensible default: a sample covariance estimated from
	// 252 days across 30 assets is mostly noise, and inverting it amplifies
	// exactly that noise into confident, wrong weights.
	Shrinkage float64
	// LongOnly clips negative weights and renormalises.
	LongOnly bool
	// MaxWeight caps any single holding, 0 for no cap.
	MaxWeight float64
	// RiskFree is the annual rate used by the tangency portfolio.
	RiskFree float64
}

// Optimize turns a matrix of aligned return series into weights.
//
// returns is one slice per asset, all the same length, oldest first.
func Optimize(returns [][]float64, opts OptimizeOptions) []float64 {
	n := len(returns)
	if n == 0 {
		return nil
	}
	if n == 1 {
		return []float64{1}
	}
	// Every method needs at least a little history; below that, equal weight
	// is the honest answer rather than a confident-looking wrong one.
	obs := len(returns[0])
	for _, r := range returns {
		if len(r) != obs {
			return equalWeights(n)
		}
	}
	if obs < 20 {
		return equalWeights(n)
	}

	means := make([]float64, n)
	for i, r := range returns {
		m, _ := meanStdev(r)
		means[i] = m
	}
	cov := covarianceMatrix(returns)
	if opts.Shrinkage != 0 {
		s := opts.Shrinkage
		if s < 0 {
			s = ledoitWolfIntensity(returns, cov)
		}
		cov = shrinkCovariance(cov, s)
	}

	var w []float64
	switch opts.Objective {
	case ObjMaxSharpe:
		excess := make([]float64, n)
		rf := opts.RiskFree / TradingDaysPerYear
		for i := range means {
			excess[i] = means[i] - rf
		}
		w = solveWeights(cov, excess)
	case ObjRiskParity:
		w = riskParityWeights(cov)
	case ObjHRP:
		w = hrpWeights(cov)
	case ObjInverseVol:
		w = inverseVolWeights(cov)
	case ObjEqual:
		w = equalWeights(n)
	default: // ObjMinVariance
		ones := make([]float64, n)
		for i := range ones {
			ones[i] = 1
		}
		w = solveWeights(cov, ones)
	}
	if w == nil {
		// A singular covariance matrix means the assets are linearly
		// dependent — duplicated tickers, or a symbol with no movement. Equal
		// weight is a real answer; a NaN vector is not.
		w = equalWeights(n)
	}

	if opts.LongOnly {
		w = clipNegative(w)
	}
	if opts.MaxWeight > 0 {
		w = capWeights(w, opts.MaxWeight)
	}
	return normalize(w)
}

// covarianceMatrix computes the sample covariance of aligned return series.
func covarianceMatrix(returns [][]float64) [][]float64 {
	n := len(returns)
	obs := len(returns[0])
	means := make([]float64, n)
	for i, r := range returns {
		var s float64
		for _, x := range r {
			s += x
		}
		means[i] = s / float64(obs)
	}

	cov := make([][]float64, n)
	den := float64(obs - 1)
	for i := range cov {
		cov[i] = make([]float64, n)
	}
	for i := 0; i < n; i++ {
		for j := i; j < n; j++ {
			var s float64
			for k := 0; k < obs; k++ {
				s += (returns[i][k] - means[i]) * (returns[j][k] - means[j])
			}
			v := s / den
			cov[i][j] = v
			cov[j][i] = v
		}
	}
	return cov
}

// shrinkCovariance blends toward a diagonal of the average variance.
func shrinkCovariance(cov [][]float64, intensity float64) [][]float64 {
	n := len(cov)
	if intensity <= 0 {
		return cov
	}
	if intensity > 1 {
		intensity = 1
	}
	var avgVar float64
	for i := 0; i < n; i++ {
		avgVar += cov[i][i]
	}
	avgVar /= float64(n)

	out := make([][]float64, n)
	for i := range out {
		out[i] = make([]float64, n)
		for j := range out[i] {
			target := 0.0
			if i == j {
				target = avgVar
			}
			out[i][j] = (1-intensity)*cov[i][j] + intensity*target
		}
	}
	return out
}

// ledoitWolfIntensity estimates how far to shrink, from the data.
//
// The intuition: shrink more when the sample covariance is noisy relative to
// how far it sits from the target. With few observations and many assets it
// approaches 1, which is exactly when the sample estimate deserves no trust.
func ledoitWolfIntensity(returns [][]float64, cov [][]float64) float64 {
	n := len(returns)
	obs := len(returns[0])
	if obs < 3 || n < 2 {
		return 1
	}

	means := make([]float64, n)
	for i, r := range returns {
		m, _ := meanStdev(r)
		means[i] = m
	}
	var avgVar float64
	for i := 0; i < n; i++ {
		avgVar += cov[i][i]
	}
	avgVar /= float64(n)

	// pi: summed variance of the entries of the sample covariance matrix.
	var pi float64
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			var s float64
			for k := 0; k < obs; k++ {
				d := (returns[i][k]-means[i])*(returns[j][k]-means[j]) - cov[i][j]
				s += d * d
			}
			pi += s / float64(obs)
		}
	}

	// gamma: squared distance from the shrinkage target.
	var gamma float64
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			target := 0.0
			if i == j {
				target = avgVar
			}
			d := cov[i][j] - target
			gamma += d * d
		}
	}
	if gamma <= 0 {
		return 1
	}
	k := pi / gamma
	return math.Max(0, math.Min(1, k/float64(obs)))
}

// solveWeights returns the normalised solution of cov * w = b, which is the
// closed form for both the minimum-variance and tangency portfolios.
func solveWeights(cov [][]float64, b []float64) []float64 {
	w := solveLinear(cov, b)
	if w == nil {
		return nil
	}
	var sum float64
	for _, x := range w {
		sum += x
	}
	if sum == 0 || math.IsNaN(sum) || math.IsInf(sum, 0) {
		return nil
	}
	for i := range w {
		w[i] /= sum
	}
	return w
}

// solveLinear solves Ax = b by Gaussian elimination with partial pivoting.
// It returns nil for a singular matrix rather than a vector of infinities.
func solveLinear(a [][]float64, b []float64) []float64 {
	n := len(a)
	if n == 0 || len(b) != n {
		return nil
	}
	m := make([][]float64, n)
	for i := range m {
		m[i] = make([]float64, n+1)
		copy(m[i], a[i])
		m[i][n] = b[i]
	}

	for col := 0; col < n; col++ {
		pivot := col
		for r := col + 1; r < n; r++ {
			if math.Abs(m[r][col]) > math.Abs(m[pivot][col]) {
				pivot = r
			}
		}
		if math.Abs(m[pivot][col]) < 1e-14 {
			return nil
		}
		m[col], m[pivot] = m[pivot], m[col]

		for r := 0; r < n; r++ {
			if r == col {
				continue
			}
			f := m[r][col] / m[col][col]
			if f == 0 {
				continue
			}
			for c := col; c <= n; c++ {
				m[r][c] -= f * m[col][c]
			}
		}
	}

	out := make([]float64, n)
	for i := 0; i < n; i++ {
		if m[i][i] == 0 {
			return nil
		}
		out[i] = m[i][n] / m[i][i]
		if math.IsNaN(out[i]) || math.IsInf(out[i], 0) {
			return nil
		}
	}
	return out
}

// inverseVolWeights allocates in proportion to 1/sigma.
func inverseVolWeights(cov [][]float64) []float64 {
	n := len(cov)
	w := make([]float64, n)
	for i := 0; i < n; i++ {
		sd := math.Sqrt(cov[i][i])
		if sd <= 0 {
			w[i] = 0
			continue
		}
		w[i] = 1 / sd
	}
	return normalize(w)
}

// riskParityWeights equalises each asset's contribution to portfolio risk.
//
// Solved by cyclical coordinate descent rather than a general optimiser: the
// update has a closed form, converges in a few dozen passes for the sizes a
// daily strategy uses, and needs no matrix inversion.
func riskParityWeights(cov [][]float64) []float64 {
	n := len(cov)
	w := inverseVolWeights(cov) // a good starting point costs nothing
	if w == nil {
		return nil
	}

	target := 1.0 / float64(n)
	for iter := 0; iter < 500; iter++ {
		// Marginal risk contribution: (cov * w)_i
		mrc := make([]float64, n)
		for i := 0; i < n; i++ {
			var s float64
			for j := 0; j < n; j++ {
				s += cov[i][j] * w[j]
			}
			mrc[i] = s
		}
		var variance float64
		for i := 0; i < n; i++ {
			variance += w[i] * mrc[i]
		}
		if variance <= 0 {
			return inverseVolWeights(cov)
		}

		var maxErr float64
		next := make([]float64, n)
		for i := 0; i < n; i++ {
			contribution := w[i] * mrc[i] / variance
			maxErr = math.Max(maxErr, math.Abs(contribution-target))
			if mrc[i] <= 0 {
				next[i] = w[i]
				continue
			}
			// Move toward the weight that would equalise this asset's share.
			next[i] = w[i] * math.Pow(target/math.Max(contribution, 1e-12), 0.5)
		}
		w = normalize(next)
		if maxErr < 1e-8 {
			break
		}
	}
	return w
}

// hrpWeights is López de Prado's hierarchical risk parity.
//
// The appeal is what it does not do: it never inverts the covariance matrix,
// so it is immune to the noise amplification that makes mean-variance weights
// unstable. It clusters assets by correlation, orders them so similar ones sit
// together, then splits capital recursively down that tree by inverse variance.
func hrpWeights(cov [][]float64) []float64 {
	n := len(cov)
	if n < 2 {
		return equalWeights(n)
	}
	corr := correlationFromCov(cov)
	order := quasiDiagonal(corr)
	if len(order) != n {
		return inverseVolWeights(cov)
	}

	w := make([]float64, n)
	for i := range w {
		w[i] = 1
	}
	var bisect func(idx []int)
	bisect = func(idx []int) {
		if len(idx) <= 1 {
			return
		}
		mid := len(idx) / 2
		left, right := idx[:mid], idx[mid:]
		vl := clusterVariance(cov, left)
		vr := clusterVariance(cov, right)
		total := vl + vr
		if total <= 0 {
			return
		}
		// The less risky half gets the larger share.
		alpha := 1 - vl/total
		for _, i := range left {
			w[i] *= alpha
		}
		for _, i := range right {
			w[i] *= 1 - alpha
		}
		bisect(left)
		bisect(right)
	}
	bisect(order)
	return normalize(w)
}

// correlationFromCov derives correlations, guarding zero-variance assets.
func correlationFromCov(cov [][]float64) [][]float64 {
	n := len(cov)
	out := make([][]float64, n)
	sd := make([]float64, n)
	for i := 0; i < n; i++ {
		sd[i] = math.Sqrt(cov[i][i])
	}
	for i := 0; i < n; i++ {
		out[i] = make([]float64, n)
		for j := 0; j < n; j++ {
			if sd[i] <= 0 || sd[j] <= 0 {
				if i == j {
					out[i][j] = 1
				}
				continue
			}
			out[i][j] = math.Max(-1, math.Min(1, cov[i][j]/(sd[i]*sd[j])))
		}
	}
	return out
}

// quasiDiagonal orders assets so correlated ones are adjacent, by single
// linkage clustering on the correlation distance.
func quasiDiagonal(corr [][]float64) []int {
	n := len(corr)
	// Correlation distance: identical series are at distance 0, perfectly
	// opposed ones at 1.
	dist := make([][]float64, n)
	for i := range dist {
		dist[i] = make([]float64, n)
		for j := range dist[i] {
			dist[i][j] = math.Sqrt(math.Max(0, 0.5*(1-corr[i][j])))
		}
	}

	// Each cluster is a list of leaves; merge the closest pair repeatedly.
	clusters := make([][]int, n)
	for i := range clusters {
		clusters[i] = []int{i}
	}
	for len(clusters) > 1 {
		bi, bj, best := 0, 1, math.Inf(1)
		for i := 0; i < len(clusters); i++ {
			for j := i + 1; j < len(clusters); j++ {
				d := singleLinkage(dist, clusters[i], clusters[j])
				if d < best {
					bi, bj, best = i, j, d
				}
			}
		}
		merged := append(append([]int{}, clusters[bi]...), clusters[bj]...)
		next := make([][]int, 0, len(clusters)-1)
		for k, c := range clusters {
			if k == bi || k == bj {
				continue
			}
			next = append(next, c)
		}
		clusters = append(next, merged)
	}
	return clusters[0]
}

// singleLinkage is the shortest distance between any two members.
func singleLinkage(dist [][]float64, a, b []int) float64 {
	best := math.Inf(1)
	for _, i := range a {
		for _, j := range b {
			if dist[i][j] < best {
				best = dist[i][j]
			}
		}
	}
	return best
}

// clusterVariance is the variance of the inverse-variance portfolio of a
// cluster, which is what HRP compares halves by.
func clusterVariance(cov [][]float64, idx []int) float64 {
	w := make([]float64, len(idx))
	var sum float64
	for k, i := range idx {
		if cov[i][i] <= 0 {
			continue
		}
		w[k] = 1 / cov[i][i]
		sum += w[k]
	}
	if sum <= 0 {
		return 0
	}
	for k := range w {
		w[k] /= sum
	}
	var v float64
	for a, i := range idx {
		for b, j := range idx {
			v += w[a] * w[b] * cov[i][j]
		}
	}
	return v
}

func equalWeights(n int) []float64 {
	if n <= 0 {
		return nil
	}
	w := make([]float64, n)
	for i := range w {
		w[i] = 1 / float64(n)
	}
	return w
}

func clipNegative(w []float64) []float64 {
	out := make([]float64, len(w))
	for i, x := range w {
		if x > 0 {
			out[i] = x
		}
	}
	var sum float64
	for _, x := range out {
		sum += x
	}
	if sum == 0 {
		return equalWeights(len(w))
	}
	return out
}

// capWeights enforces a per-holding ceiling, redistributing the excess to the
// holdings still under it.
func capWeights(w []float64, max float64) []float64 {
	if max <= 0 || max >= 1 {
		return w
	}
	n := len(w)
	if max*float64(n) < 1 {
		// The cap cannot be satisfied; the closest feasible answer is equal.
		return equalWeights(n)
	}
	out := normalize(append([]float64(nil), w...))
	for pass := 0; pass < 50; pass++ {
		var excess float64
		var under []int
		for i := range out {
			if out[i] > max {
				excess += out[i] - max
				out[i] = max
			} else if out[i] < max {
				under = append(under, i)
			}
		}
		if excess <= 1e-12 || len(under) == 0 {
			break
		}
		var underSum float64
		for _, i := range under {
			underSum += out[i]
		}
		for _, i := range under {
			share := 1 / float64(len(under))
			if underSum > 0 {
				share = out[i] / underSum
			}
			out[i] += excess * share
		}
	}
	return out
}

// normalize scales weights to sum to one, preserving sign.
func normalize(w []float64) []float64 {
	var sum float64
	for _, x := range w {
		sum += x
	}
	if sum == 0 || math.IsNaN(sum) || math.IsInf(sum, 0) {
		return equalWeights(len(w))
	}
	out := make([]float64, len(w))
	for i, x := range w {
		out[i] = x / sum
	}
	return out
}

// ObjectiveNamesForOptimize lists the supported methods.
func ObjectiveNamesForOptimize() []string {
	out := []string{
		string(ObjMinVariance), string(ObjMaxSharpe), string(ObjRiskParity),
		string(ObjHRP), string(ObjInverseVol), string(ObjEqual),
	}
	sort.Strings(out)
	return out
}
