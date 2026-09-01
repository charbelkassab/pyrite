package engine

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/charbelkassab/pyrite/internal/market"
)

// The corpus is the critique's own test. It runs offline on synthetic and
// hand-built bars, so `go test ./...` exercises it with no network, no vendor
// and no key.
func TestSelfTestCorpus(t *testing.T) {
	outs, err := RunSelfTest(context.Background())
	if err != nil {
		t.Fatalf("run the corpus: %v", err)
	}
	if len(outs) != len(SelfTestCases()) {
		t.Fatalf("ran %d cases of %d", len(outs), len(SelfTestCases()))
	}
	for _, o := range outs {
		t.Run(o.Name, func(t *testing.T) {
			if o.Pass {
				return
			}
			// A failure here is a finding about the critique, not about the
			// strategy: either it has stopped detecting this defect or it has
			// started calling a sound run critical.
			t.Errorf("%s: %s\n  the critique said: %s", o.Case.Defect, o.Why, titles(Critique{Findings: o.Findings}))
		})
	}
}

// A corpus that flags everything proves nothing, so it has to contain a case
// that must come back clean, and each defective case has to name the finding
// it expects rather than accepting whatever turns up.
func TestSelfTestCorpusIsWellFormed(t *testing.T) {
	var controls int
	seen := map[string]bool{}
	for _, c := range SelfTestCases() {
		if seen[c.Name] {
			t.Errorf("two cases are called %q", c.Name)
		}
		seen[c.Name] = true
		if c.Defect == "" {
			t.Errorf("%s: no statement of what is wrong with it", c.Name)
		}
		if c.Control {
			controls++
			if c.Expect != "" {
				t.Errorf("%s is the control and must expect no finding, not %q", c.Name, c.Expect)
			}
			continue
		}
		if c.Expect == "" || c.Severity == "" {
			t.Errorf("%s must name the finding it expects and at what severity", c.Name)
		}
	}
	if controls != 1 {
		t.Errorf("the corpus has %d controls; without exactly one it only proves the critic is noisy", controls)
	}
}

// The corpus is worth nothing if the defects it claims to embody are not
// there. These two cases are the ones whose defect is a fact about the money
// rather than about the spec, so they are checked as such.
func TestSelfTestDefectsAreReal(t *testing.T) {
	lookahead := runSelfTestCase(t, "lookahead-close-fill")
	if !(lookahead.Return > 1 && lookahead.ContrastReturn < 0.1) {
		t.Errorf("close fills returned %.1f%% against %.1f%% at the next open: the case is "+
			"meant to make its money out of the fill price and no longer does",
			lookahead.Return*100, lookahead.ContrastReturn*100)
	}

	costs := runSelfTestCase(t, "costs-artefact")
	if !(costs.Return > 0 && costs.ContrastReturn < 0) {
		t.Errorf("the churn returned %.1f%% free and %.1f%% at five basis points: it is "+
			"supposed to be profitable only when nothing is charged",
			costs.Return*100, costs.ContrastReturn*100)
	}
}

// A critique that changed with the weather could not be argued with, so the
// corpus has to come back the same every time it is run.
//
// Returns are compared to a tolerance rather than bit for bit. A portfolio's
// value is summed over a map of positions, and Go randomises map order, so two
// identical multi-symbol runs agree to about fifteen significant figures and
// not to the last bit. Nothing here is decided at that resolution.
func TestSelfTestIsDeterministic(t *testing.T) {
	first, err := RunSelfTest(context.Background())
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	second, err := RunSelfTest(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	for i := range first {
		a, b := first[i], second[i]
		if a.Name != b.Name {
			t.Fatalf("case %d was %s then %s", i, a.Name, b.Name)
		}
		if a.Pass != b.Pass || a.TrustScore != b.TrustScore {
			t.Errorf("%s: pass %v/%v, score %d/%d between two runs of the same corpus",
				a.Name, a.Pass, b.Pass, a.TrustScore, b.TrustScore)
		}
		if math.Abs(a.Return-b.Return) > 1e-9*math.Max(1, math.Abs(a.Return)) {
			t.Errorf("%s returned %.12f then %.12f", a.Name, a.Return, b.Return)
		}
		if len(a.Findings) != len(b.Findings) {
			t.Errorf("%s produced %d findings then %d", a.Name, len(a.Findings), len(b.Findings))
			continue
		}
		for j := range a.Findings {
			if a.Findings[j].Title != b.Findings[j].Title {
				t.Errorf("%s finding %d was %q then %q", a.Name, j, a.Findings[j].Title, b.Findings[j].Title)
			}
		}
	}
}

// Everything the corpus needs comes from the synthetic provider or from bars
// built in this file, so a machine with no network and no keys runs the whole
// suite. Nothing here should ever reach for a vendor.
func TestSelfTestNeedsNoVendor(t *testing.T) {
	for _, c := range SelfTestCases() {
		store, err := c.store()
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		switch name := store.ProviderName(); name {
		case "synthetic", "hand-built":
		default:
			t.Errorf("%s loads its data from %q", c.Name, name)
		}
	}
}

// --- the gaps this corpus found ---------------------------------------------
//
// Both of these pin behaviour the critique does not have. They pass today
// because the critique stays silent; when one starts failing the gap has been
// closed, and the case belongs in the corpus above instead.

// The critique detects survivorship by coverage: a symbol with no bars at the
// start date was selected into the universe by having existed later. A list of
// names hand-picked for what they are worth today, all of which did exist on
// day one, is the same bias with no such signature, and nothing is said about
// it. Detecting that needs a fact the result does not carry — that the list
// came from a screen run today — which is what spec.Index and point-in-time
// membership exist to supply.
func TestSurvivorshipWithFullHistoryIsNotCaught(t *testing.T) {
	series := map[string]*market.Series{}
	for sym, daily := range map[string]float64{
		"WIN1": 1.0006, "WIN2": 1.0007, "WIN3": 1.0008, "WIN4": 1.0009,
	} {
		days := sessions("2013-01-02", "2021-12-31")
		bars := make([]market.Bar, 0, len(days))
		price := 50.0
		for _, d := range days {
			open := price
			price *= daily
			bars = append(bars, session(d, open, price))
		}
		series[sym] = market.NewSeries(sym, bars)
	}
	c := SelfTestCase{
		spec: selfTestSpec("2014-01-02", "2021-12-31", []string{"WIN1", "WIN2", "WIN3", "WIN4"}, `
			function onDay(ctx) {
				if (!ctx.isFirstTradingDayOfMonth()) return;
				ctx.equalWeight(ctx.universe(), 0.95);
			}
		`),
		bars: func() map[string]*market.Series { return series },
	}
	res := runSpec(t, c)
	if f := findingTitled(res.Critique, "survivorship in the symbol list"); f != nil {
		t.Errorf("the critique now catches a hand-picked universe with full history (%q). "+
			"That is an improvement: move this into the corpus as a case.", f.Detail)
	}
}

// One buy, held for a decade, never closed. TradeStats.Closed is zero and a
// fill exists, so neither arm of the sample-size test applies: "too few
// trades" wants at least one closed round trip and "never traded" wants no
// fills at all. A result resting on a single entry decision is told nothing
// about its sample size.
func TestSingleUnclosedTradeGetsNoSampleSizeFinding(t *testing.T) {
	c := SelfTestCase{
		spec: selfTestSpec("2012-01-03", "2021-12-31", []string{"AAPL"}, `
			function onDay(ctx) { if (ctx.dayIndex === 0) ctx.buy("AAPL", { pctCash: 0.99 }); }
		`),
	}
	res := runSpec(t, c)
	if res.TradeStats.Closed != 0 || len(res.Fills) != 1 {
		t.Fatalf("this case is meant to be one open trade: %d closed, %d fills",
			res.TradeStats.Closed, len(res.Fills))
	}
	for _, f := range res.Critique.Findings {
		if strings.Contains(f.Title, "too few trades") || strings.Contains(f.Title, "never traded") {
			t.Errorf("the critique now speaks to sample size on an open-only run (%q). "+
				"That is an improvement: move this into the corpus as a case.", f.Title)
		}
	}
}

// runSelfTestCase runs one named case from the corpus.
func runSelfTestCase(t *testing.T, name string) SelfTestOutcome {
	t.Helper()
	for _, c := range SelfTestCases() {
		if c.Name != name {
			continue
		}
		out, err := c.Run(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}
	t.Fatalf("no case called %q", name)
	return SelfTestOutcome{}
}

// runSpec runs a case's spec against its data without judging the critique,
// for the probes that are asking what the critique does not say.
func runSpec(t *testing.T, c SelfTestCase) *Result {
	t.Helper()
	store, err := c.store()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	res, err := New(c.spec, store).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}
