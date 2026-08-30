package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charbelkassab/natural-quant/internal/config"
	"github.com/charbelkassab/natural-quant/internal/engine"
	"github.com/charbelkassab/natural-quant/internal/llm"
	"github.com/charbelkassab/natural-quant/internal/strategy"
)

// ModelProposer asks a model for the next strategy to try.
//
// Everything about how the search stays honest lives in engine.RunAgent, not
// here: this type can only see what it is handed, and what it is handed is a
// Candidate, which has no out-of-sample field. That is deliberate. A proposer
// that could read holdout results would optimise against them extremely well,
// and the resulting backtest would look exactly like evidence.
type ModelProposer struct {
	App *App
	// Goal is the user's own description of what they want improved.
	Goal string
	// Seed is the strategy the search starts from. The first proposal is the
	// seed itself, so the baseline is measured on the same footing as every
	// variant.
	Seed *strategy.Plan
	// Tier picks the model. Compilation-grade quality is worth it here: this
	// runs a handful of times per search, and a bad proposal wastes a whole
	// backtest.
	Tier config.Tier
}

// proposal is what the model returns.
type proposal struct {
	Code      string `json:"code"`
	Rationale string `json:"rationale"`
	// Done lets the model stop early when it has nothing left to try.
	Done bool `json:"done"`
}

// Propose implements engine.Proposer.
func (m *ModelProposer) Propose(ctx context.Context, history []engine.Candidate) (string, string, error) {
	if len(history) == 0 && m.Seed != nil {
		return m.Seed.Code, "the strategy as written, for a baseline", nil
	}
	if m.App == nil || m.App.Compiler == nil {
		return "", "", fmt.Errorf("no model is configured")
	}

	tier := m.Tier
	if tier == "" {
		tier = config.TierQuality
	}
	resp, err := m.App.LLM.Complete(ctx, llmRequest(tier,
		proposerSystemPrompt(), m.userPrompt(history)))
	if err != nil {
		return "", "", err
	}

	var p proposal
	if err := json.Unmarshal([]byte(extractJSONObject(resp.Text)), &p); err != nil {
		return "", "", fmt.Errorf("the model did not return a usable proposal: %w", err)
	}
	if p.Done {
		return "", "", nil
	}
	if strings.TrimSpace(p.Code) == "" {
		return "", "", nil
	}
	return p.Code, p.Rationale, nil
}

// proposerSystemPrompt sets the rules the proposer works under.
func proposerSystemPrompt() string {
	var b strings.Builder
	b.WriteString(`You are improving a trading strategy for natural-quant, under a
strict experimental protocol.

# What you can and cannot see

You are shown results over a TRAINING period only. A later HOLDOUT period
exists and is withheld from you deliberately. The strategy you settle on will
be scored once on that holdout, after this search closes, and that single
number is the only one anyone will believe.

This has a practical consequence for how you should work. Chasing the last
fraction of training performance is not merely useless, it is actively harmful:
it fits the training window and the holdout will expose it. Prefer changes that
are defensible as ideas over changes that merely raise the number.

# What to do with each result

You are given a critique of every attempt, computed from the run rather than
guessed. Act on it. If it says the sample is too small to mean anything, make
the strategy trade more, not better. If it says the returns are concentrated in
five sessions, that is not a strategy to tune, it is one to replace. If it says
the return shape is short volatility, adding leverage will look wonderful and
then destroy the holdout.

# Output format

Reply with a single JSON object and nothing else:

{
  "code":      "the complete JavaScript strategy, as a JSON string",
  "rationale": "one or two sentences: what you changed and why",
  "done":      false
}

Set "done": true and omit "code" when you have nothing worth trying. Stopping
early is a legitimate answer, and a better one than proposing noise.

# Rules

- Return the COMPLETE strategy every time, not a diff.
- Declare every number with ctx.param() so the harness can search it for you.
  You are competing on your best parameter setting, not your default one.
- Change one thing at a time where you can. A proposal that changes five things
  teaches nothing when it does better and nothing when it does worse.
- Only use functions in the API reference below.

`)
	b.WriteString("# The API you must write against\n\n")
	b.WriteString(strategy.APIReference())
	return b.String()
}

// userPrompt renders the history the proposer is allowed to see.
func (m *ModelProposer) userPrompt(history []engine.Candidate) string {
	var b strings.Builder
	if m.Goal != "" {
		fmt.Fprintf(&b, "What the user wants: %s\n\n", m.Goal)
	}
	fmt.Fprintf(&b, "Attempts so far, all measured on the training period only:\n\n")

	for _, c := range history {
		fmt.Fprintf(&b, "## Attempt %d", c.Iteration)
		if c.Rationale != "" {
			fmt.Fprintf(&b, " — %s", c.Rationale)
		}
		b.WriteString("\n")
		if c.Error != "" {
			fmt.Fprintf(&b, "FAILED: %s\n\n", c.Error)
			continue
		}
		fmt.Fprintf(&b, "return %.1f%%  CAGR %.1f%%  Sharpe %s  max drawdown %.1f%%\n",
			c.TrainMetrics.TotalReturn*100, c.TrainMetrics.CAGR*100,
			ratioText(c.TrainMetrics.Sharpe), c.TrainMetrics.MaxDrawdown*100)
		fmt.Fprintf(&b, "%d closed trades, win rate %.0f%%, expectancy %.0f, turnover %.1fx\n",
			c.TrainTrades.Closed, c.TrainTrades.WinRate*100,
			c.TrainTrades.Expectancy, c.TrainMetrics.Turnover)
		fmt.Fprintf(&b, "skew %.2f, excess kurtosis %.1f, ulcer index %.1f%%\n",
			c.TrainRisk.Skew, c.TrainRisk.ExcessKurtosis, c.TrainRisk.UlcerIndex*100)
		if len(c.Params) > 0 {
			fmt.Fprintf(&b, "best parameters found: %s\n", engine.FormatParams(c.Params))
		}
		if len(c.Critique.Findings) > 0 {
			fmt.Fprintf(&b, "\nWhat is wrong with it (%d/100):\n", c.Critique.TrustScore)
			for _, f := range c.Critique.Findings {
				fmt.Fprintf(&b, "  [%s] %s — %s\n", f.Severity, f.Title, f.Detail)
			}
		}
		b.WriteString("\nCode:\n```javascript\n")
		b.WriteString(c.Code)
		b.WriteString("\n```\n\n")
	}

	b.WriteString("Propose the next attempt, or set \"done\": true if nothing is worth trying.\n")
	return b.String()
}

func ratioText(r engine.Ratio) string {
	if !r.Defined() {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", float64(r))
}

// llmRequest builds a completion request for the proposer.
//
// Caching is off: two identical histories should still be allowed to produce
// different proposals, and a cached one would silently make the search repeat
// itself.
func llmRequest(tier config.Tier, system, user string) llm.Request {
	return llm.Request{
		Tier:     tier,
		JSONMode: true,
		NoCache:  true,
		Messages: []llm.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}
}

// extractJSONObject pulls the first balanced JSON object out of a reply that
// may be wrapped in prose or a code fence.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if j := strings.Index(rest, "\n"); j >= 0 {
			rest = rest[j+1:]
		}
		if k := strings.Index(rest, "```"); k >= 0 {
			s = strings.TrimSpace(rest[:k])
		}
	}
	start := strings.Index(s, "{")
	if start < 0 {
		return s
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// nothing
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:]
}
