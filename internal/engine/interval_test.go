package engine

import (
	"context"
	"math"
	"testing"

	"github.com/charbelkassab/pyrite/internal/market"
)

// holdCode buys once and never sells, so the equity curve is the asset itself
// and any difference between bar sizes is the measurement, not the strategy.
const holdCode = `
	function setup(ctx) { ctx.universe(["AAPL"]); ctx.warmup(5); }
	function onDay(ctx) {
		if (!ctx.hasPosition("AAPL")) ctx.buy("AAPL", { pctCash: 0.99 });
	}
`

func runAt(t *testing.T, iv market.Interval) *Result {
	t.Helper()
	spec := baseSpec(holdCode)
	spec.Universe = []string{"AAPL"}
	spec.Start, spec.End = "2022-01-03", "2022-06-30"
	spec.Interval = iv
	spec.Costs = Costs{}
	spec.OmitDayRecords = true

	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("%s: %v", iv, err)
	}
	return res
}

// The single most important property of the interval work. Annualised
// volatility is a property of the asset, not of how finely you sampled it, so
// buy-and-hold measured on 30-minute bars and on daily bars must agree.
//
// If the annualisation factor were left at 252 for intraday, the intraday
// figure would come out about five times too small here — and the Sharpe
// ratio about five times too large, in the flattering direction.
func TestAnnualisedVolatilityAgreesAcrossBarSizes(t *testing.T) {
	daily := runAt(t, market.Interval1d)
	half := runAt(t, market.Interval30m)

	if daily.Metrics.Volatility <= 0 || half.Metrics.Volatility <= 0 {
		t.Fatalf("no volatility measured: daily %v, 30m %v",
			daily.Metrics.Volatility, half.Metrics.Volatility)
	}
	ratio := half.Metrics.Volatility / daily.Metrics.Volatility
	if ratio < 0.5 || ratio > 2.0 {
		t.Errorf("annualised volatility differs by %.2fx between 30m (%.4f) and daily (%.4f); "+
			"the periods-per-year factor is wrong",
			ratio, half.Metrics.Volatility, daily.Metrics.Volatility)
	}

	// And the total return must be essentially identical: it is the same
	// position over the same period.
	if math.Abs(half.Metrics.TotalReturn-daily.Metrics.TotalReturn) > 0.05 {
		t.Errorf("total return differs across bar sizes: 30m %.4f, daily %.4f",
			half.Metrics.TotalReturn, daily.Metrics.TotalReturn)
	}
}

func TestIntradayProducesMoreBarsThanDaily(t *testing.T) {
	daily := runAt(t, market.Interval1d)
	five := runAt(t, market.Interval5m)

	if len(five.Curve) <= len(daily.Curve) {
		t.Fatalf("5m produced %d bars against daily's %d", len(five.Curve), len(daily.Curve))
	}
	// 78 five-minute bars per session, so the ratio should be in that region.
	ratio := float64(len(five.Curve)) / float64(len(daily.Curve))
	if ratio < 40 || ratio > 120 {
		t.Errorf("5m/daily bar ratio is %.0f, which is not a session's worth", ratio)
	}
	// The curve must carry timestamps, not bare dates.
	if !five.Curve[0].Date.Intraday() {
		t.Errorf("intraday curve points should carry a time: %v", five.Curve[0].Date)
	}
	if daily.Curve[0].Date.Intraday() {
		t.Errorf("daily curve points should not: %v", daily.Curve[0].Date)
	}
}

// Calendar questions must mean the same thing at any bar size: "the first
// session of the month" is about the month, not about the bar.
func TestCalendarHelpersAreBarSizeIndependent(t *testing.T) {
	code := `
		function setup(ctx) { ctx.universe(["AAPL"]); ctx.warmup(5); }
		function onDay(ctx) {
			if (ctx.isFirstTradingDayOfMonth()) ctx.buy("AAPL", { notional: 100 }, "month " + ctx.date);
		}
	`
	count := func(iv market.Interval) int {
		spec := baseSpec(code)
		spec.Universe = []string{"AAPL"}
		spec.Start, spec.End = "2022-01-03", "2022-06-30"
		spec.Interval = iv
		spec.OmitDayRecords = true
		res, err := New(spec, newTestStore(t)).Run(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", iv, err)
		}
		return len(res.Fills)
	}

	daily := count(market.Interval1d)
	intra := count(market.Interval1h)
	if daily != intra {
		t.Errorf("first-of-month fired %d times daily and %d times hourly; "+
			"the calendar should not depend on the bar size", daily, intra)
	}
	if daily != 6 {
		t.Errorf("January to June is six month starts, got %d", daily)
	}
}

func TestAttributionGroupsIntradayBarsByTheirDate(t *testing.T) {
	res := runAt(t, market.Interval1h)
	if len(res.Attribution.ByYear) != 1 {
		t.Fatalf("a six-month run spans one year, got %d groups: %+v",
			len(res.Attribution.ByYear), res.Attribution.ByYear)
	}
	if res.Attribution.ByYear[0].Label != "2022" {
		t.Errorf("year label from an intraday stamp: got %q", res.Attribution.ByYear[0].Label)
	}
	if len(res.Attribution.ByMonth) != 6 {
		t.Errorf("January to June is six months, got %d", len(res.Attribution.ByMonth))
	}
}

func TestScaleRefusesToBeSwapped(t *testing.T) {
	// The type exists so that the two numbers cannot be passed the wrong way
	// round. This checks the arithmetic it encodes.
	sc := Scale{PeriodsPerYear: 252, RiskFree: 0.05}
	if got := sc.PerPeriodRF(); math.Abs(got-0.05/252) > 1e-12 {
		t.Errorf("per-period risk-free: got %v", got)
	}
	if got := sc.Root(); math.Abs(got-math.Sqrt(252)) > 1e-12 {
		t.Errorf("root: got %v", got)
	}
	if got := sc.Vol(0.01); math.Abs(got-0.01*math.Sqrt(252)) > 1e-12 {
		t.Errorf("vol: got %v", got)
	}
	// A zero or negative periods value falls back rather than dividing by it.
	if got := (Scale{}).Periods(); got != TradingDaysPerYear {
		t.Errorf("empty scale should default to daily, got %v", got)
	}
	if !math.IsNaN(sc.Sharpe(0.001, 0)) {
		t.Error("a zero deviation should give an undefined Sharpe, not an infinity")
	}
}

func TestScaleForKnowsEachBarSize(t *testing.T) {
	if got := ScaleFor(market.Interval1d, 0).Periods(); got != 252 {
		t.Errorf("daily: %v", got)
	}
	if got := ScaleFor(market.Interval1h, 0).Periods(); got != 252*6.5 {
		t.Errorf("hourly: %v", got)
	}
	// An unset or bogus interval falls back to daily rather than to zero.
	if got := ScaleFor("", 0).Periods(); got != 252 {
		t.Errorf("empty: %v", got)
	}
	if got := ScaleFor("fortnightly", 0).Periods(); got != 252 {
		t.Errorf("bogus: %v", got)
	}
}
