package engine

import (
	"strings"
	"testing"

	"github.com/charbelkassab/pyrite/internal/market"
)

// critiqueOf runs Criticise and indexes the findings by title fragment.
func hasFinding(c Critique, fragment string) *Finding {
	for i := range c.Findings {
		if strings.Contains(c.Findings[i].Title, fragment) ||
			strings.Contains(c.Findings[i].Detail, fragment) {
			return &c.Findings[i]
		}
	}
	return nil
}

func curveOf(values ...float64) []EquityPoint {
	out := make([]EquityPoint, len(values))
	for i, v := range values {
		out[i] = EquityPoint{Date: "2020-01-01", Value: v}
	}
	return out
}

func TestCritiqueFlagsCloseFills(t *testing.T) {
	res := &Result{
		Spec:    Spec{Fill: FillClose, Costs: DefaultCosts()},
		Curve:   curveOf(100, 101, 102),
		Metrics: Metrics{TradingDays: 3, Years: 1},
	}
	c := Criticise(res)
	f := hasFinding(c, "already seen")
	if f == nil {
		t.Fatalf("close fills must be flagged: %+v", c.Findings)
	}
	if f.Severity != SeverityCritical {
		t.Errorf("close-fill lookahead is critical, got %v", f.Severity)
	}
}

func TestCritiqueFlagsFrictionlessHighTurnover(t *testing.T) {
	res := &Result{
		Spec:    Spec{Fill: FillNextOpen, Costs: Costs{}},
		Curve:   curveOf(100, 110, 120),
		Metrics: Metrics{TradingDays: 3, Years: 3, Turnover: 12},
	}
	c := Criticise(res)
	f := hasFinding(c, "Frictionless")
	if f == nil {
		f = hasFinding(c, "frictionless")
	}
	if f == nil {
		t.Fatalf("zero costs must be flagged: %+v", c.Findings)
	}
	if f.Severity != SeverityCritical {
		t.Errorf("frictionless at 12x turnover should be critical, got %v", f.Severity)
	}
}

func TestCritiqueDoesNotFlagCostsWhenCharged(t *testing.T) {
	res := &Result{
		Spec:    Spec{Fill: FillNextOpen, Costs: DefaultCosts()},
		Curve:   curveOf(100, 110, 120),
		Metrics: Metrics{TradingDays: 3, Years: 3, Turnover: 1},
	}
	if f := hasFinding(Criticise(res), "frictionless"); f != nil {
		t.Errorf("costs were charged, so nothing to flag: %+v", f)
	}
}

func TestCritiqueFlagsTooFewTrades(t *testing.T) {
	res := &Result{
		Spec:       Spec{Fill: FillNextOpen, Costs: DefaultCosts()},
		Curve:      curveOf(100, 110),
		Metrics:    Metrics{TradingDays: 2, Years: 5},
		TradeStats: TradeStats{Closed: 6},
	}
	f := hasFinding(Criticise(res), "too few trades")
	if f == nil {
		t.Fatal("six trades is not a sample")
	}
	if f.Severity != SeverityCritical {
		t.Errorf("severity: got %v", f.Severity)
	}
}

func TestCritiqueFlagsShortVolatilityShape(t *testing.T) {
	res := &Result{
		Spec:       Spec{Fill: FillNextOpen, Costs: DefaultCosts()},
		Curve:      curveOf(100, 101, 102),
		Metrics:    Metrics{TradingDays: 3, Years: 5},
		TradeStats: TradeStats{Closed: 100},
		Risk:       RiskMetrics{Skew: -1.4, ExcessKurtosis: 9},
	}
	f := hasFinding(Criticise(res), "short volatility")
	if f == nil {
		t.Fatal("left skew with fat tails is the signature and must be named")
	}
	if !strings.Contains(f.Detail, "Sharpe") {
		t.Errorf("the finding should explain why Sharpe misleads here: %q", f.Detail)
	}
}

func TestCritiqueFlagsBenchmarkDominance(t *testing.T) {
	res := &Result{
		Spec:       Spec{Fill: FillNextOpen, Costs: DefaultCosts()},
		Curve:      curveOf(100, 105),
		Metrics:    Metrics{TradingDays: 2, Years: 5, TotalReturn: 0.05, MaxDrawdown: -0.40},
		TradeStats: TradeStats{Closed: 50},
		Benchmarks: []BenchmarkResult{{
			Symbol: "SPY", Label: "SPY",
			Metric: Metrics{TotalReturn: 0.90, MaxDrawdown: -0.20},
		}},
	}
	f := hasFinding(Criticise(res), "Beaten by the benchmark")
	if f == nil {
		f = hasFinding(Criticise(res), "beaten by the benchmark")
	}
	if f == nil {
		t.Fatalf("worse on both axes must be stated: %+v", Criticise(res).Findings)
	}
}

func TestCritiqueFlagsConcentration(t *testing.T) {
	res := &Result{
		Spec:       Spec{Fill: FillNextOpen, Costs: DefaultCosts()},
		Curve:      curveOf(100, 200),
		Metrics:    Metrics{TradingDays: 2, Years: 5, TotalReturn: 1.0},
		TradeStats: TradeStats{Closed: 50},
		Attribution: Attribution{Stress: []StressResult{
			{Label: "excluding the best month", Return: 0.1, ShareOfTotal: 0.75},
		}},
	}
	f := hasFinding(Criticise(res), "concentrated")
	if f == nil {
		t.Fatal("a strategy whose gain is one month must be called out")
	}
	if f.Severity != SeverityCritical {
		t.Errorf("75%% in one month is critical, got %v", f.Severity)
	}
}

func TestCritiqueFlagsAILookahead(t *testing.T) {
	res := &Result{
		Spec:        Spec{Fill: FillNextOpen, Costs: DefaultCosts()},
		Curve:       curveOf(100, 150),
		Metrics:     Metrics{TradingDays: 2, Years: 5},
		TradeStats:  TradeStats{Closed: 40},
		AICallCount: 200,
	}
	f := hasFinding(Criticise(res), "consulted a model")
	if f == nil {
		t.Fatal("in-loop model calls carry lookahead and must be stated")
	}
	if f.Severity != SeverityCritical {
		t.Errorf("severity: got %v", f.Severity)
	}
}

func TestTrustScoreFallsWithSeverity(t *testing.T) {
	clean := trustScore(nil)
	if clean != 100 {
		t.Errorf("no findings should score 100, got %d", clean)
	}
	warned := trustScore([]Finding{{Severity: SeverityWarning}})
	critical := trustScore([]Finding{{Severity: SeverityCritical}})
	if !(critical < warned && warned < clean) {
		t.Errorf("scores should order by severity: %d %d %d", critical, warned, clean)
	}
	if trustScore([]Finding{
		{Severity: SeverityCritical}, {Severity: SeverityCritical},
		{Severity: SeverityCritical}, {Severity: SeverityCritical},
		{Severity: SeverityCritical}, {Severity: SeverityCritical},
	}) != 0 {
		t.Error("the score should floor at zero, not go negative")
	}
	// Scores are deliberately coarse.
	if trustScore([]Finding{{Severity: SeverityNote}})%5 != 0 {
		t.Error("scores should round to a multiple of 5")
	}
}

func TestCritiqueOnCleanResultSaysSoCarefully(t *testing.T) {
	res := &Result{
		Spec:       Spec{Fill: FillNextOpen, Costs: DefaultCosts()},
		Curve:      curveOf(100, 110, 121),
		Metrics:    Metrics{TradingDays: 3, Years: 6, Turnover: 0.5, TotalReturn: 0.21},
		TradeStats: TradeStats{Closed: 200, EdgeRatio: 1.4},
		Risk:       RiskMetrics{Skew: 0.2, ExcessKurtosis: 1},
	}
	c := Criticise(res)
	if len(c.Findings) != 0 {
		t.Fatalf("this result has nothing to flag: %+v", c.Findings)
	}
	if !strings.Contains(c.Headline, "not the same as right") {
		t.Errorf("a clean result should not be endorsed: %q", c.Headline)
	}
}

func TestCritiqueHandlesEmptyResult(t *testing.T) {
	if c := Criticise(nil); len(c.Findings) != 0 {
		t.Error("a nil result should produce nothing rather than panic")
	}
	if c := Criticise(&Result{}); len(c.Findings) != 0 {
		t.Error("an empty result should produce nothing rather than panic")
	}
}

func TestConcentrationIsReportedOnce(t *testing.T) {
	// Both stress tests fire on the same underlying fact. Two findings with
	// identical titles read as a bug, not as emphasis.
	res := &Result{
		Spec:       Spec{Fill: FillNextOpen, Costs: DefaultCosts()},
		Curve:      curveOf(100, 200),
		Metrics:    Metrics{TradingDays: 2, Years: 5, TotalReturn: 1.0},
		TradeStats: TradeStats{Closed: 50},
		Attribution: Attribution{Stress: []StressResult{
			{Label: "excluding the best month", Return: 0.2, ShareOfTotal: 0.76},
			{Label: "excluding the 5 best days", Return: 0.1, ShareOfTotal: 0.91},
		}},
	}
	c := Criticise(res)

	var n int
	var detail string
	for _, f := range c.Findings {
		if strings.Contains(f.Title, "concentrated") {
			n++
			detail = f.Detail
		}
	}
	if n != 1 {
		t.Fatalf("concentration should be reported once, got %d", n)
	}
	// And it should report the worst of the two, not whichever came first.
	if !strings.Contains(detail, "91%") {
		t.Errorf("the strongest concentration should be the one reported: %q", detail)
	}
}

// A run with no fills has nothing in it to assess. Scoring it by subtracting a
// fixed penalty from 100 put an empty result above a real one with two flaws,
// which inverts the whole point of the number.
func TestNeverTradedScoresZero(t *testing.T) {
	res := &Result{
		Curve:   curveOf(100, 100, 100),
		Metrics: Metrics{TradingDays: 500, Years: 2},
	}
	c := Criticise(res)
	if c.TrustScore != 0 {
		t.Errorf("a run that never traded scored %d, want 0", c.TrustScore)
	}
	var found bool
	for _, f := range c.Findings {
		if f.Title == "the strategy never traded" {
			found = true
			if f.Severity != SeverityCritical {
				t.Errorf("severity = %q, want critical", f.Severity)
			}
		}
	}
	if !found {
		t.Error("no finding said the strategy never traded")
	}
}

// A year that returned exactly zero did not lose money. Counting "not
// positive" as "negative" told a strategy that never traded that most of its
// years lost money, when none of them did.
func TestFlatYearsAreNotLosingYears(t *testing.T) {
	res := &Result{
		Curve:   curveOf(100, 100, 100),
		Metrics: Metrics{TradingDays: 800, Years: 3},
		Attribution: Attribution{ByYear: []PeriodStats{
			{Label: "2021", Return: 0},
			{Label: "2022", Return: 0},
			{Label: "2023", Return: 0},
		}},
	}
	for _, f := range Criticise(res).Findings {
		if f.Title == "most years lost money" {
			t.Errorf("a flat curve was reported as losing money: %s", f.Detail)
		}
	}

	// A genuinely losing run must still be caught.
	res.Attribution.ByYear = []PeriodStats{
		{Label: "2021", Return: -0.10},
		{Label: "2022", Return: -0.05},
		{Label: "2023", Return: 0.02},
	}
	var found bool
	for _, f := range Criticise(res).Findings {
		if f.Title == "most years lost money" {
			found = true
		}
	}
	if !found {
		t.Error("two losing years out of three went unreported")
	}
}

func TestCritiqueSurfacesDataDefects(t *testing.T) {
	res := &Result{
		Spec:    Spec{Fill: FillNextOpen, Costs: DefaultCosts()},
		Curve:   curveOf(100, 101, 102),
		Metrics: Metrics{TradingDays: 3, Years: 3},
		DataQuality: []market.Finding{{
			Severity: market.SeverityCritical,
			Kind:     market.KindSplit,
			Symbol:   "AAPL",
			Title:    "a split that looks unadjusted",
			Detail: "On 2020-08-31 the adjusted close stepped from 499.23 to 124.81, " +
				"a -75.0% move that matches a 4:1 split.",
		}},
	}
	c := Criticise(res)
	f := hasFinding(c, "price data")
	if f == nil {
		t.Fatalf("a defect in the bars must reach the critique: %+v", c.Findings)
	}
	if f.Severity != SeverityCritical {
		t.Errorf("a defect in the data outranks everything computed from it, got %v", f.Severity)
	}
	// The evidence has to travel with the claim, or it is an opinion.
	for _, want := range []string{"AAPL", "2020-08-31", "4:1"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("the finding must carry its evidence (%q): %s", want, f.Detail)
		}
	}
}
