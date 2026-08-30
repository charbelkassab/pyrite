# What pyrite cannot tell you

Every backtesting tool flatters its results. This page is the honest accounting
of how, so that you can discount what you see by roughly the right amount.

## What a parameter search does and does not tell you

`pyrite sweep` exists because a single backtest is one point in a space,
and the number it reports is a joint fact about the idea and the particular
parameters someone typed. Searching that space is the only way to separate the
two — and it introduces its own failure mode, which the tool reports rather than
hides.

**Searching harder makes the best result look better even when nothing is
there.** Try enough configurations and one will look excellent by chance. That
is what "expected best from luck alone" measures: the score the top of *N*
trials reaches with no skill at all, given the spread the search actually
produced. A best below that line is not evidence of anything, and the verdict
says so in those words.

**The deflated Sharpe is not the Sharpe.** It corrects for the number of trials
and for the skew and fat tails of the winner's own returns. A strategy that
sells tail risk scores well on the naive measure and poorly here, which is the
entire point of the correction.

**PBO is measured, not assumed.** Combinatorially symmetric cross-validation
splits the same period many ways and asks how often the in-sample winner lands
below median out of sample. Around 0.5 is a coin flip, which is what pure
overfitting looks like. It is computed only when the per-trial return series fit
inside the memory budget; when they do not, the field is `null` rather than
zero, because a zero would read as "no overfitting at all" — the most flattering
possible answer, arrived at by having computed nothing.

**Walk-forward is honest about selection, not about the data.** Choosing
parameters on one window and reporting on the next removes the search from the
reported number. It does not remove survivorship bias from the universe, or
lookahead from `ctx.web()`, or any of the other problems on this page. A strategy
with excellent walk-forward efficiency over a survivorship-biased universe is
still being flattered.

## Biases that are present

### Survivorship bias

The built-in universes (`megacap`, `tech`, `faang`, `dow`, `sectors`,
`us-large`) list the companies that matter **today**. They are not
point-in-time index membership.

A backtest starting in 2013 that "holds the top 5 mega caps" is choosing from a
list that already knows NVIDIA became enormous and that Nokia and Sears did not.
Real-time, you would have been picking from a list containing names that went on
to fail, and your results would have been worse — often much worse.

**Use `sp500` instead.** That one universe is resolved per simulated day from
recorded index membership, not from today's list:

```bash
pyrite run "each month hold the 20 strongest S&P 500 names" --universe sp500
```

The bundled table
([`internal/market/assets/sp500_membership.csv`](../internal/market/assets/sp500_membership.csv))
holds 881 tenures across 502 current and 379 former constituents, reconstructed
from Wikipedia's current-components list and its 407 recorded add/remove events
by undoing each change in reverse from today. Rebuild it with:

```bash
pyrite ingest index --index sp500
```

What that changes in practice: a 2022 run can select Silicon Valley Bank,
Signature Bank and First Republic and take the losses they produced, because
all three were genuinely in the index then. A survivorship-biased universe
contains none of them, so the strategy never takes those losses. In the other
direction, Tesla is invisible before December 2020 and Nvidia before November
2001, so a momentum strategy cannot quietly select them years early.

**What is still wrong:**

- Wikipedia is citable and checkable but not authoritative, and the change log
  thins out the further back you go. Before its reach, the universe falls back
  to the earliest constituents on record and the run says so.
- **Membership is only half the problem.** Backtesting a dropped name also needs
  its prices, and free vendors do not serve delisted securities — a symbol that
  stopped trading resolves to a per-symbol data error rather than a position.
  Point `PYRITE_CSV_DIR` at your own data for those; it is the one source that can
  hold them.
- Only the S&P 500 has a table so far.

For the other universes the mitigation is unchanged: pin the symbol list
yourself to names that existed and were plausible choices at the start of your
window.

### Market capitalisation, now from filings

Ranking by market cap needs shares outstanding *as of the historical date*.
Yahoo's `quoteSummary` and `v7/quote` endpoints return HTTP 401, but the SEC's
XBRL company-facts API at `data.sec.gov` serves the full disclosure history per
company, free and without a key.

The bundled table at `internal/market/assets/shares_outstanding.csv` is
generated from it — 8,473 rows across 290 symbols, each citing the accession
number of the filing it came from. Rebuild or extend it with:

```bash
pyrite ingest edgar --universe megacap \
    --user-agent "Your Name you@example.com"
```

**Rows are dated by publication, not measurement.** A share count on a 31 March
cover page did not become knowable until the 10-Q appeared in May. Dating rows
by the measurement date would hand every historical backtest several weeks of
free information — a small lookahead bias, but a real one, and avoidable. The
`as_of` column preserves the measurement date.

What is still wrong:

- Dates before a symbol's first filing extrapolate backwards from the earliest
  row. Accuracy decays the further back you test.
- Consecutive filings within 0.5% of each other are dropped to keep the table
  small. Splits and buyback programmes survive; quarterly drift does not, and is
  not material to a ranking.
- Multi-class filers such as META report against a share-class axis, and the
  XBRL API does not serve dimensional facts. Their counts come from a weighted
  period average, named in the `tag` column and flagged in the file header. An
  approximation that ranks a mega cap correctly against its peers beats dropping
  the symbol from every ranking.
- ETFs, indices and crypto have no share count and are absent by design.

`ctx.marketCap()` returns `null` for symbols with no data, and ranking functions
skip them rather than guessing.

**Corrections with a citation are still welcome.** See
[CONTRIBUTING.md](../CONTRIBUTING.md).

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

pyrite does not prevent this — it cannot. What it does instead:

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
  small token budget produces an empty reply. pyrite applies a floor and
  retries with a larger budget, but a strategy that overrides `maxTokens` down
  to single digits is still asking for trouble.
- There is a per-run cap on model and web calls (`PYRITE_MAX_AI_CALLS`, 2000 by
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
