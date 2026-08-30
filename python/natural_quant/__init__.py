"""natural-quant: a Python client for the natural-quant backtester.

The Go binary does the work. This package is a thin client over its JSON API,
so nothing about the engine is reimplemented here and the two can never
disagree about what a backtest means.

    from natural_quant import Client

    with Client.serve(offline=True) as nq:          # starts a local server
        run = nq.backtest(code=open("strategy.js").read(), universe=["SPY"])
        print(run.metrics["total_return"])
        print(run.curve)                            # DataFrame, or list of dicts
        for f in run.critique:
            print(f["severity"], f["title"])

pandas is optional. Tabular results come back as DataFrames when it is
installed and as lists of dicts when it is not, so the package is usable in a
plain interpreter and better in a notebook.
"""

from .client import (
    Client,
    NaturalQuantError,
    RunFailed,
    ServerUnavailable,
)
from .results import AgentRun, Backtest, Sweep, WalkForward

__all__ = [
    "Client",
    "NaturalQuantError",
    "RunFailed",
    "ServerUnavailable",
    "Backtest",
    "Sweep",
    "WalkForward",
    "AgentRun",
]

__version__ = "0.1.0"
