package engine

import (
	"math"
	"testing"
)

// noiseTrials builds a matrix of independent zero-mean return series.
func noiseTrials(trials, periods int, seed int64, edge []float64) [][]float64 {
	rng := newLCG(seed)
	out := make([][]float64, trials)
	for i := range out {
		row := make([]float64, periods)
		for j := range row {
			row[j] = (rng.float64() - 0.5) * 0.02
			if edge != nil {
				row[j] += edge[i]
			}
		}
		out[i] = row
	}
	return out
}

func TestRealityCheckDoesNotRejectPureNoise(t *testing.T) {
	// The calibration check, and the one that matters most: a test that
	// rejects here would call every sweep ever run a discovery.
	//
	// Under the null a p-value is uniform, so one sweep says almost nothing —
	// a threshold on a single draw either fails on a legitimate 5% event or is
	// so loose it can never fail. Sixteen independent sweeps of pure noise, and
	// what is checked is that the p-values spread over the unit interval
	// instead of piling up at zero.
	const sweeps = 16
	var rcP, spaP []float64
	for s := int64(1); s <= sweeps; s++ {
		var r Robustness
		r.AddRealityCheck(noiseTrials(24, 900, s, nil), DailyScale(0), 400, 0, s*7+1)
		rc := r.RealityCheck
		if !rc.RealityCheckP.Defined() || !rc.SPAP.Defined() {
			t.Fatal("both p-values should be computable for aligned noise")
		}
		if rc.Trials != 24 || rc.Periods != 900 || rc.Bootstraps != 400 {
			t.Fatalf("shape not recorded: %+v", rc)
		}
		rcP = append(rcP, float64(rc.RealityCheckP))
		spaP = append(spaP, float64(rc.SPAP))
	}

	for _, c := range []struct {
		name string
		ps   []float64
	}{{"reality check", rcP}, {"SPA", spaP}} {
		mean, _ := meanStdev(c.ps)
		// A uniform sample of sixteen has a mean of 0.5 with a standard error
		// of 0.07, so 0.3 is nearly three errors out and 0.1 is unreachable
		// without the test being broken.
		if mean < 0.3 {
			t.Errorf("%s p-values pile up near zero on noise: mean %.3f, %v", c.name, mean, c.ps)
		}
		var rejects int
		for _, p := range c.ps {
			if p < 0.05 {
				rejects++
			}
		}
		// One in twenty is the size of the test; four out of sixteen is not
		// a size, it is a bias.
		if rejects > 3 {
			t.Errorf("%s rejected %d of %d pure-noise sweeps at 5%%: %v",
				c.name, rejects, sweeps, c.ps)
		}
	}
}

func TestRealityCheckRejectsOneGenuinelyDominantTrial(t *testing.T) {
	// One trial has a large, steady edge; the rest are the same noise as
	// above. Both tests should reject decisively.
	edge := make([]float64, 24)
	edge[11] = 0.004
	var r Robustness
	r.AddRealityCheck(noiseTrials(24, 900, 42, edge), DailyScale(0), 500, 0, 7)

	rc := r.RealityCheck
	if p := float64(rc.RealityCheckP); p > 0.02 {
		t.Errorf("a genuine edge should give a small reality check p: %v", p)
	}
	if p := float64(rc.SPAP); p > 0.02 {
		t.Errorf("a genuine edge should give a small SPA p: %v", p)
	}
	// The statistic is reported for the trial that actually carries the edge.
	if !rc.BestStatistic.Defined() || float64(rc.BestStatistic) < 3 {
		t.Errorf("the dominant trial's statistic should be large: %v", float64(rc.BestStatistic))
	}
	if rc.BestExcess < 0.5 {
		t.Errorf("0.4%% a day annualises to far more than this: %v", rc.BestExcess)
	}
}

func TestSPAIsNotDraggedDownByHopelessTrials(t *testing.T) {
	// The same modest edge in one trial, first among a handful of others and
	// then among a hundred that are all clearly terrible. White's p-value
	// deteriorates as the junk is added, because every junk trial contributes
	// a fresh draw to the maximum it compares against. Hansen's recentring is
	// what stops that happening, and this is the case it was invented for.
	const periods = 900
	small := make([]float64, 6)
	small[0] = 0.0006 // about three standard errors over 900 bars
	var a Robustness
	a.AddRealityCheck(noiseTrials(6, periods, 3, small), DailyScale(0), 500, 0, 11)

	large := make([]float64, 106)
	large[0] = 0.0006
	for i := 6; i < len(large); i++ {
		large[i] = -0.003 // hopeless, and obviously so
	}
	var b Robustness
	b.AddRealityCheck(noiseTrials(106, periods, 3, large), DailyScale(0), 500, 0, 11)

	white, spa := float64(b.RealityCheck.RealityCheckP), float64(b.RealityCheck.SPAP)
	if spa > white {
		t.Errorf("SPA should be no more conservative than the reality check: %v vs %v", spa, white)
	}
	if white <= float64(a.RealityCheck.RealityCheckP) {
		t.Errorf("adding a hundred hopeless trials should cost White's test power: %v then %v",
			float64(a.RealityCheck.RealityCheckP), white)
	}
	if spa > 0.1 {
		t.Errorf("the edge is still there, so SPA should still see it: %v", spa)
	}
}

func TestRealityCheckIsReproducibleFromItsSeed(t *testing.T) {
	returns := noiseTrials(10, 500, 5, nil)
	var a, b, c Robustness
	a.AddRealityCheck(returns, DailyScale(0), 300, 0, 99)
	b.AddRealityCheck(returns, DailyScale(0), 300, 0, 99)
	c.AddRealityCheck(returns, DailyScale(0), 300, 0, 100)
	if a.RealityCheck != b.RealityCheck {
		t.Errorf("same seed produced different p-values:\n%+v\n%+v", a.RealityCheck, b.RealityCheck)
	}
	if c.RealityCheck == a.RealityCheck {
		t.Error("a different seed should resample differently")
	}
}

func TestRealityCheckRefusesInputItCannotUse(t *testing.T) {
	var r Robustness
	r.AddRealityCheck([][]float64{make([]float64, 400), make([]float64, 300)}, DailyScale(0), 500, 0, 1)
	if r.RealityCheck.RealityCheckP.Defined() {
		t.Error("series of different lengths must not be compared")
	}
	// Too short to bootstrap a mean from.
	var r2 Robustness
	r2.AddRealityCheck(noiseTrials(4, 100, 1, nil), DailyScale(0), 500, 0, 1)
	if r2.RealityCheck.SPAP.Defined() {
		t.Error("a hundred bars is not enough to run this on")
	}
	// And nothing at all.
	var r3 Robustness
	r3.AddRealityCheck(nil, DailyScale(0), 500, 0, 1)
	if r3.RealityCheck.RealityCheckP.Defined() {
		t.Error("an empty matrix has no p-value")
	}
}

func TestStationaryBootstrapCoversTheSeriesEvenly(t *testing.T) {
	// Every observation should be drawn about equally often, or the resample
	// is not of the series it claims to be of. This is the property the
	// wrap-around exists to provide: without it the last few bars can only be
	// reached from a block that started before them.
	const n = 400
	counts := make([]int, n)
	idx := make([]int, n)
	rng := newLCG(17)
	const draws = 500
	for i := 0; i < draws; i++ {
		stationaryIndices(idx, 21, rng)
		for _, j := range idx {
			counts[j]++
		}
	}
	want := float64(draws)
	for j, c := range counts {
		if math.Abs(float64(c)-want)/want > 0.35 {
			t.Fatalf("index %d drawn %d times, expected about %.0f", j, c, want)
		}
	}
}

func TestFormatPValueWillNotPrintAZeroItCannotSupport(t *testing.T) {
	if got := FormatPValue(0, 1000); got != "<0.001" {
		t.Errorf("a zero count over 1000 resamples is a bound, not a value: %q", got)
	}
	if got := FormatPValue(0.0432, 1000); got != "0.043" {
		t.Errorf("got %q", got)
	}
}
