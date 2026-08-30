<div align="center">

# pyrite

**Most backtests are fool's gold. This one tells you which.**

pyrite is a backtester that spends as much effort trying to disprove your
strategy as it does running it. Describe an idea in plain English, and get back
an equity curve *and* the specific reasons not to believe it.

[![CI](https://github.com/charbelkassab/pyrite/actions/workflows/ci.yml/badge.svg)](https://github.com/charbelkassab/pyrite/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/charbelkassab/pyrite.svg)](https://pkg.go.dev/github.com/charbelkassab/pyrite)
[![Go Report Card](https://goreportcard.com/badge/github.com/charbelkassab/pyrite)](https://goreportcard.com/report/github.com/charbelkassab/pyrite)
[![Release](https://img.shields.io/github/v/release/charbelkassab/pyrite?sort=semver)](https://github.com/charbelkassab/pyrite/releases)
[![License: MIT](https://img.shields.io/badge/licence-MIT-blue.svg)](LICENSE)

[Try it in 30 seconds](#try-it-in-30-seconds) ·
[Install](#install) ·
[Why](#why-this-exists) ·
[Searching](#one-backtest-is-not-evidence) ·
[Improving](#make-this-better-without-the-usual-trap) ·
[Python](#from-python) ·
[Limitations](#limitations-please-read)

![pyrite comparing a trend-following strategy against the S&P 500](docs/images/screenshot-chart.png)

</div>

---

## Try it in 30 seconds

No API key. No account. No config file. No network, if you like.

```bash
go install github.com/charbelkassab/pyrite/cmd/pyrite@latest

pyrite run --example golden-cross
```

```
Classic 50/200 moving average crossover with a trailing stop.

2018-01-02 to 2023-12-29   1509 trading days   universe of 1

  Total return                   74.49%
  Annualised (CAGR)               9.74%
  Sharpe ratio                     0.92
  Max drawdown                  -16.07%

  Comparison               Total return   Max drawdown
  Classic 50/200 moving          74.49%        -16.07%
  State Street SPDR S&P…         95.54%        -33.72%

How much should you believe this?  50/100

  STOP too few trades to mean anything
        2 closed round trips. A win rate or a Sharpe over this many trades
        is noise: one different outcome moves every statistic here
        materially.

  STOP this is short volatility in disguise
        Returns are left-skewed (-0.74) with fat tails (excess kurtosis
        5.0): many small gains and occasional large losses. Sharpe flatters
        this shape badly, because the risk it measures is not the risk
        being taken.
```

That last part is the product. Every other backtester stops at the Sharpe ratio.

```bash
pyrite examples                  # seven bundled strategies, all runnable
pyrite serve --offline --open    # the web app, on synthetic data
pyrite doctor                    # what works right now, and how to fix the rest
```

---

## Why this exists

Backtesting is the easiest way in finance to fool yourself, and every tool
makes it easier. Search enough parameters and something will look excellent by
chance. Pick from today's index and you have quietly excluded every company
that failed. Charge no commission and a strategy that trades daily looks free.
None of this shows up in the equity curve, which is the one thing every
backtester puts on screen.

pyrite computes the things that would tell you:

| It measures | So you find out |
| --- | --- |
| **Deflated Sharpe** | whether the Sharpe survives the number of strategies you tried to find it |
| **Probability of backtest overfitting** | how often the in-sample winner lands below median out of sample |
| **Walk-forward efficiency** | how much of the improvement survives on data the search never saw |
| **Plateau ratio** | whether the winner sits on a ridge or is a lone spike |
| **Cost sensitivity** | the slippage at which the edge disappears |
| **Block bootstrap** | the drawdown to plan around, not the one that happened |
| **Point-in-time membership** | what the index actually held that day, failures included |
| **Market impact** | what your size costs, under the square-root law |

And then it says so in a sentence, on every run, without being asked.

It is a single Go binary. No accounts, no signup, no telemetry, no cloud
service, no database. It runs on your laptop and writes to `~/.pyrite`.

---

## Install

**Prebuilt binary** — [latest release](https://github.com/charbelkassab/pyrite/releases/latest),
for Linux, macOS and Windows. Verify against `SHA256SUMS`.

**Go** (needs [Go 1.25+](https://go.dev/dl/)):

```bash
go install github.com/charbelkassab/pyrite/cmd/pyrite@latest
```

**Docker**:

```bash
docker run --rm -p 8080:8080 ghcr.io/charbelkassab/pyrite serve --addr 0.0.0.0:8080 --offline
# or: docker compose up
```

**From source**:

```bash
git clone https://github.com/charbelkassab/pyrite && cd pyrite
make build && ./pyrite run --example golden-cross
```

### Turning on plain English

Everything above works with no model. Compiling a *sentence* into a strategy
needs one, and there are two ways to have one:

```bash
# Free, on your machine. pyrite finds it automatically.
ollama pull qwen2.5-coder:7b

# Or hosted, if you want the better output.
export OPENAI_API_KEY=sk-...     # or CEREBRAS_API_KEY, or KIMI_API_KEY
```

Then:

```bash
pyrite run "buy $100 of the biggest company by market cap every day,
            and sell when that company is no longer number one"
```

`pyrite doctor` tells you which of these it can see, and what to do if the
answer is none.

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
| *"Hold equities only while the yield curve is not inverted."* | Reads real economic data, with publication lag applied |
| *"Each month hold the 2 strongest S&P 500 sector ETFs by 3 month momentum."* | Sector rotation |
| *"Every month hold the 20 strongest S&P 500 stocks over the last 6 months."* | Selects from **point-in-time** index membership, not today's list |
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

## One backtest is not evidence

A single backtest tells you how one configuration did over one sample. It
cannot tell you whether the idea works or whether that particular number
happened to fit. So pyrite makes the second question first-class.

**Every number is a parameter.** The compiler declares them rather than
hardcoding them — "the 50 day average" becomes a default of 50 and a grid
around it:

```js
function setup(ctx) {
  ctx.param("fast", 50,  { grid: [20, 35, 50, 65, 80] });
  ctx.param("slow", 200, { grid: [100, 150, 200, 250] });
}
```

**Search the space, not the point.**

```
pyrite sweep "buy SPY when the fast average crosses above the slow one"
```

```
fast across, slow down, shaded by sharpe

  200 │  *  *  =  =  =
  150 │  #  =  :  -  -
  100 │  #  @  .  :
   50 │  .  #  #  +  :
      └───────────────
         5 10 20 40 60

How much of this is real?
  Best sharpe                             0.749
  Median sharpe                           0.543
  Expected best from luck alone           0.213
  Neighbour support                      75.83%
  Prob. of backtest overfitting          85.71%   (70 splits)
  Deflated Sharpe                        94.22%

  best sharpe 0.75 against 0.21 expected from luck alone over 20 trials; the
  winner's neighbours average 76% of its score — some support, but the peak is
  doing real work; probability of backtest overfitting is 86% — selecting on
  this sample carries no information about the next one
```

A heatmap is the fastest overfitting detector ever built. One bright cell in a
dark field is a fluke; a broad warm region is an edge. The statistics put
numbers on what the eye already sees:

- **Expected best from luck alone** — what the top of *N* trials scores with no
  skill at all. A best below it is not evidence of anything.
- **Deflated Sharpe** — the Sharpe corrected for how many strategies you tried,
  plus the skew and fat tails of the winner's own returns.
- **Probability of backtest overfitting** — across many train/test splits of
  the same period, how often the in-sample winner lands below median out of
  sample. 50% is a coin flip, which is what pure overfitting looks like.
- **Neighbour support** — how the cells beside the winner scored.

**Choose on one period, report on another.**

```
pyrite walkforward "..." --train 504 --test 126
```

Parameters are picked on each training window and applied untouched to the
window that follows, with an embargo between them so a 200-day indicator
cannot leak across the boundary. The stitched curve is the only equity line in
the tool that was never fitted to.

```
  Mean in-sample return                  28.84%
  Mean out-of-sample return               5.45%
  Walk-forward efficiency                18.91%
  Positive test windows                   15 / 20

  15 of 20 test windows finished positive; out-of-sample captured only 19% of
  in-sample return, so most of the backtest was the search finding the sample
  rather than an edge
```

Runs in parallel across your cores, sharing one copy of the price data — 400
backtests in under half a second on a laptop.

---

## "Make this better", without the usual trap

```
pyrite improve "a golden cross on SPY" --budget 8
```

A model proposes a variant, the harness backtests it, the model reads the
result and proposes again. Under a fixed budget, it converges on something
better than it started with.

That loop is also an excellent way to build a strategy that fits one sample
perfectly and has no edge whatever — which is why the harness, not the model,
owns the data:

- The period is split. The model is shown results from the **training window
  only**, and every candidate is run over that window alone.
- The `Candidate` type it receives has no out-of-sample field. It cannot read
  what the struct does not hold.
- The holdout is touched **once**, at the end, after the search has closed, to
  score the winner that training data already chose.

```
Searched 2018-01-02 to 2022-03-04. Held back 2022-03-07 to 2023-12-29.
The holdout was not visible during the search and was scored once, at the end.

The winner, on data the search never saw
                                training        holdout
  Total return                     84.10%         11.62%
  Annualised (CAGR)                16.02%          6.71%
  Sharpe ratio                       1.31           0.54
  Surviving fraction                          41.88%
```

The model is also handed the critique of each attempt, so it can act on a
stated fault — "only 12 closed trades", "the returns are short volatility in
disguise" — rather than guess at what to change. And it is told to stop when it
has nothing worth trying, which is a legitimate answer and a better one than
proposing noise.

---

## Every result criticises itself

A backtesting tool that oversells itself is worse than useless, so each run
comes back with the paragraph a good quant would write about it:

```
How much should you believe this?  20/100

  STOP too few trades to mean anything
        12 closed round trips. A win rate or a Sharpe over this many trades is
        noise: one different outcome moves every statistic here materially.

  STOP this is short volatility in disguise
        Returns are left-skewed (-1.33) with fat tails (excess kurtosis 11.5):
        many small gains and occasional large losses. Sharpe flatters this
        shape badly, because the risk it measures is not the risk being taken.

  warn the return is concentrated in a few sessions
        50% of the total gain disappears when excluding the 5 best days.
```

These findings are computed, not asked of a model. They cost nothing, work
with no API key, and cannot invent a number. It detects lookahead fills,
frictionless high-turnover runs, samples too small to mean anything, returns
concentrated in a handful of sessions, short-volatility return shapes,
survivorship in the symbol list, in-loop model calls, and exits that give back
what the entries found.

The full report also breaks the result down **by year, by market regime**
(calm, normal, high volatility, bear), **by holding**, and by what is left
when the best month or the best five days are removed.

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

pyrite speaks the OpenAI chat-completions protocol, so OpenAI, Cerebras and
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
export PYRITE_ROUTE_QUALITY=kimi
export PYRITE_ROUTE_FAST=cerebras
export PYRITE_CEREBRAS_MODEL=gpt-oss-120b
```

`pyrite doctor` lists exactly which models each of your keys can reach.

### AI calls are cached, which changes the economics

Every `ctx.ai()` reply is cached on disk keyed by *(simulated day, prompt)*. A
weekly AI strategy over four years makes ~200 model calls on its first run and
**zero** on every run after. That is what makes it affordable to iterate on an
AI-driven strategy, and it also makes those backtests exactly reproducible.

---

## Limitations, please read

A backtesting tool that oversells itself is worse than useless. Here is what
pyrite genuinely cannot tell you.

**Survivorship bias.** The built-in symbol lists contain companies that matter
*today*. A 2015 backtest picking from "mega caps" is choosing from a list we now
know went on to succeed. Real-time you would have been choosing from a different
list containing names that later failed.

**Market cap data comes from filings, and is still imperfect.** Ranking by
market cap needs shares outstanding *as of the historical date*. Yahoo's
fundamentals endpoints return HTTP 401, but the SEC's XBRL company-facts API
serves the full disclosure history per company, free and without a key. So the
bundled table
([`internal/market/assets/shares_outstanding.csv`](internal/market/assets/shares_outstanding.csv))
is generated from real filings — 8,473 rows across 290 symbols, each citing the
accession number it came from. Rebuild or extend it yourself:

```bash
pyrite ingest edgar --universe megacap \
    --user-agent "Your Name you@example.com"
```

Rows are dated by when the filing was **published**, not by the date the count
was measured: a share count on a 31 March cover page was not knowable until the
10-Q appeared in May, and dating it the other way hands every historical
backtest several weeks of free information.

What is still wrong: dates before a symbol's first filing extrapolate backwards
from the earliest row; consecutive filings within 0.5% of each other are dropped
to keep the table small; and multi-class filers such as META report against a
share-class axis the SEC's API does not expose, so their counts come from a
weighted period average and are flagged as approximate in the file.

**AI and web strategies have lookahead bias by construction.** `ctx.web()` and
`ctx.news()` query the internet *as it is now*, not as it was on the simulated day.
A 2019 backtest that reads the web is being handed 2026 information. `ctx.ai()` has
a milder version of the same problem — the model knows what happened next. Treat
these runs as demonstrations of a mechanism, never as evidence that an idea works.

**Not modelled:** taxes, intraday prices and stops that trigger between bars,
options and futures, dividends as cash (they are reinvested via adjusted closes),
market impact for large orders, borrow availability for shorts, delistings and
spin-offs.

**Survivorship bias is fixed for the S&P 500, and only for it.** The universe
name `sp500` resolves per simulated day from recorded index membership, so a
2022 backtest can pick Silicon Valley Bank and take the loss it really produced,
and cannot pick Tesla before it joined in December 2020. The bundled table holds
881 tenures across 502 current and 379 former constituents, rebuilt with
`pyrite ingest index`.

The other universes (`megacap`, `tech`, `dow`, …) are still today's companies,
and the remaining half of the problem is prices: free vendors do not serve
delisted securities, so a dropped name resolves to a data error rather than a
position unless you supply its history through `PYRITE_CSV_DIR`. See
[docs/limitations.md](docs/limitations.md).

**Modelled:** commissions, slippage (5 bps by default, not zero), short borrow
cost, splits and dividends, cash drag, next-open fills, and — with `--impact 1`
— market impact under the square-root law, so a large order pays for the
liquidity it demands.

The last one changes results more than anything else on this page. The same
high-turnover strategy over the same period returns **-10% on $100,000 and
-72% on $1bn**, purely because the second one has to move the market to get
filled. Without it, position size is free and every strategy looks infinitely
scalable.

And the ordinary one: past performance says very little about future returns, an
overfitted backtest says nothing at all, and it is easy to produce an overfitted
backtest by trying prompts until one looks good.

---

## The strategy API

The model writes against a documented API — the same document you can read:

```bash
pyrite api          # or click "Strategy API" in the web interface
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

`ctx` gives you prices and history, ~35 indicators, market-cap ranking, portfolio
state, order placement by shares / dollars / target weight, stop-loss, take-profit
and trailing stops, economic series from the St. Louis Fed, portfolio construction (`ctx.optimize` — minimum variance,
maximum Sharpe, risk parity, hierarchical risk parity, with Ledoit–Wolf
shrinkage), declared parameters, calendar helpers, persistent state, and the AI
and web hooks.
Full reference: [`internal/strategy/assets/api.md`](internal/strategy/assets/api.md).

You can edit the generated code in the **Code** tab and re-run it directly — the
compiler is a starting point, not a cage.

---

## Command line

```bash
pyrite serve [--addr host:port] [--offline] [--open] [--dev ./web]
pyrite run "<strategy>" [--from 2015-01-01] [--to 2024-12-31]
                               [--cash 100000] [--benchmark SPY,QQQ]
                               [--universe tech] [--code] [--json]
                               [--code-file strategy.js]

pyrite sweep "<strategy>"        # search the parameter space
                               [--param fast=10,20,50] [--objective sharpe]
                               [--top 20] [--csv out.csv] [--max-combos 5000]
pyrite walkforward "<strategy>"  # optimise in-sample, report out
                               [--train 504] [--test 126] [--embargo 200]
                               [--anchored]
pyrite improve "<strategy>"      # guided search against a blind holdout
                               [--budget 6] [--holdout 0.3] [--goal "..."]
pyrite report "<strategy>"       # the full battery, as one document
                               [--out report.md] [--no-sweep] [--no-walkforward]

pyrite ingest edgar --universe megacap --user-agent "You you@example.com"
pyrite doctor           # check data, providers, caches
pyrite api              # print the strategy API reference
pyrite cache clear [--ai]
```

`--code-file` runs a strategy you already have, skipping the compiler. Every
command accepts it, so you can iterate on generated code without paying for a
model call each time.

## Configuration

Everything has a sensible default. Override with environment variables or
`$PYRITE_DATA_DIR/config.json`.

| Variable | Default | Purpose |
| --- | --- | --- |
| `OPENAI_API_KEY` / `CEREBRAS_API_KEY` / `KIMI_API_KEY` | — | Model access |
| `PYRITE_ADDR` | `127.0.0.1:8080` | Listen address |
| `PYRITE_DATA_DIR` | `~/.pyrite` | Cache and saved runs |
| `PYRITE_ROUTE_QUALITY` / `PYRITE_ROUTE_BALANCED` / `PYRITE_ROUTE_FAST` | `openai` / `kimi` / `cerebras` | Tier routing |
| `PYRITE_<PROVIDER>_MODEL` | see above | Per-provider model override |
| `PYRITE_MAX_AI_CALLS` | `2000` | Per-run budget for `ai()` + `web()` |
| `PYRITE_OFFLINE` | `false` | Synthetic data, no network |
| `PYRITE_SEARCH_PROVIDER` | `duckduckgo` | `duckduckgo` or `none` |
| `PYRITE_DATA_PROVIDERS` | `yahoo,stooq` | ordered fallback chain for market data |
| `PYRITE_CSV_DIR` | — | a directory of `SYMBOL.csv` files, tried before any vendor |

Market data comes from Yahoo Finance's public chart endpoint (no key required),
with Stooq behind it. Free endpoints fail in a particular way — they work for
most symbols and quietly 401 on a few — so the chain retries **only the symbols
that failed** with the next vendor, rather than dropping those names from the
universe and silently changing the backtest.

Point `PYRITE_CSV_DIR` at a directory of `SYMBOL.csv` files to use your own data.
The parser accepts what vendors actually emit — mixed-case headers, `Adj Close`
or `adjclose`, ISO or US or unix dates — and falls back to the raw close when
there is no adjusted column. This is also the only way to backtest **delisted
securities**, which no free live endpoint serves.

See [docs/data-sources.md](docs/data-sources.md) for more.

---

## The whole thing as a document

```
pyrite report "a golden cross on SPY" --out report.md
```

Runs the backtest, the parameter search, the walk-forward, the cost scan and a
block bootstrap, then writes one Markdown document: verdict first, then the
results against the benchmark, the out-of-sample evidence, the robustness
statistics, where the return came from, what survives friction, the
distribution of outcomes the same process could have produced, the specific
objections, the provenance, and the code.

Every number in it is computed. With a model key the document also opens with
a written summary; without one it is still complete, because the prose is the
only part a model contributes.

---

## From Python

```python
from pyrite import Client

with Client.serve(offline=True) as nq:
    run = nq.backtest(code=strategy, universe=["SPY"], start="2015-01-01")
    run.curve.plot()
    print(run.trades[["symbol", "net_pnl", "mae_pct", "mfe_pct"]])
    for f in run.critique:
        print(f["severity"], f["title"])

    sw = nq.sweep(code=strategy, universe=["SPY"])
    print(sw.surface("fast", "slow"))
```

`pip install pyrite`. It is a client, not a reimplementation — the Go
binary does the work, so the notebook and the CLI can never disagree about what
a backtest means. pandas is optional: tables come back as DataFrames when it is
installed and as lists of dicts when it is not. See [python/](python/).

---

## Development

```bash
go test ./...                       # unit tests, no network or keys needed
go build -o pyrite ./cmd/pyrite
./pyrite serve --dev ./web   # live-edit the front end without rebuilding
```

The front end is embedded with `go:embed`, so a normal build bakes in `web/`.
Pass `--dev ./web` to serve it from disk while working on it.

### The prompt corpus

The project's real regression suite is a corpus of natural-language prompts that
must compile *and* run. It costs API calls, so it is opt-in:

```bash
PYRITE_LIVE_TESTS=1 go test ./internal/strategy/ -run TestPromptCorpus -v -timeout 60m
PYRITE_CORPUS_FILTER=momentum PYRITE_LIVE_TESTS=1 go test ./internal/strategy/ -run TestPromptCorpus -v
```

**If pyrite cannot handle a strategy you care about, the most useful thing
you can do is add it to [`internal/strategy/testdata/corpus.json`](internal/strategy/testdata/corpus.json)
and open an issue.** That corpus is how the API grows — two real bugs in order
handling were found by it on its first run.

See [CONTRIBUTING.md](CONTRIBUTING.md).

---

## Licence

MIT. See [LICENSE](LICENSE) and [NOTICE](NOTICE) for third-party components.

pyrite is a research and education tool. It is not investment advice, it is
not a broker, and it will not place a real order. Nothing here is a recommendation
to buy or sell anything.
