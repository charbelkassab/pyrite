package engine

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charbelkassab/pyrite/internal/market"
)

// Scenario is one named historical window worth running a strategy through.
//
// A backtest over 2015 to 2023 reports one number for a period containing a
// melt-up, a pandemic crash, a mania and a rate shock, and the average hides
// every one of them. The question anybody actually has is "what did this do in
// 2008", and it is only answerable by measuring 2008 on its own.
type Scenario struct {
	Name  string     `json:"name"`
	Start market.Day `json:"start"`
	End   market.Day `json:"end"`
	// Description says what happened, because a return without the episode
	// attached to it is a number nobody can argue with.
	Description string `json:"description"`
	// IndexDrawdown is the S&P 500's own peak-to-trough decline inside the
	// window, from its published closes. It is fixed data rather than
	// something measured here, so that a reader can tell a bad strategy from
	// a bad market before reading anything else — and so that a benchmark
	// column disagreeing with it is visible as a data problem.
	IndexDrawdown float64 `json:"index_drawdown"`
}

// scenarioTable is the fixed set of windows.
//
// Every window opens on a session the index actually traded and closes on
// another, and the dates are the ones the episodes are conventionally dated
// by: the peak the fall started from and the trough it reached, except where
// the episode is named after a calendar period. The comment on each entry
// gives the two S&P 500 closes that IndexDrawdown is the ratio of, so the
// figure is checkable without running anything.
//
// The windows are in date order and do not overlap, which scenarioTable's test
// enforces: two windows covering the same sessions would report the same
// episode twice under different names.
var scenarioTable = []Scenario{
	// 328.08 on 1987-10-05 to 224.84 on 1987-10-19. The 20.5% fall on Monday
	// 19 October is still the largest single session in the index's history.
	{
		Name:  "Black Monday",
		Start: "1987-10-01", End: "1987-10-31",
		Description:   "the crash of Monday 19 October 1987, when the index lost a fifth of its value in one session",
		IndexDrawdown: -0.315,
	},
	// 1527.46 on 2000-03-24, the bubble's closing high, to 776.76 on
	// 2002-10-09, its low. The Nasdaq Composite fell about 78% over the same
	// window, so a technology-weighted strategy should look far worse here
	// than the S&P figure suggests.
	{
		Name:  "Dot-com collapse",
		Start: "2000-03-24", End: "2002-10-09",
		Description:   "the unwinding of the technology bubble, peak to trough, over two and a half years",
		IndexDrawdown: -0.492,
	},
	// 1565.15 on 2007-10-09 to 676.53 on 2009-03-09.
	{
		Name:  "Financial crisis",
		Start: "2007-10-09", End: "2009-03-09",
		Description:   "the credit crisis, from the pre-crisis high to the March 2009 low",
		IndexDrawdown: -0.568,
	},
	// 1202.26 on 2010-05-03 to 1071.59 on 2010-05-20. Deliberately short: the
	// flash crash itself was an afternoon, on 6 May 2010, and the window is
	// the fortnight of selling it sat inside. A strategy that shows no trades
	// over fifteen sessions has a warm-up problem, not a finding.
	{
		Name:  "Flash crash",
		Start: "2010-05-03", End: "2010-05-21",
		Description:   "the intraday collapse of 6 May 2010 and the fortnight of selling around it",
		IndexDrawdown: -0.109,
	},
	// 1345.02 on 2011-07-22 to 1099.23 on 2011-10-03. S&P removed the United
	// States' AAA rating after the close on Friday 5 August 2011.
	{
		Name:  "US downgrade",
		Start: "2011-07-22", End: "2011-10-03",
		Description:   "the loss of the United States' AAA rating on 5 August 2011, during the euro crisis",
		IndexDrawdown: -0.183,
	},
	// 1669.16 on 2013-05-21 to 1573.09 on 2013-06-24. The window opens the
	// session before Bernanke raised the possibility of slowing asset
	// purchases, on 22 May 2013, and closes at the yield peak in September.
	// This one is a bond episode: the ten-year went from 1.9% to 3.0% while
	// equities gave up under 6%, so a portfolio holding bonds for safety
	// should show the damage here and an equity-only one should not.
	{
		Name:  "Taper tantrum",
		Start: "2013-05-21", End: "2013-09-05",
		Description:   "the bond selloff after the May 2013 taper remarks; the ten-year yield went from 1.9% to 3.0%",
		IndexDrawdown: -0.058,
	},
	// 2109.79 on 2015-11-03 to 1829.08 on 2016-02-11. The window opens the
	// session before the PBoC devalued the yuan on 11 August 2015 and closes
	// at the February 2016 low, so it holds both legs of the selloff.
	{
		Name:  "China devaluation",
		Start: "2015-08-10", End: "2016-02-11",
		Description:   "the yuan devaluation of 11 August 2015 and the two selloffs that followed it",
		IndexDrawdown: -0.133,
	},
	// 2872.87 on 2018-01-26 to 2581.00 on 2018-02-08. The short-volatility
	// unwind of 5 February 2018 terminated the XIV note the following day.
	{
		Name:  "Volmageddon",
		Start: "2018-01-26", End: "2018-02-09",
		Description:   "the short-volatility unwind of 5 February 2018, which closed the XIV note outright",
		IndexDrawdown: -0.102,
	},
	// 2925.51 on 2018-10-03 to 2351.10 on 2018-12-24, the worst Christmas Eve
	// session the index has recorded.
	{
		Name:  "Q4 2018",
		Start: "2018-10-01", End: "2018-12-24",
		Description:   "the rate-driven quarter that ended in the worst Christmas Eve on record for the index",
		IndexDrawdown: -0.196,
	},
	// 3386.15 on 2020-02-19 to 2237.40 on 2020-03-23: the fastest fall of
	// that size in the index's history, over 23 sessions.
	{
		Name:  "COVID crash",
		Start: "2020-02-19", End: "2020-03-23",
		Description:   "the pandemic crash: a third of the index's value in 23 sessions",
		IndexDrawdown: -0.339,
	},
	// The one window here that is not a fall. The index roughly doubled from
	// the 2020-03-23 low to 4766.18 on 2021-12-31, and its worst interruption
	// was 3580.84 on 2020-09-02 to 3236.92 on 2020-09-23. It is in the table
	// because a defensive strategy that survives every crash by holding cash
	// has to be shown paying for it somewhere.
	{
		Name:  "Recovery and mania",
		Start: "2020-03-24", End: "2021-12-31",
		Description:   "the recovery and the speculative period after it — the window that charges for caution",
		IndexDrawdown: -0.096,
	},
	// 4796.56 on 2022-01-03, the record close, to 3577.03 on 2022-10-12.
	// Bonds fell alongside equities through this one, so a portfolio that
	// held them as a hedge was not hedged.
	{
		Name:  "Rate shock",
		Start: "2022-01-03", End: "2022-10-12",
		Description:   "the fastest tightening cycle since 1981, in which bonds fell with equities rather than against them",
		IndexDrawdown: -0.254,
	},
	// 3992.01 on 2023-03-08 to 3855.76 on 2023-03-13. Silicon Valley Bank
	// failed on 10 March 2023, Signature on the 12th and First Republic on
	// 1 May. The index barely registered it while regional bank equity
	// halved, which is the point of including it.
	{
		Name:  "Regional banks",
		Start: "2023-03-08", End: "2023-05-01",
		Description:   "the 2023 regional bank failures, which the index barely registered and bank shareholders did not survive",
		IndexDrawdown: -0.034,
	},
}

// Scenarios returns the table. The copy is deliberate: the table is shared
// across every run in a process and a caller that sorted or filtered it in
// place would change what every later run measures.
func Scenarios() []Scenario {
	return append([]Scenario(nil), scenarioTable...)
}

// ScenarioSpec describes a scenario replay.
type ScenarioSpec struct {
	// Base is the strategy to run. Its Start and End are not the windows —
	// those come from the table — but they do bound which scenarios are
	// considered, so a caller can ask for "the crises since 2000" without
	// having to name them.
	Base Spec
	// Scenarios overrides the table, for tests and for a caller that wants
	// one window. Empty means the whole table.
	Scenarios []Scenario
	// Workers caps parallel backtests. Zero means one per CPU.
	Workers int
}

// ScenarioRun is one scenario's outcome, or the reason there is not one.
type ScenarioRun struct {
	Scenario
	// Skipped marks a window that was not measured. It is reported rather
	// than dropped: silently omitting 1987 because the symbol did not exist
	// then would present partial coverage as full coverage, which is the
	// exact failure this command exists to prevent.
	Skipped bool `json:"skipped,omitempty"`
	// SkipReason names what was missing, with its dates.
	SkipReason string `json:"skip_reason,omitempty"`
	// outOfRange separates a window the caller asked to exclude from one the
	// data could not reach. Both are skips; only the second is a limit on
	// what the strategy has been shown against, and a finding that conflates
	// them tells the reader their own --from flag is a gap in the evidence.
	outOfRange bool
	// Error records a run that started and failed, which is a different fact
	// from one that was never eligible.
	Error string `json:"error,omitempty"`

	// LeadInFrom is the session the strategy actually started trading on,
	// which is earlier than the window. See scenarioLeadIn.
	LeadInFrom   market.Day `json:"lead_in_from,omitempty"`
	FirstSession market.Day `json:"first_session,omitempty"`
	LastSession  market.Day `json:"last_session,omitempty"`
	Sessions     int        `json:"sessions,omitempty"`

	// Every measurement below is a Ratio, and every one of them is null on a
	// row that was not measured. A skipped window carrying "return": 0 would
	// say the strategy was flat through the financial crisis, which is a
	// claim, not an absence — and it is the specific misreading this whole
	// command exists to prevent. The benchmark figures are null for the
	// second reason as well: a window its symbol does not reach has no number.
	Return            Ratio  `json:"return"`
	MaxDrawdown       Ratio  `json:"max_drawdown"`
	BenchmarkReturn   Ratio  `json:"benchmark_return"`
	BenchmarkDrawdown Ratio  `json:"benchmark_drawdown"`
	Excess            Ratio  `json:"excess"`
	Benchmark         string `json:"benchmark,omitempty"`

	// Exposure is average gross exposure over the window. Without it a flat
	// return through a crash is unreadable: sitting in cash and being long
	// something that happened not to move are the same number.
	Exposure Ratio `json:"exposure"`
	Fills    int   `json:"fills"`
	Trades   int   `json:"trades"`
	// Missing lists universe symbols with no data covering the window. A
	// two-asset portfolio measured over a window where one of the assets did
	// not yet exist is not that portfolio.
	Missing []string `json:"missing_symbols,omitempty"`
}

// newScenarioRun starts a row with every measurement undefined, so a row that
// never runs cannot report a number it does not have.
func newScenarioRun(sc Scenario) ScenarioRun {
	nan := Ratio(math.NaN())
	return ScenarioRun{
		Scenario: sc, Return: nan, MaxDrawdown: nan, Exposure: nan,
		BenchmarkReturn: nan, BenchmarkDrawdown: nan, Excess: nan,
	}
}

// Measured reports whether this row holds a result.
func (r ScenarioRun) Measured() bool { return !r.Skipped && r.Error == "" }

// ScenarioReport is the whole replay.
type ScenarioReport struct {
	Runs    []ScenarioRun `json:"runs"`
	Covered int           `json:"covered"`
	Skipped int           `json:"skipped"`

	// DataFrom and DataTo are what the strategy's own universe spans, which
	// is what decides the coverage above.
	DataFrom market.Day `json:"data_from,omitempty"`
	DataTo   market.Day `json:"data_to,omitempty"`
	// DataProvider is where the prices came from. It is on the report rather
	// than left implicit because these windows are named after real episodes:
	// the same table filled with synthetic prices looks exactly like history
	// and is not, and the reader has to be able to tell from the output.
	DataProvider string `json:"data_provider,omitempty"`
	// Warmup is the number of bars the strategy needs before it can trade.
	Warmup int `json:"warmup"`
	// LeadIn is how many sessions of live trading precede each window.
	LeadIn int `json:"lead_in"`

	Findings []Finding `json:"findings,omitempty"`
	Verdict  string    `json:"verdict"`
	Elapsed  int64     `json:"elapsed_ms"`
}

// scenarioLeadIn is how many sessions of ordinary trading run before a window
// opens, on top of the indicator warm-up.
//
// Warm-up alone is not enough, and getting this wrong is the subtle version of
// the artefact this whole command exists to avoid. Warm-up primes indicators
// but never calls the strategy, so a run that begins on the window's first day
// begins in cash with no state: a crossover strategy has no crossing to react
// to, a quarterly rebalancer has not rebalanced yet, and every short window
// reports a flat line that looks like a defensive result and is really an
// empty one. Trading a year in front of the window instead means the strategy
// enters it holding whatever it would actually have been holding, which is the
// only version of "what did this do in March 2020" worth reporting.
//
// A year is the shortest lead-in that covers an annual rebalance. Nothing
// inside it is measured, and nothing in it can see past the window's start, so
// it adds no lookahead — only position.
const scenarioLeadIn = 252

// RunScenarios runs the strategy through every window the data can cover.
func RunScenarios(ctx context.Context, ss ScenarioSpec, store *market.Store, progress func(done, total int, name string)) (*ScenarioReport, error) {
	started := time.Now()
	ss.Base.ApplyDefaults()
	// The per-session audit trail is the most useful thing a single run
	// produces and the fastest way to exhaust memory across thirteen of them.
	ss.Base.OmitDayRecords = true

	list := ss.Scenarios
	if len(list) == 0 {
		list = Scenarios()
	}

	sessions, firstBar, warmup, err := scenarioCoverage(ctx, ss.Base, store)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, fmt.Errorf("no market data for the strategy's universe, so no scenario can be run")
	}

	leadIn := scenarioLeadIn
	if warmup > leadIn {
		// A strategy needing more history than that has said so itself, and
		// its own figure is the better one.
		leadIn = warmup
	}
	rep := &ScenarioReport{
		Runs:         make([]ScenarioRun, len(list)),
		DataFrom:     sessions[0],
		DataTo:       sessions[len(sessions)-1],
		DataProvider: store.ProviderName(),
		Warmup:       warmup,
		LeadIn:       leadIn,
	}

	// Eligibility is decided before anything runs, so the expensive part is
	// only paid for windows that can produce a number.
	var eligible []int
	for i, sc := range list {
		run := newScenarioRun(sc)
		reason, from, asked := scenarioWindow(sc, ss.Base, sessions, warmup, leadIn)
		if reason != "" {
			run.Skipped = true
			run.SkipReason = reason
			run.outOfRange = asked
			rep.Runs[i] = run
			rep.Skipped++
			continue
		}
		run.LeadInFrom = from
		run.Missing = missingSymbols(sc, firstBar)
		rep.Runs[i] = run
		eligible = append(eligible, i)
	}

	workers := ss.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0

	for _, idx := range eligible {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			runScenario(ctx, &rep.Runs[i], ss.Base, store)

			mu.Lock()
			done++
			if progress != nil {
				progress(done, len(eligible), rep.Runs[i].Name)
			}
			mu.Unlock()
		}(idx)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for _, r := range rep.Runs {
		if r.Measured() {
			rep.Covered++
		}
	}
	rep.Findings = scenarioFindings(rep)
	rep.Verdict = scenarioVerdict(rep)
	rep.Elapsed = time.Since(started).Milliseconds()
	return rep, nil
}

// scenarioCoverage loads the strategy's data once and reports every session
// available, each symbol's first bar, and the warm-up the strategy asks for.
//
// setup() runs first because it is allowed to name the universe and raise the
// warm-up: a coverage check made before it would be checking a different
// strategy from the one that is about to be run.
func scenarioCoverage(ctx context.Context, base Spec, store *market.Store) ([]market.Day, map[string]market.Day, int, error) {
	probe := base
	// An empty Start means "as far back as the data goes", which is exactly
	// the question being asked here.
	probe.Start = ""
	if probe.End == "" {
		probe.End = market.NewDay(time.Now())
	}

	e := New(probe, store)
	if err := e.resolveSetup(ctx); err != nil {
		return nil, nil, 0, err
	}
	if err := e.loadData(ctx); err != nil {
		return nil, nil, 0, err
	}
	first := make(map[string]market.Day, len(e.series))
	for sym, s := range e.series {
		if s == nil || len(s.Bars) == 0 {
			continue
		}
		first[sym] = s.Bars[0].Date
	}
	return e.days, first, e.spec.Warmup, nil
}

// scenarioWindow decides whether a window can be measured, and from which
// session the run in front of it has to start.
//
// It returns a reason and an empty date when it cannot. Every branch is a case
// where running anyway would produce a number that looks like a result: a
// partial window reported as a whole episode, or a window entered with no
// position because there was no room in front of it to take one.
func scenarioWindow(sc Scenario, base Spec, sessions []market.Day, warmup, leadIn int) (reason string, from market.Day, asked bool) {
	if base.Start != "" && sc.Start < base.Start {
		return fmt.Sprintf("before the requested start date, %s", base.Start), "", true
	}
	if base.End != "" && sc.End > base.End {
		return fmt.Sprintf("after the requested end date, %s", base.End), "", true
	}
	if sessions[0] > sc.Start {
		return fmt.Sprintf("the data begins on %s, after this window opens", sessions[0]), "", false
	}
	if last := sessions[len(sessions)-1]; last < sc.End {
		return fmt.Sprintf("the data ends on %s, before this window closes", last), "", false
	}
	// Sessions strictly before the window are all the room there is for the
	// warm-up and the lead-in together.
	before := sort.Search(len(sessions), func(i int) bool { return sessions[i] >= sc.Start })
	if need := warmup + leadIn; before < need {
		return fmt.Sprintf("only %d sessions of history before it, and the strategy needs %d "+
			"to warm up and reach the window already positioned", before, need), "", false
	}
	return "", sessions[before-leadIn], false
}

// missingSymbols lists universe members with no history at the window's start.
func missingSymbols(sc Scenario, firstBar map[string]market.Day) []string {
	var out []string
	for sym, first := range firstBar {
		if first > sc.Start {
			out = append(out, sym)
		}
	}
	sort.Strings(out)
	return out
}

// runScenario measures one window.
//
// The spec starts at the lead-in, not at the window, so the engine loads the
// warm-up in front of that and the strategy trades its way up to the window's
// first session. Only the segment from that session is measured, and the
// strategy and the benchmark are both measured over it the same way, so the
// excess is a like-for-like figure.
func runScenario(ctx context.Context, run *ScenarioRun, base Spec, store *market.Store) {
	spec := base
	spec.Start, spec.End = run.LeadInFrom, run.End

	res, err := New(spec, store).Run(ctx)
	if err != nil {
		run.Error = truncateErr(err.Error())
		return
	}
	seg, base0, ok := segmentFrom(res.Curve, run.Start)
	if !ok {
		run.Error = "the window holds no sessions once the data is loaded"
		return
	}
	sc := spec.Scale()
	ps := periodStats(run.Name, seg, base0, sc)

	run.Return = Ratio(ps.Return)
	run.MaxDrawdown = Ratio(ps.MaxDrawdown)
	run.Exposure = Ratio(ps.Exposure)
	run.Sessions = ps.TradingDays
	run.FirstSession, run.LastSession = ps.Start, ps.End
	run.Trades = countTradesIn(res.Trades, ps.Start, ps.End)
	for _, f := range res.Fills {
		if f.Date >= ps.Start && f.Date <= ps.End {
			run.Fills++
		}
	}

	if len(res.Benchmarks) > 0 {
		b := res.Benchmarks[0]
		if bseg, bbase, bok := segmentFrom(b.Curve, run.Start); bok {
			bps := periodStats(b.Symbol, bseg, bbase, sc)
			run.Benchmark = b.Symbol
			run.BenchmarkReturn = Ratio(bps.Return)
			run.BenchmarkDrawdown = Ratio(bps.MaxDrawdown)
			run.Excess = Ratio(ps.Return - bps.Return)
		}
	}
}

// segmentFrom cuts a curve at a date, returning the segment and the value it
// is measured from.
//
// The base is the window's own opening close, not the session before it, which
// is where this differs from the calendar attribution. A year boundary is
// arbitrary and dropping its first day would lose that day from every period;
// a scenario boundary is not arbitrary — the window opens on the closing price
// the episode is dated from. Measuring from the previous close would add the
// last day of the rally to the crash and put the benchmark column permanently
// half a point away from the published figure beside it.
func segmentFrom(curve []EquityPoint, from market.Day) ([]EquityPoint, float64, bool) {
	i := sort.Search(len(curve), func(i int) bool { return curve[i].Date >= from })
	if i >= len(curve) {
		return nil, 0, false
	}
	return curve[i:], curve[i].Value, true
}

// scenarioUnderperformance is how far behind the benchmark a strategy has to
// finish, in a window where the benchmark itself lost money, before it is
// worth saying so. Five points is wide enough that fees and a day's entry lag
// cannot produce it.
const scenarioUnderperformance = -0.05

// scenarioFindings states what the replay found, in the terms the rest of the
// critique uses.
func scenarioFindings(rep *ScenarioReport) []Finding {
	var out []Finding
	add := func(sev Severity, title, format string, args ...any) {
		out = append(out, Finding{sev, title, fmt.Sprintf(format, args...)})
	}

	// Synthetic prices outrank every other finding here. Elsewhere in the tool
	// they cost the run its realism; in this command they make the output a
	// forgery, because the row labelled "COVID crash" is then a generated
	// series that never crashed.
	if strings.Contains(rep.DataProvider, "synthetic") {
		add(SeverityCritical, "these are not the real crises",
			"The prices came from %q. These windows are named after real episodes and "+
				"dated from real closes, but the series they were measured on is "+
				"generated, so every number here is fiction. Run without --offline.",
			rep.DataProvider)
	}

	// The headline case: it lost materially more than the benchmark in a
	// window where the benchmark was already falling. Only the worst one is
	// named, with a count for the rest — thirteen near-identical findings
	// would bury the one that matters.
	var worst *ScenarioRun
	var behind, falling int
	for i := range rep.Runs {
		r := &rep.Runs[i]
		if !r.Measured() || !r.Excess.Defined() || float64(r.BenchmarkReturn) >= 0 {
			continue
		}
		falling++
		if float64(r.Excess) > scenarioUnderperformance || r.Return >= 0 {
			continue
		}
		behind++
		if worst == nil || r.Excess < worst.Excess {
			worst = r
		}
	}
	if worst != nil {
		sev := SeverityWarning
		if float64(worst.Excess) < -0.15 {
			sev = SeverityCritical
		}
		rest := ""
		if behind > 1 {
			rest = fmt.Sprintf(" It was materially behind the benchmark in %d of the %d "+
				"falling windows the data covers.", behind, falling)
		}
		add(sev, "it lost more than the benchmark in a named crisis",
			"%s (%s to %s): the strategy returned %.1f%% against the benchmark's %.1f%%, "+
				"%.1f points worse, with a %.1f%% drawdown.%s",
			worst.Name, worst.Start, worst.End, worst.Return*100,
			float64(worst.BenchmarkReturn)*100, -float64(worst.Excess)*100,
			worst.MaxDrawdown*100, rest)
	}

	// A large absolute loss, when it is not already the finding above. The
	// strategy can be ahead of a benchmark that fell further and still have
	// lost more than anybody would sit through.
	var deepest *ScenarioRun
	for i := range rep.Runs {
		r := &rep.Runs[i]
		if !r.Measured() || r.Return > -0.20 {
			continue
		}
		if worst != nil && r.Name == worst.Name {
			continue
		}
		if deepest == nil || r.Return < deepest.Return {
			deepest = r
		}
	}
	if deepest != nil {
		add(SeverityWarning, "a crisis it did not survive well",
			"%s: %.1f%%, with a %.1f%% drawdown inside the window. Whatever the "+
				"full-period figures say, this is what holding it through that episode "+
				"would have felt like.",
			deepest.Name, deepest.Return*100, deepest.MaxDrawdown*100)
	}

	// Coverage. A strategy measured over three crises is not a strategy that
	// has seen crises, and the reader cannot tell without being told. Windows
	// the caller excluded are not counted here: their own date range is not
	// evidence about the strategy.
	var unreachable int
	for _, r := range rep.Runs {
		if r.Skipped && !r.outOfRange {
			unreachable++
		}
	}
	if unreachable > 0 {
		add(SeverityNote, "most of history is outside this test",
			"%d of the %d named windows could not be measured against this strategy's data, "+
				"which runs from %s to %s. The record it has been shown is shorter than it looks.",
			unreachable, len(rep.Runs), rep.DataFrom, rep.DataTo)
	}

	// A strategy that was flat through the crashes did not survive them; it
	// was absent for them, which is a different claim.
	var idle []string
	for _, r := range rep.Runs {
		if r.Measured() && r.Fills == 0 && r.Exposure < 0.01 {
			idle = append(idle, r.Name)
		}
	}
	if len(idle) > 0 && len(idle) == rep.Covered {
		add(SeverityNote, "it held nothing through any of them",
			"The strategy was flat in all %d measured windows. Those rows describe an "+
				"absence rather than behaviour under stress.", rep.Covered)
	} else if len(idle) > 0 {
		add(SeverityNote, "flat through some of the crises",
			"It held nothing at all in %d of %d measured windows (%s), so those rows say "+
				"nothing about how it behaves under stress.",
			len(idle), rep.Covered, joinNames(idle, 3))
	}

	// A portfolio missing one of its assets is not that portfolio.
	var incomplete []string
	for _, r := range rep.Runs {
		if r.Measured() && len(r.Missing) > 0 {
			incomplete = append(incomplete, r.Name)
		}
	}
	if len(incomplete) > 0 {
		add(SeverityWarning, "the portfolio was incomplete in some windows",
			"%s ran without at least one symbol the strategy trades, because it had no "+
				"data that far back (%s). Those rows measure a different portfolio from "+
				"the one described.", plural(len(incomplete), "window"), joinNames(incomplete, 3))
	}

	sort.SliceStable(out, func(i, j int) bool {
		return severityRank(out[i].Severity) < severityRank(out[j].Severity)
	})
	return out
}

// scenarioVerdict is the one sentence to read if you read nothing else.
func scenarioVerdict(rep *ScenarioReport) string {
	if rep.Covered == 0 {
		for _, r := range rep.Runs {
			if r.Skipped && !r.outOfRange {
				return fmt.Sprintf("none of the %d named windows are inside the available data, "+
					"which runs from %s to %s, so this says nothing about behaviour in a crisis",
					len(rep.Runs), rep.DataFrom, rep.DataTo)
			}
		}
		return fmt.Sprintf("all %d named windows were excluded by the requested date range, "+
			"so nothing was measured", len(rep.Runs))
	}

	// worst is the deepest loss of all; worstRelative is the widest gap to a
	// falling benchmark, which is a different window and the only one whose
	// benchmark figure is guaranteed to exist.
	var worst, worstRelative *ScenarioRun
	var beat, compared int
	for i := range rep.Runs {
		r := &rep.Runs[i]
		if !r.Measured() {
			continue
		}
		if worst == nil || r.Return < worst.Return {
			worst = r
		}
		if !r.Excess.Defined() || float64(r.BenchmarkReturn) >= 0 {
			continue
		}
		compared++
		if float64(r.Excess) > 0 {
			beat++
		}
		if worstRelative == nil || r.Excess < worstRelative.Excess {
			worstRelative = r
		}
	}
	if worst == nil {
		return ""
	}

	switch {
	case compared == 0:
		return fmt.Sprintf("across %d measured windows the worst was %s at %.1f%%; none of them "+
			"had a falling benchmark to compare against",
			rep.Covered, worst.Name, worst.Return*100)
	case beat == compared:
		return fmt.Sprintf("it lost less than the benchmark in all %d falling windows the data "+
			"covers; its own worst was %s at %.1f%%", compared, worst.Name, worst.Return*100)
	case beat == 0:
		return fmt.Sprintf("it lost more than the benchmark in every one of the %d falling "+
			"windows the data covers, worst of them %s at %.1f%% against %.1f%%",
			compared, worstRelative.Name, worstRelative.Return*100,
			float64(worstRelative.BenchmarkReturn)*100)
	default:
		return fmt.Sprintf("it held up better than the benchmark in %d of the %d falling windows "+
			"the data covers; its own worst was %s at %.1f%%",
			beat, compared, worst.Name, worst.Return*100)
	}
}

// joinNames renders a short list, naming at most n before counting the rest.
func joinNames(names []string, n int) string {
	if len(names) <= n {
		out := ""
		for i, s := range names {
			switch {
			case i == 0:
				out = s
			case i == len(names)-1:
				out += " and " + s
			default:
				out += ", " + s
			}
		}
		return out
	}
	out := ""
	for _, s := range names[:n] {
		if out != "" {
			out += ", "
		}
		out += s
	}
	return fmt.Sprintf("%s and %d more", out, len(names)-n)
}
