package ledger

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charbelkassab/pyrite/internal/market"
)

// Dataset is the data a search was run against.
type Dataset struct {
	// Symbols is the tradable universe. Order and case do not matter.
	Symbols []string
	// Index names a point-in-time index universe such as "sp500". When set it
	// replaces the symbol list, because which symbols the index holds depends
	// on the day and no fixed list describes it.
	Index string
	// Start and End bound the backtest. An empty bound means the caller never
	// pinned it down.
	Start, End market.Day
	Interval   market.Interval
}

// openBound stands in for a date the caller left unset.
const openBound = "*"

// DatasetKey identifies the research problem a search was run against.
//
// This is the crux of the ledger. Deflated Sharpe and PBO ask "how many
// combinations did this one search try"; the ledger asks "how many has this
// dataset seen from you, ever", and that question only has an answer if two
// invocations three weeks apart can be recognised as the same problem. Two
// runs are the same problem when they could have produced the same result by
// luck: same securities, same period, same bar size. Nothing else belongs in
// the key. The strategy, its parameters and the objective all vary freely
// within one research problem, and folding any of them in would reset the
// trial count every time the researcher changed their mind — which is
// precisely the count that must not reset.
//
// Symbols are upper-cased, de-duplicated and sorted, so a universe typed as
// "spy,qqq" one week and "QQQ, SPY" the next lands on one key. A different
// period or bar size gets its own key, because a search never touched the
// data it did not load and owes it nothing.
func DatasetKey(d Dataset) string {
	subject := openBound
	if idx := strings.ToLower(strings.TrimSpace(d.Index)); idx != "" {
		// The marker keeps an index name from colliding with a ticker of the
		// same spelling: tickers never carry it.
		subject = "@" + idx
	} else if syms := normaliseSymbols(d.Symbols); len(syms) > 0 {
		subject = strings.Join(syms, ",")
	}

	interval := market.DefaultInterval
	if d.Interval.Valid() {
		interval = d.Interval
	}
	return fmt.Sprintf("%s:%s:%s:%s", subject,
		bound(d.Start), bound(d.End), interval)
}

func bound(d market.Day) string {
	if s := strings.TrimSpace(string(d)); s != "" {
		return s
	}
	return openBound
}

func normaliseSymbols(syms []string) []string {
	seen := make(map[string]bool, len(syms))
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		s = market.NormalizeSymbol(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
