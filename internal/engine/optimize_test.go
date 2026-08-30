package engine

import (
	"math"
	"testing"
)

// synthReturns builds correlated return series with known volatilities, so
// each optimiser's answer can be checked against what it must obviously be.
func synthReturns(vols []float64, obs int, seed int64) [][]float64 {
	rng := newLCG(seed)
	out := make([][]float64, len(vols))
	for i, v := range vols {
		r := make([]float64, obs)
		for j := range r {
			// Uniform noise scaled to the requested volatility.
			r[j] = (float64(rng.next()%2000001)/1e6 - 1) * v
		}
		out[i] = r
	}
	return out
}

func sumOf(w []float64) float64 {
	var s float64
	for _, x := range w {
		s += x
	}
	return s
}

func TestWeightsAlwaysSumToOne(t *testing.T) {
	r := synthReturns([]float64{0.01, 0.02, 0.03, 0.015}, 400, 1)
	for _, obj := range []Objective{
		ObjMinVariance, ObjMaxSharpe, ObjRiskParity, ObjHRP, ObjInverseVol, ObjEqual,
	} {
		w := Optimize(r, OptimizeOptions{Objective: obj, Shrinkage: -1})
		if len(w) != 4 {
			t.Fatalf("%s returned %d weights, want 4", obj, len(w))
		}
		if math.Abs(sumOf(w)-1) > 1e-9 {
			t.Errorf("%s weights sum to %v, want 1", obj, sumOf(w))
		}
		for i, x := range w {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				t.Errorf("%s produced a non-finite weight at %d: %v", obj, i, x)
			}
		}
	}
}

func TestInverseVolFavoursTheCalmAsset(t *testing.T) {
	// One asset is three times as volatile as the other.
	r := synthReturns([]float64{0.01, 0.03}, 400, 2)
	w := Optimize(r, OptimizeOptions{Objective: ObjInverseVol})
	if !(w[0] > w[1]) {
		t.Fatalf("the calmer asset should get more weight: %v", w)
	}
	// The ratio should be roughly the inverse volatility ratio, so about 3:1.
	if ratio := w[0] / w[1]; ratio < 2 || ratio > 4.5 {
		t.Errorf("weight ratio %v is not close to the 3:1 volatility ratio", ratio)
	}
}

func TestMinVarianceBeatsEqualWeightOnItsOwnObjective(t *testing.T) {
	r := synthReturns([]float64{0.005, 0.02, 0.04, 0.01, 0.03}, 600, 3)
	cov := covarianceMatrix(r)

	mv := Optimize(r, OptimizeOptions{Objective: ObjMinVariance})
	eq := Optimize(r, OptimizeOptions{Objective: ObjEqual})

	// The defining property: nothing else may have lower variance.
	if portfolioVariance(cov, mv) >= portfolioVariance(cov, eq) {
		t.Errorf("minimum variance is not minimal: %v vs equal %v",
			portfolioVariance(cov, mv), portfolioVariance(cov, eq))
	}
	for _, other := range []Objective{ObjRiskParity, ObjHRP, ObjInverseVol} {
		w := Optimize(r, OptimizeOptions{Objective: other})
		if portfolioVariance(cov, mv) > portfolioVariance(cov, w)+1e-12 {
			t.Errorf("%s beat minimum variance on variance: %v vs %v",
				other, portfolioVariance(cov, w), portfolioVariance(cov, mv))
		}
	}
}

func portfolioVariance(cov [][]float64, w []float64) float64 {
	var v float64
	for i := range w {
		for j := range w {
			v += w[i] * w[j] * cov[i][j]
		}
	}
	return v
}

func TestRiskParityEqualisesRiskContributions(t *testing.T) {
	r := synthReturns([]float64{0.008, 0.02, 0.035, 0.012}, 600, 4)
	cov := covarianceMatrix(r)
	w := Optimize(r, OptimizeOptions{Objective: ObjRiskParity})

	variance := portfolioVariance(cov, w)
	if variance <= 0 {
		t.Fatal("degenerate covariance")
	}
	target := 1.0 / float64(len(w))
	for i := range w {
		var mrc float64
		for j := range w {
			mrc += cov[i][j] * w[j]
		}
		contribution := w[i] * mrc / variance
		if math.Abs(contribution-target) > 0.02 {
			t.Errorf("asset %d contributes %.4f of risk, want ~%.4f", i, contribution, target)
		}
	}
}

func TestHRPIsSaneAndLongOnly(t *testing.T) {
	r := synthReturns([]float64{0.01, 0.011, 0.03, 0.031, 0.05}, 500, 5)
	w := Optimize(r, OptimizeOptions{Objective: ObjHRP})
	if math.Abs(sumOf(w)-1) > 1e-9 {
		t.Fatalf("HRP weights sum to %v", sumOf(w))
	}
	// HRP allocates by inverse variance down a tree, so it never shorts.
	for i, x := range w {
		if x < 0 {
			t.Errorf("HRP produced a negative weight at %d: %v", i, x)
		}
	}
	// The most volatile asset should not be the largest holding.
	largest := 0
	for i := range w {
		if w[i] > w[largest] {
			largest = i
		}
	}
	if largest == 4 {
		t.Errorf("HRP gave the most volatile asset the largest weight: %v", w)
	}
}

func TestQuasiDiagonalGroupsCorrelatedAssets(t *testing.T) {
	// Assets 0 and 2 move together; 1 and 3 move together.
	base := synthReturns([]float64{0.02, 0.02}, 300, 6)
	r := [][]float64{base[0], base[1], nil, nil}
	r[2] = make([]float64, 300)
	r[3] = make([]float64, 300)
	for i := range base[0] {
		r[2][i] = base[0][i] * 0.98
		r[3][i] = base[1][i] * 0.98
	}
	order := quasiDiagonal(correlationFromCov(covarianceMatrix(r)))
	if len(order) != 4 {
		t.Fatalf("order should contain every asset: %v", order)
	}
	pos := map[int]int{}
	for i, a := range order {
		pos[a] = i
	}
	// The two pairs must each end up adjacent.
	if math.Abs(float64(pos[0]-pos[2])) != 1 {
		t.Errorf("assets 0 and 2 are correlated but not adjacent: %v", order)
	}
	if math.Abs(float64(pos[1]-pos[3])) != 1 {
		t.Errorf("assets 1 and 3 are correlated but not adjacent: %v", order)
	}
}

func TestSolveLinearRecoversAKnownSolution(t *testing.T) {
	// 2x + y = 5, x + 3y = 10  →  x = 1, y = 3
	a := [][]float64{{2, 1}, {1, 3}}
	x := solveLinear(a, []float64{5, 10})
	if x == nil {
		t.Fatal("solve returned nil for a well-conditioned system")
	}
	if math.Abs(x[0]-1) > 1e-9 || math.Abs(x[1]-3) > 1e-9 {
		t.Errorf("got %v, want [1 3]", x)
	}
}

func TestSolveLinearRefusesSingularMatrices(t *testing.T) {
	// Second row is twice the first: no unique solution.
	if got := solveLinear([][]float64{{1, 2}, {2, 4}}, []float64{3, 6}); got != nil {
		t.Errorf("a singular matrix should return nil, got %v", got)
	}
}

func TestOptimizeSurvivesDuplicateAssets(t *testing.T) {
	// A duplicated series makes the covariance matrix singular, which is what
	// happens the moment a universe lists the same ticker twice.
	r := synthReturns([]float64{0.02}, 300, 7)
	dup := [][]float64{r[0], r[0], r[0]}
	for _, obj := range []Objective{ObjMinVariance, ObjMaxSharpe, ObjRiskParity, ObjHRP} {
		w := Optimize(dup, OptimizeOptions{Objective: obj})
		if len(w) != 3 || math.Abs(sumOf(w)-1) > 1e-9 {
			t.Errorf("%s failed on duplicated assets: %v", obj, w)
		}
		for _, x := range w {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				t.Errorf("%s produced a non-finite weight on duplicates: %v", obj, w)
			}
		}
	}
}

func TestOptimizeFallsBackWithoutEnoughHistory(t *testing.T) {
	r := synthReturns([]float64{0.01, 0.02}, 5, 8)
	w := Optimize(r, OptimizeOptions{Objective: ObjMinVariance})
	// Five observations cannot support an estimate; equal weight is the
	// honest answer rather than a confident-looking wrong one.
	if math.Abs(w[0]-0.5) > 1e-9 || math.Abs(w[1]-0.5) > 1e-9 {
		t.Errorf("want equal weights with too little history, got %v", w)
	}
}

func TestOptimizeHandlesRaggedAndEmptyInput(t *testing.T) {
	if got := Optimize(nil, OptimizeOptions{}); got != nil {
		t.Errorf("no assets should give no weights, got %v", got)
	}
	if got := Optimize([][]float64{{0.1, 0.2}}, OptimizeOptions{}); len(got) != 1 || got[0] != 1 {
		t.Errorf("one asset should get everything, got %v", got)
	}
	ragged := [][]float64{make([]float64, 100), make([]float64, 50)}
	w := Optimize(ragged, OptimizeOptions{Objective: ObjMinVariance})
	if len(w) != 2 || math.Abs(sumOf(w)-1) > 1e-9 {
		t.Errorf("ragged input should fall back to equal weights, got %v", w)
	}
}

func TestLongOnlyClipsShorts(t *testing.T) {
	// A tangency portfolio on noisy data readily produces shorts.
	r := synthReturns([]float64{0.01, 0.02, 0.03}, 300, 9)
	w := Optimize(r, OptimizeOptions{Objective: ObjMaxSharpe, LongOnly: true})
	for i, x := range w {
		if x < 0 {
			t.Errorf("long-only produced a short at %d: %v", i, x)
		}
	}
	if math.Abs(sumOf(w)-1) > 1e-9 {
		t.Errorf("long-only weights should still sum to one: %v", sumOf(w))
	}
}

func TestMaxWeightIsEnforced(t *testing.T) {
	r := synthReturns([]float64{0.002, 0.05, 0.05, 0.05}, 400, 10)
	// Without a cap the calm asset dominates.
	uncapped := Optimize(r, OptimizeOptions{Objective: ObjInverseVol})
	if uncapped[0] < 0.4 {
		t.Skip("this fixture did not concentrate; nothing to cap")
	}
	w := Optimize(r, OptimizeOptions{Objective: ObjInverseVol, MaxWeight: 0.35})
	for i, x := range w {
		if x > 0.35+1e-9 {
			t.Errorf("weight %d is %v, over the 0.35 cap", i, x)
		}
	}
	if math.Abs(sumOf(w)-1) > 1e-9 {
		t.Errorf("capped weights should still sum to one: %v", sumOf(w))
	}
}

func TestMaxWeightTooTightFallsBackToEqual(t *testing.T) {
	// A cap of 0.1 across 4 assets cannot sum to 1.
	r := synthReturns([]float64{0.01, 0.02, 0.03, 0.04}, 300, 11)
	w := Optimize(r, OptimizeOptions{Objective: ObjInverseVol, MaxWeight: 0.1})
	for _, x := range w {
		if math.Abs(x-0.25) > 1e-9 {
			t.Errorf("an unsatisfiable cap should give equal weights, got %v", w)
			break
		}
	}
}

func TestLedoitWolfShrinksHarderWithLessData(t *testing.T) {
	vols := []float64{0.01, 0.02, 0.03, 0.015, 0.025, 0.012}
	few := synthReturns(vols, 30, 12)
	many := synthReturns(vols, 2000, 12)

	a := ledoitWolfIntensity(few, covarianceMatrix(few))
	b := ledoitWolfIntensity(many, covarianceMatrix(many))
	if !(a > b) {
		t.Errorf("shrinkage should fall as data accumulates: %v with 30 obs, %v with 2000", a, b)
	}
	for _, v := range []float64{a, b} {
		if v < 0 || v > 1 {
			t.Errorf("shrinkage intensity out of range: %v", v)
		}
	}
}

func TestShrinkageMovesTowardTheDiagonal(t *testing.T) {
	cov := [][]float64{{0.04, 0.02}, {0.02, 0.01}}
	full := shrinkCovariance(cov, 1)
	// Fully shrunk: off-diagonals gone, diagonal at the average variance.
	if full[0][1] != 0 || full[1][0] != 0 {
		t.Errorf("full shrinkage should zero the off-diagonals: %v", full)
	}
	avg := (0.04 + 0.01) / 2
	if math.Abs(full[0][0]-avg) > 1e-12 {
		t.Errorf("diagonal should be the average variance %v, got %v", avg, full[0][0])
	}
	// No shrinkage leaves it untouched.
	none := shrinkCovariance(cov, 0)
	if none[0][1] != 0.02 {
		t.Errorf("zero shrinkage should be a no-op: %v", none)
	}
}
