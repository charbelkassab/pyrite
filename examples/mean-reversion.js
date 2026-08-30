// Buy mega caps that have become oversold, exit when they recover.
//
// Each position is capped at 10% of the portfolio so one name cannot dominate,
// and an 8% stop bounds the damage when "oversold" turns out to mean "falling".

function setup(ctx) {
  ctx.universe("megacap");
  ctx.warmup(80);
}

function onDay(ctx) {
  for (const sym of ctx.universe()) {
    const rsi = ctx.rsi(sym, 14);
    if (rsi === null) continue;

    if (rsi < 30 && !ctx.hasPosition(sym) && ctx.cash > ctx.equity * 0.1) {
      ctx.buy(sym, { pctEquity: 0.1, stopLoss: 0.08 },
              "RSI " + rsi.toFixed(0) + ", oversold");
    } else if (rsi > 55 && ctx.hasPosition(sym)) {
      ctx.close(sym, "RSI " + rsi.toFixed(0) + ", recovered");
    }
  }
}
