// Hold the three strongest large-cap technology names, refreshed monthly.
//
// universe: tech
// benchmarks: QQQ
// warmup: 280

function setup(ctx) {
  ctx.universe("tech");

  ctx.param("hold", 3, { grid: [1, 2, 3, 5, 8] });
  ctx.param("lookback", 126, { grid: [21, 63, 126, 189, 252] });
  ctx.warmup(280);
}

function onDay(ctx) {
  if (!ctx.isFirstTradingDayOfMonth()) return;

  // One month is roughly 21 trading days, so 126 is about six months.
  const winners = ctx.rank("momentum", ctx.params.hold, { window: ctx.params.lookback });
  if (!winners.length) return;

  ctx.log("rotating into " + winners.join(", "));
  ctx.equalWeight(winners);
}
