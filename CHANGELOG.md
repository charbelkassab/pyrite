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
- **Research reports.** `pyrite report` runs the whole battery and writes one
  Markdown document, verdict first.
- **Cost sensitivity.** `--cost-scan` re-runs at 0, 5, 20 and 50 bps and
  interpolates the break-even.

### Honest data

- **Point-in-time S&P 500 membership.** `ctx.universe("sp500")` resolves per
  simulated day from 881 recorded tenures, 379 of them ended. A 2022 backtest
  can pick Silicon Valley Bank and take the loss; it cannot pick Tesla before
  December 2020. Rebuild with `pyrite ingest index`.
- **Share counts from SEC filings.** `pyrite ingest edgar` builds the
  market-cap table from XBRL company facts — 8,473 rows across 290 symbols,
  each citing its accession number, dated by filing rather than by cover date.
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
- Portfolio construction: minimum variance, maximum Sharpe, risk parity,
  hierarchical risk parity and inverse volatility, with Ledoit–Wolf shrinkage.
- Market impact under the square-root law. The same strategy returns -10% on
  $100,000 and -72% on $1bn.
- Around 35 indicators.
- A reproducibility manifest on every run: data vendor, per-symbol coverage,
  code hash, cost model, seed, and which models answered.

### Getting started

- **Works with no API key.** Ollama and LM Studio are detected automatically;
  bundled strategies run with nothing installed at all.
- `pyrite examples`, `run --example`, `sweep --example`.
- A Python client (`pip install pyrite`), a Dockerfile, and prebuilt binaries.

### Fixed

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
