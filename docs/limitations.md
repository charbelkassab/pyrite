# What natural-quant cannot tell you

Every backtesting tool flatters its results. This page is the honest accounting
of how, so that you can discount what you see by roughly the right amount.

## Biases that are present

### Survivorship bias

The built-in universes (`megacap`, `tech`, `faang`, `dow`, `sectors`) list the
companies that matter **today**. They are not point-in-time index membership.

A backtest starting in 2013 that "holds the top 5 mega caps" is choosing from a
list that already knows NVIDIA became enormous and that Nokia and Sears did not.
Real-time, you would have been picking from a list containing names that went on
to fail, and your results would have been worse — often much worse.

This is the single largest distortion in the tool. It affects any strategy that
selects from a universe rather than trading a fixed ticker. A strategy that only
trades SPY is unaffected.

**Mitigation:** pin the universe yourself to symbols that existed and were
plausible choices at the start of your window, using `--universe` or the
universe field in the interface.

### Approximate market capitalisation

Ranking by market cap needs shares outstanding *as of the historical date*. No
free, keyless source provides that series: Yahoo's `quoteSummary` and `v7/quote`
endpoints return HTTP 401, and the remaining free tiers give only a current
snapshot.

Using one present-day share count across a whole backtest would silently corrupt
every historical ranking — Apple's count alone has moved by a factor of four
through splits and buybacks. So natural-quant ships a curated table of
piecewise-constant share counts at
`internal/market/assets/shares_outstanding.csv`.

What that means in practice:

- Figures are rounded to roughly three significant figures. Good enough to rank
  mega caps against each other; not accounting-grade.
- Symbols with a single row use one recent count for all of history. Accuracy for
  those names decays the further back you test.
- The table covers large caps, not the whole market.

`ctx.marketCap()` returns `null` for symbols with no data, and ranking functions
skip them rather than guessing.

**Corrections with a citation are the highest-value contribution to this
project.** See [CONTRIBUTING.md](../CONTRIBUTING.md).

### Lookahead bias in AI and web strategies

This one is severe and worth understanding precisely.

`ctx.web()` and `ctx.news()` query the live internet. When you backtest a
news-reading strategy over 2019, the search returns *today's* internet — articles
written years after the simulated date, indexed by relevance to what turned out to
matter. A strategy that "reads the news and decides" is, in a 2019 backtest,
reading a summary of what already happened.

`ctx.ai()` has a subtler version: the model's training data includes everything
after your simulated date. Ask it "is the outlook for this company positive?" in a
2020 backtest and it answers with knowledge of 2021–2026.

natural-quant does not prevent this — it cannot. What it does instead:

- Records every model and web call against the day it was made, visible in the
  day-detail panel, so you can see exactly what the strategy was told.
- Caches replies per `(day, prompt)` so results are at least reproducible.
- Warns in the interface and the run notes whenever a run used these hooks.

**Treat AI-driven backtests as illustrations of a mechanism, not as evidence.**

### AI calls can fail mid-backtest

A model call inside a backtest is a network request, and it can fail. When it
does, `ctx.ai()` returns `null` and the day proceeds without a verdict — the
run continues rather than aborting, and the failure is recorded against that
day and surfaced in the run warnings.

That means a strategy which does not check for `null` will silently do nothing
on the affected days. Check the Notes tab for AI warnings before drawing
conclusions from a run that used them.

Two related sharp edges worth knowing:

- The model serving in-strategy calls reasons before it answers, so a very
  small token budget produces an empty reply. natural-quant applies a floor and
  retries with a larger budget, but a strategy that overrides `maxTokens` down
  to single digits is still asking for trouble.
- There is a per-run cap on model and web calls (`NQ_MAX_AI_CALLS`, 2000 by
  default). Past it, `ctx.ai()` returns `null` and a warning is recorded. A
  daily AI strategy over ten years will hit this.

### Overfitting by prompt iteration

Trying twenty phrasings and keeping the one with the best chart is curve fitting,
just with English instead of parameters. The tool makes this fast, which makes it
dangerous. Prefer to decide what you believe first, test it once, and take the
answer.

## Which prompts work, and which do not

The [prompt corpus](../internal/strategy/testdata/corpus.json) is run against
live models and real data. As of the last full pass, **40 of 42 prompts
compiled and ran on the first attempt**; the two failures were AI strategies
that exceeded a time budget, not incorrect code, and both now pass.

### Reliably supported

Trend and crossover rules · momentum and relative-strength ranking · mean
reversion on RSI, Bollinger bands and z-scores · fixed allocations and periodic
rebalancing · dollar-cost averaging · market-cap ranking and selection · sector
and asset rotation · long/short and pairs trades · stop-loss, take-profit and
trailing exits · volatility targeting · regime filters using the VIX or a moving
average · calendar and seasonality rules · holding-period rules · position and
concentration limits · strategies that consult a model or the news.

Vague prompts ("something that beats the market but isn't too risky", "invest
like Warren Buffett") also produce a working strategy — the model picks a
concrete interpretation and records it under **Assumptions**. Read that before
trusting the result: the assumptions often *are* the strategy.

### Not supported, and why

| Ask | Why not |
| --- | --- |
| Anything intraday — "buy at 10am", "sell on a 5 minute breakout" | The engine is daily-bar only |
| Options, spreads, covered calls | No options data or pricing model |
| "Buy companies with a P/E under 15", earnings surprises, revenue growth | No fundamentals beyond share counts |
| Futures roll, continuous contracts | Not modelled |
| Order-book, bid/ask spread, level 2 | Not available in daily bars |
| "Rebalance when my broker charges under $X" | No broker integration; costs are a model |
| Point-in-time index membership — "the S&P 500 as it was in 2012" | Universes are current-membership only |
| Anything needing per-name news history at a past date | Search returns today's internet |

When a request touches one of these, the compiler is instructed to implement
the closest daily-bar approximation and state the gap under **Limitations** in
the run notes, rather than silently pretending. If you find it doing that
badly, that is a bug worth reporting.

### Sharp edges in generated code

Two failure modes appear often enough to name:

- **Crossings versus conditions.** "Buy when the 50 day crosses above the 200
  day" is not the same as "buy while the 50 day is above the 200 day". The
  compiler is told to track prior state for crossing language, but if a
  strategy trades far less often than you expect, check the code for this.
- **Missing the opening state.** A pure crossing strategy holds nothing until
  the first crossing *after* the backtest starts. If the condition was already
  true on day one, it will sit in cash until the next flip.

## What is not modelled

| | |
| --- | --- |
| **Taxes** | No capital gains, dividend or wash-sale treatment |
| **Intraday prices** | Daily bars only. A stop between the open and close triggers at the modelled level, not the real tick |
| **Options and futures** | Not supported. Futures continuation and roll are not modelled |
| **Market impact** | Fills assume your order does not move the price |
| **Borrow availability** | Shorts always fill; a real hard-to-borrow name may not be available at all |
| **Delistings and spin-offs** | Not modelled; a symbol simply stops having data |
| **Dividends as cash** | Reinvested implicitly via adjusted closes, not paid into the cash balance |
| **After-hours and gaps** | An overnight gap through a stop fills at the stop level, which is optimistic |

## What is modelled

| | |
| --- | --- |
| **Next-open fills** | Orders placed on day D fill at D+1's open. This is the default and the only lookahead-free choice |
| **Slippage** | 5 basis points against you by default, configurable — not zero |
| **Commission** | Per-share, percentage and per-order minimum, all configurable |
| **Short borrow** | Charged daily at an annual rate on the value of short positions |
| **Splits and dividends** | Via adjusted closes; raw closes retained where share counts require them |
| **Cash drag** | Uninvested cash earns nothing unless you set a rate |
| **Partial fills on exhausted cash** | Orders are reduced rather than silently overdrawing, and a warning is recorded |

## Reading a result honestly

A few habits that help:

1. **Always compare against buy-and-hold.** The interface adds SPY by default.
   A strategy that underperforms a index fund after costs is not interesting,
   however clever it is.
2. **Look at max drawdown before total return.** Returns are what you hope for;
   drawdown is what you actually have to live through.
3. **Check the trade count.** Three trades over ten years is not a strategy, it
   is an anecdote. Two thousand trades means costs dominate.
4. **Read the Notes tab.** The assumptions the model made where your wording was
   ambiguous are often the whole result.
5. **Re-run over a different period.** A strategy that works only in 2020–2021
   found a bull market, not an edge.
