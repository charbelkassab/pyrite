package forward

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
)

// minToSpeak is the point below which the aggregate is not worth reading out
// at all.
//
// It is not a threshold for belief. Thirty daily decisions is six weeks and
// establishes nothing. It is the point below which the standard error of a
// hit rate is wider than the whole range of answers anyone would argue about,
// so quoting the hit rate at all is misleading.
const minToSpeak = 30

// enoughToArgue is about a year of daily decisions, which is the point where
// the numbers stop being decoration and start being an argument. It is still
// one year of one market.
const enoughToArgue = 250

// Prices is the slice of the market data store the scorer needs.
//
// It is an interface rather than *market.Store so the tests can score a known
// series with no network and no keys, which is the only way a test of this
// can be deterministic.
type Prices interface {
	Bars(ctx context.Context, symbol string, from, to market.Day) ([]market.Bar, error)
}

// SymbolReturn is what one holding did over the recorded window.
type SymbolReturn struct {
	Symbol string       `json:"symbol"`
	Weight float64      `json:"weight"`
	Entry  float64      `json:"entry_price"`
	Exit   float64      `json:"exit_price"`
	Return engine.Ratio `json:"return"`
}

// Scored is one recorded decision measured against what actually happened.
type Scored struct {
	At       time.Time  `json:"at"`
	Strategy string     `json:"strategy"`
	AsOf     market.Day `json:"as_of"`
	// Entered and Exited are the sessions the decision was held across:
	// bought at Entered's open, valued at Exited's close.
	Entered market.Day     `json:"entered,omitempty"`
	Exited  market.Day     `json:"exited,omitempty"`
	Symbols []SymbolReturn `json:"symbols,omitempty"`
	Return  engine.Ratio   `json:"return"`
	// Flat marks a decision to hold nothing. It is a real decision and its
	// realised return really is zero, so it counts.
	Flat bool `json:"flat,omitempty"`
	// Backfilled marks a record written when its outcome already existed.
	// Those are excluded from the forward aggregate; see Score.
	Backfilled bool `json:"backfilled,omitempty"`
	// Pending explains why there is no outcome yet. A pending decision is
	// left out of every aggregate: counting it as a zero return would be
	// inventing the outcome it is still waiting for.
	Pending string `json:"pending,omitempty"`
	// Problem is set when the outcome cannot be measured at all, which is a
	// different thing from not having arrived and also not a zero.
	Problem string `json:"problem,omitempty"`
}

// Aggregate is what a set of realised returns adds up to.
type Aggregate struct {
	Count int `json:"count"`
	Hits  int `json:"hits"`
	// Every ratio here is an engine.Ratio because every one of them is
	// undefined at a sample size this feature will spend its first months at.
	HitRate    engine.Ratio `json:"hit_rate"`
	MeanReturn engine.Ratio `json:"mean_return"`
	StdDev     engine.Ratio `json:"std_dev"`
	// TStat tests the mean against zero. It needs at least two observations
	// and some spread, so it is undefined until then.
	TStat engine.Ratio `json:"t_stat"`
	// DecisionsForT2 is how many decisions like these it would take in total
	// for the mean to reach |t| = 2, at the mean and spread seen so far. It
	// is usually the most informative number in the whole report.
	DecisionsForT2 int `json:"decisions_for_t2,omitempty"`
}

// Scorecard is every recorded decision measured, and what little the
// aggregate is entitled to say.
type Scorecard struct {
	Entries []Scored `json:"entries"`
	// Forward covers only the records written before their outcome existed,
	// which is the only set that is evidence of anything.
	Forward Aggregate `json:"forward"`
	// Backfill covers records made with --as-of over a period that had
	// already happened. They are reported because they show the machinery
	// works, and kept apart because they are not out-of-sample: whoever ran
	// them could see the answer first.
	Backfill Aggregate `json:"backfill"`
	Pending  int       `json:"pending"`
	Flat     int       `json:"flat"`
	// Unscorable counts decisions whose symbols have no usable prices.
	Unscorable int    `json:"unscorable"`
	Verdict    string `json:"verdict"`
}

// Score measures every recorded decision against what happened next.
//
// Each decision is bought at the open of the first session after its as-of
// date and valued at the close of the session Horizon sessions later, which
// is the same next-open convention the engine's default fill model uses. Any
// other choice would score the strategy on a price it could not have traded.
func Score(ctx context.Context, entries []Entry, px Prices) (Scorecard, error) {
	return scoreAsOf(ctx, entries, px, market.NewDay(time.Now()))
}

// scoreAsOf is Score with the present day handed in, so a test is not at the
// mercy of the calendar it runs on.
func scoreAsOf(ctx context.Context, entries []Entry, px Prices, today market.Day) (Scorecard, error) {
	s := Scorecard{Forward: emptyAggregate(), Backfill: emptyAggregate()}
	bars := &barCache{px: px, today: today}

	var forward, backfill []float64
	for _, e := range entries {
		sc, err := scoreOne(ctx, e, bars)
		if err != nil {
			return s, err
		}
		s.Entries = append(s.Entries, sc)
		switch {
		case sc.Pending != "":
			s.Pending++
			continue
		case sc.Problem != "":
			s.Unscorable++
			continue
		}
		if sc.Flat {
			s.Flat++
		}
		if sc.Backfilled {
			backfill = append(backfill, float64(sc.Return))
		} else {
			forward = append(forward, float64(sc.Return))
		}
	}
	s.Forward = aggregate(forward)
	s.Backfill = aggregate(backfill)
	s.Verdict = verdictFor(s)
	return s, nil
}

func scoreOne(ctx context.Context, e Entry, bars *barCache) (Scored, error) {
	sc := Scored{
		At: e.At, Strategy: e.Strategy, AsOf: e.AsOf,
		Flat: len(e.Positions) == 0, Return: engine.Ratio(math.NaN()),
	}
	horizon := e.Horizon
	if horizon < 1 {
		horizon = 1
	}
	// The window is generous in calendar days because the horizon is counted
	// in sessions, and a fortnight of them can span a month of holidays.
	from, to := e.AsOf.Add(1), e.AsOf.Add(2*horizon+30)

	ref := e.Reference
	if ref == "" && len(e.Positions) > 0 {
		ref = e.Positions[0].Symbol
	}
	if ref == "" {
		sc.Problem = "no reference symbol was recorded, so there is no calendar to say whether the holding period has passed"
		return sc, nil
	}
	refBars, err := bars.get(ctx, ref, from, to)
	if err != nil {
		return sc, err
	}
	if len(refBars) < horizon {
		sc.Pending = fmt.Sprintf("only %d of the %d sessions after %s have happened",
			len(refBars), horizon, e.AsOf)
		return sc, nil
	}
	sc.Entered, sc.Exited = refBars[0].Date, refBars[horizon-1].Date

	// A record written on or after the session it would have traded in was
	// written when the entry price already existed. That is a backfill, and
	// it cannot be counted as a decision made in advance.
	sc.Backfilled = market.NewDay(e.At) >= sc.Entered

	var total float64
	for _, p := range e.Positions {
		symBars, err := bars.get(ctx, p.Symbol, from, to)
		if err != nil {
			return sc, err
		}
		entry, exit, ok := window(symBars, sc.Entered, sc.Exited)
		if !ok {
			sc.Problem = fmt.Sprintf("%s has no prices between %s and %s, so this decision cannot be measured",
				p.Symbol, sc.Entered, sc.Exited)
			return sc, nil
		}
		r := exit/entry - 1
		total += p.Weight * r
		sc.Symbols = append(sc.Symbols, SymbolReturn{
			Symbol: p.Symbol, Weight: p.Weight,
			Entry: entry, Exit: exit, Return: engine.Ratio(r),
		})
	}
	// Whatever the book did not hold sat in cash and earned nothing over a
	// session or two, which is close enough to true not to be worth
	// pretending otherwise about.
	sc.Return = engine.Ratio(total)
	return sc, nil
}

// window picks the price the decision was bought at and the price it is
// valued at.
//
// Both are adjusted: the open is scaled by the bar's split factor and the
// exit is the adjusted close, so a split inside the holding period does not
// manufacture a return that nobody earned.
func window(bars []market.Bar, entered, exited market.Day) (entry, exit float64, ok bool) {
	for _, b := range bars {
		if b.Date == entered && b.Open > 0 {
			entry = b.Open * b.SplitFactor()
			break
		}
	}
	for _, b := range bars {
		if b.Date > exited {
			break
		}
		if b.Date >= entered && b.AdjClose > 0 {
			exit = b.AdjClose
		}
	}
	if entry <= 0 || exit <= 0 {
		return 0, 0, false
	}
	return entry, exit, true
}

// barCache stops a hundred decisions on one symbol from becoming a hundred
// fetches of the same window.
type barCache struct {
	px    Prices
	today market.Day
	hit   map[string][]market.Bar
}

func (c *barCache) get(ctx context.Context, symbol string, from, to market.Day) ([]market.Bar, error) {
	key := symbol + "\x00" + string(from) + "\x00" + string(to)
	if b, ok := c.hit[key]; ok {
		return b, nil
	}
	bars, err := c.px.Bars(ctx, symbol, from, to)
	if err != nil {
		return nil, fmt.Errorf("fetch %s from %s to %s to score a recorded decision: %w", symbol, from, to, err)
	}
	// The window is trimmed rather than trusted, at both ends.
	//
	// A bar at or before the as-of date would let a decision be scored on a
	// price it was made from. A bar dated after today is not an outcome at
	// all: a real provider returns none, but the synthetic provider behind
	// --offline generates them on demand, and scoring a recorded decision
	// against a price that has not been printed yet is precisely the failure
	// this package exists to prevent.
	kept := bars[:0]
	for _, b := range bars {
		if b.Date >= from && (c.today == "" || b.Date.Date() <= c.today) {
			kept = append(kept, b)
		}
	}
	if c.hit == nil {
		c.hit = map[string][]market.Bar{}
	}
	c.hit[key] = kept
	return kept, nil
}

func emptyAggregate() Aggregate {
	nan := engine.Ratio(math.NaN())
	return Aggregate{HitRate: nan, MeanReturn: nan, StdDev: nan, TStat: nan}
}

func aggregate(rs []float64) Aggregate {
	a := emptyAggregate()
	a.Count = len(rs)
	if len(rs) == 0 {
		return a
	}
	var sum float64
	for _, r := range rs {
		sum += r
		if r > 0 {
			a.Hits++
		}
	}
	n := float64(len(rs))
	mean := sum / n
	a.MeanReturn = engine.Ratio(mean)
	a.HitRate = engine.Ratio(float64(a.Hits) / n)
	if len(rs) < 2 {
		return a
	}
	var ss float64
	for _, r := range rs {
		ss += (r - mean) * (r - mean)
	}
	sd := math.Sqrt(ss / (n - 1))
	a.StdDev = engine.Ratio(sd)
	if sd <= 0 {
		return a
	}
	a.TStat = engine.Ratio(mean / (sd / math.Sqrt(n)))
	if mean != 0 {
		if need := math.Ceil(4 * sd * sd / (mean * mean)); need >= 1 && need < 1e9 {
			a.DecisionsForT2 = int(need)
		}
	}
	return a
}

// verdictFor says in words what the numbers are entitled to claim, which for
// a long time will be nothing.
func verdictFor(s Scorecard) string {
	var b strings.Builder
	fw := s.Forward

	switch {
	case fw.Count == 0 && s.Pending == 0 && s.Backfill.Count == 0 && s.Unscorable == 0:
		return "Nothing recorded yet. `pyrite forward record` writes down what the strategy " +
			"wants to hold next session, and this scores those records once the session has happened."
	case fw.Count == 0 && s.Pending == 1:
		b.WriteString("One recorded decision, and it is not old enough to score: the session it " +
			"refers to has not finished yet. That is the normal state of this command for its " +
			"first few days, and there is nothing to do but wait.")
	case fw.Count == 0 && s.Pending > 0:
		b.WriteString(fmt.Sprintf("%d recorded decisions, none of them old enough to score: the "+
			"sessions they refer to have not finished yet. That is the normal state of this "+
			"command for its first few days, and there is nothing to do but wait.", s.Pending))
	case fw.Count == 0:
		b.WriteString("Nothing here was recorded before its outcome existed, so there is no " +
			"forward evidence at all — only a demonstration that the machinery runs.")
	default:
		n := fw.Count
		b.WriteString(fmt.Sprintf("%d scored %s.", n, plural(n, "decision")))
		lo, hi := coinRange(n)
		switch {
		case n < 5:
			b.WriteString(" That is not a sample, it is an anecdote: over this many, every hit " +
				"rate from nothing to everything is ordinary.")
		case n < minToSpeak:
			b.WriteString(fmt.Sprintf(" That is far too few to mean anything. A coin flipped %d "+
				"times lands between %.0f%% and %.0f%% heads in 95 runs out of 100, which covers "+
				"most of the hit rates a strategy could show, so the figures above are entirely "+
				"consistent with no skill whatsoever. The number worth watching for now is how "+
				"many decisions there are, not what they say.", n, lo*100, hi*100))
		case n < enoughToArgue:
			b.WriteString(fmt.Sprintf(" Enough to look at and not enough to conclude from: chance "+
				"alone puts a hit rate anywhere between %.0f%% and %.0f%% over this many, and one "+
				"unusual session still moves the mean.", lo*100, hi*100))
		default:
			b.WriteString(fmt.Sprintf(" That is roughly %s of sessions, which is the point where "+
				"these figures start being an argument rather than decoration. It is still one "+
				"strategy over one stretch of one market.", approxSessions(n)))
		}
		b.WriteString(tStatSentence(fw))
	}

	if s.Backfill.Count > 0 {
		b.WriteString(fmt.Sprintf(" %d further %s recorded after the outcome already existed "+
			"(an --as-of backfill) and %s excluded from everything above: a decision written down "+
			"once the answer was available is a test of this machinery, not evidence about the "+
			"strategy.", s.Backfill.Count, plural(s.Backfill.Count, "decision was"),
			plural(s.Backfill.Count, "is")))
	}
	if s.Unscorable > 0 {
		b.WriteString(fmt.Sprintf(" %d could not be measured because a symbol they held has no "+
			"prices over the holding period; they are left out rather than counted as zero.",
			s.Unscorable))
	}
	if s.Flat > 0 {
		b.WriteString(fmt.Sprintf(" %d of the scored decisions held nothing at all and count as "+
			"a zero return, which is what they earned.", s.Flat))
	}
	return b.String()
}

func tStatSentence(a Aggregate) string {
	if !a.TStat.Defined() {
		return " There is not enough spread across the observations to compute a t-statistic yet."
	}
	t := float64(a.TStat)
	switch {
	case math.Abs(t) < 2 && a.DecisionsForT2 > 0 && float64(a.MeanReturn) > 0:
		return fmt.Sprintf(" The mean return's t-statistic is %.2f, short of the conventional bar "+
			"of two. At this mean and this spread it would take about %d decisions in total — "+
			"roughly %s of sessions — to reach it, and that is the honest horizon for this test.",
			t, a.DecisionsForT2, approxSessions(a.DecisionsForT2))
	case math.Abs(t) < 2:
		return fmt.Sprintf(" The mean return's t-statistic is %.2f, short of the conventional bar "+
			"of two, so the mean is not distinguishable from zero.", t)
	case a.Count < enoughToArgue:
		return fmt.Sprintf(" The mean return's t-statistic is %.2f, which clears the conventional "+
			"bar of two — but over %d observations that statistic leans on returns being roughly "+
			"normal and independent, and daily portfolio returns are neither. At this sample size "+
			"the assumption is doing most of the work.", t, a.Count)
	default:
		return fmt.Sprintf(" The mean return's t-statistic is %.2f over %d decisions, all of them "+
			"written down before their outcomes existed. That is the strongest claim this tool can "+
			"make about a strategy. It is still one stretch of market, and it says nothing about "+
			"the next one.", t, a.Count)
	}
}

// coinRange is the interval a fair coin's hit rate falls in 95 times out of a
// hundred over n flips, which is the range a result has to escape before it
// is worth discussing.
func coinRange(n int) (lo, hi float64) {
	se := 0.5 / math.Sqrt(float64(n))
	lo, hi = 0.5-2*se, 0.5+2*se
	return math.Max(lo, 0), math.Min(hi, 1)
}

// approxSessions renders a number of trading sessions as elapsed time,
// because "1008 decisions" means less to a reader than "four years".
func approxSessions(n int) string {
	years := float64(n) / engine.TradingDaysPerYear
	switch {
	case years >= 1.5:
		return fmt.Sprintf("%.0f years", years)
	case years >= 0.9:
		return "a year"
	case years >= 0.12:
		return fmt.Sprintf("%d months", int(math.Round(years*12)))
	default:
		return fmt.Sprintf("%d weeks", max(1, int(math.Round(years*52))))
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	switch noun {
	case "decision was":
		return "decisions were"
	case "is":
		return "are"
	}
	return noun + "s"
}
