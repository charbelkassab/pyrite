"""HTTP client for a natural-quant server."""

from __future__ import annotations

import json
import os
import shutil
import socket
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
from contextlib import contextmanager
from typing import Any, Dict, Iterator, List, Optional, Sequence

from .results import AgentRun, Backtest, Sweep, WalkForward

DEFAULT_URL = "http://127.0.0.1:8080"


class NaturalQuantError(Exception):
    """Base class for every error this package raises."""


class ServerUnavailable(NaturalQuantError):
    """No server is reachable at the configured address."""


class RunFailed(NaturalQuantError):
    """A backtest or search finished in an error state."""


class Client:
    """Talks to a running natural-quant server.

    The server does all the work. This class exists so results arrive as
    Python objects rather than JSON, and so a notebook can drive the same
    engine the CLI and the web interface drive.
    """

    def __init__(self, url: str = DEFAULT_URL, timeout: float = 900.0):
        self.url = url.rstrip("/")
        self.timeout = timeout

    # -- lifecycle ---------------------------------------------------------

    @classmethod
    @contextmanager
    def serve(cls, binary: Optional[str] = None, offline: bool = False,
              port: Optional[int] = None, timeout: float = 900.0,
              env: Optional[Dict[str, str]] = None) -> Iterator["Client"]:
        """Start a server for the duration of a block, and stop it after.

            with Client.serve(offline=True) as nq:
                run = nq.backtest(code=..., universe=["SPY"])

        The binary is found on PATH, or via NQ_BINARY, or passed explicitly.
        """
        exe = binary or os.environ.get("NQ_BINARY") or shutil.which("natural-quant")
        if not exe:
            raise ServerUnavailable(
                "no natural-quant binary found. Pass binary=..., set NQ_BINARY, "
                "or put it on PATH. Alternatively start one yourself and use "
                "Client(url=...) instead."
            )
        port = port or _free_port()
        addr = f"127.0.0.1:{port}"

        cmd = [exe, "serve", "--addr", addr]
        if offline:
            cmd.append("--offline")
        proc = subprocess.Popen(
            cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
            env={**os.environ, **(env or {})},
        )
        client = cls(f"http://{addr}", timeout=timeout)
        try:
            client._wait_ready(proc)
            yield client
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:  # pragma: no cover
                proc.kill()
                proc.wait(timeout=5)
            # The stderr pipe is ours to close; leaving it open leaks a file
            # descriptor per server started, which a test suite notices long
            # before a user does.
            if proc.stderr is not None:
                proc.stderr.close()

    def _wait_ready(self, proc: subprocess.Popen, seconds: float = 30.0) -> None:
        deadline = time.time() + seconds
        while time.time() < deadline:
            if proc.poll() is not None:
                err = (proc.stderr.read() or b"").decode(errors="replace") if proc.stderr else ""
                raise ServerUnavailable(f"the server exited immediately: {err.strip()[:400]}")
            try:
                self.health()
                return
            except NaturalQuantError:
                time.sleep(0.15)
        raise ServerUnavailable(f"the server did not become ready within {seconds:.0f}s")

    # -- plumbing ----------------------------------------------------------

    def _request(self, method: str, path: str, body: Optional[dict] = None,
                 timeout: Optional[float] = None) -> Any:
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(
            self.url + path, data=data, method=method,
            headers={"Content-Type": "application/json"} if data else {},
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout or self.timeout) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as e:
            with e:
                detail = e.read().decode(errors="replace")
            try:
                detail = json.loads(detail).get("error", detail)
            except json.JSONDecodeError:
                pass
            raise NaturalQuantError(f"{method} {path}: {e.code} {detail}") from None
        except (urllib.error.URLError, OSError) as e:
            raise ServerUnavailable(
                f"cannot reach a natural-quant server at {self.url} ({e}). "
                f"Start one with `natural-quant serve`, or use Client.serve()."
            ) from None
        if not raw:
            return None
        return json.loads(raw)

    # -- read-only endpoints ----------------------------------------------

    def health(self) -> dict:
        """Provider, data source and cache status."""
        return self._request("GET", "/api/health", timeout=15)

    def universes(self) -> List[dict]:
        """The built-in symbol lists."""
        return self._request("GET", "/api/universes", timeout=15)

    def objectives(self) -> List[str]:
        """Metrics a search can be ranked by."""
        return (self._request("GET", "/api/objectives", timeout=15) or {}).get("objectives", [])

    def strategy_api(self) -> str:
        """The strategy API reference, as Markdown."""
        req = urllib.request.Request(self.url + "/api/strategy-api")
        with urllib.request.urlopen(req, timeout=15) as resp:
            return resp.read().decode()

    def search_symbols(self, query: str) -> List[dict]:
        return self._request("GET", "/api/symbols?" + urllib.parse.urlencode({"q": query}), timeout=20)

    def series(self, symbols: Sequence[str], start: str = "", end: str = "") -> Any:
        """Price history for arbitrary symbols, as buy-and-hold curves."""
        q = {"symbols": ",".join(symbols)}
        if start:
            q["from"] = start
        if end:
            q["to"] = end
        return self._request("GET", "/api/series?" + urllib.parse.urlencode(q))

    # -- running things ----------------------------------------------------

    def backtest(self, prompt: str = "", *, code: str = "", **options: Any) -> Backtest:
        """Run one backtest and wait for it.

        Pass a plain-English `prompt` to have it compiled, or `code` to run a
        strategy you already have. Options map to the run request: universe,
        start, end, initial_cash, benchmarks, fill, allow_short, warmup,
        slippage_bps, commission_pct, params.
        """
        body = _run_body(prompt, code, options)
        run = self._await_run(self._request("POST", "/api/runs", body)["id"])
        if not run.get("result"):
            raise RunFailed(run.get("error") or "the run produced no result")
        return Backtest(run["result"], run)

    def sweep(self, prompt: str = "", *, code: str = "",
              grids: Optional[Dict[str, list]] = None,
              objective: str = "sharpe", max_combos: int = 5000,
              **options: Any) -> Sweep:
        """Search a strategy's declared parameters."""
        body = _run_body(prompt, code, options)
        body.update({"grids": grids or {}, "objective": objective, "max_combos": max_combos})
        run = self._await_run(self._request("POST", "/api/sweeps", body)["id"])
        if not run.get("sweep"):
            raise RunFailed(run.get("error") or "the search produced no result")
        return Sweep(run["sweep"])

    def walkforward(self, prompt: str = "", *, code: str = "",
                    train_days: int = 504, test_days: int = 126,
                    embargo: int = 0, anchored: bool = False,
                    grids: Optional[Dict[str, list]] = None,
                    objective: str = "sharpe", **options: Any) -> WalkForward:
        """Optimise on rolling training windows and report on the windows after."""
        body = _run_body(prompt, code, options)
        body.update({
            "walk_forward": True, "train_days": train_days, "test_days": test_days,
            "embargo": embargo, "anchored": anchored,
            "grids": grids or {}, "objective": objective,
        })
        run = self._await_run(self._request("POST", "/api/sweeps", body)["id"])
        if not run.get("walk_forward"):
            raise RunFailed(run.get("error") or "the evaluation produced no result")
        return WalkForward(run["walk_forward"])

    # -- run history -------------------------------------------------------

    def runs(self, limit: int = 25) -> List[dict]:
        """Recent runs, newest first."""
        return self._request("GET", f"/api/runs?limit={int(limit)}", timeout=30) or []

    def run(self, run_id: str) -> Backtest:
        """Fetch a completed run by id."""
        run = self._request("GET", f"/api/runs/{run_id}")
        if not run.get("result"):
            raise RunFailed(run.get("error") or f"run {run_id} has no result")
        return Backtest(run["result"], run)

    def cancel(self, run_id: str) -> None:
        self._request("POST", f"/api/runs/{run_id}/cancel", {}, timeout=15)

    def _await_run(self, run_id: str, poll: float = 0.25) -> dict:
        """Poll until a run reaches a terminal state.

        Polling rather than following the SSE stream keeps this package to the
        standard library. The interval is short enough that a fast run feels
        immediate and slack enough that a long one costs nothing.
        """
        deadline = time.time() + self.timeout
        while time.time() < deadline:
            run = self._request("GET", f"/api/runs/{run_id}", timeout=120)
            status = run.get("status")
            if status in ("done", "error", "cancelled"):
                if status != "done":
                    raise RunFailed(run.get("error") or f"run {run_id} ended as {status}")
                return run
            time.sleep(poll)
        raise RunFailed(f"run {run_id} did not finish within {self.timeout:.0f}s")


def _run_body(prompt: str, code: str, options: Dict[str, Any]) -> Dict[str, Any]:
    if not prompt and not code:
        raise ValueError("pass a prompt to compile, or code= to run directly")
    body: Dict[str, Any] = {"prompt": prompt, "code": code}
    # Universe accepts a bare string for convenience, including "sp500".
    if isinstance(options.get("universe"), str):
        options["universe"] = [options["universe"]]
    body.update({k: v for k, v in options.items() if v is not None})
    return body


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]
