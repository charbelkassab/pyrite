package engine

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/charbelkassab/pyrite/internal/market"
)

// Candidate is one strategy the search considered.
//
// Note what is absent: there is no out-of-sample field. A candidate carries
// only what the proposer is allowed to see, so the type system participates in
// keeping the holdout closed rather than relying on discipline at every call
// site.
type Candidate struct {
	Iteration int    `json:"iteration"`
	Code      string `json:"code"`
	// Rationale is the proposer's own account of what it changed and why.
	Rationale string `json:"rationale,omitempty"`
	// Params is the best parameter combination found for this code, if the
	// candidate declared any.
	Params map[string]any `json:"params,omitempty"`

	TrainMetrics Metrics     `json:"train_metrics"`
	TrainRisk    RiskMetrics `json:"train_risk"`
	TrainTrades  TradeStats  `json:"train_trades"`
	TrainScore   Ratio       `json:"train_score"`
	// Critique is the deterministic assessment of the training run, which the
	// proposer is shown so it can act on specific faults rather than guess.
	Critique Critique `json:"critique"`
	Error    string   `json:"error,omitempty"`
	Elapsed  int64    `json:"elapsed_ms"`
}

// Proposer suggests the next strategy to try.
//
// It is an interface rather than a direct model call so the harness can be
// tested without one, and so the discipline below is provably independent of
// what is proposing.
type Proposer interface {
	// Propose returns the next candidate's code. history holds every previous
	// attempt, best first is not guaranteed — the order is chronological.
	// Returning an empty string ends the search.
	Propose(ctx context.Context, history []Candidate) (code, rationale string, err error)
}

// ProposerFunc adapts a function to the Proposer interface.
type ProposerFunc func(ctx context.Context, history []Candidate) (string, string, error)

// Propose implements Proposer.
func (f ProposerFunc) Propose(ctx context.Context, h []Candidate) (string, string, error) {
	return f(ctx, h)
}

// AgentSpec configures a guided search.
type AgentSpec struct {
	Base Spec
	// Budget caps how many candidates are tried.
	Budget int
	// HoldoutFraction is the tail of the period withheld from the search.
	// Default 0.3.
	HoldoutFraction float64
	// Objective ranks candidates during the search.
	Objective string
	// SweepParams searches each candidate's declared parameters on the
	// training window as well, so the proposer competes on its best setting
	// rather than its default one.
	SweepParams bool
	MaxCombos   int
	Workers     int
}

// AgentResult is the outcome, including the single holdout measurement.
type AgentResult struct {
	Candidates []Candidate `json:"candidates"`
	// BestIndex points into Candidates. Chosen on training data only.
	BestIndex int `json:"best_index"`

	// TrainStart..TrainEnd is what the search could see; HoldoutStart..End is
	// what it could not.
	TrainStart   market.Day `json:"train_start"`
	TrainEnd     market.Day `json:"train_end"`
	HoldoutStart market.Day `json:"holdout_start"`
	HoldoutEnd   market.Day `json:"holdout_end"`

	// Holdout is the winner scored once, after the search closed. It is the
	// only number in this struct that means anything about the future.
	Holdout *Result `json:"holdout,omitempty"`
	// Degradation is holdout return over training return. Near 1 means the
	// improvement transferred; near 0 means the search fitted the sample.
	Degradation Ratio  `json:"degradation"`
	Verdict     string `json:"verdict"`
	Elapsed     int64  `json:"elapsed_ms"`
}

// RunAgent searches for a better strategy under a fixed budget, and scores the
// winner exactly once on data the search never saw.
//
// The wall is the whole feature. An agent that proposes variants, measures
// them, and proposes again is a very effective optimiser — including at
// optimising for the sample it is being measured on. Shown the full period, it
// would reliably produce a strategy with an excellent backtest and no edge,
// and the backtest would look like evidence.
//
// So the harness, not the proposer, owns the split: candidates are only ever
// run over the training window, the Candidate type carries no out-of-sample
// field for a proposer to read, and the holdout is touched once, at the end,
// after the search has closed.
func RunAgent(ctx context.Context, as AgentSpec, store *market.Store, p Proposer,
	progress func(n, budget int, c Candidate)) (*AgentResult, error) {
	started := time.Now()
	as.Base.ApplyDefaults()
	if as.Budget <= 0 {
		as.Budget = 8
	}
	if as.HoldoutFraction <= 0 || as.HoldoutFraction >= 1 {
		as.HoldoutFraction = 0.3
	}
	if as.Objective == "" {
		as.Objective = "sharpe"
	}
	score, ok := objectives[as.Objective]
	if !ok {
		return nil, fmt.Errorf("unknown objective %q (available: %v)", as.Objective, ObjectiveNames())
	}

	days, err := TradingDays(ctx, as.Base, store)
	if err != nil {
		return nil, err
	}
	if len(days) < 100 {
		return nil, fmt.Errorf("only %d sessions available; a guided search needs enough "+
			"history to hold out a meaningful test period", len(days))
	}
	split := int(float64(len(days)) * (1 - as.HoldoutFraction))
	if split < 60 || len(days)-split < 30 {
		return nil, fmt.Errorf("the period cannot be split into a usable train and holdout")
	}

	res := &AgentResult{
		BestIndex:    -1,
		TrainStart:   days[0],
		TrainEnd:     days[split-1],
		HoldoutStart: days[split],
		HoldoutEnd:   days[len(days)-1],
		Degradation:  Ratio(math.NaN()),
	}

	trainSpec := as.Base
	trainSpec.Start, trainSpec.End = res.TrainStart, res.TrainEnd
	trainSpec.OmitDayRecords = true

	bestScore := math.Inf(-1)
	for i := 0; i < as.Budget; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		code, rationale, err := p.Propose(ctx, res.Candidates)
		if err != nil {
			return nil, fmt.Errorf("proposing candidate %d: %w", i+1, err)
		}
		if code == "" {
			break // the proposer has nothing further to offer
		}

		c := Candidate{Iteration: i + 1, Code: code, Rationale: rationale,
			TrainScore: Ratio(math.NaN())}
		iterStart := time.Now()

		spec := trainSpec
		spec.Code = code
		if as.SweepParams {
			// Competing on its best setting rather than its default one is
			// the fair comparison, and it stays inside the training window.
			sw, serr := RunSweep(ctx, SweepSpec{
				Base: spec, Objective: as.Objective, MaxCombos: as.MaxCombos,
				Workers: as.Workers, KeepBest: 1, PBOBlocks: -1,
			}, store, nil)
			switch {
			case serr != nil:
				// A strategy with no declared parameters is not an error here,
				// it just has nothing to sweep. Fall through to a plain run.
				c.Error = ""
			case len(sw.Best) > 0:
				c.Params = sw.Best[0].ParamValues
				spec.Params = c.Params
			}
		}

		r, rerr := New(spec, store).Run(ctx)
		if rerr != nil {
			c.Error = truncateErr(rerr.Error())
		} else {
			c.TrainMetrics = r.Metrics
			c.TrainRisk = r.Risk
			c.TrainTrades = r.TradeStats
			c.TrainScore = Ratio(score(r))
			c.Critique = r.Critique
			if c.TrainScore.Defined() && float64(c.TrainScore) > bestScore {
				bestScore = float64(c.TrainScore)
				res.BestIndex = len(res.Candidates)
			}
		}
		c.Elapsed = time.Since(iterStart).Milliseconds()
		res.Candidates = append(res.Candidates, c)
		if progress != nil {
			progress(i+1, as.Budget, c)
		}
	}

	if res.BestIndex < 0 {
		res.Verdict = "no candidate completed a training run, so there is nothing to test"
		res.Elapsed = time.Since(started).Milliseconds()
		return res, nil
	}

	// The search is now closed. This is the only time the holdout is read.
	best := res.Candidates[res.BestIndex]
	holdSpec := as.Base
	holdSpec.Code = best.Code
	holdSpec.Params = best.Params
	holdSpec.Start, holdSpec.End = res.HoldoutStart, res.HoldoutEnd

	hold, err := New(holdSpec, store).Run(ctx)
	if err != nil {
		res.Verdict = "the winning candidate failed on the holdout period: " + truncateErr(err.Error())
		res.Elapsed = time.Since(started).Milliseconds()
		return res, nil
	}
	res.Holdout = hold
	if best.TrainMetrics.CAGR != 0 {
		res.Degradation = Ratio(hold.Metrics.CAGR / best.TrainMetrics.CAGR)
	}
	res.Verdict = agentVerdict(res, best, as.Objective)
	res.Elapsed = time.Since(started).Milliseconds()
	return res, nil
}

// agentVerdict states what the single holdout measurement showed.
func agentVerdict(res *AgentResult, best Candidate, objective string) string {
	tried := len(res.Candidates)
	out := fmt.Sprintf("%d candidates tried on %s to %s; the winner was chosen on that "+
		"period alone and scored once on %s to %s",
		tried, res.TrainStart, res.TrainEnd, res.HoldoutStart, res.HoldoutEnd)

	h := res.Holdout.Metrics
	out += fmt.Sprintf(". In-sample CAGR %.1f%%, out-of-sample %.1f%%",
		best.TrainMetrics.CAGR*100, h.CAGR*100)

	if res.Degradation.Defined() {
		d := float64(res.Degradation)
		switch {
		case d <= 0:
			out += ". The search improved a period it could see and lost money on the one " +
				"it could not, which is what optimising against a sample looks like"
		case d < 0.4:
			out += fmt.Sprintf(". Only %.0f%% of the improvement survived, so most of what "+
				"the search found belonged to the training window", d*100)
		case d > 0.8:
			out += fmt.Sprintf(". %.0f%% of it survived, which is what a real improvement "+
				"looks like", d*100)
		default:
			out += fmt.Sprintf(". %.0f%% of it survived", d*100)
		}
	}
	// Searching harder makes the winner look better whether or not anything
	// is there, so the trial count belongs in the sentence.
	if tried >= 5 {
		out += fmt.Sprintf(". Remember that the best of %d attempts flatters itself even "+
			"when none of them has an edge", tried)
	}
	return out
}
