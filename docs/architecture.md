# Architecture

natural-quant is one Go binary with an embedded single-page front end. This is a
tour of how a sentence becomes an equity curve, and why each piece works the way
it does.

## Packages

```
cmd/natural-quant      CLI and server entry point
internal/
  config               env + file configuration, tier routing
  llm                  one OpenAI-compatible client for all three providers
  market               prices, cache, fundamentals, universes
  engine               backtest loop, portfolio, metrics, JS bindings
  strategy             prompt -> validated JavaScript
  websearch            keyless news and web lookups
  server               JSON API, SSE, static assets
  app                  wiring: builds everything from a Config
web/                   front end, embedded with go:embed
```

Dependency direction is strictly one-way: `server` → `app` → `strategy` →
`engine` → `market`. Nothing lower ever imports something higher.

Inside `engine`, the files split along what they answer:

```
engine.go        the daily loop
portfolio.go     cash, positions, fills, financing
indicators.go    the maths a strategy can call
jsvm.go          the ctx object handed to the strategy
metrics.go       the headline numbers
riskmetrics.go   distribution shape, drawdown persistence, capture
trades.go        fills paired into round trips, with MAE/MFE
attribution.go   by year, by regime, by holding, and stress tests
params.go        declared tunables and grid expansion
sweep.go         the parallel search
walkforward.go   rolling train/test evaluation
robustness.go    deflated Sharpe, PBO, bootstrap, plateau
critique.go      what is wrong with this result
manifest.go      provenance
```

## The request path

```
POST /api/runs {prompt, start, end, ...}
    │
    ├─ RunStore.Create              → run id returned immediately (202)
    │
    └─ goroutine:
         strategy.Compile           quality tier, up to 3 attempts
           ├─ systemPrompt() embeds api.md verbatim
           ├─ model returns JSON: code, universe, warmup, assumptions, ...
           ├─ goja.Compile           syntax check
           ├─ banned-construct scan  require/fetch/setTimeout
           └─ smoke backtest         ~150 days, AI disabled
                  ↓ failures fed back as a repair message
         engine.Run                  full period, progress published per 20 days
         RunStore.Save               JSON on disk

GET /api/runs/{id}/events           SSE: status, progress, done
```

The client watches the SSE stream, so a five-minute run reports progress instead
of hanging on one request.

## The search path

```
POST /api/sweeps {prompt|code, grids, objective, walk_forward, ...}
    │
    └─ goroutine, same Run lifecycle as a backtest:
         planFor                     compile, or take supplied code
         DeclaredParams              run setup() once to find ctx.param()
           └─ one data load, then shared by every worker
         Combos                      cartesian product, stably ordered
         worker pool                 one goja.Runtime per worker, reused
           └─ each run: OmitDayRecords, full metrics kept
         AssessRobustness            spread, plateau, expected max
         AddPBO                      combinatorially symmetric CV
         AddDeflatedSharpe           winner's own skew and kurtosis
         Finish                      write the verdict last
```

A sweep reuses the whole `Run` lifecycle — progress, SSE, cancellation,
persistence — because it is the same thing many times over. A parallel store
for it would be duplication rather than design.

### Why the parallelism is nearly free

`market.Store` is read-only once loaded and guards itself with a mutex, so N
workers share one copy of the price data with no coordination and no
duplication. There is no interpreter lock to work around and nothing to
serialise between processes. What limits the search is the interpreter, not the
data layer, which is why `Spec.OmitDayRecords` exists: a full `DayRecord` per
session is the single most valuable thing one run produces and the fastest way
to exhaust memory across ten thousand of them.

Walk-forward is a sweep per training window plus one untouched run on the test
window that follows, chained so the stitched curve is the equity of someone who
actually re-optimised on that schedule. The embargo between them defaults to the
strategy's warm-up, which is exactly the horizon over which an indicator can
leak across the boundary.

## NaN is a wire-format problem, not a maths problem

`encoding/json` refuses to encode NaN and ±Inf. Because `net/http` has already
written the status line by the time the encoder reaches a bad float, the client
receives a 200 with an empty body and no error anywhere — the server log is the
only trace.

Every metric that can legitimately be undefined is therefore an `engine.Ratio`,
which marshals to `null`: a Sortino with no losing days, a profit factor with no
losing trades, a score for a combination that failed. `TestResultAlwaysEncodes`
and its siblings encode a result, a sweep and a walk-forward with deliberately
undefined cells and fail on any non-finite literal reaching the output.

## Why JavaScript in goja

The alternative was generating Go and compiling it at runtime. That is slower,
much harder to sandbox, and needs a toolchain on the user's machine.

[goja](https://github.com/dop251/goja) is a pure-Go ES5.1+ interpreter. It ships
with **no** filesystem, network, process or timer access — the sandbox is the
default, not something bolted on. The only capabilities a strategy has are the
ones explicitly attached to the `ctx` object.

Two globals are replaced for determinism:

- `Math.random` is reseeded from the run's seed, so a sampling strategy is
  reproducible.
- `console.log` routes into the day's log so it appears in the interface.

Each `onDay` call is guarded by `Runtime.Interrupt` on a watchdog timer, so an
accidental infinite loop stops that day rather than wedging the server.

Language models write good, idiomatic JavaScript, which matters more than it
sounds: the quality ceiling of the whole tool is set by how reliably the model
hits the API on the first try.

## The daily loop

For each date in the calendar:

1. **Execute yesterday's orders** at today's open, and evaluate standing stops
   against today's high and low.
2. **Accrue financing** — short borrow, cash interest — and update trailing marks.
3. **Call `onDay(ctx)`**, but only once the date reaches the backtest start.
   Earlier dates exist purely to warm up indicators.
4. **Mark to market**, record the equity point and the full day record.

Orders submitted during step 3 are queued, not filled. They execute in step 1 of
the *next* iteration. This is the single most important design decision in the
engine: a strategy can never trade at a price it has already seen.

`FillClose` is offered for comparison with published backtests that do fill at the
close, and the interface labels it as optimistic.

### Order sizing

Orders are expressed as shares, notional dollars, or a target weight, and resolved
against prices at execution time rather than submission time. Within a batch,
reductions are processed before additions so a full rebalance can fund itself
from its own sales.

Buys are clamped to available cash (or to the leverage limit) and reduced rather
than rejected, with a warning recorded — a strategy that slightly overcommits
should degrade, not fail.

## Market data

`market.Store` wraps a `Provider` with an in-memory map and a disk cache. It
always fetches a wider window than asked for, so the second backtest over an
overlapping period is free. A symbol that fails resolves to a per-symbol error
rather than failing the run: a bad ticker in a 40-name universe should cost you
that name, not the backtest.

The trading calendar is the **union** of dates across loaded symbols, not the
intersection, so a halted or newly listed symbol does not truncate everything
else. `Series.AsOf` returns the last bar on or before a date, and the engine
refuses to price a symbol before its first bar exists.

## The model router

All three providers speak the same `/chat/completions` contract, so there is one
client with a configurable base URL rather than three integrations. The
differences that do exist are handled by adaptive retry: if an endpoint rejects
`max_tokens`, the client resends with `max_completion_tokens`; if it rejects a
custom temperature, it drops it. That keeps the code free of per-model
special-casing that would rot as model families change.

Tiers (`fast` / `balanced` / `quality`) map to providers, and resolution degrades
gracefully — a quality request served by a fast model beats no model at all.

### The AI cache is load-bearing

`ctx.ai()` replies are cached on disk keyed by *(provider, model, simulated day,
prompt)*. A strategy calling the model once per trading day over five years makes
~1,250 requests. Without the cache, every re-run — every position-sizing tweak,
every new benchmark — pays that again in money and minutes. With it, only the
first run is expensive, and AI-driven backtests become exactly reproducible,
which is what makes comparing two variants meaningful.

## Reference data from filings

Ranking by market cap needs shares outstanding as of the historical date.
`internal/market/edgar.go` builds that table from the SEC's XBRL company-facts
API, which is free and needs no key beyond a declared User-Agent.

Two decisions in there are worth knowing about. Rows are dated by the filing's
publication date rather than the cover date it measured, because a count printed
on a 31 March cover page was not knowable until the 10-Q appeared in May.
And the client reads `companyfacts` rather than `companyconcept`: the
per-concept endpoint returns an empty object where an array is documented for
some filers, and one request per company is politer than one per tag anyway.

Tag preference is ordered by how closely each answers the question a market cap
asks. Weighted-average tags sit above `CommonStockSharesIssued` deliberately:
issued counts include treasury stock, so for a company that has bought back
heavily they are wrong by a wide margin, while the weighted average lands within
a fraction of a percent. Multi-class filers report against a share-class axis
that the XBRL API does not expose at all, so they fall back to the average and
are flagged as approximate rather than dropped.

## Front end

Vanilla JavaScript, no build step. Everything on screen derives from a list of
*entities*: a strategy run or a plain symbol. Each entity claims a colour slot
from a fixed categorical palette at creation and holds it until removed, so
colour follows the entity rather than its rank — removing one comparison never
repaints the others.

Curves are drawn as percentage return by default, which is the only honest way to
put a $100,000 portfolio and a $300 share price on one axis.

`--dev ./web` serves the assets from disk instead of the embedded copy, because
`go:embed` otherwise requires a rebuild for every CSS tweak.
