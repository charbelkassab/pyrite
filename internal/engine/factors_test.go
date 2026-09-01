package engine

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/charbelkassab/pyrite/internal/market"
)

// noiseSeries builds a deterministic pseudo-random return series, so every
// assertion below is reproducible without a network or a data provider.
func noiseSeries(n int, sd float64, seed int64) []float64 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]float64, n)
	for i := range out {
		out[i] = rng.NormFloat64() * sd
	}
	return out
}

func TestFactorBetaRecoversAKnownMultiple(t *testing.T) {
	// A series built as exactly 1.5 times a factor has a beta of 1.5 and no
	// alpha at all. If the regression cannot recover that, nothing else it
	// reports is worth reading.
	f := noiseSeries(500, 0.01, 1)
	y := make([]float64, len(f))
	for i := range f {
		y[i] = 1.5 * f[i]
	}

	res, err := FitFactorModel(y, []FactorSeries{{Name: "Market", Proxy: "SPY - rf", Values: f}}, DailyScale(0))
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if len(res.Factors) != 1 {
		t.Fatalf("expected 1 loading, got %d", len(res.Factors))
	}
	if got := float64(res.Factors[0].Beta); math.Abs(got-1.5) > 1e-6 {
		t.Errorf("beta: got %.6f, want 1.5", got)
	}
	if got := float64(res.Alpha); math.Abs(got) > 1e-6 {
		t.Errorf("alpha on an exact multiple: got %.8f, want 0", got)
	}
	if got := float64(res.RSquared); math.Abs(got-1) > 1e-9 {
		t.Errorf("R² on an exact fit: got %.9f, want 1", got)
	}
}

func TestFactorAlphaWithNoiseKeepsTheBetaAndFindsTheAlpha(t *testing.T) {
	// The same 1.5 beta, but with a real per-bar alpha and idiosyncratic
	// noise on top. Both the loading and the intercept must come back, and
	// the alpha must be significant when it is genuinely there.
	const perBarAlpha = 0.0008
	f := noiseSeries(2000, 0.01, 2)
	e := noiseSeries(2000, 0.002, 3)
	y := make([]float64, len(f))
	for i := range f {
		y[i] = perBarAlpha + 1.5*f[i] + e[i]
	}

	res, err := FitFactorModel(y, []FactorSeries{{Name: "Market", Values: f}}, DailyScale(0))
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if got := float64(res.Factors[0].Beta); math.Abs(got-1.5) > 0.02 {
		t.Errorf("beta: got %.4f, want 1.5", got)
	}
	// Three standard errors of room: the alpha is estimated, not read off.
	wantAlpha := perBarAlpha * TradingDaysPerYear
	tol := 3 * float64(res.AlphaStdErr)
	if got := float64(res.Alpha); math.Abs(got-wantAlpha) > tol {
		t.Errorf("annual alpha: got %.4f, want about %.4f (±%.4f)", got, wantAlpha, tol)
	}
	if got := float64(res.AlphaTStat); got < 2 {
		t.Errorf("a real alpha of %.2f%% a year should be significant, got t=%.2f", wantAlpha*100, got)
	}
	if !strings.Contains(res.Verdict, "t-statistic") {
		t.Errorf("verdict should quote the t-statistic, got %q", res.Verdict)
	}
}

func TestFactorNoiseProducesAnInsignificantAlpha(t *testing.T) {
	// Returns unrelated to the factor and with no drift at all. A two-
	// standard-error bar is a 5% test, so a couple of these draws will clear
	// it by construction and asserting that no single one does would be
	// asserting the wrong thing. What must hold is that the great majority do
	// not, and that the verdict says so in words when they do not.
	const draws = 40
	var significant int
	var quiet *FactorExposure

	for i := 0; i < draws; i++ {
		f := noiseSeries(1000, 0.01, int64(100+2*i))
		y := noiseSeries(1000, 0.01, int64(101+2*i))
		res, err := FitFactorModel(y, []FactorSeries{{Name: "Market", Values: f}}, DailyScale(0))
		if err != nil {
			t.Fatalf("draw %d: fit: %v", i, err)
		}
		if !res.AlphaTStat.Defined() {
			t.Fatalf("draw %d: t-statistic came back undefined", i)
		}
		if math.Abs(float64(res.AlphaTStat)) >= 2 {
			significant++
			continue
		}
		if quiet == nil {
			quiet = res
		}
		if got := float64(res.RSquared); got > 0.05 {
			t.Errorf("draw %d: unrelated series should share almost no variance, got R² %.3f", i, got)
		}
	}

	if significant > draws/5 {
		t.Errorf("%d of %d pure-noise draws produced a significant alpha, which is far more than a 5%% test should",
			significant, draws)
	}
	if quiet == nil {
		t.Fatal("every noise draw came back significant")
	}
	if !strings.Contains(quiet.Verdict, "indistinguishable from zero") {
		t.Errorf("verdict on noise should call the alpha indistinguishable from zero, got %q", quiet.Verdict)
	}
}

func TestFactorCollinearInputErrorsRatherThanReturningNaN(t *testing.T) {
	// Two factors that are the same series, and a third that is the sum of
	// two others. Either way there is no unique set of betas, and the honest
	// answer is an error a user can act on rather than a table of NaNs.
	a := noiseSeries(400, 0.01, 21)
	b := noiseSeries(400, 0.01, 22)
	sum := make([]float64, len(a))
	dup := make([]float64, len(a))
	for i := range a {
		sum[i] = a[i] + b[i]
		dup[i] = a[i]
	}
	y := noiseSeries(400, 0.01, 23)

	cases := map[string][]FactorSeries{
		"duplicate column": {
			{Name: "Market", Values: a},
			{Name: "Size", Values: dup},
		},
		"exact linear combination": {
			{Name: "Market", Values: a},
			{Name: "Size", Values: b},
			{Name: "Value", Values: sum},
		},
	}
	for label, factors := range cases {
		res, err := FitFactorModel(y, factors, DailyScale(0))
		if err == nil {
			t.Errorf("%s: expected an error, got alpha %v and %d loadings", label, res.Alpha, len(res.Factors))
			continue
		}
		if res != nil {
			t.Errorf("%s: a failed fit must not also return a result", label)
		}
		if !strings.Contains(err.Error(), "cannot separate") {
			t.Errorf("%s: error should name the problem, got %q", label, err)
		}
	}
}

func TestFactorConstantColumnIsRejected(t *testing.T) {
	// A factor that never moves is collinear with the intercept, so no
	// exposure to it exists to be measured.
	y := noiseSeries(200, 0.01, 31)
	flat := make([]float64, len(y))
	_, err := FitFactorModel(y, []FactorSeries{{Name: "Flat", Values: flat}}, DailyScale(0))
	if err == nil {
		t.Fatal("expected an error for a factor that never moves")
	}
	if !strings.Contains(err.Error(), "zero at every observation") {
		t.Errorf("error should say why, got %q", err)
	}
}

func TestFactorAlphaFollowsTheScaleNotAConstant(t *testing.T) {
	// The alpha is a per-bar mean annualised by the run's Scale. Hardcoding
	// 252 would give a monthly or a 5-minute run an alpha out by orders of
	// magnitude, in whichever direction happened to flatter it.
	const perBar = 0.001
	f := noiseSeries(600, 0.01, 41)
	y := make([]float64, len(f))
	for i := range f {
		y[i] = perBar + 0.8*f[i]
	}

	for _, tc := range []struct {
		name  string
		scale Scale
	}{
		{"daily", DailyScale(0)},
		{"monthly", Scale{PeriodsPerYear: 12}},
		{"five minute", ScaleFor(market.Interval5m, 0)},
	} {
		res, err := FitFactorModel(y, []FactorSeries{{Name: "Market", Values: f}}, tc.scale)
		if err != nil {
			t.Fatalf("%s: fit: %v", tc.name, err)
		}
		want := perBar * tc.scale.Periods()
		if got := float64(res.Alpha); math.Abs(got-want) > math.Abs(want)*1e-6 {
			t.Errorf("%s: annual alpha %.6f, want %.6f (%.0f periods per year)",
				tc.name, got, want, tc.scale.Periods())
		}
		if res.PeriodsPerYear != tc.scale.Periods() {
			t.Errorf("%s: result records %.0f periods per year, want %.0f",
				tc.name, res.PeriodsPerYear, tc.scale.Periods())
		}
	}

	// The t-statistic is a ratio of two quantities that annualise by the same
	// factor, so it must not move at all.
	daily, err := FitFactorModel(y, []FactorSeries{{Name: "Market", Values: f}}, DailyScale(0))
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	monthly, err := FitFactorModel(y, []FactorSeries{{Name: "Market", Values: f}}, Scale{PeriodsPerYear: 12})
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if a, b := float64(daily.AlphaTStat), float64(monthly.AlphaTStat); math.Abs(a-b) > 1e-9 {
		t.Errorf("t-statistic changed with the annualisation: %.6f then %.6f", a, b)
	}
}

func TestFactorStandardErrorsAllowForAutocorrelation(t *testing.T) {
	// Strongly autocorrelated residuals. Ordinary least squares would treat
	// each bar as an independent draw and report a standard error that is far
	// too small; the Newey-West correction must widen it.
	f := noiseSeries(1000, 0.01, 51)
	shock := noiseSeries(1000, 0.004, 52)
	y := make([]float64, len(f))
	carry := 0.0
	for i := range f {
		carry = 0.9*carry + shock[i]
		y[i] = 0.0003 + f[i] + carry
	}
	factors := []FactorSeries{{Name: "Market", Values: f}}

	res, err := FitFactorModel(y, factors, DailyScale(0))
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if res.NeweyWestLag < 1 {
		t.Fatalf("expected a positive Newey-West lag, got %d", res.NeweyWestLag)
	}

	// The same fit with no lags is the plain White estimator; the corrected
	// standard error on the intercept must be the larger of the two.
	_, plainSE, _, _, err := fitOLS(y, factors, 0)
	if err != nil {
		t.Fatalf("fit without lags: %v", err)
	}
	corrected := float64(res.AlphaStdErr) / DailyScale(0).Periods()
	if corrected <= plainSE[0] {
		t.Errorf("autocorrelated residuals should widen the alpha standard error: %g with lags against %g without",
			corrected, plainSE[0])
	}
}

func TestNeweyWestLagIsSmallAndBounded(t *testing.T) {
	for _, tc := range []struct{ n, min, max int }{
		{100, 1, 4},
		{1500, 4, 10},
		{100000, 1, 10},
	} {
		got := neweyWestLag(tc.n)
		if got < tc.min || got > tc.max {
			t.Errorf("neweyWestLag(%d) = %d, want between %d and %d", tc.n, got, tc.min, tc.max)
		}
	}
	if got := neweyWestLag(2); got != 0 {
		t.Errorf("neweyWestLag(2) = %d, want 0 for a series too short to lag", got)
	}
}

func TestFactorShortSampleIsRefused(t *testing.T) {
	y := noiseSeries(20, 0.01, 61)
	f := noiseSeries(20, 0.01, 62)
	_, err := FitFactorModel(y, []FactorSeries{{Name: "Market", Values: f}}, DailyScale(0))
	if err == nil {
		t.Fatal("expected a refusal on a 20 bar sample")
	}
	if !strings.Contains(err.Error(), "at least") {
		t.Errorf("error should say what the minimum is, got %q", err)
	}
}

func TestFactorMisalignedSeriesIsRefused(t *testing.T) {
	y := noiseSeries(200, 0.01, 71)
	f := noiseSeries(150, 0.01, 72)
	_, err := FitFactorModel(y, []FactorSeries{{Name: "Market", Values: f}}, DailyScale(0))
	if err == nil {
		t.Fatal("expected a refusal on series of different lengths")
	}
	if !strings.Contains(err.Error(), "aligned") {
		t.Errorf("error should point at the alignment, got %q", err)
	}
}

func TestAnalyseFactorsGivesBuyAndHoldAMarketBetaOfOne(t *testing.T) {
	// A curve that is SPY itself must load about 1.0 on the market factor and
	// nothing on any other. This is the check that the return series are
	// aligned on the same pairs of dates: get that wrong and the beta lands
	// somewhere absurd while every other number still looks plausible.
	store := newTestStore(t)
	ctx := context.Background()

	spy, err := store.Get(ctx, "SPY", "2016-01-04", "2022-12-30")
	if err != nil {
		t.Fatalf("fetch SPY: %v", err)
	}
	var days []market.Day
	for _, b := range spy.Range("2016-01-04", "2022-12-30") {
		days = append(days, b.Date)
	}
	curve := BuyAndHoldCurve(spy, days, 100000)

	res, err := AnalyseFactors(ctx, curve, store, market.Interval1d, DailyScale(0), nil)
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if res.Observations < 1500 {
		t.Fatalf("expected most of seven years of bars, got %d", res.Observations)
	}
	mkt, ok := res.loading("Market")
	if !ok {
		t.Fatal("no market loading in the result")
	}
	if got := float64(mkt.Beta); math.Abs(got-1) > 0.02 {
		t.Errorf("buy and hold SPY has market beta %.3f, want about 1", got)
	}
	if got := math.Abs(float64(res.Alpha)); got > 0.01 {
		t.Errorf("holding the market factor itself should leave no alpha, got %.4f", got)
	}
	if got := float64(res.RSquared); got < 0.99 {
		t.Errorf("SPY against a factor set containing SPY should be almost fully explained, got R² %.4f", got)
	}
	if got := float64(res.AvgExposure); math.Abs(got-1) > 1e-9 {
		t.Errorf("a buy-and-hold curve is fully invested throughout, got exposure %.3f", got)
	}
	if res.ProxyNote == "" {
		t.Error("the ETF proxy caveat must travel with the result")
	}
}

func TestFactorVerdictReadsBetaAgainstExposure(t *testing.T) {
	// The same market beta means two different things depending on how much
	// of the period the strategy was actually invested for. A timing rule
	// that sat out the volatile stretches must not be described as having
	// found something uncorrelated with the index.
	invested := &FactorExposure{
		Observations: 1000, Alpha: 0.05, AlphaStdErr: 0.05, AlphaTStat: 1.0,
		RSquared: 0.5, AvgExposure: 1.0,
		Factors: []FactorLoading{{Name: "Market", Beta: 0.2, StdErr: 0.02, TStat: 10}},
	}
	if got := factorVerdict(invested); !strings.Contains(got, "really are largely unrelated") {
		t.Errorf("a fully invested low-beta book is genuinely uncorrelated, got %q", got)
	}

	timing := *invested
	timing.AvgExposure = 0.6
	if got := factorVerdict(&timing); !strings.Contains(got, "out of the market for much of the period") {
		t.Errorf("a low beta against high exposure is a timing rule, got %q", got)
	}

	levered := *invested
	levered.Factors = []FactorLoading{{Name: "Market", Beta: 1.6, StdErr: 0.02, TStat: 80}}
	if got := factorVerdict(&levered); !strings.Contains(got, "levered index exposure") {
		t.Errorf("a beta of 1.6 is levered index exposure, got %q", got)
	}
}

func TestAnalyseFactorsDropsProxiesWithoutData(t *testing.T) {
	// Only SPY and IWM exist at this provider. Value, momentum and quality
	// cannot be built, and the result must say so rather than quietly
	// reporting a two-factor model as though it were the whole set.
	series := map[string]*market.Series{}
	for _, sym := range []string{"SPY", "IWM"} {
		s, err := market.NewSyntheticProvider().Fetch(context.Background(), sym, "2018-01-02", "2021-12-31")
		if err != nil {
			t.Fatalf("synthesise %s: %v", sym, err)
		}
		series[sym] = s
	}
	store := market.NewStore(&fixedProvider{series: series}, nil, mustFundamentals(t))
	ctx := context.Background()

	spy := series["SPY"]
	var days []market.Day
	for _, b := range spy.Range("2018-01-02", "2021-12-31") {
		days = append(days, b.Date)
	}
	curve := BuyAndHoldCurve(spy, days, 100000)

	res, err := AnalyseFactors(ctx, curve, store, market.Interval1d, DailyScale(0), nil)
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if len(res.Factors) != 2 {
		t.Fatalf("expected market and size only, got %d loadings", len(res.Factors))
	}
	got := map[string]bool{}
	for _, d := range res.Dropped {
		got[d.Name] = true
		if d.Reason == "" {
			t.Errorf("dropped factor %q has no reason", d.Name)
		}
	}
	for _, want := range []string{"Value", "Momentum", "Low volatility"} {
		if !got[want] {
			t.Errorf("factor %q should have been reported as dropped", want)
		}
	}
	if !strings.Contains(res.Verdict, "could not be measured") {
		t.Errorf("verdict should mention the dropped factors, got %q", res.Verdict)
	}
}

func TestAnalyseFactorsRefusesWhenNoProxyHasData(t *testing.T) {
	series := map[string]*market.Series{}
	s, err := market.NewSyntheticProvider().Fetch(context.Background(), "AAPL", "2018-01-02", "2021-12-31")
	if err != nil {
		t.Fatalf("synthesise: %v", err)
	}
	series["AAPL"] = s
	store := market.NewStore(&fixedProvider{series: series}, nil, mustFundamentals(t))

	var days []market.Day
	for _, b := range s.Range("2018-01-02", "2021-12-31") {
		days = append(days, b.Date)
	}
	curve := BuyAndHoldCurve(s, days, 100000)

	_, err = AnalyseFactors(context.Background(), curve, store, market.Interval1d, DailyScale(0), nil)
	if err == nil {
		t.Fatal("expected a refusal when no proxy has data")
	}
	if !strings.Contains(err.Error(), "nothing to regress against") {
		t.Errorf("error should say what went wrong, got %q", err)
	}
}

func TestFactorExposureJSONSurvivesUndefinedNumbers(t *testing.T) {
	// A bare NaN aborts encoding/json for the whole document, which has
	// truncated a response here before. Every float that can go undefined is
	// a Ratio, so the encoder must produce nulls and no error.
	f := &FactorExposure{
		Observations: 100,
		Alpha:        Ratio(math.NaN()),
		AlphaStdErr:  Ratio(math.Inf(1)),
		AlphaTStat:   Ratio(math.NaN()),
		RSquared:     Ratio(math.NaN()),
		AdjRSquared:  Ratio(math.NaN()),
		Factors: []FactorLoading{{
			Name: "Market", Beta: Ratio(math.NaN()),
			StdErr: Ratio(math.NaN()), TStat: Ratio(math.NaN()),
		}},
	}
	mustEncode(t, "FactorExposure", f)

	out, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"alpha":null`) {
		t.Errorf("undefined alpha should encode as null, got %s", out)
	}
}
