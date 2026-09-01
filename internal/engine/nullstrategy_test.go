package engine

import (
	"fmt"
	"math"
	"testing"
)

// syntheticMarket is a deterministic return series with a mild upward drift,
// so the null distribution has the bull-market bias that makes exposure
// matching matter in the first place.
func syntheticMarket(n int, seed int64) []float64 {
	rng := newLCG(seed)
	out := make([]float64, n)
	for i := range out {
		out[i] = (rng.float64()-0.5)*0.02 + 0.0003
	}
	return out
}

// curveFromExposure builds the equity curve of holding an exposure path
// through a market, which is the shape CompareToNullStrategies reads.
func curveFromExposure(expo, mkt []float64) []EquityPoint {
	curve := make([]EquityPoint, len(expo)+1)
	v := 100000.0
	for i := range expo {
		curve[i] = EquityPoint{Value: v, Exposure: expo[i]}
		v *= 1 + expo[i]*mkt[i]
	}
	curve[len(expo)] = EquityPoint{Value: v}
	return curve
}

// randomExposurePath lays holds of the given lengths down at random.
func randomExposurePath(n int, lengths []int, seed int64) []float64 {
	episodes := make([][]float64, len(lengths))
	held := 0
	for i, l := range lengths {
		ep := make([]float64, l)
		for j := range ep {
			ep[j] = 1
		}
		episodes[i] = ep
		held += l
	}
	rng := newLCG(seed)
	order := make([]int, len(lengths))
	gaps := make([]int, len(lengths)+1)
	randomOrder(order, rng)
	randomGaps(gaps, n-held, rng)
	path := make([]float64, n)
	layout(path, episodes, order, gaps)
	return path
}

func TestARandomStrategySitsInTheMiddleOfItsOwnNull(t *testing.T) {
	// The sanity check for the whole construction. A strategy whose entries
	// were chosen at random has no timing skill by definition, so it must land
	// somewhere unremarkable in a distribution of strategies matched to it. If
	// the matching were wrong — if the random ones were more invested, or held
	// for longer, or paid different costs — this would come out at one end or
	// the other, and every percentile the file reports would be a fiction.
	//
	// The percentile is uniform under this null, so one draw proves nothing:
	// what is checked is that a dozen of them average out near the middle.
	const n, sweeps = 1200, 12
	mkt := syntheticMarket(n, 5)
	lengths := []int{40, 95, 20, 160, 60, 110}

	var sum float64
	var extremes int
	for s := int64(1); s <= sweeps; s++ {
		expo := randomExposurePath(n, lengths, s)
		ns := CompareToNullStrategies(curveFromExposure(expo, mkt), mkt, 0, DailyScale(0), 400, 99)
		if !ns.Percentile.Defined() {
			t.Fatalf("seed %d produced no percentile: %+v", s, ns)
		}
		p := float64(ns.Percentile)
		sum += p
		if p < 0.05 || p > 0.95 {
			extremes++
		}
	}
	mean := sum / sweeps
	// Twelve uniforms average 0.5 with a standard error of 0.083, so anything
	// outside this range is a bias rather than a run of luck.
	if mean < 0.3 || mean > 0.7 {
		t.Errorf("random entries should score around the middle of their own null, got mean %.3f", mean)
	}
	if extremes > 3 {
		t.Errorf("%d of %d random strategies landed in a 10%% tail; the matching is skewed", extremes, sweeps)
	}
}

func TestPerfectTimingScoresAtTheTop(t *testing.T) {
	// The other side of the same check: a strategy that holds only during the
	// stretches the market rose has to beat almost every random arrangement of
	// the same holds, or the test has no power and the result above is
	// meaningless.
	const n = 1200
	mkt := syntheticMarket(n, 5)
	lengths := []int{40, 95, 20, 160, 60, 110}
	// Five windows the market rises hard through, and one it does not, so the
	// strategy is good rather than clairvoyant.
	starts := []int{60, 300, 520, 700, 950, 1120}
	expo := make([]float64, n)
	for i, at := range starts {
		for j := 0; j < lengths[i] && at+j < n; j++ {
			expo[at+j] = 1
			if i < 5 {
				mkt[at+j] += 0.004
			}
		}
	}

	ns := CompareToNullStrategies(curveFromExposure(expo, mkt), mkt, 0, DailyScale(0), 500, 99)
	if p := float64(ns.Percentile); p < 0.99 {
		t.Errorf("timing the only rises there were should beat nearly every random entry: %v", p)
	}
	if float64(ns.Score) <= float64(ns.NullP95) {
		t.Errorf("the strategy should clear the 95th percentile of its null: %v vs %v",
			float64(ns.Score), float64(ns.NullP95))
	}
}

func TestNullStrategiesAreMatchedExactly(t *testing.T) {
	// What is claimed on the tin: same trade count, same holding periods, same
	// exposure. The relocation preserves all three by construction, and this
	// pins that down rather than trusting the argument.
	const n = 1000
	lengths := []int{30, 75, 12, 140}
	expo := randomExposurePath(n, lengths, 4)
	for i := range expo {
		if expo[i] > 0 {
			expo[i] = 0.6 // a partial position, so exposure is not just 0 or 1
		}
	}
	episodes := exposureEpisodes(expo)
	if len(episodes) != len(lengths) {
		t.Fatalf("expected %d episodes, got %d", len(lengths), len(episodes))
	}

	var want float64
	for _, e := range expo {
		want += e
	}
	held := 0
	for _, e := range episodes {
		held += len(e)
	}

	rng := newLCG(3)
	order := make([]int, len(episodes))
	gaps := make([]int, len(episodes)+1)
	path := make([]float64, n)
	for trial := 0; trial < 200; trial++ {
		randomOrder(order, rng)
		randomGaps(gaps, n-held, rng)
		entries := layout(path, episodes, order, gaps)
		if len(entries) != len(episodes) {
			t.Fatalf("trial %d placed %d of %d episodes", trial, len(entries), len(episodes))
		}
		var got, sq float64
		for _, e := range path {
			got += e
			sq += e * e
		}
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("trial %d changed total exposure: %v vs %v", trial, got, want)
		}
		// The second moment matters as much as the first: it is what the
		// Sharpe's denominator is built from.
		if math.Abs(sq-want*0.6) > 1e-9 {
			t.Fatalf("trial %d changed squared exposure: %v", trial, sq)
		}
		placed := exposureEpisodes(path)
		if len(placed) != len(episodes) {
			// Two episodes landing adjacent would merge into one, which is a
			// legitimate arrangement; anything else is a bug.
			if len(placed) > len(episodes) {
				t.Fatalf("trial %d invented episodes: %d", trial, len(placed))
			}
		}
	}
}

func TestLeadingGapsRebuildsTheStrategysOwnPath(t *testing.T) {
	// The observed score has to come out of the same code as the null scores,
	// so the strategy's real arrangement is rebuilt through layout rather than
	// scored directly. If that rebuild were not exact, the percentile would be
	// measuring an implementation difference.
	const n = 600
	expo := randomExposurePath(n, []int{25, 60, 10, 90}, 8)
	episodes := exposureEpisodes(expo)
	path := make([]float64, n)
	layout(path, episodes, identityOrder(len(episodes)), leadingGaps(expo, episodes))
	for i := range expo {
		if expo[i] != path[i] {
			t.Fatalf("rebuilt path differs at bar %d: %v vs %v", i, path[i], expo[i])
		}
	}
}

func TestNullStrategyIsReproducibleFromItsSeed(t *testing.T) {
	const n = 800
	mkt := syntheticMarket(n, 2)
	curve := curveFromExposure(randomExposurePath(n, []int{50, 120, 30}, 6), mkt)

	// Compared as text rather than with ==, because an undefined Ratio holds a
	// NaN and a NaN is not equal to itself.
	a := fmt.Sprintf("%+v", CompareToNullStrategies(curve, mkt, 0, DailyScale(0), 300, 17))
	b := fmt.Sprintf("%+v", CompareToNullStrategies(curve, mkt, 0, DailyScale(0), 300, 17))
	if a != b {
		t.Errorf("same seed produced different results:\n%s\n%s", a, b)
	}
	if c := fmt.Sprintf("%+v", CompareToNullStrategies(curve, mkt, 0, DailyScale(0), 300, 18)); c == a {
		t.Error("a different seed should generate different random strategies")
	}
}

func TestNullStrategyRefusesTheCasesItCannotSpeakTo(t *testing.T) {
	const n = 500
	mkt := syntheticMarket(n, 1)

	// Always invested: every rearrangement is the strategy itself.
	always := make([]float64, n)
	for i := range always {
		always[i] = 1
	}
	ns := CompareToNullStrategies(curveFromExposure(always, mkt), mkt, 0, DailyScale(0), 100, 1)
	if ns.Percentile.Defined() {
		t.Error("a strategy invested on every bar has no entry timing to randomise")
	}
	if ns.Verdict == "" {
		t.Error("and it should say so rather than going quiet")
	}

	// Invested on all but a handful of bars, which is the same problem with a
	// number attached: there is nowhere to move the holds to, so every random
	// arrangement is a near-copy of the strategy.
	nearly := make([]float64, n)
	for i := range nearly {
		if i%50 != 0 {
			nearly[i] = 1
		}
	}
	ns = CompareToNullStrategies(curveFromExposure(nearly, mkt), mkt, 0, DailyScale(0), 100, 1)
	if ns.Percentile.Defined() {
		t.Error("a strategy invested on 98% of bars has no entry timing worth testing")
	}

	// And barely invested, where the score on both sides is decided by a
	// couple of bars.
	rare := make([]float64, n)
	rare[10], rare[300] = 1, 1
	ns = CompareToNullStrategies(curveFromExposure(rare, mkt), mkt, 0, DailyScale(0), 100, 1)
	if ns.Percentile.Defined() {
		t.Error("two bars in the market is not enough to place against anything")
	}

	// Never invested.
	flat := make([]float64, n)
	ns = CompareToNullStrategies(curveFromExposure(flat, mkt), mkt, 0, DailyScale(0), 100, 1)
	if ns.Percentile.Defined() || ns.Verdict == "" {
		t.Errorf("a strategy that never traded has nothing to compare: %+v", ns)
	}

	// Misaligned market series.
	ns = CompareToNullStrategies(curveFromExposure(always, mkt), mkt[:10], 0, DailyScale(0), 100, 1)
	if ns.Percentile.Defined() {
		t.Error("a market series of the wrong length must be refused")
	}
	// Too short to say anything about.
	short := syntheticMarket(30, 1)
	ns = CompareToNullStrategies(curveFromExposure(short, short), short, 0, DailyScale(0), 100, 1)
	if ns.Percentile.Defined() {
		t.Error("thirty bars is not a distribution")
	}
}

func TestNullStrategyReportsWhatItMatchedOn(t *testing.T) {
	const n = 1000
	mkt := syntheticMarket(n, 3)
	expo := randomExposurePath(n, []int{20, 40, 60, 80}, 9)
	for i := range expo {
		if expo[i] > 0 {
			expo[i] = 0.5
		}
	}
	ns := CompareToNullStrategies(curveFromExposure(expo, mkt), mkt, 0, DailyScale(0), 200, 1)
	if ns.Episodes != 4 {
		t.Errorf("trade count: got %d, want 4", ns.Episodes)
	}
	// Lengths 20, 40, 60, 80 — the median of four takes the upper middle.
	if ns.MedianHoldBars != 60 {
		t.Errorf("median hold: got %d, want 60", ns.MedianHoldBars)
	}
	// 200 held bars out of 1000, each at half exposure.
	if math.Abs(ns.AvgExposure-0.1) > 1e-9 {
		t.Errorf("average exposure: got %v, want 0.1", ns.AvgExposure)
	}
	if ns.Trials != 200 {
		t.Errorf("trials: got %d", ns.Trials)
	}
}
