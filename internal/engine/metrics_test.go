package engine

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"

	"github.com/charbelkassab/pyrite/internal/market"
)

// curveFrom builds an equity curve on consecutive weekdays from raw values.
func curveFrom(start market.Day, values ...float64) []EquityPoint {
	out := make([]EquityPoint, 0, len(values))
	d := start
	peak := 0.0
	for _, v := range values {
		for {
			if wd := d.Time().Weekday(); wd != 0 && wd != 6 {
				break
			}
			d = d.Add(1)
		}
		if v > peak {
			peak = v
		}
		dd := 0.0
		if peak > 0 {
			dd = v/peak - 1
		}
		out = append(out, EquityPoint{Date: d, Value: v, Drawdown: dd})
		d = d.Add(1)
	}
	return out
}

func TestTotalReturnAndCAGR(t *testing.T) {
	// Exactly doubling over a full year should give ~100% total and ~100% CAGR.
	curve := []EquityPoint{
		{Date: "2020-01-02", Value: 100},
		{Date: "2021-01-01", Value: 200},
	}
	m := ComputeMetrics(curve, 0)

	if math.Abs(m.TotalReturn-1.0) > 1e-9 {
		t.Errorf("total return: got %v, want 1.0", m.TotalReturn)
	}
	if math.Abs(m.CAGR-1.0) > 0.02 {
		t.Errorf("CAGR over one year should match total return, got %v", m.CAGR)
	}
	if m.StartValue != 100 || m.EndValue != 200 {
		t.Errorf("boundary values wrong: %v -> %v", m.StartValue, m.EndValue)
	}
}

func TestCAGRAnnualisesMultiYearReturns(t *testing.T) {
	// 4x over four years is a 41.4% compound annual rate, not 100%.
	curve := []EquityPoint{
		{Date: "2020-01-02", Value: 100},
		{Date: "2024-01-02", Value: 400},
	}
	m := ComputeMetrics(curve, 0)
	want := math.Pow(4, 1.0/4.0) - 1 // ≈ 0.4142
	if math.Abs(m.CAGR-want) > 0.01 {
		t.Errorf("CAGR: got %v, want about %v", m.CAGR, want)
	}
}

func TestMaxDrawdownMeasuresPeakToTrough(t *testing.T) {
	// Rise to 120, fall to 60 (a 50% drawdown), then partially recover.
	curve := curveFrom("2024-01-01", 100, 110, 120, 90, 60, 80, 100)
	m := ComputeMetrics(curve, 0)

	if math.Abs(m.MaxDrawdown-(-0.5)) > 1e-9 {
		t.Errorf("max drawdown: got %v, want -0.5", m.MaxDrawdown)
	}
	if m.MaxDrawdownStart == "" || m.MaxDrawdownEnd == "" {
		t.Error("the drawdown period should be dated")
	}
	if m.MaxDrawdownStart >= m.MaxDrawdownEnd {
		t.Errorf("drawdown start %s should precede end %s", m.MaxDrawdownStart, m.MaxDrawdownEnd)
	}
}

func TestFlatCurveHasNoRiskAndNoReturn(t *testing.T) {
	curve := curveFrom("2024-01-01", 100, 100, 100, 100, 100)
	m := ComputeMetrics(curve, 0)

	if m.TotalReturn != 0 {
		t.Errorf("total return should be 0, got %v", m.TotalReturn)
	}
	if m.Volatility != 0 {
		t.Errorf("volatility should be 0, got %v", m.Volatility)
	}
	if m.MaxDrawdown != 0 {
		t.Errorf("max drawdown should be 0, got %v", m.MaxDrawdown)
	}
	// Sharpe is undefined with zero deviation; it must not be NaN or Inf.
	if m.Sharpe.Defined() && float64(m.Sharpe) != 0 {
		t.Errorf("a flat curve has no risk-adjusted return to report, got %v", float64(m.Sharpe))
	}
}

func TestEmptyAndSingletonCurvesDoNotPanic(t *testing.T) {
	if m := ComputeMetrics(nil, 0); m.TradingDays != 0 {
		t.Errorf("an empty curve should yield zeroed metrics, got %+v", m)
	}
	m := ComputeMetrics([]EquityPoint{{Date: "2024-01-02", Value: 100}}, 0)
	if m.TradingDays != 1 || m.TotalReturn != 0 {
		t.Errorf("single point: got %d days, return %v", m.TradingDays, m.TotalReturn)
	}
}

func TestSortinoIsUndefinedRatherThanZeroWithNoLosingDays(t *testing.T) {
	// A curve that only ever rises has no downside deviation, so Sortino is
	// mathematically undefined. Reporting it as 0 would read as the worst
	// possible score for what is actually the best possible case.
	curve := curveFrom("2024-01-01", 100, 104, 108, 115, 121, 130)
	m := ComputeMetrics(curve, 0)

	if m.Sortino.Defined() {
		t.Errorf("Sortino should be undefined with no losing days, got %v", float64(m.Sortino))
	}
	// And it must serialise as null rather than breaking the encoder.
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("metrics with an undefined ratio must still encode: %v", err)
	}
	if !bytes.Contains(b, []byte(`"sortino":null`)) {
		t.Errorf("expected sortino to encode as null, got %s", b)
	}
}

func TestMetricsWithInfiniteProfitFactorStillEncode(t *testing.T) {
	// A strategy with no losing trades yields an infinite profit factor.
	// encoding/json refuses NaN and Inf outright, so without the Ratio type
	// this would truncate the entire API response.
	m := Metrics{Years: 1}
	m.AddTradeStats([]Fill{
		{Side: SideSell, Value: 100, RealizedPnL: 50},
		{Side: SideSell, Value: 100, RealizedPnL: 25},
	}, 1000)

	if m.ProfitFactor.Defined() {
		t.Errorf("profit factor should be undefined with no losses, got %v", float64(m.ProfitFactor))
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encoding must not fail on an infinite ratio: %v", err)
	}
	if !bytes.Contains(b, []byte(`"profit_factor":null`)) {
		t.Errorf("expected profit_factor null, got %s", b)
	}
}

func TestTradeStatsCountOnlyRealisingFills(t *testing.T) {
	fills := []Fill{
		{Side: SideBuy, Value: 1000, Commission: 1}, // opening, no P&L
		{Side: SideSell, Value: 1200, Commission: 1, RealizedPnL: 200},
		{Side: SideBuy, Value: 1000, Commission: 1},
		{Side: SideSell, Value: 900, Commission: 1, RealizedPnL: -100},
	}
	m := Metrics{Years: 1}
	m.AddTradeStats(fills, 10000)

	if m.TotalTrades != 2 {
		t.Errorf("only realising fills are trades: got %d, want 2", m.TotalTrades)
	}
	if m.WinningTrades != 1 || m.LosingTrades != 1 {
		t.Errorf("win/loss split wrong: %d/%d", m.WinningTrades, m.LosingTrades)
	}
	if math.Abs(m.TradeWinRate-0.5) > 1e-9 {
		t.Errorf("win rate: got %v, want 0.5", m.TradeWinRate)
	}
	if math.Abs(float64(m.ProfitFactor)-2.0) > 1e-9 {
		t.Errorf("profit factor should be 200/100, got %v", float64(m.ProfitFactor))
	}
	if m.TotalCosts != 4 {
		t.Errorf("costs should sum commission and slippage across all fills, got %v", m.TotalCosts)
	}
}

func TestBenchmarkStatsAlignOnDate(t *testing.T) {
	// The benchmark is missing a day the strategy has; alignment must pair by
	// date rather than by index, or beta becomes nonsense.
	strat := []EquityPoint{
		{Date: "2024-01-02", Value: 100},
		{Date: "2024-01-03", Value: 102}, // benchmark has no bar here
		{Date: "2024-01-04", Value: 101},
		{Date: "2024-01-05", Value: 104},
		{Date: "2024-01-08", Value: 103},
		{Date: "2024-01-09", Value: 106},
	}
	bench := []EquityPoint{
		{Date: "2024-01-02", Value: 100},
		{Date: "2024-01-04", Value: 101},
		{Date: "2024-01-05", Value: 103},
		{Date: "2024-01-08", Value: 102},
		{Date: "2024-01-09", Value: 105},
	}
	m := ComputeMetrics(strat, 0)
	m.AddBenchmarkStats(strat, bench, 0)

	if !m.BetaVsBenchmark.Defined() {
		t.Error("beta should be computable from the three shared dates")
	}
	if c := float64(m.CorrelationVsBench); c < -1.0001 || c > 1.0001 {
		t.Errorf("correlation out of range: %v", c)
	}
}

func TestBuyAndHoldCurveUsesAdjustedCloses(t *testing.T) {
	// A 2:1 split halves the raw close but leaves adjusted returns flat. A
	// buy-and-hold curve must show no loss across the split.
	s := market.NewSeries("X", []market.Bar{
		{Date: "2024-01-02", Close: 100, AdjClose: 50},
		{Date: "2024-01-03", Close: 50, AdjClose: 50},
		{Date: "2024-01-04", Close: 55, AdjClose: 55},
	})
	days := []market.Day{"2024-01-02", "2024-01-03", "2024-01-04"}
	curve := BuyAndHoldCurve(s, days, 1000)

	if len(curve) != 3 {
		t.Fatalf("expected 3 points, got %d", len(curve))
	}
	if math.Abs(curve[1].Value-1000) > 1e-9 {
		t.Errorf("the split day should not change value, got %v", curve[1].Value)
	}
	if math.Abs(curve[2].Value-1100) > 1e-9 {
		t.Errorf("a 10%% adjusted gain should give 1100, got %v", curve[2].Value)
	}
}

func TestIndicatorsReturnNaNWhenHistoryIsShort(t *testing.T) {
	short := []float64{1, 2, 3}
	if !math.IsNaN(SMA(short, 10)) {
		t.Error("SMA should be NaN with insufficient history")
	}
	if !math.IsNaN(RSI(short, 14)) {
		t.Error("RSI should be NaN with insufficient history")
	}
	if !math.IsNaN(Stdev(short, 10)) {
		t.Error("Stdev should be NaN with insufficient history")
	}
	if r := MACD(short, 12, 26, 9); !math.IsNaN(r.MACD) {
		t.Error("MACD should be NaN with insufficient history")
	}
}

func TestSMAAndMomentumAreCorrect(t *testing.T) {
	v := []float64{10, 20, 30, 40, 50}
	if got := SMA(v, 5); got != 30 {
		t.Errorf("SMA: got %v, want 30", got)
	}
	if got := SMA(v, 2); got != 45 {
		t.Errorf("SMA of the last 2: got %v, want 45", got)
	}
	// Momentum over 4 bars: 50/10 - 1 = 4.
	if got := Momentum(v, 4); math.Abs(got-4) > 1e-9 {
		t.Errorf("Momentum: got %v, want 4", got)
	}
	if got := Highest(v, 3); got != 50 {
		t.Errorf("Highest: got %v, want 50", got)
	}
	if got := Lowest(v, 3); got != 30 {
		t.Errorf("Lowest: got %v, want 30", got)
	}
}
