package engine

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/charbelkassab/natural-quant/internal/market"
)

// The point of these tests is the wall, not the search. A proposer that could
// see out-of-sample results would be an excellent optimiser of the wrong
// thing, and the resulting backtest would look like evidence.

func agentBase() Spec {
	spec := baseSpec("")
	spec.Universe = []string{"AAPL"}
	spec.Start = "2018-01-02"
	spec.End = "2023-12-29"
	return spec
}

// buyAndHoldCode returns a strategy that buys once, with a nominal parameter
// so each candidate differs.
func buyAndHoldCode(pct float64) string {
	return fmt.Sprintf(`
		function onDay(ctx) {
			if (ctx.dayIndex === 0) ctx.buy("AAPL", { pctCash: %.2f });
		}
	`, pct)
}

func TestAgentSplitsThePeriodAndHoldsTheTailBack(t *testing.T) {
	p := ProposerFunc(func(ctx context.Context, h []Candidate) (string, string, error) {
		if len(h) >= 3 {
			return "", "", nil
		}
		return buyAndHoldCode(0.5 + 0.1*float64(len(h))), "trying a larger position", nil
	})

	res, err := RunAgent(context.Background(), AgentSpec{Base: agentBase(), Budget: 5},
		newTestStore(t), p, nil)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	if len(res.Candidates) != 3 {
		t.Fatalf("the proposer stopped after 3, got %d candidates", len(res.Candidates))
	}

	// The split must be a real split, in order, with nothing shared.
	if !(res.TrainStart < res.TrainEnd && res.TrainEnd < res.HoldoutStart &&
		res.HoldoutStart < res.HoldoutEnd) {
		t.Fatalf("the periods are not ordered and disjoint: %+v", res)
	}
	if res.Holdout == nil {
		t.Fatal("the winner was never scored on the holdout")
	}
	// The holdout run must cover only the holdout window.
	if res.Holdout.Curve[0].Date < res.HoldoutStart {
		t.Errorf("the holdout run started at %s, before the holdout window",
			res.Holdout.Curve[0].Date)
	}
	if res.Holdout.Spec.Start != res.HoldoutStart || res.Holdout.Spec.End != res.HoldoutEnd {
		t.Errorf("the holdout run did not use the holdout window: %v..%v",
			res.Holdout.Spec.Start, res.Holdout.Spec.End)
	}
}

func TestProposerNeverSeesHoldoutResults(t *testing.T) {
	// Every candidate handed to the proposer must be measured only over the
	// training window. This is checked by comparing the training metrics it
	// was shown against a run of the same code over the same window.
	var shown []Candidate
	p := ProposerFunc(func(ctx context.Context, h []Candidate) (string, string, error) {
		shown = append([]Candidate(nil), h...)
		if len(h) >= 2 {
			return "", "", nil
		}
		return buyAndHoldCode(0.9), "hold", nil
	})

	store := newTestStore(t)
	res, err := RunAgent(context.Background(), AgentSpec{Base: agentBase(), Budget: 4}, store, p, nil)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	if len(shown) == 0 {
		t.Fatal("the proposer was never shown any history")
	}

	// Re-run the same code over the training window and require a match.
	trainOnly := agentBase()
	trainOnly.Code = buyAndHoldCode(0.9)
	trainOnly.Start, trainOnly.End = res.TrainStart, res.TrainEnd
	trainOnly.OmitDayRecords = true
	ref, err := New(trainOnly, store).Run(context.Background())
	if err != nil {
		t.Fatalf("reference run: %v", err)
	}
	got := shown[0].TrainMetrics
	if math.Abs(got.TotalReturn-ref.Metrics.TotalReturn) > 1e-9 {
		t.Errorf("the proposer was shown %v but the training window returned %v — "+
			"it saw more than the training period", got.TotalReturn, ref.Metrics.TotalReturn)
	}
	if got.TradingDays != ref.Metrics.TradingDays {
		t.Errorf("the proposer saw %d sessions, the training window has %d",
			got.TradingDays, ref.Metrics.TradingDays)
	}

	// And the full period must be strictly longer, so the check above is not
	// vacuously true.
	full := agentBase()
	full.Code = buyAndHoldCode(0.9)
	full.OmitDayRecords = true
	fullRes, err := New(full, store).Run(context.Background())
	if err != nil {
		t.Fatalf("full run: %v", err)
	}
	if fullRes.Metrics.TradingDays <= ref.Metrics.TradingDays {
		t.Fatal("the training window is not shorter than the full period; the test proves nothing")
	}
}

func TestCandidateCarriesNoOutOfSampleField(t *testing.T) {
	// The type is part of the guarantee: a proposer cannot read what the
	// struct does not hold. This fails loudly if a future change adds one.
	c := Candidate{}
	blob := fmt.Sprintf("%+v", c)
	for _, banned := range []string{"Holdout", "Test", "OutOfSample", "Future"} {
		if strings.Contains(blob, banned) {
			t.Errorf("Candidate exposes a %q field; the proposer must not be able to see one", banned)
		}
	}
}

func TestAgentPicksTheTrainingWinnerNotTheHoldoutWinner(t *testing.T) {
	// Two candidates. The harness must choose on training score alone, even
	// though it computes a holdout run afterwards.
	codes := []string{buyAndHoldCode(0.2), buyAndHoldCode(0.95)}
	i := 0
	p := ProposerFunc(func(ctx context.Context, h []Candidate) (string, string, error) {
		if i >= len(codes) {
			return "", "", nil
		}
		c := codes[i]
		i++
		return c, "variant", nil
	})

	res, err := RunAgent(context.Background(), AgentSpec{Base: agentBase(), Budget: 4},
		newTestStore(t), p, nil)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	if res.BestIndex < 0 {
		t.Fatal("no winner chosen")
	}
	best := res.Candidates[res.BestIndex]
	for _, c := range res.Candidates {
		if c.Error != "" || !c.TrainScore.Defined() {
			continue
		}
		if c.TrainScore > best.TrainScore {
			t.Errorf("candidate %d scored better in training (%v) than the chosen winner (%v)",
				c.Iteration, float64(c.TrainScore), float64(best.TrainScore))
		}
	}
}

func TestAgentSurvivesABrokenCandidate(t *testing.T) {
	// One proposal that does not compile must not end the search.
	codes := []string{"this is not javascript {{{", buyAndHoldCode(0.9)}
	i := 0
	p := ProposerFunc(func(ctx context.Context, h []Candidate) (string, string, error) {
		if i >= len(codes) {
			return "", "", nil
		}
		c := codes[i]
		i++
		return c, "variant", nil
	})

	res, err := RunAgent(context.Background(), AgentSpec{Base: agentBase(), Budget: 4},
		newTestStore(t), p, nil)
	if err != nil {
		t.Fatalf("a broken candidate should not fail the search: %v", err)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(res.Candidates))
	}
	if res.Candidates[0].Error == "" {
		t.Error("the broken candidate should record its error")
	}
	if res.BestIndex != 1 {
		t.Errorf("the working candidate should win, got index %d", res.BestIndex)
	}
	// And the proposer must be told what went wrong, so it can react.
	if res.Candidates[0].Error == "" {
		t.Error("no error recorded for the proposer to read")
	}
}

func TestAgentReportsWhenEverythingFailed(t *testing.T) {
	p := ProposerFunc(func(ctx context.Context, h []Candidate) (string, string, error) {
		if len(h) >= 2 {
			return "", "", nil
		}
		return "syntax error {{{", "broken", nil
	})
	res, err := RunAgent(context.Background(), AgentSpec{Base: agentBase(), Budget: 3},
		newTestStore(t), p, nil)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	if res.Holdout != nil {
		t.Error("nothing worked, so the holdout must not have been touched")
	}
	if !strings.Contains(res.Verdict, "nothing to test") {
		t.Errorf("the verdict should say so plainly: %q", res.Verdict)
	}
}

func TestAgentRefusesTooLittleHistory(t *testing.T) {
	spec := agentBase()
	spec.Start, spec.End = "2022-01-03", "2022-03-01"
	p := ProposerFunc(func(ctx context.Context, h []Candidate) (string, string, error) {
		return buyAndHoldCode(0.9), "", nil
	})
	_, err := RunAgent(context.Background(), AgentSpec{Base: spec, Budget: 2},
		newTestStore(t), p, nil)
	if err == nil {
		t.Fatal("a two-month period cannot support a holdout")
	}
	if !strings.Contains(err.Error(), "holdout") && !strings.Contains(err.Error(), "history") {
		t.Errorf("the error should explain why: %v", err)
	}
}

func TestAgentHonoursItsBudget(t *testing.T) {
	calls := 0
	p := ProposerFunc(func(ctx context.Context, h []Candidate) (string, string, error) {
		calls++
		return buyAndHoldCode(0.5), "always more", nil
	})
	res, err := RunAgent(context.Background(), AgentSpec{Base: agentBase(), Budget: 3},
		newTestStore(t), p, nil)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	if calls != 3 || len(res.Candidates) != 3 {
		t.Errorf("budget of 3 produced %d proposals and %d candidates", calls, len(res.Candidates))
	}
}

func TestAgentVerdictNamesFittedImprovement(t *testing.T) {
	res := &AgentResult{
		Candidates:   make([]Candidate, 6),
		TrainStart:   "2018-01-02",
		TrainEnd:     "2022-01-03",
		HoldoutStart: "2022-01-04",
		HoldoutEnd:   "2023-12-29",
		Holdout:      &Result{Metrics: Metrics{CAGR: -0.05}},
		Degradation:  Ratio(-0.25),
	}
	v := agentVerdict(res, Candidate{TrainMetrics: Metrics{CAGR: 0.20}}, "sharpe")
	if !strings.Contains(v, "optimising against a sample") {
		t.Errorf("a negative holdout should be named plainly: %q", v)
	}
	if !strings.Contains(v, "flatters itself") {
		t.Errorf("the trial count belongs in the verdict: %q", v)
	}
}

func TestAgentProgressIsReported(t *testing.T) {
	var got []int
	p := ProposerFunc(func(ctx context.Context, h []Candidate) (string, string, error) {
		if len(h) >= 2 {
			return "", "", nil
		}
		return buyAndHoldCode(0.5), "", nil
	})
	_, err := RunAgent(context.Background(), AgentSpec{Base: agentBase(), Budget: 4},
		newTestStore(t), p, func(n, budget int, c Candidate) { got = append(got, n) })
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("progress should count candidates in order: %v", got)
	}
}

var _ = market.Day("")
