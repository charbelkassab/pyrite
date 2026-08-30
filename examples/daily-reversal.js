// Buy yesterday's biggest losers among mega caps, replace them the next day.
//
// This one is here as a cautionary tale rather than a suggestion. Short-horizon
// reversal is a real, documented effect and the gross equity curve looks
// superb — but the strategy replaces its whole book every session, so it pays
// the spread twice a day on everything it owns. Run it with --cost-scan and
// watch a 300% return become a total loss somewhere between 5 and 20 bps.
//
// The lesson generalises: any backtest that rebalances daily is really a bet
// that your execution is cheaper than the edge, and the equity curve alone
// will never tell you whether it is.
//
// universe: megacap
// benchmarks: SPY
// warmup: 5

function setup(ctx) {
  ctx.universe("megacap");
  ctx.param("hold", 3, { grid: [2, 3, 5] });
  ctx.warmup(5);
}

function onDay(ctx) {
  // Rank the universe by yesterday's return, worst first.
  const scored = [];
  for (const s of ctx.symbols()) {
    const r = ctx.roc(s, 1);
    if (r !== null) scored.push({ s: s, r: r });
  }
  if (scored.length < 5) return;
  scored.sort(function (a, b) { return a.r - b.r; });

  const want = scored.slice(0, ctx.params.hold).map(function (x) { return x.s; });

  // Sell out of anything that dropped off the list, then equal-weight the rest.
  for (const s of ctx.heldSymbols()) {
    if (want.indexOf(s) < 0) ctx.close(s, "no longer among yesterday's losers");
  }
  ctx.equalWeight(want);
}
