package engine

import (
	"context"
	"math"
	"testing"
)

// churn trades a large fraction of the account every other day, so order size
// is the variable under test.
const churnCode = `
	function setup(ctx) { ctx.universe(["AAPL"]); ctx.warmup(30); }
	function onDay(ctx) {
		if (ctx.dayIndex % 2 === 0) ctx.buy("AAPL", { pctCash: 0.98 });
		else ctx.close("AAPL");
	}
`

func impactSpec(cash, k float64) Spec {
	spec := baseSpec(churnCode)
	spec.Universe = []string{"AAPL"}
	spec.Start, spec.End = "2020-01-06", "2022-12-30"
	spec.InitialCash = cash
	spec.Costs = DefaultCosts()
	spec.Costs.ImpactCoefficient = k
	spec.OmitDayRecords = true
	return spec
}

func TestImpactIsOffByDefault(t *testing.T) {
	spec := impactSpec(100000, 0)
	e := New(spec, newTestStore(t))
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if e.portfolio.Impact != nil {
		t.Error("a zero coefficient should leave the impact model unwired")
	}
}

func TestImpactGrowsWithOrderSize(t *testing.T) {
	// The same strategy at two account sizes. With impact on, the larger
	// account must pay more per dollar traded — which is the entire point.
	store := newTestStore(t)

	small := New(impactSpec(50_000, 1), store)
	smallRes, err := small.Run(context.Background())
	if err != nil {
		t.Fatalf("small: %v", err)
	}
	big := New(impactSpec(500_000_000, 1), store)
	bigRes, err := big.Run(context.Background())
	if err != nil {
		t.Fatalf("big: %v", err)
	}

	smallCost := smallRes.Metrics.TotalCosts / smallRes.Metrics.StartValue
	bigCost := bigRes.Metrics.TotalCosts / bigRes.Metrics.StartValue
	if !(bigCost > smallCost*1.5) {
		t.Errorf("a 10,000x larger account paid %.4f of equity in costs against %.4f — "+
			"impact is not scaling with size", bigCost, smallCost)
	}
	// And the large account's returns must be worse for it.
	if !(bigRes.Metrics.TotalReturn < smallRes.Metrics.TotalReturn) {
		t.Errorf("size was free: %.4f vs %.4f",
			bigRes.Metrics.TotalReturn, smallRes.Metrics.TotalReturn)
	}
}

func TestMarketImpactFollowsTheSquareRootLaw(t *testing.T) {
	spec := impactSpec(100000, 1)
	e := New(spec, newTestStore(t))
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Re-point the engine at a mid-run day so history is available.
	e.today = e.days[len(e.days)/2]
	e.snapshotPrices(e.today)

	base := e.marketImpact("AAPL", 1000)
	if base <= 0 {
		t.Skip("synthetic data provided no usable volume for this symbol")
	}
	quad := e.marketImpact("AAPL", 4000)
	// Four times the size should cost about twice as much, not four times.
	ratio := quad / base
	if math.Abs(ratio-2) > 0.15 {
		t.Errorf("quadrupling the order changed impact by %.2fx, want about 2x", ratio)
	}
	// Direction must not matter: selling into a market costs the same as
	// buying out of it under this model.
	if math.Abs(e.marketImpact("AAPL", -1000)-base) > 1e-12 {
		t.Error("impact should be symmetric in direction")
	}
}

func TestMarketImpactIsCappedAndSafe(t *testing.T) {
	spec := impactSpec(100000, 1)
	e := New(spec, newTestStore(t))
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	e.today = e.days[len(e.days)/2]
	e.snapshotPrices(e.today)

	// An absurd order must not produce an impact above the cap, or a fill
	// price below zero.
	if got := e.marketImpact("AAPL", 1e15); got > 0.5 {
		t.Errorf("impact should cap at 0.5, got %v", got)
	}
	// Unknown symbols and zero-size orders return nothing rather than
	// guessing, because an invented cost looks like modelling.
	if got := e.marketImpact("NOSUCHSYMBOL", 1000); got != 0 {
		t.Errorf("an unknown symbol should have no modelled impact, got %v", got)
	}
	if got := e.marketImpact("AAPL", 0); got != 0 {
		t.Errorf("a zero-size order has no impact, got %v", got)
	}
}

func TestImpactAppearsInSlippageAccounting(t *testing.T) {
	// Impact is a cost and must be recorded as one, not silently absorbed
	// into the fill price where nothing counts it.
	store := newTestStore(t)
	withOut, err := New(impactSpec(200_000_000, 0), store).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	withIn, err := New(impactSpec(200_000_000, 1), store).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !(withIn.Metrics.TotalCosts > withOut.Metrics.TotalCosts) {
		t.Errorf("impact did not reach the cost total: %v vs %v",
			withIn.Metrics.TotalCosts, withOut.Metrics.TotalCosts)
	}
}
