"""Optional pandas integration.

Every tabular accessor in this package routes through `frame()`. With pandas
installed you get a DataFrame; without it you get the list of dicts that would
have gone into one. Nothing else in the package needs to know which.
"""

from __future__ import annotations

from typing import Any, Iterable, Optional, Sequence

try:  # pragma: no cover - exercised by whichever environment runs the tests
    import pandas as _pd
except ImportError:  # pragma: no cover
    _pd = None


def has_pandas() -> bool:
    """Whether tabular results will be returned as DataFrames."""
    return _pd is not None


def frame(rows: Iterable[dict], index: Optional[str] = None,
          columns: Optional[Sequence[str]] = None) -> Any:
    """Return rows as a DataFrame, or unchanged when pandas is absent.

    index names a column to use as the index; it is left as an ordinary key
    when pandas is not installed, so the data is never silently lost.
    """
    rows = list(rows)
    if _pd is None:
        return rows
    df = _pd.DataFrame(rows, columns=list(columns) if columns else None)
    if index and index in df.columns:
        df = df.set_index(index)
        if index in ("date", "test_start", "entry_date"):
            try:
                df.index = _pd.to_datetime(df.index)
            except (ValueError, TypeError):
                pass
    return df


def series(mapping: dict) -> Any:
    """Return a mapping as a Series, or unchanged when pandas is absent."""
    if _pd is None:
        return mapping
    return _pd.Series(mapping)
