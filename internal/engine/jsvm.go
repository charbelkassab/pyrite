package engine

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/charbelkassab/natural-quant/internal/market"
	"github.com/dop251/goja"
)

// strategyVM wraps a goja runtime configured for strategy execution.
//
// The runtime is deliberately bare: goja ships no filesystem, network or
// process access, and nothing is added here beyond the ctx object. The only
// ways a strategy can reach the outside world are ctx.ai(), ctx.web() and
// ctx.news(), all of which are counted, capped and recorded.
type strategyVM struct {
	e       *Engine
	rt      *goja.Runtime
	onDay   goja.Callable
	setup   goja.Callable
	ctxObj  *goja.Object
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
	return vm, nil
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

	// ---- Universe -------------------------------------------------------

	// universe() reads the tradable symbol list; universe(list) sets it
	// during setup().
	set("universe", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return rt.ToValue(e.tradableSymbols())
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
		return nanToNull(Volatility(e.closes(sym, n+2), n, TradingDaysPerYear))
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
				f = Volatility(e.closes(s, window+2), window, TradingDaysPerYear)
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
		if _, ok := e.adjPrices[sym]; ok {
			out = append(out, sym)
		}
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

// isFirstOfPeriod reports the first trading day seen in each month, week or
// year, using memo maps so repeated calls in one day agree.
func (e *Engine) isFirstOfPeriod(period string) bool {
	t := e.today.Time()
	var key string
	var seen map[string]bool
	switch period {
	case "month":
		key = fmt.Sprintf("%d-%02d", t.Year(), int(t.Month()))
		seen = e.monthSeen
	case "week":
		y, w := t.ISOWeek()
		key = fmt.Sprintf("%d-W%02d", y, w)
		seen = e.weekSeen
	default:
		key = fmt.Sprintf("%d", t.Year())
		seen = e.yearSeen
	}
	if seen[key] {
		return false
	}
	// Only mark as consumed once trading has begun, so warm-up days do not
	// swallow the first real occurrence.
	if e.today >= e.spec.Start {
		seen[key] = true
		return true
	}
	return false
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
