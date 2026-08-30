// Buy mega caps that have become oversold, exit when they recover.
//
// Each position is capped at 10% of the portfolio so one name cannot dominate,
// and an 8% stop bounds the damage when "oversold" turns out to mean "falling".
//
// universe: megacap
// benchmarks: SPY
// warmup: 80

function setup(ctx) {
  ctx.universe("megacap");

  ctx.param("entry", 30, { grid: [20, 25, 30, 35, 40] });
  ctx.param("exit", 55, { grid: [45, 50, 55, 60, 70] });
  ctx.param("stop", 0.08, { min: 0.04, max: 0.16, step: 0.02 });
  ctx.param("size", 0.10, { grid: [0.05, 0.10, 0.20] });
  ctx.warmup(80);
}

function onDay(ctx) {
  for (const sym of ctx.universe()) {
    const rsi = ctx.rsi(sym, 14);
    if (rsi === null) continue;

    if (rsi < ctx.params.entry && !ctx.hasPosition(sym) &&
        ctx.cash > ctx.equity * ctx.params.size) {
      ctx.buy(sym, { pctEquity: ctx.params.size, stopLoss: ctx.params.stop },
              "RSI " + rsi.toFixed(0) + ", oversold");
    } else if (rsi > ctx.params.exit && ctx.hasPosition(sym)) {
      ctx.close(sym, "RSI " + rsi.toFixed(0) + ", recovered");
    }
  }
}
