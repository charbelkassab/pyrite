# Example strategies

Hand-written strategies in the natural-quant JavaScript API. They serve three
purposes: something to read when learning the API, a starting point to edit, and
a check that the API can express these ideas cleanly without a model in the loop.

## Running one

Paste the file into the **Code** tab in the web interface and press *Re-run
edited code*, or post it directly:

```bash
curl -X POST http://127.0.0.1:8080/api/runs \
  -H 'Content-Type: application/json' \
  -d "$(jq -n --arg code "$(cat examples/golden-cross.js)" \
        '{code:$code, name:"Golden cross", universe:["SPY"], warmup:220,
          start:"2015-01-01", end:"2024-12-31", benchmarks:["SPY"]}')"
```

`universe` and `warmup` must be supplied when running code directly — those come
from the compiler's plan when you use a plain-English prompt.

## The files

| File | Idea | Notes |
| --- | --- | --- |
| `biggest-company.js` | Own whichever US company is largest by market cap | The flagship example |
| `golden-cross.js` | 50/200 SMA crossover with a trailing stop | Shows crossing detection via `ctx.state` |
| `momentum-rotation.js` | Monthly rotation into the strongest names | Shows `ctx.rank` and `ctx.equalWeight` |
| `mean-reversion.js` | Buy oversold, exit on recovery | Shows per-symbol loops and risk exits |
| `sixty-forty.js` | 60/40 stocks and bonds, quarterly rebalance | Shows `ctx.rebalance` |
| `pairs-trade.js` | Long/short KO against PEP on their price ratio | Requires `allow_short` |
| `news-sentiment.js` | Weekly news read by a model | Read the lookahead warning first |

## Writing your own

The full API reference is `natural-quant api`, or the **Strategy API** button in
the interface. Two rules cover most mistakes:

1. **Guard every indicator.** They return `null` until enough history exists.
2. **Set `warmup` high enough.** A 200-day average needs at least 200 bars before
   the strategy can do anything.
