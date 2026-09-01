package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/charbelkassab/pyrite/internal/config"
	"github.com/charbelkassab/pyrite/internal/engine"
)

// Narrate asks a model to write the opening summary of a report.
//
// The model writes prose and nothing else. Every number in the document is
// computed, and the model is shown them rather than asked to produce them —
// a model is a good writer and an unreliable arithmetician, and a report whose
// figures came from a language model would be worth less than no report.
func (a *App) Narrate(ctx context.Context, rep *engine.Report) (string, error) {
	if a.LLM == nil || !a.Cfg.AnyProviderEnabled() {
		return "", fmt.Errorf("no model is configured")
	}
	if rep == nil || rep.Run == nil {
		return "", fmt.Errorf("nothing to narrate")
	}

	resp, err := a.LLM.Complete(ctx, llmRequest(config.TierQuality,
		narrateSystemPrompt(), narrateUserPrompt(rep)))
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(resp.Text)
	// A model that returns a JSON object or a fenced block despite being
	// asked for prose should not put braces in the document.
	text = strings.TrimPrefix(text, "```markdown")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	return strings.TrimSpace(text), nil
}

func narrateSystemPrompt() string {
	return `You write the opening summary of a quantitative research note.

Every figure you are given was computed from the run. Use them; do not invent
any, do not round them into vagueness, and do not produce a number that is not
in front of you.

Write three or four short paragraphs of plain prose, in Markdown, with no
heading. Cover, in this order:

1. What the strategy does, in one sentence a non-specialist would follow.
2. What it produced, and how that compares to simply holding the benchmark.
3. The single most important reason to doubt it. There is always one. If the
   objections list is empty, the reason is the general one: a backtest is one
   sample of one history.
4. What a reader should conclude, stated plainly.

Rules:

- Be specific. "The return is concentrated in five sessions" is useful;
  "results may vary" is not.
- Do not hedge everything equally. If the out-of-sample evidence is strong,
  say so; if it is absent, say that instead of implying it is weak.
- Never recommend trading it. This is a research note, not advice.
- No bullet lists, no headings, no preamble about what you are about to do.`
}

func narrateUserPrompt(rep *engine.Report) string {
	var b strings.Builder
	if rep.Prompt != "" {
		fmt.Fprintf(&b, "The user asked for: %s\n\n", rep.Prompt)
	}
	m := rep.Run.Metrics
	fmt.Fprintf(&b, "Period %s to %s, %d sessions.\n", rep.Run.Spec.Start, rep.Run.Spec.End, m.TradingDays)
	fmt.Fprintf(&b, "Total return %.2f%%, annualised %.2f%%, Sharpe %s, max drawdown %.2f%%.\n",
		m.TotalReturn*100, m.CAGR*100, ratioText(m.Sharpe), m.MaxDrawdown*100)
	for _, bm := range rep.Run.Benchmarks {
		fmt.Fprintf(&b, "Benchmark %s: total return %.2f%%, max drawdown %.2f%%.\n",
			bm.Label, bm.Metric.TotalReturn*100, bm.Metric.MaxDrawdown*100)
	}
	t := rep.Run.TradeStats
	fmt.Fprintf(&b, "%d closed trades, win rate %.0f%%, expectancy %.0f.\n",
		t.Closed, t.WinRate*100, t.Expectancy)
	fmt.Fprintf(&b, "Return skew %.2f, excess kurtosis %.1f.\n\n",
		rep.Run.Risk.Skew, rep.Run.Risk.ExcessKurtosis)

	if wf := rep.WalkForward; wf != nil {
		fmt.Fprintf(&b, "Walk-forward, out of sample: total return %.2f%%, Sharpe %s, "+
			"%d of %d test windows positive. %s\n\n",
			wf.StitchedMetrics.TotalReturn*100, ratioText(wf.StitchedMetrics.Sharpe),
			wf.ConsistentFolds, len(wf.Folds), wf.Verdict)
	} else {
		b.WriteString("No out-of-sample evaluation was run.\n\n")
	}
	if sw := rep.Sweep; sw != nil {
		fmt.Fprintf(&b, "Parameter search over %d configurations. %s\n\n",
			sw.Combos, sw.Robustness.Verdict)
	}
	if c := rep.Costs; c != nil && c.Verdict != "" {
		fmt.Fprintf(&b, "Cost sensitivity: %s\n\n", c.Verdict)
	}
	if f := rep.Factors; f != nil && f.Verdict != "" {
		fmt.Fprintf(&b, "Factor decomposition against ETF proxies (not the academic "+
			"series, so treat the loadings as indicative): %s\n\n", f.Verdict)
	}
	if bs := rep.Bootstrap; bs.Trials > 0 {
		fmt.Fprintf(&b, "Block bootstrap over %d resamples: fifth-percentile total return "+
			"%.2f%%, median %.2f%%, fifth-percentile max drawdown %.2f%%, "+
			"%.0f%% chance of finishing down.\n\n",
			bs.Trials, bs.ReturnP05*100, bs.ReturnMedian*100,
			bs.DrawdownP05*100, bs.LossProbability*100)
	}

	c := rep.Run.Critique
	fmt.Fprintf(&b, "Computed objections (trust score %d/100):\n", c.TrustScore)
	if len(c.Findings) == 0 {
		b.WriteString("  none found\n")
	}
	for _, f := range c.Findings {
		fmt.Fprintf(&b, "  [%s] %s — %s\n", f.Severity, f.Title, f.Detail)
	}
	b.WriteString("\nThe strategy code:\n```javascript\n")
	b.WriteString(rep.Run.Spec.Code)
	b.WriteString("\n```\n")
	return b.String()
}
