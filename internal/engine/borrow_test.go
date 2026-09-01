package engine

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/charbelkassab/pyrite/internal/market"
)

func TestParseBorrowCSV(t *testing.T) {
	sched, err := ParseBorrowCSV(strings.NewReader(
		"symbol,annual_pct,available\n" +
			"# a comment line\n" +
			"aapl,0.3\n" +
			"GME,85%,yes\n" +
			"SIRI,,no\n" +
			"HTZ,12,n/a\n" +
			"*,2.5\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sched.GeneralCollateralPct != 0.025 {
		t.Errorf("general collateral is %v, want 0.025", sched.GeneralCollateralPct)
	}
	for sym, want := range map[string]float64{"AAPL": 0.003, "GME": 0.85} {
		r, ok := sched.Rate(sym, 0.03)
		if !ok {
			t.Errorf("%s should be borrowable", sym)
			continue
		}
		if math.Abs(r-want) > 1e-12 {
			t.Errorf("%s is %v, want %v", sym, r, want)
		}
	}
	// A blank rate and an explicit "no" both mean there is no locate.
	for _, sym := range []string{"SIRI", "HTZ"} {
		if _, ok := sched.Rate(sym, 0.03); ok {
			t.Errorf("%s should have no locate", sym)
		}
	}
	// Anything unnamed falls to general collateral, not to the caller's rate.
	if r, ok := sched.Rate("MSFT", 0.03); !ok || r != 0.025 {
		t.Errorf("an unnamed symbol got %v (%v), want the GC rate", r, ok)
	}

	if !sched.HardToBorrow("GME") || !sched.HardToBorrow("SIRI") {
		t.Error("a special rate and a missing locate are both hard to borrow")
	}
	if sched.HardToBorrow("AAPL") || sched.HardToBorrow("MSFT") {
		t.Error("general collateral is not hard to borrow")
	}
}

func TestParseBorrowCSVRejectsNonsense(t *testing.T) {
	for _, in := range []string{
		"AAPL,not-a-number\n",
		"AAPL,-3\n",
		"\n",
		"*,\n",
		// ParseFloat accepts both of these, and either would truncate the
		// JSON response the moment it reached the manifest.
		"AAPL,NaN\n",
		"AAPL,Inf\n",
	} {
		if _, err := ParseBorrowCSV(strings.NewReader(in)); err == nil {
			t.Errorf("%q was accepted", strings.TrimSpace(in))
		}
	}
}

// The manifest records which rates a run used without carrying the table, so
// the fingerprint has to be stable across read order and sensitive to a
// changed rate.
func TestBorrowFingerprintIsStableAndSensitive(t *testing.T) {
	a := &BorrowSchedule{GeneralCollateralPct: 0.02, Rates: map[string]BorrowRate{
		"AAPL": {AnnualPct: 0.003}, "GME": {Unavailable: true},
	}}
	b := &BorrowSchedule{GeneralCollateralPct: 0.02, Rates: map[string]BorrowRate{
		"GME": {Unavailable: true}, "AAPL": {AnnualPct: 0.003},
	}}
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("the same rates hashed differently depending on insertion order")
	}
	c := &BorrowSchedule{GeneralCollateralPct: 0.02, Rates: map[string]BorrowRate{
		"AAPL": {AnnualPct: 0.004}, "GME": {Unavailable: true},
	}}
	if a.Fingerprint() == c.Fingerprint() {
		t.Error("a changed rate produced the same fingerprint")
	}
	var nilSched *BorrowSchedule
	if nilSched.Fingerprint() != "" || nilSched.Names() != 0 {
		t.Error("a nil schedule should fingerprint to nothing")
	}
}

// The schedule is hashed into the manifest rather than copied into it: a
// stock loan file runs to thousands of names and the manifest sits on every
// saved run.
func TestManifestHashesTheBorrowScheduleRatherThanCarryingIt(t *testing.T) {
	spec := shortSpec(shortCode)
	spec.Costs.Borrow = &BorrowSchedule{
		Rates: map[string]BorrowRate{"AAPL": {AnnualPct: 0.40}},
	}
	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Manifest.Costs.Borrow != nil {
		t.Error("the manifest carried the rate table")
	}
	if res.Manifest.BorrowNames != 1 || res.Manifest.BorrowSHA256 == "" {
		t.Errorf("the manifest recorded %d names and hash %q",
			res.Manifest.BorrowNames, res.Manifest.BorrowSHA256)
	}
	// The spec still carries it, because that is what a re-run needs.
	if res.Spec.Costs.Borrow == nil {
		t.Error("the spec lost the schedule it was given")
	}
}

// A nil schedule has to behave exactly as the engine did before there was
// one: everything borrowable, everything at the flat rate.
func TestNilBorrowScheduleChargesTheFlatRate(t *testing.T) {
	var sched *BorrowSchedule
	r, ok := sched.Rate("ANYTHING", 0.03)
	if !ok || r != 0.03 {
		t.Errorf("a nil schedule returned %v (%v)", r, ok)
	}
	if sched.PerName() || sched.HardToBorrow("ANYTHING") {
		t.Error("a nil schedule should claim nothing about any name")
	}
}

// shortCode opens a short on the first day and holds it, so the only thing
// that accrues afterwards is the borrow.
const shortCode = `
	function setup(ctx) { ctx.warmup(5); }
	function onDay(ctx) {
		if (!ctx.hasPosition("AAPL")) ctx.short("AAPL", { pctEquity: 0.5 }, "short it");
	}
`

func shortSpec(code string) Spec {
	spec := baseSpec(code)
	spec.Universe = []string{"AAPL"}
	spec.Start, spec.End = "2022-01-03", "2022-12-30"
	spec.AllowShort = true
	spec.Warmup = 5
	spec.OmitDayRecords = true
	spec.Costs = Costs{ShortBorrowAnnualPct: 0.03}
	return spec
}

// The point of the availability flag: a name nobody could borrow cannot be
// shorted, and the run says so instead of quietly charging a fee for a trade
// that never existed.
func TestAHardToBorrowNameCannotBeShorted(t *testing.T) {
	spec := shortSpec(shortCode)
	spec.Costs.Borrow = &BorrowSchedule{
		Rates: map[string]BorrowRate{"AAPL": {Unavailable: true}},
	}
	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, f := range res.Fills {
		if f.Side == SideShort {
			t.Fatalf("a short filled in a name with no locate: %+v", f)
		}
	}
	if res.Borrow == nil || len(res.Borrow.Refused) == 0 {
		t.Fatal("the run did not record the refusal")
	}
	ref := res.Borrow.Refused[0]
	if ref.Symbol != "AAPL" || ref.Orders == 0 || ref.Shares <= 0 {
		t.Errorf("refusal recorded as %+v", ref)
	}
	if res.Borrow.TotalCost != 0 {
		t.Errorf("a refused short was charged %v of borrow", res.Borrow.TotalCost)
	}

	var said bool
	for _, f := range res.Critique.Findings {
		if strings.Contains(f.Title, "could not be borrowed") {
			said = true
			if !strings.Contains(f.Detail, "AAPL") {
				t.Errorf("the finding does not name the symbol: %s", f.Detail)
			}
		}
	}
	if !said {
		t.Error("the critique must say the shorts were refused")
	}
}

// The same run with a locate available: the short opens and the fee is
// charged at the name's own rate rather than at general collateral.
func TestABorrowableNameIsChargedItsOwnRate(t *testing.T) {
	spec := shortSpec(shortCode)
	spec.Costs.Borrow = &BorrowSchedule{
		GeneralCollateralPct: 0.02,
		Rates:                map[string]BorrowRate{"AAPL": {AnnualPct: 0.40}},
	}
	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Borrow == nil || len(res.Borrow.Names) != 1 {
		t.Fatalf("expected one name in the borrow report, got %+v", res.Borrow)
	}
	n := res.Borrow.Names[0]
	if n.Symbol != "AAPL" || n.AnnualPct != 0.40 {
		t.Errorf("charged %s at %v", n.Symbol, n.AnnualPct)
	}
	if !n.HardToBorrow {
		t.Error("40% a year is not general collateral")
	}
	if n.Cost <= 0 || n.Sessions < 200 {
		t.Errorf("borrow accrued %v over %d sessions", n.Cost, n.Sessions)
	}
	if !res.Borrow.PerName {
		t.Error("the report should record that names were priced individually")
	}

	var said bool
	for _, f := range res.Critique.Findings {
		if strings.Contains(f.Title, "hardest to borrow") {
			said = true
		}
	}
	if !said {
		t.Error("the critique must name the special-rate shorts")
	}
}

// Borrow is rent on a position, not a fee on a trade. Two runs holding the
// same short for the same number of sessions must pay the same, however many
// times they touched it — and holding it for twice as long must cost twice as
// much.
func TestBorrowAccruesPerSessionHeldNotPerTrade(t *testing.T) {
	prices := map[string]float64{"X": 100}

	oneTrade := NewPortfolio(100000, Costs{ShortBorrowAnnualPct: 0.10})
	if _, err := oneTrade.Execute("2022-01-03", "X", -100, 100, "", ""); err != nil {
		t.Fatalf("execute: %v", err)
	}
	for i := 0; i < 10; i++ {
		oneTrade.AccrueFinancing(prices, TradingDaysPerYear)
	}

	// The same exposure built in ten pieces rather than one.
	manyTrades := NewPortfolio(100000, Costs{ShortBorrowAnnualPct: 0.10})
	for i := 0; i < 10; i++ {
		if _, err := manyTrades.Execute("2022-01-03", "X", -10, 100, "", ""); err != nil {
			t.Fatalf("execute: %v", err)
		}
	}
	for i := 0; i < 10; i++ {
		manyTrades.AccrueFinancing(prices, TradingDaysPerYear)
	}
	_, _, _, few := oneTrade.Totals()
	_, _, _, many := manyTrades.Totals()
	if math.Abs(few-many) > 1e-9 {
		t.Errorf("ten trades paid %v against one trade's %v; the fee is on the "+
			"position, not the trade", many, few)
	}

	// Ten sessions at 10% a year on $10,000 short.
	want := 10 * 10000 * 0.10 / TradingDaysPerYear
	if math.Abs(few-want) > 1e-9 {
		t.Errorf("ten sessions accrued %v, want %v", few, want)
	}

	twice := NewPortfolio(100000, Costs{ShortBorrowAnnualPct: 0.10})
	if _, err := twice.Execute("2022-01-03", "X", -100, 100, "", ""); err != nil {
		t.Fatalf("execute: %v", err)
	}
	for i := 0; i < 20; i++ {
		twice.AccrueFinancing(prices, TradingDaysPerYear)
	}
	_, _, _, longer := twice.Totals()
	if math.Abs(longer-2*few) > 1e-9 {
		t.Errorf("twice the holding period cost %v, want %v", longer, 2*few)
	}
}

// A year of borrow is a year of borrow whatever the bar count: an equity book
// paying 252 daily charges and a crypto book paying 365 must both end the year
// having paid the annual rate once.
func TestBorrowIsAnnualWhateverTheCalendar(t *testing.T) {
	prices := map[string]float64{"X": 100}
	charge := func(periods float64) float64 {
		p := NewPortfolio(100000, Costs{ShortBorrowAnnualPct: 0.10})
		if _, err := p.Execute("2022-01-03", "X", -100, 100, "", ""); err != nil {
			t.Fatalf("execute: %v", err)
		}
		for i := 0; i < int(periods); i++ {
			p.AccrueFinancing(prices, periods)
		}
		_, _, _, borrow := p.Totals()
		return borrow
	}
	equity := charge(market.CalendarUSEquity.SessionsPerYear())
	crypto := charge(market.CalendarContinuous.SessionsPerYear())
	if math.Abs(equity-crypto) > 1e-9 {
		t.Errorf("a year on 252 bars cost %v and on 365 bars cost %v", equity, crypto)
	}
	if math.Abs(equity-1000) > 1e-9 {
		t.Errorf("a year at 10%% on $10,000 short cost %v, want 1000", equity)
	}
}

// Selling a long needs no locate, so an unavailable name must still be
// sellable — down to flat, and no further.
func TestNoLocateStillAllowsSellingALong(t *testing.T) {
	p := NewPortfolio(100000, Costs{ShortBorrowAnnualPct: 0.03})
	p.Borrow = &BorrowSchedule{Rates: map[string]BorrowRate{"X": {Unavailable: true}}}

	if _, err := p.Execute("2022-01-03", "X", 100, 100, "", ""); err != nil {
		t.Fatalf("buy: %v", err)
	}
	// Sell 150 of a 100 long: 100 fills, the 50 that would open a short does
	// not.
	f, err := p.Execute("2022-01-04", "X", -150, 100, "", "")
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if f == nil || f.Shares != 100 {
		t.Fatalf("the sell was clamped to %+v, want 100 shares", f)
	}
	if pos := p.Position("X"); pos != nil {
		t.Errorf("the book should be flat, holding %v", pos.Shares)
	}
	if p.borrowRefused["X"] != 1 {
		t.Errorf("the refused portion was not recorded: %v", p.borrowRefused)
	}
}

// Without a borrow file every short is charged the same rate, and the run has
// to say so — that flat assumption is the friction understated exactly where
// a short strategy expects its edge.
func TestFlatBorrowIsReportedAsAnAssumption(t *testing.T) {
	res, err := New(shortSpec(shortCode), newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Borrow == nil || res.Borrow.TotalCost <= 0 {
		t.Fatalf("no borrow was charged: %+v", res.Borrow)
	}
	if res.Borrow.PerName {
		t.Error("no schedule was supplied, so nothing was priced per name")
	}
	var said bool
	for _, f := range res.Critique.Findings {
		if f.Title == "every short was charged the same rate" {
			said = true
		}
	}
	if !said {
		t.Error("the critique must state the flat-rate assumption")
	}
}

// A long-only run must carry no borrow section at all: an empty one reads as
// a missing number rather than as an absent question.
func TestLongOnlyRunHasNoBorrowReport(t *testing.T) {
	spec := baseSpec(holdOne("SPY"))
	spec.Universe = []string{"SPY"}
	spec.Start, spec.End = "2022-01-03", "2022-12-30"
	spec.Warmup = 5
	spec.OmitDayRecords = true
	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Borrow.Shorted() {
		t.Errorf("a long-only run produced a borrow report: %+v", res.Borrow)
	}
}
