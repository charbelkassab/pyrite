<div align="center">

# natural-quant

**Describe a trading strategy in plain English. Watch how it would have done.**

natural-quant turns a sentence into a real, runnable trading strategy, backtests it
against years of market data, and puts the result on a TradingView-grade chart next
to any stock, index or rival strategy you want to compare it with.

[Quick start](#quick-start-60-seconds) ·
[Examples](#what-you-can-ask-for) ·
[How it works](#how-it-works) ·
[Which AI provider](#choosing-an-ai-provider) ·
[Limitations](#limitations-please-read)

![natural-quant comparing a trend-following strategy against the S&P 500](docs/images/screenshot-chart.png)

</div>

---

## The idea

Backtesting frameworks make you learn a framework. You want to test "buy the biggest
company in America every day and sell it when it stops being the biggest" — and
instead you are reading API docs about `Portfolio.rebalance()` for an hour.

natural-quant removes that step. You write the idea the way you would say it out
loud. A language model translates it into JavaScript against a documented strategy
API, the code runs in a sandbox over real historical data, and you get an equity
curve, trade-by-trade attribution, and a day-by-day audit trail of everything the
strategy did and why.

It is a single Go binary. No accounts, no signup, no telemetry, no cloud service.
Bring your own model API key and run it on your laptop.

```
natural-quant run "buy $100 of the biggest company by market cap every day,
                   and sell when that company is no longer number one"
```

```
Daily Biggest Company Accumulator
2020-01-01 to 2024-12-31   1258 trading days   universe of 43

  Starting capital          $100,000.00
  Final value               $116,862.64
  Total return                   16.86%
  Annualised (CAGR)               3.17%
  Sharpe ratio                     0.65
  Max drawdown                   -6.94%
  Trades                             18

  Comparison               Total return   Max drawdown
  Daily Biggest Company          16.86%         -6.94%
  SPDR S&P 500 ETF               94.58%        -33.72%
```

---

## Quick start (60 seconds)

You need [Go 1.24+](https://go.dev/dl/) and one model API key.

```bash
git clone https://github.com/charbelkassab/natural-quant
cd natural-quant
go build -o natural-quant ./cmd/natural-quant

export OPENAI_API_KEY=sk-...        # or CEREBRAS_API_KEY, or KIMI_API_KEY
./natural-quant serve
```

Open <http://127.0.0.1:8080>, type a strategy, press **Backtest it**.

**No API key? No problem.** Everything except the plain-English compiler works
offline, on deterministic synthetic data:

```bash
./natural-quant serve --offline
```

Check your setup any time:

```bash
./natural-quant doctor
```

---

## What you can ask for

These all work today. Each was compiled from the sentence shown and run against
real market data as part of the project's [prompt corpus](internal/strategy/testdata/corpus.json).

| Ask | What it does |
| --- | --- |
| *"Buy $100 of the biggest company by market cap every day, sell when it is no longer number one."* | Ranks a mega-cap universe by point-in-time market cap daily |
| *"Buy SPY when the 50 day crosses above the 200 day, sell on the reverse cross, 12% trailing stop."* | Golden cross with a trailing exit |
| *"Every month hold the 3 best performing big tech stocks over the last 6 months."* | Momentum rotation, monthly rebalance |
| *"Buy any mega cap whose RSI drops below 30 at 10% of the portfolio, sell above 55, 8% stop."* | Mean reversion with position sizing and risk exits |
| *"Hold 60% SPY and 40% AGG, rebalanced quarterly."* | Classic fixed allocation |
| *"Trade KO against PEP when their price ratio is 2 standard deviations from its 60 day average."* | Market-neutral pairs trade with shorting |
| *"Hold SPY normally but move to cash for a month whenever the VIX closes above 30."* | Volatility regime filter |
| *"Each month hold the 2 strongest S&P 500 sector ETFs by 3 month momentum."* | Sector rotation |
| *"Hold SPY from November through April and cash from May through October."* | Seasonality |
| *"Once a week read the news about Apple, ask the AI if the tone is positive, hold AAPL if so."* | Calls a model **inside** the backtest |
| *"Invest $500 into VTI on the first trading day of every month and never sell."* | Dollar-cost averaging |
| *"Something that beats the market but isn't too risky."* | Vague prompts get a concrete strategy plus its stated assumptions |

The strategy that gets built is never a black box. Every run shows you the
generated code, the assumptions the model made where your wording was ambiguous,
and the limitations it could not model.

---

## The chart

The comparison view is the point of the tool. Everything on the chart is a
**view** — a strategy, or any ticker, index or ETF — and each one has its own
colour, name, and show/hide toggle that you control from the panel on the
right. Rename them, recolour them, delete them, or edit a strategy's prompt and
re-run it in place.

- **Overlay anything.** Strategies, individual stocks, indices, ETFs, crypto —
  as many at once as you like, normalised to percentage return so a $100,000
  portfolio and a $300 share price are honestly comparable.
- **Runs cover all available history by default**, back to a symbol's first
  print — 1993 for SPY, 1980 for AAPL. Use **Custom…** on the chart for a
  specific window such as 2002 to 2003: symbols refetch and strategies replay
  from the code they were already compiled to, so changing the period costs no
  model call and takes about a second.
- **Returns rebase to what you are looking at.** Zoom to 1Y and every series
  restarts at zero from the first visible bar, and the metrics table recomputes
  over that same window. Measuring a zoomed view against a baseline scrolled
  off the left edge is how comparison charts mislead people, so this one does
  not do it.
- **Trade markers** on the equity curve, with entry and exit arrows.
- **Click any day** to open the full audit trail for that session: what was bought
  and sold and *why*, every open position with its weight and unrealised P&L, the
  strategy's own log lines, and — if the strategy consulted a model — the exact
  prompt it sent and the answer it got back.

  ![The day-detail panel showing a trade, its reason, and the resulting holdings](docs/images/screenshot-day.png)

- **Drawdown subchart**, synchronised to the main time axis.
- Range presets, log scale, crosshair with a live legend.

Built on TradingView's [Lightweight Charts](https://github.com/tradingview/lightweight-charts),
vendored locally so the app has zero external runtime dependencies.

---

## How it works

```
  your sentence
       │
       ▼
  ┌─────────────────┐   strategy API reference is fed to the model verbatim
  │ compiler        │   → JSON: code, universe, benchmarks, warm-up,
  │ (quality tier)  │           assumptions, limitations
  └────────┬────────┘
           │  compile-check, then a real smoke backtest
           │  failures are fed back for repair, up to 3 attempts
           ▼
  ┌─────────────────┐   goja: pure-Go JS sandbox, no filesystem,
  │ strategy (JS)   │   no network, no timers — only the ctx object
  └────────┬────────┘
           │  ctx.ai() / ctx.news() ─→ fast tier, cached per (day, prompt)
           ▼
  ┌─────────────────┐   daily event loop, orders fill at the NEXT open
  │ backtest engine │   commissions, slippage, short borrow, corporate actions
  └────────┬────────┘
           ▼
     equity curve · metrics · per-day audit trail
```

**Orders fill at the next session's open, not today's close.** This is the default
and it matters: filling at a close the strategy has already observed is lookahead
bias, and it silently inflates returns. You can switch to close fills for
comparison, and the interface labels it as optimistic when you do.

The generated code runs in [goja](https://github.com/dop251/goja), a pure-Go
JavaScript interpreter. It ships with no filesystem, network, process or timer
access. The only path to the outside world is `ctx.ai()`, `ctx.web()` and
`ctx.news()` — all counted, capped, and recorded per day.

---

## Choosing an AI provider

natural-quant speaks the OpenAI chat-completions protocol, so OpenAI, Cerebras and
Moonshot (Kimi) all work through one client. There are two very different jobs, and
they want different models:

| Job | How often | Wants | Default |
| --- | --- | --- | --- |
| **Compiling** your sentence into code | Once per strategy | Correctness — being wrong costs far more than being slow | `openai / gpt-5.5` |
| **In-strategy `ctx.ai()`** calls | Once per simulated day — hundreds or thousands of times | Speed and price | `cerebras / gpt-oss-120b` |

That split is the whole recommendation. Compilation is a single call where quality
dominates. In-strategy calls are a tight loop where a 3000 token/sec model at low
cost is the only thing that makes an AI-driven backtest practical.

Kimi sits in the middle and is an excellent, cheaper substitute for compilation —
`kimi-k2.7-code-highspeed` is code-tuned and fast.

Set whichever keys you have; the router uses what is available and degrades
gracefully when a tier is missing.

```bash
export OPENAI_API_KEY=sk-...
export CEREBRAS_API_KEY=csk-...
export KIMI_API_KEY=sk-...          # or MOONSHOT_API_KEY

# Override any routing decision
export NQ_ROUTE_QUALITY=kimi
export NQ_ROUTE_FAST=cerebras
export NQ_CEREBRAS_MODEL=gpt-oss-120b
```

`natural-quant doctor` lists exactly which models each of your keys can reach.

### AI calls are cached, which changes the economics

Every `ctx.ai()` reply is cached on disk keyed by *(simulated day, prompt)*. A
weekly AI strategy over four years makes ~200 model calls on its first run and
**zero** on every run after. That is what makes it affordable to iterate on an
AI-driven strategy, and it also makes those backtests exactly reproducible.

---

## Limitations, please read

A backtesting tool that oversells itself is worse than useless. Here is what
natural-quant genuinely cannot tell you.

**Survivorship bias.** The built-in symbol lists contain companies that matter
*today*. A 2015 backtest picking from "mega caps" is choosing from a list we now
know went on to succeed. Real-time you would have been choosing from a different
list containing names that later failed.

**Market cap data is approximate.** No free, keyless API serves point-in-time
shares outstanding — Yahoo's fundamentals endpoints now return HTTP 401. So
natural-quant ships a curated, piecewise-constant share-count table
([`internal/market/assets/shares_outstanding.csv`](internal/market/assets/shares_outstanding.csv))
covering mega caps. It is accurate enough to rank the largest companies against
each other, which is what "the biggest company" needs. It is not accounting-grade,
and accuracy decays the further back you test. Corrections with a citation are the
single most valuable contribution you can make to this project.

**AI and web strategies have lookahead bias by construction.** `ctx.web()` and
`ctx.news()` query the internet *as it is now*, not as it was on the simulated day.
A 2019 backtest that reads the web is being handed 2026 information. `ctx.ai()` has
a milder version of the same problem — the model knows what happened next. Treat
these runs as demonstrations of a mechanism, never as evidence that an idea works.

**Not modelled:** taxes, intraday prices and stops that trigger between bars,
options and futures, dividends as cash (they are reinvested via adjusted closes),
market impact for large orders, borrow availability for shorts, delistings and
spin-offs.

**Modelled:** commissions, slippage (5 bps by default, not zero), short borrow
cost, splits and dividends, cash drag, next-open fills.

And the ordinary one: past performance says very little about future returns, an
overfitted backtest says nothing at all, and it is easy to produce an overfitted
backtest by trying prompts until one looks good.

---

## The strategy API

The model writes against a documented API — the same document you can read:

```bash
natural-quant api          # or click "Strategy API" in the web interface
```

A strategy is two functions:

```js
function setup(ctx) {
  ctx.universe("megacap");     // or ["AAPL", "MSFT", ...]
  ctx.warmup(200);             // bars of history needed before trading
}

function onDay(ctx) {
  const top = ctx.biggestCompany();
  if (!top) return;
  for (const sym of ctx.heldSymbols()) {
    if (sym !== top) ctx.close(sym, "no longer the largest company");
  }
  if (!ctx.hasPosition(top)) ctx.buy(top, 100, "largest company by market cap");
}
```

`ctx` gives you prices and history, ~15 indicators, market-cap ranking, portfolio
state, order placement by shares / dollars / target weight, stop-loss, take-profit
and trailing stops, calendar helpers, persistent state, and the AI and web hooks.
Full reference: [`internal/strategy/assets/api.md`](internal/strategy/assets/api.md).

You can edit the generated code in the **Code** tab and re-run it directly — the
compiler is a starting point, not a cage.

---

## Command line

```bash
natural-quant serve [--addr host:port] [--offline] [--open] [--dev ./web]
natural-quant run "<strategy>" [--from 2015-01-01] [--to 2024-12-31]
                               [--cash 100000] [--benchmark SPY,QQQ]
                               [--universe tech] [--code] [--json]
natural-quant doctor           # check data, providers, caches
natural-quant api              # print the strategy API reference
natural-quant cache clear [--ai]
```

## Configuration

Everything has a sensible default. Override with environment variables or
`$NQ_DATA_DIR/config.json`.

| Variable | Default | Purpose |
| --- | --- | --- |
| `OPENAI_API_KEY` / `CEREBRAS_API_KEY` / `KIMI_API_KEY` | — | Model access |
| `NQ_ADDR` | `127.0.0.1:8080` | Listen address |
| `NQ_DATA_DIR` | `~/.natural-quant` | Cache and saved runs |
| `NQ_ROUTE_QUALITY` / `NQ_ROUTE_BALANCED` / `NQ_ROUTE_FAST` | `openai` / `kimi` / `cerebras` | Tier routing |
| `NQ_<PROVIDER>_MODEL` | see above | Per-provider model override |
| `NQ_MAX_AI_CALLS` | `2000` | Per-run budget for `ai()` + `web()` |
| `NQ_OFFLINE` | `false` | Synthetic data, no network |
| `NQ_SEARCH_PROVIDER` | `duckduckgo` | `duckduckgo` or `none` |

Market data comes from Yahoo Finance's public chart endpoint (no key required).
See [docs/data-sources.md](docs/data-sources.md) for how to plug in a paid vendor.

---

## Development

```bash
go test ./...                       # unit tests, no network or keys needed
go build -o natural-quant ./cmd/natural-quant
./natural-quant serve --dev ./web   # live-edit the front end without rebuilding
```

The front end is embedded with `go:embed`, so a normal build bakes in `web/`.
Pass `--dev ./web` to serve it from disk while working on it.

### The prompt corpus

The project's real regression suite is a corpus of natural-language prompts that
must compile *and* run. It costs API calls, so it is opt-in:

```bash
NQ_LIVE_TESTS=1 go test ./internal/strategy/ -run TestPromptCorpus -v -timeout 60m
NQ_CORPUS_FILTER=momentum NQ_LIVE_TESTS=1 go test ./internal/strategy/ -run TestPromptCorpus -v
```

**If natural-quant cannot handle a strategy you care about, the most useful thing
you can do is add it to [`internal/strategy/testdata/corpus.json`](internal/strategy/testdata/corpus.json)
and open an issue.** That corpus is how the API grows — two real bugs in order
handling were found by it on its first run.

See [CONTRIBUTING.md](CONTRIBUTING.md).

---

## Licence

MIT. See [LICENSE](LICENSE) and [NOTICE](NOTICE) for third-party components.

natural-quant is a research and education tool. It is not investment advice, it is
not a broker, and it will not place a real order. Nothing here is a recommendation
to buy or sell anything.
