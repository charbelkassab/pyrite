package engine

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/charbelkassab/pyrite/internal/market"
)

// BorrowRate is what one name costs to borrow, and whether it can be borrowed
// at all.
type BorrowRate struct {
	// AnnualPct is the fee, as an annual fraction of the short's market
	// value. Ignored when Unavailable is set.
	AnnualPct float64 `json:"annual_pct"`
	// Unavailable marks a name with no locate.
	//
	// Refusing the trade is the honest model, and it is why this is a flag
	// rather than a very large fee. A punitive rate still lets the position
	// open, still lets it run, and still lets the backtest book the profit
	// from a trade nobody could have made — it only makes that profit
	// slightly smaller. A rejection says the trade did not exist.
	Unavailable bool `json:"unavailable,omitempty"`
}

// BorrowSchedule prices the short side per name.
//
// The flat rate it replaces is wrong in the one direction that matters. A
// general-collateral fee is a couple of percent a year, and the names a short
// strategy picks — the crowded, the distressed, the recently halved — are
// exactly the ones on special at twenty, fifty, or not available at any
// price. Charging three percent across the book is not a conservative
// simplification; it is a model of a book nobody could have run.
type BorrowSchedule struct {
	// GeneralCollateralPct is charged on any name not listed in Rates. Zero
	// falls back to Costs.ShortBorrowAnnualPct, so a schedule that only
	// names the hard cases does not silently make everything else free.
	GeneralCollateralPct float64 `json:"general_collateral_pct,omitempty"`
	// Rates are the per-name overrides, keyed by normalised symbol.
	Rates map[string]BorrowRate `json:"rates,omitempty"`
}

// hardToBorrowPct is the annual fee above which a name has stopped being
// general collateral.
//
// Anything on special is being rationed, and a rate that is a multiple of GC
// is the market saying the locate is scarce. Five percent is where the stock
// loan desks draw that line, and it is well clear of the two to three percent
// a broad borrow costs.
const hardToBorrowPct = 0.05

// Rate reports what shorting a symbol costs and whether it can be shorted.
//
// A nil schedule prices everything at the fallback and refuses nothing, which
// is exactly what the engine did before there was a schedule.
func (b *BorrowSchedule) Rate(symbol string, fallback float64) (float64, bool) {
	// A rate that is not a number is charged as zero rather than carried.
	// It reaches the borrow report and the manifest, and a bare NaN there
	// truncates the whole JSON response.
	if math.IsNaN(fallback) || math.IsInf(fallback, 0) {
		fallback = 0
	}
	if b == nil {
		return fallback, true
	}
	if r, ok := b.Rates[market.NormalizeSymbol(symbol)]; ok {
		if r.Unavailable {
			return 0, false
		}
		return r.AnnualPct, true
	}
	if b.GeneralCollateralPct > 0 {
		return b.GeneralCollateralPct, true
	}
	return fallback, true
}

// PerName reports whether the schedule prices any individual name, as opposed
// to only moving the general collateral rate.
func (b *BorrowSchedule) PerName() bool { return b != nil && len(b.Rates) > 0 }

// HardToBorrow reports whether a name is priced above general collateral or
// refused outright.
func (b *BorrowSchedule) HardToBorrow(symbol string) bool {
	if b == nil {
		return false
	}
	r, ok := b.Rates[market.NormalizeSymbol(symbol)]
	return ok && (r.Unavailable || r.AnnualPct >= hardToBorrowPct)
}

// Fingerprint identifies a schedule without carrying it.
//
// A stock loan file can hold thousands of names, and the manifest's rule is
// hashes rather than bodies: a run has to record which rates it used, not
// reproduce them. Built over the sorted names so the same file always hashes
// the same way whatever order it was read in.
func (b *BorrowSchedule) Fingerprint() string {
	if b == nil {
		return ""
	}
	syms := make([]string, 0, len(b.Rates))
	for sym := range b.Rates {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	var sb strings.Builder
	fmt.Fprintf(&sb, "gc=%g\n", b.GeneralCollateralPct)
	for _, sym := range syms {
		r := b.Rates[sym]
		fmt.Fprintf(&sb, "%s=%g,%t\n", sym, r.AnnualPct, r.Unavailable)
	}
	return hashString(sb.String())
}

// Names is how many symbols the schedule prices individually.
func (b *BorrowSchedule) Names() int {
	if b == nil {
		return 0
	}
	return len(b.Rates)
}

// LoadBorrowCSV reads a borrow file from disk.
func LoadBorrowCSV(path string) (*BorrowSchedule, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	s, err := ParseBorrowCSV(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// ParseBorrowCSV reads a borrow schedule from CSV.
//
// The format is what a stock loan report already looks like:
//
//	symbol,annual_pct[,available]
//	AAPL,0.3
//	GME,85,yes
//	SIRI,,no
//	*,2.5
//
// Rates are percentages, because that is how a desk quotes them and how the
// file will have arrived. A symbol of `*` or `default` sets the general
// collateral rate. A blank rate, or an availability column reading no or
// false, means there is no locate and the short cannot be opened. A header
// row is detected and skipped.
func ParseBorrowCSV(r io.Reader) (*BorrowSchedule, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	cr.Comment = '#'

	out := &BorrowSchedule{Rates: map[string]BorrowRate{}}
	line := 0
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		line++
		if len(rec) == 0 {
			continue
		}
		sym := strings.TrimSpace(rec[0])
		if sym == "" {
			continue
		}
		if line == 1 && isBorrowHeader(sym) {
			continue
		}

		var rate BorrowRate
		raw := ""
		if len(rec) > 1 {
			raw = strings.TrimSpace(rec[1])
		}
		switch {
		case raw == "" || isNoLocate(raw):
			rate.Unavailable = true
		default:
			v, err := strconv.ParseFloat(strings.TrimSuffix(raw, "%"), 64)
			if err != nil {
				return nil, fmt.Errorf("line %d: %q is not a borrow rate", line, raw)
			}
			// ParseFloat accepts "NaN" and "Inf". Either would travel into
			// the manifest and the borrow report, and a bare NaN truncates
			// the whole JSON response — so the file is rejected here rather
			// than the number being carried and defended everywhere after.
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, fmt.Errorf("line %d: %q is not a usable borrow rate", line, raw)
			}
			if v < 0 {
				return nil, fmt.Errorf("line %d: a borrow rate cannot be negative (%s)", line, raw)
			}
			rate.AnnualPct = v / 100
		}
		if len(rec) > 2 {
			if a := strings.TrimSpace(rec[2]); a != "" && isNoLocate(a) {
				rate.Unavailable = true
			}
		}

		if sym == "*" || strings.EqualFold(sym, "default") {
			if rate.Unavailable {
				return nil, fmt.Errorf("line %d: the general collateral rate cannot be unavailable", line)
			}
			out.GeneralCollateralPct = rate.AnnualPct
			continue
		}
		out.Rates[market.NormalizeSymbol(sym)] = rate
	}
	if len(out.Rates) == 0 && out.GeneralCollateralPct == 0 {
		return nil, fmt.Errorf("no borrow rates found")
	}
	return out, nil
}

func isBorrowHeader(first string) bool {
	switch strings.ToLower(strings.TrimSpace(first)) {
	case "symbol", "ticker", "sym", "name":
		return true
	}
	return false
}

// isNoLocate reads the several ways a file says there is nothing to borrow.
func isNoLocate(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "no", "n", "false", "0", "na", "n/a", "none", "unavailable", "htb", "-":
		return true
	}
	return false
}

// BorrowName is one symbol's share of the borrow bill.
type BorrowName struct {
	Symbol string `json:"symbol"`
	// AnnualPct is the rate charged on it.
	AnnualPct float64 `json:"annual_pct"`
	// Cost is what accrued over the run.
	Cost float64 `json:"cost"`
	// Sessions is how many sessions the name was held short. Borrow accrues
	// on the position rather than on the trade, so this is the multiplier a
	// reader needs in order to check the figure.
	Sessions int `json:"sessions"`
	// HardToBorrow marks a name priced above general collateral.
	HardToBorrow bool `json:"hard_to_borrow,omitempty"`
}

// BorrowRefusal is a short the run would not let the strategy open.
type BorrowRefusal struct {
	Symbol string `json:"symbol"`
	// Orders is how many times a short in this name was refused.
	Orders int `json:"orders"`
	// Shares is the total quantity refused.
	Shares float64 `json:"shares"`
}

// BorrowReport is what the short side cost and what it was not allowed to do.
type BorrowReport struct {
	// TotalCost is the borrow charged over the whole run.
	TotalCost float64 `json:"total_cost"`
	// PerName is true when a schedule priced individual names. When it is
	// false every short was charged the same rate, which is the assumption
	// most likely to be wrong exactly where a short strategy thinks its edge
	// is.
	PerName bool `json:"per_name"`
	// Names is the per-symbol accrual, dearest first.
	Names []BorrowName `json:"names,omitempty"`
	// Refused lists the shorts that could not be opened at all.
	Refused []BorrowRefusal `json:"refused,omitempty"`
	// LowPricedShorts names symbols shorted below pennyShortPrice a share.
	// It is a prompt to check a locate, not a claim that one was missing.
	LowPricedShorts []string `json:"low_priced_shorts,omitempty"`
}

// pennyShortPrice is the share price below which a locate stops being routine.
//
// It is not a claim that every cheap share is hard to borrow. It is the band
// the hard ones cluster in, and the point of naming them is to send a reader
// to check rather than to settle it here.
const pennyShortPrice = 5.0

// Shorted reports whether the run carried any short exposure at all, which is
// what decides whether any of this is worth showing.
func (b *BorrowReport) Shorted() bool {
	return b != nil && (b.TotalCost > 0 || len(b.Names) > 0 ||
		len(b.Refused) > 0 || len(b.LowPricedShorts) > 0)
}

// Charged reports whether there is a bill to show. A run that shorted with
// the borrow rate set to zero has a short side worth criticising and no table
// worth printing.
func (b *BorrowReport) Charged() bool {
	return b != nil && (b.TotalCost > 0 || len(b.Names) > 0 || len(b.Refused) > 0)
}

// HardNames lists the names charged above general collateral.
func (b *BorrowReport) HardNames() []string {
	if b == nil {
		return nil
	}
	var out []string
	for _, n := range b.Names {
		if n.HardToBorrow {
			out = append(out, n.Symbol)
		}
	}
	return out
}

// buildBorrowReport assembles the short-side accounting after a run.
func buildBorrowReport(p *Portfolio, fills []Fill) *BorrowReport {
	if p == nil {
		return nil
	}
	sched := p.Borrow
	rep := &BorrowReport{TotalCost: p.borrowCost, PerName: sched.PerName()}

	for sym, cost := range p.borrowBySym {
		rate, _ := sched.Rate(sym, p.Costs.ShortBorrowAnnualPct)
		rep.Names = append(rep.Names, BorrowName{
			Symbol: sym, AnnualPct: rate, Cost: cost,
			Sessions: p.borrowSessions[sym], HardToBorrow: sched.HardToBorrow(sym),
		})
	}
	// Dearest first, then by name, so two identical runs list them the same
	// way whatever order the book was built in.
	sort.SliceStable(rep.Names, func(i, j int) bool {
		if rep.Names[i].Cost != rep.Names[j].Cost {
			return rep.Names[i].Cost > rep.Names[j].Cost
		}
		return rep.Names[i].Symbol < rep.Names[j].Symbol
	})

	for sym, n := range p.borrowRefused {
		rep.Refused = append(rep.Refused, BorrowRefusal{
			Symbol: sym, Orders: n, Shares: p.borrowRefusedShares[sym],
		})
	}
	sort.SliceStable(rep.Refused, func(i, j int) bool {
		return rep.Refused[i].Symbol < rep.Refused[j].Symbol
	})

	// A price-based prompt, for the common case where nobody supplied a
	// borrow file at all. The engine cannot know what was on special in
	// 2013; it can see that the strategy shorted something trading at $1.80,
	// and that is where the locates that never existed live.
	seen := map[string]bool{}
	for _, f := range fills {
		if f.Side != SideShort || f.Price >= pennyShortPrice || seen[f.Symbol] {
			continue
		}
		seen[f.Symbol] = true
		rep.LowPricedShorts = append(rep.LowPricedShorts, f.Symbol)
	}
	sort.Strings(rep.LowPricedShorts)

	if !rep.Shorted() {
		return nil
	}
	return rep
}

// borrowShareOfGain is how much of a run's profit the borrow bill has to eat
// before it stops being a line item and becomes the thing the result turns
// on. A quarter is where a plausible error in the rate — and the rate is the
// input with the least evidence behind it — starts moving the conclusion.
const borrowShareOfGain = 0.25

// addBorrowFindings reports what the short side cost and what it was refused.
func addBorrowFindings(res *Result, add func(Severity, string, string, ...any)) {
	b := res.Borrow
	if b == nil {
		return
	}

	if len(b.Refused) > 0 {
		var orders int
		names := make([]string, 0, len(b.Refused))
		for _, r := range b.Refused {
			orders += r.Orders
			names = append(names, r.Symbol)
		}
		add(SeverityWarning, "shorts that could not be borrowed were refused",
			"%s in %s %s rejected for want of a locate (%s). Those positions are "+
				"absent from this result rather than priced into it, so the strategy "+
				"being measured is not quite the one that was written.",
			plural(orders, "order"), plural(len(names), "name"),
			verb(orders, "was", "were"), joinList(names))
	}

	gross := res.Metrics.EndValue - res.Metrics.StartValue
	if b.TotalCost > 0 {
		switch {
		case gross <= 0:
			add(SeverityNote, "the borrow bill on a losing run",
				"%s of borrow was paid on a run that finished %s down. The fee is not "+
					"why it lost, but it is money that left the account before the "+
					"strategy had a chance to be wrong.", money(b.TotalCost), money(-gross))
		case b.TotalCost > borrowShareOfGain*gross:
			add(SeverityWarning, "much of the profit went on borrow",
				"%s of borrow was charged against %s of gain — %.0f%% of it. A short "+
					"book this marginal turns on the fee, and the fee is the input here "+
					"with the least evidence behind it.",
				money(b.TotalCost), money(gross), b.TotalCost/gross*100)
		}
	}

	if hard := b.HardNames(); len(hard) > 0 {
		add(SeverityWarning, "the edge is in the names that are hardest to borrow",
			"%s %s shorted at a special rate (%s). A name is on special because the "+
				"locate is scarce, which is also the reason it is mispriced — the "+
				"strategy is being paid for taking a risk the borrow desk is already "+
				"charging for.",
			plural(len(hard), "name"), verb(len(hard), "was", "were"), joinList(hard))
	} else if b.TotalCost > 0 && !b.PerName {
		// The rate quoted is the one actually charged, which is the
		// schedule's general collateral figure when there is a schedule
		// setting only that, and the cost model's otherwise.
		rate, _ := res.Spec.Costs.Borrow.Rate("", res.Spec.Costs.ShortBorrowAnnualPct)
		add(SeverityNote, "every short was charged the same rate",
			"%s of borrow was charged at a flat %.1f%% a year across every name. Real "+
				"borrow is per-name and the crowded shorts cost multiples of general "+
				"collateral, so this is the friction understated where a short strategy "+
				"most expects its edge. Supply a borrow file to price the names "+
				"individually.",
			money(b.TotalCost), rate*100)
	}

	if len(b.LowPricedShorts) > 0 && !b.PerName {
		add(SeverityNote, "shorts in names that may not have been borrowable",
			"%s %s shorted below $%.0f a share (%s). That is the price band "+
				"hard-to-borrow names sit in, and no locate was checked here: the "+
				"backtest assumed every one of them was available. Worth confirming "+
				"before believing the short side of this.",
			plural(len(b.LowPricedShorts), "name"),
			verb(len(b.LowPricedShorts), "was", "were"), pennyShortPrice,
			joinList(b.LowPricedShorts))
	}
}

// verb picks the form of a verb that agrees with a count. A finding is meant
// to be read, and "1 name were shorted" reads as a machine talking.
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
	if len(items) > 6 {
		return strings.Join(items[:6], ", ") +
			fmt.Sprintf(" and %d more", len(items)-6)
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// borrowPerPeriod is the fee for holding one dollar short for one bar.
func borrowPerPeriod(annual, periods float64) float64 {
	if periods <= 0 || annual <= 0 || math.IsNaN(annual) || math.IsInf(annual, 0) {
		return 0
	}
	return annual / periods
}
