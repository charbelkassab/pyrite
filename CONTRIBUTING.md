# Contributing to pyrite

Thanks for looking. This is a small, focused project and contributions are very
welcome.

## The most valuable contributions

In rough order of how much they help:

### 1. A prompt that does not work

This is the big one. pyrite's job is to turn *any* reasonable trading idea
into a working strategy, and the only way to know where it falls short is for
people to try things it has not seen.

If you describe a strategy and get bad code, a crash, or a backtest that does not
match what you asked for:

1. Add the prompt to [`internal/strategy/testdata/corpus.json`](internal/strategy/testdata/corpus.json).
2. Open an issue with the prompt, what you expected, and what happened.

```json
{
  "id": "short-name-for-it",
  "family": "momentum",
  "prompt": "The exact sentence you typed.",
  "expect": { "trades": true, "allow_short": true }
}
```

The corpus is the project's real regression suite. Its first run found two
genuine bugs in order handling — a bare `ctx.cover()` that bought with all cash
instead of closing the short, and a reason string in the size position that
silently discarded the order. Both are now permanent tests.

Run it with:

```bash
PYRITE_LIVE_TESTS=1 go test ./internal/strategy/ -run TestPromptCorpus -v -timeout 60m

# just one family or case
PYRITE_CORPUS_FILTER=pairs PYRITE_LIVE_TESTS=1 go test ./internal/strategy/ -run TestPromptCorpus -v
```

It uses real API calls, so it is opt-in and never runs in normal `go test ./...`.

### 2. Corrections to the share-count table

[`internal/market/assets/shares_outstanding.csv`](internal/market/assets/shares_outstanding.csv)
is approximate reference data, and its accuracy directly determines whether
"the biggest company by market cap" is right on any given day.

If you can correct a figure **with a citation** — a 10-K cover page, a 10-Q, or an
official investor-relations page — that is a direct, permanent improvement to
every market-cap strategy. Please include the source in the pull request.

Adding a company that has plausibly been top-20 by market cap and is missing is
equally welcome.

### 3. Additions to the strategy API

If a strategy family cannot be expressed at all — not just awkwardly, but
genuinely not — the API needs a new function. Open an issue describing the
strategies it would unlock before writing code, so we can keep the surface
coherent rather than accumulating one-offs.

### 4. Data providers

`market.Provider` is a three-method interface. An implementation for a vendor
people actually subscribe to (Tiingo, Polygon, FMP, Alpaca) would be genuinely
useful. See [docs/data-sources.md](docs/data-sources.md).

## Getting set up

```bash
git clone https://github.com/charbelkassab/pyrite
cd pyrite
go test ./...                          # no network or API keys needed
go build -o pyrite ./cmd/pyrite
./pyrite serve --dev ./web      # live front-end editing
```

The front end is embedded with `go:embed`. In a normal build, editing `web/app.js`
does nothing until you rebuild — `--dev ./web` serves it from disk instead. This
catches people out; it caught the author out.

## House style

**Go.** Standard `gofmt`. Run `go vet ./...` before opening a PR. Comments explain
*why*, not *what* — if a line needs a comment to say what it does, rename
something instead. Errors get context (`fmt.Errorf("load fundamentals: %w", err)`)
and error strings read as sentences a user could act on.

**Tests.** Every behavioural fix gets a regression test. Tests must pass with no
network and no API keys — use `market.NewSyntheticProvider()` or the `fixedProvider`
helper in `engine_test.go` for hand-built price series.

**Front end.** Vanilla JavaScript, no build step, no framework, no runtime
dependencies. This is deliberate: the whole app must stay a single binary you can
run offline. If you need a library, vendor it under `web/vendor/` and add it to
`NOTICE`.

**Colour.** Series colours come from a fixed, validated categorical palette in
`styles.css`. Slots are claimed per entity and never cycled, so adding or removing
a comparison never repaints the others. If you change the palette, keep it
colour-blind safe — the eight slots were chosen to clear a CVD separation
threshold on the dark surface.

## Things to be careful about

**Lookahead bias.** The engine fills orders at the *next* open for a reason. Any
change that lets a strategy see or trade on information from its own day or later
is a correctness bug, not a feature. If you add a `ctx` accessor, ask what the
strategy could learn from it that it should not know yet.

**Honest defaults.** Slippage defaults to 5 bps rather than zero, benchmarks are
added automatically, and warnings surface in the interface. Please do not make the
tool quieter about its own uncertainty — the value of a backtester is entirely in
whether you can trust it.

**Cost.** `ctx.ai()` runs once per simulated day. Anything that increases per-day
model calls multiplies a user's bill by the length of their backtest. Cache
aggressively and keep the per-run budget enforced.

## Pull requests

Small and focused beats large and sweeping. Describe what changed and why, and
mention anything you deliberately left out. If it changes behaviour a user could
notice, update the relevant doc in the same PR.

By contributing you agree your work is licensed under the MIT License.
