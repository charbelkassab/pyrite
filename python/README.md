# natural-quant (Python)

A client for [natural-quant](https://github.com/charbelkassab/natural-quant).
The Go binary does the work; this package turns its JSON into Python objects,
so a notebook drives the same engine as the CLI and the web interface and the
two can never disagree about what a backtest means.

```bash
pip install natural-quant          # the client
# and the binary itself:
go install github.com/charbelkassab/natural-quant/cmd/natural-quant@latest
```

## Usage

```python
from natural_quant import Client

with Client.serve(offline=True) as nq:          # starts and stops a server
    run = nq.backtest(code=open("strategy.js").read(),
                      universe=["SPY"], start="2015-01-01")

    print(run.summary())
    run.curve.plot()                            # DataFrame, if pandas is installed
    print(run.trades[["symbol", "net_pnl", "mae_pct", "mfe_pct"]])
    print(run.by_year)

    for f in run.critique:                      # what is wrong with this result
        print(f"[{f['severity']}] {f['title']}: {f['detail']}")
```

Already running a server? `Client("http://127.0.0.1:8080")`.

### Searching

```python
sw = nq.sweep(code=strategy, universe=["SPY"], objective="sharpe")

print(sw.rows.sort_values("score", ascending=False).head())
print(sw.surface("fast", "slow"))               # a grid, ready for a heatmap
print(sw.robustness["pbo"])                     # probability of overfitting
print(sw.verdict)
```

### Choosing on one period, reporting on another

```python
wf = nq.walkforward(code=strategy, universe=["SPY"],
                    train_days=504, test_days=126)

wf.curve.plot()          # the only equity curve here that was never fitted to
print(wf.efficiency)     # out-of-sample return over in-sample return
print(wf.verdict)
```

## pandas is optional

Tabular results come back as DataFrames when pandas is installed and as lists
of dicts when it is not. Nothing in the package requires it, so it is usable
from a plain interpreter and better in a notebook.

```python
from natural_quant.frames import has_pandas
```

## Everything is still there

Each result object keeps the untouched response on `.raw`, so anything this
package has not thought to surface is one attribute away:

```python
run.raw["attribution"]["by_month_of_year"]
sw.raw["robustness"]["deflated_sharpe"]
```

## Tests

They start a real server in offline mode rather than mocking one, because a
mock would happily agree with a wrong assumption about what the API returns.

```bash
go build -o /tmp/natural-quant ./cmd/natural-quant
NQ_BINARY=/tmp/natural-quant python3 python/tests/test_client.py
```
