// Classic 50/200 moving average crossover with a trailing stop.
//
// Note the use of ctx.state to detect the *crossing* rather than the condition.
// Testing "fast > slow" fires on every day the condition holds; a crossover
// strategy should act only on the day it becomes true.
//
// universe: SPY
// benchmarks: SPY
// warmup: 270

function setup(ctx) {
  ctx.universe(["SPY"]);

  // Every number this strategy depends on is declared rather than written
  // inline, so `pyrite sweep --example golden-cross` can search the
  // space around it instead of testing the one point someone happened to pick.
  ctx.param("fast", 50, { grid: [20, 35, 50, 65, 80] });
  ctx.param("slow", 200, { grid: [100, 150, 200, 250] });
  ctx.param("trail", 0.12, { min: 0.06, max: 0.20, step: 0.02 });

  // Warm-up comes from the largest value the slow grid can take, not from
  // its default: 200 bars would leave the 250 setting untradeable.
  ctx.warmup(270);
}

function onDay(ctx) {
  const fast = ctx.sma("SPY", ctx.params.fast);
  const slow = ctx.sma("SPY", ctx.params.slow);
  if (fast === null || slow === null) return;

  const above = fast > slow;
  const wasAbove = ctx.state.above;
  ctx.state.above = above;

  // No prior observation yet, so no crossing can be detected.
  if (wasAbove === undefined) return;

  if (above && !wasAbove) {
    ctx.buy("SPY", { pctCash: 1, trailingStop: ctx.params.trail },
            ctx.params.fast + " day crossed above " + ctx.params.slow + " day");
  } else if (!above && wasAbove && ctx.hasPosition("SPY")) {
    ctx.close("SPY", ctx.params.fast + " day crossed below " + ctx.params.slow + " day");
  }
}
