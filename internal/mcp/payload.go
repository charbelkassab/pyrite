package mcp

import (
	"math"

	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
	"github.com/charbelkassab/pyrite/internal/strategy"
)

// num wraps every float that reaches the wire.
//
// encoding/json refuses NaN and ±Inf outright, and one undefined cell would
// take the whole frame with it. Over a stdio transport that failure is worse
// than it is over HTTP: there is no status line and no server log, only a
// client reporting that a response never arrived.
func num(v float64) engine.Ratio { return engine.Ratio(v) }

// dataNote says what the numbers were computed from. Synthetic prices produce
// a real-looking Sharpe from data that is not the market, and an agent reading
// only the metrics has no other way to know.
type dataNote struct {
	Provider string `json:"provider"`
	Offline  bool   `json:"offline,omitempty"`
	Note     string `json:"note,omitempty"`
}

func (s *Server) dataNote() dataNote {
	d := dataNote{Provider: s.app.Store.ProviderName(), Offline: s.app.Cfg.OfflineMode}
	if d.Offline {
		d.Note = "Offline mode: prices are deterministic synthetic data, not the market. " +
			"Nothing here is evidence about a real strategy."
	}
	return d
}

type metricsPayload struct {
	StartValue  engine.Ratio `json:"start_value"`
	EndValue    engine.Ratio `json:"end_value"`
	TotalReturn engine.Ratio `json:"total_return"`
	CAGR        engine.Ratio `json:"cagr"`
	Volatility  engine.Ratio `json:"volatility"`
	Sharpe      engine.Ratio `json:"sharpe"`
	Sortino     engine.Ratio `json:"sortino"`
	Calmar      engine.Ratio `json:"calmar"`
	MaxDrawdown engine.Ratio `json:"max_drawdown"`

	MaxDrawdownStart market.Day `json:"max_drawdown_start,omitempty"`
	MaxDrawdownEnd   market.Day `json:"max_drawdown_end,omitempty"`

	Trades       int          `json:"trades"`
	TradeWinRate engine.Ratio `json:"trade_win_rate"`
	ProfitFactor engine.Ratio `json:"profit_factor"`
	Turnover     engine.Ratio `json:"turnover"`
	TotalCosts   engine.Ratio `json:"total_costs"`

	TradingDays int          `json:"trading_days"`
	Years       engine.Ratio `json:"years"`
}

func metricsOf(m engine.Metrics) metricsPayload {
	return metricsPayload{
		StartValue: num(m.StartValue), EndValue: num(m.EndValue),
		TotalReturn: num(m.TotalReturn), CAGR: num(m.CAGR), Volatility: num(m.Volatility),
		Sharpe: m.Sharpe, Sortino: m.Sortino, Calmar: m.Calmar,
		MaxDrawdown:      num(m.MaxDrawdown),
		MaxDrawdownStart: m.MaxDrawdownStart, MaxDrawdownEnd: m.MaxDrawdownEnd,
		Trades: m.TotalTrades, TradeWinRate: num(m.TradeWinRate), ProfitFactor: m.ProfitFactor,
		Turnover: num(m.Turnover), TotalCosts: num(m.TotalCosts),
		TradingDays: m.TradingDays, Years: num(m.Years),
	}
}

type benchmarkPayload struct {
	Label       string       `json:"label"`
	TotalReturn engine.Ratio `json:"total_return"`
	CAGR        engine.Ratio `json:"cagr"`
	Sharpe      engine.Ratio `json:"sharpe"`
	MaxDrawdown engine.Ratio `json:"max_drawdown"`
}

// runPayload is one backtest as an agent reads it.
//
// The critique is not optional and not a separate call. The headline metrics
// and the objections to them travel together because separating them is
// exactly how a search for a good-looking number succeeds.
type runPayload struct {
	Name     string           `json:"name"`
	Universe []string         `json:"universe"`
	Index    string           `json:"index,omitempty"`
	Start    market.Day       `json:"start"`
	End      market.Day       `json:"end"`
	Interval market.Interval  `json:"interval"`
	Fill     engine.FillModel `json:"fill"`
	Data     dataNote         `json:"data"`

	TrustScore int                `json:"trust_score"`
	Headline   string             `json:"headline"`
	Critique   []engine.Finding   `json:"critique"`
	Metrics    metricsPayload     `json:"metrics"`
	Benchmarks []benchmarkPayload `json:"benchmarks,omitempty"`

	ParamValues    map[string]any    `json:"param_values,omitempty"`
	Warnings       []string          `json:"warnings,omitempty"`
	SkippedSymbols map[string]string `json:"skipped_symbols,omitempty"`
	StrategyErrors int               `json:"strategy_errors,omitempty"`
	AICalls        int               `json:"ai_calls,omitempty"`
	Notes          []string          `json:"notes,omitempty"`
	ElapsedMS      int64             `json:"elapsed_ms"`
}

func (s *Server) runPayload(plan *strategy.Plan, res *engine.Result) *runPayload {
	out := &runPayload{
		Name:     plan.Name,
		Universe: res.Spec.Universe,
		Index:    res.Spec.Index,
		Start:    res.Spec.Start,
		End:      res.Spec.End,
		Interval: res.Spec.Interval,
		Fill:     res.Spec.Fill,
		Data:     s.dataNote(),

		TrustScore: res.Critique.TrustScore,
		Headline:   res.Critique.Headline,
		Critique:   res.Critique.Findings,
		Metrics:    metricsOf(res.Metrics),

		ParamValues:    safeParams(res.ParamValues),
		Warnings:       res.Warnings,
		SkippedSymbols: res.SkippedSymbols,
		StrategyErrors: res.StrategyErrors,
		AICalls:        res.AICallCount,
		ElapsedMS:      res.Elapsed,
	}
	if out.Critique == nil {
		out.Critique = []engine.Finding{}
	}
	for _, b := range res.Benchmarks {
		out.Benchmarks = append(out.Benchmarks, benchmarkPayload{
			Label: b.Label, TotalReturn: num(b.Metric.TotalReturn), CAGR: num(b.Metric.CAGR),
			Sharpe: b.Metric.Sharpe, MaxDrawdown: num(b.Metric.MaxDrawdown),
		})
	}
	return out
}

type sweepRowPayload struct {
	Params      map[string]any `json:"params"`
	Label       string         `json:"label"`
	Score       engine.Ratio   `json:"score"`
	TotalReturn engine.Ratio   `json:"total_return"`
	CAGR        engine.Ratio   `json:"cagr"`
	Sharpe      engine.Ratio   `json:"sharpe"`
	MaxDrawdown engine.Ratio   `json:"max_drawdown"`
	Trades      int            `json:"trades"`
	Turnover    engine.Ratio   `json:"turnover"`
	Error       string         `json:"error,omitempty"`
}

type robustnessPayload struct {
	Trials           int          `json:"trials"`
	BestScore        engine.Ratio `json:"best_score"`
	MedianScore      engine.Ratio `json:"median_score"`
	WorstScore       engine.Ratio `json:"worst_score"`
	ScoreStdev       engine.Ratio `json:"score_stdev"`
	PositiveShare    engine.Ratio `json:"positive_share"`
	ExpectedMaxScore engine.Ratio `json:"expected_max_score"`
	DeflatedSharpe   engine.Ratio `json:"deflated_sharpe"`
	PBO              engine.Ratio `json:"pbo"`
	PBOSplits        int          `json:"pbo_splits"`
	PlateauRatio     engine.Ratio `json:"plateau_ratio"`
	Neighbours       int          `json:"neighbours"`
	Verdict          string       `json:"verdict"`
}

type sweepPayload struct {
	Objective  string            `json:"objective"`
	Combos     int               `json:"combos"`
	Failed     int               `json:"failed"`
	Axes       []string          `json:"axes"`
	Grids      map[string][]any  `json:"grids,omitempty"`
	TopRows    []sweepRowPayload `json:"top_rows"`
	Robustness robustnessPayload `json:"robustness"`
	// Best is the winning combination run in full, critique included. The
	// table says which cell won; this says whether winning meant anything.
	Best      *runPayload `json:"best,omitempty"`
	Data      dataNote    `json:"data"`
	Notes     []string    `json:"notes,omitempty"`
	ElapsedMS int64       `json:"elapsed_ms"`
}

// defaultTopRows is how many ranked combinations come back when the caller did
// not say. A whole grid is thousands of rows and an agent reading all of them
// learns nothing the robustness block does not already say.
const defaultTopRows = 10

func (s *Server) sweepPayload(plan *strategy.Plan, res *engine.SweepResult, top int) *sweepPayload {
	if top <= 0 {
		top = defaultTopRows
	}
	r := res.Robustness
	out := &sweepPayload{
		Objective: res.Objective,
		Combos:    res.Combos,
		Failed:    res.Failed,
		Axes:      res.Axes,
		Grids:     safeGrids(res.Grids),
		Robustness: robustnessPayload{
			Trials: r.Trials, BestScore: num(r.BestScore), MedianScore: num(r.MedianScore),
			WorstScore: num(r.WorstScore), ScoreStdev: num(r.ScoreStdev),
			PositiveShare: num(r.PositiveShare), ExpectedMaxScore: num(r.ExpectedMaxScore),
			DeflatedSharpe: r.DeflatedSharpe, PBO: r.PBO, PBOSplits: r.PBOSplits,
			PlateauRatio: r.PlateauRatio, Neighbours: r.Neighbours, Verdict: r.Verdict,
		},
		Data:      s.dataNote(),
		ElapsedMS: res.Elapsed,
	}

	rows := res.Sorted()
	if len(rows) > top {
		rows = rows[:top]
	}
	out.TopRows = make([]sweepRowPayload, 0, len(rows))
	for _, row := range rows {
		out.TopRows = append(out.TopRows, sweepRowPayload{
			Params: safeParams(row.Params), Label: row.Label, Score: row.Score,
			TotalReturn: num(row.TotalReturn), CAGR: num(row.CAGR), Sharpe: row.Sharpe,
			MaxDrawdown: num(row.MaxDrawdown), Trades: row.Trades, Turnover: num(row.Turnover),
			Error: row.Error,
		})
	}
	if len(res.Best) > 0 && res.Best[0] != nil {
		out.Best = s.runPayload(plan, res.Best[0])
	}
	return out
}

type foldPayload struct {
	Index      int            `json:"index"`
	TrainStart market.Day     `json:"train_start"`
	TrainEnd   market.Day     `json:"train_end"`
	TestStart  market.Day     `json:"test_start"`
	TestEnd    market.Day     `json:"test_end"`
	BestParams map[string]any `json:"best_params,omitempty"`
	TrainScore engine.Ratio   `json:"train_score"`
	TestScore  engine.Ratio   `json:"test_score"`
	TestReturn engine.Ratio   `json:"test_return"`
	Combos     int            `json:"combos"`
	Error      string         `json:"error,omitempty"`
}

type walkForwardPayload struct {
	Name      string   `json:"name"`
	Objective string   `json:"objective"`
	Data      dataNote `json:"data"`

	// Verdict is the point of the whole evaluation, so it leads.
	Verdict           string       `json:"verdict"`
	Efficiency        engine.Ratio `json:"efficiency"`
	InSampleReturn    engine.Ratio `json:"in_sample_return"`
	OutOfSampleReturn engine.Ratio `json:"out_of_sample_return"`
	ConsistentFolds   int          `json:"consistent_folds"`
	TotalFolds        int          `json:"total_folds"`
	ParamStability    engine.Ratio `json:"param_stability"`

	// OutOfSampleMetrics measures the stitched test windows, which is the one
	// equity curve here that was never fitted to.
	OutOfSampleMetrics metricsPayload `json:"out_of_sample_metrics"`
	Folds              []foldPayload  `json:"folds"`
	Notes              []string       `json:"notes,omitempty"`
	ElapsedMS          int64          `json:"elapsed_ms"`
}

func (s *Server) walkForwardPayload(plan *strategy.Plan, res *engine.WalkForwardResult, elapsedMS int64) *walkForwardPayload {
	out := &walkForwardPayload{
		Name:               plan.Name,
		Objective:          res.Objective,
		Data:               s.dataNote(),
		Verdict:            res.Verdict,
		Efficiency:         res.Efficiency,
		InSampleReturn:     num(res.InSampleReturn),
		OutOfSampleReturn:  num(res.OutOfSampleMean),
		ConsistentFolds:    res.ConsistentFolds,
		TotalFolds:         len(res.Folds),
		ParamStability:     num(res.ParamStability),
		OutOfSampleMetrics: metricsOf(res.StitchedMetrics),
		ElapsedMS:          elapsedMS,
	}
	out.Folds = make([]foldPayload, 0, len(res.Folds))
	for _, f := range res.Folds {
		out.Folds = append(out.Folds, foldPayload{
			Index: f.Index, TrainStart: f.TrainStart, TrainEnd: f.TrainEnd,
			TestStart: f.TestStart, TestEnd: f.TestEnd,
			BestParams: safeParams(f.BestParams),
			TrainScore: f.TrainScore, TestScore: f.TestScore,
			TestReturn: num(f.TestMetrics.TotalReturn),
			Combos:     f.Combos, Error: f.Error,
		})
	}
	return out
}

// safeParams copies a parameter map, replacing any non-finite number with
// null. Parameter values arrive from JavaScript, where NaN is an ordinary
// number, and one of them would refuse to encode and take the frame with it.
func safeParams(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = safeValue(v)
	}
	return out
}

func safeGrids(in map[string][]any) map[string][]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]any, len(in))
	for k, vals := range in {
		copied := make([]any, len(vals))
		for i, v := range vals {
			copied[i] = safeValue(v)
		}
		out[k] = copied
	}
	return out
}

func safeValue(v any) any {
	f, ok := v.(float64)
	if !ok {
		return v
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return f
}
