package engine

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestCapacityLadderNeverImprovesWithSize(t *testing.T) {
	// The churn strategy from the impact tests replaces its position every
	// other session, so order size is the only variable moving across the
	// ladder. Under the square-root law a larger order can only pay more for
	// the liquidity it takes, and the returns must follow it down.
	spec := impactSpec(100000, 1)
	c, err := RunCapacity(context.Background(), spec, newTestStore(t), nil, 1)
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if len(c.Points) != len(DefaultCapacityLadder) {
		t.Fatalf("want %d rungs, got %d", len(DefaultCapacityLadder), len(c.Points))
	}
	for i, p := range c.Points {
		if p.Error != "" {
			t.Fatalf("rung %v failed: %s", p.Capital, p.Error)
		}
		if p.Capital != DefaultCapacityLadder[i] {
			t.Errorf("rungs must stay in the order requested: %v at index %d", p.Capital, i)
		}
	}
	for i := 1; i < len(c.Points); i++ {
		a, b := c.Points[i-1], c.Points[i]
		if b.CostBps < a.CostBps {
			t.Errorf("friction fell from %.1f bps at %v to %.1f bps at %v — impact is not "+
				"scaling with order size", a.CostBps, a.Capital, b.CostBps, b.Capital)
		}
		if b.TotalReturn > a.TotalReturn {
			t.Errorf("size helped: %v returned %.4f against %.4f at %v",
				b.Capital, b.TotalReturn, a.TotalReturn, a.Capital)
		}
	}
	if !c.Bites {
		t.Errorf("a strategy that turns its whole book over every other day must show a "+
			"capacity constraint across four orders of magnitude; it gave up %.4f",
			float64(c.Degradation))
	}
	if !c.Deployed {
		t.Error("the strategy sizes by percentage of cash, so every rung was deployed")
	}
	if c.Verdict == "" {
		t.Error("a verdict should always be written")
	}
}

func TestCapacityForcesImpactOnEvenWhenTheSpecDoesNot(t *testing.T) {
	// A ladder run without the impact model measures nothing: flat slippage
	// charges the same fraction at every size, so the rungs would come back
	// identical and the report would claim infinite capacity.
	spec := impactSpec(100000, 0)
	c, err := RunCapacity(context.Background(), spec, newTestStore(t), []float64{1e5, 1e9}, 0)
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if c.ImpactCoefficient != DefaultImpactCoefficient {
		t.Fatalf("impact must default to %v when neither caller nor spec sets it, got %v",
			DefaultImpactCoefficient, c.ImpactCoefficient)
	}
	if !(c.Points[1].CostBps > c.Points[0].CostBps) {
		t.Errorf("impact was not wired in: %.1f bps at $1bn against %.1f bps at $100k",
			c.Points[1].CostBps, c.Points[0].CostBps)
	}
}

func TestCapacitySaysWhenImpactNeverBites(t *testing.T) {
	// One purchase, held to the end, on a ladder that stays small. The
	// ladder is small on purpose: a synthetic bar carries a few million
	// shares, so $1bn of it is a far larger share of the tape than $1bn of a
	// real megacap would be, and the point being tested here is what the
	// code says when size costs nothing rather than how the model behaves.
	spec := baseSpec(`
		function onDay(ctx) {
			if (ctx.dayIndex === 0) ctx.buy("AAPL", { pctCash: 0.9 });
		}
	`)
	spec.Universe = []string{"AAPL"}

	c, err := RunCapacity(context.Background(), spec, newTestStore(t),
		[]float64{1e3, 1e4, 1e5}, 1)
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if c.Bites {
		t.Fatalf("a single purchase across a thousandfold ladder gave up %.4f of "+
			"cumulative return, which should not count as a capacity constraint",
			float64(c.Degradation))
	}
	if c.ZeroReturnCapital.Defined() {
		t.Errorf("no rung crossed zero, so there is no threshold to report, got %v",
			float64(c.ZeroReturnCapital))
	}
	if !strings.Contains(c.Verdict, "never bites") {
		t.Errorf("the verdict must say plainly that size never bit: %q", c.Verdict)
	}
}

func TestCapacityCrossingInterpolatesInLogCapital(t *testing.T) {
	// Return crosses zero exactly halfway between the two rungs, and the
	// rungs are a factor of ten apart, so the crossing belongs at 10^6.5.
	pts := []CapacityPoint{
		{Capital: 1e6, TotalReturn: 0.2},
		{Capital: 1e7, TotalReturn: -0.2},
	}
	got := capacityCrossing(pts, 0)
	if !got.Defined() {
		t.Fatal("a crossing exists and must be reported")
	}
	want := math.Pow(10, 6.5)
	if math.Abs(float64(got)-want) > 1 {
		t.Errorf("crossing: got %v, want %v — the interpolation is linear in dollars "+
			"rather than in the logarithm", float64(got), want)
	}
	// And against a benchmark rather than against zero.
	if bm := capacityCrossing(pts, 0.2); math.Abs(float64(bm)-1e6) > 1 {
		t.Errorf("the strategy stops beating a 20%% benchmark at the first rung, got %v",
			float64(bm))
	}
}

func TestCapacityCrossingUndefinedWhenNeverCrossed(t *testing.T) {
	if capacityCrossing([]CapacityPoint{
		{Capital: 1e5, TotalReturn: 0.4},
		{Capital: 1e9, TotalReturn: 0.3},
	}, 0).Defined() {
		t.Error("a strategy profitable at every rung has no threshold inside the ladder")
	}
	if capacityCrossing([]CapacityPoint{
		{Capital: 1e5, TotalReturn: -0.1},
		{Capital: 1e9, TotalReturn: -0.9},
	}, 0).Defined() {
		t.Error("a strategy that lost money at the smallest size never crossed zero")
	}
}

func TestCapacityVerdictOnAStrategyThatLosesAtEverySize(t *testing.T) {
	c := &Capacity{
		Points: []CapacityPoint{
			{Capital: 1e5, TotalReturn: -0.15, Turnover: 400},
			{Capital: 1e9, TotalReturn: -0.996, Turnover: 400},
		},
		Degradation: Ratio(0.846), Bites: true, Deployed: true,
		ZeroReturnCapital: Ratio(math.NaN()),
	}
	v := capacityVerdict(c)
	if !strings.Contains(v, "capacity is not what is wrong with it") {
		t.Errorf("a strategy losing money at $100k has no capacity limit to report: %q", v)
	}
	// The figure at the top of the ladder still has to appear: -99.6% and
	// -100% are different facts about an account.
	if !strings.Contains(v, "-99.6%") {
		t.Errorf("the verdict must carry the figure at the top rung: %q", v)
	}
}

func TestCapacityVerdictNamesTheThreshold(t *testing.T) {
	c := &Capacity{
		Points: []CapacityPoint{
			{Capital: 1e6, TotalReturn: 0.4, Turnover: 20},
			{Capital: 1e7, TotalReturn: 0.1, Turnover: 20},
			{Capital: 1e8, TotalReturn: -0.3, Turnover: 20},
			{Capital: 1e9, TotalReturn: -0.72, Turnover: 20},
		},
		Bites: true, Deployed: true, Degradation: Ratio(1.12),
	}
	c.ZeroReturnCapital = capacityCrossing(c.Points, 0)
	v := capacityVerdict(c)
	if !strings.Contains(v, "the edge is gone above") {
		t.Errorf("the headline is the size the edge dies at: %q", v)
	}
	if !strings.Contains(v, "-72.0%") {
		t.Errorf("the verdict must say what the top of the ladder returns: %q", v)
	}
	if !strings.Contains(v, "estimates") {
		t.Errorf("an interpolated threshold has to be labelled as an estimate: %q", v)
	}
}

func TestCapacityVerdictRefusesToJudgeAStrategyThatDidNotScale(t *testing.T) {
	// Fixed dollar sizing: every rung submits the same order, so the flat
	// returns say nothing about capacity.
	c := &Capacity{
		Points: []CapacityPoint{
			{Capital: 1e5, TotalReturn: 0.4, Turnover: 8},
			{Capital: 1e9, TotalReturn: 0.0004, Turnover: 0.0008},
		},
		Deployed: false, Degradation: Ratio(0.3996),
	}
	if v := capacityVerdict(c); !strings.Contains(v, "never put to work") {
		t.Errorf("the ladder must decline to answer rather than report a threshold: %q", v)
	}
}

func TestCapacityAlwaysEncodes(t *testing.T) {
	// Every rung of an empty ladder leaves its ratios undefined, and one NaN
	// reaching the encoder truncates the whole response.
	c := &Capacity{
		Points:            []CapacityPoint{{Capital: 1e5, Sharpe: Ratio(math.NaN()), Error: "boom"}},
		ZeroReturnCapital: Ratio(math.NaN()), BenchmarkCapital: Ratio(math.Inf(1)),
		BenchmarkReturn: Ratio(math.NaN()), Degradation: Ratio(math.NaN()),
	}
	mustEncode(t, "Capacity", c)
}
