// Read the week's headlines about Apple, ask a model how they read, and hold
// AAPL only while the answer is positive.
//
// READ THIS FIRST. ctx.news() queries the internet as it is today, not as it
// was on the simulated day, and the model knows what happened after that date.
// This backtest demonstrates a mechanism; it is not evidence the idea works.
// See docs/limitations.md.

function setup(ctx) {
  ctx.universe(["AAPL"]);
  ctx.warmup(10);
}

function onDay(ctx) {
  // Once a week keeps the model call count near 200 over four years.
  if (!ctx.isFirstTradingDayOfWeek()) return;

  const hits = ctx.news("AAPL", { limit: 4 });
  if (!hits.length) return;

  const headlines = hits.map((h) => "- " + h.title).join("\n");
  const verdict = ctx.ai(
    "Headlines about Apple:\n" + headlines +
    "\n\nDoes the overall tone read as positive or negative for the stock? " +
    "Answer with exactly one word: POSITIVE or NEGATIVE.");

  if (!verdict) return;
  ctx.log("verdict: " + verdict.trim());

  if (/POSITIVE/i.test(verdict)) {
    ctx.setWeight("AAPL", 1, "headlines read positive");
  } else {
    ctx.setWeight("AAPL", 0, "headlines read negative");
  }
}
