package engine

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/charbelkassab/pyrite/internal/market"
)

// holdOne buys the named symbol once and never trades again, so the equity
// curve is the asset and any difference between two runs is the measurement.
// The universe is left to the spec rather than declared in setup(), so a run
// can hold one symbol and still carry a second in its universe — which is how
// a mixed calendar arises.
func holdOne(sym string) string {
	return `function setup(ctx) { ctx.warmup(5); }
		function onDay(ctx) {
			if (!ctx.hasPosition("` + sym + `")) ctx.buy("` + sym + `", { pctCash: 0.99 });
		}`
}

func runHold(t *testing.T, syms ...string) *Result {
	t.Helper()
	spec := baseSpec(holdOne(syms[0]))
	spec.Universe = syms
	spec.Start, spec.End = "2021-01-04", "2022-12-30"
	spec.Costs = Costs{}
	spec.Warmup = 5
	spec.OmitDayRecords = true
	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("%v: %v", syms, err)
	}
	return res
}

// The headline claim of the whole calendar change: a crypto series is scaled
// by 365 and a US equity by 252, and the run says which it used.
func TestCryptoAnnualisesAt365AndEquityAt252(t *testing.T) {
	crypto := runHold(t, "BTC-USD")
	if got := crypto.Manifest.PeriodsPerYear; got != 365 {
		t.Errorf("BTC-USD annualised by %v, want 365", got)
	}
	if got := crypto.Manifest.TradingCalendar; got != market.CalendarContinuous {
		t.Errorf("BTC-USD ran on the %q calendar", got)
	}

	equity := runHold(t, "SPY")
	if got := equity.Manifest.PeriodsPerYear; got != TradingDaysPerYear {
		t.Errorf("SPY annualised by %v, want %v", got, TradingDaysPerYear)
	}
	if got := equity.Manifest.TradingCalendar; got != market.CalendarUSEquity {
		t.Errorf("SPY ran on the %q calendar", got)
	}
}

// The same return series scored on both calendars differs by exactly the root
// of the session ratio, and by nothing else. Anything other than that factor
// means the calendar is reaching something it should not.
func TestSameCurveOnTwoCalendarsDiffersByTheRootOfTheRatio(t *testing.T) {
	base := runHold(t, "BTC-USD")
	if len(base.Curve) < 100 {
		t.Fatalf("only %d points to score", len(base.Curve))
	}

	// Risk-free is zero here on purpose: with a non-zero rate the per-bar
	// subtraction moves too, and the ratio stops being a clean root.
	equityScale := ScaleOn(market.Interval1d, market.CalendarUSEquity, 0)
	cryptoScale := ScaleOn(market.Interval1d, market.CalendarContinuous, 0)

	on252 := ComputeMetrics(base.Curve, equityScale)
	on365 := ComputeMetrics(base.Curve, cryptoScale)

	want := math.Sqrt(365.0 / 252.0)
	gotSharpe := float64(on365.Sharpe) / float64(on252.Sharpe)
	if math.Abs(gotSharpe-want) > 1e-12 {
		t.Errorf("Sharpe scaled by %.15f, want exactly sqrt(365/252) = %.15f",
			gotSharpe, want)
	}
	gotVol := on365.Volatility / on252.Volatility
	if math.Abs(gotVol-want) > 1e-12 {
		t.Errorf("volatility scaled by %.15f, want %.15f", gotVol, want)
	}
	// CAGR is compounded from the calendar span, so the divisor must not
	// touch it at all.
	if on365.CAGR != on252.CAGR {
		t.Errorf("CAGR moved with the calendar: %v against %v", on365.CAGR, on252.CAGR)
	}
	if on365.TotalReturn != on252.TotalReturn {
		t.Errorf("total return moved with the calendar")
	}
}

// A mixed book is annualised by the widest calendar, because the curve is
// marked on the union of the sessions — and it says so out loud rather than
// picking one quietly.
func TestMixedUniverseUsesTheWidestCalendarAndSaysSo(t *testing.T) {
	res := runHold(t, "BTC-USD", "SPY")
	if got := res.Manifest.TradingCalendar; got != market.CalendarContinuous {
		t.Errorf("a mixed universe ran on %q, want the widest calendar", got)
	}
	if got := res.Manifest.PeriodsPerYear; got != 365 {
		t.Errorf("a mixed universe annualised by %v, want 365", got)
	}
	if len(res.Manifest.Calendars) != 2 {
		t.Errorf("the manifest should list both calendars, got %v", res.Manifest.Calendars)
	}

	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "mixes trading calendars") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("a mixed universe must be reported, warnings were %v", res.Warnings)
	}
	var noted bool
	for _, f := range res.Critique.Findings {
		if f.Title == "the universe mixes trading calendars" {
			noted = true
			if !strings.Contains(f.Detail, "stale price") {
				t.Errorf("the finding should say what the mixture costs: %s", f.Detail)
			}
		}
	}
	if !noted {
		t.Error("the critique should carry the mixed-calendar finding")
	}

	// A single-calendar run must not carry either.
	clean := runHold(t, "SPY")
	if len(clean.Manifest.Calendars) != 0 {
		t.Errorf("a single-calendar run listed %v", clean.Manifest.Calendars)
	}
}

// An explicit calendar overrules the data, and survives the second data load
// that happens when setup() names its own universe.
func TestExplicitCalendarOverrulesTheData(t *testing.T) {
	spec := baseSpec(holdOne("BTC-USD"))
	spec.Universe = []string{"BTC-USD"}
	spec.Start, spec.End = "2021-01-04", "2022-12-30"
	spec.Costs = Costs{}
	spec.Warmup = 5
	spec.OmitDayRecords = true
	spec.Calendar = market.CalendarUSEquity

	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.Manifest.PeriodsPerYear; got != TradingDaysPerYear {
		t.Errorf("an explicit calendar was overruled: annualised by %v", got)
	}
}

// Without bars to judge, the tickers still have to be used: a crypto universe
// must not silently fall back to 252 before the data arrives.
func TestSpecScaleGuessesFromTheUniverse(t *testing.T) {
	crypto := Spec{Universe: []string{"BTC-USD"}, Interval: market.Interval1d}
	if got := crypto.Scale().Periods(); got != 365 {
		t.Errorf("an unloaded crypto spec annualises by %v, want 365", got)
	}
	equity := Spec{Universe: []string{"SPY"}, Interval: market.Interval1d}
	if got := equity.Scale().Periods(); got != TradingDaysPerYear {
		t.Errorf("an unloaded equity spec annualises by %v, want %v", got, TradingDaysPerYear)
	}
	// And an unrecognised calendar is not honoured as though it meant
	// something.
	bad := Spec{Universe: []string{"SPY"}, Interval: market.Interval1d, Calendar: "lunar"}
	bad.ApplyDefaults()
	if bad.Calendar != "" {
		t.Errorf("an invalid calendar survived ApplyDefaults as %q", bad.Calendar)
	}
}

// ScaleFor is what every caller that has never heard of a calendar still
// uses, so it must go on meaning the US equity session exactly.
func TestScaleForIsUnchangedForUSEquities(t *testing.T) {
	old := ScaleFor(market.Interval1d, 0.02)
	if old.Periods() != TradingDaysPerYear {
		t.Errorf("ScaleFor moved to %v", old.Periods())
	}
	if old.Market() != market.CalendarUSEquity {
		t.Errorf("ScaleFor is on the %q calendar", old.Market())
	}
	if DailyScale(0.02).Periods() != TradingDaysPerYear {
		t.Errorf("DailyScale moved to %v", DailyScale(0.02).Periods())
	}
	// A zero Scale still falls back rather than dividing by nothing.
	if (Scale{}).Periods() != TradingDaysPerYear {
		t.Errorf("an empty Scale annualises by %v", (Scale{}).Periods())
	}
	if (Scale{}).Market() != market.CalendarUSEquity {
		t.Errorf("an empty Scale is on the %q calendar", (Scale{}).Market())
	}
}
