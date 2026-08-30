package engine

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/charbelkassab/pyrite/internal/market"
	"github.com/dop251/goja"
)

// strategyVM wraps a goja runtime configured for strategy execution.
//
// The runtime is deliberately bare: goja ships no filesystem, network or
// process access, and nothing is added here beyond the ctx object. The only
// ways a strategy can reach the outside world are ctx.ai(), ctx.web() and
// ctx.news(), all of which are counted, capped and recorded.
type strategyVM struct {
	e      *Engine
	rt     *goja.Runtime
	onDay  goja.Callable
	setup  goja.Callable
	ctxObj *goja.Object
	// Optional lifecycle hooks. onFill and onStop close the one real gap in
	// the single-hook design: both fire during the engine's execution phase,
	// before onDay runs, so a strategy reacting to its own fills otherwise
	// has to re-derive from position state what the engine already knew.
	onFill  goja.Callable
	onStop  goja.Callable
	onWeek  goja.Callable
	onMonth goja.Callable
	rng     *rand.Rand
	stopped chan struct{}
}

func newStrategyVM(e *Engine) (*strategyVM, error) {
	rt := goja.New()
	// Field names reach JS exactly as declared in the JSON tags, so a
	// strategy sees bar.adj_close rather than Go's exported names.
	rt.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))

	seed := e.spec.Seed
	if seed == 0 {
		seed = 42 // fixed default so every run is reproducible
	}
	vm := &strategyVM{
		e:       e,
		rt:      rt,
		rng:     rand.New(rand.NewSource(seed)),
		stopped: make(chan struct{}),
	}

	vm.installGlobals()
	if err := vm.installContext(); err != nil {
		return nil, err
	}

	// Compile and evaluate the strategy source.
	prog, err := goja.Compile("strategy.js", e.spec.Code, true)
	if err != nil {
		return nil, fmt.Errorf("strategy failed to compile: %w", err)
	}
	if _, err := rt.RunProgram(prog); err != nil {
		return nil, fmt.Errorf("strategy failed to load: %w", err)
	}

	// onDay is required; setup is optional.
	fn := rt.Get("onDay")
	if fn == nil || goja.IsUndefined(fn) || goja.IsNull(fn) {
		return nil, fmt.Errorf("strategy must define a function named onDay(ctx)")
	}
	cb, ok := goja.AssertFunction(fn)
	if !ok {
		return nil, fmt.Errorf("onDay must be a function")
	}
	vm.onDay = cb

	if s := rt.Get("setup"); s != nil && !goja.IsUndefined(s) {
		if cb, ok := goja.AssertFunction(s); ok {
			vm.setup = cb
		}
	}
	vm.onFill = optionalHook(rt, "onFill")
	vm.onStop = optionalHook(rt, "onStop")
	vm.onWeek = optionalHook(rt, "onWeek")
	vm.onMonth = optionalHook(rt, "onMonth")
	return vm, nil
}

// optionalHook resolves a named global to a callable, or nil.
func optionalHook(rt *goja.Runtime, name string) goja.Callable {
	v := rt.Get(name)
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	cb, ok := goja.AssertFunction(v)
	if !ok {
		return nil
	}
	return cb
}

// callFillHook runs onFill or onStop for one execution.
//
// A hook that throws is recorded and the run continues, exactly as onDay is
// treated: one bad edge case should cost that callback, not the backtest.
func (v *strategyVM) callFillHook(cb goja.Callable, f Fill) error {
	if cb == nil {
		return nil
	}
	done := v.armInterrupt(30 * time.Second)
	defer close(done)

	obj := v.rt.NewObject()
	_ = obj.Set("date", string(f.Date))
	_ = obj.Set("symbol", f.Symbol)
	_ = obj.Set("side", string(f.Side))
	_ = obj.Set("shares", f.Shares)
	_ = obj.Set("price", f.Price)
	_ = obj.Set("value", f.Value)
	_ = obj.Set("commission", f.Commission)
	_ = obj.Set("slippage", f.Slippage)
	_ = obj.Set("realizedPnl", f.RealizedPnL)
	_ = obj.Set("reason", f.Reason)
	_ = obj.Set("tag", f.Tag)

	_, err := cb(goja.Undefined(), v.ctxObj, obj)
	if err != nil {
		if ex, ok := err.(*goja.Exception); ok {
			return fmt.Errorf("%s", strings.TrimSpace(ex.String()))
		}
	}
	return err
}

// callPeriodHook runs onWeek or onMonth.
func (v *strategyVM) callPeriodHook(cb goja.Callable) error {
	if cb == nil {
		return nil
	}
	done := v.armInterrupt(60 * time.Second)
	defer close(done)
	_, err := cb(goja.Undefined(), v.ctxObj)
	if err != nil {
		if ex, ok := err.(*goja.Exception); ok {
			return fmt.Errorf("%s", strings.TrimSpace(ex.String()))
		}
	}
	return err
}

func (v *strategyVM) Close() { close(v.stopped) }

// installGlobals replaces sources of non-determinism and hidden lookahead.
func (v *strategyVM) installGlobals() {
	rt := v.rt

	// Deterministic Math.random so a strategy that samples is reproducible.
	mathObj := rt.Get("Math").(*goja.Object)
	_ = mathObj.Set("random", func() float64 { return v.rng.Float64() })

	// console.log routes into the day's log so it shows in the UI.
	console := rt.NewObject()
	logFn := func(call goja.FunctionCall) goja.Value {
		v.e.appendLog(joinArgs(call))
		return goja.Undefined()
	}
	_ = console.Set("log", logFn)
	_ = console.Set("info", logFn)
	_ = console.Set("warn", logFn)
	_ = console.Set("error", logFn)
	_ = rt.Set("console", console)
}

// callSetup runs the optional setup(ctx) hook.
func (v *strategyVM) callSetup() error {
	if v.setup == nil {
		return nil
	}
	done := v.armInterrupt(10 * time.Second)
	defer close(done)
	_, err := v.setup(goja.Undefined(), v.ctxObj)
	return err
}

// callOnDay runs the strategy for the current simulated day.
func (v *strategyVM) callOnDay() error {
	// A strategy that makes model calls legitimately takes seconds; one that
	// loops forever must still be stopped.
	done := v.armInterrupt(120 * time.Second)
	defer close(done)

	_, err := v.onDay(goja.Undefined(), v.ctxObj)
	if err != nil {
		if ex, ok := err.(*goja.Exception); ok {
			return fmt.Errorf("%s", strings.TrimSpace(ex.String()))
		}
		return err
	}
	return nil
}

// armInterrupt stops runaway JS after d, and clears the interrupt afterwards.
func (v *strategyVM) armInterrupt(d time.Duration) chan struct{} {
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			v.rt.ClearInterrupt()
		case <-v.stopped:
		case <-time.After(d):
			v.rt.Interrupt(fmt.Sprintf("strategy exceeded its %s time budget for a single day", d))
		}
	}()
	return done
}

// installContext builds the ctx object handed to setup() and onDay().
func (v *strategyVM) installContext() error {
	e := v.e
	rt := v.rt
	obj := rt.NewObject()
	v.ctxObj = obj

	// ---- Dynamic scalar properties -------------------------------------
	prop := func(name string, get func() any) {
		_ = obj.DefineAccessorProperty(name,
			rt.ToValue(func(goja.FunctionCall) goja.Value { return rt.ToValue(get()) }),
			nil, goja.FLAG_FALSE, goja.FLAG_TRUE)
	}
	prop("date", func() any { return string(e.today) })
	prop("day", func() any { return string(e.today) })
	prop("dayIndex", func() any { return e.dayIdx })
	prop("cash", func() any { return e.portfolio.Cash })
	prop("equity", func() any { return e.portfolio.Equity(e.adjPrices) })
	prop("startingCash", func() any { return e.spec.InitialCash })
	prop("exposure", func() any {
		eq := e.portfolio.Equity(e.adjPrices)
		if eq == 0 {
			return 0.0
		}
		return e.portfolio.GrossExposure(e.adjPrices) / eq
	})
	prop("year", func() any { return e.today.Time().Year() })
	prop("month", func() any { return int(e.today.Time().Month()) })
	prop("dayOfMonth", func() any { return e.today.Time().Day() })
	prop("weekday", func() any { return int(e.today.Time().Weekday()) })

	// state and params are stable objects the strategy can mutate freely.
	stateObj := rt.NewObject()
	_ = obj.Set("state", stateObj)
	paramsObj := rt.NewObject()
	for k, val := range e.spec.Params {
		_ = paramsObj.Set(k, rt.ToValue(val))
	}
	_ = obj.Set("params", paramsObj)

	set := func(name string, fn any) { _ = obj.Set(name, fn) }

	// ---- Declared parameters --------------------------------------------

	// param(name, default, opts) declares a tunable and returns the value in
	// force. Declaring one is what makes a strategy sweepable: a number
	// written inline can only ever be tested at the value it was written at.
	//
	//   ctx.param("fast", 50, { grid: [10, 20, 50, 100] })
	//   ctx.param("stop", 0.08, { min: 0.02, max: 0.20, step: 0.02 })
	set("param", func(call goja.FunctionCall) goja.Value {
		name := call.Argument(0).String()
		if name == "" || goja.IsUndefined(call.Argument(0)) {
			panic(rt.NewTypeError("ctx.param() needs a name"))
		}
		var def any
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Argument(1)) {
			def = call.Argument(1).Export()
		}

		var grid []any
		var desc string
		if len(call.Arguments) > 2 {
			if m, ok := call.Argument(2).Export().(map[string]any); ok {
				if raw, ok := m["grid"].([]any); ok {
					grid = raw
				}
				if s, ok := m["description"].(string); ok {
					desc = s
				}
				lo, hasLo := toFloatOK(firstKey(m, "min", "from"))
				hi, hasHi := toFloatOK(firstKey(m, "max", "to"))
				st, hasSt := toFloatOK(m["step"])
				if len(grid) == 0 && hasLo && hasHi && hasSt {
					grid = ExpandRange(lo, hi, st)
				}
			}
		}

		v := e.declareParam(name, def, grid, desc)
		// Keep ctx.params in step so both spellings agree.
		_ = paramsObj.Set(name, rt.ToValue(v))
		return rt.ToValue(v)
	})

	// ---- Universe -------------------------------------------------------

	// universe() reads the tradable symbol list; universe(list) sets it
	// during setup().
	set("universe", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return rt.ToValue(e.tradableSymbols())
		}
		// A point-in-time index name cannot be expanded to a fixed list, so
		// it is recorded on the spec and resolved per session instead.
		if name, ok := call.Argument(0).Export().(string); ok {
			if idx := market.IndexUniverse(name); idx != "" {
				e.spec.Index = idx
				return rt.ToValue(e.tradableSymbols())
			}
		}
		syms := toSymbolList(call.Argument(0).Export())
		if len(syms) > 0 {
			e.spec.Universe = syms
		}
		return rt.ToValue(syms)
	})
	set("symbols", func() []string { return e.tradableSymbols() })
	set("warmup", func(n int) {
		if n > e.spec.Warmup {
			e.spec.Warmup = n
		}
	})
	set("hasData", func(sym string) bool {
		_, ok := e.adjPrices[market.NormalizeSymbol(sym)]
		return ok
	})

	// ---- Prices and bars ------------------------------------------------

	set("price", func(sym string) any { return nanToNull(e.priceOf(sym)) })
	set("rawPrice", func(sym string) any {
		return nanToNull(e.rawPrices[market.NormalizeSymbol(sym)])
	})
	set("open", func(sym string) any { return e.barField(sym, "open") })
	set("high", func(sym string) any { return e.barField(sym, "high") })
	set("low", func(sym string) any { return e.barField(sym, "low") })
	set("close", func(sym string) any { return e.barField(sym, "close") })
	set("volume", func(sym string) any { return e.barField(sym, "volume") })

	set("bar", func(sym string) any {
		s, ok := e.series[market.NormalizeSymbol(sym)]
		if !ok {
			return nil
		}
		b, ok := s.AsOf(e.today)
		if !ok {
			return nil
		}
		return map[string]any{
			"date": string(b.Date), "open": b.Open, "high": b.High,
			"low": b.Low, "close": b.Close, "adjClose": b.AdjClose,
			"volume": b.Volume,
		}
	})
	set("history", func(sym string, n int) []map[string]any {
		bars := e.historyBars(sym, n)
		out := make([]map[string]any, 0, len(bars))
		for _, b := range bars {
			sf := b.SplitFactor()
			out = append(out, map[string]any{
				"date": string(b.Date), "open": b.Open * sf, "high": b.High * sf,
				"low": b.Low * sf, "close": b.AdjClose, "rawClose": b.Close,
				"volume": b.Volume,
			})
		}
		return out
	})
	set("closes", func(sym string, n int) []float64 { return e.closes(sym, n) })

	// ---- Returns and indicators -----------------------------------------

	set("ret", func(sym string, n int) any { return nanToNull(Momentum(e.closes(sym, n+2), defInt(n, 1))) })
	set("momentum", func(sym string, n int) any { return nanToNull(Momentum(e.closes(sym, n+2), defInt(n, 20))) })
	set("sma", func(sym string, n int) any { return nanToNull(SMA(e.closes(sym, n+1), defInt(n, 20))) })
	set("ema", func(sym string, n int) any { return nanToNull(EMA(e.closes(sym, n*3+5), defInt(n, 20))) })
	set("rsi", func(sym string, n int) any {
		n = defInt(n, 14)
		return nanToNull(RSI(e.closes(sym, n*4+5), n))
	})
	set("stdev", func(sym string, n int) any { return nanToNull(Stdev(e.closes(sym, n+1), defInt(n, 20))) })
	set("zscore", func(sym string, n int) any { return nanToNull(ZScore(e.closes(sym, n+1), defInt(n, 20))) })
	set("highest", func(sym string, n int) any { return nanToNull(Highest(e.closes(sym, n+1), defInt(n, 20))) })
	set("lowest", func(sym string, n int) any { return nanToNull(Lowest(e.closes(sym, n+1), defInt(n, 20))) })
	set("volatility", func(sym string, n int) any {
		n = defInt(n, 20)
		return nanToNull(Volatility(e.closes(sym, n+2), n, e.scale().Periods()))
	})
	set("drawdown", func(sym string, n int) any { return nanToNull(Drawdown(e.closes(sym, defInt(n, 252)), defInt(n, 252))) })
	set("macd", func(sym string, fast, slow, signal int) any {
		fast, slow, signal = defInt(fast, 12), defInt(slow, 26), defInt(signal, 9)
		r := MACD(e.closes(sym, slow*4+signal*3+10), fast, slow, signal)
		if math.IsNaN(r.MACD) {
			return nil
		}
		return map[string]any{"macd": r.MACD, "signal": r.Signal, "histogram": r.Histogram}
	})
	set("bollinger", func(sym string, n int, k float64) any {
		n = defInt(n, 20)
		if k == 0 {
			k = 2
		}
		r := Bollinger(e.closes(sym, n+1), n, k)
		if math.IsNaN(r.Middle) {
			return nil
		}
		return map[string]any{"upper": r.Upper, "middle": r.Middle, "lower": r.Lower}
	})
	set("atr", func(sym string, n int) any {
		n = defInt(n, 14)
		h, l, c := e.ohlc(sym, n*4+5)
		return nanToNull(ATR(h, l, c, n))
	})
	// ---- Extended indicators -------------------------------------------
	//
	// These exist so the compiler never has to hand-roll one in JavaScript.
	// Each takes the same shape as the originals: a symbol, a window, and
	// null when there is not enough history.

	set("wma", func(sym string, n int) any { return nanToNull(WMA(e.closes(sym, n+1), defInt(n, 20))) })
	set("hma", func(sym string, n int) any {
		n = defInt(n, 20)
		return nanToNull(HMA(e.closes(sym, n*2+10), n))
	})
	set("roc", func(sym string, n int) any { return nanToNull(ROC(e.closes(sym, n+2), defInt(n, 20))) })
	set("trix", func(sym string, n int) any {
		n = defInt(n, 15)
		return nanToNull(TRIX(e.closes(sym, n*4+10), n))
	})
	set("adx", func(sym string, n int) any {
		n = defInt(n, 14)
		h, l, c := e.ohlc(sym, n*4+10)
		r := ADX(h, l, c, n)
		if math.IsNaN(r.ADX) {
			return nil
		}
		return map[string]any{"adx": r.ADX, "plusDI": nanToNull(r.PlusDI), "minusDI": nanToNull(r.MinusDI)}
	})
	set("cci", func(sym string, n int) any {
		n = defInt(n, 20)
		h, l, c := e.ohlc(sym, n+2)
		return nanToNull(CCI(h, l, c, n))
	})
	set("stochastic", func(sym string, n, smooth int) any {
		n, smooth = defInt(n, 14), defInt(smooth, 3)
		h, l, c := e.ohlc(sym, n+smooth+2)
		r := Stochastic(h, l, c, n, smooth)
		if math.IsNaN(r.K) {
			return nil
		}
		return map[string]any{"k": r.K, "d": nanToNull(r.D)}
	})
	set("williamsR", func(sym string, n int) any {
		n = defInt(n, 14)
		h, l, c := e.ohlc(sym, n+2)
		return nanToNull(WilliamsR(h, l, c, n))
	})
	set("obv", func(sym string, n int) any {
		c, v := e.closesVolumes(sym, defInt(n, 60))
		return nanToNull(OBV(c, v))
	})
	set("mfi", func(sym string, n int) any {
		n = defInt(n, 14)
		h, l, c, v := e.ohlcv(sym, n+2)
		return nanToNull(MFI(h, l, c, v, n))
	})
	set("vwap", func(sym string, n int) any {
		n = defInt(n, 20)
		h, l, c, v := e.ohlcv(sym, n+1)
		return nanToNull(VWAP(h, l, c, v, n))
	})
	set("cmf", func(sym string, n int) any {
		n = defInt(n, 20)
		h, l, c, v := e.ohlcv(sym, n+1)
		return nanToNull(CMF(h, l, c, v, n))
	})
	set("donchian", func(sym string, n int) any {
		n = defInt(n, 20)
		h, l, _ := e.ohlc(sym, n+1)
		return channelMap(Donchian(h, l, n))
	})
	set("keltner", func(sym string, n int, mult float64) any {
		n = defInt(n, 20)
		h, l, c := e.ohlc(sym, n*3+5)
		return channelMap(Keltner(h, l, c, n, mult))
	})
	set("supertrend", func(sym string, n int, mult float64) any {
		n = defInt(n, 10)
		h, l, c := e.ohlc(sym, n*6+10)
		r := SuperTrend(h, l, c, n, mult)
		if math.IsNaN(r.Value) {
			return nil
		}
		return map[string]any{"value": r.Value, "trend": r.Trend}
	})
	set("aroon", func(sym string, n int) any {
		n = defInt(n, 25)
		h, l, _ := e.ohlc(sym, n+2)
		r := Aroon(h, l, n)
		if math.IsNaN(r.Up) {
			return nil
		}
		return map[string]any{"up": r.Up, "down": r.Down, "oscillator": r.Oscillator}
	})
	set("psar", func(sym string, step, max float64) any {
		h, l, _ := e.ohlc(sym, 120)
		return nanToNull(PSAR(h, l, step, max))
	})
	set("ichimoku", func(sym string, conv, base, span int) any {
		conv, base, span = defInt(conv, 9), defInt(base, 26), defInt(span, 52)
		h, l, _ := e.ohlc(sym, span+2)
		r := Ichimoku(h, l, conv, base, span)
		if math.IsNaN(r.SpanB) {
			return nil
		}
		return map[string]any{
			"conversion": nanToNull(r.Conversion), "base": nanToNull(r.Base),
			"spanA": nanToNull(r.SpanA), "spanB": r.SpanB,
		}
	})
	set("choppiness", func(sym string, n int) any {
		n = defInt(n, 14)
		h, l, c := e.ohlc(sym, n+2)
		return nanToNull(Choppiness(h, l, c, n))
	})
	set("linreg", func(sym string, n int) any {
		n = defInt(n, 20)
		r := LinReg(e.closes(sym, n+1), n)
		if math.IsNaN(r.Slope) {
			return nil
		}
		return map[string]any{
			"slope": r.Slope, "intercept": r.Intercept,
			"r2": r.R2, "forecast": r.Forecast,
		}
	})

	// ---- Portfolio construction ----------------------------------------

	// optimize(symbols, opts) returns weights that sum to one, ready to hand
	// straight to ctx.rebalance().
	//
	//   ctx.rebalance(ctx.optimize(names, { objective: "hrp", lookback: 252 }))
	set("optimize", func(call goja.FunctionCall) goja.Value {
		syms := toSymbolList(call.Argument(0).Export())
		if len(syms) == 0 {
			return rt.ToValue(map[string]any{})
		}

		opts := OptimizeOptions{Objective: ObjMinVariance, Shrinkage: -1, LongOnly: true}
		lookback := 252
		if len(call.Arguments) > 1 {
			if m, ok := call.Argument(1).Export().(map[string]any); ok {
				if v, ok := m["objective"].(string); ok && v != "" {
					opts.Objective = Objective(v)
				}
				if n, ok := toFloatOK(m["lookback"]); ok && n > 0 {
					lookback = int(n)
				}
				if v, ok := toFloatOK(m["shrinkage"]); ok {
					opts.Shrinkage = v
				}
				if v, ok := toFloatOK(firstKey(m, "maxWeight", "max_weight")); ok {
					opts.MaxWeight = v
				}
				if v, ok := m["longOnly"].(bool); ok {
					opts.LongOnly = v
				}
			}
		}
		opts.RiskFree = e.spec.RiskFreeRate
		opts.PeriodsPerYear = e.scale().Periods()

		// Only symbols with a full window contribute. A name that listed
		// halfway through the lookback has no comparable covariance, and
		// padding it would fabricate a correlation that was never observed.
		var kept []string
		var series [][]float64
		for _, sym := range syms {
			r := Returns(e.closes(sym, lookback+1))
			if len(r) < lookback {
				continue
			}
			kept = append(kept, sym)
			series = append(series, r[len(r)-lookback:])
		}
		if len(kept) == 0 {
			return rt.ToValue(map[string]any{})
		}

		w := Optimize(series, opts)
		out := make(map[string]any, len(kept))
		for i, sym := range kept {
			out[sym] = w[i]
		}
		return rt.ToValue(out)
	})

	// ---- Multiple timeframes --------------------------------------------

	// resample(sym, tf, n) returns the last n bars aggregated to a coarser
	// size, so a run on 5-minute bars can read a daily trend.
	//
	//   const daily = ctx.resample("SPY", "1d", 200);
	//   const closes = daily.map(b => b.close);
	set("resample", func(sym, tf string, n int) any {
		iv, err := market.ParseInterval(tf)
		if err != nil {
			panic(rt.NewTypeError(err.Error()))
		}
		bars := e.resampleBars(sym, iv, defInt(n, 50))
		if len(bars) == 0 {
			return nil
		}
		out := make([]map[string]any, 0, len(bars))
		for _, b := range bars {
			out = append(out, map[string]any{
				"date": string(b.Date), "open": b.Open, "high": b.High,
				"low": b.Low, "close": b.AdjClose, "rawClose": b.Close,
				"volume": b.Volume,
			})
		}
		return out
	})

	// resampledCloses(sym, tf, n) is the common case of the above.
	set("resampledCloses", func(sym, tf string, n int) any {
		iv, err := market.ParseInterval(tf)
		if err != nil {
			panic(rt.NewTypeError(err.Error()))
		}
		bars := e.resampleBars(sym, iv, defInt(n, 50))
		if len(bars) == 0 {
			return nil
		}
		out := make([]float64, 0, len(bars))
		for _, b := range bars {
			out = append(out, b.AdjClose)
		}
		return out
	})

	// ---- Economic data --------------------------------------------------

	// fred(id) returns a macro series' value as of today, accounting for how
	// late the figure was actually published.
	set("fred", func(sym string) any { return nanToNull(e.fredValue(sym)) })
	// fredChange(id, n) is the change over n calendar days, as a fraction.
	set("fredChange", func(id string, days int) any {
		now := e.fredValue(id)
		then := e.fredValueOn(id, e.today.Add(-defInt(days, 365)))
		if math.IsNaN(now) || math.IsNaN(then) || then == 0 {
			return nil
		}
		return now/then - 1
	})

	set("correlation", func(a, b string, n int) any {
		n = defInt(n, 60)
		ra, rb := Returns(e.closes(a, n+1)), Returns(e.closes(b, n+1))
		if len(ra) != len(rb) {
			return nil
		}
		return nanToNull(Correlation(ra, rb))
	})
	set("beta", func(sym, bench string, n int) any {
		n = defInt(n, 60)
		ra, rb := Returns(e.closes(sym, n+1)), Returns(e.closes(bench, n+1))
		if len(ra) != len(rb) {
			return nil
		}
		return nanToNull(Beta(ra, rb))
	})

	// ---- Fundamentals and ranking ---------------------------------------

	set("marketCap", func(sym string) any {
		sym = market.NormalizeSymbol(sym)
		s, ok := e.series[sym]
		if !ok {
			return nil
		}
		mc, ok := e.store.Fundamentals().MarketCap(sym, e.today, s)
		if !ok {
			return nil
		}
		return mc
	})
	set("rankByMarketCap", func() []map[string]any {
		ranks := e.store.Fundamentals().RankByMarketCap(e.today, e.tradableSeries())
		out := make([]map[string]any, 0, len(ranks))
		for _, r := range ranks {
			out = append(out, map[string]any{
				"rank": r.Rank, "symbol": r.Symbol, "name": r.Name,
				"marketCap": r.MarketCap, "price": r.Price, "shares": r.Shares,
			})
		}
		return out
	})
	set("topByMarketCap", func(n int) []string {
		n = defInt(n, 1)
		ranks := e.store.Fundamentals().RankByMarketCap(e.today, e.tradableSeries())
		out := make([]string, 0, n)
		for i, r := range ranks {
			if i >= n {
				break
			}
			out = append(out, r.Symbol)
		}
		return out
	})
	set("biggestCompany", func() any {
		ranks := e.store.Fundamentals().RankByMarketCap(e.today, e.tradableSeries())
		if len(ranks) == 0 {
			return nil
		}
		return ranks[0].Symbol
	})
	set("marketCapRank", func(sym string) any {
		sym = market.NormalizeSymbol(sym)
		ranks := e.store.Fundamentals().RankByMarketCap(e.today, e.tradableSeries())
		for _, r := range ranks {
			if r.Symbol == sym {
				return r.Rank
			}
		}
		return nil
	})

	// rank(metric, n) sorts the universe by a named metric or a callback.
	set("rank", func(call goja.FunctionCall) goja.Value {
		return rt.ToValue(v.rankUniverse(call))
	})

	// ---- Positions -------------------------------------------------------

	set("positions", func() map[string]any {
		out := map[string]any{}
		for _, sym := range e.portfolio.OpenSymbols() {
			out[sym] = e.positionMap(sym)
		}
		return out
	})
	set("heldSymbols", func() []string { return e.portfolio.OpenSymbols() })
	set("position", func(sym string) any {
		sym = market.NormalizeSymbol(sym)
		if e.portfolio.Position(sym) == nil {
			return nil
		}
		return e.positionMap(sym)
	})
	set("hasPosition", func(sym string) bool {
		return e.portfolio.Position(market.NormalizeSymbol(sym)) != nil
	})
	set("shares", func(sym string) float64 {
		if p := e.portfolio.Position(market.NormalizeSymbol(sym)); p != nil {
			return p.Shares
		}
		return 0
	})
	set("weight", func(sym string) float64 {
		p := e.portfolio.Position(market.NormalizeSymbol(sym))
		if p == nil {
			return 0
		}
		eq := e.portfolio.Equity(e.adjPrices)
		if eq == 0 {
			return 0
		}
		return p.Shares * e.adjPrices[market.NormalizeSymbol(sym)] / eq
	})
	set("entryPrice", func(sym string) any {
		if p := e.portfolio.Position(market.NormalizeSymbol(sym)); p != nil {
			return p.AvgPrice
		}
		return nil
	})
	set("gainPct", func(sym string) any {
		sym = market.NormalizeSymbol(sym)
		p := e.portfolio.Position(sym)
		if p == nil || p.AvgPrice == 0 {
			return nil
		}
		g := e.adjPrices[sym]/p.AvgPrice - 1
		if p.Shares < 0 {
			g = -g
		}
		return g
	})
	set("daysHeld", func(sym string) int {
		p := e.portfolio.Position(market.NormalizeSymbol(sym))
		if p == nil || p.OpenedOn == "" {
			return 0
		}
		return int(e.today.Time().Sub(p.OpenedOn.Time()).Hours() / 24)
	})

	// ---- Orders ----------------------------------------------------------

	set("buy", func(call goja.FunctionCall) goja.Value { v.submit(call, 1, intentBuy); return goja.Undefined() })
	set("sell", func(call goja.FunctionCall) goja.Value { v.submit(call, -1, intentSell); return goja.Undefined() })
	set("short", func(call goja.FunctionCall) goja.Value { v.submit(call, -1, intentShort); return goja.Undefined() })
	set("cover", func(call goja.FunctionCall) goja.Value { v.submit(call, 1, intentCover); return goja.Undefined() })

	set("order", func(sym string, shares float64, reason string) {
		e.addOrder(Order{Symbol: market.NormalizeSymbol(sym), Kind: KindShares, Shares: shares, Reason: reason})
	})
	set("setWeight", func(sym string, w float64, reason string) {
		e.addOrder(Order{Symbol: market.NormalizeSymbol(sym), Kind: KindWeight, Weight: w, IsTarget: true, Reason: reason})
	})
	set("close", func(sym string, reason string) { v.closePosition(sym, reason) })
	set("closePosition", func(sym string, reason string) { v.closePosition(sym, reason) })
	set("exit", func(sym string, reason string) { v.closePosition(sym, reason) })

	set("liquidate", func(reason string) {
		for _, sym := range e.portfolio.OpenSymbols() {
			v.closePosition(sym, defStr(reason, "liquidate"))
		}
	})

	// rebalance({SYM: weight}) moves the whole book to target weights,
	// closing anything not named.
	set("rebalance", func(call goja.FunctionCall) goja.Value {
		targets := toWeightMap(call.Argument(0).Export())
		reason := "rebalance"
		if len(call.Arguments) > 1 {
			reason = call.Argument(1).String()
		}
		held := map[string]bool{}
		for _, s := range e.portfolio.OpenSymbols() {
			held[s] = true
		}
		for sym, w := range targets {
			e.addOrder(Order{Symbol: sym, Kind: KindWeight, Weight: w, IsTarget: true, Reason: reason})
			delete(held, sym)
		}
		for sym := range held {
			e.addOrder(Order{Symbol: sym, Kind: KindWeight, Weight: 0, IsTarget: true, Reason: reason})
		}
		return goja.Undefined()
	})

	// equalWeight([syms]) is the most common rebalance shape.
	set("equalWeight", func(call goja.FunctionCall) goja.Value {
		syms := toSymbolList(call.Argument(0).Export())
		gross := 1.0
		if len(call.Arguments) > 1 {
			if g := call.Argument(1).ToFloat(); g > 0 {
				gross = g
			}
		}
		targets := map[string]float64{}
		if len(syms) > 0 {
			w := gross / float64(len(syms))
			for _, s := range syms {
				targets[s] = w
			}
		}
		held := map[string]bool{}
		for _, s := range e.portfolio.OpenSymbols() {
			held[s] = true
		}
		for sym, w := range targets {
			e.addOrder(Order{Symbol: sym, Kind: KindWeight, Weight: w, IsTarget: true, Reason: "equal weight"})
			delete(held, sym)
		}
		for sym := range held {
			e.addOrder(Order{Symbol: sym, Kind: KindWeight, Weight: 0, IsTarget: true, Reason: "equal weight"})
		}
		return goja.Undefined()
	})

	// ---- Standing risk exits ---------------------------------------------

	set("stopLoss", func(sym string, pct float64) { e.setStop(sym, func(s *stopOrder) { s.StopLossPct = math.Abs(pct) }) })
	set("takeProfit", func(sym string, pct float64) { e.setStop(sym, func(s *stopOrder) { s.TakeProfitPct = math.Abs(pct) }) })
	set("trailingStop", func(sym string, pct float64) {
		e.setStop(sym, func(s *stopOrder) { s.TrailingStopPct = math.Abs(pct) })
	})
	set("clearStops", func(sym string) { delete(e.stops, market.NormalizeSymbol(sym)) })

	// ---- Calendar helpers -------------------------------------------------

	set("isFirstTradingDayOfMonth", func() bool { return e.isFirstOfPeriod("month") })
	set("isFirstTradingDayOfWeek", func() bool { return e.isFirstOfPeriod("week") })
	set("isFirstTradingDayOfYear", func() bool { return e.isFirstOfPeriod("year") })
	set("isLastTradingDayOfMonth", func() bool { return e.lastOfMonth[e.today] })
	set("isLastTradingDayOfWeek", func() bool { return e.lastOfWeek[e.today] })
	set("isMonthEnd", func() bool { return e.lastOfMonth[e.today] })
	set("isWeekEnd", func() bool { return e.lastOfWeek[e.today] })
	set("everyNDays", func(n int) bool {
		n = defInt(n, 1)
		return e.dayIdx%n == 0
	})

	// ---- AI, web and news --------------------------------------------------

	set("ai", func(call goja.FunctionCall) goja.Value { return v.callAI(call) })
	set("askAI", func(call goja.FunctionCall) goja.Value { return v.callAI(call) })
	set("web", func(call goja.FunctionCall) goja.Value { return v.callSearch(call, false) })
	set("search", func(call goja.FunctionCall) goja.Value { return v.callSearch(call, false) })
	set("news", func(call goja.FunctionCall) goja.Value { return v.callSearch(call, true) })

	// ---- Diagnostics --------------------------------------------------------

	set("log", func(call goja.FunctionCall) goja.Value {
		e.appendLog(joinArgs(call))
		return goja.Undefined()
	})
	set("note", func(call goja.FunctionCall) goja.Value {
		e.appendLog(joinArgs(call))
		return goja.Undefined()
	})

	return nil
}

// closePosition flattens a symbol.
func (v *strategyVM) closePosition(sym, reason string) {
	sym = market.NormalizeSymbol(sym)
	pos := v.e.portfolio.Position(sym)
	if pos == nil {
		return
	}
	v.e.addOrder(Order{
		Symbol: sym, Kind: KindShares, Shares: -pos.Shares,
		Reason: defStr(reason, "close"),
	})
	delete(v.e.stops, sym)
}

// orderIntent distinguishes the four entry points, which matters only when a
// call gives no size and the right default depends on which verb was used.
type orderIntent int

const (
	intentBuy orderIntent = iota
	intentSell
	intentShort
	intentCover
)

// submit parses the flexible buy/sell argument forms into an Order.
//
// Accepted shapes, all of which appear naturally in generated code:
//
//	ctx.buy("AAPL", 100)                  -> $100 notional
//	ctx.buy("AAPL", {notional: 100})
//	ctx.buy("AAPL", {shares: 10})
//	ctx.buy("AAPL", {weight: 0.25})       -> target weight
//	ctx.buy("AAPL", {pctCash: 0.5})       -> half of available cash
//	ctx.buy("AAPL")                       -> all available cash
//	ctx.cover("TSLA")                     -> buy back exactly the short
func (v *strategyVM) submit(call goja.FunctionCall, sign float64, intent orderIntent) {
	e := v.e
	if len(call.Arguments) == 0 {
		return
	}
	sym := market.NormalizeSymbol(call.Argument(0).String())
	if sym == "" {
		return
	}

	o := Order{Symbol: sym, NoFlip: intent == intentCover}
	arg := call.Argument(1)

	switch {
	case len(call.Arguments) < 2 || goja.IsUndefined(arg) || goja.IsNull(arg) || isString(arg):
		// No size given, or a bare reason string in the size position. The
		// latter is worth accepting rather than rejecting: ctx.close() and
		// ctx.order() both take a reason as their second argument, so
		// ctx.cover(sym, "why") is a natural thing to write, and silently
		// dropping the order would leave a position open with no diagnostic.
		if isString(arg) {
			o.Reason = arg.String()
		}
		// The sensible default depends on the verb: closing verbs flatten the
		// position, opening verbs commit available capital. Treating a bare
		// cover() as "buy with all cash" would leave the short open and
		// silently add a long on top of it.
		switch intent {
		case intentSell, intentCover:
			pos := e.portfolio.Position(sym)
			if pos == nil {
				return
			}
			o.Kind = KindShares
			o.Shares = -pos.Shares
			e.addOrder(o)
			return
		case intentShort:
			o.Kind = KindNotional
			o.Notional = -e.portfolio.Equity(e.adjPrices)
		default: // intentBuy
			o.Kind = KindNotional
			o.Notional = e.portfolio.Cash
		}
	case isNumber(arg):
		o.Kind = KindNotional
		o.Notional = sign * math.Abs(arg.ToFloat())
	default:
		m, ok := arg.Export().(map[string]any)
		if !ok {
			// An unrecognised size argument must not silently discard the
			// order. Fall back to the verb's default sizing and record a
			// warning so the cause is visible in the run notes.
			e.warnOnce("order-arg", fmt.Sprintf(
				"an order for %s passed an unrecognised size argument; the default size for that action was used", sym))
			if intent == intentSell || intent == intentCover {
				pos := e.portfolio.Position(sym)
				if pos == nil {
					return
				}
				o.Kind = KindShares
				o.Shares = -pos.Shares
			} else {
				o.Kind = KindNotional
				o.Notional = sign * e.portfolio.Cash
			}
			e.addOrder(o)
			return
		}
		switch {
		case hasKey(m, "shares"):
			o.Kind = KindShares
			o.Shares = sign * math.Abs(toFloat(m["shares"]))
		case hasKey(m, "notional"), hasKey(m, "amount"), hasKey(m, "dollars"), hasKey(m, "usd"):
			o.Kind = KindNotional
			o.Notional = sign * math.Abs(toFloat(firstKey(m, "notional", "amount", "dollars", "usd")))
		case hasKey(m, "weight"), hasKey(m, "targetWeight"):
			o.Kind = KindWeight
			o.IsTarget = true
			o.Weight = sign * math.Abs(toFloat(firstKey(m, "weight", "targetWeight")))
		case hasKey(m, "pctCash"), hasKey(m, "percentOfCash"):
			o.Kind = KindNotional
			o.Notional = sign * math.Abs(toFloat(firstKey(m, "pctCash", "percentOfCash"))) * e.portfolio.Cash
		case hasKey(m, "pctEquity"), hasKey(m, "percentOfEquity"):
			o.Kind = KindNotional
			o.Notional = sign * math.Abs(toFloat(firstKey(m, "pctEquity", "percentOfEquity"))) * e.portfolio.Equity(e.adjPrices)
		default:
			o.Kind = KindNotional
			o.Notional = sign * e.portfolio.Cash
		}
		if lim := toFloat(m["limit"]); lim > 0 {
			o.Limit = lim
		}
		if r, ok := m["reason"].(string); ok {
			o.Reason = r
		}
		if t, ok := m["tag"].(string); ok {
			o.Tag = t
		}
		// Convenience: stops declared inline with the entry.
		if sl := toFloat(firstKey(m, "stopLoss", "stop")); sl > 0 {
			e.setStop(sym, func(s *stopOrder) { s.StopLossPct = sl })
		}
		if tp := toFloat(firstKey(m, "takeProfit", "target")); tp > 0 {
			e.setStop(sym, func(s *stopOrder) { s.TakeProfitPct = tp })
		}
		if ts := toFloat(firstKey(m, "trailingStop", "trailing")); ts > 0 {
			e.setStop(sym, func(s *stopOrder) { s.TrailingStopPct = ts })
		}
	}

	if len(call.Arguments) > 2 && o.Reason == "" {
		o.Reason = call.Argument(2).String()
	}
	e.addOrder(o)
}

// rankUniverse implements ctx.rank(metricOrFn, n, opts).
func (v *strategyVM) rankUniverse(call goja.FunctionCall) []string {
	e := v.e
	syms := e.tradableSymbols()
	if len(syms) == 0 {
		return nil
	}
	n := 0
	if len(call.Arguments) > 1 {
		n = int(call.Argument(1).ToInteger())
	}
	ascending := false
	if len(call.Arguments) > 2 {
		if m, ok := call.Argument(2).Export().(map[string]any); ok {
			if b, ok := m["ascending"].(bool); ok {
				ascending = b
			}
		}
	}

	scores := make(map[string]float64, len(syms))
	arg0 := call.Argument(0)

	if fn, ok := goja.AssertFunction(arg0); ok {
		for _, s := range syms {
			res, err := fn(goja.Undefined(), v.rt.ToValue(s))
			if err != nil {
				continue
			}
			f := res.ToFloat()
			if !math.IsNaN(f) && !math.IsInf(f, 0) {
				scores[s] = f
			}
		}
	} else {
		metric := strings.ToLower(strings.TrimSpace(arg0.String()))
		window := 20
		if len(call.Arguments) > 2 {
			if m, ok := call.Argument(2).Export().(map[string]any); ok {
				if w := toFloat(firstKey(m, "window", "lookback", "period")); w > 0 {
					window = int(w)
				}
			}
		}
		for _, s := range syms {
			var f float64
			switch metric {
			case "marketcap", "market_cap", "cap", "size":
				if ser, ok := e.series[s]; ok {
					if mc, ok := e.store.Fundamentals().MarketCap(s, e.today, ser); ok {
						f = mc
					} else {
						continue
					}
				}
			case "momentum", "return", "performance", "ret":
				f = Momentum(e.closes(s, window+2), window)
			case "volatility", "vol":
				f = Volatility(e.closes(s, window+2), window, e.scale().Periods())
			case "rsi":
				f = RSI(e.closes(s, window*4+5), window)
			case "volume", "dollarvolume":
				if ser, ok := e.series[s]; ok {
					if b, ok := ser.AsOf(e.today); ok {
						f = b.Volume * b.Close
					}
				}
			case "price":
				f = e.priceOf(s)
			case "drawdown":
				f = Drawdown(e.closes(s, window), window)
			default:
				f = Momentum(e.closes(s, window+2), window)
			}
			if !math.IsNaN(f) && !math.IsInf(f, 0) {
				scores[s] = f
			}
		}
	}

	ranked := make([]string, 0, len(scores))
	for s := range scores {
		ranked = append(ranked, s)
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := scores[ranked[i]], scores[ranked[j]]
		if a == b {
			return ranked[i] < ranked[j] // deterministic tie-break
		}
		if ascending {
			return a < b
		}
		return a > b
	})
	if n > 0 && n < len(ranked) {
		ranked = ranked[:n]
	}
	return ranked
}

// ---- helpers on Engine used by the bindings -----------------------------

func (e *Engine) tradableSymbols() []string {
	out := make([]string, 0, len(e.series))
	for sym := range e.series {
		if _, ok := e.adjPrices[sym]; !ok {
			continue
		}
		// Under a point-in-time index, a symbol is only selectable on days it
		// was actually a member. Loading the union of the window is what makes
		// dropped names available at all; this is what stops a strategy
		// picking one before it joined or after it left.
		if e.members != nil && !e.members.WasMember(sym, e.today) {
			continue
		}
		out = append(out, sym)
	}
	sort.Strings(out)
	return out
}

func (e *Engine) tradableSeries() map[string]*market.Series {
	out := make(map[string]*market.Series, len(e.series))
	for sym, s := range e.series {
		if _, ok := e.adjPrices[sym]; ok {
			out[sym] = s
		}
	}
	return out
}

func (e *Engine) priceOf(sym string) float64 {
	if px, ok := e.adjPrices[market.NormalizeSymbol(sym)]; ok {
		return px
	}
	return math.NaN()
}

func (e *Engine) barField(sym, field string) any {
	s, ok := e.series[market.NormalizeSymbol(sym)]
	if !ok {
		return nil
	}
	b, ok := s.AsOf(e.today)
	if !ok {
		return nil
	}
	sf := b.SplitFactor()
	switch field {
	case "open":
		return b.Open * sf
	case "high":
		return b.High * sf
	case "low":
		return b.Low * sf
	case "close":
		return b.AdjClose
	case "volume":
		return b.Volume
	}
	return nil
}

func (e *Engine) historyBars(sym string, n int) []market.Bar {
	s, ok := e.series[market.NormalizeSymbol(sym)]
	if !ok {
		return nil
	}
	return s.History(e.today, defInt(n, 20))
}

// closes returns up to n adjusted closes ending today, oldest first.
func (e *Engine) closes(sym string, n int) []float64 {
	bars := e.historyBars(sym, defInt(n, 20))
	out := make([]float64, 0, len(bars))
	for _, b := range bars {
		out = append(out, b.AdjClose)
	}
	return out
}

func (e *Engine) ohlc(sym string, n int) (high, low, close []float64) {
	bars := e.historyBars(sym, defInt(n, 20))
	for _, b := range bars {
		sf := b.SplitFactor()
		high = append(high, b.High*sf)
		low = append(low, b.Low*sf)
		close = append(close, b.AdjClose)
	}
	return
}

// fredValue reads a macro series as of the simulated day.
func (e *Engine) fredValue(id string) float64 {
	return e.fredValueOn(id, e.today)
}

// fredValueOn reads a macro series as of an arbitrary day.
//
// Fetching happens lazily and once per series per run. A failure returns NaN
// rather than an error: a strategy that cannot reach one macro series should
// degrade, not die, and the warning tells the reader what it lost.
func (e *Engine) fredValueOn(id string, day market.Day) float64 {
	if e.Econ == nil || id == "" {
		return math.NaN()
	}
	key := strings.ToUpper(strings.TrimSpace(id))
	if e.econ == nil {
		e.econ = map[string]*market.EconSeries{}
		e.econRevised = map[string]bool{}
	}
	s, ok := e.econ[key]
	if !ok {
		var err error
		s, err = e.Econ.Series(e.ctx, key)
		if err != nil {
			e.warnOnce("fred-"+key, fmt.Sprintf("could not load the %s series: %s",
				key, truncateErr(err.Error())))
			e.econ[key] = nil
			return math.NaN()
		}
		e.econ[key] = s
		if s.Revised {
			e.econRevised[key] = true
			e.warnOnce("fred-revised-"+key, fmt.Sprintf(
				"%s is a revised series: the values used are today's vintage, not what "+
					"was published at the time. Its %d-day release lag is applied, so the "+
					"timing is right even though the figures are not the original prints.",
				key, s.LagDays))
		}
	}
	if s == nil {
		return math.NaN()
	}
	v, ok := s.AsOf(day)
	if !ok {
		return math.NaN()
	}
	return v
}

// resampleBars aggregates a symbol's history up to a coarser bar size,
// returning at most n bars ending at the current session.
//
// Nothing after the simulated moment is included, which is the only thing
// that makes this safe: a strategy asking for "today's daily bar" from inside
// a 5-minute run gets the bar as it stands so far, not as it will close.
func (e *Engine) resampleBars(sym string, iv market.Interval, n int) []market.Bar {
	if n <= 0 {
		n = 50
	}
	s, ok := e.series[market.NormalizeSymbol(sym)]
	if !ok || s == nil {
		return nil
	}
	if !iv.Coarser(e.spec.Interval) {
		// Asking for the run's own size, or finer, is not resampling. The
		// finer case would mean inventing prices.
		return s.History(e.today, n)
	}

	// Take enough fine bars to build n coarse ones, with slack for partial
	// buckets at each end.
	perCoarse := 1.0
	if fine := e.spec.Interval.PeriodsPerYear(); fine > 0 {
		perCoarse = fine / iv.PeriodsPerYear()
	}
	need := int(float64(n+2)*math.Max(perCoarse, 1)) + 2
	fine := s.History(e.today, need)
	if len(fine) == 0 {
		return nil
	}
	coarse := market.Resample(market.NewSeries(s.Symbol, fine), iv)
	if coarse == nil || len(coarse.Bars) == 0 {
		return nil
	}
	if len(coarse.Bars) > n {
		return coarse.Bars[len(coarse.Bars)-n:]
	}
	return coarse.Bars
}

// ohlcv is ohlc plus volume, for the flow indicators.
func (e *Engine) ohlcv(sym string, n int) (high, low, close, volume []float64) {
	bars := e.historyBars(sym, defInt(n, 20))
	for _, b := range bars {
		sf := b.SplitFactor()
		high = append(high, b.High*sf)
		low = append(low, b.Low*sf)
		close = append(close, b.AdjClose)
		// Volume is adjusted inversely to price: a 4:1 split quadruples the
		// share count, and pairing raw volume with adjusted prices would
		// misstate every money-flow reading across a split.
		if sf > 0 {
			volume = append(volume, b.Volume/sf)
		} else {
			volume = append(volume, b.Volume)
		}
	}
	return
}

// closesVolumes returns the two series OBV needs.
func (e *Engine) closesVolumes(sym string, n int) (close, volume []float64) {
	_, _, close, volume = e.ohlcv(sym, n)
	return
}

// channelMap renders any upper/middle/lower band for JavaScript.
func channelMap(c ChannelResult) any {
	if math.IsNaN(c.Middle) {
		return nil
	}
	return map[string]any{
		"upper": nanToNull(c.Upper), "middle": c.Middle, "lower": nanToNull(c.Lower),
	}
}

func (e *Engine) positionMap(sym string) map[string]any {
	p := e.portfolio.Positions[sym]
	if p == nil {
		return nil
	}
	px := e.adjPrices[sym]
	eq := e.portfolio.Equity(e.adjPrices)
	w := 0.0
	if eq != 0 {
		w = p.Shares * px / eq
	}
	gain := 0.0
	if p.AvgPrice > 0 {
		gain = px/p.AvgPrice - 1
		if p.Shares < 0 {
			gain = -gain
		}
	}
	held := 0
	if p.OpenedOn != "" {
		held = int(e.today.Time().Sub(p.OpenedOn.Time()).Hours() / 24)
	}
	return map[string]any{
		"symbol": sym, "shares": p.Shares, "avgPrice": p.AvgPrice,
		"price": px, "value": p.Shares * px, "weight": w,
		"unrealizedPnl": (px - p.AvgPrice) * p.Shares,
		"gainPct":       gain, "daysHeld": held,
		"isShort": p.Shares < 0, "openedOn": string(p.OpenedOn),
	}
}

func (e *Engine) addOrder(o Order) {
	if o.Symbol == "" {
		return
	}
	o.SubmittedOn = e.today
	e.pending = append(e.pending, o)
	e.dayOrders = append(e.dayOrders, o)
}

func (e *Engine) setStop(sym string, mut func(*stopOrder)) {
	sym = market.NormalizeSymbol(sym)
	s, ok := e.stops[sym]
	if !ok {
		s = &stopOrder{}
		e.stops[sym] = s
	}
	mut(s)
}

func (e *Engine) appendLog(msg string) {
	if len(e.dayLogs) < 50 {
		e.dayLogs = append(e.dayLogs, msg)
	}
}

// isFirstOfPeriod reports whether today is the first trading session of the
// month, week or year.
//
// It reads flags the engine computed once at the top of the session rather
// than deciding here. The previous design memoised on first call and answered
// false thereafter, which was fine while onDay was the only caller and became
// a trap the moment a lifecycle hook needed the same answer earlier in the
// same session.
func (e *Engine) isFirstOfPeriod(period string) bool {
	switch period {
	case "month":
		return e.firstOfMonth
	case "week":
		return e.firstOfWeek
	default:
		return e.firstOfYear
	}
}

// ---- small conversion helpers -------------------------------------------

func defInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func defStr(v, def string) string {
	if strings.TrimSpace(v) == "" || v == "undefined" {
		return def
	}
	return v
}

func isNumber(v goja.Value) bool {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) || v.ExportType() == nil {
		return false
	}
	switch v.ExportType().Kind().String() {
	case "int64", "float64", "int", "int32":
		return true
	}
	return false
}

func isString(v goja.Value) bool {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) || v.ExportType() == nil {
		return false
	}
	return v.ExportType().Kind().String() == "string"
}

func hasKey(m map[string]any, k string) bool {
	v, ok := m[k]
	return ok && v != nil
}

func firstKey(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

// toFloatOK is toFloat plus whether the value was numeric at all, so that a
// missing option and an explicit zero can be told apart.
func toFloatOK(v any) (float64, bool) {
	switch v.(type) {
	case float64, float32, int64, int, int32:
		return toFloat(v), true
	}
	return 0, false
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case int32:
		return float64(x)
	}
	return 0
}

// toSymbolList accepts an array, a comma separated string, a universe key or
// a single ticker.
func toSymbolList(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok {
				out = append(out, market.NormalizeSymbol(s))
			}
		}
		return market.DedupeSymbols(out)
	case []string:
		return market.DedupeSymbols(x)
	case string:
		return market.DedupeSymbols(market.ResolveUniverse(x))
	}
	return nil
}

// toWeightMap converts a JS object of symbol -> weight.
func toWeightMap(v any) map[string]float64 {
	out := map[string]float64{}
	m, ok := v.(map[string]any)
	if !ok {
		return out
	}
	for k, val := range m {
		out[market.NormalizeSymbol(k)] = toFloat(val)
	}
	return out
}

func joinArgs(call goja.FunctionCall) string {
	parts := make([]string, 0, len(call.Arguments))
	for _, a := range call.Arguments {
		if a == nil {
			continue
		}
		if o, ok := a.Export().(map[string]any); ok {
			parts = append(parts, fmt.Sprintf("%v", o))
			continue
		}
		parts = append(parts, a.String())
	}
	return strings.Join(parts, " ")
}
