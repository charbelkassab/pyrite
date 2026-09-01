package engine

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/charbelkassab/pyrite/internal/market"
)

const (
	// minDiffSessions is the shortest paired sample worth a t-statistic.
	//
	// The same floor the factor regression uses, and for the same reason:
	// below this the standard error is so wide that every comparison comes
	// back insignificant, which a reader takes for a finding when it is only
	// a short sample.
	minDiffSessions = 60

	// defaultDiffBootstraps is the number of stationary-bootstrap resamples
	// behind the interval on the Sharpe difference. A thousand puts the
	// standard error of a percentile near the tail at roughly a hundredth of
	// a Sharpe point, which is finer than any decision made from it.
	defaultDiffBootstraps = 1000
	// defaultDiffBlock is the mean block length, in bars: the same month the
	// equity-curve bootstrap and the reality check use, long enough to carry
	// the volatility clustering in daily returns through the resample.
	defaultDiffBlock = 21
)

// DiffSide is one strategy's headline numbers, as they appear side by side.
type DiffSide struct {
	Name string `json:"name"`
	// TotalReturn is the cumulative fraction over the run.
	TotalReturn float64 `json:"total_return"`
	CAGR        float64 `json:"cagr"`
	Sharpe      Ratio   `json:"sharpe"`
	// MaxDrawdown is the worst peak-to-trough decline, negative.
	MaxDrawdown float64 `json:"max_drawdown"`
	// Trades is realising fills, the same figure the run report calls trades.
	Trades int `json:"trades"`
	// Turnover is dollars traded per dollar of equity per year.
	Turnover float64 `json:"turnover"`
	// Universe is what the strategy declared it could trade. It is reported
	// because in this engine a strategy sets its own universe from setup(),
	// so two sides of a comparison can legitimately differ on it.
	Universe []string `json:"universe,omitempty"`
}

// PairedTest is the test of whether B's per-session returns exceed A's.
//
// It is paired — one observation per session, B's return minus A's — rather
// than a two-sample test of the two return distributions, and that choice is
// the crux of the whole command. Both strategies trade the same market on the
// same days, so most of what moves either of them is the market moving both at
// once. A two-sample test has no way to know that: it counts the shared
// movement as noise inside each series, and the difference being tested then
// has to clear a bar built almost entirely out of something neither strategy
// caused. Subtracting first cancels it, and what is left is the part of the
// two that is not common, which is the part the comparison is about.
//
// The arithmetic says the same thing. The variance of the difference is
// var(A) + var(B) - 2cov(A, B); an unpaired test drops the covariance term.
// At a correlation of 0.9 between two strategies of similar volatility, that
// term is most of what is there, and dropping it inflates the standard error
// by a factor of about three — enough to hide any difference an author is
// likely to have made.
type PairedTest struct {
	// Sessions is the number of paired observations.
	Sessions int `json:"sessions"`
	// MeanDifference is the mean of B's return minus A's, per bar.
	MeanDifference float64 `json:"mean_difference"`
	// AnnualDifference is that mean annualised through Scale, which is the
	// figure the verdict quotes in points a year.
	AnnualDifference float64 `json:"annual_difference"`
	// AnnualStdErr is the Newey-West standard error of the mean difference,
	// annualised by the same factor so that it stands beside
	// AnnualDifference and can be read against it. The t-statistic is
	// unaffected: scaling numerator and denominator alike cancels.
	AnnualStdErr Ratio `json:"annual_std_err"`
	// TStat is the mean difference over its standard error, and PValue its
	// two-sided normal tail.
	TStat  Ratio `json:"t_stat"`
	PValue Ratio `json:"p_value"`
	// NeweyWestLag is the Bartlett bandwidth used, in bars.
	NeweyWestLag int `json:"newey_west_lag"`
	// NaiveTStat is the same statistic with the textbook independent-sample
	// standard error. It sits next to TStat because the gap between the two
	// is the size of the error the correction removes, and that error is
	// always in the flattering direction.
	NaiveTStat Ratio `json:"naive_t_stat"`
	// WinShare is the fraction of sessions on which B strictly beat A. Ties
	// count for neither side, so two identical strategies score zero here and
	// Diff.Identical is what says why.
	WinShare Ratio `json:"win_share"`
}

// SharpeGap is the difference in annualised Sharpe, with a bootstrap interval.
type SharpeGap struct {
	// Difference is B's Sharpe minus A's, both annualised.
	Difference Ratio `json:"difference"`
	// CILow and CIHigh are the 2.5th and 97.5th percentiles of the
	// resampled difference. An interval that spans zero means the ranking of
	// the two Sharpes is not stable under resampling, whatever the point
	// estimate says.
	CILow  Ratio `json:"ci_low"`
	CIHigh Ratio `json:"ci_high"`
	// Bootstraps, BlockLength and Seed are recorded so the interval can be
	// reproduced exactly rather than approximately.
	Bootstraps  int   `json:"bootstraps"`
	BlockLength int   `json:"block_length"`
	Seed        int64 `json:"seed"`
}

// Overlap measures how much of the two strategies is one strategy.
//
// This is often the most useful thing the command says. An author who has
// changed a threshold and gained two points of return has usually not built a
// second strategy; they have built the same one with a slightly different
// entry, and the two numbers they are choosing between are two draws from one
// process. A correlation of 0.98 says so in a way a return figure cannot.
type Overlap struct {
	// Correlation is Pearson's, on the paired per-session returns.
	Correlation Ratio `json:"correlation"`
	// SameHoldings is the fraction of paired sessions that ended with both
	// strategies holding the same symbols on the same side.
	SameHoldings Ratio `json:"same_holdings"`
	// HoldingSessions is how many sessions that fraction was measured over.
	// Zero means the runs did not keep per-session records, which a sweep
	// turns off.
	HoldingSessions int `json:"holding_sessions"`
}

// Diff is the answer to the question a strategy author asks more often than
// any other: is B actually better than A, or is the gap noise.
//
// The headline comparison — 14% against 11%, so the first one wins — is wrong
// often enough to be the default way of being wrong. Over a few hundred
// sessions a three-point gap between two related strategies is routinely
// indistinguishable from chance, and every statistic here exists to say so
// when it is true.
type Diff struct {
	A DiffSide `json:"a"`
	B DiffSide `json:"b"`

	// Sessions is the number of paired return observations: one fewer than
	// the number of dates both runs traded.
	Sessions int `json:"sessions"`
	// From and To are the first and last paired session. They are the period
	// every statistic below is about, which is not necessarily the period
	// either run was asked for.
	From market.Day `json:"from,omitempty"`
	To   market.Day `json:"to,omitempty"`
	// UnpairedA and UnpairedB count sessions one run had and the other did
	// not. They are dropped from every statistic here, and reported because a
	// large count means the two runs are not quite over the same period.
	UnpairedA int `json:"unpaired_a"`
	UnpairedB int `json:"unpaired_b"`

	// Identical is set when every paired difference is exactly zero.
	Identical bool `json:"identical"`

	Paired  PairedTest `json:"paired"`
	Sharpe  SharpeGap  `json:"sharpe"`
	Overlap Overlap    `json:"overlap"`

	// Findings are the critique of the comparison itself, in the same shape
	// the critique of a single run uses.
	Findings []Finding `json:"findings"`
	// Verdict is the plain-English reading of the numbers above.
	Verdict string `json:"verdict"`
}

// DiffOptions are the knobs on the comparison.
type DiffOptions struct {
	// Bootstraps and BlockLength govern the interval on the Sharpe
	// difference; zero takes the defaults.
	Bootstraps  int
	BlockLength int
	// Seed makes that interval reproducible. It is not optional in spirit:
	// an unseeded bootstrap gives a different confidence interval every time
	// it is run, which is the one thing a tool arguing about reproducibility
	// cannot do.
	Seed int64
}

func (o *DiffOptions) applyDefaults() {
	if o.Bootstraps <= 0 {
		o.Bootstraps = defaultDiffBootstraps
	}
	if o.BlockLength <= 0 {
		o.BlockLength = defaultDiffBlock
	}
	if o.Seed == 0 {
		o.Seed = 1
	}
}

// CompareRuns tests whether the difference between two completed runs is real.
//
// It refuses rather than compares when the two were not the same experiment;
// see setupMismatch for where that line is drawn.
func CompareRuns(a, b *Result, opts DiffOptions) (*Diff, error) {
	if a == nil || b == nil {
		return nil, fmt.Errorf("two completed runs are needed to compare them")
	}
	if mismatch := setupMismatch(a.Spec, b.Spec); len(mismatch) > 0 {
		return nil, fmt.Errorf("these two runs are not the same experiment, so the "+
			"difference between them is not the difference between the strategies:\n  - %s\n"+
			"Re-run both over the same setup and compare that",
			strings.Join(mismatch, "\n  - "))
	}
	opts.applyDefaults()

	sc := ScaleFor(a.Spec.Interval, a.Spec.RiskFreeRate)
	days, ra, rb, onlyA, onlyB := pairSessions(a.Curve, b.Curve)

	nan := Ratio(math.NaN())
	d := &Diff{
		A: diffSide(a), B: diffSide(b),
		Sessions:  len(ra),
		UnpairedA: onlyA, UnpairedB: onlyB,
		Paired: PairedTest{
			Sessions:     len(ra),
			AnnualStdErr: nan, TStat: nan, PValue: nan, NaiveTStat: nan, WinShare: nan,
		},
		Sharpe: SharpeGap{
			Difference: nan, CILow: nan, CIHigh: nan,
			Bootstraps: opts.Bootstraps, BlockLength: opts.BlockLength, Seed: opts.Seed,
		},
		Overlap: Overlap{Correlation: nan, SameHoldings: nan},
	}
	if len(days) > 0 {
		d.From, d.To = days[0], days[len(days)-1]
	}

	diff := make([]float64, len(ra))
	identical := len(ra) > 0
	var wins int
	for i := range ra {
		diff[i] = rb[i] - ra[i]
		if diff[i] != 0 {
			identical = false
		}
		if diff[i] > 0 {
			wins++
		}
	}
	d.Identical = identical
	if len(diff) > 0 {
		d.Paired.WinShare = Ratio(float64(wins) / float64(len(diff)))
	}

	if d.Identical {
		// Two runs of the same strategy produce the same series bar for bar,
		// and every statistic below then divides zero by zero: the Newey-West
		// variance of an identically zero series is zero, and Pearson's
		// correlation of a series with itself only lands on exactly 1 if
		// sqrt(x*x) happens to round back to x. Both answers are known
		// without computing them. This is also the case anyone checks the
		// command's calibration with, so it is answered directly rather than
		// left to arithmetic that cannot get there.
		d.Paired.AnnualStdErr = 0
		d.Paired.TStat = 0
		d.Paired.PValue = 1
		d.Paired.NaiveTStat = 0
		d.Sharpe.Difference = 0
		d.Sharpe.CILow, d.Sharpe.CIHigh = 0, 0
		d.Overlap.Correlation = 1
	} else {
		if len(diff) >= minDiffSessions {
			mean, sd := meanStdev(diff)
			d.Paired.MeanDifference = mean
			d.Paired.AnnualDifference = sc.Annualise(mean)

			// The standard error comes from regressing the difference on
			// nothing but an intercept. An intercept-only fit's coefficient is
			// the sample mean by construction, so only the standard error is
			// taken from here — but taking it from here means the sandwich and
			// the bandwidth rule are the ones the factor table already uses.
			// A second implementation of Newey and West in this package would
			// be a second answer to the same question, and the two would drift.
			lag := neweyWestLag(len(diff))
			d.Paired.NeweyWestLag = lag
			if _, se, _, _, err := fitOLS(diff, nil, lag); err == nil && se[0] > 0 {
				d.Paired.AnnualStdErr = Ratio(sc.Annualise(se[0]))
				t := mean / se[0]
				d.Paired.TStat = Ratio(t)
				// Normal rather than Student's t: at the sample lengths a
				// backtest reaches the two agree to three decimal places, and
				// the autocorrelation correction above has already moved the
				// answer by far more than the distributional choice can.
				d.Paired.PValue = Ratio(2 * (1 - normalCDF(math.Abs(t))))
			}
			if sd > 0 {
				d.Paired.NaiveTStat = Ratio(mean / (sd / math.Sqrt(float64(len(diff)))))
			}
			d.Sharpe = bootstrapSharpeGap(ra, rb, sc, opts)
		}
		if len(ra) >= 2 {
			d.Overlap.Correlation = Ratio(Correlation(ra, rb))
		}
	}

	d.Overlap.SameHoldings, d.Overlap.HoldingSessions = holdingOverlap(a.Days, b.Days)
	d.Findings = diffFindings(d, a, b)
	d.Verdict = diffVerdict(*d)
	return d, nil
}

// diffSide pulls the headline numbers off a completed run.
func diffSide(res *Result) DiffSide {
	return DiffSide{
		Name:        res.Spec.Name,
		TotalReturn: res.Metrics.TotalReturn,
		CAGR:        res.Metrics.CAGR,
		Sharpe:      res.Metrics.Sharpe,
		MaxDrawdown: res.Metrics.MaxDrawdown,
		Trades:      res.Metrics.TotalTrades,
		Turnover:    res.Metrics.Turnover,
		Universe:    res.Spec.Universe,
	}
}

// setupMismatch lists the ways two runs were not the same experiment.
//
// Everything below changes a strategy's returns without the strategy having
// done anything, and no statistic further down can tell the two causes apart.
// Five basis points of extra slippage on one side, or a start date a month
// earlier, produces a difference that looks exactly like an edge. So the
// comparison is refused rather than reported with a caveat nobody reads.
//
// The universe, the index, the warm-up and the shorting permission are
// deliberately not on this list. In this engine a strategy declares all four
// itself — through ctx.universe() and ctx.warmup() in setup(), and through the
// header of the file it came from — so they are part of what the strategy is
// rather than part of the harness it ran in. Refusing to compare a SPY
// strategy against a basket one, or a long-only rule against a pairs trade,
// would refuse most of the comparisons anybody wants to make. They are
// reported as findings instead, because they do mean part of the gap is
// something other than the rules.
func setupMismatch(a, b Spec) []string {
	a.ApplyDefaults()
	b.ApplyDefaults()

	var out []string
	add := func(what, x, y string) {
		if x != y {
			out = append(out, fmt.Sprintf("%s: %s against %s", what, x, y))
		}
	}
	add("period", fmt.Sprintf("%s to %s", a.Start, a.End), fmt.Sprintf("%s to %s", b.Start, b.End))
	add("bar size", string(a.Interval), string(b.Interval))
	add("fill model", string(a.Fill), string(b.Fill))
	add("starting capital", fmt.Sprintf("%.2f", a.InitialCash), fmt.Sprintf("%.2f", b.InitialCash))
	add("maximum leverage", fmt.Sprintf("%.2f", a.MaxLeverage), fmt.Sprintf("%.2f", b.MaxLeverage))
	add("risk-free rate", fmt.Sprintf("%.4f", a.RiskFreeRate), fmt.Sprintf("%.4f", b.RiskFreeRate))
	add("fractional shares", fmt.Sprintf("%t", a.AllowFractional), fmt.Sprintf("%t", b.AllowFractional))

	// Costs are broken out field by field rather than compared as a struct,
	// because "the cost models differ" leaves the reader to find which of
	// seven numbers moved.
	ca, cb := a.Costs, b.Costs
	add("slippage", fmt.Sprintf("%.2f bps", ca.SlippageBps), fmt.Sprintf("%.2f bps", cb.SlippageBps))
	add("commission per share", fmt.Sprintf("%.4f", ca.CommissionPerShare), fmt.Sprintf("%.4f", cb.CommissionPerShare))
	add("commission rate", fmt.Sprintf("%.5f", ca.CommissionPct), fmt.Sprintf("%.5f", cb.CommissionPct))
	add("commission minimum", fmt.Sprintf("%.2f", ca.CommissionMin), fmt.Sprintf("%.2f", cb.CommissionMin))
	add("short borrow rate", fmt.Sprintf("%.4f", ca.ShortBorrowAnnualPct), fmt.Sprintf("%.4f", cb.ShortBorrowAnnualPct))
	add("cash interest", fmt.Sprintf("%.4f", ca.CashAnnualPct), fmt.Sprintf("%.4f", cb.CashAnnualPct))
	add("market impact", fmt.Sprintf("%.2f", ca.ImpactCoefficient), fmt.Sprintf("%.2f", cb.ImpactCoefficient))
	return out
}

// pairSessions aligns two equity curves on date and returns the per-session
// returns over the dates both runs traded.
//
// Returns are measured between consecutive common dates on both sides, so a
// session one run is missing spans the same interval in both series rather
// than putting a one-day move next to a two-day one.
func pairSessions(a, b []EquityPoint) (days []market.Day, ra, rb []float64, onlyA, onlyB int) {
	bv := make(map[market.Day]float64, len(b))
	for _, p := range b {
		bv[p.Date] = p.Value
	}
	seenA := make(map[market.Day]struct{}, len(a))
	for _, p := range a {
		seenA[p.Date] = struct{}{}
	}

	type pair struct {
		d    market.Day
		x, y float64
	}
	common := make([]pair, 0, len(a))
	for _, p := range a {
		v, ok := bv[p.Date]
		if !ok {
			onlyA++
			continue
		}
		common = append(common, pair{p.Date, p.Value, v})
	}
	for _, p := range b {
		if _, ok := seenA[p.Date]; !ok {
			onlyB++
		}
	}
	// The curves arrive in date order, but sorting makes that a property of
	// this function rather than an assumption about its callers, and the
	// pairing is the one place an ordering bug would be invisible.
	sort.Slice(common, func(i, j int) bool { return common[i].d < common[j].d })
	if len(common) < 2 {
		return nil, nil, nil, onlyA, onlyB
	}
	for i := 1; i < len(common); i++ {
		if common[i-1].x <= 0 || common[i-1].y <= 0 {
			continue
		}
		days = append(days, common[i].d)
		ra = append(ra, common[i].x/common[i-1].x-1)
		rb = append(rb, common[i].y/common[i-1].y-1)
	}
	return days, ra, rb, onlyA, onlyB
}

// bootstrapSharpeGap is the difference in annualised Sharpe with a percentile
// confidence interval from a stationary bootstrap.
//
// Two things about the resampling matter more than the interval itself.
//
// The blocks are Politis and Romano's geometric ones rather than fixed ones,
// for the reason stationaryIndices already gives: this is inference about a
// sampling distribution, and blocks anchored to a fixed grid are not
// stationary, so observations near a boundary are sampled differently from
// observations in the middle.
//
// And both series are resampled on the same draw of dates. Drawing them
// separately would break the pairing, discard the covariance between the two
// strategies and widen the interval until nothing could ever be
// distinguished — the two-sample mistake again, in bootstrap form.
func bootstrapSharpeGap(ra, rb []float64, sc Scale, opts DiffOptions) SharpeGap {
	nan := Ratio(math.NaN())
	g := SharpeGap{
		Difference: nan, CILow: nan, CIHigh: nan,
		Bootstraps: opts.Bootstraps, BlockLength: opts.BlockLength, Seed: opts.Seed,
	}
	sa, sb := annualSharpe(ra, sc), annualSharpe(rb, sc)
	if math.IsNaN(sa) || math.IsNaN(sb) {
		return g
	}
	g.Difference = Ratio(sb - sa)

	rng := newLCG(opts.Seed)
	idx := make([]int, len(ra))
	xa := make([]float64, len(ra))
	xb := make([]float64, len(rb))
	draws := make([]float64, 0, opts.Bootstraps)
	for i := 0; i < opts.Bootstraps; i++ {
		stationaryIndices(idx, float64(opts.BlockLength), rng)
		for j, k := range idx {
			xa[j], xb[j] = ra[k], rb[k]
		}
		da, db := annualSharpe(xa, sc), annualSharpe(xb, sc)
		if math.IsNaN(da) || math.IsNaN(db) {
			continue
		}
		draws = append(draws, db-da)
	}
	// A resample with no variation on one side has no Sharpe, and a handful
	// of those changes nothing. Losing half of them means the series is
	// degenerate and the percentiles would be read off whatever survived.
	if len(draws) < opts.Bootstraps/2 {
		return g
	}
	sort.Float64s(draws)
	g.CILow = Ratio(percentileSorted(draws, 0.025))
	g.CIHigh = Ratio(percentileSorted(draws, 0.975))
	return g
}

// annualSharpe is the run's Sharpe over a return series, through Scale so an
// intraday comparison is not annualised as though it were daily.
func annualSharpe(rets []float64, sc Scale) float64 {
	if len(rets) < 2 {
		return math.NaN()
	}
	mean, sd := meanStdev(rets)
	return sc.Sharpe(mean, sd)
}

// holdingOverlap is the fraction of sessions the two runs ended holding the
// same thing.
//
// "The same thing" is the set of symbols held and the side held on, not the
// weights. Two strategies with 99% and 95% of the book in SPY are holding the
// same position, and an overlap measure that called those different would
// report near-zero agreement for two strategies differing only in their
// sizing, which is the opposite of what it is for. Both flat counts as
// agreement, because it is.
func holdingOverlap(a, b []DayRecord) (Ratio, int) {
	if len(a) == 0 || len(b) == 0 {
		return Ratio(math.NaN()), 0
	}
	other := make(map[market.Day]string, len(b))
	for _, r := range b {
		other[r.Date] = holdingKey(r.Positions)
	}
	var same, n int
	for _, r := range a {
		k, ok := other[r.Date]
		if !ok {
			continue
		}
		n++
		if holdingKey(r.Positions) == k {
			same++
		}
	}
	if n == 0 {
		return Ratio(math.NaN()), 0
	}
	return Ratio(float64(same) / float64(n)), n
}

// holdingKey renders a day's book as a canonical string. Sorted, because the
// position list arrives in whatever order the portfolio produced it and two
// identical books must not compare unequal on that account.
func holdingKey(ps []PositionSnapshot) string {
	keys := make([]string, 0, len(ps))
	for _, p := range ps {
		if p.Shares == 0 {
			continue
		}
		side := "+"
		if p.Shares < 0 {
			side = "-"
		}
		keys = append(keys, side+p.Symbol)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// diffFindings criticises the comparison itself.
func diffFindings(d *Diff, a, b *Result) []Finding {
	var out []Finding
	add := func(sev Severity, title, format string, args ...any) {
		out = append(out, Finding{sev, title, fmt.Sprintf(format, args...)})
	}

	// --- Is there a difference here at all? ------------------------------

	// The thresholds below are set on what a correlation implies about the
	// difference series rather than on how large the number sounds. For two
	// strategies of similar volatility the difference carries 2(1-rho) of
	// either one's variance, so 0.99 leaves 2% of it and 0.95 leaves 10%: at
	// the first there is almost nothing left to test, at the second there is
	// little. Below that they are genuinely different strategies, however
	// alike the equity curves look on a chart.
	switch c := d.Overlap.Correlation; {
	case d.Identical:
		add(SeverityCritical, "you did not actually change anything",
			"Both strategies returned exactly the same amount on all %d paired sessions. "+
				"Whatever differs between them never reached a trade, so there is no "+
				"difference here to be significant or otherwise.", d.Sessions)
	case c.Defined() && float64(c) >= 0.99:
		add(SeverityCritical, "these are one strategy with two names",
			"The two return series correlate at %.3f%s. At that correlation the "+
				"difference between them carries about %.0f%% of either strategy's "+
				"variance: they are two draws from one process, and choosing between "+
				"them on a return figure is choosing between two samples of the same thing.",
			float64(c), holdingClause(d.Overlap), 2*(1-float64(c))*100)
	case c.Defined() && float64(c) >= 0.95:
		add(SeverityWarning, "the two are mostly the same strategy",
			"The two return series correlate at %.2f%s, so most of what separates them "+
				"is the same trade on the same day. Expect any difference to be small "+
				"relative to how noisily it is measured.",
			float64(c), holdingClause(d.Overlap))
	}

	// --- Is there enough evidence to be testing it? ----------------------

	if d.Sessions > 0 && d.Sessions < minDiffSessions {
		add(SeverityCritical, "too few paired sessions to compare anything",
			"%d sessions. A difference in mean return over this many observations has a "+
				"standard error wider than any edge either strategy could plausibly have.",
			d.Sessions)
	}
	if d.Sessions == 0 {
		add(SeverityCritical, "the two runs share no sessions",
			"There is no date on which both strategies produced a return, so nothing "+
				"about them can be compared.")
	}
	if u := d.UnpairedA + d.UnpairedB; u > 0 && d.Sessions > 0 {
		// Unpaired sessions are normal when the two trade different symbols,
		// because the calendar is the union of what their universes have data
		// for. They stop being harmless once they are a real slice of the
		// sample: the two runs are then measured over different periods, and
		// the difference partly reflects which days each one saw.
		share := float64(u) / float64(d.Sessions+u)
		sev := SeverityNote
		if share > 0.05 {
			sev = SeverityWarning
		}
		add(sev, "the two runs did not trade the same sessions",
			"%d sessions appear in one run and not the other (%.1f%% of the union), and "+
				"are dropped from every statistic here. %d belong to A and %d to B.",
			u, share*100, d.UnpairedA, d.UnpairedB)
	}

	// --- Is the comparison about the strategies? -------------------------

	if !sameSymbolSet(a.Spec.Universe, b.Spec.Universe) || a.Spec.Index != b.Spec.Index {
		add(SeverityWarning, "the two strategies trade different universes",
			"A trades %s; B trades %s. Part of the difference reported here is the "+
				"universe rather than the rules, and nothing in this comparison can "+
				"separate the two. Run both over the same --universe if that is the question.",
			describeUniverse(a.Spec), describeUniverse(b.Spec))
	}
	if a.Spec.AllowShort != b.Spec.AllowShort {
		long, short := "A", "B"
		if a.Spec.AllowShort {
			long, short = "B", "A"
		}
		add(SeverityWarning, "only one of the two was allowed to short",
			"%s may take short positions and %s may not. That is a difference in what "+
				"the two were permitted to do rather than in what they decided, and it "+
				"shows up in the return difference the same way an edge would.",
			short, long)
	}
	if a.StrategyErrors > 0 || b.StrategyErrors > 0 {
		add(SeverityWarning, "one of the strategies threw during the run",
			"onDay() failed on %d of A's sessions and %d of B's. Those days did nothing, "+
				"so part of what is being compared is a strategy that was absent.",
			a.StrategyErrors, b.StrategyErrors)
	}
	if !a.Manifest.Reproducible() || !b.Manifest.Reproducible() {
		add(SeverityWarning, "at least one side is not reproducible",
			"A run that called a model and missed cache cannot be re-run to the same "+
				"numbers, so the difference measured here is one draw and re-running it "+
				"would give another.")
	}

	sort.SliceStable(out, func(i, j int) bool {
		return severityRank(out[i].Severity) < severityRank(out[j].Severity)
	})
	return out
}

// holdingClause adds the book overlap to a correlation finding when it was
// measured, because "they held the same thing on 97% of days" is the sentence
// a reader believes and a correlation is the one they argue with.
func holdingClause(o Overlap) string {
	if !o.SameHoldings.Defined() {
		return ""
	}
	return fmt.Sprintf(" and held the same book on %.0f%% of %d sessions",
		float64(o.SameHoldings)*100, o.HoldingSessions)
}

// describeUniverse names what a spec could trade, in the shortest form that is
// still specific.
func describeUniverse(s Spec) string {
	if s.Index != "" {
		return s.Index + " membership"
	}
	switch n := len(s.Universe); {
	case n == 0:
		return "nothing declared"
	case n <= 4:
		return strings.Join(s.Universe, ", ")
	default:
		return fmt.Sprintf("%s and %d more", strings.Join(s.Universe[:3], ", "), n-3)
	}
}

// sameSymbolSet compares two universes as sets, since order carries no meaning.
func sameSymbolSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// diffVerdict is the sentence the command exists to produce.
func diffVerdict(d Diff) string {
	switch {
	case d.Identical:
		return fmt.Sprintf("A and B returned exactly the same amount on all %d sessions — "+
			"whatever was changed between them never reached a trade", d.Sessions)
	case d.Sessions == 0:
		return "the two runs share no sessions, so there is nothing to compare"
	case d.Sessions < minDiffSessions:
		return fmt.Sprintf("%d paired sessions is too short a sample to compare two "+
			"strategies over; nothing here is a finding", d.Sessions)
	case !d.Paired.TStat.Defined():
		return "the difference between the two return series has no measurable standard " +
			"error, so nothing can be said about it"
	}

	t := math.Abs(float64(d.Paired.TStat))
	pts := d.Paired.AnnualDifference * 100
	ahead := "B"
	if pts < 0 {
		ahead, pts = "A", -pts
	}
	head := fmt.Sprintf("%s returns %.1f points more a year, with a t-statistic of %.1f on the difference",
		ahead, pts, t)
	// Two is the conventional two-sided 5% cut, and it is already generous
	// here: an author comparing strategies has almost always compared several,
	// and a single t-statistic has no way to know how many. Three is roughly
	// where a difference survives that too.
	switch {
	case t < 2:
		head += " — that is not a difference you can act on"
	case t < 3:
		head += " — enough to notice, not enough to act on, and it is one comparison out of however many were run to arrive at it"
	default:
		head += ", which survives the correction for autocorrelation"
	}
	parts := []string{head}

	// The gap between the corrected and the uncorrected statistic is worth
	// stating only when it would have changed the reading. Below about 15%
	// the two lead to the same decision and the remark is noise.
	if n := d.Paired.NaiveTStat; n.Defined() && math.Abs(float64(n)) > t*1.15 {
		parts = append(parts, fmt.Sprintf(
			"treating the sessions as independent draws would have given %.1f instead, which is the "+
				"size of the error the autocorrelation correction removes", math.Abs(float64(n))))
	}

	spansZero := false
	if d.Sharpe.CILow.Defined() && d.Sharpe.CIHigh.Defined() {
		lo, hi := float64(d.Sharpe.CILow), float64(d.Sharpe.CIHigh)
		spansZero = lo <= 0 && hi >= 0
		s := fmt.Sprintf("the Sharpe difference is %.2f with a 95%% bootstrap interval of %.2f to %.2f",
			float64(d.Sharpe.Difference), lo, hi)
		if spansZero {
			s += ", which contains zero"
		}
		parts = append(parts, s)
	}
	if c := d.Overlap.Correlation; c.Defined() {
		parts = append(parts, fmt.Sprintf("the two return series correlate at %.2f%s",
			float64(c), holdingClause(d.Overlap)))
	}
	// The two tests can disagree, and when they do the weaker reading is the
	// one to take: a difference that clears one and not the other has not
	// cleared anything. Same habit as the reality check.
	if t >= 2 && spansZero {
		parts = append(parts, "the t-statistic and the bootstrap interval disagree, and a "+
			"difference that clears one test and not the other has not cleared anything")
	}
	return strings.Join(parts, "; ")
}
