package engine

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// NoReasonLabel is the bucket for orders the strategy never explained.
//
// These are not dropped. An unreasoned entry is still a rule — usually a
// rebalance — and a table that quietly omits it stops summing to the result
// it claims to decompose, which is the one property that makes it checkable.
const NoReasonLabel = "(no reason given)"

// Grouping names for the two ways a run's reason strings are collapsed into
// rules. They are prose because they are printed: the reader has to be told
// which of the two tables they are looking at.
const (
	// GroupingVerbatim treats every distinct string as its own rule.
	GroupingVerbatim = "as written"
	// GroupingNumberless replaces each number with N first.
	GroupingNumberless = "with numbers replaced by N"
)

// ReasonStats is one rule's record: every closed round trip whose entry, or
// whose exit, was explained the same way.
type ReasonStats struct {
	Reason string `json:"reason"`
	Trades int    `json:"trades"`
	Wins   int    `json:"wins"`

	// NetPnL is realised and after costs. MeanPnL is it per round trip,
	// which is what decides whether a rule is worth keeping — a rule can
	// hold the largest total simply by firing most often.
	NetPnL  float64 `json:"net_pnl"`
	MeanPnL float64 `json:"mean_pnl"`
	WinRate float64 `json:"win_rate"`
	// Share is this rule's slice of the run's realised P&L. The denominator
	// is the size of that total, so the column is a decomposition: it sums
	// to 100% for a run that made money and to -100% for one that lost it,
	// and a rule can exceed 100% when another rule is handing money back.
	// That last case is the finding, not a formatting fault. It is
	// undefined when the run realised nothing at all, which is the only
	// figure a share of it could honestly take.
	Share Ratio `json:"share"`

	MeanDaysHeld float64 `json:"mean_days_held"`
	MeanBarsHeld float64 `json:"mean_bars_held"`
	// MeanMAEPct and MeanMFEPct are the average worst and best excursions of
	// this rule's trades. They separate two failures that look identical in
	// the P&L column: a rule whose losers show a large mean MFE found
	// something and handed it back, and a rule whose losers show neither
	// found nothing at all.
	MeanMAEPct float64 `json:"mean_mae_pct"`
	MeanMFEPct float64 `json:"mean_mfe_pct"`
}

// ReasonTable is a run's round trips decomposed by rule, under one way of
// deciding when two reason strings name the same rule.
type ReasonTable struct {
	// Grouping says how these rows were collapsed. A row labelled
	// "RSI N, oversold" is a different claim from a row labelled
	// "RSI 28, oversold", so the table cannot be read without it.
	Grouping string        `json:"grouping"`
	ByEntry  []ReasonStats `json:"by_entry"`
	ByExit   []ReasonStats `json:"by_exit"`
}

// ReasonAttribution answers which rule made the money, rather than which
// symbol did.
//
// It is the more useful of the two questions and the one nothing else here
// asks. A symbol cannot be switched off; an entry condition can. A strategy
// with four entry conditions of which three lose money is three deletions
// away from being simpler and better, and no other view in this tool says so.
type ReasonAttribution struct {
	// Verbatim groups the reasons exactly as the strategy wrote them.
	Verbatim ReasonTable `json:"verbatim"`
	// Numberless is the same round trips regrouped with the numbers taken
	// out, present only when that actually merges rows.
	Numberless *ReasonTable `json:"numberless,omitempty"`
	// Preferred is the Grouping of the table meant for display.
	Preferred string `json:"preferred"`
}

// Table returns the decomposition to show a reader.
func (ra ReasonAttribution) Table() ReasonTable {
	if ra.Numberless != nil && ra.Preferred == ra.Numberless.Grouping {
		return *ra.Numberless
	}
	return ra.Verbatim
}

// Unattributed reports that nothing in this table says why it happened —
// every round trip came from an order that gave no reason. There is then
// nothing to compare, which is worth saying rather than showing.
func (t ReasonTable) Unattributed() bool {
	return unreasonedOnly(t.ByEntry) && unreasonedOnly(t.ByExit)
}

func unreasonedOnly(rows []ReasonStats) bool {
	return len(rows) == 0 || (len(rows) == 1 && rows[0].Reason == NoReasonLabel)
}

// ComputeReasonAttribution groups closed round trips by the reason they were
// opened with and, separately, by the reason they were closed with.
func ComputeReasonAttribution(trades []Trade) ReasonAttribution {
	entry := func(t Trade) string { return t.EntryReason }
	exit := func(t Trade) string { return t.ExitReason }

	ra := ReasonAttribution{
		Verbatim: ReasonTable{
			Grouping: GroupingVerbatim,
			ByEntry:  groupByReason(trades, entry, tidyReason),
			ByExit:   groupByReason(trades, exit, tidyReason),
		},
		Preferred: GroupingVerbatim,
	}
	if len(ra.Verbatim.ByEntry) == 0 && len(ra.Verbatim.ByExit) == 0 {
		return ra
	}

	numberless := ReasonTable{
		Grouping: GroupingNumberless,
		ByEntry:  groupByReason(trades, entry, numberlessReason),
		ByExit:   groupByReason(trades, exit, numberlessReason),
	}
	if rowCount(numberless) < rowCount(ra.Verbatim) {
		ra.Numberless = &numberless
		if preferNumberless(ra.Verbatim, numberless) {
			ra.Preferred = numberless.Grouping
		}
	}
	return ra
}

// groupByReason aggregates the closed round trips by a normalised reason.
//
// label is the normalisation: it decides which strings name the same rule and
// what that rule is called. Open positions are excluded throughout, because
// an unrealised gain attributed to a rule is a claim about a trade that has
// not finished.
func groupByReason(trades []Trade, pick func(Trade) string, label func(string) string) []ReasonStats {
	agg := map[string]*ReasonStats{}
	// First-seen order, so that rows with identical P&L land in a stable
	// order rather than a map's.
	var order []string
	var total float64

	for _, t := range trades {
		if t.Open {
			continue
		}
		name := label(pick(t))
		if name == "" {
			name = NoReasonLabel
		}
		// Case-folding decides the grouping; the first spelling seen decides
		// the display. Nothing fuzzier is attempted: two reasons that differ
		// are two rules, and merging a pair that merely look alike would
		// destroy exactly the comparison this table exists to make.
		key := strings.ToLower(name)
		s, ok := agg[key]
		if !ok {
			s = &ReasonStats{Reason: name, Share: Ratio(math.NaN())}
			agg[key] = s
			order = append(order, key)
		}
		s.Trades++
		s.NetPnL += t.NetPnL
		s.MeanDaysHeld += float64(t.DaysHeld)
		s.MeanBarsHeld += float64(t.BarsHeld)
		s.MeanMAEPct += t.MAEPct
		s.MeanMFEPct += t.MFEPct
		if t.NetPnL > 0 {
			s.Wins++
		}
		total += t.NetPnL
	}

	out := make([]ReasonStats, 0, len(agg))
	for _, k := range order {
		s := agg[k]
		n := float64(s.Trades)
		s.MeanPnL = s.NetPnL / n
		s.WinRate = float64(s.Wins) / n
		s.MeanDaysHeld /= n
		s.MeanBarsHeld /= n
		s.MeanMAEPct /= n
		s.MeanMFEPct /= n
		if total != 0 {
			s.Share = Ratio(s.NetPnL / math.Abs(total))
		}
		out = append(out, *s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].NetPnL != out[j].NetPnL {
			return out[i].NetPnL > out[j].NetPnL
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// tidyReason trims a reason and collapses its internal whitespace, so a
// string that differs only in how it was typed is not a second rule.
func tidyReason(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// numberlessReason replaces every number in a reason with N.
//
// Reasons are interpolated — "RSI " + rsi.toFixed(0) + ", oversold", or
// ctx.params.fast + " day crossed above " + ctx.params.slow — so a swept
// parameter or a printed indicator value produces one string per trade, and
// the verbatim table degenerates into the trade list it was meant to
// summarise. This is the coarser view: it merges "50 day crossed above 200
// day" with "20 day crossed above 100 day", which are the same rule at two
// settings, and leaves genuinely different sentences apart.
func numberlessReason(s string) string {
	tidy := tidyReason(s)
	if tidy == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(tidy))
	r := []rune(tidy)
	for i := 0; i < len(r); i++ {
		if !unicode.IsDigit(r[i]) {
			b.WriteRune(r[i])
			continue
		}
		// Consume the whole number, decimal point included, and emit one N
		// for it. A decimal point only stays inside the number when a digit
		// follows it, so "up 3.5%, take profit" collapses but "oversold. 30
		// is low" keeps its full stop.
		for i+1 < len(r) && (unicode.IsDigit(r[i+1]) ||
			(r[i+1] == '.' && i+2 < len(r) && unicode.IsDigit(r[i+2]))) {
			i++
		}
		b.WriteRune('N')
	}
	return b.String()
}

// preferNumberless decides which table to put in front of the reader.
//
// Collapsing the numbers costs real information — the settings a rule fired
// at — so it is only worth it when it actually merges most of the rows.
// Thirty rows down to three is a different table; four down to three is the
// same table with worse labels.
func preferNumberless(verbatim, numberless ReasonTable) bool {
	v, n := rowCount(verbatim), rowCount(numberless)
	const tooManyToRead = 8
	return v >= tooManyToRead && n*2 <= v
}

func rowCount(t ReasonTable) int { return len(t.ByEntry) + len(t.ByExit) }

// SumNetPnL totals a set of reason rows.
//
// Every closed round trip lands in exactly one entry row and one exit row, so
// both tables sum to the realised P&L of the whole run. That is the check
// that the decomposition dropped nothing and double-counted nothing, and it
// is worth stating in the output rather than only in a test.
func SumNetPnL(rows []ReasonStats) float64 {
	var total float64
	for _, r := range rows {
		total += r.NetPnL
	}
	return total
}

// TopAndBottom trims a reason table for display, keeping both ends.
//
// Rows are sorted by P&L, so keeping the first n would hide the losing rules
// — which are the rows a reader came here to find. It returns the head, how
// many rows were dropped from the middle, and the tail.
func TopAndBottom(rows []ReasonStats, n int) ([]ReasonStats, int, []ReasonStats) {
	if n < 2 || len(rows) <= n {
		return rows, 0, nil
	}
	head := n / 2
	tail := n - head
	return rows[:head], len(rows) - n, rows[len(rows)-tail:]
}
