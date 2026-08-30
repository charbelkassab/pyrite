// Market-neutral pairs trade on two consumer staples that normally move together.
//
// Requires shorting: run with allow_short set.
//
// universe: KO,PEP
// benchmarks: SPY
// warmup: 160
// allow_short: true

function setup(ctx) {
  ctx.universe(["KO", "PEP"]);

  ctx.param("window", 60, { grid: [20, 40, 60, 90, 120] });
  ctx.param("entry", 2.0, { min: 1.0, max: 3.0, step: 0.5 });
  ctx.param("exit", 0.5, { min: 0.0, max: 1.5, step: 0.5 });
  ctx.warmup(160);
}

function onDay(ctx) {
  const n = ctx.params.window;
  const ko = ctx.closes("KO", n + 1);
  const pep = ctx.closes("PEP", n + 1);
  if (ko.length < n + 1 || pep.length < n + 1) return;

  // Build the price ratio series and measure how far today's ratio sits from
  // its own trailing average.
  const ratios = [];
  for (let i = 0; i < ko.length; i++) {
    if (pep[i] > 0) ratios.push(ko[i] / pep[i]);
  }
  if (ratios.length < n) return;

  const window = ratios.slice(-n);
  const mean = window.reduce((a, b) => a + b, 0) / window.length;
  let ss = 0;
  for (const r of window) ss += (r - mean) * (r - mean);
  const sd = Math.sqrt(ss / (window.length - 1));
  if (sd === 0) return;

  const z = (ratios[ratios.length - 1] - mean) / sd;
  const openPosition = ctx.hasPosition("KO") || ctx.hasPosition("PEP");
  const entry = ctx.params.entry;

  if (!openPosition && z < -entry) {
    ctx.setWeight("KO", 0.5, "ratio " + entry + " sigma cheap");
    ctx.setWeight("PEP", -0.5, "ratio " + entry + " sigma cheap");
  } else if (!openPosition && z > entry) {
    ctx.setWeight("KO", -0.5, "ratio " + entry + " sigma rich");
    ctx.setWeight("PEP", 0.5, "ratio " + entry + " sigma rich");
  } else if (openPosition && Math.abs(z) < ctx.params.exit) {
    ctx.close("KO", "ratio back to its average");
    ctx.close("PEP", "ratio back to its average");
  }
}
