# Driving pyrite from an agent

```bash
pyrite mcp
```

serves the [Model Context Protocol](https://modelcontextprotocol.io) over
stdio, so Claude — or any other MCP client — can write a strategy, run it, read
what is wrong with it, and try again.

The direction is the point. Everywhere else in this tool a model is a component
pyrite calls, to turn English into JavaScript. Here pyrite is the tool and the
model is the caller, and the reason that is worth doing is the critique: an
agent left alone with a backtester will try variations until one looks good,
which is the exact failure this project was built to measure. Every result that
came from a backtest carries its objections, its trust score and its verdict in
the same payload as the numbers. There is no call that returns one without the
other.

## Configuration

**Claude Code**, one command:

```bash
claude mcp add pyrite -- /usr/local/bin/pyrite mcp
```

Or check a `.mcp.json` into the repository so everyone working on it gets the
same server:

```json
{
  "mcpServers": {
    "pyrite": {
      "command": "/usr/local/bin/pyrite",
      "args": ["mcp"],
      "env": {
        "PYRITE_DATA_DIR": "~/.pyrite"
      }
    }
  }
}
```

**Claude Desktop**, in `claude_desktop_config.json` — on macOS
`~/Library/Application Support/Claude/claude_desktop_config.json`, on Windows
`%APPDATA%\Claude\claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "pyrite": {
      "command": "/usr/local/bin/pyrite",
      "args": ["mcp"]
    }
  }
}
```

Use an absolute path to the binary: the launcher does not run a login shell, so
whatever is on your `PATH` in a terminal is not necessarily on its.

To try it against synthetic data, with no network and no keys, add `--offline`
to `args`. Prices are then deterministic pseudo-random series rather than the
market, and every result says so in its `data` block.

No API key is needed. A key compiles plain English into a strategy, and an
agent calling this server writes the JavaScript itself.

## The tools

| Tool | What it does |
| --- | --- |
| `strategy_api` | The reference for the JavaScript a strategy is written in. Read this first. |
| `list_examples` | The strategies bundled in the binary. Pass a `name` to get one back with its source. |
| `backtest` | One run over a symbol list and a period. Returns the headline metrics, the trust score and the critique. |
| `sweep` | Every combination of the declared parameters, ranked, with the robustness block. |
| `walkforward` | Parameters chosen on one window and applied to the next. Returns out-of-sample efficiency and the verdict. |

`backtest`, `sweep` and `walkforward` share their inputs: `code` (or `example`),
`universe`, `benchmarks`, `start`, `end`, `initial_cash`, `interval`, `fill`,
`slippage_bps`, `commission_pct`, `impact`, `allow_short`, `warmup`,
`risk_free_rate` and `params`. `sweep` adds `grids`, `objective`, `max_combos`
and `top`; `walkforward` adds `grids`, `objective`, `train_days`, `test_days`,
`embargo` and `anchored`.

Arguments are read strictly: an unknown field is an error rather than something
quietly ignored. A misspelling that is dropped in silence produces a run over
the wrong period, or with the wrong costs, and nothing anywhere says so.

## What comes back

Each result arrives twice, as MCP requires: once as `structuredContent` for a
client that can read it, and once serialised into a text block for one that
cannot.

A `backtest` result:

```json
{
  "name": "golden cross",
  "universe": ["SPY"],
  "start": "2015-01-02",
  "end": "2023-12-29",
  "data": { "provider": "yahoo" },
  "trust_score": 45,
  "headline": "the return is concentrated in a few sessions",
  "critique": [
    {
      "severity": "critical",
      "title": "the return is concentrated in a few sessions",
      "detail": "81% of the total gain disappears when excluding the 5 best days. ..."
    }
  ],
  "metrics": { "total_return": 0.24, "cagr": 0.024, "sharpe": 0.24, "max_drawdown": -0.29, "...": "..." },
  "benchmarks": [{ "label": "SPY", "total_return": 1.09, "sharpe": 0.58 }]
}
```

`sweep` returns the top rows, the winning combination run in full with its own
critique, and the robustness block — deflated Sharpe, the probability of
backtest overfitting, and whether the winner sits on a plateau or alone on a
spike — with a plain-English `verdict` over the lot. `walkforward` returns the
per-fold results, the stitched out-of-sample metrics, the efficiency and its
verdict.

Every metric that can legitimately be undefined arrives as `null` rather than
as a number: a Sortino with no losing days, a profit factor with no losing
trades, a score for a combination that failed. Reporting those as zero would
read as the worst possible result when it is closer to the best.

## Failures

A malformed message, an unknown method or a bad argument comes back as a
JSON-RPC error — `-32700`, `-32601`, `-32602`. A strategy that threw, or a
symbol with no data, does not: that is a real answer to the question the agent
asked, so it arrives as a tool result with `isError` set and an explanation the
agent can act on. Most clients surface a protocol error as a broken connection,
which is the wrong response to a strategy with a typo in it.

## Notes

- stdout carries protocol frames and nothing else. Anything diagnostic goes to
  stderr, which is where Claude Desktop and Claude Code keep a server's log.
- Requests are served one at a time, in order. A backtest is a long job and the
  caller is waiting for it either way.
- A strategy that calls a model or the web inside the backtest cannot be swept
  or walked forward: a search would multiply those calls by the number of
  combinations. Run it once with `backtest`.
- The server is implemented against the specification using the standard
  library alone, at protocol revision `2025-06-18`. pyrite has one dependency
  and this was not worth a second.
