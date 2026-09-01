package engine

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/charbelkassab/pyrite/internal/market"
)

// CPCVSpec describes a combinatorial purged cross-validation.
//
// Walk-forward produces exactly one out-of-sample path: train on 1-2, test on
// 3, train on 2-3, test on 4. One path is one draw, and whether that draw was
// lucky is not answerable from the draw itself — which is the one question a
// reader of a single out-of-sample number most needs answered.
//
// This cuts the period into groups instead and holds out every combination of
// them in turn. Each group is tested several times, under the parameters that
// won on several different training sets, so the same data reconstructs
// several distinct full-length out-of-sample paths. The answer is then a
// distribution, and the spread of that distribution is the evidence
// walk-forward structurally cannot produce.
type CPCVSpec struct {
	Base  Spec
	Grids map[string][]any

	// Groups is how many contiguous blocks of sessions the period is cut
	// into. Default 6.
	Groups int
	// TestGroups is how many blocks are held out per split. Default 2, which
	// gives C(6,2) = 15 splits and C(5,1) = 5 full-length paths.
	//
	// Raising it buys more splits and more paths at the cost of a smaller
	// training set behind each one, which is the same trade every
	// cross-validation makes.
	TestGroups int
	// Embargo withholds this many sessions from the training set on each
	// side of every test group.
	//
	// Same reasoning and same default as walk-forward: a 200-day moving
	// average computed on the first test session is mostly made of training
	// data unless the sessions immediately before it are withheld, and the
	// horizon over which that can happen is exactly the strategy's warm-up.
	// Zero means the warm-up; negative means none at all.
	//
	// It applies on both sides here because walk-forward only ever has
	// training data before its test window, while a held-out group has
	// training data after it too, and an indicator computed on the session
	// after a test group is made of test data in the same way.
	Embargo int

	Objective string
	Workers   int
	MaxCombos int

	// TrainDays and TestDays size the walk-forward path the distribution is
	// compared against; zero uses walk-forward's own defaults.
	TrainDays int
	TestDays  int
	// SkipWalkForward drops that comparison. It is the single most useful
	// line the command produces, so it is on unless asked otherwise.
	SkipWalkForward bool
}

// CPCVGroup is one contiguous block of the calendar.
type CPCVGroup struct {
	Index    int        `json:"index"`
	Start    market.Day `json:"start"`
	End      market.Day `json:"end"`
	Sessions int        `json:"sessions"`
}

// CPCVSplit is one partition: these groups held out, the rest trained on.
type CPCVSplit struct {
	Index      int   `json:"index"`
	TestGroups []int `json:"test_groups"`

	// TrainSessions is what survived purging, and PurgedSessions is what did
	// not. The second number is worth printing: it is the cost of taking the
	// leakage seriously, and a reader who does not see it will assume the
	// training set was the whole remainder.
	TrainSessions  int `json:"train_sessions"`
	TestSessions   int `json:"test_sessions"`
	PurgedSessions int `json:"purged_sessions"`

	// BestParams won on the purged training set and was then applied to the
	// held-out groups untouched.
	BestParams map[string]any `json:"best_params"`
	// Scores and returns are Ratios so a split that failed marshals to null
	// rather than claiming a real zero.
	TrainScore  Ratio  `json:"train_score"`
	TestScore   Ratio  `json:"test_score"`
	TrainReturn Ratio  `json:"train_return"`
	TestReturn  Ratio  `json:"test_return"`
	Error       string `json:"error,omitempty"`

	// winner indexes the combination that won in training, and oos holds
	// every combination's score on the held-out groups. Both are working
	// state for the overfitting statistic and the path reconstruction, and
	// neither is worth carrying into the JSON: oos is one float per
	// combination per split, which is the largest thing in the evaluation.
	winner int
	oos    []float64
	// closed and barsHeld are the winner's round trips on the held-out
	// groups, kept so the report can compare the average trade against the
	// length of the window it was measured in.
	closed   int
	barsHeld float64
}

// CPCVPath is one reconstructed out-of-sample path.
//
// Every group appears exactly once, each under the parameters chosen by a
// split that did not see it, so the path covers the whole period and no part
// of it was fitted to.
type CPCVPath struct {
	Index int `json:"index"`
	// Splits records which split supplied each group, in group order, so a
	// path can be traced back to the parameters that produced it.
	Splits  []int         `json:"splits"`
	Start   market.Day    `json:"start"`
	End     market.Day    `json:"end"`
	Metrics Metrics       `json:"metrics"`
	Curve   []EquityPoint `json:"curve,omitempty"`
	Error   string        `json:"error,omitempty"`
}

// PathSpread summarises one statistic across the reconstructed paths.
//
// Every field is a Ratio because with too few valid paths none of them exist,
// and a zero in that position reads as a measurement rather than as its
// absence.
type PathSpread struct {
	Mean   Ratio `json:"mean"`
	Median Ratio `json:"median"`
	Stdev  Ratio `json:"stdev"`
	P05    Ratio `json:"p05"`
	P95    Ratio `json:"p95"`
	Worst  Ratio `json:"worst"`
	Best   Ratio `json:"best"`
}

// CPCVWalkForward places the single walk-forward path inside the distribution.
type CPCVWalkForward struct {
	Start market.Day `json:"start"`
	End   market.Day `json:"end"`

	TotalReturn Ratio `json:"total_return"`
	CAGR        Ratio `json:"cagr"`
	Sharpe      Ratio `json:"sharpe"`
	// Percentiles are the share of CPCV paths that finished below the
	// walk-forward path on the same statistic. Return is deliberately absent:
	// walk-forward's first training window is not out of sample, so its path
	// is shorter than a CPCV path and their total returns are not comparable.
	// CAGR and Sharpe are per-unit-time and are.
	CAGRPercentile   Ratio `json:"cagr_percentile"`
	SharpePercentile Ratio `json:"sharpe_percentile"`

	Folds         int `json:"folds"`
	PositiveFolds int `json:"positive_folds"`
}

// CPCVResult is the whole evaluation.
type CPCVResult struct {
	Groups      int         `json:"groups"`
	TestGroups  int         `json:"test_groups"`
	Embargo     int         `json:"embargo"`
	Sessions    int         `json:"sessions"`
	Combos      int         `json:"combos"`
	GroupBounds []CPCVGroup `json:"group_bounds"`

	Splits []CPCVSplit `json:"splits"`
	Paths  []CPCVPath  `json:"paths"`

	// The distribution, which is the reason to run this rather than a
	// walk-forward.
	Return      PathSpread `json:"return"`
	CAGR        PathSpread `json:"cagr"`
	Sharpe      PathSpread `json:"sharpe"`
	MaxDrawdown PathSpread `json:"max_drawdown"`

	// NoSelection is the same six group runs chained without choosing
	// anything: one configuration held for the whole period, once per
	// combination. It is the bar the selection has to clear, and the gap
	// between it and the paths is the only part of the result the search can
	// claim credit for. Without it a distribution of profitable paths reads
	// as a working search when it may only be a rising market.
	NoSelection PathSpread `json:"no_selection"`

	ValidPaths      int `json:"valid_paths"`
	ProfitablePaths int `json:"profitable_paths"`
	// WorstPath indexes into Paths, or -1.
	WorstPath int `json:"worst_path"`
	// AvgBarsHeld is how long the average round trip lasted out of sample.
	// It is here because it decides how much of any out-of-sample scheme's
	// answer is the scheme: a window shorter than the average trade measures
	// the window.
	AvgBarsHeld float64 `json:"avg_bars_held"`
	// GroupSessions and WalkForwardTestDays are the two out-of-sample window
	// lengths being compared, for the same reason.
	GroupSessions       int `json:"group_sessions"`
	WalkForwardTestDays int `json:"walk_forward_test_days"`

	// PBO is the probability of backtest overfitting over these splits: how
	// often the configuration that won on the purged training set landed
	// below median on the groups it had never seen.
	PBO       Ratio `json:"pbo"`
	PBOSplits int   `json:"pbo_splits"`
	// BlockPBO is the same statistic the sweep already reports, computed the
	// way the sweep computes it — an even cut into blocks, half train and
	// half test, with no purging and no embargo. It is here to be compared
	// with the number above rather than trusted on its own.
	BlockPBO       Ratio `json:"block_pbo"`
	BlockPBOSplits int   `json:"block_pbo_splits"`

	WalkForward *CPCVWalkForward `json:"walk_forward,omitempty"`

	Objective string `json:"objective"`
	Failed    int    `json:"failed"`
	Verdict   string `json:"verdict"`
	Elapsed   int64  `json:"elapsed_ms"`
}

// groupRange is one group's inclusive index range into the calendar.
type groupRange struct{ from, to int }

// cpcvRun is one combination's backtest over one group.
//
// Only the returns survive, not the curve: the same group run is reused by
// every split and every path that contains it, and holding N x G full results
// is how a laptop runs out of memory in the middle of a search.
type cpcvRun struct {
	rets   []float64
	fills  []Fill
	trades []Trade
	err    string
}

// cpcvState is the shared, read-only-after-build working set.
type cpcvState struct {
	spec   Spec
	sc     Scale
	days   []market.Day
	bounds []groupRange
	// runs is indexed [combination][group].
	runs [][]cpcvRun
}

// RunCPCV holds out every combination of groups and reports the distribution
// of the out-of-sample paths that reconstructs.
func RunCPCV(ctx context.Context, cs CPCVSpec, store *market.Store, progress func(done, total int)) (*CPCVResult, error) {
	started := time.Now()
	cs.Base.ApplyDefaults()
	if cs.Objective == "" {
		cs.Objective = "sharpe"
	}
	score, ok := objectives[cs.Objective]
	if !ok {
		return nil, fmt.Errorf("unknown objective %q (available: %v)", cs.Objective, ObjectiveNames())
	}
	if cs.Groups <= 0 {
		cs.Groups = 6
	}
	if cs.TestGroups <= 0 {
		cs.TestGroups = 2
	}
	if cs.Workers <= 0 {
		cs.Workers = runtime.NumCPU()
	}
	if cs.MaxCombos <= 0 {
		cs.MaxCombos = 5000
	}
	if cs.Groups < 3 {
		return nil, fmt.Errorf("--groups must be at least 3; with %d there is no combination to take",
			cs.Groups)
	}
	if cs.TestGroups >= cs.Groups {
		return nil, fmt.Errorf("--test-groups (%d) must be fewer than --groups (%d), "+
			"or there is nothing left to train on", cs.TestGroups, cs.Groups)
	}
	days, err := TradingDays(ctx, cs.Base, store)
	if err != nil {
		return nil, err
	}
	// Thirty sessions is the floor the deflated Sharpe already uses for a
	// return series, for the same reason: below it the mean and the deviation
	// of the segment are noise and every statistic built on them inherits it.
	const minGroupSessions = 30
	if len(days) < cs.Groups*minGroupSessions {
		return nil, fmt.Errorf("not enough history for %d groups: %d sessions leaves %d per group, "+
			"below the %d needed to measure anything.\n  Widen the date range, or lower --groups.",
			cs.Groups, len(days), len(days)/cs.Groups, minGroupSessions)
	}
	bounds := planGroups(len(days), cs.Groups)

	decls, warmup, err := declaredSetup(ctx, cs.Base, store)
	if err != nil {
		return nil, err
	}
	// The embargo has to cover the strategy's real warm-up, and a strategy is
	// free to declare that inside setup() rather than in the spec it was
	// handed. Reading the spec alone left a strategy loaded from a file
	// purging nothing at all, which is the one thing this command must not do
	// quietly.
	if warmup > cs.Base.Warmup {
		cs.Base.Warmup = warmup
	}
	cs.Embargo = resolveEmbargo(cs.Embargo, cs.Base.Warmup)

	decls = mergeGrids(decls, cs.Grids)
	combos, err := Combos(decls, cs.MaxCombos)
	if err != nil {
		return nil, err
	}

	state := &cpcvState{
		spec:   cs.Base,
		sc:     ScaleFor(cs.Base.Interval, cs.Base.RiskFreeRate),
		days:   days,
		bounds: bounds,
		runs:   make([][]cpcvRun, len(combos)),
	}
	for i := range state.runs {
		state.runs[i] = make([]cpcvRun, cs.Groups)
	}

	res := &CPCVResult{
		Groups: cs.Groups, TestGroups: cs.TestGroups, Embargo: cs.Embargo,
		Sessions: len(days), Combos: len(combos), Objective: cs.Objective,
		WorstPath: -1,
		PBO:       Ratio(math.NaN()), BlockPBO: Ratio(math.NaN()),
	}
	for i, b := range bounds {
		res.GroupBounds = append(res.GroupBounds, CPCVGroup{
			Index: i, Start: days[b.from], End: days[b.to], Sessions: b.to - b.from + 1,
		})
	}

	failed, err := runGroupBacktests(ctx, cs, state, combos, store, progress)
	if err != nil {
		return nil, err
	}
	res.Failed = failed

	splits := combinations(cs.Groups, cs.TestGroups)
	res.Splits = scoreSplits(ctx, cs, state, combos, splits, score)
	res.PBO, res.PBOSplits = purgedPBO(res.Splits)

	res.Paths = buildPaths(state, splits, res.Splits)
	summarisePaths(res)
	res.NoSelection = state.fixedConfigSpread()
	res.GroupSessions = len(days) / cs.Groups
	res.AvgBarsHeld = avgBarsHeld(res.Splits)

	// The sweep's own statistic, computed on the same runs so that the
	// comparison costs nothing. Chaining the group runs is not identical to
	// one continuous backtest — the strategy starts flat in each group — but
	// it is the same series the splits above were scored on, which is what
	// makes the two numbers comparable at all.
	var rb Robustness
	rb.AddPBO(state.fullMatrix(), 8)
	res.BlockPBO, res.BlockPBOSplits = rb.PBO, rb.PBOSplits

	if !cs.SkipWalkForward {
		res.WalkForwardTestDays = cs.TestDays
		if res.WalkForwardTestDays <= 0 {
			res.WalkForwardTestDays = defaultTestDays
		}
		res.WalkForward = compareWalkForward(ctx, cs, store, res)
	}

	res.Verdict = cpcvVerdict(res)
	res.Elapsed = time.Since(started).Milliseconds()
	return res, nil
}

// planGroups cuts n sessions into g contiguous blocks.
//
// The remainder is spread across the blocks rather than dumped on the last
// one, which would otherwise carry up to g-1 extra sessions and make one
// segment of every path systematically longer than the rest. This is the same
// arithmetic the sweep's block partition uses, so the two agree on where a
// boundary falls.
func planGroups(n, g int) []groupRange {
	if g < 2 || n < g {
		return nil
	}
	out := make([]groupRange, g)
	for i := 0; i < g; i++ {
		out[i] = groupRange{from: i * n / g, to: (i+1)*n/g - 1}
	}
	return out
}

// purgeMask marks the sessions withheld from training for one split.
//
// The mask always grows outward from a test group's edge, so within any
// training group it covers a prefix, a suffix, or both — never a hole in the
// middle. Pooling relies on that.
func purgeMask(n int, bounds []groupRange, test []int, embargo int) []bool {
	mask := make([]bool, n)
	if embargo <= 0 {
		return mask
	}
	for _, g := range test {
		b := bounds[g]
		for i := b.from - embargo; i < b.from; i++ {
			if i >= 0 {
				mask[i] = true
			}
		}
		for i := b.to + 1; i <= b.to+embargo && i < n; i++ {
			mask[i] = true
		}
	}
	return mask
}

// runGroupBacktests runs every combination over every group.
//
// The work is keyed by combination and group rather than by split, which is
// the whole reason this is affordable: a group's run is shared by every split
// that contains it and by every path that uses it, so running per split would
// repeat each backtest C(groups-1, test-1) times for an identical answer.
//
// The parallelism is the sweep's, for the sweep's reason: market.Store is
// read-only once loaded and guards itself, so the workers share one copy of
// the price data with nothing to coordinate. Results are written to fixed
// indices, so the output does not depend on which worker finished first.
func runGroupBacktests(ctx context.Context, cs CPCVSpec, state *cpcvState,
	combos []map[string]any, store *market.Store, progress func(done, total int)) (int, error) {

	total := len(combos) * cs.Groups
	var (
		mu      sync.Mutex
		done    int
		failed  int
		firstEr error
	)

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < cs.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}
				ci, gi := j/cs.Groups, j%cs.Groups
				b := state.bounds[gi]

				spec := cs.Base
				spec.Params = combos[ci]
				spec.Start, spec.End = state.days[b.from], state.days[b.to]
				spec.OmitDayRecords = true
				r, err := New(spec, store).Run(ctx)

				var run cpcvRun
				if err != nil {
					run.err = truncateErr(err.Error())
				} else {
					run.rets = segmentReturns(r.Curve, spec.InitialCash, state.days[b.from:b.to+1])
					run.fills, run.trades = r.Fills, r.Trades
				}

				mu.Lock()
				state.runs[ci][gi] = run
				done++
				if run.err != "" {
					failed++
					if firstEr == nil {
						firstEr = err
					}
				}
				if progress != nil {
					progress(done, total)
				}
				mu.Unlock()
			}
		}()
	}
	for j := 0; j < total; j++ {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return 0, ctx.Err()
		case jobs <- j:
		}
	}
	close(jobs)
	wg.Wait()

	// Everything failing is a broken strategy, not a result, so say what went
	// wrong rather than printing an empty distribution.
	if failed == total && firstEr != nil {
		return failed, fmt.Errorf("every group failed; first error: %w", firstEr)
	}
	return failed, nil
}

// segmentReturns converts a group's run into one growth rate per session of
// that group.
//
// Aligned on the calendar rather than on position: a run that came back a
// session short would otherwise shift every later return by one and quietly
// misdate the whole path. The first rate is measured from the run's opening
// cash, so the group's first session is not silently dropped.
func segmentReturns(curve []EquityPoint, base float64, days []market.Day) []float64 {
	out := make([]float64, len(days))
	if base <= 0 {
		return out
	}
	byDate := make(map[market.Day]float64, len(curve))
	for _, p := range curve {
		byDate[p.Date] = p.Value
	}
	prev := base
	for i, d := range days {
		v, ok := byDate[d]
		if !ok || v <= 0 || prev <= 0 {
			continue
		}
		out[i] = v/prev - 1
		prev = v
	}
	return out
}

// pool joins a combination's per-group segments into one series that can be
// scored as though it were a single run.
//
// Re-running the strategy over the union of the training groups is not an
// option: the union is not a date range, and the engine runs date ranges.
// Chaining the per-group returns is the same arrangement walk-forward uses to
// join its folds, and it is what makes a group's run reusable across every
// split and path that contains it.
//
// The pooled training series skips the purged sessions, so its calendar has
// gaps and its CAGR is spread over more elapsed time than it earned in. That
// bias is identical for every combination scored on the same split, so it
// cannot change which one wins — and the paths, which are what gets reported,
// are contiguous and carry no such gap.
func (c *cpcvState) pool(combo int, groups []int, skip []bool) (curve []EquityPoint, fills []Fill, trades []Trade, ok bool) {
	value := c.spec.InitialCash
	for _, g := range groups {
		run := c.runs[combo][g]
		if run.err != "" {
			return nil, nil, nil, false
		}
		b := c.bounds[g]
		from, to := b.from, b.to
		if skip != nil {
			for from <= to && skip[from] {
				from++
			}
			for to >= from && skip[to] {
				to--
			}
		}
		if from > to {
			continue
		}

		for i := from; i <= to; i++ {
			value *= 1 + run.rets[i-b.from]
			curve = append(curve, EquityPoint{Date: c.days[i], Value: value})
		}
		lo, hi := c.days[from], c.days[to]
		for _, f := range run.fills {
			if f.Date >= lo && f.Date <= hi {
				fills = append(fills, f)
			}
		}
		// Filtered on entry, not on both legs: requiring the exit to fall
		// inside the window too would drop every trade still open at a group
		// boundary and bias the statistics towards short holds.
		for _, t := range run.trades {
			if t.EntryDate >= lo && t.EntryDate <= hi {
				trades = append(trades, t)
			}
		}
	}
	return curve, fills, trades, true
}

// evaluate builds the Result an objective function reads.
func (c *cpcvState) evaluate(curve []EquityPoint, fills []Fill, trades []Trade) *Result {
	rebuildDrawdowns(curve)
	res := &Result{Spec: c.spec, Curve: curve, Fills: fills, Trades: trades}
	res.Metrics = ComputeMetrics(curve, c.sc)
	res.Metrics.AddTradeStats(fills, avgEquity(curve))
	res.Risk = ComputeRiskMetrics(curve, res.Metrics.CAGR, c.sc)
	res.TradeStats = ComputeTradeStats(trades)
	return res
}

// fullMatrix is one row per combination covering every session, which is the
// shape combinatorially symmetric cross-validation reads.
func (c *cpcvState) fullMatrix() [][]float64 {
	out := make([][]float64, 0, len(c.runs))
	for ci := range c.runs {
		full := make([]float64, 0, len(c.days))
		ok := true
		for g := range c.bounds {
			if c.runs[ci][g].err != "" {
				ok = false
				break
			}
			full = append(full, c.runs[ci][g].rets...)
		}
		if ok {
			out = append(out, full)
		}
	}
	return out
}

// fixedConfigSpread is what every combination returned over the whole period
// with no selection at all, chained across the same group boundaries the paths
// are chained across.
//
// Same resets, same friction, same period: the only difference between this
// and a path is that a path chose. That makes the two directly comparable,
// which is the point — a distribution of profitable paths says nothing on its
// own if holding any configuration blindly would have returned as much.
func (c *cpcvState) fixedConfigSpread() PathSpread {
	rows := c.fullMatrix()
	totals := make([]float64, 0, len(rows))
	for _, row := range rows {
		v := 1.0
		for _, r := range row {
			v *= 1 + r
		}
		totals = append(totals, v-1)
	}
	return spreadOf(totals)
}

// scoreSplits picks a winner on each purged training set and applies it to the
// groups that split never saw.
//
// Parallel across splits, and deterministic for the same reason the backtests
// above are: each worker writes one fixed index and nothing is accumulated in
// completion order.
func scoreSplits(ctx context.Context, cs CPCVSpec, state *cpcvState, combos []map[string]any,
	splits [][]int, score func(*Result) float64) []CPCVSplit {

	out := make([]CPCVSplit, len(splits))
	jobs := make(chan int)
	var wg sync.WaitGroup
	workers := cs.Workers
	if workers > len(splits) {
		workers = len(splits)
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}
				out[s] = scoreSplit(cs, state, combos, splits[s], s, score)
			}
		}()
	}
	for s := range splits {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return out
		case jobs <- s:
		}
	}
	close(jobs)
	wg.Wait()
	return out
}

// scoreSplit is one train/test partition, start to finish.
func scoreSplit(cs CPCVSpec, state *cpcvState, combos []map[string]any,
	test []int, index int, score func(*Result) float64) CPCVSplit {

	sp := CPCVSplit{
		Index: index, TestGroups: append([]int(nil), test...), winner: -1,
		TrainScore: Ratio(math.NaN()), TestScore: Ratio(math.NaN()),
		TrainReturn: Ratio(math.NaN()), TestReturn: Ratio(math.NaN()),
	}
	inTest := make([]bool, cs.Groups)
	for _, g := range test {
		inTest[g] = true
		sp.TestSessions += state.bounds[g].to - state.bounds[g].from + 1
	}
	train := make([]int, 0, cs.Groups-len(test))
	for g := 0; g < cs.Groups; g++ {
		if !inTest[g] {
			train = append(train, g)
		}
	}
	mask := purgeMask(len(state.days), state.bounds, test, cs.Embargo)

	// Counted from the mask rather than from any one combination's pooled
	// series: the split's shape is the same whichever configuration is being
	// scored, and reading it off combination zero would report nothing at all
	// on the day combination zero happens to fail.
	for _, g := range train {
		b := state.bounds[g]
		for i := b.from; i <= b.to; i++ {
			if mask[i] {
				sp.PurgedSessions++
			} else {
				sp.TrainSessions++
			}
		}
	}

	// The out-of-sample score of every combination, not just the winner's:
	// the overfitting statistic is a question about where the winner ranked,
	// which cannot be answered from the winner alone.
	sp.oos = make([]float64, len(combos))
	for i := range sp.oos {
		sp.oos[i] = math.Inf(-1)
	}

	best, bestScore := -1, math.Inf(-1)
	for ci := range combos {
		// Thirty sessions again, and for the reason given where the group
		// floor is set: a training set below it cannot rank anything.
		if curve, fills, trades, ok := state.pool(ci, train, mask); ok && len(curve) >= 30 {
			if s := score(state.evaluate(curve, fills, trades)); !math.IsNaN(s) && s > bestScore {
				best, bestScore = ci, s
			}
		}
		if c, f, t, ok := state.pool(ci, test, nil); ok && len(c) >= 2 {
			if s := score(state.evaluate(c, f, t)); !math.IsNaN(s) {
				sp.oos[ci] = s
			}
		}
	}
	if best < 0 {
		if sp.TrainSessions == 0 {
			sp.Error = "purging left no usable training data for this split"
		} else {
			sp.Error = "no configuration scored on this split's training data"
		}
		return sp
	}

	sp.BestParams = combos[best]
	sp.TrainScore = Ratio(bestScore)
	sp.winner = best
	if curve, fills, trades, ok := state.pool(best, train, mask); ok {
		sp.TrainReturn = Ratio(state.evaluate(curve, fills, trades).Metrics.TotalReturn)
	}
	if curve, fills, trades, ok := state.pool(best, test, nil); ok {
		r := state.evaluate(curve, fills, trades)
		sp.TestReturn = Ratio(r.Metrics.TotalReturn)
		sp.TestScore = Ratio(sp.oos[best])
		sp.closed, sp.barsHeld = r.TradeStats.Closed, r.TradeStats.AvgBarsHeld
	}
	return sp
}

// avgBarsHeld is how long the average out-of-sample round trip lasted, over
// every split that produced one.
func avgBarsHeld(splits []CPCVSplit) float64 {
	var bars float64
	var n int
	for _, sp := range splits {
		if sp.Error != "" || sp.closed == 0 {
			continue
		}
		bars += sp.barsHeld * float64(sp.closed)
		n += sp.closed
	}
	if n == 0 {
		return 0
	}
	return bars / float64(n)
}

// purgedPBO is the probability of backtest overfitting over the purged
// splits: how often the configuration that won in training landed in the
// bottom half out of sample.
//
// It is the same reading as the sweep's, over different partitions. Selection
// that carries no information puts the winner below median half the time,
// which is what a value near 0.5 says.
func purgedPBO(splits []CPCVSplit) (Ratio, int) {
	var below, total int
	for _, sp := range splits {
		if sp.Error != "" || sp.winner < 0 || len(sp.oos) < 2 {
			continue
		}
		if belowMedianRank(sp.oos, sp.winner) {
			below++
		}
		total++
	}
	if total == 0 {
		return Ratio(math.NaN()), 0
	}
	return Ratio(float64(below) / float64(total)), total
}

// buildPaths reassembles the split results into full-length paths.
//
// Every group is held out by C(groups-1, test-1) different splits. Taking the
// first such split's parameters for every group gives one path, the second
// gives another, and so on — each covering the whole period, none of it fitted
// to. That is the arithmetic that turns one out-of-sample number into a
// distribution.
func buildPaths(state *cpcvState, splits [][]int, scored []CPCVSplit) []CPCVPath {
	groups := len(state.bounds)
	testedBy := make([][]int, groups)
	for s, test := range splits {
		for _, g := range test {
			testedBy[g] = append(testedBy[g], s)
		}
	}
	if len(testedBy) == 0 || len(testedBy[0]) == 0 {
		return nil
	}
	n := len(testedBy[0])

	out := make([]CPCVPath, 0, n)
	for p := 0; p < n; p++ {
		path := CPCVPath{Index: p, Splits: make([]int, groups)}
		value := state.spec.InitialCash
		curve := make([]EquityPoint, 0, len(state.days))
		for g := 0; g < groups; g++ {
			s := testedBy[g][p]
			path.Splits[g] = s
			sp := scored[s]
			if sp.Error != "" || sp.winner < 0 {
				path.Error = fmt.Sprintf("split %d supplied no parameters for group %d", s, g)
				break
			}
			run := state.runs[sp.winner][g]
			if run.err != "" {
				path.Error = fmt.Sprintf("group %d failed under the parameters split %d chose", g, s)
				break
			}
			b := state.bounds[g]
			for i := b.from; i <= b.to; i++ {
				value *= 1 + run.rets[i-b.from]
				curve = append(curve, EquityPoint{Date: state.days[i], Value: value})
			}
		}
		if path.Error == "" && len(curve) > 1 {
			rebuildDrawdowns(curve)
			path.Curve = curve
			path.Start, path.End = curve[0].Date, curve[len(curve)-1].Date
			path.Metrics = ComputeMetrics(curve, state.sc)
		} else if path.Error == "" {
			path.Error = "no sessions in this path"
		}
		out = append(out, path)
	}
	return out
}

// summarisePaths reduces the paths to the distribution the command reports.
func summarisePaths(res *CPCVResult) {
	var rets, cagrs, sharpes, dds []float64
	worst, worstRet := -1, math.Inf(1)
	for i, p := range res.Paths {
		if p.Error != "" {
			continue
		}
		res.ValidPaths++
		rets = append(rets, p.Metrics.TotalReturn)
		cagrs = append(cagrs, p.Metrics.CAGR)
		dds = append(dds, p.Metrics.MaxDrawdown)
		if p.Metrics.Sharpe.Defined() {
			sharpes = append(sharpes, float64(p.Metrics.Sharpe))
		}
		if p.Metrics.TotalReturn > 0 {
			res.ProfitablePaths++
		}
		if p.Metrics.TotalReturn < worstRet {
			worstRet, worst = p.Metrics.TotalReturn, i
		}
	}
	res.WorstPath = worst
	res.Return = spreadOf(rets)
	res.CAGR = spreadOf(cagrs)
	res.Sharpe = spreadOf(sharpes)
	res.MaxDrawdown = spreadOf(dds)
}

// spreadOf summarises a sample. Undefined rather than zero when there is
// nothing to summarise.
func spreadOf(xs []float64) PathSpread {
	nan := Ratio(math.NaN())
	s := PathSpread{Mean: nan, Median: nan, Stdev: nan, P05: nan, P95: nan, Worst: nan, Best: nan}
	if len(xs) == 0 {
		return s
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	mean, sd := meanStdev(xs)
	s.Mean = Ratio(mean)
	s.Median = Ratio(percentileSorted(sorted, 0.5))
	s.P05 = Ratio(percentileSorted(sorted, 0.05))
	s.P95 = Ratio(percentileSorted(sorted, 0.95))
	s.Worst = Ratio(sorted[0])
	s.Best = Ratio(sorted[len(sorted)-1])
	if len(xs) > 1 {
		s.Stdev = Ratio(sd)
	}
	return s
}

// compareWalkForward runs the rolling scheme over the same data and places its
// single path inside the distribution.
//
// This is the sentence the command exists to produce. A walk-forward number
// read alone is one draw presented as the answer; the same number read against
// the spread of paths the same data supports says how much of it was the draw.
func compareWalkForward(ctx context.Context, cs CPCVSpec, store *market.Store, res *CPCVResult) *CPCVWalkForward {
	// The engine reads a zero embargo as "use the warm-up", so a run that
	// resolved to no embargo has to say so explicitly rather than have one
	// reinstated underneath it.
	embargo := cs.Embargo
	if embargo == 0 {
		embargo = -1
	}
	wf, err := RunWalkForward(ctx, WalkForwardSpec{
		Base: cs.Base, Grids: cs.Grids, TrainDays: cs.TrainDays, TestDays: cs.TestDays,
		Embargo: embargo, Objective: cs.Objective, Workers: cs.Workers, MaxCombos: cs.MaxCombos,
	}, store, nil)
	// A period too short for a rolling scheme is not a failure of the
	// cross-validation, so the comparison is dropped and the rest stands.
	if err != nil || len(wf.Stitched) == 0 {
		return nil
	}

	out := &CPCVWalkForward{
		Start: wf.Stitched[0].Date, End: wf.Stitched[len(wf.Stitched)-1].Date,
		TotalReturn: Ratio(wf.StitchedMetrics.TotalReturn),
		CAGR:        Ratio(wf.StitchedMetrics.CAGR),
		Sharpe:      wf.StitchedMetrics.Sharpe,
		Folds:       len(wf.Folds), PositiveFolds: wf.ConsistentFolds,
		CAGRPercentile: Ratio(math.NaN()), SharpePercentile: Ratio(math.NaN()),
	}
	var cagrs, sharpes []float64
	for _, p := range res.Paths {
		if p.Error != "" {
			continue
		}
		cagrs = append(cagrs, p.Metrics.CAGR)
		if p.Metrics.Sharpe.Defined() {
			sharpes = append(sharpes, float64(p.Metrics.Sharpe))
		}
	}
	out.CAGRPercentile = percentileOf(cagrs, wf.StitchedMetrics.CAGR)
	if wf.StitchedMetrics.Sharpe.Defined() {
		out.SharpePercentile = percentileOf(sharpes, float64(wf.StitchedMetrics.Sharpe))
	}
	return out
}

// percentileOf is the share of the sample that finished below v.
func percentileOf(xs []float64, v float64) Ratio {
	if len(xs) == 0 || math.IsNaN(v) {
		return Ratio(math.NaN())
	}
	below := 0
	for _, x := range xs {
		if x < v {
			below++
		}
	}
	return Ratio(float64(below) / float64(len(xs)))
}

// cpcvVerdict states the finding in the terms a reader needs.
func cpcvVerdict(r *CPCVResult) string {
	if r.ValidPaths == 0 {
		return "no out-of-sample path could be reconstructed, so there is no evidence here either way"
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("%d of %d out-of-sample paths finished profitable",
		r.ProfitablePaths, r.ValidPaths))

	if r.Combos < 2 {
		// Every split picks the same configuration because there is only one,
		// so the paths are identical and their spread is zero. Saying that is
		// better than printing a distribution with no width and letting a
		// reader take it for stability.
		parts = append(parts, "this strategy declares a single configuration, so every split chose it "+
			"and every path is the same path; the spread below is not a measurement of anything")
		return joinVerdict(parts)
	}

	if r.Return.Median.Defined() {
		parts = append(parts, fmt.Sprintf("the median path returned %s", pctText(float64(r.Return.Median))))
	}

	// This clause comes before the spread on purpose. A distribution of
	// profitable paths is the most misreadable thing this command produces —
	// it can be a working search or it can be a rising market — and the
	// control that separates the two belongs beside the claim, not four
	// clauses down where a reader has already stopped.
	if r.NoSelection.Median.Defined() && r.Return.Median.Defined() {
		fixed, chosen := float64(r.NoSelection.Median), float64(r.Return.Median)
		if chosen <= fixed {
			parts = append(parts, fmt.Sprintf("holding one configuration over the same groups and "+
				"choosing nothing returned %s, so the selection subtracted %s and none of the result "+
				"above is attributable to the search", pctText(fixed), pctText(fixed-chosen)))
		} else {
			parts = append(parts, fmt.Sprintf("holding one configuration over the same groups and "+
				"choosing nothing returned %s, so the selection is worth %s of the %s",
				pctText(fixed), pctText(chosen-fixed), pctText(chosen)))
		}
	}

	if r.Return.P05.Defined() && r.Return.P95.Defined() {
		lo, hi := float64(r.Return.P05), float64(r.Return.P95)
		if lo < 0 && hi > 0 {
			parts = append(parts, fmt.Sprintf("the 5th to 95th percentile runs from %s to %s, so this "+
				"data does not establish even the sign of the out-of-sample return",
				pctText(lo), pctText(hi)))
		} else {
			parts = append(parts, fmt.Sprintf("the 5th to 95th percentile runs from %s to %s",
				pctText(lo), pctText(hi)))
		}
	}
	if r.Return.Worst.Defined() && r.WorstPath >= 0 {
		parts = append(parts, fmt.Sprintf("the worst path finished %s", pctText(float64(r.Return.Worst))))
	}

	if r.PBO.Defined() {
		switch p := float64(r.PBO); {
		case p >= 0.5:
			parts = append(parts, fmt.Sprintf("probability of backtest overfitting is %.0f%% over these "+
				"%d purged splits — choosing on one part of this period says nothing about the rest",
				p*100, r.PBOSplits))
		case p <= 0.2:
			parts = append(parts, fmt.Sprintf("probability of backtest overfitting is %.0f%% over these "+
				"%d purged splits, low enough that the selection is doing something", p*100, r.PBOSplits))
		default:
			parts = append(parts, fmt.Sprintf("probability of backtest overfitting is %.0f%% over these "+
				"%d purged splits", p*100, r.PBOSplits))
		}
	}
	// Both figures are proportions, measured over different numbers of splits,
	// and the bar for calling them different has to come from that rather than
	// from a round number. Two standard errors of the less precise of the two
	// is 26 points over fifteen splits and 13 over seventy, so the same gap is
	// news in one setting and noise in the other.
	if r.PBO.Defined() && r.BlockPBO.Defined() {
		n := min(r.PBOSplits, r.BlockPBOSplits)
		tolerance := 1.0
		if n > 0 {
			tolerance = 2 * math.Sqrt(0.25/float64(n))
		}
		if math.Abs(float64(r.PBO)-float64(r.BlockPBO)) > tolerance {
			parts = append(parts, fmt.Sprintf("that disagrees with the %.0f%% the sweep's unpurged block "+
				"partition gives on the same runs, by more than either estimate's noise; the purged "+
				"figure is the one to believe, because the other lets a training block sit against a "+
				"test block with nothing between them", float64(r.BlockPBO)*100))
		} else {
			parts = append(parts, fmt.Sprintf("the sweep's unpurged block partition gives %.0f%% on the "+
				"same runs, the same answer within the noise of %d splits", float64(r.BlockPBO)*100, n))
		}
	}

	if wf := r.WalkForward; wf != nil && wf.CAGRPercentile.Defined() {
		q := float64(wf.CAGRPercentile)
		parts = append(parts, fmt.Sprintf("the single walk-forward path annualised %s, the %.0fth "+
			"percentile of these paths — the same strategy, the same data, and one draw from a "+
			"spread of %s to %s a year",
			pctText(float64(wf.CAGR)), q*100,
			pctText(float64(r.CAGR.Worst)), pctText(float64(r.CAGR.Best))))
		if c := windowMismatch(r); c != "" {
			parts = append(parts, c)
		}
	}
	return joinVerdict(parts)
}

// windowMismatch names the reason two out-of-sample schemes can disagree
// without either of them being wrong.
//
// Both schemes flatten the strategy whenever they hand it a new window, so
// both measure a strategy that is repeatedly forced into cash. A window
// shorter than the average round trip forces that far more often, and for a
// slow strategy that difference alone can be most of the gap between the two
// answers. It is only worth saying when the walk-forward path fell outside the
// whole spread — inside it, the two agree and the reader needs no excuse.
func windowMismatch(r *CPCVResult) string {
	wf := r.WalkForward
	if wf == nil || r.AvgBarsHeld <= 0 || r.GroupSessions <= 0 || r.WalkForwardTestDays <= 0 {
		return ""
	}
	q := float64(wf.CAGRPercentile)
	if q > 0 && q < 1 {
		return ""
	}
	if r.AvgBarsHeld <= float64(r.WalkForwardTestDays) {
		return ""
	}
	return fmt.Sprintf("before reading that as a verdict on either scheme, note that the average "+
		"round trip here lasts %.0f bars, against %d sessions in a held-out group and %d in a "+
		"walk-forward test window: both schemes start the strategy flat in every new window, and "+
		"one of them is shorter than the average trade, so a good part of the gap is the window "+
		"length rather than the strategy", r.AvgBarsHeld, r.GroupSessions, r.WalkForwardTestDays)
}

func joinVerdict(parts []string) string {
	if len(parts) == 0 {
		return "not enough completed splits to say anything useful"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "; " + p
	}
	return out
}
