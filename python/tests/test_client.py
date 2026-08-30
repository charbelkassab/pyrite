"""End-to-end tests against a real natural-quant binary.

These start an actual server in offline mode, so they exercise the whole path
— HTTP, JSON shapes, result parsing — rather than a mock that would happily
agree with a wrong assumption about what the API returns.
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from natural_quant import Client, RunFailed, ServerUnavailable  # noqa: E402
from natural_quant.frames import has_pandas  # noqa: E402

CROSS = """
function setup(ctx) {
  ctx.universe(["SPY"]);
  ctx.param("fast", 20, { grid: [10, 20, 40] });
  ctx.param("slow", 100, { grid: [50, 100] });
  ctx.warmup(120);
}
function onDay(ctx) {
  const f = ctx.sma("SPY", ctx.params.fast);
  const s = ctx.sma("SPY", ctx.params.slow);
  if (f === null || s === null) return;
  if (f > s && !ctx.hasPosition("SPY")) ctx.buy("SPY", { pctCash: 0.95 }, "cross up");
  else if (f < s && ctx.hasPosition("SPY")) ctx.close("SPY", "cross down");
}
"""

COMMON = dict(universe=["SPY"], warmup=120, start="2016-01-05",
              end="2023-12-29", benchmarks=["SPY"])


class ClientTest(unittest.TestCase):
    server = None

    @classmethod
    def setUpClass(cls):
        cls._ctx = Client.serve(offline=True)
        cls.nq = cls._ctx.__enter__()

    @classmethod
    def tearDownClass(cls):
        cls._ctx.__exit__(None, None, None)

    def test_health_and_metadata(self):
        h = self.nq.health()
        self.assertTrue(h.get("offline_mode"), "the test server should be offline")
        self.assertIn("sharpe", self.nq.objectives())
        self.assertTrue(any(u["key"] == "megacap" for u in self.nq.universes()))
        self.assertIn("ctx.param", self.nq.strategy_api())

    def test_backtest_returns_every_section(self):
        run = self.nq.backtest(code=CROSS, **COMMON)

        m = run.metrics
        total = m["total_return"] if has_pandas() else m["total_return"]
        self.assertIsInstance(float(total), float)
        self.assertGreater(len(run.curve), 500)
        self.assertGreater(len(run.trades), 0)
        self.assertGreater(len(run.rolling), 0)

        # The sections Phase 1 added must all arrive.
        self.assertGreater(len(run.by_year), 4)
        self.assertGreater(len(run.stress), 0)
        self.assertTrue(run.manifest.get("code_sha256"))
        self.assertEqual(run.manifest.get("data_provider"), "synthetic")

        # And the critique, which is the point of the tool.
        self.assertGreaterEqual(run.trust_score, 0)
        self.assertLessEqual(run.trust_score, 100)
        for f in run.critique:
            self.assertIn(f["severity"], ("critical", "warning", "note"))
            self.assertTrue(f["title"])
            self.assertTrue(f["detail"])

        self.assertIn("total return", run.summary())
        self.assertIn("Backtest", repr(run))

    def test_trades_carry_excursions(self):
        run = self.nq.backtest(code=CROSS, **COMMON)
        rows = run.trades if not has_pandas() else run.trades.to_dict("records")
        closed = [t for t in rows if not t.get("open")]
        self.assertGreater(len(closed), 0, "expected some closed round trips")
        for t in closed:
            self.assertLessEqual(t["mae_pct"], 0, "MAE must be an adverse move")
            self.assertGreaterEqual(t["mfe_pct"], 0, "MFE must be a favourable move")
            self.assertAlmostEqual(t["gross_pnl"] - t["costs"], t["net_pnl"], places=6)

    def test_benchmark_curve_is_available(self):
        run = self.nq.backtest(code=CROSS, **COMMON)
        self.assertGreater(len(run.benchmarks), 0)
        self.assertGreater(len(run.benchmark_curve("SPY")), 500)

    def test_sweep_and_surface(self):
        sw = self.nq.sweep(code=CROSS, **COMMON)
        self.assertEqual(sw.raw["combos"], 6)
        self.assertEqual(sorted(sw.axes), ["fast", "slow"])
        self.assertEqual(len(sw.rows), 6)
        self.assertTrue(sw.verdict)

        surface = sw.surface("fast", "slow")
        self.assertEqual(len(surface), 2, "two values of slow means two rows")

        best = sw.best
        self.assertIsNotNone(best, "the winner's full result should be retained")
        self.assertGreater(len(best.curve), 0)

    def test_sweep_grids_override_the_declaration(self):
        sw = self.nq.sweep(code=CROSS, grids={"fast": [5, 8]}, **COMMON)
        self.assertEqual(sw.raw["combos"], 4, "2 supplied x 2 declared")

    def test_walkforward_reports_out_of_sample(self):
        wf = self.nq.walkforward(code=CROSS, train_days=300, test_days=150, **COMMON)
        self.assertGreater(len(wf.folds), 1)
        self.assertGreater(len(wf.curve), 0)
        self.assertTrue(wf.verdict)
        self.assertIn("WalkForward", repr(wf))
        # Every fold must test on data after it trained on.
        rows = wf.folds if not has_pandas() else wf.folds.to_dict("records")
        for f in rows:
            self.assertGreater(f["test_start"], f["train_end"])

    def test_run_history(self):
        run = self.nq.backtest(code=CROSS, **COMMON)
        listing = self.nq.runs(limit=5)
        self.assertTrue(any(r["id"] == run.id for r in listing))
        again = self.nq.run(run.id)
        self.assertEqual(again.manifest["code_sha256"], run.manifest["code_sha256"])

    def test_universe_accepts_a_bare_string(self):
        opts = dict(COMMON, universe="SPY")
        run = self.nq.backtest(code=CROSS, **opts)
        self.assertGreater(len(run.curve), 0)

    def test_broken_strategy_raises(self):
        with self.assertRaises(RunFailed):
            self.nq.backtest(code="this is not javascript {{{", **COMMON)

    def test_prompt_without_a_key_is_refused_clearly(self):
        # Offline mode has no model, so a plain-English prompt cannot compile.
        with self.assertRaises(Exception) as cm:
            self.nq.backtest("buy SPY when the moon is full", **COMMON)
        self.assertIn("API key", str(cm.exception))

    def test_missing_prompt_and_code_is_a_value_error(self):
        with self.assertRaises(ValueError):
            self.nq.backtest()


class UnreachableServerTest(unittest.TestCase):
    def test_helpful_error(self):
        nq = Client("http://127.0.0.1:1", timeout=2)
        with self.assertRaises(ServerUnavailable) as cm:
            nq.health()
        self.assertIn("natural-quant serve", str(cm.exception))


if __name__ == "__main__":
    unittest.main(verbosity=2)
