# natural-quant Strategy API

A strategy is a small JavaScript program. natural-quant calls it once for
every trading day, in order, from the start of the backtest to the end.

```js
function setup(ctx) {          // optional, runs once before the first day
  ctx.universe(["AAPL", "MSFT"]);
  ctx.warmup(200);             // bars of history needed before trading starts
}

function onDay(ctx) {          // required, runs once per trading day
  // decide what to do today
}
```

## Execution model

- `onDay(ctx)` is called **after the close** of that day. Everything on `ctx`
  reflects information available at that close and nothing later.
- Orders you place **execute at the next session's open**. You cannot trade at
  a price you have already seen. This is deliberate and is what keeps results
  honest.
- Anything you do not sell stays held. There is no automatic liquidation.
- Exceptions thrown on one day are recorded and the run continues to the next
  day, so one bad edge case does not destroy a whole backtest.

## Values that may be missing

Indicator and price accessors return `null` when there is not enough history
(a symbol that has not listed yet, or a 200-day average on day 30). Always
guard:

```js
const fast = ctx.sma("AAPL", 50);
if (fast === null) return;      // not enough data yet
```

---

## Time and calendar

| Expression | Meaning |
| --- | --- |
| `ctx.date` | today as `"2024-03-05"` (a string property, not a function) |
| `ctx.dayIndex` | 0-based index of the day within the run |
| `ctx.year`, `ctx.month`, `ctx.dayOfMonth`, `ctx.weekday` | numeric parts; `weekday` is 0=Sunday |
| `ctx.isFirstTradingDayOfMonth()` | true on the first session of each month |
| `ctx.isFirstTradingDayOfWeek()` / `ctx.isFirstTradingDayOfYear()` | as above |
| `ctx.isLastTradingDayOfMonth()` / `ctx.isLastTradingDayOfWeek()` | true on the final session of the period |
| `ctx.everyNDays(n)` | true every n-th day of the run |

## Universe and prices

| Expression | Returns |
| --- | --- |
| `ctx.universe()` | array of symbols tradable today |
| `ctx.universe([...])` | **setup() only** — set the tradable symbol list; the data for it is loaded before the first day |
| `ctx.symbols()` | same as `ctx.universe()` |
| `ctx.hasData(sym)` | whether the symbol has a price today |
| `ctx.price(sym)` | today's split- and dividend-adjusted close |
| `ctx.rawPrice(sym)` | today's unadjusted close, as printed |
| `ctx.open/high/low/close/volume(sym)` | today's OHLCV |
| `ctx.bar(sym)` | `{date, open, high, low, close, adjClose, volume}` |
| `ctx.history(sym, n)` | array of the last n bars, oldest first |
| `ctx.closes(sym, n)` | array of the last n adjusted closes |

Use `ctx.price()` for return maths. Use `ctx.rawPrice()` only when you need
the literal printed price.

## Indicators

All take a symbol and return a number, or `null` when history is short.

| Call | Meaning |
| --- | --- |
| `ctx.sma(sym, n)` / `ctx.ema(sym, n)` | moving averages |
| `ctx.rsi(sym, n)` | Wilder RSI, default n = 14 |
| `ctx.macd(sym, fast, slow, signal)` | `{macd, signal, histogram}` |
| `ctx.bollinger(sym, n, k)` | `{upper, middle, lower}` |
| `ctx.atr(sym, n)` | average true range |
| `ctx.stdev(sym, n)` | standard deviation of closes |
| `ctx.zscore(sym, n)` | how many sigma today's close is from the n-day mean |
| `ctx.momentum(sym, n)` / `ctx.ret(sym, n)` | return over the last n bars, e.g. `0.12` |
| `ctx.volatility(sym, n)` | annualised volatility |
| `ctx.drawdown(sym, n)` | decline from the n-day peak, negative |
| `ctx.highest(sym, n)` / `ctx.lowest(sym, n)` | extremes over n bars |
| `ctx.correlation(a, b, n)` / `ctx.beta(sym, benchmark, n)` | pairwise stats |

### Trend and momentum

| Call | Meaning |
| --- | --- |
| `ctx.wma(sym, n)` / `ctx.hma(sym, n)` | weighted and Hull moving averages |
| `ctx.roc(sym, n)` | rate of change over n bars, as a fraction |
| `ctx.trix(sym, n)` | rate of change of a triple-smoothed EMA |
| `ctx.adx(sym, n)` | `{adx, plusDI, minusDI}` — trend strength; above 25 is a trend |
| `ctx.aroon(sym, n)` | `{up, down, oscillator}` — how recently the extremes occurred |
| `ctx.psar(sym, step, max)` | parabolic stop and reverse, an accelerating trailing stop |
| `ctx.supertrend(sym, n, mult)` | `{value, trend}` — ATR band; `trend` is +1 or -1 |
| `ctx.ichimoku(sym, conv, base, span)` | `{conversion, base, spanA, spanB}` |
| `ctx.linreg(sym, n)` | `{slope, intercept, r2, forecast}` — least squares over the window |

### Oscillators

| Call | Meaning |
| --- | --- |
| `ctx.stochastic(sym, n, smooth)` | `{k, d}` — where the close sits in the range, 0..100 |
| `ctx.williamsR(sym, n)` | the same idea on a -100..0 scale |
| `ctx.cci(sym, n)` | commodity channel index; ±100 is the conventional band |
| `ctx.choppiness(sym, n)` | 0..100; high means ranging, low means trending |

### Volume and flow

| Call | Meaning |
| --- | --- |
| `ctx.obv(sym, n)` | on-balance volume over the last n bars |
| `ctx.mfi(sym, n)` | money flow index — RSI weighted by volume |
| `ctx.vwap(sym, n)` | volume-weighted average price |
| `ctx.cmf(sym, n)` | Chaikin money flow; positive is accumulation |

Volume is split-adjusted to match the adjusted prices beside it, so a money-flow
reading does not jump on the day of a split.

### Channels

| Call | Meaning |
| --- | --- |
| `ctx.donchian(sym, n)` | `{upper, middle, lower}` — the n-bar high and low |
| `ctx.keltner(sym, n, mult)` | `{upper, middle, lower}` — EMA with ATR bands |

Bollinger bands widen with dispersion around the mean; Keltner bands widen with
realised range. On a series that gaps and then sits still they say different
things, so pick deliberately.

## Market capitalisation and ranking

| Call | Returns |
| --- | --- |
| `ctx.marketCap(sym)` | market cap today, or `null` if unknown |
| `ctx.biggestCompany()` | symbol with the largest market cap today |
| `ctx.topByMarketCap(n)` | the n largest, in order |
| `ctx.marketCapRank(sym)` | 1 for the largest, or `null` |
| `ctx.rankByMarketCap()` | `[{rank, symbol, name, marketCap, price, shares}]` |

`ctx.rank(metric, n, opts)` sorts the universe. `metric` is either a string —
`"momentum"`, `"volatility"`, `"rsi"`, `"marketCap"`, `"volume"`, `"price"`,
`"drawdown"` — or a callback `(sym) => number`. `opts` accepts
`{window: 20, ascending: false}`.

```js
const winners = ctx.rank("momentum", 5, { window: 126 });      // top 5
const cheap   = ctx.rank("rsi", 3, { ascending: true });        // 3 lowest RSI
const custom  = ctx.rank(s => ctx.sma(s, 20) / ctx.price(s), 5);
```

## Positions

| Call | Returns |
| --- | --- |
| `ctx.positions()` | `{SYM: {...}}` for every open position |
| `ctx.heldSymbols()` | array of symbols currently held |
| `ctx.position(sym)` | one position object, or `null` |
| `ctx.hasPosition(sym)` | boolean |
| `ctx.shares(sym)` | signed share count, negative when short |
| `ctx.weight(sym)` | fraction of equity, signed |
| `ctx.entryPrice(sym)` | average entry price |
| `ctx.gainPct(sym)` | return since entry, e.g. `0.08`; sign-corrected for shorts |
| `ctx.daysHeld(sym)` | calendar days since the position opened |

A position object is
`{symbol, shares, avgPrice, price, value, weight, unrealizedPnl, gainPct, daysHeld, isShort, openedOn}`.

Portfolio scalars: `ctx.cash`, `ctx.equity`, `ctx.startingCash`, `ctx.exposure`.

## Orders

Every order placed today fills at tomorrow's open.

```js
ctx.buy("AAPL", 100);                  // $100 of notional
ctx.buy("AAPL", { notional: 100 });    // identical
ctx.buy("AAPL", { shares: 10 });
ctx.buy("AAPL", { weight: 0.25 });     // move to 25% of equity
ctx.buy("AAPL", { pctCash: 0.5 });     // spend half of available cash
ctx.buy("AAPL", { pctEquity: 0.1 });   // spend 10% of equity
ctx.buy("AAPL");                       // spend all available cash

ctx.sell("AAPL", { shares: 5 });       // partial exit
ctx.sell("AAPL");                      // flatten the position
ctx.close("AAPL");                     // flatten, clearer intent
ctx.liquidate();                       // flatten everything

ctx.short("TSLA", { notional: 5000 }); // requires shorting to be enabled
ctx.cover("TSLA");                     // buy back

ctx.order("AAPL", -10);                // signed share count
ctx.setWeight("AAPL", 0.3);            // target weight, negative to short
```

Rebalancing the whole book — anything not named is closed:

```js
ctx.rebalance({ AAPL: 0.4, MSFT: 0.4, TLT: 0.2 });
ctx.equalWeight(["AAPL", "MSFT", "NVDA"]);        // 1/3 each
ctx.equalWeight(winners, 0.8);                     // 80% invested, 20% cash
```

## Portfolio construction

`ctx.optimize(symbols, opts)` returns weights summing to one, ready to hand
straight to `ctx.rebalance()`.

```js
function onDay(ctx) {
  if (!ctx.isFirstTradingDayOfMonth()) return;
  const w = ctx.optimize(ctx.universe(), { objective: "hrp", lookback: 252 });
  ctx.rebalance(w);
}
```

| `objective` | What it does |
| --- | --- |
| `"min_variance"` | the lowest-variance combination (default) |
| `"max_sharpe"` | the tangency portfolio, using the run's risk-free rate |
| `"risk_parity"` | every holding contributes the same share of total risk |
| `"hrp"` | hierarchical risk parity — clusters by correlation, never inverts the covariance matrix |
| `"inverse_vol"` | weight by 1/volatility, the crude version of risk parity |
| `"equal"` | the baseline the others have to beat |

Other options: `lookback` (bars of history, default 252), `maxWeight` (a
per-holding cap), `longOnly` (default true), and `shrinkage` — how far to blend
the covariance matrix toward its diagonal, defaulting to the Ledoit–Wolf
estimate chosen from the data.

**Why shrinkage is on by default.** A covariance matrix estimated from 252 days
across 30 assets is mostly noise, and `min_variance` and `max_sharpe` invert it,
which amplifies exactly that noise into confident, wrong weights. Shrinking
toward the diagonal is what stops the optimiser betting the portfolio on a
correlation that was never really there. `hrp` sidesteps the problem entirely by
never inverting anything, which is the reason to prefer it when the universe is
large relative to the history.

Symbols without a full lookback window are excluded rather than padded: a name
that listed halfway through has no comparable covariance, and filling it in
would fabricate a correlation nobody observed. Declare the method as a parameter
and let a sweep tell you which one your universe actually rewards.

Optional keys on any order object: `limit`, `reason`, `tag`. `reason` is shown
in the day-detail view and is worth setting — it is how a reader understands
why a trade happened.

A reason may also be passed positionally. Both of these work, and a bare
closing verb flattens the position rather than sizing a new one:

```js
ctx.cover("TSLA", "signal reversed");            // close the short, with a reason
ctx.buy("AAPL", { notional: 100 }, "new leader"); // reason after the size
```

## Risk exits

Standing exits are checked against each day's high and low, before `onDay`
runs.

```js
ctx.stopLoss("AAPL", 0.08);       // exit if 8% below entry
ctx.takeProfit("AAPL", 0.25);     // exit if 25% above entry
ctx.trailingStop("AAPL", 0.10);   // exit 10% below the best price since entry
ctx.clearStops("AAPL");

ctx.buy("AAPL", { pctCash: 0.5, stopLoss: 0.08, trailingStop: 0.15 });
```

## AI and the internet, from inside the strategy

These are for strategies that genuinely need judgement or outside
information. They are counted and capped, and every call is recorded so you
can inspect exactly what was asked and answered on any day.

```js
const view = ctx.ai("In one word, BULLISH or BEARISH: " + headline);

const parsed = ctx.ai("Return JSON {sentiment:-1..1, reason:string} for: " + text,
                      { json: true });
if (parsed && parsed.sentiment > 0.5) ctx.buy("SPY", { pctCash: 1 });

const hits = ctx.news("Apple", { limit: 5 });   // [{title, url, snippet, published}]
const web  = ctx.web("Fed decision", { limit: 3 });
```

`ctx.ai(prompt, opts)` — `opts` accepts `{json: true, tier: "fast"|"balanced"|"quality", maxTokens, system}`.
Returns a string, or a parsed value when `json` is set, or `null` on failure.

**Do not set a small `maxTokens`.** Even when you ask for a single word, the
model serving these calls reasons before it answers, and a tight budget is
consumed entirely by that reasoning — leaving the visible reply empty. Leave
`maxTokens` unset unless you have a specific reason; the default already
reserves enough headroom.

Always check the reply before acting on it. `ctx.ai()` returns `null` when a
call fails, and a model may answer in a form you did not anticipate:

```js
const verdict = ctx.ai("Answer with one word, BUY or SELL: " + headline);
if (!verdict) return;                       // the call failed; do nothing today
if (/BUY/i.test(verdict))       ctx.setWeight("SPY", 1);
else if (/SELL/i.test(verdict)) ctx.setWeight("SPY", 0);
// no else — an unrecognised answer should not silently mean "sell"
```

Answers are cached per `(day, prompt)`, so the first run pays for the calls and
every later run is free and returns identical answers.

**Read this before writing an AI or web strategy.** `ctx.web()` and
`ctx.news()` query the internet *as it is now*, not as it was on the simulated
day. A backtest over 2019 that searches the web is seeing 2026 information.
That is lookahead bias and it will flatter results, sometimes enormously.
`ctx.ai()` has a milder version of the same problem: the model knows what
happened after the simulated date. Treat AI-driven backtests as illustrations
of a mechanism, not as evidence that a strategy works.

## State and logging

`ctx.state` is an object that persists across days — use it for anything you
need to remember. `ctx.log(...)` writes to the day's log, visible in the
day-detail panel. `console.log` does the same.

```js
function onDay(ctx) {
  ctx.state.peak = Math.max(ctx.state.peak || 0, ctx.equity);
  if (ctx.equity < ctx.state.peak * 0.8) {
    ctx.log("down 20% from peak, going to cash");
    ctx.liquidate();
  }
}
```

## Parameters

Declare every number the strategy depends on. `ctx.param(name, default, opts)`
registers a tunable in `setup()` and returns the value in force, which is the
default unless a run overrides it.

```js
function setup(ctx) {
  ctx.universe(["SPY"]);
  ctx.param("fast", 50,  { grid: [20, 35, 50, 65, 80] });
  ctx.param("slow", 200, { grid: [100, 150, 200, 250] });
  ctx.param("stop", 0.12, { min: 0.05, max: 0.20, step: 0.05 });
  ctx.warmup(280);          // the largest value the grids can reach
}

function onDay(ctx) {
  const fast = ctx.sma("SPY", ctx.params.fast);
  const slow = ctx.sma("SPY", ctx.params.slow);
  if (fast === null || slow === null) return;
  ...
}
```

`opts` accepts `{grid: [...]}` for explicit values, `{min, max, step}` for a
numeric range, and `{description}`. Omit all three and the parameter is fixed:
still overridable by hand, but contributing no dimension to a search.

The declared values are read back through `ctx.params.<name>`, so the two
spellings always agree.

**Why bother.** A number written inline can only ever be tested at the value it
was written at. Declaring it is what lets `natural-quant sweep` search the
space and `natural-quant walkforward` choose on one period and report on
another — which is the difference between knowing an idea works and knowing
that one number fitted one sample.

Set `ctx.warmup()` from the largest value any lookback grid can reach. A grid
running to 250 behind a warm-up of 200 silently produces no trades at its own
upper end, and the sweep will report that as a bad parameter rather than as
missing history.

---

## Worked examples

**Buy the biggest company, sell when it is no longer number one**

```js
function setup(ctx) { ctx.universe("megacap"); }

function onDay(ctx) {
  const top = ctx.biggestCompany();
  if (!top) return;
  for (const sym of ctx.heldSymbols()) {
    if (sym !== top) ctx.close(sym, "no longer the largest company");
  }
  if (!ctx.hasPosition(top)) ctx.buy(top, 100, "largest company by market cap");
}
```

**Golden cross with a trailing stop**

```js
function setup(ctx) { ctx.universe(["SPY"]); ctx.warmup(220); }

function onDay(ctx) {
  const fast = ctx.sma("SPY", 50), slow = ctx.sma("SPY", 200);
  if (fast === null || slow === null) return;
  if (fast > slow && !ctx.hasPosition("SPY")) {
    ctx.buy("SPY", { pctCash: 1, trailingStop: 0.12 }, "50d crossed above 200d");
  } else if (fast < slow && ctx.hasPosition("SPY")) {
    ctx.close("SPY", "50d crossed below 200d");
  }
}
```

**Monthly momentum rotation into the top 3 names**

```js
function setup(ctx) { ctx.universe("tech"); ctx.warmup(140); }

function onDay(ctx) {
  if (!ctx.isFirstTradingDayOfMonth()) return;
  const winners = ctx.rank("momentum", 3, { window: 126 });
  if (winners.length) ctx.equalWeight(winners);
}
```

**Mean reversion: buy oversold, exit on recovery**

```js
function setup(ctx) { ctx.universe("megacap"); ctx.warmup(60); }

function onDay(ctx) {
  for (const sym of ctx.universe()) {
    const rsi = ctx.rsi(sym, 14);
    if (rsi === null) continue;
    if (rsi < 30 && !ctx.hasPosition(sym) && ctx.cash > 5000) {
      ctx.buy(sym, { pctEquity: 0.1, stopLoss: 0.1 }, "RSI " + rsi.toFixed(0));
    } else if (rsi > 55 && ctx.hasPosition(sym)) {
      ctx.close(sym, "RSI recovered");
    }
  }
}
```

**News-driven, using the model inside the loop**

```js
function setup(ctx) { ctx.universe(["AAPL"]); }

function onDay(ctx) {
  if (!ctx.isFirstTradingDayOfWeek()) return;
  const hits = ctx.news("Apple stock", { limit: 3 });
  if (!hits.length) return;
  const verdict = ctx.ai(
    "Headlines:\n" + hits.map(h => "- " + h.title).join("\n") +
    "\nAnswer with one word, BUY or SELL.");
  if (verdict && /BUY/i.test(verdict)) ctx.setWeight("AAPL", 1);
  else ctx.setWeight("AAPL", 0);
}
```
