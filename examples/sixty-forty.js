// The textbook 60/40 portfolio, rebalanced at the start of each quarter.
//
// universe: SPY,AGG
// benchmarks: SPY
// warmup: 5

function setup(ctx) {
  ctx.universe(["SPY", "AGG"]);

  // The split is the whole argument of this portfolio, so it is the thing
  // worth searching: sweep it and see whether 60/40 was ever special.
  ctx.param("equity", 0.6, { min: 0.2, max: 0.9, step: 0.1 });
  ctx.warmup(5);
}

function onDay(ctx) {
  if (!ctx.isFirstTradingDayOfMonth()) return;
  // Quarters begin in January, April, July and October.
  if ([1, 4, 7, 10].indexOf(ctx.month) === -1) return;

  const w = ctx.params.equity;
  ctx.rebalance({ SPY: w, AGG: 1 - w }, "quarterly rebalance");
}
