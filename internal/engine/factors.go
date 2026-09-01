package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/charbelkassab/pyrite/internal/market"
)

// minFactorObservations is the shortest run worth regressing.
//
// Below this the standard errors are so wide that every t-statistic comes back
// insignificant, which reads as a finding when it is only a short sample.
const minFactorObservations = 60

// FactorProxy is a tradable stand-in for a risk factor, expressed as one ETF
// minus another.
//
// The academic factor series are built from the whole cross-section of listed
// equities, rebalanced monthly, with no fees and no tracking error. These are
// two funds subtracted from one another. The direction of each spread is the
// same, and for the purpose this serves — deciding whether an apparent edge is
// a known exposure wearing a disguise — that is enough. The precision is not
// the same, and nothing here should be read as though it were.
type FactorProxy struct {
	Name string
	// Long is the fund on the long side of the spread.
	Long string
	// Short is subtracted from Long. Empty means the risk-free rate, which
	// is what makes the market factor an excess return rather than a spread.
	Short string
	// Captures says what the spread is standing in for.
	Captures string
}

// Label renders the spread the way it is displayed.
func (p FactorProxy) Label() string {
	if p.Short == "" {
		return p.Long + " - rf"
	}
	return p.Long + " - " + p.Short
}

// symbols lists the funds this proxy needs.
func (p FactorProxy) symbols() []string {
	if p.Short == "" {
		return []string{p.Long}
	}
	return []string{p.Long, p.Short}
}

// DefaultFactorProxies is the set the tool regresses against.
//
// Five factors and roughly fifteen hundred daily bars is already a generous
// ratio of parameters to data; adding more would widen every standard error
// for the sake of exposures that overlap what is here.
var DefaultFactorProxies = []FactorProxy{
	{Name: "Market", Long: "SPY", Captures: "the equity market itself, in excess of cash"},
	{Name: "Size", Long: "IWM", Short: "SPY", Captures: "small companies over large ones"},
	{Name: "Value", Long: "IWD", Short: "IWF", Captures: "cheap companies over expensive ones"},
	{Name: "Momentum", Long: "MTUM", Short: "SPY", Captures: "recent winners over the index"},
	// USMV tracks minimum volatility, which overlaps the quality premium
	// without being it. Named for what the fund actually holds rather than
	// for the factor it is nearest to.
	{Name: "Low volatility", Long: "USMV", Short: "SPY", Captures: "defensive, low-volatility names over the index"},
}

// FactorProxyNote travels with every factor result, in every output.
//
// Overstating the precision of these numbers would be the same dishonesty the
// rest of the tool exists to catch, so the caveat is a field rather than a
// line of documentation somebody might not read.
const FactorProxyNote = "These factors are ETF spreads, not the Fama-French or AQR series. " +
	"The funds charge fees, hold a few hundred names rather than the whole cross-section, " +
	"and rebalance on their own schedule, so a loading here points the same way as the " +
	"academic factor without being the same number."

// FactorLoading is one factor's regression coefficient.
type FactorLoading struct {
	Name  string `json:"name"`
	Proxy string `json:"proxy"`
	// Captures repeats what the spread stands for, so a reader of the JSON
	// does not have to know what IWD minus IWF means.
	Captures string `json:"captures,omitempty"`
	Beta     Ratio  `json:"beta"`
	StdErr   Ratio  `json:"std_error"`
	// TStat is Beta over StdErr. Below about 2 in absolute value the loading
	// is not distinguishable from no exposure at all.
	TStat Ratio `json:"t_stat"`
}

// Significant reports whether the loading clears the conventional bar.
func (l FactorLoading) Significant() bool {
	return l.TStat.Defined() && math.Abs(float64(l.TStat)) >= 2
}

// DroppedFactor is a factor that could not be built, and why not.
//
// A missing proxy is reported rather than silently omitted: "we did not
// measure momentum" and "there is no momentum exposure" are different
// statements, and a table that shows four rows instead of five without saying
// so lets a reader take the second one for the first.
type DroppedFactor struct {
	Name   string `json:"name"`
	Proxy  string `json:"proxy"`
	Reason string `json:"reason"`
}

// FactorExposure is what is left of a strategy once known risk premia are
// taken out of it.
//
// Most apparent edges are an exposure that already has a name. A strategy
// returning 20% a year against a market beta of 1.6 has produced nothing a
// margin loan and an index fund would not have; one whose returns load on
// momentum has rediscovered momentum. The number that matters is the
// intercept and its t-statistic: what the strategy earned that the factors do
// not account for, and whether that amount is distinguishable from zero.
type FactorExposure struct {
	Factors []FactorLoading `json:"factors"`
	Dropped []DroppedFactor `json:"dropped,omitempty"`

	// Alpha is the regression intercept, annualised by the run's Scale.
	Alpha Ratio `json:"alpha"`
	// AlphaStdErr is annualised alongside Alpha, so the ratio of the two is
	// the same t-statistic either way.
	AlphaStdErr Ratio `json:"alpha_std_error"`
	// AlphaTStat is the whole point of this analysis. Below about 2 the
	// alpha is indistinguishable from zero however large it looks.
	AlphaTStat Ratio `json:"alpha_t_stat"`

	// RSquared is how much of the strategy's variance the factors explain.
	// Near 1 means the strategy is a repackaging of them.
	RSquared    Ratio `json:"r_squared"`
	AdjRSquared Ratio `json:"adj_r_squared"`

	Observations int `json:"observations"`
	// AvgExposure is the strategy's mean gross exposure over the same bars.
	//
	// It is here because a market beta only means what a reader assumes it
	// means when it is read alongside this. A beta of 0.2 from a book that
	// was fully invested throughout is a market-neutral strategy; the same
	// beta from one that held an index fund half the time is a timing rule
	// that happened to sit out the volatile sessions. The regression cannot
	// tell those two apart and this number can.
	AvgExposure Ratio `json:"avg_exposure"`
	// NeweyWestLag is the Bartlett bandwidth used for the standard errors.
	NeweyWestLag int `json:"newey_west_lag"`
	// PeriodsPerYear records what the alpha was annualised by, so a result
	// computed on 5-minute bars cannot be mistaken for a daily one.
	PeriodsPerYear float64 `json:"periods_per_year"`

	// ProxyNote is the caveat about the factor construction.
	ProxyNote string `json:"proxy_note"`
	// Verdict is the plain-English reading of the numbers above.
	Verdict string `json:"verdict"`
}

// FactorSeries is one factor's per-bar returns, aligned to the strategy's.
type FactorSeries struct {
	Name     string
	Proxy    string
	Captures string
	Values   []float64
}

// AnalyseFactors decomposes an equity curve against the ETF factor proxies.
//
// proxies may be nil, in which case DefaultFactorProxies is used. A proxy
// whose funds have no data over the period is dropped and reported rather
// than failing the whole analysis, because a run starting before a fund
// existed is a normal thing to ask for.
func AnalyseFactors(ctx context.Context, curve []EquityPoint, store *market.Store, iv market.Interval, sc Scale, proxies []FactorProxy) (*FactorExposure, error) {
	if store == nil {
		return nil, errors.New("factor analysis needs a market data store to fetch the ETF proxies from")
	}
	if len(curve) < minFactorObservations {
		return nil, fmt.Errorf("factor analysis needs at least %d bars of equity curve and this run has %d: widen the date range with --from and --to",
			minFactorObservations, len(curve))
	}
	if len(proxies) == 0 {
		proxies = DefaultFactorProxies
	}

	from, to := curve[0].Date, curve[len(curve)-1].Date
	prices, fetchErr := loadProxyPrices(ctx, store, proxies, from, to, iv)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Decide which factors survive before building anything, so that a fund
	// with a handful of stray bars does not quietly halve the sample.
	var kept []FactorProxy
	var dropped []DroppedFactor
	for _, p := range proxies {
		reason := ""
		for _, sym := range p.symbols() {
			series := prices[sym]
			if series == nil {
				reason = fmt.Sprintf("no data for %s over this period", sym)
				if e := fetchErr[sym]; e != nil {
					reason = fmt.Sprintf("%s could not be fetched: %s", sym, truncateErr(e.Error()))
				}
				break
			}
			if r := coverageShortfall(series, curve); r != "" {
				reason = r
				break
			}
		}
		if reason != "" {
			dropped = append(dropped, DroppedFactor{Name: p.Name, Proxy: p.Label(), Reason: reason})
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("none of the %d factor proxies has usable data between %s and %s, so there is nothing to regress against: try a later start date, or a shorter list of proxies",
			len(proxies), from, to)
	}

	excess, series, exposure := alignFactorReturns(curve, kept, prices, sc)
	if len(excess) < minFactorObservations {
		return nil, fmt.Errorf("only %d bars are shared between the strategy and the factor proxies, and the regression needs %d: the funds and the strategy do not overlap enough over %s to %s",
			len(excess), minFactorObservations, from, to)
	}

	f, err := FitFactorModel(excess, series, sc)
	if err != nil {
		return nil, err
	}
	f.Dropped = dropped
	f.AvgExposure = exposure
	f.Verdict = factorVerdict(f)
	return f, nil
}

// FitFactorModel regresses per-bar excess returns on factor returns.
//
// This is the whole analysis with the data plumbing removed, so it can be
// exercised on series whose right answer is known.
func FitFactorModel(excess []float64, factors []FactorSeries, sc Scale) (*FactorExposure, error) {
	n := len(excess)
	k := len(factors) + 1 // the intercept is a column too
	if len(factors) == 0 {
		return nil, errors.New("a factor regression with no factors is just a mean: supply at least one factor series")
	}
	if n < minFactorObservations {
		return nil, fmt.Errorf("factor regression needs at least %d observations and got %d",
			minFactorObservations, n)
	}
	if n <= k+10 {
		return nil, fmt.Errorf("%d observations cannot support %d factors plus an intercept: either lengthen the period or regress against fewer factors",
			n, len(factors))
	}
	for _, fs := range factors {
		if len(fs.Values) != n {
			return nil, fmt.Errorf("factor %q has %d observations against the strategy's %d: the two series were not aligned before fitting",
				fs.Name, len(fs.Values), n)
		}
	}

	lag := neweyWestLag(n)
	coef, se, r2, adjR2, err := fitOLS(excess, factors, lag)
	if err != nil {
		return nil, err
	}

	f := &FactorExposure{
		Observations:   n,
		NeweyWestLag:   lag,
		PeriodsPerYear: sc.Periods(),
		RSquared:       Ratio(r2),
		AdjRSquared:    Ratio(adjR2),
		ProxyNote:      FactorProxyNote,
	}
	// The intercept is a per-bar mean, so it annualises like one. Its
	// standard error is annualised by the same factor, which leaves the
	// t-statistic untouched — that is the number the reader is here for, and
	// it must not depend on how the alpha happens to be quoted.
	f.Alpha = Ratio(sc.Annualise(coef[0]))
	f.AlphaStdErr = Ratio(sc.Annualise(se[0]))
	f.AlphaTStat = Ratio(tStat(coef[0], se[0]))

	f.Factors = make([]FactorLoading, len(factors))
	for i, fs := range factors {
		f.Factors[i] = FactorLoading{
			Name:     fs.Name,
			Proxy:    fs.Proxy,
			Captures: fs.Captures,
			Beta:     Ratio(coef[i+1]),
			StdErr:   Ratio(se[i+1]),
			TStat:    Ratio(tStat(coef[i+1], se[i+1])),
		}
	}
	f.Verdict = factorVerdict(f)
	return f, nil
}

// tStat guards the division so an unestimable standard error reads as
// undefined rather than as an infinitely significant result.
func tStat(coef, se float64) float64 {
	if se <= 0 || math.IsNaN(se) || math.IsInf(se, 0) {
		return math.NaN()
	}
	return coef / se
}

// loadProxyPrices fetches every fund the proxies need, once each.
func loadProxyPrices(ctx context.Context, store *market.Store, proxies []FactorProxy, from, to market.Day, iv market.Interval) (map[string]*market.Series, map[string]error) {
	seen := map[string]bool{}
	var symbols []string
	for _, p := range proxies {
		for _, s := range p.symbols() {
			if !seen[s] {
				seen[s] = true
				symbols = append(symbols, s)
			}
		}
	}
	return store.GetManyInterval(ctx, symbols, from.Date(), to.EndOfDay(), iv)
}

// minProxyCoverage is how much of the strategy's calendar a fund must cover
// before its factor is worth reporting.
//
// A fund launched partway through the period would otherwise contribute a
// factor fitted on the tail of the run and a beta presented as though it
// applied to all of it.
const minProxyCoverage = 0.9

// coverageShortfall reports why a series cannot cover the curve, or "".
func coverageShortfall(s *market.Series, curve []EquityPoint) string {
	var have int
	for _, p := range curve {
		if bar, ok := s.At(p.Date); ok && bar.AdjClose > 0 {
			have++
		}
	}
	share := float64(have) / float64(len(curve))
	if share >= minProxyCoverage {
		return ""
	}
	if first, ok := s.First(); ok && have > 0 {
		return fmt.Sprintf("%s only has bars from %s, covering %s of this period",
			s.Symbol, first.Date, fmtPercent(share))
	}
	return fmt.Sprintf("%s has bars for only %s of this period", s.Symbol, fmtPercent(share))
}

// alignFactorReturns pairs the strategy's per-bar returns with the factors',
// on the strategy's own calendar.
//
// Both sides of every observation are measured over the same two timestamps.
// Filling a missing fund bar forward instead would enter a zero return
// against a day the strategy actually moved, which biases every beta towards
// zero and every alpha upwards — the flattering direction, as usual.
func alignFactorReturns(curve []EquityPoint, proxies []FactorProxy, prices map[string]*market.Series, sc Scale) ([]float64, []FactorSeries, Ratio) {
	series := make([]FactorSeries, len(proxies))
	for i, p := range proxies {
		series[i] = FactorSeries{Name: p.Name, Proxy: p.Label(), Captures: p.Captures}
	}
	rf := sc.PerPeriodRF()

	// ret returns a fund's return between two of the strategy's timestamps,
	// and reports false when either bar is absent.
	ret := func(sym string, prev, cur market.Day) (float64, bool) {
		s := prices[sym]
		if s == nil {
			return 0, false
		}
		a, okA := s.At(prev)
		b, okB := s.At(cur)
		if !okA || !okB || a.AdjClose <= 0 || b.AdjClose <= 0 {
			return 0, false
		}
		return b.AdjClose/a.AdjClose - 1, true
	}

	excess := make([]float64, 0, len(curve)-1)
	row := make([]float64, len(proxies))
	var grossExposure float64
	for i := 1; i < len(curve); i++ {
		if curve[i-1].Value <= 0 {
			continue
		}
		prev, cur := curve[i-1].Date, curve[i].Date
		ok := true
		for j, p := range proxies {
			long, okL := ret(p.Long, prev, cur)
			if !okL {
				ok = false
				break
			}
			if p.Short == "" {
				// The market factor is an excess return over cash, not a
				// spread between two funds.
				row[j] = long - rf
				continue
			}
			short, okS := ret(p.Short, prev, cur)
			if !okS {
				ok = false
				break
			}
			row[j] = long - short
		}
		if !ok {
			continue
		}
		excess = append(excess, curve[i].Value/curve[i-1].Value-1-rf)
		if e := curve[i].Exposure; !math.IsNaN(e) && !math.IsInf(e, 0) {
			grossExposure += e
		}
		for j := range proxies {
			series[j].Values = append(series[j].Values, row[j])
		}
	}
	exposure := Ratio(math.NaN())
	if len(excess) > 0 {
		exposure = Ratio(grossExposure / float64(len(excess)))
	}
	return excess, series, exposure
}

// neweyWestLag picks the Bartlett bandwidth for the standard errors.
//
// Daily strategy returns are not independent draws. A position held for a
// week spreads one decision over five bars, a rebalance that takes days to
// complete does the same, and volatility clusters regardless. Ordinary least
// squares assumes none of that and returns standard errors that are too
// small, which inflates every t-statistic in the flattering direction — the
// one mistake this particular table exists to avoid making.
//
// The bandwidth is Newey and West's own automatic rule, floor(4*(n/100)^(2/9)),
// which lands between five and eight bars over the sample lengths a backtest
// reaches: about a trading week, which is roughly how long a daily strategy's
// autocorrelation persists. It is clamped at both ends so a short run still
// gets some correction and a long one does not spend precision on lags that
// carry nothing.
func neweyWestLag(n int) int {
	if n < 3 {
		return 0
	}
	lag := int(4 * math.Pow(float64(n)/100, 2.0/9.0))
	if lag < 1 {
		lag = 1
	}
	if lag > 10 {
		lag = 10
	}
	if lag > n-2 {
		lag = n - 2
	}
	return lag
}

// singularError names the column that could not be separated from the rest,
// so the caller can turn it into a factor name the user recognises.
type singularError struct{ col int }

func (e singularError) Error() string {
	return fmt.Sprintf("design matrix column %d is a linear combination of the others", e.col)
}

// fitOLS regresses y on an intercept plus the given factor series, with
// Newey-West standard errors.
//
// The intercept is added here rather than by the caller, because a caller who
// forgets it gets a regression through the origin and an alpha of exactly
// zero by construction.
//
// Columns are scaled to unit root-mean-square before the elimination. That is
// not cosmetic: the intercept column sums to n while a daily return column
// sums to a few hundredths, and the four orders of magnitude between them make
// the pivot test meaningless. Scaled, every diagonal of the Gram matrix is
// exactly n, so a pivot that has collapsed relative to n is genuine
// collinearity rather than a units artefact.
func fitOLS(y []float64, factors []FactorSeries, lag int) (coef, se []float64, r2, adjR2 float64, err error) {
	n := len(y)
	k := len(factors) + 1

	// Design matrix, row-major, intercept first.
	d := make([][]float64, n)
	for t := 0; t < n; t++ {
		d[t] = make([]float64, k)
		d[t][0] = 1
		for j, f := range factors {
			v := f.Values[t]
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, nil, 0, 0, fmt.Errorf("factor %q has a non-finite value at observation %d, so the regression cannot be fitted: check the price series it was built from", f.Name, t)
			}
			d[t][j+1] = v
		}
	}
	for t := 0; t < n; t++ {
		if math.IsNaN(y[t]) || math.IsInf(y[t], 0) {
			return nil, nil, 0, 0, fmt.Errorf("the strategy's return series has a non-finite value at observation %d, so the regression cannot be fitted", t)
		}
	}

	scale := make([]float64, k)
	for j := 0; j < k; j++ {
		var ss float64
		for t := 0; t < n; t++ {
			ss += d[t][j] * d[t][j]
		}
		scale[j] = math.Sqrt(ss / float64(n))
		if scale[j] <= 0 {
			name := "the intercept"
			if j > 0 {
				name = "factor " + factors[j-1].Name
			}
			return nil, nil, 0, 0, fmt.Errorf("%s is zero at every observation, so no exposure to it can be estimated: drop it from the factor list", name)
		}
		for t := 0; t < n; t++ {
			d[t][j] /= scale[j]
		}
	}

	// Gram matrix and the moment vector, both on the scaled columns.
	gram := make([][]float64, k)
	for i := range gram {
		gram[i] = make([]float64, k)
	}
	moment := make([]float64, k)
	for t := 0; t < n; t++ {
		for i := 0; i < k; i++ {
			moment[i] += d[t][i] * y[t]
			for j := i; j < k; j++ {
				gram[i][j] += d[t][i] * d[t][j]
			}
		}
	}
	for i := 0; i < k; i++ {
		for j := i + 1; j < k; j++ {
			gram[j][i] = gram[i][j]
		}
	}

	inv, err := invert(gram, float64(n))
	if err != nil {
		var sing singularError
		if errors.As(err, &sing) {
			name := "the intercept"
			if sing.col > 0 && sing.col-1 < len(factors) {
				name = factors[sing.col-1].Name
			}
			return nil, nil, 0, 0, fmt.Errorf("the factor regression cannot separate %s from the other factors: over this period it moves with them almost exactly, so no unique set of betas exists. Drop one of the overlapping factors and run again", name)
		}
		return nil, nil, 0, 0, err
	}

	scaled := make([]float64, k)
	for i := 0; i < k; i++ {
		for j := 0; j < k; j++ {
			scaled[i] += inv[i][j] * moment[j]
		}
	}

	resid := make([]float64, n)
	var mean, sst, ssr float64
	for _, v := range y {
		mean += v
	}
	mean /= float64(n)
	for t := 0; t < n; t++ {
		fit := 0.0
		for i := 0; i < k; i++ {
			fit += d[t][i] * scaled[i]
		}
		resid[t] = y[t] - fit
		ssr += resid[t] * resid[t]
		dy := y[t] - mean
		sst += dy * dy
	}
	if sst > 0 {
		r2 = 1 - ssr/sst
		adjR2 = 1 - (1-r2)*float64(n-1)/float64(n-k)
	} else {
		r2, adjR2 = math.NaN(), math.NaN()
	}

	variance := neweyWestVariance(d, resid, inv, lag)

	coef = make([]float64, k)
	se = make([]float64, k)
	for i := 0; i < k; i++ {
		coef[i] = scaled[i] / scale[i]
		v := variance[i][i]
		if v > 0 {
			se[i] = math.Sqrt(v) / scale[i]
		} else {
			// A negative sandwich variance is possible in finite samples and
			// means nothing can be said about this coefficient's precision.
			// Reporting it as zero would make the t-statistic infinite.
			se[i] = math.NaN()
		}
	}
	return coef, se, r2, adjR2, nil
}

// neweyWestVariance builds the heteroskedasticity- and autocorrelation-
// consistent covariance matrix of the coefficients.
//
// The sandwich is (X'X)^-1 * omega * (X'X)^-1, where omega sums the
// contemporaneous outer products and then the lagged cross-products weighted
// by the Bartlett kernel. The kernel is what keeps the result positive
// semi-definite; a flat weighting of the same lags does not.
func neweyWestVariance(d [][]float64, resid []float64, inv [][]float64, lag int) [][]float64 {
	n := len(resid)
	k := len(inv)

	omega := make([][]float64, k)
	for i := range omega {
		omega[i] = make([]float64, k)
	}
	for l := 0; l <= lag; l++ {
		w := 1 - float64(l)/float64(lag+1)
		if lag == 0 {
			w = 1
		}
		for t := l; t < n; t++ {
			e := resid[t] * resid[t-l]
			if e == 0 {
				continue
			}
			for i := 0; i < k; i++ {
				for j := 0; j < k; j++ {
					term := e * d[t][i] * d[t-l][j]
					if l == 0 {
						omega[i][j] += term
					} else {
						// The kernel needs the lagged cross-product and its
						// transpose. Every ordered (i, j) pair is visited, so
						// writing each term into both cells accumulates
						// exactly that sum and nothing twice over.
						omega[i][j] += w * term
						omega[j][i] += w * term
					}
				}
			}
		}
	}

	// Small-sample correction for the degrees of freedom the fit consumed.
	dof := float64(n) / float64(n-k)

	tmp := make([][]float64, k)
	for i := range tmp {
		tmp[i] = make([]float64, k)
		for j := 0; j < k; j++ {
			var s float64
			for m := 0; m < k; m++ {
				s += inv[i][m] * omega[m][j]
			}
			tmp[i][j] = s
		}
	}
	out := make([][]float64, k)
	for i := range out {
		out[i] = make([]float64, k)
		for j := 0; j < k; j++ {
			var s float64
			for m := 0; m < k; m++ {
				s += tmp[i][m] * inv[m][j]
			}
			out[i][j] = s * dof
		}
	}
	return out
}

// invert inverts a square matrix by Gauss-Jordan elimination with partial
// pivoting.
//
// Written out rather than taken from a linear algebra package because pyrite
// ships as a single binary with no dependencies, and that property is worth
// more than the hundred lines it costs here.
//
// diag is the size every diagonal entry has when the columns are independent,
// which is what makes the pivot test a statement about collinearity rather
// than about the units the returns happen to be measured in.
func invert(a [][]float64, diag float64) ([][]float64, error) {
	n := len(a)
	m := make([][]float64, n)
	inv := make([][]float64, n)
	for i := range a {
		m[i] = append([]float64(nil), a[i]...)
		inv[i] = make([]float64, n)
		inv[i][i] = 1
	}
	if diag <= 0 {
		diag = 1
	}
	// A pivot a billionth of the independent size means the column is
	// explained by the others to within single-precision noise. Anything
	// past that produces betas of arbitrary magnitude that happen to cancel,
	// which is worse than no answer at all.
	tol := 1e-9 * diag

	for col := 0; col < n; col++ {
		pivot, best := -1, tol
		for row := col; row < n; row++ {
			if v := math.Abs(m[row][col]); v > best {
				pivot, best = row, v
			}
		}
		if pivot < 0 {
			return nil, singularError{col: col}
		}
		m[col], m[pivot] = m[pivot], m[col]
		inv[col], inv[pivot] = inv[pivot], inv[col]

		d := m[col][col]
		for j := 0; j < n; j++ {
			m[col][j] /= d
			inv[col][j] /= d
		}
		for row := 0; row < n; row++ {
			if row == col {
				continue
			}
			f := m[row][col]
			if f == 0 {
				continue
			}
			for j := 0; j < n; j++ {
				m[row][j] -= f * m[col][j]
				inv[row][j] -= f * inv[col][j]
			}
		}
	}
	return inv, nil
}

// factorVerdict states, in one sentence, what survived the factors.
func factorVerdict(f *FactorExposure) string {
	if f == nil || f.Observations == 0 {
		return ""
	}

	// Significant loadings, largest first, because the reader wants to know
	// what the strategy actually is before hearing what is left over.
	sig := make([]FactorLoading, 0, len(f.Factors))
	for _, l := range f.Factors {
		if l.Significant() {
			sig = append(sig, l)
		}
	}
	sort.SliceStable(sig, func(i, j int) bool {
		return math.Abs(float64(sig[i].TStat)) > math.Abs(float64(sig[j].TStat))
	})
	names := make([]string, len(sig))
	for i, l := range sig {
		names[i] = strings.ToLower(l.Name)
	}

	lead := "after " + joinAnd(names) + " exposure, "
	if len(names) == 0 {
		lead = "no factor loading is distinguishable from zero, and "
	}

	if !f.Alpha.Defined() || !f.AlphaTStat.Defined() {
		return lead + "the alpha could not be estimated over this period"
	}
	alpha, t := float64(f.Alpha), float64(f.AlphaTStat)
	main := fmt.Sprintf("%sannual alpha is %.1f%% with a t-statistic of %.1f", lead, alpha*100, t)
	switch {
	case math.Abs(t) < 2:
		main += " — indistinguishable from zero"
	case t >= 2:
		main += " — which clears the conventional two-standard-error bar, so something here is not the factors"
	default:
		main += " — significantly negative, meaning the strategy did worse than its own exposures would have"
	}
	parts := []string{main}

	if mkt, ok := f.loading("Market"); ok && mkt.Beta.Defined() {
		b := float64(mkt.Beta)
		exposure, haveExposure := 0.0, f.AvgExposure.Defined()
		if haveExposure {
			exposure = float64(f.AvgExposure)
		}
		switch {
		case b > 1.25:
			parts = append(parts, fmt.Sprintf(
				"a market beta of %.2f means this is largely levered index exposure, which a margin loan and an index fund would reproduce", b))
		case haveExposure && exposure < 0.85 && exposure-b > 0.25:
			// Beta is variance-weighted and exposure is time-weighted, so a
			// beta well below the exposure of a book that spent time in cash
			// is arithmetic rather than skill: the bars it sat out carried
			// more of the market's variance than the ones it held. Calling
			// that "uncorrelated" instead would be flattering and wrong.
			parts = append(parts, fmt.Sprintf(
				"a market beta of %.2f against average gross exposure of %.2f — the strategy was out of the market for much of the period, and the beta sits below the exposure because the sessions it missed were the volatile ones",
				b, exposure))
		case b < 0.25 && b > -0.25 && haveExposure && exposure >= 0.85:
			parts = append(parts, fmt.Sprintf(
				"a market beta of %.2f while holding gross exposure of %.2f throughout, so the returns really are largely unrelated to the index", b, exposure))
		case b < 0.25 && b > -0.25:
			parts = append(parts, fmt.Sprintf(
				"a market beta of %.2f says the returns are close to unrelated to the index", b))
		}
	}

	if f.RSquared.Defined() {
		switch r := float64(f.RSquared); {
		case r > 0.9:
			parts = append(parts, fmt.Sprintf(
				"the factors explain %s of the variance, so this is close to a repackaging of them", fmtPercent(r)))
		case r < 0.3:
			parts = append(parts, fmt.Sprintf(
				"the factors explain only %s of the variance, so most of what this strategy does is something else", fmtPercent(r)))
		}
	}

	if len(f.Dropped) > 0 {
		var missing []string
		for _, dd := range f.Dropped {
			missing = append(missing, strings.ToLower(dd.Name))
		}
		verb := "is"
		if len(missing) > 1 {
			verb = "are"
		}
		parts = append(parts, joinAnd(missing)+
			" could not be measured over this period and "+verb+" not in the numbers above")
	}

	return strings.Join(parts, "; ")
}

// loading finds a factor by name.
func (f *FactorExposure) loading(name string) (FactorLoading, bool) {
	for _, l := range f.Factors {
		if strings.EqualFold(l.Name, name) {
			return l, true
		}
	}
	return FactorLoading{}, false
}

// joinAnd renders a list the way a sentence would.
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
