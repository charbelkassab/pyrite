// Own whichever US company is currently the largest by market capitalisation.
//
// Market cap is computed as raw close x point-in-time shares outstanding. See
// docs/limitations.md — that share-count table is approximate, and it is the
// main source of error in this strategy.

function setup(ctx) {
  ctx.universe("megacap");
  ctx.warmup(10);
}

function onDay(ctx) {
  const top = ctx.biggestCompany();
  if (!top) return;

  // Sell anything that is no longer number one.
  for (const sym of ctx.heldSymbols()) {
    if (sym !== top) ctx.close(sym, "no longer the largest company");
  }

  // Put everything into the current leader.
  if (!ctx.hasPosition(top)) {
    ctx.buy(top, { pctCash: 1 }, "largest company by market cap");
  }
}
