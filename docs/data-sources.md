# Data sources

pyrite is designed to run with no API keys for market data, so a clone
works immediately. This page explains what it uses, why, and how to substitute
something better.

## Prices — Yahoo Finance chart endpoint

**Endpoint:** `https://query1.finance.yahoo.com/v8/finance/chart/{symbol}`
**Key required:** none
**Provides:** daily OHLCV, split- and dividend-adjusted closes, dividend events

This is the default and it covers US equities, ETFs, indices (`^GSPC`, `^IXIC`,
`^DJI`, `^VIX`), futures (`GC=F`, `CL=F`) and crypto (`BTC-USD`).

It is an undocumented endpoint. The parser is deliberately defensive: missing
fields fall back rather than failing, holiday padding is dropped, and each bar's
date is derived from the exchange's own UTC offset rather than the server's
timezone. Requests retry with backoff on 429 and 5xx.

### Why not Stooq?

Stooq was the obvious keyless alternative and is now behind a JavaScript
proof-of-work challenge, so it cannot be used from a plain HTTP client.

### Caching

Every fetched series is cached to `$PYRITE_DATA_DIR/market-cache/` as JSON, one file
per symbol. The store always fetches a wider window than requested, so repeated
backtests over overlapping periods hit the cache. Clear it with:

```bash
pyrite cache clear
```

## Fundamentals — bundled share counts

Market-cap ranking needs point-in-time shares outstanding. Yahoo's fundamentals
endpoints (`quoteSummary`, `v7/quote`) now return **HTTP 401**, and no other free
keyless source provides a historical series.

pyrite therefore ships
[`internal/market/assets/shares_outstanding.csv`](../internal/market/assets/shares_outstanding.csv):
a piecewise-constant table of share counts for US large caps, with step changes at
splits and gradual drift from buybacks.

```
symbol,effective_date,shares_outstanding,name
AAPL,2020-08-31,17100000000,Apple Inc.
AAPL,2021-01-01,16800000000,Apple Inc.
```

A row takes effect on its date and holds until the next row for that symbol.
Market cap is `raw close × shares outstanding` — the **raw**, unadjusted close,
because share counts are stated in actual shares and pairing them with a
split-adjusted price would understate every company that has ever split.

Read the header of that file and [limitations.md](limitations.md) for accuracy
caveats. They are significant.

### Replacing it with your own data

Drop a file at `$PYRITE_DATA_DIR/shares_outstanding.csv` in the same format. It
replaces the bundled table wholesale rather than merging, so results stay
explainable. Comment lines beginning with `#` are allowed.

```bash
mkdir -p ~/.pyrite
cp my-fundamentals.csv ~/.pyrite/shares_outstanding.csv
pyrite doctor      # "fundamentals" will show your file's path
```

### Using a commercial vendor

If you have a Tiingo, FMP, Polygon or Sharadar subscription, the cleanest
integration point is `market.Provider`:

```go
type Provider interface {
    Name() string
    Fetch(ctx context.Context, symbol string, from, to Day) (*Series, error)
    Search(ctx context.Context, query string) ([]Quote, error)
}
```

Implement it, then swap the constructor in `internal/app/app.go`. The caching
store, engine and interface need no changes. For fundamentals, export your
vendor's share counts to the CSV format above on a schedule — the resolution
that matters is quarterly.

## News and web search — DuckDuckGo and Yahoo RSS

**`ctx.news(symbol)`** reads Yahoo Finance's RSS headline feed for that ticker,
falling back to a general search for non-ticker topics.
**`ctx.web(query)`** posts to DuckDuckGo's `lite` endpoint and parses the results.

Neither needs a key. Both are rate-limited to a few requests per second and
cached in memory per query for the lifetime of a run.

**These return the internet as it is now, not as it was on the simulated day.**
That is lookahead bias and it is unavoidable with any live search backend. See
[limitations.md](limitations.md#lookahead-bias-in-ai-and-web-strategies).

Disable search entirely with:

```bash
export PYRITE_SEARCH_PROVIDER=none
```

## Offline and synthetic data

`--offline` swaps in a synthetic provider that generates deterministic price
histories from a hash of each ticker — geometric Brownian motion with a
per-symbol drift, volatility and slow cycle, on a plausible trading calendar.

This is what the unit tests run against, and it means the whole application can
be developed, demoed and tested with no network and no keys. Numbers produced in
offline mode are meaningless as research; they are for exercising the machinery.

```bash
pyrite serve --offline
go test ./...              # always offline
```
