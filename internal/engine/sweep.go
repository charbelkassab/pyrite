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

// SweepSpec describes a search over a strategy's declared parameters.
type SweepSpec struct {
	// Base is the run every combination is a variation of.
	Base Spec
	// Grids overrides the strategy's own declarations. A name absent here
	// keeps whatever the strategy declared.
	Grids map[string][]any
	// MaxCombos refuses a search larger than this. Default 5000.
	MaxCombos int
	// Workers defaults to the number of CPUs.
	Workers int
	// Objective names the metric rows are ranked by. Default "sharpe".
	Objective string
	// KeepBest retains the full Result for this many top rows, so the winner
	// can be charted without re-running it. Default 1.
	KeepBest int
	// PBOBlocks is the number of blocks combinatorially symmetric
	// cross-validation cuts the period into. Must be even. Default 8, which
	// gives 70 train/test partitions. Zero disables PBO.
	PBOBlocks int
	// MaxReturnCells caps the memory spent retaining per-trial return series
	// for PBO, in floats. Default 8 million, about 64MB — beyond that the
	// statistic is skipped rather than exhausting the machine to compute it.
	MaxReturnCells int
}

// SweepRow is one combination and what it scored.
//
// Rows are deliberately flat rather than nested: a sweep is a table, and
// keeping it one makes the CSV export, the heatmap and the robustness
// statistics all fall out of the same shape.
type SweepRow struct {
	Params map[string]any `json:"params"`
	Label  string         `json:"label"`
	// Score is a Ratio, not a float64, so that a failed or undefined
	// combination marshals to null. encoding/json refuses NaN outright, and
	// a single such cell would otherwise truncate the entire response — the
	// same trap Metrics already avoids for the same reason.
	Score Ratio `json:"score"`

	TotalReturn float64 `json:"total_return"`
	CAGR        float64 `json:"cagr"`
	Sharpe      Ratio   `json:"sharpe"`
	Sortino     Ratio   `json:"sortino"`
	Calmar      Ratio   `json:"calmar"`
	MaxDrawdown float64 `json:"max_drawdown"`
	Volatility  float64 `json:"volatility"`
	Trades      int     `json:"trades"`
	WinRate     float64 `json:"win_rate"`
	Turnover    float64 `json:"turnover"`
	UlcerIndex  float64 `json:"ulcer_index"`
	Expectancy  float64 `json:"expectancy"`

	// Error records a combination that failed rather than dropping it. A
	// hole in a heatmap is information; a silently missing cell is a lie.
	Error string `json:"error,omitempty"`
}

// SweepResult is the full search.
type SweepResult struct {
	// Axes are the parameter names that actually varied, sorted, which is
	// the order the heatmap uses.
	Axes  []string         `json:"axes"`
	Grids map[string][]any `json:"grids"`
	Rows  []SweepRow       `json:"rows"`

	Objective string `json:"objective"`
	// Best is the full result for the top-scoring combination, so the winner
	// can be charted and audited without running it again.
	Best []*Result `json:"best,omitempty"`

	Combos  int   `json:"combos"`
	Failed  int   `json:"failed"`
	Elapsed int64 `json:"elapsed_ms"`

	// Robustness is the overfitting assessment across the whole search. It
	// is the reason to run a sweep at all: the point of testing ten thousand
	// variants is not to find the best one, it is to find out whether the
	// best one means anything.
	Robustness Robustness `json:"robustness"`
}

// SweepProgress reports completed combinations.
type SweepProgress func(done, total int, row SweepRow)

// objectives maps a name to a scoring function. Higher is always better, so a
// drawdown objective scores its negation and callers never have to remember
// which way round a metric points.
var objectives = map[string]func(*Result) float64{
	"sharpe":        func(r *Result) float64 { return float64(r.Metrics.Sharpe) },
	"sortino":       func(r *Result) float64 { return float64(r.Metrics.Sortino) },
	"calmar":        func(r *Result) float64 { return float64(r.Metrics.Calmar) },
	"cagr":          func(r *Result) float64 { return r.Metrics.CAGR },
	"total_return":  func(r *Result) float64 { return r.Metrics.TotalReturn },
	"profit_factor": func(r *Result) float64 { return float64(r.Metrics.ProfitFactor) },
	"max_drawdown":  func(r *Result) float64 { return -math.Abs(r.Metrics.MaxDrawdown) },
	"ulcer":         func(r *Result) float64 { return -r.Risk.UlcerIndex },
	"expectancy":    func(r *Result) float64 { return r.TradeStats.Expectancy },
}

// ObjectiveNames lists the available ranking metrics.
func ObjectiveNames() []string {
	out := make([]string, 0, len(objectives))
	for k := range objectives {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DeclaredParams runs a strategy's setup() to discover what it exposes.
//
// This costs one data load, which every worker then shares through the store's
// in-memory cache, so the probe is close to free relative to the search it
// enables.
func DeclaredParams(ctx context.Context, spec Spec, store *market.Store) ([]ParamDecl, error) {
	spec.ApplyDefaults()
	spec.OmitDayRecords = true
	e := New(spec, store)
	if err := e.loadData(ctx); err != nil {
		return nil, err
	}
	e.portfolio = NewPortfolio(spec.InitialCash, spec.Costs)
	vm, err := newStrategyVM(e)
	if err != nil {
		return nil, err
	}
	defer vm.Close()
	if err := vm.callSetup(); err != nil {
		return nil, fmt.Errorf("strategy setup() failed: %w", err)
	}
	return e.paramDecls, nil
}

// Sweep runs every combination of a strategy's parameters.
//
// The parallelism here is the whole point, and it is close to free because of
// how the pieces already fit: market.Store is read-only once loaded and guards
// itself with a mutex, so N workers share one copy of the price data with no
// coordination and no duplication. There is no interpreter lock to work
// around and nothing to serialise between processes.
func RunSweep(ctx context.Context, ss SweepSpec, store *market.Store, progress SweepProgress) (*SweepResult, error) {
	started := time.Now()
	if ss.MaxCombos <= 0 {
		ss.MaxCombos = 5000
	}
	if ss.Workers <= 0 {
		ss.Workers = runtime.NumCPU()
	}
	if ss.Objective == "" {
		ss.Objective = "sharpe"
	}
	if ss.KeepBest <= 0 {
		ss.KeepBest = 1
	}
	if ss.PBOBlocks == 0 {
		ss.PBOBlocks = 8
	}
	if ss.MaxReturnCells <= 0 {
		ss.MaxReturnCells = 8 << 20
	}
	score, ok := objectives[ss.Objective]
	if !ok {
		return nil, fmt.Errorf("unknown objective %q (available: %v)", ss.Objective, ObjectiveNames())
	}

	decls, err := DeclaredParams(ctx, ss.Base, store)
	if err != nil {
		return nil, err
	}
	decls = mergeGrids(decls, ss.Grids)
	if len(decls) == 0 {
		return nil, fmt.Errorf("this strategy declares no parameters, so there is nothing to sweep.\n" +
			"  Declare one with ctx.param(\"name\", default, { grid: [...] }) in setup(),\n" +
			"  or pass a grid explicitly with --param name=1,2,3")
	}

	combos, err := Combos(decls, ss.MaxCombos)
	if err != nil {
		return nil, err
	}

	res := &SweepResult{
		Objective: ss.Objective,
		Combos:    len(combos),
		Grids:     map[string][]any{},
		Rows:      make([]SweepRow, len(combos)),
	}
	for _, d := range decls {
		if vals := d.Values(); len(vals) > 1 {
			res.Axes = append(res.Axes, d.Name)
			res.Grids[d.Name] = vals
		}
	}
	sort.Strings(res.Axes)

	// Keeping full results for the leaders means holding a few complete runs
	// in memory; every other combination discards its day records as it goes,
	// which is what keeps a ten-thousand-run search inside a laptop.
	type kept struct {
		score float64
		res   *Result
	}
	var (
		mu      sync.Mutex
		best    []kept
		done    int
		failed  int
		firstEr error
	)

	// Return series are retained for PBO only while they fit the budget. The
	// statistic is worth real memory, but not an out-of-memory kill on a
	// laptop, and a search big enough to blow the budget is one where the
	// plateau ratio already tells most of the story.
	keepReturns := ss.PBOBlocks >= 2 && ss.PBOBlocks%2 == 0
	var returns [][]float64
	if keepReturns {
		returns = make([][]float64, len(combos))
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < ss.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}

				spec := ss.Base
				spec.Params = combos[i]
				spec.OmitDayRecords = true
				// Trade records survive: they are small next to a day record
				// and the aggregate stats are worth having per combination.
				r, err := New(spec, store).Run(ctx)

				row := SweepRow{Params: combos[i], Label: FormatParams(combos[i])}
				if err != nil {
					row.Error = truncateErr(err.Error())
					row.Score = Ratio(math.NaN())
				} else {
					row.Score = Ratio(score(r))
					row.TotalReturn = r.Metrics.TotalReturn
					row.CAGR = r.Metrics.CAGR
					row.Sharpe = r.Metrics.Sharpe
					row.Sortino = r.Metrics.Sortino
					row.Calmar = r.Metrics.Calmar
					row.MaxDrawdown = r.Metrics.MaxDrawdown
					row.Volatility = r.Metrics.Volatility
					row.Trades = r.TradeStats.Closed
					row.WinRate = r.TradeStats.WinRate
					row.Turnover = r.Metrics.Turnover
					row.UlcerIndex = r.Risk.UlcerIndex
					row.Expectancy = r.TradeStats.Expectancy
				}

				mu.Lock()
				res.Rows[i] = row
				if keepReturns && err == nil {
					returns[i] = dailyReturns(r.Curve)
				}
				done++
				if err != nil {
					failed++
					if firstEr == nil {
						firstEr = err
					}
				} else if row.Score.Defined() {
					best = append(best, kept{float64(row.Score), r})
					sort.Slice(best, func(a, b int) bool { return best[a].score > best[b].score })
					if len(best) > ss.KeepBest {
						best = best[:ss.KeepBest]
					}
				}
				if progress != nil {
					progress(done, len(combos), row)
				}
				mu.Unlock()
			}
		}()
	}

	for i := range combos {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil, ctx.Err()
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()

	res.Failed = failed
	// Every single combination failing is a broken strategy, not a search
	// result, so surface the underlying error rather than an empty table.
	if failed == len(combos) && firstEr != nil {
		return nil, fmt.Errorf("every combination failed; first error: %w", firstEr)
	}
	for _, k := range best {
		res.Best = append(res.Best, k.res)
	}
	res.Robustness = AssessRobustness(res.Rows, ss.Objective)
	if keepReturns {
		if ok, matrix := alignedTrialReturns(returns, ss.MaxReturnCells); ok {
			res.Robustness.AddPBO(matrix, ss.PBOBlocks)
		}
	}
	if len(res.Best) > 0 {
		sharpes := make([]float64, 0, len(res.Rows))
		for _, row := range res.Rows {
			if row.Error == "" && row.Sharpe.Defined() {
				sharpes = append(sharpes, float64(row.Sharpe))
			}
		}
		res.Robustness.AddDeflatedSharpe(res.Best[0].Curve, sharpes, ScaleFor(ss.Base.Interval, ss.Base.RiskFreeRate))
	}
	// The verdict is written last, once every statistic that feeds it exists.
	res.Robustness.Finish(ss.Objective)
	res.Elapsed = time.Since(started).Milliseconds()
	return res, nil
}

// Sorted returns the rows ranked best first, failures last.
func (r *SweepResult) Sorted() []SweepRow {
	out := append([]SweepRow(nil), r.Rows...)
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := out[i].Score.Defined(), out[j].Score.Defined()
		if ai != aj {
			return ai
		}
		return out[i].Score > out[j].Score
	})
	return out
}

// Scores returns the finite scores in the search, for the statistics.
func (r *SweepResult) Scores() []float64 {
	out := make([]float64, 0, len(r.Rows))
	for _, row := range r.Rows {
		if row.Score.Defined() {
			out = append(out, float64(row.Score))
		}
	}
	return out
}

// Surface projects the sweep onto two axes for a heatmap.
//
// When more than two parameters varied, the others are held at the values that
// the best-scoring row used — which is the honest projection, because it is
// the slice through the space that the reported winner actually lives on.
func (r *SweepResult) Surface(xAxis, yAxis string) (xs, ys []any, z [][]float64) {
	xs = r.Grids[xAxis]
	ys = r.Grids[yAxis]
	if len(xs) == 0 || len(ys) == 0 {
		return nil, nil, nil
	}

	var pin map[string]any
	if sorted := r.Sorted(); len(sorted) > 0 {
		pin = sorted[0].Params
	}

	z = make([][]float64, len(ys))
	for i := range z {
		z[i] = make([]float64, len(xs))
		for j := range z[i] {
			z[i][j] = math.NaN()
		}
	}
	for _, row := range r.Rows {
		if row.Error != "" {
			continue
		}
		matches := true
		for _, ax := range r.Axes {
			if ax == xAxis || ax == yAxis {
				continue
			}
			if pin != nil && !sameParam(row.Params[ax], pin[ax]) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		xi := indexOfParam(xs, row.Params[xAxis])
		yi := indexOfParam(ys, row.Params[yAxis])
		if xi < 0 || yi < 0 {
			continue
		}
		z[yi][xi] = float64(row.Score)
	}
	return xs, ys, z
}

func indexOfParam(vals []any, v any) int {
	for i, x := range vals {
		if sameParam(x, v) {
			return i
		}
	}
	return -1
}

// sameParam compares parameter values across the numeric types that JavaScript
// and Go disagree about, so a grid of 50 matches a run that stored 50.0.
func sameParam(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	af, aok := toFloatOK(a)
	bf, bok := toFloatOK(b)
	if aok && bok {
		return math.Abs(af-bf) < 1e-9
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// alignedTrialReturns keeps only the trials that produced a full-length return
// series, which is what CSCV needs to compare like with like.
//
// A combination that traded a shorter window — a warm-up that consumed more
// history, say — is not comparable to one that did not, and silently padding
// it would corrupt every split it appeared in.
// mergeGrids applies caller-supplied grids to the strategy's declarations.
//
// A caller grid wins over a declared one and may introduce a parameter the
// strategy never declared — useful for sweeping something like a stop that was
// written inline.
//
// New names are appended in sorted order rather than map order. The set of
// combinations is the same either way, but their order decides how ties break
// when the results are ranked, so appending in map order let the same search
// name a different winner on a second run over identical data.
func mergeGrids(decls []ParamDecl, grids map[string][]any) []ParamDecl {
	byName := make(map[string]int, len(decls))
	for i, d := range decls {
		byName[d.Name] = i
	}
	names := make([]string, 0, len(grids))
	for name := range grids {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		grid := grids[name]
		if i, ok := byName[name]; ok {
			decls[i].Grid = grid
			continue
		}
		var def any
		if len(grid) > 0 {
			def = grid[0]
		}
		decls = append(decls, ParamDecl{Name: name, Default: def, Grid: grid})
	}
	return decls
}

func alignedTrialReturns(returns [][]float64, maxCells int) (bool, [][]float64) {
	lengths := map[int]int{}
	for _, r := range returns {
		if len(r) > 0 {
			lengths[len(r)]++
		}
	}
	if len(lengths) == 0 {
		return false, nil
	}
	// The modal length is the one worth keeping.
	bestLen, bestCount := 0, 0
	for l, c := range lengths {
		if c > bestCount || (c == bestCount && l > bestLen) {
			bestLen, bestCount = l, c
		}
	}
	if bestCount < 2 || bestLen < 40 {
		return false, nil
	}
	if bestLen*bestCount > maxCells {
		return false, nil
	}
	out := make([][]float64, 0, bestCount)
	for _, r := range returns {
		if len(r) == bestLen {
			out = append(out, r)
		}
	}
	return len(out) >= 2, out
}
