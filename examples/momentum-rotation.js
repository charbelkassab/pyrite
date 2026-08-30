// Hold the three strongest large-cap technology names, refreshed monthly.

function setup(ctx) {
  ctx.universe("tech");
  ctx.warmup(140);
}

function onDay(ctx) {
  if (!ctx.isFirstTradingDayOfMonth()) return;

  // Six months is roughly 126 trading days.
  const winners = ctx.rank("momentum", 3, { window: 126 });
  if (!winners.length) return;

  ctx.log("rotating into " + winners.join(", "));
  ctx.equalWeight(winners);
}
