// The textbook 60/40 portfolio, rebalanced at the start of each quarter.

function setup(ctx) {
  ctx.universe(["SPY", "AGG"]);
  ctx.warmup(5);
}

function onDay(ctx) {
  if (!ctx.isFirstTradingDayOfMonth()) return;
  // Quarters begin in January, April, July and October.
  if ([1, 4, 7, 10].indexOf(ctx.month) === -1) return;

  ctx.rebalance({ SPY: 0.6, AGG: 0.4 }, "quarterly rebalance");
}
