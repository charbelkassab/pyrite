// Classic 50/200 moving average crossover with a trailing stop.
//
// Note the use of ctx.state to detect the *crossing* rather than the condition.
// Testing "fast > slow" fires on every day the condition holds; a crossover
// strategy should act only on the day it becomes true.

function setup(ctx) {
  ctx.universe(["SPY"]);
  ctx.warmup(220);
}

function onDay(ctx) {
  const fast = ctx.sma("SPY", 50);
  const slow = ctx.sma("SPY", 200);
  if (fast === null || slow === null) return;

  const above = fast > slow;
  const wasAbove = ctx.state.above;
  ctx.state.above = above;

  // No prior observation yet, so no crossing can be detected.
  if (wasAbove === undefined) return;

  if (above && !wasAbove) {
    ctx.buy("SPY", { pctCash: 1, trailingStop: 0.12 }, "50 day crossed above 200 day");
  } else if (!above && wasAbove && ctx.hasPosition("SPY")) {
    ctx.close("SPY", "50 day crossed below 200 day");
  }
}
