package engine

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/charbelkassab/natural-quant/internal/market"
)

// newTestStore builds a store backed by the deterministic synthetic provider,
// so these tests need no network and no API keys.
func newTestStore(t *testing.T) *market.Store {
	t.Helper()
	fund, err := market.LoadFundamentals("")
	if err != nil {
		t.Fatalf("load fundamentals: %v", err)
	}
	return market.NewStore(market.NewSyntheticProvider(), nil, fund)
}

func baseSpec(code string) Spec {
	return Spec{
		Name:            "test",
		Code:            code,
		Universe:        []string{"AAPL", "MSFT", "NVDA"},
		Start:           "2022-01-03",
		End:             "2022-12-30",
		InitialCash:     100000,
		AllowFractional: true,
		Warmup:          30,
	}
}

func TestBuyAndHoldTracksTheAsset(t *testing.T) {
	// A strategy that buys once and never trades again must match a
	// buy-and-hold benchmark on the same symbol, apart from entry friction.
	spec := baseSpec(`
		function onDay(ctx) {
			if (ctx.dayIndex >= 0 && !ctx.hasPosition("AAPL") && ctx.cash > 1000) {
				ctx.buy("AAPL", { pctCash: 0.99 });
			}
		}
	`)
	spec.Universe = []string{"AAPL"}
	spec.Benchmarks = []string{"AAPL"}
	spec.Costs = Costs{} // frictionless so the comparison is exact

	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Curve) < 200 {
		t.Fatalf("expected a full year of bars, got %d", len(res.Curve))
	}
	if len(res.Benchmarks) != 1 {
		t.Fatalf("expected 1 benchmark, got %d", len(res.Benchmarks))
	}

	got := res.Metrics.TotalReturn
	want := res.Benchmarks[0].Metric.TotalReturn
	// ~1% cash drag from the 0.99 sizing plus a one-day entry lag.
	if math.Abs(got-want) > 0.03 {
		t.Errorf("buy-and-hold diverged from benchmark: strategy %.4f vs benchmark %.4f", got, want)
	}
	if res.StrategyErrors != 0 {
		t.Errorf("unexpected strategy errors: %d (%v)", res.StrategyErrors, res.Warnings)
	}
}

func TestNoTradesLeavesCashUntouched(t *testing.T) {
	res, err := New(baseSpec(`function onDay(ctx) {}`), newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.Metrics.EndValue; math.Abs(got-100000) > 1e-6 {
		t.Errorf("idle strategy changed equity: got %f, want 100000", got)
	}
	if res.Metrics.TotalTrades != 0 {
		t.Errorf("idle strategy recorded %d trades", res.Metrics.TotalTrades)
	}
}

func TestOrdersFillAtNextOpenNotTodayClose(t *testing.T) {
	// The order is placed on the first day; the fill must be dated later,
	// proving the engine is not letting the strategy trade on a price it has
	// already seen.
	spec := baseSpec(`
		var placed = false;
		function onDay(ctx) {
			if (!placed) { ctx.buy("AAPL", { shares: 10 }); placed = true; ctx.log("ordered on " + ctx.date); }
		}
	`)
	spec.Universe = []string{"AAPL"}
	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Fills) != 1 {
		t.Fatalf("expected exactly 1 fill, got %d", len(res.Fills))
	}
	orderDay := res.Days[0].Date
	fillDay := res.Fills[0].Date
	if !(fillDay > orderDay) {
		t.Errorf("fill on %s should be strictly after order day %s", fillDay, orderDay)
	}
}

func TestShortPositionProfitsWhenPriceFalls(t *testing.T) {
	// Hand-built series: a straight decline. A short must make money.
	bars := []market.Bar{}
	price := 100.0
	for d := market.Day("2022-01-03"); d <= "2022-03-01"; d = d.Add(1) {
		wd := d.Time().Weekday()
		if wd == 0 || wd == 6 {
			continue
		}
		price *= 0.99
		bars = append(bars, market.Bar{
			Date: d, Open: price, High: price * 1.001,
			Low: price * 0.999, Close: price, AdjClose: price, Volume: 1e6,
		})
	}
	store := market.NewStore(&fixedProvider{series: map[string]*market.Series{
		"DOWN": market.NewSeries("DOWN", bars),
	}}, nil, mustFundamentals(t))

	spec := Spec{
		Name: "short", Universe: []string{"DOWN"},
		Start: "2022-01-10", End: "2022-03-01",
		InitialCash: 100000, AllowShort: true, AllowFractional: true,
		Costs: Costs{},
		Code: `
			function onDay(ctx) {
				if (!ctx.hasPosition("DOWN")) ctx.short("DOWN", { shares: 100 });
			}
		`,
	}
	res, err := New(spec, store).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Metrics.TotalReturn <= 0 {
		t.Errorf("short of a falling asset should profit, got %.4f", res.Metrics.TotalReturn)
	}
}

func TestBareCoverClosesTheShortRatherThanBuying(t *testing.T) {
	// Regression: ctx.cover(sym) with no size once fell through to "spend all
	// cash", which left the short open and stacked a long on top of it. A
	// bare cover must flatten the position and nothing more.
	spec := baseSpec(`
		function onDay(ctx) {
			if (ctx.dayIndex === 1) ctx.short("AAPL", { shares: 50 });
			if (ctx.dayIndex === 20) ctx.cover("AAPL");
		}
	`)
	spec.Universe = []string{"AAPL"}
	spec.AllowShort = true
	spec.Costs = Costs{}

	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	last := res.Days[len(res.Days)-1]
	if len(last.Positions) != 0 {
		t.Fatalf("expected a flat book after cover, got %+v", last.Positions)
	}
	if len(res.Fills) != 2 {
		t.Fatalf("expected exactly 2 fills (short then cover), got %d", len(res.Fills))
	}
	if res.Fills[1].Side != SideCover {
		t.Errorf("second fill should be a cover, got %q", res.Fills[1].Side)
	}
	if res.Fills[1].Shares != 50 {
		t.Errorf("cover should buy back exactly 50 shares, got %v", res.Fills[1].Shares)
	}
}

func TestReasonStringInSizePositionStillTrades(t *testing.T) {
	// Regression: ctx.cover(sym, "why") passes a reason where a size object
	// was expected. That is a natural thing to write, since ctx.close() takes
	// a reason in the same position, and it must not silently drop the order.
	spec := baseSpec(`
		function onDay(ctx) {
			if (ctx.dayIndex === 0) ctx.short("AAPL", { shares: 25 });
			if (ctx.dayIndex === 15) ctx.cover("AAPL", "signal reversed");
		}
	`)
	spec.Universe = []string{"AAPL"}
	spec.AllowShort = true
	spec.Costs = Costs{}

	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Fills) != 2 {
		t.Fatalf("expected a short and a cover, got %d fills", len(res.Fills))
	}
	if res.Fills[1].Reason != "signal reversed" {
		t.Errorf("the reason string should be recorded, got %q", res.Fills[1].Reason)
	}
	last := res.Days[len(res.Days)-1]
	if len(last.Positions) != 0 {
		t.Errorf("expected a flat book, got %+v", last.Positions)
	}
}

func TestOversizedCoverDoesNotFlipLong(t *testing.T) {
	spec := baseSpec(`
		function onDay(ctx) {
			if (ctx.dayIndex === 1) ctx.short("AAPL", { shares: 10 });
			if (ctx.dayIndex === 20) ctx.cover("AAPL", { shares: 500 });
		}
	`)
	spec.Universe = []string{"AAPL"}
	spec.AllowShort = true
	spec.Costs = Costs{}

	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	last := res.Days[len(res.Days)-1]
	if len(last.Positions) != 0 {
		t.Fatalf("an oversized cover must leave the book flat, got %+v", last.Positions)
	}
}

func TestSpendAllCashDoesNotOverdraw(t *testing.T) {
	// "Buy with all available cash" must size against the price the order
	// actually fills at, including slippage and commission. Sizing against
	// the clean reference price leaves the account slightly negative.
	spec := baseSpec(`
		function onDay(ctx) {
			if (!ctx.hasPosition("AAPL")) ctx.buy("AAPL", { pctCash: 1 });
		}
	`)
	spec.Universe = []string{"AAPL"}
	spec.Costs = Costs{SlippageBps: 25, CommissionPct: 0.001, CommissionPerShare: 0.005}

	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, d := range res.Days {
		if d.Cash < -1e-6 {
			t.Fatalf("cash went negative (%.4f) on %s", d.Cash, d.Date)
		}
	}
	if len(res.Fills) == 0 {
		t.Fatal("expected the strategy to trade")
	}
}

func TestStopLossExitsPosition(t *testing.T) {
	bars := []market.Bar{}
	price := 100.0
	for i, d := 0, market.Day("2022-01-03"); d <= "2022-03-01"; d = d.Add(1) {
		wd := d.Time().Weekday()
		if wd == 0 || wd == 6 {
			continue
		}
		// Rise for 10 sessions, then collapse.
		if i < 10 {
			price *= 1.01
		} else {
			price *= 0.94
		}
		bars = append(bars, market.Bar{
			Date: d, Open: price, High: price * 1.002,
			Low: price * 0.998, Close: price, AdjClose: price, Volume: 1e6,
		})
		i++
	}
	store := market.NewStore(&fixedProvider{series: map[string]*market.Series{
		"CRASH": market.NewSeries("CRASH", bars),
	}}, nil, mustFundamentals(t))

	spec := Spec{
		Name: "stop", Universe: []string{"CRASH"},
		Start: "2022-01-04", End: "2022-03-01",
		InitialCash: 100000, AllowFractional: true, Costs: Costs{},
		Code: `
			function onDay(ctx) {
				if (!ctx.hasPosition("CRASH")) {
					ctx.buy("CRASH", { pctCash: 0.9, stopLoss: 0.10 });
				}
			}
		`,
	}
	res, err := New(spec, store).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var stopFills int
	for _, f := range res.Fills {
		if f.Tag == "stop" {
			stopFills++
		}
	}
	if stopFills == 0 {
		t.Errorf("expected at least one stop-loss exit, got none (%d fills)", len(res.Fills))
	}
}

func TestMarketCapRankingPicksLargest(t *testing.T) {
	spec := baseSpec(`
		function onDay(ctx) {
			var top = ctx.biggestCompany();
			if (top) ctx.log("top=" + top + " cap=" + ctx.marketCap(top));
		}
	`)
	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Days) == 0 || len(res.Days[0].Logs) == 0 {
		t.Fatalf("expected market cap logs on the first day")
	}
}

func TestRebalanceReachesTargetWeights(t *testing.T) {
	spec := baseSpec(`
		function onDay(ctx) {
			if (ctx.isFirstTradingDayOfMonth()) {
				ctx.equalWeight(["AAPL", "MSFT", "NVDA"]);
			}
		}
	`)
	spec.Costs = Costs{}
	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	last := res.Days[len(res.Days)-1]
	if len(last.Positions) != 3 {
		t.Fatalf("expected 3 positions at the end, got %d", len(last.Positions))
	}
	for _, p := range last.Positions {
		if p.Weight < 0.2 || p.Weight > 0.47 {
			t.Errorf("%s weight %.3f is far from the 1/3 target", p.Symbol, p.Weight)
		}
	}
}

func TestMetricsAreSane(t *testing.T) {
	spec := baseSpec(`function onDay(ctx) { if (!ctx.hasPosition("AAPL")) ctx.buy("AAPL"); }`)
	spec.Universe = []string{"AAPL"}
	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := res.Metrics
	if m.MaxDrawdown > 0 {
		t.Errorf("max drawdown should be <= 0, got %f", m.MaxDrawdown)
	}
	if m.Volatility <= 0 {
		t.Errorf("volatility should be positive, got %f", m.Volatility)
	}
	if m.TradingDays != len(res.Curve) {
		t.Errorf("trading days %d != curve length %d", m.TradingDays, len(res.Curve))
	}
	if !m.Sharpe.Defined() {
		t.Errorf("sharpe should be defined for a real price series: %v", float64(m.Sharpe))
	}
}

func TestStrategyErrorDoesNotAbortRun(t *testing.T) {
	spec := baseSpec(`
		function onDay(ctx) {
			if (ctx.dayIndex % 50 === 0) { throw new Error("boom"); }
		}
	`)
	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("a per-day exception must not fail the whole run: %v", err)
	}
	if res.StrategyErrors == 0 {
		t.Errorf("expected recorded strategy errors")
	}
	if len(res.Curve) < 200 {
		t.Errorf("run should have continued past the errors, got %d days", len(res.Curve))
	}
}

func TestMissingOnDayIsAClearError(t *testing.T) {
	_, err := New(baseSpec(`var x = 1;`), newTestStore(t)).Run(context.Background())
	if err == nil {
		t.Fatal("expected an error when onDay is absent")
	}
}

// fixedProvider serves pre-built series for deterministic tests.
type fixedProvider struct{ series map[string]*market.Series }

func (f *fixedProvider) Name() string { return "fixed" }

func (f *fixedProvider) Fetch(_ context.Context, symbol string, _, _ market.Day) (*market.Series, error) {
	if s, ok := f.series[symbol]; ok {
		return s, nil
	}
	return nil, market.ErrNotFound
}

func (f *fixedProvider) Search(context.Context, string) ([]market.Quote, error) { return nil, nil }

func mustFundamentals(t *testing.T) *market.Fundamentals {
	t.Helper()
	f, err := market.LoadFundamentals("")
	if err != nil {
		t.Fatalf("fundamentals: %v", err)
	}
	return f
}

// The API reference documents ctx.universe([...]) and ctx.warmup(n) as things
// setup() sets. Both are read by loadData, which runs before setup(), so
// without a reload pass they are documented but inert: a strategy that chooses
// its own symbols fails with "empty universe", and one that raises its own
// warm-up silently trades on too little history.
func TestSetupCanChooseTheUniverse(t *testing.T) {
	spec := baseSpec(`
		function setup(ctx) { ctx.universe(["MSFT"]); }
		function onDay(ctx) {
			if (ctx.dayIndex === 0) ctx.buy("MSFT", { pctCash: 0.9 }, "chosen in setup");
		}
	`)
	// Deliberately empty: setup() is the only thing that names a symbol.
	spec.Universe = nil

	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("a strategy that sets its own universe should run: %v", err)
	}
	if len(res.Fills) == 0 {
		t.Fatal("no fills: the universe setup() chose was never loaded")
	}
	if res.Fills[0].Symbol != "MSFT" {
		t.Errorf("traded %s, want MSFT", res.Fills[0].Symbol)
	}
}

func TestSetupCanRaiseTheWarmup(t *testing.T) {
	// A 200-day average is only available on the first traded day if setup()'s
	// warm-up was honoured, because the spec asked for far less.
	spec := baseSpec(`
		function setup(ctx) { ctx.universe(["AAPL"]); ctx.warmup(200); }
		function onDay(ctx) {
			if (ctx.dayIndex === 0 && ctx.sma("AAPL", 200) !== null) {
				ctx.buy("AAPL", { pctCash: 0.5 }, "had 200 bars on day one");
			}
		}
	`)
	spec.Universe = []string{"AAPL"}
	spec.Warmup = 5

	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Fills) == 0 {
		t.Fatal("the 200-day average was unavailable, so setup()'s warm-up was ignored")
	}
}

func TestEmptyUniverseAfterSetupIsStillAnError(t *testing.T) {
	// Deferring the check must not swallow it: a strategy that never names a
	// symbol should still say so clearly.
	spec := baseSpec(`function onDay(ctx) {}`)
	spec.Universe = nil

	_, err := New(spec, newTestStore(t)).Run(context.Background())
	if err == nil {
		t.Fatal("expected an error for a strategy with no symbols at all")
	}
	if !strings.Contains(err.Error(), "universe") {
		t.Errorf("the error should name the problem: %v", err)
	}
}
