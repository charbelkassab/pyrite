package engine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/charbelkassab/natural-quant/internal/market"
)

// FillModel decides at what price an order submitted on day D executes.
type FillModel string

const (
	// FillNextOpen executes at the next session's open. This is the default
	// and the only model that is free of lookahead bias: the strategy sees
	// day D's close, and trades at a price that had not yet printed when the
	// decision was made.
	FillNextOpen FillModel = "next_open"
	// FillClose executes at the same day's close. Convenient, and common in
	// published backtests, but it lets a strategy trade at a price it has
	// already observed. Offered for comparison, flagged in the UI.
	FillClose FillModel = "close"
)

// Spec fully describes a backtest.
type Spec struct {
	Name string `json:"name"`
	// Prompt is the original natural language description, kept for display.
	Prompt string `json:"prompt,omitempty"`
	// Code is the JavaScript strategy body.
	Code string `json:"code"`

	Universe   []string `json:"universe"`
	Benchmarks []string `json:"benchmarks,omitempty"`

	Start market.Day `json:"start"`
	End   market.Day `json:"end"`

	InitialCash float64   `json:"initial_cash"`
	Costs       Costs     `json:"costs"`
	Fill        FillModel `json:"fill"`

	AllowShort      bool    `json:"allow_short"`
	AllowFractional bool    `json:"allow_fractional"`
	MaxLeverage     float64 `json:"max_leverage"`
	RiskFreeRate    float64 `json:"risk_free_rate"`

	// Warmup is how many bars of history to load before Start so that
	// indicators are valid on the first trading day.
	Warmup int `json:"warmup"`

	// Params are user-tunable values exposed to the strategy as ctx.params.
	Params map[string]any `json:"params,omitempty"`

	// Seed makes any stochastic strategy behaviour reproducible.
	Seed int64 `json:"seed,omitempty"`

	// Index, when set, resolves the universe from point-in-time index
	// membership instead of a fixed list. The engine loads every symbol that
	// held membership at any point in the window, and lets each session trade
	// only the ones that were actually members that day.
	//
	// This is the fix for the survivorship bias the docs call the single
	// largest distortion in the tool: a universe of "companies in the index"
	// that means today's list already knows which companies survived.
	Index string `json:"index,omitempty"`

	// OmitDayRecords drops the per-session audit trail from the result.
	//
	// A DayRecord holds every position, order, log line and model exchange
	// for one session. That is the most valuable thing the tool produces for
	// a single run, and the fastest way to exhaust memory across ten thousand
	// of them. Sweeps set this; interactive runs do not.
	OmitDayRecords bool `json:"omit_day_records,omitempty"`
}

// ApplyDefaults fills unset fields with sensible values.
func (s *Spec) ApplyDefaults() {
	if s.InitialCash <= 0 {
		s.InitialCash = 100000
	}
	if s.Fill == "" {
		s.Fill = FillNextOpen
	}
	if s.MaxLeverage <= 0 {
		s.MaxLeverage = 1.0
	}
	if s.Warmup < 0 {
		s.Warmup = 0
	}
	if s.Costs == (Costs{}) {
		s.Costs = DefaultCosts()
	}
}

// PositionSnapshot is a position as it stood at a day's close.
type PositionSnapshot struct {
	Symbol        string  `json:"symbol"`
	Shares        float64 `json:"shares"`
	AvgPrice      float64 `json:"avg_price"`
	Price         float64 `json:"price"`
	Value         float64 `json:"value"`
	Weight        float64 `json:"weight"`
	UnrealizedPnL float64 `json:"unrealized_pnl"`
	ReturnPct     float64 `json:"return_pct"`
	DaysHeld      int     `json:"days_held"`
}

// AICall records one model or web call made by the strategy on a given day,
// so the day-detail view can show exactly what the strategy asked and what it
// was told.
type AICall struct {
	Kind     string `json:"kind"` // "ai" | "web" | "news"
	Prompt   string `json:"prompt"`
	Response string `json:"response"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Cached   bool   `json:"cached"`
	Millis   int64  `json:"millis"`
	Error    string `json:"error,omitempty"`
}

// DayRecord is the full audit trail for one simulated session.
type DayRecord struct {
	Date      market.Day         `json:"date"`
	Equity    float64            `json:"equity"`
	Cash      float64            `json:"cash"`
	Exposure  float64            `json:"exposure"`
	Return    float64            `json:"return"`
	Drawdown  float64            `json:"drawdown"`
	Positions []PositionSnapshot `json:"positions,omitempty"`
	Fills     []Fill             `json:"fills,omitempty"`
	Orders    []Order            `json:"orders,omitempty"`
	Logs      []string           `json:"logs,omitempty"`
	AICalls   []AICall           `json:"ai_calls,omitempty"`
	// Error records a strategy exception on this day; the run continues.
	Error string `json:"error,omitempty"`
}

// BenchmarkResult is a comparison series computed alongside the strategy.
type BenchmarkResult struct {
	Symbol string        `json:"symbol"`
	Label  string        `json:"label"`
	Curve  []EquityPoint `json:"curve"`
	Metric Metrics       `json:"metrics"`
}

// Result is everything a completed run produces.
type Result struct {
	Spec       Spec              `json:"spec"`
	Curve      []EquityPoint     `json:"curve"`
	Days       []DayRecord       `json:"days"`
	Fills      []Fill            `json:"fills"`
	Metrics    Metrics           `json:"metrics"`
	Benchmarks []BenchmarkResult `json:"benchmarks,omitempty"`

	// Trades are fills paired into round trips; TradeStats aggregates them.
	// This is the level at which "did the idea work" is answerable, which
	// the fill list alone is not.
	Trades     []Trade    `json:"trades,omitempty"`
	TradeStats TradeStats `json:"trade_stats"`
	// Risk holds the distribution and drawdown statistics behind the
	// headline numbers.
	Risk RiskMetrics `json:"risk"`
	// Attribution decomposes the result by period, regime and holding.
	Attribution Attribution `json:"attribution"`
	// Rolling is a trailing-window view of Sharpe, volatility and beta.
	Rolling []RollingPoint `json:"rolling,omitempty"`
	// Manifest records everything needed to judge whether a re-run is
	// comparable to this one.
	Manifest Manifest `json:"manifest"`
	// Params are the tunables the strategy declared, and ParamValues the
	// combination this run actually used. Together they are what a sweep
	// needs in order to know what there is to search.
	Params      []ParamDecl    `json:"params,omitempty"`
	ParamValues map[string]any `json:"param_values,omitempty"`
	// Critique is the deterministic assessment of this result: what is wrong
	// with it, with the numbers that say so.
	Critique Critique `json:"critique"`

	Warnings []string `json:"warnings,omitempty"`
	// StrategyErrors counts days where onDay threw.
	StrategyErrors int   `json:"strategy_errors"`
	AICallCount    int   `json:"ai_call_count"`
	Elapsed        int64 `json:"elapsed_ms"`
	// SkippedSymbols lists universe members that had no data.
	SkippedSymbols map[string]string `json:"skipped_symbols,omitempty"`
}

// SearchResult is one web or news hit handed to a strategy.
type SearchResult struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Snippet   string `json:"snippet"`
	Published string `json:"published,omitempty"`
	Source    string `json:"source,omitempty"`
}

// AIOptions are the knobs a strategy may pass to ctx.ai().
type AIOptions struct {
	JSON      bool
	Tier      string
	MaxTokens int
	System    string
}

// AIFunc is the host callback backing ctx.ai().
type AIFunc func(ctx context.Context, day market.Day, prompt string, opts AIOptions) (text, provider, model string, cached bool, err error)

// SearchFunc is the host callback backing ctx.web() and ctx.news().
type SearchFunc func(ctx context.Context, day market.Day, query string, limit int, news bool) ([]SearchResult, error)

// ProgressFunc reports incremental progress during a run.
type ProgressFunc func(done, total int, day market.Day)

// Engine executes one backtest.
type Engine struct {
	spec  Spec
	store *market.Store

	AI       AIFunc
	Search   SearchFunc
	Progress ProgressFunc
	// MaxAICalls caps ai()/web() calls for the whole run.
	MaxAICalls int

	// runtime state, visible to the JS bindings
	ctx         context.Context
	series      map[string]*market.Series
	benchSer    map[string]*market.Series
	days        []market.Day
	dayIdx      int
	today       market.Day
	portfolio   *Portfolio
	adjPrices   map[string]float64
	rawPrices   map[string]float64
	equityNow   float64
	pending     []Order
	stops       map[string]*stopOrder
	state       map[string]any
	dayLogs     []string
	dayAI       []AICall
	dayOrders   []Order
	paramDecls  []ParamDecl
	paramIdx    map[string]int
	aiCalls     int
	aiCacheHits int
	aiModels    map[string]bool
	warnings    []string
	warnSeen    map[string]bool
	monthSeen   map[string]bool
	weekSeen    map[string]bool
	yearSeen    map[string]bool
	lastOfMonth map[market.Day]bool
	lastOfWeek  map[market.Day]bool
	// members is the point-in-time constituent table when spec.Index is set.
	members *market.Membership
}

// stopOrder is a standing exit registered by the strategy.
type stopOrder struct {
	StopLossPct     float64
	TakeProfitPct   float64
	TrailingStopPct float64
}

// New builds an engine for a spec.
func New(spec Spec, store *market.Store) *Engine {
	spec.ApplyDefaults()
	return &Engine{
		spec:       spec,
		store:      store,
		MaxAICalls: 2000,
		state:      map[string]any{},
		stops:      map[string]*stopOrder{},
		warnSeen:   map[string]bool{},
	}
}

// Run executes the backtest.
func (e *Engine) Run(ctx context.Context) (*Result, error) {
	started := time.Now()
	e.ctx = ctx

	// An empty universe here is not yet an error: setup() has not run, and it
	// may be about to choose one. The check is not skipped, only deferred to
	// the pass below, which always runs.
	loaded := false
	if len(e.spec.Universe) > 0 || e.spec.Index != "" {
		if err := e.loadData(ctx); err != nil {
			return nil, err
		}
		if len(e.days) == 0 {
			return nil, fmt.Errorf("no trading days between %s and %s for the requested symbols", e.spec.Start, e.spec.End)
		}
		loaded = true
	}

	e.portfolio = NewPortfolio(e.spec.InitialCash, e.spec.Costs)
	e.portfolio.AllowShort = e.spec.AllowShort
	e.portfolio.AllowFractional = e.spec.AllowFractional
	e.portfolio.MaxLeverage = e.spec.MaxLeverage

	vm, err := newStrategyVM(e)
	if err != nil {
		return nil, err
	}
	defer vm.Close()

	// setup() may set the universe and the warm-up, which are exactly the two
	// things loadData needed in order to run. So it runs once with whatever
	// the spec carried, and again if setup() changed either.
	//
	// Without this second pass, ctx.universe([...]) in setup() is documented
	// but inert: the data was already chosen, so a strategy that sets its own
	// symbol list fails with "empty universe", and one that raises its own
	// warm-up silently trades on too little history. The store caches, so a
	// reload only fetches symbols that are genuinely new.
	beforeUniverse := strings.Join(e.spec.Universe, ",")
	beforeWarmup := e.spec.Warmup
	beforeIndex := e.spec.Index
	if err := vm.callSetup(); err != nil {
		return nil, fmt.Errorf("strategy setup() failed: %w", err)
	}
	if !loaded || strings.Join(e.spec.Universe, ",") != beforeUniverse ||
		e.spec.Warmup != beforeWarmup || e.spec.Index != beforeIndex {
		// loadData reports an empty universe itself, which is the case a
		// strategy that never names a symbol falls into.
		if err := e.loadData(ctx); err != nil {
			return nil, err
		}
		if len(e.days) == 0 {
			return nil, fmt.Errorf("no trading days between %s and %s for the symbols setup() selected",
				e.spec.Start, e.spec.End)
		}
	}

	res := &Result{Spec: e.spec, SkippedSymbols: map[string]string{}}
	peak := e.spec.InitialCash

	total := len(e.days)
	tradedDays := 0
	for i, day := range e.days {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// dayIndex counts traded sessions from the backtest start, not from
		// the warm-up load date. A strategy asking "is this day 0?" means the
		// first day it was actually run, and warm-up days are invisible to it.
		if day >= e.spec.Start {
			e.dayIdx = tradedDays
			tradedDays++
		}
		e.today = day
		e.snapshotPrices(day)

		// 1. Execute orders submitted yesterday, and any triggered stops.
		var fills []Fill
		if e.spec.Fill == FillNextOpen {
			fills = append(fills, e.runStops(day)...)
			fills = append(fills, e.executePending(day)...)
		}

		// 2. Financing and trailing marks.
		e.portfolio.AccrueFinancing(e.adjPrices, TradingDaysPerYear)
		e.portfolio.UpdateTrailing(e.adjPrices)

		// 3. Ask the strategy what to do, unless we are still warming up.
		e.dayLogs = nil
		e.dayAI = nil
		e.dayOrders = nil
		e.pending = nil

		var dayErr string
		if day >= e.spec.Start {
			e.equityNow = e.portfolio.Equity(e.adjPrices)
			if err := vm.callOnDay(); err != nil {
				dayErr = err.Error()
				res.StrategyErrors++
				e.warn(fmt.Sprintf("strategy error on %s: %s", day, truncateErr(err.Error())))
			}
		}

		// 4. Under the close-fill model, orders execute immediately.
		if e.spec.Fill == FillClose {
			fills = append(fills, e.executeAt(day, e.pending, e.adjPrices)...)
			e.pending = nil
			fills = append(fills, e.runStops(day)...)
		}

		// 5. Mark to market and record.
		equity := e.portfolio.Equity(e.adjPrices)
		if equity > peak {
			peak = equity
		}
		dd := 0.0
		if peak > 0 {
			dd = equity/peak - 1
		}
		ret := 0.0
		if n := len(res.Curve); n > 0 && res.Curve[n-1].Value > 0 {
			ret = equity/res.Curve[n-1].Value - 1
		}
		exposure := 0.0
		if equity > 0 {
			exposure = e.portfolio.GrossExposure(e.adjPrices) / equity
		}

		if day >= e.spec.Start {
			res.Curve = append(res.Curve, EquityPoint{
				Date: day, Value: equity, Cash: e.portfolio.Cash,
				Return: ret, Drawdown: dd, Exposure: exposure,
			})
			if !e.spec.OmitDayRecords {
				res.Days = append(res.Days, DayRecord{
					Date: day, Equity: equity, Cash: e.portfolio.Cash,
					Exposure: exposure, Return: ret, Drawdown: dd,
					Positions: e.snapshotPositions(day, equity),
					Fills:     fills,
					Orders:    e.dayOrders,
					Logs:      e.dayLogs,
					AICalls:   e.dayAI,
					Error:     dayErr,
				})
			}
			res.Fills = append(res.Fills, fills...)
		}

		if e.Progress != nil && (i%20 == 0 || i == total-1) {
			e.Progress(i+1, total, day)
		}
	}

	res.Metrics = ComputeMetrics(res.Curve, e.spec.RiskFreeRate)
	res.Metrics.AddTradeStats(res.Fills, avgEquity(res.Curve))
	res.AICallCount = e.aiCalls
	res.Warnings = e.warnings

	// Benchmarks share the strategy's calendar so the curves line up exactly.
	// They are built before the risk and attribution sections because both
	// want a reference curve to measure against.
	curveDays := make([]market.Day, 0, len(res.Curve))
	for _, p := range res.Curve {
		curveDays = append(curveDays, p.Date)
	}
	for _, sym := range e.spec.Benchmarks {
		ser, ok := e.benchSer[sym]
		if !ok || ser == nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("benchmark %s unavailable", sym))
			continue
		}
		curve := BuyAndHoldCurve(ser, curveDays, e.spec.InitialCash)
		if len(curve) == 0 {
			continue
		}
		bm := ComputeMetrics(curve, e.spec.RiskFreeRate)
		label := ser.Name
		if label == "" {
			label = sym
		}
		res.Benchmarks = append(res.Benchmarks, BenchmarkResult{
			Symbol: sym, Label: label, Curve: curve, Metric: bm,
		})
	}
	var benchCurve []EquityPoint
	if len(res.Benchmarks) > 0 {
		benchCurve = res.Benchmarks[0].Curve
		res.Metrics.AddBenchmarkStats(res.Curve, benchCurve, e.spec.RiskFreeRate)
	}

	res.Trades = BuildTrades(res.Fills, e.series)
	res.TradeStats = ComputeTradeStats(res.Trades)
	res.Risk = ComputeRiskMetrics(res.Curve, res.Metrics.CAGR, e.spec.RiskFreeRate)
	if benchCurve != nil {
		res.Risk.AddCapture(res.Curve, benchCurve)
	}
	res.Attribution = ComputeAttribution(res.Curve, res.Trades, benchCurve, e.spec.RiskFreeRate)
	// Half a trading year: long enough for the statistic to mean something,
	// short enough to show a regime change while it is happening.
	res.Rolling = RollingStats(res.Curve, benchCurve, 126, e.spec.RiskFreeRate)
	res.Manifest = e.buildManifest(res)
	res.Params = e.paramDecls
	res.ParamValues = e.activeParams()
	// The critique reads everything above it, so it is built last. It is
	// attached to every run rather than hidden behind a flag: the project's
	// stated position is that a backtesting tool which oversells itself is
	// worse than useless, and this is that position made executable.
	res.Critique = Criticise(res)
	res.Elapsed = time.Since(started).Milliseconds()
	return res, nil
}

// activeParams reports the value in force for every declared parameter.
func (e *Engine) activeParams() map[string]any {
	if len(e.paramDecls) == 0 {
		return nil
	}
	out := make(map[string]any, len(e.paramDecls))
	for _, d := range e.paramDecls {
		out[d.Name] = e.paramValue(d.Name, d.Default)
	}
	return out
}

// recordAI files one model or search exchange against the current day and
// keeps the running totals the manifest needs.
//
// The totals are tracked here rather than derived from the day records
// afterwards, because a sweep discards those records and the provenance must
// survive anyway.
func (e *Engine) recordAI(rec AICall) {
	e.dayAI = append(e.dayAI, rec)
	if rec.Cached {
		e.aiCacheHits++
	}
	if rec.Model != "" {
		if e.aiModels == nil {
			e.aiModels = map[string]bool{}
		}
		e.aiModels[rec.Provider+"/"+rec.Model] = true
	}
}

// loadData fetches every symbol the run needs and builds the trading calendar.
func (e *Engine) loadData(ctx context.Context) error {
	symbols := market.DedupeSymbols(e.spec.Universe)

	// A point-in-time index loads every symbol that held membership at any
	// point in the window — including the names that were later dropped,
	// which are exactly the ones survivorship bias removes. Which of them any
	// given session may trade is decided per day, in tradableSymbols.
	if e.spec.Index != "" {
		m, err := e.store.Membership(e.spec.Index)
		if err != nil {
			return err
		}
		e.members = m
		from, to := e.spec.Start, e.spec.End
		if from == "" {
			from = market.Day(earliestPossible)
		}
		if to == "" {
			to = market.NewDay(time.Now())
		}
		if !m.Covers(from) {
			e.warn(fmt.Sprintf("index membership is only recorded from %s; before that the "+
				"universe falls back to the earliest constituents on record", m.Earliest))
		}
		symbols = market.DedupeSymbols(append(m.EverMembers(from, to), symbols...))
		if len(symbols) == 0 {
			return fmt.Errorf("no %s constituents recorded between %s and %s", e.spec.Index, from, to)
		}
	}

	if len(symbols) == 0 {
		return fmt.Errorf("the strategy has an empty universe: nothing to trade")
	}

	// Load enough history before the start date for indicator warm-up.
	warmupDays := e.spec.Warmup
	if warmupDays < 5 {
		warmupDays = 5
	}

	// An empty Start means "as far back as the data goes". The real start is
	// resolved below, once we know when each symbol's history actually
	// begins, so that warm-up is still honoured.
	fullHistory := e.spec.Start == ""
	loadFrom := market.Day(earliestPossible)
	if !fullHistory {
		// Convert trading-day warmup into calendar days with slack for
		// weekends and holidays.
		loadFrom = e.spec.Start.Add(-(warmupDays*7/5 + 20))
	}

	series, errs := e.store.GetMany(ctx, symbols, loadFrom, e.spec.End)
	if len(series) == 0 {
		var first string
		for _, err := range errs {
			first = err.Error()
			break
		}
		return fmt.Errorf("no market data available for any symbol in the universe (%s)", first)
	}
	e.series = series
	for sym, err := range errs {
		e.warn(fmt.Sprintf("no data for %s: %s", sym, truncateErr(err.Error())))
	}

	// Benchmarks are loaded separately so a bad benchmark cannot break a run.
	e.benchSer = map[string]*market.Series{}
	if len(e.spec.Benchmarks) > 0 {
		bs, berrs := e.store.GetMany(ctx, e.spec.Benchmarks, loadFrom, e.spec.End)
		e.benchSer = bs
		for sym, err := range berrs {
			e.warn(fmt.Sprintf("benchmark %s unavailable: %s", sym, truncateErr(err.Error())))
		}
	}

	// The calendar spans warm-up through the end so indicators are primed,
	// but only days at or after Start are recorded and traded.
	e.days = market.TradingCalendar(series, loadFrom, e.spec.End)

	if fullHistory {
		// Begin trading once the warm-up window has elapsed, so a strategy
		// asking for a 200 day average is not run against 30 days of data.
		if len(e.days) == 0 {
			return fmt.Errorf("no market data available for the requested symbols")
		}
		i := warmupDays
		if i >= len(e.days) {
			i = len(e.days) - 1
		}
		e.spec.Start = e.days[i]
	}

	e.precomputeCalendarFlags()
	return nil
}

// earliestPossible bounds a full-history request. No symbol served here has
// daily data before this, and asking for less keeps the request sane.
const earliestPossible = "1970-01-02"

// precomputeCalendarFlags marks the last trading day of each week and month,
// which strategies need for "rebalance monthly" style rules and which cannot
// be derived from a single day in isolation.
func (e *Engine) precomputeCalendarFlags() {
	e.lastOfMonth = map[market.Day]bool{}
	e.lastOfWeek = map[market.Day]bool{}
	e.monthSeen = map[string]bool{}
	e.weekSeen = map[string]bool{}
	e.yearSeen = map[string]bool{}

	for i, d := range e.days {
		t := d.Time()
		if i == len(e.days)-1 {
			e.lastOfMonth[d] = true
			e.lastOfWeek[d] = true
			continue
		}
		next := e.days[i+1].Time()
		if next.Month() != t.Month() || next.Year() != t.Year() {
			e.lastOfMonth[d] = true
		}
		ny, nw := next.ISOWeek()
		cy, cw := t.ISOWeek()
		if ny != cy || nw != cw {
			e.lastOfWeek[d] = true
		}
	}
}

// snapshotPrices caches today's prices for fast lookup by the JS layer.
func (e *Engine) snapshotPrices(day market.Day) {
	if e.adjPrices == nil {
		e.adjPrices = make(map[string]float64, len(e.series))
		e.rawPrices = make(map[string]float64, len(e.series))
	}
	for k := range e.adjPrices {
		delete(e.adjPrices, k)
	}
	for k := range e.rawPrices {
		delete(e.rawPrices, k)
	}
	for sym, s := range e.series {
		if bar, ok := s.AsOf(day); ok {
			// Do not price a symbol before its first bar exists.
			if first, ok2 := s.First(); ok2 && first.Date > day {
				continue
			}
			e.adjPrices[sym] = bar.AdjClose
			e.rawPrices[sym] = bar.Close
		}
	}
}

// executePending fills yesterday's orders at today's open.
func (e *Engine) executePending(day market.Day) []Fill {
	if len(e.pending) == 0 {
		return nil
	}
	opens := make(map[string]float64, len(e.pending))
	for _, o := range e.pending {
		if s, ok := e.series[o.Symbol]; ok {
			if bar, ok := s.At(day); ok && bar.Open > 0 {
				// Adjusted open, so that splits do not create phantom P&L.
				opens[o.Symbol] = bar.Open * bar.SplitFactor()
			} else if px, ok := e.adjPrices[o.Symbol]; ok {
				opens[o.Symbol] = px
			}
		}
	}
	orders := e.pending
	e.pending = nil
	return e.executeAt(day, orders, opens)
}

// executeAt resolves order sizes against the given prices and fills them.
func (e *Engine) executeAt(day market.Day, orders []Order, prices map[string]float64) []Fill {
	if len(orders) == 0 {
		return nil
	}
	equity := e.portfolio.Equity(e.adjPrices)
	var fills []Fill

	// Process reductions before additions so that freed cash is available to
	// the buys in the same batch. Without this, a full rebalance would fail
	// to fund itself.
	sort.SliceStable(orders, func(i, j int) bool {
		return orderPriority(orders[i], e.portfolio, prices) < orderPriority(orders[j], e.portfolio, prices)
	})

	for _, o := range orders {
		px, ok := prices[o.Symbol]
		if !ok || px <= 0 {
			e.warn(fmt.Sprintf("no price for %s on %s: order skipped", o.Symbol, day))
			continue
		}
		shares := e.resolveShares(o, px, equity)
		if shares == 0 {
			continue
		}
		if o.Limit > 0 {
			if (shares > 0 && px > o.Limit) || (shares < 0 && px < o.Limit) {
				continue // limit not met
			}
		}
		// Refuse to spend cash the portfolio does not have.
		//
		// The budget is checked against the price the order will actually
		// fill at, including slippage and commission. Sizing against the
		// clean reference price instead would let "spend all my cash" end
		// the day slightly overdrawn, because the fill lands above it.
		if shares > 0 {
			c := e.spec.Costs
			effPrice := px * (1 + c.SlippageBps/10000.0)
			perShare := effPrice*(1+c.CommissionPct) + c.CommissionPerShare

			cost := shares*perShare + c.CommissionMin
			maxSpend := e.portfolio.Cash
			if e.spec.MaxLeverage > 1 {
				maxSpend = equity*e.spec.MaxLeverage - e.portfolio.GrossExposure(e.adjPrices)
			}
			if cost > maxSpend {
				if maxSpend <= 0 || perShare <= 0 {
					e.warnOnce("insufficient-cash", "some orders were skipped or reduced because cash was exhausted")
					continue
				}
				shares = (maxSpend - c.CommissionMin) / perShare
				if shares <= 0 {
					e.warnOnce("insufficient-cash", "some orders were skipped because cash was exhausted")
					continue
				}
				e.warnOnce("insufficient-cash", "some orders were reduced because cash was exhausted")
			}
		}
		f, err := e.portfolio.Execute(day, o.Symbol, shares, px, o.Reason, o.Tag)
		if err != nil {
			e.warn(err.Error())
			continue
		}
		if f != nil {
			fills = append(fills, *f)
		}
	}
	return fills
}

// orderPriority sorts sells and shorts ahead of buys.
func orderPriority(o Order, p *Portfolio, prices map[string]float64) int {
	switch {
	case o.Kind == KindShares && o.Shares < 0:
		return 0
	case o.Kind == KindNotional && o.Notional < 0:
		return 0
	case o.IsTarget:
		pos := p.Position(o.Symbol)
		cur := 0.0
		if pos != nil {
			cur = pos.Shares * prices[o.Symbol]
		}
		if o.Weight*1e9 < cur {
			return 0
		}
		return 1
	default:
		return 1
	}
}

// resolveShares converts an order's size expression into signed shares.
func (e *Engine) resolveShares(o Order, price, equity float64) float64 {
	shares := e.resolveRawShares(o, price, equity)
	if o.NoFlip && shares != 0 {
		cur := 0.0
		if pos := e.portfolio.Position(o.Symbol); pos != nil {
			cur = pos.Shares
		}
		switch {
		case cur == 0:
			return 0 // nothing to close
		case cur < 0 && shares > -cur:
			return -cur
		case cur > 0 && shares < -cur:
			return -cur
		}
	}
	return shares
}

func (e *Engine) resolveRawShares(o Order, price, equity float64) float64 {
	switch o.Kind {
	case KindShares:
		return o.Shares
	case KindNotional:
		return o.Notional / price
	case KindWeight:
		target := o.Weight * equity
		cur := 0.0
		if pos := e.portfolio.Position(o.Symbol); pos != nil {
			cur = pos.Shares * price
		}
		if o.IsTarget {
			return (target - cur) / price
		}
		return target / price
	}
	return 0
}

// runStops evaluates standing stop-loss, take-profit and trailing exits
// against today's high and low, before the strategy runs.
func (e *Engine) runStops(day market.Day) []Fill {
	if len(e.stops) == 0 {
		return nil
	}
	var fills []Fill
	for sym, st := range e.stops {
		pos := e.portfolio.Position(sym)
		if pos == nil {
			continue
		}
		s, ok := e.series[sym]
		if !ok {
			continue
		}
		bar, ok := s.At(day)
		if !ok {
			continue
		}
		sf := bar.SplitFactor()
		high, low := bar.High*sf, bar.Low*sf
		entry := pos.AvgPrice

		var trigger float64
		var reason string
		long := pos.Shares > 0

		if st.StopLossPct > 0 {
			var level float64
			if long {
				level = entry * (1 - st.StopLossPct)
				if low <= level {
					trigger, reason = level, fmt.Sprintf("stop loss %.1f%%", st.StopLossPct*100)
				}
			} else {
				level = entry * (1 + st.StopLossPct)
				if high >= level {
					trigger, reason = level, fmt.Sprintf("stop loss %.1f%%", st.StopLossPct*100)
				}
			}
		}
		if trigger == 0 && st.TakeProfitPct > 0 {
			if long {
				level := entry * (1 + st.TakeProfitPct)
				if high >= level {
					trigger, reason = level, fmt.Sprintf("take profit %.1f%%", st.TakeProfitPct*100)
				}
			} else {
				level := entry * (1 - st.TakeProfitPct)
				if low <= level {
					trigger, reason = level, fmt.Sprintf("take profit %.1f%%", st.TakeProfitPct*100)
				}
			}
		}
		if trigger == 0 && st.TrailingStopPct > 0 {
			if long && pos.PeakPrice > 0 {
				level := pos.PeakPrice * (1 - st.TrailingStopPct)
				if low <= level {
					trigger, reason = level, fmt.Sprintf("trailing stop %.1f%%", st.TrailingStopPct*100)
				}
			} else if !long && pos.TroughPrice > 0 {
				level := pos.TroughPrice * (1 + st.TrailingStopPct)
				if high >= level {
					trigger, reason = level, fmt.Sprintf("trailing stop %.1f%%", st.TrailingStopPct*100)
				}
			}
		}

		if trigger > 0 {
			f, err := e.portfolio.Execute(day, sym, -pos.Shares, trigger, reason, "stop")
			if err == nil && f != nil {
				fills = append(fills, *f)
			}
		}
	}
	return fills
}

func (e *Engine) snapshotPositions(day market.Day, equity float64) []PositionSnapshot {
	syms := e.portfolio.OpenSymbols()
	if len(syms) == 0 {
		return nil
	}
	out := make([]PositionSnapshot, 0, len(syms))
	for _, sym := range syms {
		pos := e.portfolio.Positions[sym]
		px := e.adjPrices[sym]
		value := pos.Shares * px
		w := 0.0
		if equity != 0 {
			w = value / equity
		}
		upl := (px - pos.AvgPrice) * pos.Shares
		rp := 0.0
		if pos.AvgPrice > 0 {
			rp = (px/pos.AvgPrice - 1)
			if pos.Shares < 0 {
				rp = -rp
			}
		}
		held := 0
		if pos.OpenedOn != "" {
			held = int(day.Time().Sub(pos.OpenedOn.Time()).Hours() / 24)
		}
		out = append(out, PositionSnapshot{
			Symbol: sym, Shares: pos.Shares, AvgPrice: pos.AvgPrice,
			Price: px, Value: value, Weight: w, UnrealizedPnL: upl,
			ReturnPct: rp, DaysHeld: held,
		})
	}
	return out
}

func (e *Engine) warn(msg string) {
	if len(e.warnings) < 100 {
		e.warnings = append(e.warnings, msg)
	}
}

func (e *Engine) warnOnce(key, msg string) {
	if e.warnSeen[key] {
		return
	}
	e.warnSeen[key] = true
	e.warn(msg)
}

func avgEquity(curve []EquityPoint) float64 {
	if len(curve) == 0 {
		return 0
	}
	var sum float64
	for _, p := range curve {
		sum += p.Value
	}
	return sum / float64(len(curve))
}

func truncateErr(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 240 {
		return s[:240] + "..."
	}
	return s
}

// nanToNull converts NaN and Inf to a nil interface so JSON and JS see null
// rather than an invalid number.
func nanToNull(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return v
}
