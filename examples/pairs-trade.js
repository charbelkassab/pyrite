// Market-neutral pairs trade on two consumer staples that normally move together.
//
// Requires shorting: run with allow_short set.

function setup(ctx) {
  ctx.universe(["KO", "PEP"]);
  ctx.warmup(90);
}

function onDay(ctx) {
  const ko = ctx.closes("KO", 61);
  const pep = ctx.closes("PEP", 61);
  if (ko.length < 61 || pep.length < 61) return;

  // Build the price ratio series and measure how far today's ratio sits from
  // its own 60 day average.
  const ratios = [];
  for (let i = 0; i < ko.length; i++) {
    if (pep[i] > 0) ratios.push(ko[i] / pep[i]);
  }
  if (ratios.length < 60) return;

  const window = ratios.slice(-60);
  const mean = window.reduce((a, b) => a + b, 0) / window.length;
  let ss = 0;
  for (const r of window) ss += (r - mean) * (r - mean);
  const sd = Math.sqrt(ss / (window.length - 1));
  if (sd === 0) return;

  const z = (ratios[ratios.length - 1] - mean) / sd;
  const openPosition = ctx.hasPosition("KO") || ctx.hasPosition("PEP");

  if (!openPosition && z < -2) {
    ctx.setWeight("KO", 0.5, "ratio 2 sigma cheap");
    ctx.setWeight("PEP", -0.5, "ratio 2 sigma cheap");
  } else if (!openPosition && z > 2) {
    ctx.setWeight("KO", -0.5, "ratio 2 sigma rich");
    ctx.setWeight("PEP", 0.5, "ratio 2 sigma rich");
  } else if (openPosition && Math.abs(z) < 0.25) {
    ctx.close("KO", "ratio back to its average");
    ctx.close("PEP", "ratio back to its average");
  }
}
