package engine

import (
	"context"
	"fmt"
	"math"

	"github.com/charbelkassab/pyrite/internal/market"
)

// WalkForwardSpec describes a rolling out-of-sample evaluation.
//
// The design opinion built into this file: a single full-period backtest with
// hand-picked parameters is the least informative thing a backtester can
// produce, and it is what every backtester produces by default. Walk-forward
// chooses parameters on data and reports on data it has never seen, which is
// the only arrangement whose numbers mean what a reader assumes they mean.
type WalkForwardSpec struct {
	Base  Spec
	Grids map[string][]any

	// TrainDays and TestDays are lengths in trading sessions.
	TrainDays int
	TestDays  int
	// Anchored grows the training window from the start of the data instead
	// of rolling it forward at a fixed length.
	Anchored bool
	// Embargo drops this many sessions between the training window and the
	// test window.
	//
	// Without it a 200-day moving average computed on the first test day is
	// mostly made of training data, and the "out-of-sample" result quietly
	// inherits the fit. Defaults to the strategy's warm-up, which is exactly
	// the horizon over which that leakage can occur.
	Embargo int

	Objective string
	Workers   int
	MaxCombos int
}

// Fold is one train/test pair.
type Fold struct {
	Index      int        `json:"index"`
	TrainStart market.Day `json:"train_start"`
	TrainEnd   market.Day `json:"train_end"`
	TestStart  market.Day `json:"test_start"`
	TestEnd    market.Day `json:"test_end"`

	// BestParams is what won in training and was then applied to the test
	// window untouched.
	BestParams map[string]any `json:"best_params"`
	// Scores are Ratios so an undefined objective marshals to null rather
	// than refusing to encode and truncating the whole response.
	TrainScore Ratio `json:"train_score"`
	TestScore  Ratio `json:"test_score"`

	TrainMetrics Metrics `json:"train_metrics"`
	TestMetrics  Metrics `json:"test_metrics"`
	// Combos is how many configurations were searched to pick BestParams.
	Combos int    `json:"combos"`
	Error  string `json:"error,omitempty"`

	// TestCurve is the fold's out-of-sample equity, rebased to the running
	// stitched value so the folds join into one continuous line.
	TestCurve []EquityPoint `json:"test_curve,omitempty"`
}

// WalkForwardResult is the whole evaluation.
type WalkForwardResult struct {
	Folds []Fold `json:"folds"`

	// Stitched is every test window joined end to end. This is the headline
	// curve: the only equity line in the tool that was never fitted to.
	Stitched        []EquityPoint `json:"stitched"`
	StitchedMetrics Metrics       `json:"stitched_metrics"`
	StitchedRisk    RiskMetrics   `json:"stitched_risk"`

	// InSampleReturn averages the training windows, for the comparison that
	// matters most.
	InSampleReturn  float64 `json:"in_sample_return"`
	OutOfSampleMean float64 `json:"out_of_sample_return"`
	// Efficiency is out-of-sample over in-sample return. Around 1 means the
	// fit transferred. Near 0, or negative, means the search was memorising.
	Efficiency Ratio `json:"efficiency"`
	// ConsistentFolds is how many test windows finished positive.
	ConsistentFolds int `json:"consistent_folds"`
	// ParamStability is how often the winning configuration stayed the same
	// between consecutive folds. A strategy whose optimum jumps every period
	// does not have an optimum.
	ParamStability float64 `json:"param_stability"`

	Objective string `json:"objective"`
	Verdict   string `json:"verdict"`
	Elapsed   int64  `json:"elapsed_ms"`
}

// TradingDays loads a spec's data and returns the sessions it would run over.
func TradingDays(ctx context.Context, spec Spec, store *market.Store) ([]market.Day, error) {
	spec.ApplyDefaults()
	spec.OmitDayRecords = true
	e := New(spec, store)
	// The calendar is built from the symbols, and the symbols may only be
	// named inside setup(), so it has to run before the load rather than
	// after it.
	if len(spec.Universe) == 0 && spec.Index == "" {
		if err := e.resolveSetup(ctx); err != nil {
			return nil, err
		}
	}
	if err := e.loadData(ctx); err != nil {
		return nil, err
	}
	out := make([]market.Day, 0, len(e.days))
	for _, d := range e.days {
		if d >= e.spec.Start {
			out = append(out, d)
		}
	}
	return out, nil
}

// RunWalkForward optimises on each training window and reports on the test
// window that follows it.
func RunWalkForward(ctx context.Context, ws WalkForwardSpec, store *market.Store, progress func(fold, total int)) (*WalkForwardResult, error) {
	ws.Base.ApplyDefaults()
	if ws.Objective == "" {
		ws.Objective = "sharpe"
	}
	score, ok := objectives[ws.Objective]
	if !ok {
		return nil, fmt.Errorf("unknown objective %q (available: %v)", ws.Objective, ObjectiveNames())
	}

	days, err := TradingDays(ctx, ws.Base, store)
	if err != nil {
		return nil, err
	}
	if ws.TrainDays <= 0 {
		ws.TrainDays = 504 // two years
	}
	if ws.TestDays <= 0 {
		ws.TestDays = 126 // half a year
	}
	if ws.Embargo < 0 {
		ws.Embargo = 0
	} else if ws.Embargo == 0 {
		ws.Embargo = ws.Base.Warmup
	}

	windows := planFolds(len(days), ws.TrainDays, ws.TestDays, ws.Embargo, ws.Anchored)
	if len(windows) == 0 {
		return nil, fmt.Errorf("not enough history for walk-forward: %d sessions cannot hold "+
			"a %d-session training window, a %d-session embargo and a %d-session test window.\n"+
			"  Widen the date range, or shorten --train / --test.",
			len(days), ws.TrainDays, ws.Embargo, ws.TestDays)
	}

	res := &WalkForwardResult{Objective: ws.Objective}
	running := ws.Base.InitialCash
	var isSum, oosSum float64
	var isN, oosN int
	var prevLabel string
	var stable, comparisons int

	for i, w := range windows {
		if progress != nil {
			progress(i+1, len(windows))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		f := Fold{
			Index:      i,
			TrainStart: days[w.trainFrom],
			TrainEnd:   days[w.trainTo],
			TestStart:  days[w.testFrom],
			TestEnd:    days[w.testTo],
		}

		// 1. Search the training window.
		trainSpec := ws.Base
		trainSpec.Start, trainSpec.End = f.TrainStart, f.TrainEnd
		sw, err := RunSweep(ctx, SweepSpec{
			Base: trainSpec, Grids: ws.Grids, Workers: ws.Workers,
			MaxCombos: ws.MaxCombos, Objective: ws.Objective,
			KeepBest: 1, PBOBlocks: -1, // no PBO per fold; it is a whole-search statistic
		}, store, nil)
		if err != nil {
			f.Error = truncateErr(err.Error())
			res.Folds = append(res.Folds, f)
			continue
		}
		f.Combos = sw.Combos
		sorted := sw.Sorted()
		if len(sorted) == 0 || sorted[0].Error != "" {
			f.Error = "no configuration completed the training window"
			res.Folds = append(res.Folds, f)
			continue
		}
		f.BestParams = sorted[0].Params
		f.TrainScore = sorted[0].Score
		if len(sw.Best) > 0 {
			f.TrainMetrics = sw.Best[0].Metrics
		}

		// 2. Apply the winner to the test window, untouched.
		testSpec := ws.Base
		testSpec.Start, testSpec.End = f.TestStart, f.TestEnd
		testSpec.Params = f.BestParams
		testSpec.InitialCash = running
		testSpec.OmitDayRecords = true
		tr, err := New(testSpec, store).Run(ctx)
		if err != nil {
			f.Error = truncateErr(err.Error())
			res.Folds = append(res.Folds, f)
			continue
		}
		f.TestMetrics = tr.Metrics
		f.TestScore = Ratio(score(tr))
		f.TestCurve = tr.Curve

		// Each fold starts where the last one finished, so the stitched line
		// is the equity of someone who actually re-optimised on this
		// schedule and traded the result.
		if n := len(tr.Curve); n > 0 {
			running = tr.Curve[n-1].Value
		}
		res.Stitched = append(res.Stitched, tr.Curve...)

		isSum += f.TrainMetrics.TotalReturn
		isN++
		oosSum += f.TestMetrics.TotalReturn
		oosN++
		if f.TestMetrics.TotalReturn > 0 {
			res.ConsistentFolds++
		}
		label := FormatParams(f.BestParams)
		if prevLabel != "" {
			comparisons++
			if label == prevLabel {
				stable++
			}
		}
		prevLabel = label

		res.Folds = append(res.Folds, f)
	}

	// The stitched curve is chained across folds, so its drawdowns are
	// recomputed here rather than inherited from any single fold.
	rebuildDrawdowns(res.Stitched)
	sc := ScaleFor(ws.Base.Interval, ws.Base.RiskFreeRate)
	res.StitchedMetrics = ComputeMetrics(res.Stitched, sc)
	res.StitchedRisk = ComputeRiskMetrics(res.Stitched, res.StitchedMetrics.CAGR, sc)
	if isN > 0 {
		res.InSampleReturn = isSum / float64(isN)
	}
	if oosN > 0 {
		res.OutOfSampleMean = oosSum / float64(oosN)
	}
	res.Efficiency = Ratio(math.NaN())
	if res.InSampleReturn != 0 {
		res.Efficiency = Ratio(res.OutOfSampleMean / res.InSampleReturn)
	}
	if comparisons > 0 {
		res.ParamStability = float64(stable) / float64(comparisons)
	}
	res.Verdict = walkForwardVerdict(res)
	return res, nil
}

// window is one fold's index ranges into the calendar.
type window struct {
	trainFrom, trainTo int
	testFrom, testTo   int
}

// planFolds lays out rolling or anchored train/test windows over n sessions.
func planFolds(n, train, test, embargo int, anchored bool) []window {
	if train <= 0 || test <= 0 || n < train+embargo+test {
		return nil
	}
	var out []window
	for start := 0; ; start += test {
		trainTo := start + train - 1
		testFrom := trainTo + 1 + embargo
		testTo := testFrom + test - 1
		if testTo >= n {
			break
		}
		from := start
		if anchored {
			from = 0
		}
		out = append(out, window{from, trainTo, testFrom, testTo})
	}
	return out
}

// rebuildDrawdowns recomputes the running peak across a concatenated curve.
func rebuildDrawdowns(curve []EquityPoint) {
	if len(curve) == 0 {
		return
	}
	peak := curve[0].Value
	for i := range curve {
		if curve[i].Value > peak {
			peak = curve[i].Value
		}
		if peak > 0 {
			curve[i].Drawdown = curve[i].Value/peak - 1
		}
		if i > 0 && curve[i-1].Value > 0 {
			curve[i].Return = curve[i].Value/curve[i-1].Value - 1
		} else {
			curve[i].Return = 0
		}
	}
}

// walkForwardVerdict states the finding in the terms a reader needs.
func walkForwardVerdict(r *WalkForwardResult) string {
	valid := 0
	for _, f := range r.Folds {
		if f.Error == "" {
			valid++
		}
	}
	if valid == 0 {
		return "no fold completed; there is no out-of-sample evidence here"
	}

	out := fmt.Sprintf("%d of %d test windows finished positive", r.ConsistentFolds, valid)
	if r.Efficiency.Defined() {
		e := float64(r.Efficiency)
		switch {
		case e < 0:
			// Reported as a percentage, matching the table above it. Printing
			// the bare ratio here made the verdict say "-0.01" beside a table
			// saying "-0.78%", which reads as two different measurements.
			out += fmt.Sprintf("; out-of-sample returns are negative against positive in-sample "+
				"ones (efficiency %.1f%%), which is the signature of a fitted strategy", e*100)
		case e < 0.35:
			out += fmt.Sprintf("; out-of-sample captured only %.0f%% of in-sample return, so most "+
				"of the backtest was the search finding the sample rather than an edge", e*100)
		case e > 0.75:
			out += fmt.Sprintf("; out-of-sample captured %.0f%% of in-sample return, which is what "+
				"a genuine edge looks like", e*100)
		default:
			out += fmt.Sprintf("; out-of-sample captured %.0f%% of in-sample return", e*100)
		}
	}
	if r.ParamStability < 0.34 && valid > 2 {
		out += fmt.Sprintf("; the winning configuration changed in %.0f%% of re-optimisations, "+
			"so the strategy does not have a stable optimum", (1-r.ParamStability)*100)
	}
	return out
}
