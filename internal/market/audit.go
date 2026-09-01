package market

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Severity ranks how much a data defect should change what a reader does.
//
// This mirrors the engine's severity scale deliberately, and does not share
// it: the engine imports this package, so the type cannot live there without
// a cycle. The engine maps these onto its own when a defect reaches a
// backtest critique.
type Severity string

const (
	// SeverityCritical means a backtest on this series is measuring the data
	// rather than the strategy.
	SeverityCritical Severity = "critical"
	// SeverityWarning means a real defect that a result has to be read
	// against.
	SeverityWarning Severity = "warning"
	// SeverityNote is context worth having before trusting the numbers.
	SeverityNote Severity = "note"
)

// Kinds of finding, stable enough for a pipeline to switch on.
const (
	KindSplit      = "unadjusted_split"
	KindStale      = "stale_price"
	KindGap        = "calendar_gap"
	KindClosedDay  = "bar_on_closed_day"
	KindOHLC       = "ohlc_violation"
	KindZeroVolume = "zero_volume"
	KindExtreme    = "extreme_return"
	KindDuplicate  = "duplicate_date"
	KindContinuous = "continuous_market"
)

// Finding is one specific, evidenced defect in a price series.
//
// There is not a single float on this type, and that is on purpose. Every
// number a finding needs is a count or a date, and the magnitudes live inside
// Detail where a reader will actually see them. A bare NaN reaching
// encoding/json truncates the whole response, and a report about broken data
// that breaks its own output would be a poor joke.
type Finding struct {
	Severity Severity `json:"severity"`
	// Kind is the machine-readable check that fired.
	Kind string `json:"kind"`
	// Symbol lets a finding stand alone once findings from several series
	// have been collected together.
	Symbol string `json:"symbol,omitempty"`
	// Title is the claim, in a few words.
	Title string `json:"title"`
	// Detail states the evidence with the numbers in it. A finding without
	// its numbers is an opinion.
	Detail string `json:"detail"`
	// Dates are the specific bars involved, truncated to a readable number.
	Dates []Day `json:"dates,omitempty"`
	// Count is how many bars the check matched in total, which is often more
	// than Dates lists.
	Count int `json:"count,omitempty"`
}

// Report is the audit of one symbol's series.
type Report struct {
	Symbol string `json:"symbol"`
	Bars   int    `json:"bars"`
	First  Day    `json:"first,omitempty"`
	Last   Day    `json:"last,omitempty"`
	// Sessions is how many trading days the calendar says lie between the
	// first and last bar. It is the denominator for anything missing.
	Sessions int       `json:"sessions,omitempty"`
	Findings []Finding `json:"findings"`
	// Verdict is the single most important thing to say about this series.
	Verdict string `json:"verdict"`
}

// Count returns how many findings carry a severity.
func (r Report) Count(sev Severity) int {
	var n int
	for _, f := range r.Findings {
		if f.Severity == sev {
			n++
		}
	}
	return n
}

// HasCritical reports whether anything found disqualifies the series.
func (r Report) HasCritical() bool { return r.Count(SeverityCritical) > 0 }

// Thresholds. Each is set where a defect is a likelier explanation than the
// market, because the failure mode that matters here is the false positive:
// an auditor that fires on ordinary data gets ignored, and an ignored
// auditor is worse than no auditor at all.
const (
	// staleRun is the number of identical consecutive closes that stops
	// being a quiet day and starts being a frozen feed.
	staleRun = 5
	// staleFrozen is a run nothing liquid produces by accident.
	staleFrozen = 20
	// splitTolerance is how far a one-day move may sit from a split ratio
	// and still be called one, as a fraction of the ratio. Loose enough for
	// rounded vendor prices, tight enough that an ordinary bad day is not a
	// 3:2 split.
	splitTolerance = 0.02
	// splitMaxRange caps the day's own high-to-low range. A share that
	// halved because it split traded all day at the new price; one that
	// halved on news traded the whole way down.
	splitMaxRange = 0.15
	// splitLookahead is how far forward to look for the move back that
	// would make this a bad print rather than a split.
	splitLookahead = 5
	// extremeMove is the daily return past which a move is worth naming.
	// Large caps cleared 12% in March 2020 and nothing here should mention
	// it; 20% is rare enough to be worth a look and common enough to be
	// real news more often than not.
	extremeMove = 0.20
	// continuousWeekendShare is the fraction of weekend bars above which
	// this is not an exchange-traded symbol at all, and the whole US market
	// calendar stops applying to it.
	continuousWeekendShare = 0.10
	// maxListedDates caps how many dates a single finding names.
	maxListedDates = 8
)

// splitRatios are the ratios worth naming, as the factor the price is
// multiplied by. Reverses are included because they fail the same way and are
// easier to miss: a 10x one-day gain looks like a triumph, not a defect.
var splitRatios = []struct {
	Label  string
	Factor float64
}{
	{"2:1", 1.0 / 2}, {"3:1", 1.0 / 3}, {"3:2", 2.0 / 3}, {"4:1", 1.0 / 4},
	{"5:1", 1.0 / 5}, {"7:1", 1.0 / 7}, {"10:1", 1.0 / 10}, {"20:1", 1.0 / 20},
	{"1:2 reverse", 2}, {"1:3 reverse", 3}, {"2:3 reverse", 1.5}, {"1:4 reverse", 4},
	{"1:5 reverse", 5}, {"1:7 reverse", 7}, {"1:10 reverse", 10}, {"1:20 reverse", 20},
}

// Audit inspects a price series for the defects that quietly invalidate any
// backtest built on it.
//
// Every check asks the same question a different way: is this series what it
// claims to be? An unadjusted split turns an arithmetic artefact into a -50%
// day the strategy trades. A repeated close manufactures low volatility and
// inflates every risk-adjusted ratio computed from it. A missing month
// shortens the test without saying so. None of that shows up in a result,
// which is precisely why it has to be looked for before one.
func Audit(s *Series) Report {
	r := Report{}
	if s == nil {
		return r
	}
	r.Symbol = s.Symbol
	r.Bars = len(s.Bars)
	if len(s.Bars) == 0 {
		r.Verdict = "no bars at all, so there is nothing to audit"
		return r
	}
	r.First, r.Last = s.Bars[0].Date, s.Bars[len(s.Bars)-1].Date

	a := &auditor{series: s, bars: s.Bars}
	a.classify()

	a.checkDuplicateDates()
	a.checkOHLC()
	a.checkSplits()
	a.checkStale()
	a.checkZeroVolume()
	a.checkExtremes()
	if a.continuous {
		a.add(SeverityNote, KindContinuous, "not an exchange calendar",
			"%d of %d bars fall at weekends, so this trades continuously rather than "+
				"on the US session calendar. Missing sessions and bars on closed days "+
				"were not checked.", a.weekendBars, len(a.bars))
	} else {
		a.checkClosedDays()
		r.Sessions = a.checkGaps()
		if r.First.Date() < CalendarFrom {
			a.add(SeverityNote, KindContinuous, "the calendar does not reach back this far",
				"Bars before %s were not checked for missing or impossible sessions. The "+
					"exchange kept a different holiday list then, and reporting days it "+
					"really was shut as gaps would be worse than saying nothing.", CalendarFrom)
		}
	}

	r.Findings = a.findings
	sort.SliceStable(r.Findings, func(i, j int) bool {
		return severityRank(r.Findings[i].Severity) < severityRank(r.Findings[j].Severity)
	})
	for i := range r.Findings {
		r.Findings[i].Symbol = s.Symbol
	}
	r.Verdict = verdict(r)
	return r
}

// AuditCritical runs only the checks that can disqualify a series, and keeps
// only what does.
//
// This is the version wired into every backtest. It walks the bars a handful
// of times with no calendar arithmetic and no allocation unless something is
// actually wrong, which is what makes it cheap enough to run unconditionally
// on data the run has already paid to load.
func AuditCritical(s *Series) []Finding {
	if s == nil || len(s.Bars) == 0 {
		return nil
	}
	a := &auditor{series: s, bars: s.Bars}
	a.checkDuplicateDates()
	a.checkOHLC()
	a.checkSplits()
	a.checkStale()

	out := a.findings[:0]
	for _, f := range a.findings {
		if f.Severity != SeverityCritical {
			continue
		}
		f.Symbol = s.Symbol
		out = append(out, f)
	}
	return out
}

// auditor carries the state the checks share.
type auditor struct {
	series   *Series
	bars     []Bar
	findings []Finding

	// continuous marks a symbol that trades every day of the week, such as
	// crypto, for which the US market calendar means nothing.
	continuous  bool
	weekendBars int
	// splitDays are the bars already explained by a suspected split, so the
	// extreme-return check does not report the same day twice under a
	// weaker heading.
	splitDays map[Day]bool
}

func (a *auditor) add(sev Severity, kind, title, format string, args ...any) *Finding {
	a.findings = append(a.findings, Finding{
		Severity: sev, Kind: kind, Title: title, Detail: fmt.Sprintf(format, args...),
	})
	return &a.findings[len(a.findings)-1]
}

// classify decides whether the US session calendar applies to this symbol.
func (a *auditor) classify() {
	for _, b := range a.bars {
		if IsWeekend(b.Date) {
			a.weekendBars++
		}
	}
	a.continuous = float64(a.weekendBars) > continuousWeekendShare*float64(len(a.bars))
}

// --- the checks ----------------------------------------------------------

// checkDuplicateDates finds the same session recorded twice.
//
// NewSeries de-duplicates, so a series that reached here through the normal
// loader can never trip this. That is the reason to check anyway: the loader
// silently keeps the later row, and a file with two contradictory prints for
// one day is a file whose other days deserve no confidence either.
func (a *auditor) checkDuplicateDates() {
	var dates []Day
	var n int
	for i := 1; i < len(a.bars); i++ {
		if a.bars[i].Date != a.bars[i-1].Date {
			continue
		}
		n++
		if len(dates) < maxListedDates && (len(dates) == 0 || dates[len(dates)-1] != a.bars[i].Date) {
			dates = append(dates, a.bars[i].Date)
		}
	}
	if n == 0 {
		return
	}
	f := a.add(SeverityCritical, KindDuplicate, "the same day appears more than once",
		"%s, starting %s. Two prices for one session means one of them is wrong, and "+
			"nothing here says which.", plural(n, "duplicate row"), dates[0])
	f.Dates, f.Count = dates, n
}

// checkOHLC finds bars that cannot exist.
//
// These are grouped into one finding rather than four. They have one cause —
// a vendor or an export that did not check its own arithmetic — and four
// separate entries would read as four separate problems.
func (a *auditor) checkOHLC() {
	var kinds []string
	seen := map[string]bool{}
	note := func(k string) {
		if !seen[k] {
			seen[k] = true
			kinds = append(kinds, k)
		}
	}
	var dates []Day
	var n int
	for _, b := range a.bars {
		bad := false
		if notFinite(b.Open, b.High, b.Low, b.Close, b.Volume) {
			note("a value that is not a number")
			bad = true
		}
		if b.Close <= 0 {
			note("a close of zero or less")
			bad = true
		}
		if b.Volume < 0 {
			note("negative volume")
			bad = true
		}
		// A bar carrying only a close is a thin vendor file, not a broken
		// one: the loader backfills the other three from it. Range checks
		// against those backfilled values would report the file's shape as
		// an arithmetic error.
		if b.Open != 0 || b.High != 0 || b.Low != 0 {
			if b.High < b.Low {
				note("a high below the low")
				bad = true
			}
			if b.Close > b.High || b.Close < b.Low {
				note("a close outside the day's range")
				bad = true
			}
			if b.Open > b.High || b.Open < b.Low {
				note("an open outside the day's range")
				bad = true
			}
			if b.Open < 0 || b.High <= 0 || b.Low <= 0 {
				note("a price of zero or less")
				bad = true
			}
		}
		if !bad {
			continue
		}
		n++
		if len(dates) < maxListedDates {
			dates = append(dates, b.Date)
		}
	}
	if n == 0 {
		return
	}
	f := a.add(SeverityCritical, KindOHLC, "bars that cannot exist",
		"%s of %d %s internally inconsistent: %s. Any indicator reading the high, the "+
			"low or a stop checking them is working from impossible numbers.",
		plural(n, "bar"), len(a.bars), verb(n, "is", "are"), joinList(kinds))
	f.Dates, f.Count = dates, n
}

// checkSplits finds one-day steps that look like a corporate action nobody
// adjusted for.
//
// The test is deliberately narrow. A move within 2% of a common ratio is not
// enough on its own — plenty of shares halve on news — so it also has to look
// like a split rather than a collapse: the day's own range must be small,
// because a share that halved because it split traded all day at the new
// price; and the level must hold, because a bad print comes back.
func (a *auditor) checkSplits() {
	for i := 1; i < len(a.bars); i++ {
		prev, cur := adjusted(a.bars[i-1]), adjusted(a.bars[i])
		if prev <= 0 || cur <= 0 {
			continue
		}
		ratio := cur / prev
		label, ok := matchSplitRatio(ratio)
		if !ok {
			continue
		}
		b := a.bars[i]
		if b.Low > 0 && b.High > b.Low && (b.High-b.Low)/b.Low > splitMaxRange {
			continue // it traded the whole way there, so it is a move, not a step
		}
		if a.reverts(i, ratio) {
			continue // a bad print that corrected, not a level that shifted
		}
		if a.recovers(i) {
			continue // the level it arrived at is the one it had all along
		}
		if a.splitDays == nil {
			a.splitDays = map[Day]bool{}
		}
		a.splitDays[b.Date] = true
		f := a.add(SeverityCritical, KindSplit, "a split that looks unadjusted",
			"On %s the adjusted close stepped from %s to %s, a %.1f%% move that matches "+
				"a %s split to within %.1f%%, with a %.1f%% intraday range and no move back "+
				"over the following %d sessions. If this is a corporate action the vendor "+
				"did not adjust for, every return through this date is fiction.",
			b.Date, price(prev), price(cur), (ratio-1)*100, label,
			math.Abs(ratio/splitFactor(label)-1)*100, rangePct(b), a.forward(i))
		f.Dates, f.Count = []Day{b.Date}, 1
	}
}

// reverts reports whether the move at i is undone within the next few bars,
// which makes it a single bad print rather than a permanent level shift.
func (a *auditor) reverts(i int, ratio float64) bool {
	base := adjusted(a.bars[i])
	if base <= 0 {
		return true
	}
	end := i + splitLookahead
	if end >= len(a.bars) {
		end = len(a.bars) - 1
	}
	for j := i + 1; j <= end; j++ {
		p := adjusted(a.bars[j])
		if p <= 0 {
			continue
		}
		// Back to within a tenth of where it was before the step.
		if math.Abs(p/(base/ratio)-1) < 0.10 {
			return true
		}
	}
	return false
}

// recovers reports whether the level this step arrives at is one the series
// was already sitting at shortly before it.
//
// Without this, the day after a single bad print is itself a flawless reverse
// split: the price doubles back to where it was and stays there, which is
// every test a split has to pass. A real split arrives somewhere the symbol
// has not just been.
func (a *auditor) recovers(i int) bool {
	cur := adjusted(a.bars[i])
	if cur <= 0 {
		return false
	}
	start := i - 1 - splitLookahead
	if start < 0 {
		start = 0
	}
	for j := start; j <= i-2; j++ {
		p := adjusted(a.bars[j])
		if p > 0 && math.Abs(p/cur-1) < 0.10 {
			return true
		}
	}
	return false
}

// forward reports how many bars follow i, capped at the lookahead window.
func (a *auditor) forward(i int) int {
	n := len(a.bars) - 1 - i
	if n > splitLookahead {
		n = splitLookahead
	}
	return n
}

// checkStale finds runs of an identical close.
//
// One unchanged close is a quiet day. A week of them on anything liquid is a
// feed that stopped updating, and it does more than hide a move: a run of
// zero returns pulls measured volatility down and every ratio divided by it
// up, so the effect on a result is to flatter it.
func (a *auditor) checkStale() {
	var runs, longest, listed int
	var longestStart, longestEnd Day
	var dates []Day

	i := 0
	for i < len(a.bars) {
		j := i + 1
		for j < len(a.bars) && a.bars[j].Close == a.bars[i].Close {
			j++
		}
		if n := j - i; n >= staleRun {
			runs++
			if n > longest {
				longest, longestStart, longestEnd = n, a.bars[i].Date, a.bars[j-1].Date
			}
			if listed < maxListedDates {
				dates = append(dates, a.bars[i].Date)
				listed++
			}
		}
		i = j
	}
	if runs == 0 {
		return
	}
	sev := SeverityWarning
	if longest >= staleFrozen {
		sev = SeverityCritical
	}
	f := a.add(sev, KindStale, "the price stopped moving",
		"%s of an identical close, the longest %d sessions from %s to %s. "+
			"A run of unchanged closes is measured as zero volatility, which lowers the "+
			"denominator of every risk-adjusted ratio computed from this series.",
		plural(runs, "run"), longest, longestStart, longestEnd)
	f.Dates, f.Count = dates, runs
}

// checkGaps finds sessions the market held and this series does not have. It
// returns the number of sessions the calendar expected.
func (a *auditor) checkGaps() int {
	first, last := a.bars[0].Date.Date(), a.bars[len(a.bars)-1].Date.Date()
	if first < CalendarFrom {
		first = CalendarFrom
	}
	if last < first {
		return 0
	}
	have := make(map[Day]bool, len(a.bars))
	for _, b := range a.bars {
		have[b.Date.Date()] = true
	}
	sessions := TradingDays(first, last)

	var missing []Day
	for _, d := range sessions {
		if !have[d] {
			missing = append(missing, d)
		}
	}
	if len(missing) == 0 {
		return len(sessions)
	}

	// The longest unbroken run matters more than the total: forty scattered
	// halts and one missing quarter are different problems.
	longest, runStart, runEnd := 1, missing[0], missing[0]
	cur, curStart := 1, missing[0]
	for i := 1; i < len(missing); i++ {
		prev := sessionBefore(sessions, missing[i])
		if prev == missing[i-1] {
			cur++
		} else {
			cur, curStart = 1, missing[i]
		}
		if cur > longest {
			longest, runStart, runEnd = cur, curStart, missing[i]
		}
	}

	share := float64(len(missing)) / float64(len(sessions))
	sev := SeverityNote
	switch {
	case longest >= 10 || share > 0.02:
		sev = SeverityCritical
	case longest >= 3 || len(missing) > 3:
		sev = SeverityWarning
	}
	dates := missing
	if len(dates) > maxListedDates {
		dates = dates[:maxListedDates]
	}
	detail := fmt.Sprintf("%s of %d between %s and %s %s no bar (%.1f%%), the longest "+
		"stretch %d sessions from %s to %s. Weekends and US market holidays are already "+
		"excluded, so these are days the market was open and this symbol is silent.",
		plural(len(missing), "session"), len(sessions), first, last,
		verb(len(missing), "has", "have"), share*100, longest, runStart, runEnd)
	if longest == 1 {
		detail = fmt.Sprintf("%s of %d between %s and %s %s no bar (%.1f%%), none of them "+
			"consecutive. Weekends and US market holidays are already excluded, so these "+
			"are days the market was open and this symbol is silent — a halt, a delisting "+
			"window, or a vendor that dropped them.",
			plural(len(missing), "session"), len(sessions), first, last,
			verb(len(missing), "has", "have"), share*100)
	}
	f := a.add(sev, KindGap, "sessions missing from the series", "%s", detail)
	f.Dates, f.Count = dates, len(missing)
	return len(sessions)
}

// checkClosedDays finds bars on days the market was shut.
func (a *auditor) checkClosedDays() {
	var weekend, holiday []Day
	var nWeekend, nHoliday int
	for _, b := range a.bars {
		d := b.Date.Date()
		if d < CalendarFrom {
			continue
		}
		switch {
		case IsWeekend(d):
			nWeekend++
			if len(weekend) < maxListedDates {
				weekend = append(weekend, b.Date)
			}
		case IsMarketHoliday(d):
			nHoliday++
			if len(holiday) < maxListedDates {
				holiday = append(holiday, b.Date)
			}
		}
	}
	if nWeekend > 0 {
		f := a.add(SeverityCritical, KindClosedDay, "bars at the weekend",
			"%s of %d %s on a Saturday or a Sunday, starting %s. US equities do not "+
				"trade then, so either the dates are shifted by a timezone or the vendor "+
				"is inventing sessions. Every date-based rule in a strategy is then "+
				"aimed at the wrong bar.",
			plural(nWeekend, "bar"), len(a.bars), verb(nWeekend, "falls", "fall"), weekend[0])
		f.Dates, f.Count = weekend, nWeekend
	}
	if nHoliday > 0 {
		// A warning rather than critical, because the other explanation is
		// that this calendar is missing an unscheduled closure, and being
		// wrong about the market is not grounds for failing someone's data.
		f := a.add(SeverityWarning, KindClosedDay, "bars on market holidays",
			"%s %s on %s the exchange was shut, starting %s. Usually a vendor padding "+
				"its calendar by carrying the previous close forward, which adds sessions "+
				"that never happened.",
			plural(nHoliday, "bar"), verb(nHoliday, "falls", "fall"),
			verb(nHoliday, "a day", "days"), holiday[0])
		f.Dates, f.Count = holiday, nHoliday
	}
}

// checkZeroVolume finds sessions with no volume on a series that otherwise
// reports it. A symbol with no volume anywhere is an index, not a defect.
func (a *auditor) checkZeroVolume() {
	var withVolume, zero int
	var dates []Day
	for _, b := range a.bars {
		if b.Volume > 0 {
			withVolume++
			continue
		}
		if b.Volume == 0 {
			zero++
			if len(dates) < maxListedDates {
				dates = append(dates, b.Date)
			}
		}
	}
	if zero == 0 || withVolume < len(a.bars)*9/10 {
		return
	}
	sev := SeverityNote
	if float64(zero) > 0.01*float64(len(a.bars)) {
		sev = SeverityWarning
	}
	f := a.add(sev, KindZeroVolume, "sessions with no volume",
		"%s of %d %s zero volume while the rest report it, starting %s. A price with "+
			"nothing traded behind it is a quote, not a fill, and a backtest that "+
			"trades on those days is filling against nobody.",
		plural(zero, "bar"), len(a.bars), verb(zero, "reports", "report"), dates[0])
	f.Dates, f.Count = dates, zero
}

// checkExtremes names the largest daily moves.
//
// This one is not an accusation. Most moves this size are real, and the point
// of listing the dates is so a reader can go and find out which — the check
// earns its place by making that a two-minute job instead of a project.
func (a *auditor) checkExtremes() {
	type move struct {
		d Day
		r float64
	}
	var moves []move
	for i := 1; i < len(a.bars); i++ {
		prev, cur := adjusted(a.bars[i-1]), adjusted(a.bars[i])
		if prev <= 0 || cur <= 0 || a.splitDays[a.bars[i].Date] {
			continue
		}
		if r := cur/prev - 1; math.Abs(r) >= extremeMove {
			moves = append(moves, move{a.bars[i].Date, r})
		}
	}
	if len(moves) == 0 {
		return
	}
	sort.SliceStable(moves, func(i, j int) bool {
		return math.Abs(moves[i].r) > math.Abs(moves[j].r)
	})
	listed := moves
	if len(listed) > 5 {
		listed = listed[:5]
	}
	parts := make([]string, 0, len(listed))
	dates := make([]Day, 0, len(listed))
	for _, m := range listed {
		parts = append(parts, fmt.Sprintf("%s %+.1f%%", m.d, m.r*100))
		dates = append(dates, m.d)
	}
	sev := SeverityNote
	if float64(len(moves)) > 0.02*float64(len(a.bars)) {
		sev = SeverityWarning
	}
	f := a.add(sev, KindExtreme, "very large single-day moves",
		"%s moved more than %.0f%%, the largest being %s. Most moves this size are "+
			"real — earnings, a bid, a crash — and this is a list to check rather "+
			"than a list of errors. It is the ones with no news behind them that are "+
			"the data problem.",
		plural(len(moves), "session"), extremeMove*100, strings.Join(parts, ", "))
	f.Dates, f.Count = dates, len(moves)
}

// --- helpers -------------------------------------------------------------

// adjusted is the price a backtest actually computes returns from, which is
// the series that has to be audited. A vendor that adjusted its close
// correctly leaves no step here even though the raw close has one.
func adjusted(b Bar) float64 {
	if b.AdjClose > 0 {
		return b.AdjClose
	}
	return b.Close
}

// matchSplitRatio names the split a one-day factor resembles, if any.
func matchSplitRatio(ratio float64) (string, bool) {
	for _, s := range splitRatios {
		if math.Abs(ratio/s.Factor-1) <= splitTolerance {
			return s.Label, true
		}
	}
	return "", false
}

func splitFactor(label string) float64 {
	for _, s := range splitRatios {
		if s.Label == label {
			return s.Factor
		}
	}
	return 1
}

// notFinite reports whether any value is NaN or infinite. A NaN price does
// not fail any comparison, so nothing downstream notices it until a metric
// comes out undefined and the reason is three layers away.
func notFinite(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return true
		}
	}
	return false
}

func rangePct(b Bar) float64 {
	if b.Low <= 0 || b.High <= b.Low {
		return 0
	}
	return (b.High - b.Low) / b.Low * 100
}

// sessionBefore returns the trading day immediately before d in sessions.
func sessionBefore(sessions []Day, d Day) Day {
	i := sort.Search(len(sessions), func(i int) bool { return sessions[i] >= d })
	if i <= 0 {
		return ""
	}
	return sessions[i-1]
}

func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 0
	case SeverityWarning:
		return 1
	default:
		return 2
	}
}

// price formats a price with enough precision to be recognisable at any
// level, without pretending to more than a vendor prints.
func price(v float64) string {
	if math.Abs(v) < 1 {
		return fmt.Sprintf("%.4f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// plural renders a count with its noun. Findings are meant to be read, and
// "1 sessions" reads as a machine talking.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// verb picks the form of a verb that agrees with a count.
func verb(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

func joinList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// verdict is the one sentence to read if nothing else is read.
func verdict(r Report) string {
	if len(r.Findings) == 0 {
		return fmt.Sprintf("nothing found across %d bars, which is evidence of nothing "+
			"having been found rather than of the data being right", r.Bars)
	}
	return r.Findings[0].Title
}
