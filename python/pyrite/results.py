"""Typed views over what the API returns.

Each class wraps the raw JSON and exposes the parts worth reading, without
hiding the rest: `.raw` is always the untouched response, so anything this
package has not thought to surface is still one attribute away.
"""

from __future__ import annotations

from typing import Any, Dict, List, Optional

from .frames import frame, series


class _Result:
    """Common behaviour: keep the raw payload, expose it, print usefully."""

    def __init__(self, raw: dict):
        self.raw = raw

    def __getitem__(self, key: str) -> Any:
        return self.raw[key]

    def get(self, key: str, default: Any = None) -> Any:
        return self.raw.get(key, default)


class Backtest(_Result):
    """One completed backtest."""

    def __init__(self, raw: dict, run: Optional[dict] = None):
        super().__init__(raw)
        self.run = run or {}

    @property
    def id(self) -> str:
        return self.run.get("id", "")

    @property
    def name(self) -> str:
        plan = self.run.get("plan") or {}
        return plan.get("name") or self.run.get("label") or ""

    @property
    def code(self) -> str:
        """The JavaScript that actually ran."""
        return (self.run.get("plan") or {}).get("code", "")

    @property
    def metrics(self) -> Any:
        return series(self.raw.get("metrics") or {})

    @property
    def risk(self) -> Any:
        """Distribution shape, drawdown persistence and capture ratios."""
        return series(self.raw.get("risk") or {})

    @property
    def curve(self) -> Any:
        """The daily equity curve."""
        return frame(self.raw.get("curve") or [], index="date")

    @property
    def trades(self) -> Any:
        """Round trips, with maximum adverse and favourable excursion."""
        return frame(self.raw.get("trades") or [], index=None)

    @property
    def trade_stats(self) -> Any:
        return series(self.raw.get("trade_stats") or {})

    @property
    def fills(self) -> Any:
        return frame(self.raw.get("fills") or [], index=None)

    @property
    def rolling(self) -> Any:
        """Trailing-window Sharpe, volatility and beta."""
        return frame(self.raw.get("rolling") or [], index="date")

    @property
    def by_year(self) -> Any:
        return frame((self.raw.get("attribution") or {}).get("by_year") or [], index="label")

    @property
    def by_regime(self) -> Any:
        return frame((self.raw.get("attribution") or {}).get("by_regime") or [], index="label")

    @property
    def by_symbol(self) -> Any:
        return frame((self.raw.get("attribution") or {}).get("by_symbol") or [], index="symbol")

    @property
    def stress(self) -> Any:
        """What the result looks like with its best episodes removed."""
        return frame((self.raw.get("attribution") or {}).get("stress") or [], index="label")

    @property
    def critique(self) -> List[dict]:
        """What is wrong with this result, most severe first."""
        return (self.raw.get("critique") or {}).get("findings") or []

    @property
    def trust_score(self) -> int:
        return (self.raw.get("critique") or {}).get("trust_score", 0)

    @property
    def manifest(self) -> Dict[str, Any]:
        """Provenance: data vendor, code hash, cost model, seed."""
        return self.raw.get("manifest") or {}

    @property
    def benchmarks(self) -> List[dict]:
        return self.raw.get("benchmarks") or []

    def benchmark_curve(self, symbol: Optional[str] = None) -> Any:
        """The equity curve of a benchmark, for plotting beside the strategy."""
        for b in self.benchmarks:
            if symbol is None or b.get("symbol") == symbol:
                return frame(b.get("curve") or [], index="date")
        return frame([], index="date")

    def summary(self) -> str:
        m = self.raw.get("metrics") or {}
        lines = [
            f"{self.name or 'strategy'}",
            f"  total return   {_pct(m.get('total_return'))}",
            f"  CAGR           {_pct(m.get('cagr'))}",
            f"  Sharpe         {_num(m.get('sharpe'))}",
            f"  max drawdown   {_pct(m.get('max_drawdown'))}",
            f"  trust score    {self.trust_score}/100",
        ]
        for f in self.critique[:3]:
            lines.append(f"  [{f.get('severity')}] {f.get('title')}")
        return "\n".join(lines)

    def __repr__(self) -> str:
        m = self.raw.get("metrics") or {}
        return (f"<Backtest {self.name!r} return={_pct(m.get('total_return'))} "
                f"sharpe={_num(m.get('sharpe'))} trust={self.trust_score}/100>")


class Sweep(_Result):
    """A parameter search."""

    @property
    def rows(self) -> Any:
        """Every combination, one row each."""
        out = []
        for r in self.raw.get("rows") or []:
            row = dict(r.get("params") or {})
            row.update({k: v for k, v in r.items() if k != "params"})
            out.append(row)
        return frame(out, index=None)

    @property
    def axes(self) -> List[str]:
        """The parameters that actually varied."""
        return self.raw.get("axes") or []

    @property
    def grids(self) -> Dict[str, list]:
        return self.raw.get("grids") or {}

    @property
    def robustness(self) -> Any:
        """Deflated Sharpe, PBO, plateau ratio, and the verdict."""
        return series(self.raw.get("robustness") or {})

    @property
    def verdict(self) -> str:
        return (self.raw.get("robustness") or {}).get("verdict", "")

    @property
    def best(self) -> Optional[Backtest]:
        """The winning combination's full result."""
        best = self.raw.get("best") or []
        return Backtest(best[0]) if best else None

    def surface(self, x: str, y: str) -> Any:
        """A grid of scores over two parameters, for a heatmap.

        Other parameters are held at the values the best row used, which is
        the slice through the space the reported winner actually lives on.
        """
        rows = [r for r in (self.raw.get("rows") or [])
                if not r.get("error") and r.get("score") is not None]
        if not rows:
            return frame([], index=None)
        pin = max(rows, key=lambda r: r["score"]).get("params") or {}

        table: Dict[Any, Dict[Any, Any]] = {}
        for r in rows:
            p = r.get("params") or {}
            if any(str(p.get(ax)) != str(pin.get(ax))
                   for ax in self.axes if ax not in (x, y)):
                continue
            table.setdefault(p.get(y), {})[p.get(x)] = r["score"]

        out = []
        for yv in sorted(table, key=_sortable):
            row = {y: yv}
            row.update({str(k): v for k, v in sorted(table[yv].items(), key=lambda kv: _sortable(kv[0]))})
            out.append(row)
        return frame(out, index=y)

    def __repr__(self) -> str:
        r = self.raw.get("robustness") or {}
        return (f"<Sweep {self.raw.get('combos', 0)} combinations "
                f"best={_num(r.get('best_score'))} pbo={_pct(r.get('pbo'))}>")


class WalkForward(_Result):
    """A rolling train/test evaluation."""

    @property
    def folds(self) -> Any:
        out = []
        for f in self.raw.get("folds") or []:
            row = {k: v for k, v in f.items()
                   if k not in ("test_curve", "train_metrics", "test_metrics", "best_params")}
            row["params"] = " ".join(f"{k}={v}" for k, v in sorted((f.get("best_params") or {}).items()))
            row["train_return"] = (f.get("train_metrics") or {}).get("total_return")
            row["test_return"] = (f.get("test_metrics") or {}).get("total_return")
            out.append(row)
        return frame(out, index=None)

    @property
    def curve(self) -> Any:
        """The stitched out-of-sample equity curve.

        This is the only equity line in the tool that was never fitted to.
        """
        return frame(self.raw.get("stitched") or [], index="date")

    @property
    def metrics(self) -> Any:
        return series(self.raw.get("stitched_metrics") or {})

    @property
    def efficiency(self) -> Optional[float]:
        """Out-of-sample return over in-sample return."""
        return self.raw.get("efficiency")

    @property
    def verdict(self) -> str:
        return self.raw.get("verdict", "")

    def __repr__(self) -> str:
        return (f"<WalkForward {len(self.raw.get('folds') or [])} folds "
                f"oos_return={_pct((self.raw.get('stitched_metrics') or {}).get('total_return'))} "
                f"efficiency={_pct(self.raw.get('efficiency'))}>")


class AgentRun(_Result):
    """A guided search scored once against a withheld holdout."""

    @property
    def candidates(self) -> Any:
        out = []
        for c in self.raw.get("candidates") or []:
            m = c.get("train_metrics") or {}
            out.append({
                "iteration": c.get("iteration"),
                "rationale": c.get("rationale"),
                "train_return": m.get("total_return"),
                "train_cagr": m.get("cagr"),
                "train_sharpe": m.get("sharpe"),
                "trust_score": (c.get("critique") or {}).get("trust_score"),
                "error": c.get("error", ""),
            })
        return frame(out, index="iteration")

    @property
    def holdout(self) -> Optional[Backtest]:
        """The winner's single out-of-sample measurement."""
        h = self.raw.get("holdout")
        return Backtest(h) if h else None

    @property
    def degradation(self) -> Optional[float]:
        """How much of the training improvement survived the holdout."""
        return self.raw.get("degradation")

    @property
    def verdict(self) -> str:
        return self.raw.get("verdict", "")

    def __repr__(self) -> str:
        return (f"<AgentRun {len(self.raw.get('candidates') or [])} candidates "
                f"degradation={_pct(self.raw.get('degradation'))}>")


def _pct(v: Any) -> str:
    if v is None:
        return "n/a"
    try:
        return f"{float(v) * 100:.2f}%"
    except (TypeError, ValueError):
        return "n/a"


def _num(v: Any) -> str:
    if v is None:
        return "n/a"
    try:
        return f"{float(v):.2f}"
    except (TypeError, ValueError):
        return "n/a"


def _sortable(v: Any) -> Any:
    """Sort numerically when possible, so a grid axis reads 5, 10, 20 rather
    than 10, 20, 5."""
    try:
        return (0, float(v))
    except (TypeError, ValueError):
        return (1, str(v))
