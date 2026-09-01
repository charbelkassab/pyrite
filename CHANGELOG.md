# Changelog

Notable changes. Dates are the date of the change, not of a release.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and versions follow [semantic versioning](https://semver.org/) once there is a
release to version.

## Unreleased

Everything below is the first public shape of the project. Until `v0.1.0` is
tagged there is no compatibility promise: the strategy API, the JSON API and
the CLI flags may all move.

### The tool decides whether to believe itself

- **A critique on every result.** Detects lookahead fills, frictionless
  high-turnover runs, samples too small to mean anything, returns concentrated
  in a handful of sessions, short-volatility return shapes, survivorship in the
  symbol list, in-loop model calls, benchmark dominance and late exits. Computed
  in Go rather than asked of a model, so it costs nothing, needs no key, and
  cannot invent a number.
- **A self-test for the critic.** `pyrite selftest` runs the critique against a
  corpus of strategies built to be caught — two round trips in ten years, close
  fills, frictionless churn, a return carried by six sessions, a short
  volatility payoff, an empty `onDay`, a universe of today's winners, a
  benchmark that wins on return and drawdown, an unadjusted split — each paired
  with the finding it must produce, plus one sound control that must come back
  with nothing critical against it. Two of the cases also run a contrast with
  the defect removed, so a critic that fired regardless would fail. Synthetic
  and hand-built bars throughout, so it runs offline in `go test ./...`, and
  exits 1 if a defect goes unreported.
- **Parameter search.** `pyrite sweep` runs the whole declared space across a
  worker pool — 1,064 combinations over nine years in 4.6 seconds — and reports
  a heatmap, the deflated Sharpe, the probability of backtest overfitting by
  combinatorially symmetric cross-validation, a plateau ratio and a verdict.
- **Walk-forward.** `pyrite walkforward` chooses parameters on rolling training
  windows and reports on the windows that follow, with purging and an embargo
  defaulting to the strategy's warm-up.
- **Guided search with a blind holdout.** `pyrite improve` lets a model propose,
  measure and re-propose under a budget, while the harness withholds the test
  period. The `Candidate` type has no out-of-sample field, so a proposer cannot
  read what the struct does not hold.
- **A research ledger that survives the session.** Deflated Sharpe and PBO
  correct for the trials in one search; forty sweeps over the same symbols and
  period across three weeks are thousands of trials that every one of those
  statistics ignored. `pyrite ledger` records each run and sweep against a
  dataset key — sorted symbols or index name, start, end, bar size — and
  reports the cumulative trial count, the strategy versions tried, and the
  score the best of that many tries reaches by luck alone. Runs and sweeps say
  so when the history has outgrown them. `PYRITE_NO_LEDGER=1` turns it off.
- **Paper-forward mode.** Walk-forward, the blind holdout and the ledger all
  constrain how a past sample is used, and a determined person defeats all
  three by iterating. `pyrite forward record` writes down the book a strategy
  wants to hold when the market next opens, before the prices exist; `pyrite
  forward score` measures those records once their sessions have happened, and
  reports realised return per decision, hit rate, mean and a t-statistic — with
  a verdict that says plainly how little a handful of sessions proves, and how
  many more it would take to reach `|t| = 2` at the mean and spread seen so
  far. Each record is sealed into a hash chain, so an entry cannot be altered
  or removed afterwards without `pyrite forward verify` naming the index of the
  first record that no longer adds up; scoring refuses to run over a chain that
  does not verify. Recording twice in one day is a no-op, and a second,
  differing decision for the same day is refused rather than merged. `--as-of`
  exists for backfilling a missed session and for testing the loop: anything
  written after its outcome already existed is marked as a backfill and kept
  out of the forward figures.
- **Research reports.** `pyrite report` runs the whole battery and writes one
  Markdown document, verdict first.
- **Shareable HTML reports.** `pyrite report --html out.html` renders the same
  document as one self-contained page: the equity curve with the benchmark
  overlaid, the drawdown beneath it on the same x axis, and the calendar years
  as bars, all drawn as inline SVG in Go. No scripts, no fonts, no charting
  library, no network requests at all, light and dark, and a print stylesheet
  so it saves to PDF. `--out` and `--html` work together or separately.
- **Cost sensitivity.** `--cost-scan` re-runs at 0, 5, 20 and 50 bps and
  interpolates the break-even.
- **Factor exposure.** `--factors` regresses the strategy's excess returns on
  market, size, value, momentum and quality, and reports the residual alpha
  with its t-statistic. Newey-West standard errors, because daily strategy
  returns are autocorrelated and the naive t-statistic is too generous. The
  factors are ETF spreads (SPY, IWM, IWD/IWF, MTUM, USMV) rather than the
  academic series, which is stated in the output rather than buried here. A
  proxy with no data over the period is dropped and named. `pyrite report`
  carries the same analysis as its own section.
- **Reality check and SPA.** `pyrite sweep` runs White's Reality Check and
  Hansen's Superior Predictive Ability test over every trial's return series at
  once, stationary-bootstrapped, so the search is judged as the search it was
  rather than as one strategy. Both p-values are reported: they differ by the
  recentring Hansen added, and the gap between them is a measure of how much
  dead weight the sweep carried.
- **Null strategy distribution.** `pyrite sweep`, and `pyrite run
  --null-strategy`, place a strategy against a thousand random ones matched to
  it on trade count, on the distribution of holding periods and — the part that
  decides whether the comparison means anything — on exposure. Only the entry
  timing is randomised; the prices are untouched.
- **A weak draw in the bootstrap.** The block bootstrap picked its starting
  points with a modulus, which reads a linear congruential generator's weaker
  low bits. The effect was small — measured against the known answer for an
  iid bootstrap, it tracked the theoretical resample variance to within half a
  percent at most series lengths and ran about 2.5% low at the short end — but
  it ran in the flattering direction, because a resample that covers its
  series more evenly than chance understates variance and so understates every
  p-value drawn from it. Now drawn from the high bits. Numbers `Bootstrap`
  reports move in the third decimal place.

### Bar sizes

- **Intraday.** `--interval` runs a backtest on 1m, 5m, 15m, 30m, 1h, 1wk or
  1mo bars. Every annualised statistic scales with the bar size: a Sharpe
  computed on 1-minute bars and annualised as daily is out by about twentyfold,
  in the flattering direction, so the annualisation factor travels as a typed
  `Scale` rather than two swappable float64 parameters.
- **Multiple timeframes.** `ctx.resampledCloses(sym, "1d", n)` gives a daily
  view from inside an intraday run, including only what had happened by the
  simulated moment.
- Vendor history limits are clamped and reported rather than silently returning
  a shorter series than asked for.

### Honest data

- **Point-in-time S&P 500 membership.** `ctx.universe("sp500")` resolves per
  simulated day from 881 recorded tenures, 379 of them ended. A 2022 backtest
  can pick Silicon Valley Bank and take the loss; it cannot pick Tesla before
  December 2020. Rebuild with `pyrite ingest index`.
- **Share counts from SEC filings.** `pyrite ingest edgar` builds the
  market-cap table from XBRL company facts — 8,473 rows across 290 symbols,
  each citing its accession number, dated by filing rather than by cover date.
- **Point-in-time news.** `ctx.news()` queries an article index with an
  explicit publication-date window ending at the simulated day, so a strategy
  standing on 4 March 2019 sees what had been published by then and nothing
  after it. There is no fallback to a live feed when the window is empty:
  substituting today's internet would reintroduce exactly the bias this
  removes, invisibly. `PYRITE_NEWS_PROVIDER` selects `gdelt` (the default),
  `live` or `none`. `ctx.web()` remains a look at today's internet and still
  raises a critical finding.
- **Economic series with publication lag.** `ctx.fred()` reads St. Louis Fed
  data as of the simulated day, accounting for how late each figure was
  actually published.
- **A provider chain.** Yahoo with Stooq behind it, retrying only the symbols
  that failed, plus a local CSV directory — the only source that can hold
  delisted securities.

### Analysis

- Round-trip trades with maximum adverse and favourable excursion, an edge
  ratio and a give-back figure.
- Omega, ulcer, Martin, VaR and CVaR, skew, kurtosis, tail ratio, gain-to-pain,
  Kelly, equity-curve R², up and down capture, and rolling Sharpe, volatility
  and beta.
- Attribution by year, month, month-of-year, market regime and holding, plus
  drop-the-best-month and drop-the-best-five-days stress tests.
- **`pyrite scenarios`.** Thirteen named historical windows — Black Monday
  through the 2023 regional bank failures — each run on its own, with the
  indicator warm-up and a year of trading loaded in front of it so the strategy
  enters the window already positioned rather than in cash. Reports return,
  drawdown, the benchmark over the same dates and the excess, beside the S&P
  500's own decline in that window as fixed reference data. Windows the data
  cannot reach are listed with the reason rather than dropped, and losing
  materially more than the benchmark in one of them is a finding. Also a
  section in `pyrite report`.
- Portfolio construction: minimum variance, maximum Sharpe, risk parity,
  hierarchical risk parity and inverse volatility, with Ledoit–Wolf shrinkage.
- Market impact under the square-root law. The `daily-reversal` example
  returns -15.1% on $100,000 and -99.6% on $1bn.
- **A capacity ladder.** `run --capacity` re-runs the strategy at $100k, $1m,
  $10m, $100m and $1bn with impact enabled and reports the size at which the
  edge disappears, interpolated between rungs and against the benchmark as well
  as against zero. A strategy small enough that impact never reaches it is told
  so plainly rather than given an invented threshold.
- **Signal decay.** `run --decay` reports the average round trip's cumulative
  return 1, 2, 3, 5, 10, 20 and 40 bars after entry, where that curve peaks, and
  whether it is still rising at the average holding period. A peak long before
  the exit raises a finding: the entries are working and the exit rule is
  handing back what they found.
- Around 35 indicators.
- A reproducibility manifest on every run: data vendor, per-symbol coverage,
  code hash, cost model, seed, and which models answered.

### An agent can drive it

- **`pyrite mcp`.** A Model Context Protocol server on stdio, so Claude and
  other agents can call pyrite as a tool: `strategy_api`, `list_examples`,
  `backtest`, `sweep` and `walkforward`. Every result that came from a backtest
  carries the critique, the trust score and the verdict alongside the numbers,
  because an agent left alone with a backtester will otherwise try variations
  until one looks good. JSON-RPC 2.0 implemented against the specification with
  the standard library; no new dependency.

### Getting started

- **Works with no API key.** Ollama and LM Studio are detected automatically;
  bundled strategies run with nothing installed at all.
- `pyrite examples`, `run --example`, `sweep --example`. Eight bundled
  strategies, including `daily-reversal`, which exists to be run with
  `--cost-scan` and lose all 327% of its gross return to the spread.
- A Python client (`pip install pyrite-quant`), a Dockerfile, and prebuilt
  binaries.

### Fixed

- **The same backtest returned three different numbers.** `ctx.rebalance()`
  and `ctx.equalWeight()` queued orders by ranging over a Go map, whose
  iteration order is randomised. Invisible while there is cash for every
  order and decisive when there is not, because the engine reduces whichever
  orders arrive last: one example returned 17.16%, 16.21% and 16.35% on three
  consecutive runs over identical data. Single-symbol strategies were never
  affected. The `--param` path had the same bug in its grid merge.
- **A strategy could write files outside its sandbox.** The response cache
  used its key directly as a file path, and `websearch` builds that key from
  the search query — which comes from strategy code. `ctx.news()` or
  `ctx.web()` with a traversal in the query wrote a JSON file wherever it
  landed. Strategies are given no filesystem access on purpose, since the
  whole design runs generated code; the cache was quietly handing one back.
  Keys are now hashed before they become paths, which also fixes a panic on
  keys shorter than two characters. Existing cached entries are invalidated
  and simply repopulate.
- **`sweep`, `walkforward`, `improve` and `report` all rejected a working
  strategy passed with `--code-file`.** Each probes the spec before running
  it, and each loaded market data before calling `setup()` — so the universe
  was still empty when they checked it, and a strategy that names its symbols
  in `setup()`, which is the only way to write one without `--universe`,
  failed with "empty universe: nothing to trade". `run` had been fixed for
  this; its three siblings had not.
- A run that never placed an order scored 65/100 — above a real result with
  two flaws — because the score subtracts a fixed penalty per finding and an
  empty run trips fewer of them. "Never traded" now floors the score at zero.
- A year that returned exactly zero was counted as a losing year, so a
  strategy that never traded was told most of its years lost money.
- The walk-forward verdict printed a bare ratio ("efficiency -0.01") beside a
  table reporting the same quantity as "-0.78%", which read as two different
  measurements.
- A NaN in a score field truncated entire API responses: a 200 with an empty
  body and no error anywhere.
- One `--offline` run poisoned the market cache with synthetic prices, which
  every later real backtest then read back believing they were the market.
- `ctx.universe([...])` and `ctx.warmup(n)` were documented as things `setup()`
  sets, and both were inert.
- A calendar flag consumed its own memo on first read, so a lifecycle hook
  asking the same question earlier in the session made `onDay` see the wrong
  answer.
- The market-data cache, the parameter heatmap, the FRED client and the EDGAR
  ingester each carried a defect found only by running them against the real
  thing. See the commit log.
