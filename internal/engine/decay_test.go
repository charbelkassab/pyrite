package engine

import (
	"math"
	"strings"
	"testing"

	"github.com/charbelkassab/pyrite/internal/market"
)

// decaySeries builds a symbol whose closes are exactly the values given,
// starting on the first weekday of 2021 and running one bar per calendar day.
// Real prices are not needed here: what is under test is which bar each
// horizon reads, and a hand-written path is the only way to know the answer
// in advance.
func decaySeries(symbol string, closes ...float64) (*market.Series, []market.Day) {
	bars := make([]market.Bar, len(closes))
	days := make([]market.Day, len(closes))
	d := market.Day("2021-01-04")
	for i, p := range closes {
		bars[i] = market.Bar{
			Date: d, Open: p, High: p, Low: p, Close: p, AdjClose: p, Volume: 1e6,
		}
		days[i] = d
		d = d.Add(1)
	}
	return market.NewSeries(symbol, bars), days
}

// repeatTrade makes n identical closed round trips, which is how a curve
// built for a single price path is given enough trades to be read.
func repeatTrade(n int, t Trade) []Trade {
	out := make([]Trade, n)
	for i := range out {
		out[i] = t
	}
	return out
}

func TestDecayPeaksWhereTheEdgeIs(t *testing.T) {
	// The whole move happens on the bar after entry and is handed back from
	// there. A curve that does not peak at bar 1 is reading the wrong bars.
	series, days := decaySeries("X", 100, 112, 108, 106, 104, 102)
	trades := repeatTrade(25, Trade{
		Symbol: "X", Direction: DirLong,
		EntryDate: days[0], ExitDate: days[5], EntryPrice: 100, Shares: 1,
	})

	d := ComputeDecay(trades, map[string]*market.Series{"X": series}, nil)
	if d.Trades != 25 {
		t.Fatalf("want 25 trades averaged, got %d", d.Trades)
	}
	if d.PeakBars != 1 {
		t.Errorf("the edge is entirely in the first bar, so the curve must peak "+
			"there; it peaked at %d bars", d.PeakBars)
	}
	if got := d.Points[0].MeanReturn; math.Abs(got-0.12) > 1e-9 {
		t.Errorf("bar 1 should read 12%%, got %.4f", got)
	}
	if !d.ExitReturn.Defined() || math.Abs(float64(d.ExitReturn)-0.02) > 1e-9 {
		t.Errorf("the trade finished at 2%%, got %v", float64(d.ExitReturn))
	}
	if d.StillRising {
		t.Error("the curve turned over on bar 2 and the trade was held to bar 5, " +
			"so it was not still rising at the exit")
	}
	// (12% - 2%) / 12%.
	if !d.GivenBack.Defined() || math.Abs(float64(d.GivenBack)-0.8333333) > 1e-6 {
		t.Errorf("give-back: got %v, want 0.833", float64(d.GivenBack))
	}
}

func TestDecayDoesNotCountHorizonsPastTheExitAsZero(t *testing.T) {
	// One short trade that made 10% and one long one that made nothing. At
	// horizon 10 the short trade has been closed for seven bars: it must
	// contribute the 10% it finished with, so the mean is 5%. Counting it as
	// zero would report 0% and invent a decay out of the holding periods.
	shortSer, shortDays := decaySeries("SHORT", 100, 105, 110)
	longSer, longDays := decaySeries("LONG",
		100, 100, 100, 100, 100, 100, 100, 100, 100, 100, 100, 100)

	trades := []Trade{
		{Symbol: "SHORT", Direction: DirLong, EntryDate: shortDays[0],
			ExitDate: shortDays[2], EntryPrice: 100, Shares: 1},
		{Symbol: "LONG", Direction: DirLong, EntryDate: longDays[0],
			ExitDate: longDays[11], EntryPrice: 100, Shares: 1},
	}
	series := map[string]*market.Series{"SHORT": shortSer, "LONG": longSer}

	d := ComputeDecay(trades, series, []int{1, 2, 3, 5, 10})
	for _, p := range d.Points {
		if p.Bars < 3 {
			continue
		}
		if math.Abs(p.MeanReturn-0.05) > 1e-9 {
			t.Errorf("horizon %d averaged %.4f, want 0.05 — a trade shorter than the "+
				"horizon is being counted as zero rather than as the return it "+
				"finished with", p.Bars, p.MeanReturn)
		}
	}
	// The sample must not shrink either: both trades are in every row.
	last := d.Points[len(d.Points)-1]
	if d.Trades != 2 {
		t.Errorf("both trades belong to the curve at every horizon, got %d", d.Trades)
	}
	if last.StillOpen != 1 {
		t.Errorf("one trade was still open at bar 10, got %d", last.StillOpen)
	}
}

func TestDecayReadsShortsInTheDirectionTheyWereTaken(t *testing.T) {
	// A falling price is a gain for a short, and the curve has to say so or
	// every short strategy reads as a total loss.
	series, days := decaySeries("X", 100, 90, 95, 98)
	trades := repeatTrade(20, Trade{
		Symbol: "X", Direction: DirShort,
		EntryDate: days[0], ExitDate: days[3], EntryPrice: 100, Shares: 1,
	})

	d := ComputeDecay(trades, map[string]*market.Series{"X": series}, []int{1, 2, 3})
	if got := d.Points[0].MeanReturn; math.Abs(got-0.10) > 1e-9 {
		t.Errorf("a short entered at 100 and marked at 90 is up 10%%, got %.4f", got)
	}
	if d.PeakBars != 1 {
		t.Errorf("the short was at its best on bar 1, got bar %d", d.PeakBars)
	}
}

func TestDecayExcludesOpenTrades(t *testing.T) {
	// Open positions are excluded from every other trade statistic for the
	// same reason: their outcome is not known yet.
	series, days := decaySeries("X", 100, 110, 120)
	d := ComputeDecay([]Trade{
		{Symbol: "X", Direction: DirLong, EntryDate: days[0], ExitDate: days[2],
			EntryPrice: 100, Shares: 1},
		{Symbol: "X", Direction: DirLong, EntryDate: days[0],
			EntryPrice: 100, Shares: 1, Open: true},
	}, map[string]*market.Series{"X": series}, []int{1})
	if d.Trades != 1 {
		t.Errorf("only the closed round trip belongs in the curve, got %d", d.Trades)
	}
}

func TestDecayVerdictSaysTooFewTrades(t *testing.T) {
	series, days := decaySeries("X", 100, 112, 102)
	d := ComputeDecay(repeatTrade(3, Trade{
		Symbol: "X", Direction: DirLong,
		EntryDate: days[0], ExitDate: days[2], EntryPrice: 100, Shares: 1,
	}), map[string]*market.Series{"X": series}, nil)
	if !strings.Contains(d.Verdict, "too few") {
		t.Errorf("three round trips cannot support a decay curve: %q", d.Verdict)
	}
}

func TestDecayVerdictNamesTheExitAsTheProblem(t *testing.T) {
	series, days := decaySeries("X", 100, 112, 108, 106, 104, 102)
	d := ComputeDecay(repeatTrade(25, Trade{
		Symbol: "X", Direction: DirLong,
		EntryDate: days[0], ExitDate: days[5], EntryPrice: 100, Shares: 1,
	}), map[string]*market.Series{"X": series}, nil)
	if !strings.Contains(d.Verdict, "exit rule is the thing to change") {
		t.Errorf("a peak at bar 1 held to bar 5 is an exit problem: %q", d.Verdict)
	}
}

func TestDecayVerdictDeclinesWhenTheHoldOutrunsTheHorizons(t *testing.T) {
	// Held for far longer than the longest horizon, so the curve describes
	// the opening of the trade and nothing about its exit. Reporting a peak
	// at the last horizon as though it were the peak of the trade would be a
	// statement about the grid.
	closes := make([]float64, 80)
	for i := range closes {
		closes[i] = 100 + float64(i)
	}
	series, days := decaySeries("X", closes...)
	d := ComputeDecay(repeatTrade(25, Trade{
		Symbol: "X", Direction: DirLong, EntryDate: days[0],
		ExitDate: days[len(days)-1], EntryPrice: 100, Shares: 1,
	}), map[string]*market.Series{"X": series}, nil)

	if d.GivenBack.Defined() {
		t.Errorf("the curve never turned over inside the window, so nothing has been "+
			"given back yet; got %v", float64(d.GivenBack))
	}
	if !strings.Contains(d.Verdict, "past the longest horizon measured") {
		t.Errorf("the verdict must say the curve does not reach the exit: %q", d.Verdict)
	}
}

func TestDecayWithoutSeriesIsUndefinedRatherThanZero(t *testing.T) {
	d := ComputeDecay([]Trade{{Symbol: "X", EntryPrice: 100}}, nil, nil)
	if len(d.Points) != 0 || d.Trades != 0 {
		t.Fatalf("no price series means no curve, got %d points", len(d.Points))
	}
	for name, r := range map[string]Ratio{
		"peak": d.PeakReturn, "exit": d.ExitReturn, "give-back": d.GivenBack,
	} {
		if r.Defined() {
			t.Errorf("%s should be undefined, not %v", name, float64(r))
		}
	}
	mustEncode(t, "SignalDecay", d)
}

func TestCritiqueFlagsAnEdgeThatDiesBeforeTheExit(t *testing.T) {
	series, days := decaySeries("X", 100, 112, 108, 106, 104, 102)
	decay := ComputeDecay(repeatTrade(25, Trade{
		Symbol: "X", Direction: DirLong,
		EntryDate: days[0], ExitDate: days[5], EntryPrice: 100, Shares: 1,
	}), map[string]*market.Series{"X": series}, nil)

	res := &Result{
		Spec:       Spec{Costs: DefaultCosts()},
		Curve:      curveOf(100, 101, 102),
		Metrics:    Metrics{TradingDays: 3, Years: 3},
		TradeStats: TradeStats{Closed: 25},
		Decay:      decay,
	}
	f := hasFinding(Criticise(res), "gone long before the exit")
	if f == nil {
		t.Fatalf("a peak at bar 1 against a five-bar hold must be raised: %+v",
			Criticise(res).Findings)
	}
	if f.Severity != SeverityWarning {
		t.Errorf("severity: got %s", f.Severity)
	}
	if !strings.Contains(f.Detail, "83%") {
		t.Errorf("the finding has to carry the give-back figure: %q", f.Detail)
	}
}

func TestCritiqueLeavesAStrategyStillRisingAlone(t *testing.T) {
	series, days := decaySeries("X", 100, 101, 103, 106, 110, 115)
	decay := ComputeDecay(repeatTrade(25, Trade{
		Symbol: "X", Direction: DirLong,
		EntryDate: days[0], ExitDate: days[5], EntryPrice: 100, Shares: 1,
	}), map[string]*market.Series{"X": series}, nil)

	res := &Result{
		Spec:       Spec{Costs: DefaultCosts()},
		Curve:      curveOf(100, 101, 102),
		Metrics:    Metrics{TradingDays: 3, Years: 3},
		TradeStats: TradeStats{Closed: 25},
		Decay:      decay,
	}
	if f := hasFinding(Criticise(res), "gone long before the exit"); f != nil {
		t.Errorf("a curve still climbing at the exit is not an exit problem: %q", f.Detail)
	}
}
