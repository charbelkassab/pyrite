package engine

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/charbelkassab/pyrite/internal/market"
)

// diffSpec is the setup both sides of every synthetic comparison share, so
// that a test which means to change the strategies does not accidentally
// change the experiment and get refused.
func diffSpec(name string) Spec {
	s := Spec{
		Name:            name,
		Universe:        []string{"AAPL"},
		Start:           "2019-01-02",
		End:             "2022-12-30",
		InitialCash:     100000,
		AllowFractional: true,
	}
	s.ApplyDefaults()
	return s
}

// diffRun builds a Result from a return series, so a test can control
// exactly what the two strategies did rather than hoping a rule produces it.
func diffRun(name string, rets []float64) *Result {
	curve := make([]EquityPoint, 0, len(rets)+1)
	day := market.Day("2019-01-02")
	nextSession := func(d market.Day) market.Day {
		for {
			d = d.Add(1)
			if wd := d.Time().Weekday(); wd != 0 && wd != 6 {
				return d
			}
		}
	}
	equity, peak := 100000.0, 100000.0
	curve = append(curve, EquityPoint{Date: day, Value: equity, Exposure: 1})
	for _, r := range rets {
		day = nextSession(day)
		equity *= 1 + r
		if equity > peak {
			peak = equity
		}
		curve = append(curve, EquityPoint{
			Date: day, Value: equity, Return: r, Drawdown: equity/peak - 1, Exposure: 1,
		})
	}
	spec := diffSpec(name)
	spec.End = day
	return &Result{Spec: spec, Curve: curve, Metrics: ComputeMetrics(curve, DailyScale(0))}
}

// normalDraws is a deterministic standard normal sample, built from the same
// generator the bootstrap uses so a test is reproducible without a global
// source.
func normalDraws(n int, seed int64) []float64 {
	rng := newLCG(seed)
	out := make([]float64, n)
	for i := range out {
		u := rng.float64()
		if u <= 0 {
			u = 1e-12
		}
		out[i] = normalInv(u)
	}
	return out
}

func TestDiffOfARunAgainstItselfIsExactlyZero(t *testing.T) {
	// The calibration check. If a strategy compared with itself reports
	// anything other than no difference at all, the pairing or the date
	// alignment is wrong and every other number the command prints is
	// unreliable.
	spec := baseSpec(`
		function onDay(ctx) {
			if (!ctx.hasPosition("AAPL") && ctx.cash > 1000) {
				ctx.buy("AAPL", { pctCash: 0.5 });
			}
		}
	`)
	spec.Universe = []string{"AAPL"}
	store := newTestStore(t)

	first, err := New(spec, store).Run(context.Background())
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := New(spec, store).Run(context.Background())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	d, err := CompareRuns(first, second, DiffOptions{Seed: 3})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !d.Identical {
		t.Fatalf("two runs of one spec should be identical")
	}
	if d.Paired.MeanDifference != 0 || d.Paired.AnnualDifference != 0 {
		t.Errorf("mean difference should be exactly zero, got %v and %v",
			d.Paired.MeanDifference, d.Paired.AnnualDifference)
	}
	if float64(d.Paired.TStat) != 0 {
		t.Errorf("t-statistic should be exactly zero, got %v", float64(d.Paired.TStat))
	}
	if float64(d.Overlap.Correlation) != 1 {
		t.Errorf("correlation should be exactly 1, got %v", float64(d.Overlap.Correlation))
	}
	if float64(d.Sharpe.Difference) != 0 {
		t.Errorf("Sharpe difference should be exactly zero, got %v", float64(d.Sharpe.Difference))
	}
	if float64(d.Overlap.SameHoldings) != 1 {
		t.Errorf("both runs held the same book every session, got %v", float64(d.Overlap.SameHoldings))
	}
	if !hasDiffFinding(d.Findings, "you did not actually change anything") {
		t.Errorf("an identical pair must be told so, got %v", findingTitles(d.Findings))
	}
}

func TestDiffOfIndependentNoiseIsInsignificant(t *testing.T) {
	// Two strategies with nothing in common and no edge either way. Anything
	// the command calls significant here it would call significant on a pair
	// of coin flips.
	a := scaleBy(normalDraws(1000, 11), 0.01)
	b := scaleBy(normalDraws(1000, 22), 0.01)

	d, err := CompareRuns(diffRun("A", a), diffRun("B", b), DiffOptions{Seed: 5})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if d.Identical {
		t.Fatal("independent noise should not read as identical")
	}
	if got := math.Abs(float64(d.Paired.TStat)); got >= 2 {
		t.Errorf("independent noise produced t = %.2f, which claims a difference that is not there", got)
	}
	if !strings.Contains(d.Verdict, "not a difference you can act on") {
		t.Errorf("verdict should refuse to act on noise: %q", d.Verdict)
	}
	if lo, hi := float64(d.Sharpe.CILow), float64(d.Sharpe.CIHigh); lo > 0 || hi < 0 {
		t.Errorf("the Sharpe interval %.2f to %.2f should contain zero", lo, hi)
	}
}

func TestDiffFindsAGenuineEdgeThatAnUnpairedTestWouldMiss(t *testing.T) {
	// Both strategies are mostly the same market. B adds five basis points a
	// day on top, which is a real and large edge, plus its own noise so the
	// difference series is not a constant.
	//
	// This is the case the whole file is built around: the paired statistic
	// finds the edge, and the two-sample statistic computed below — the one a
	// comparison of two backtests amounts to when it does not pair — cannot,
	// because the market movement both strategies share swamps it.
	common := scaleBy(normalDraws(1000, 31), 0.012)
	idio := scaleBy(normalDraws(1000, 41), 0.003)
	a := common
	b := make([]float64, len(a))
	for i := range b {
		b[i] = a[i] + 0.0005 + idio[i]
	}

	d, err := CompareRuns(diffRun("A", a), diffRun("B", b), DiffOptions{Seed: 5})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if got := float64(d.Paired.TStat); got < 3 {
		t.Errorf("a five basis point daily edge over 1000 sessions should be plain, got t = %.2f", got)
	}
	if p := float64(d.Paired.PValue); p > 0.01 {
		t.Errorf("p-value %.4f is too large for an edge this size", p)
	}
	if !strings.Contains(d.Verdict, "survives the correction") {
		t.Errorf("verdict should say the difference stands: %q", d.Verdict)
	}

	// The same data through an unpaired two-sample t, for contrast.
	ma, sa := meanStdev(a)
	mb, sb := meanStdev(b)
	n := float64(len(a))
	unpaired := (mb - ma) / math.Sqrt(sa*sa/n+sb*sb/n)
	if math.Abs(unpaired) >= 2 {
		t.Fatalf("the two-sample statistic was meant to miss this edge; it got %.2f, so the "+
			"test no longer demonstrates why pairing matters", unpaired)
	}
}

func TestDiffCallsNearIdenticalStrategiesOneStrategy(t *testing.T) {
	// B is A with a whisper of its own. That is what a tweaked threshold
	// usually produces, and reporting the return gap without saying so is the
	// error this finding exists for.
	a := scaleBy(normalDraws(800, 51), 0.01)
	nudge := scaleBy(normalDraws(800, 61), 0.0005)
	b := make([]float64, len(a))
	for i := range b {
		b[i] = a[i] + nudge[i]
	}

	d, err := CompareRuns(diffRun("A", a), diffRun("B", b), DiffOptions{Seed: 5})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if c := float64(d.Overlap.Correlation); c < 0.99 {
		t.Fatalf("the two series were built to correlate above 0.99, got %.4f", c)
	}
	if !hasDiffFinding(d.Findings, "these are one strategy with two names") {
		t.Errorf("a near-identical pair must be called out, got %v", findingTitles(d.Findings))
	}
	if d.Findings[0].Severity != SeverityCritical {
		t.Errorf("that finding should lead and should be critical, got %v", d.Findings[0])
	}
}

func TestDiffIsReproducibleFromItsSeed(t *testing.T) {
	a := scaleBy(normalDraws(600, 71), 0.01)
	b := scaleBy(normalDraws(600, 81), 0.01)
	runA, runB := diffRun("A", a), diffRun("B", b)

	first, err := CompareRuns(runA, runB, DiffOptions{Seed: 9})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	second, err := CompareRuns(runA, runB, DiffOptions{Seed: 9})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if first.Sharpe.CILow != second.Sharpe.CILow || first.Sharpe.CIHigh != second.Sharpe.CIHigh {
		t.Errorf("the same seed gave two intervals: %v-%v then %v-%v",
			first.Sharpe.CILow, first.Sharpe.CIHigh, second.Sharpe.CILow, second.Sharpe.CIHigh)
	}
	if first.Verdict != second.Verdict {
		t.Errorf("the same seed gave two verdicts:\n%s\n%s", first.Verdict, second.Verdict)
	}

	// And the seed has to be doing something, or the reproducibility above is
	// the reproducibility of a constant.
	other, err := CompareRuns(runA, runB, DiffOptions{Seed: 12345})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if other.Sharpe.CILow == first.Sharpe.CILow && other.Sharpe.CIHigh == first.Sharpe.CIHigh {
		t.Error("a different seed produced an identical interval, so the seed is not reaching the bootstrap")
	}
}

func TestDiffRefusesRunsThatWereNotTheSameExperiment(t *testing.T) {
	rets := scaleBy(normalDraws(300, 91), 0.01)
	base := diffRun("A", rets)

	cases := []struct {
		name   string
		change func(*Result)
		expect string
	}{
		{"period", func(r *Result) { r.Spec.Start = "2019-06-03" }, "period"},
		{"slippage", func(r *Result) { r.Spec.Costs.SlippageBps += 5 }, "slippage"},
		{"fill model", func(r *Result) { r.Spec.Fill = FillClose }, "fill model"},
		{"capital", func(r *Result) { r.Spec.InitialCash = 250000 }, "starting capital"},
		{"bar size", func(r *Result) { r.Spec.Interval = market.Interval("1h") }, "bar size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			other := diffRun("B", rets)
			tc.change(other)
			_, err := CompareRuns(base, other, DiffOptions{Seed: 1})
			if err == nil {
				t.Fatalf("a %s mismatch should be refused, not compared", tc.name)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("the refusal should name what differs; got %v", err)
			}
		})
	}

	// A universe difference is not a refusal: a strategy declares its own
	// universe here, so refusing would refuse most real comparisons.
	other := diffRun("B", rets)
	other.Spec.Universe = []string{"MSFT"}
	d, err := CompareRuns(base, other, DiffOptions{Seed: 1})
	if err != nil {
		t.Fatalf("a universe difference should be reported, not refused: %v", err)
	}
	if !hasDiffFinding(d.Findings, "the two strategies trade different universes") {
		t.Errorf("but it must be reported, got %v", findingTitles(d.Findings))
	}
}

func TestDiffDropsSessionsOnlyOneRunTraded(t *testing.T) {
	rets := scaleBy(normalDraws(300, 101), 0.01)
	a := diffRun("A", rets)
	b := diffRun("B", rets)
	// Remove a fortnight from the middle of B. The pairing must span the gap
	// on both sides rather than lining up a one-day move against a longer one.
	b.Curve = append(append([]EquityPoint{}, b.Curve[:100]...), b.Curve[110:]...)

	d, err := CompareRuns(a, b, DiffOptions{Seed: 1})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if d.UnpairedA != 10 || d.UnpairedB != 0 {
		t.Errorf("expected 10 unpaired sessions on A, got %d and %d", d.UnpairedA, d.UnpairedB)
	}
	if d.Sessions != len(b.Curve)-1 {
		t.Errorf("expected %d paired sessions, got %d", len(b.Curve)-1, d.Sessions)
	}
	if !hasDiffFinding(d.Findings, "the two runs did not trade the same sessions") {
		t.Errorf("dropped sessions must be reported, got %v", findingTitles(d.Findings))
	}
	// The two curves still agree on every date they share, so the difference
	// across the gap must still be exactly nothing.
	if !d.Identical {
		t.Errorf("the shared sessions are identical; difference was %v", d.Paired.MeanDifference)
	}
}

func TestDiffRefusesToTestTooShortASample(t *testing.T) {
	a := scaleBy(normalDraws(40, 111), 0.01)
	b := scaleBy(normalDraws(40, 121), 0.01)

	d, err := CompareRuns(diffRun("A", a), diffRun("B", b), DiffOptions{Seed: 1})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if d.Paired.TStat.Defined() {
		t.Errorf("40 sessions should produce no t-statistic, got %v", float64(d.Paired.TStat))
	}
	if !hasDiffFinding(d.Findings, "too few paired sessions to compare anything") {
		t.Errorf("got %v", findingTitles(d.Findings))
	}
}

func TestDiffSurvivesJSONWithUndefinedStatistics(t *testing.T) {
	// The bug this guards: a bare NaN reaching encoding/json truncates the
	// whole response rather than failing loudly, so every statistic that can
	// be undefined has to be a Ratio.
	a := scaleBy(normalDraws(20, 131), 0.01)
	b := scaleBy(normalDraws(20, 141), 0.01)
	d, err := CompareRuns(diffRun("A", a), diffRun("B", b), DiffOptions{Seed: 1})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if d.Paired.TStat.Defined() {
		t.Fatal("this fixture is meant to leave the statistics undefined")
	}
	out, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"t_stat":null`) {
		t.Errorf("an undefined t-statistic should marshal to null: %s", out)
	}
	var back Diff
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Paired.TStat.Defined() {
		t.Error("null should come back undefined, not zero")
	}
}

func TestHoldingKeyIgnoresOrderAndSize(t *testing.T) {
	// The book is a set, and the portfolio produces it in whatever order it
	// happens to hold. Two identical books arriving in different orders must
	// not read as a change of position.
	x := holdingKey([]PositionSnapshot{{Symbol: "MSFT", Shares: 10}, {Symbol: "AAPL", Shares: 3}})
	y := holdingKey([]PositionSnapshot{{Symbol: "AAPL", Shares: 300}, {Symbol: "MSFT", Shares: 1}})
	if x != y {
		t.Errorf("same book, different order and size: %q against %q", x, y)
	}
	if s := holdingKey([]PositionSnapshot{{Symbol: "AAPL", Shares: -3}}); s == x {
		t.Error("a short should not read as the same position as a long")
	}
	if s := holdingKey([]PositionSnapshot{{Symbol: "AAPL", Shares: 0}}); s != "" {
		t.Errorf("a closed position is not a holding, got %q", s)
	}
}

func TestNeweyWestWidensTheStandardErrorOnAutocorrelatedDifferences(t *testing.T) {
	// The reason the correction is here at all: a difference series that
	// persists from one session to the next carries less information than its
	// length suggests, and the uncorrected statistic does not know that. The
	// error is always in the flattering direction, so the corrected t must be
	// the smaller of the two.
	shock := normalDraws(1000, 151)
	d := make([]float64, len(shock))
	prev := 0.0
	for i := range d {
		// A first-order autoregression with a coefficient of 0.7, which is
		// far more persistence than a real strategy difference has and makes
		// the effect unmistakable.
		prev = 0.7*prev + 0.003*shock[i]
		d[i] = prev + 0.0004
	}
	a := scaleBy(normalDraws(1000, 161), 0.01)
	b := make([]float64, len(a))
	for i := range b {
		b[i] = a[i] + d[i]
	}

	diff, err := CompareRuns(diffRun("A", a), diffRun("B", b), DiffOptions{Seed: 1})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	corrected := math.Abs(float64(diff.Paired.TStat))
	naive := math.Abs(float64(diff.Paired.NaiveTStat))
	if !(corrected < naive) {
		t.Errorf("the corrected t (%.2f) should be below the naive one (%.2f)", corrected, naive)
	}
	if diff.Paired.NeweyWestLag != neweyWestLag(len(d)) {
		t.Errorf("the bandwidth should come from the shared rule: got %d, want %d",
			diff.Paired.NeweyWestLag, neweyWestLag(len(d)))
	}
}

// scaleBy multiplies a series by a constant.
func scaleBy(xs []float64, k float64) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = x * k
	}
	return out
}

func hasDiffFinding(fs []Finding, title string) bool {
	for _, f := range fs {
		if f.Title == title {
			return true
		}
	}
	return false
}

func findingTitles(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Title)
	}
	return out
}
