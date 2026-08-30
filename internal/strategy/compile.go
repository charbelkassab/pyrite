// Package strategy turns a natural language description of a trading idea
// into a validated JavaScript strategy that the engine can execute.
package strategy

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charbelkassab/pyrite/internal/config"
	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/llm"
	"github.com/charbelkassab/pyrite/internal/market"
	"github.com/dop251/goja"
)

//go:embed assets/api.md
var apiReference string

// APIReference returns the strategy API documentation. It is the single
// source of truth: the same text is given to the model and published in the
// docs, so the two can never drift apart.
func APIReference() string { return apiReference }

// Plan is the model's structured answer: a runnable strategy plus everything
// needed to configure the backtest around it.
type Plan struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Code        string   `json:"code"`
	Universe    []string `json:"universe"`
	Benchmarks  []string `json:"benchmarks"`
	Warmup      int      `json:"warmup"`
	AllowShort  bool     `json:"allow_short"`
	NeedsAI     bool     `json:"needs_ai"`
	NeedsWeb    bool     `json:"needs_web"`
	// Assumptions records choices the model made that the prompt left open.
	// Surfacing these is what stops a backtest from quietly meaning something
	// other than what the user asked for.
	Assumptions []string `json:"assumptions"`
	// Limitations records aspects of the request that could not be modelled
	// faithfully.
	Limitations []string `json:"limitations"`

	// Populated by the compiler, not the model.
	Provider string   `json:"provider,omitempty"`
	Model    string   `json:"model,omitempty"`
	Attempts int      `json:"attempts,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Request describes what to compile.
type Request struct {
	Prompt string `json:"prompt"`
	// Tier picks the model tier. Compilation defaults to the quality tier:
	// this call happens once per strategy and being wrong is far more costly
	// than being slow.
	Tier config.Tier `json:"tier,omitempty"`
	// Universe optionally pins the tradable symbols, overriding the model.
	Universe []string `json:"universe,omitempty"`
	// Start and End let the model reason about the period being tested.
	Start market.Day `json:"start,omitempty"`
	End   market.Day `json:"end,omitempty"`
	// MaxAttempts bounds the validate-and-repair loop.
	MaxAttempts int `json:"max_attempts,omitempty"`
}

// Compiler converts prompts to strategies.
type Compiler struct {
	llm   *llm.Client
	store *market.Store
}

// NewCompiler builds a compiler.
func NewCompiler(client *llm.Client, store *market.Store) *Compiler {
	return &Compiler{llm: client, store: store}
}

// Compile produces a validated Plan, repairing the code if the first attempt
// does not run.
func (c *Compiler) Compile(ctx context.Context, req Request) (*Plan, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("describe a strategy in plain language to get started")
	}
	if req.MaxAttempts <= 0 {
		req.MaxAttempts = 3
	}
	tier := req.Tier
	if tier == "" {
		tier = config.TierQuality
	}

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: systemPrompt()},
		{Role: llm.RoleUser, Content: userPrompt(req)},
	}

	var lastErr error
	for attempt := 1; attempt <= req.MaxAttempts; attempt++ {
		temp := 0.1
		resp, err := c.llm.Complete(ctx, llm.Request{
			Tier:        tier,
			Messages:    messages,
			Temperature: &temp,
			MaxTokens:   8000,
			JSONMode:    true,
		})
		if err != nil {
			return nil, fmt.Errorf("the model could not be reached: %w", err)
		}

		plan, err := parsePlan(resp.Text)
		if err != nil {
			lastErr = err
			messages = append(messages,
				llm.Message{Role: llm.RoleAssistant, Content: resp.Text},
				llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf(
					"That reply could not be parsed: %s\n\nReply with a single JSON object and nothing else.", err)})
			continue
		}

		plan.Provider, plan.Model, plan.Attempts = resp.Provider, resp.Model, attempt
		c.applyDefaults(plan, req)

		if problems := c.validate(ctx, plan, req); len(problems) > 0 {
			lastErr = fmt.Errorf("%s", strings.Join(problems, "; "))
			messages = append(messages,
				llm.Message{Role: llm.RoleAssistant, Content: resp.Text},
				llm.Message{Role: llm.RoleUser, Content: repairPrompt(problems)})
			continue
		}
		return plan, nil
	}
	return nil, fmt.Errorf("could not produce a working strategy after %d attempts: %w", req.MaxAttempts, lastErr)
}

// applyDefaults fills in anything the model left out.
func (c *Compiler) applyDefaults(p *Plan, req Request) {
	if len(req.Universe) > 0 {
		p.Universe = market.DedupeSymbols(req.Universe)
	} else {
		// The model may name a built-in universe key rather than tickers.
		var expanded []string
		for _, u := range p.Universe {
			expanded = append(expanded, market.ResolveUniverse(u)...)
		}
		p.Universe = market.DedupeSymbols(expanded)
	}
	if len(p.Universe) == 0 {
		p.Universe = market.ResolveUniverse("megacap")
		p.Assumptions = append(p.Assumptions,
			"No universe was specified, so US mega caps were used.")
	}
	p.Benchmarks = market.DedupeSymbols(p.Benchmarks)
	if len(p.Benchmarks) == 0 {
		p.Benchmarks = []string{"SPY"}
	}
	if p.Warmup <= 0 {
		p.Warmup = 30
	}
	if p.Warmup > 800 {
		p.Warmup = 800
	}
	if strings.TrimSpace(p.Name) == "" {
		p.Name = "Untitled strategy"
	}
}

// validate compiles the strategy and runs it over a short slice of real data.
//
// A syntax check alone is not enough: most failures are runtime ones — calling
// a function that does not exist, or reading a property of a null indicator —
// and those only surface when the code actually executes.
func (c *Compiler) validate(ctx context.Context, p *Plan, req Request) []string {
	var problems []string

	code := strings.TrimSpace(p.Code)
	if code == "" {
		return []string{"the strategy code was empty"}
	}

	if _, err := goja.Compile("strategy.js", code, true); err != nil {
		return []string{"the code has a JavaScript syntax error: " + err.Error()}
	}
	if !strings.Contains(code, "function onDay") && !strings.Contains(code, "onDay =") {
		problems = append(problems, "the code does not define onDay(ctx)")
	}
	// Catch reliance on a runtime that does not exist in the sandbox.
	for _, banned := range []string{"require(", "import ", "fetch(", "process.", "setTimeout(", "XMLHttpRequest"} {
		if strings.Contains(code, banned) {
			problems = append(problems, fmt.Sprintf(
				"the code uses %q, which is not available in the sandbox", strings.TrimSuffix(banned, "(")))
		}
	}
	if len(problems) > 0 {
		return problems
	}

	// Smoke run: a short window is enough to surface runtime errors while
	// keeping validation fast.
	end := req.End
	if end == "" {
		end = market.NewDay(time.Now())
	}
	start := end.Add(-150)
	if req.Start != "" && req.Start > start {
		start = req.Start
	}

	spec := engine.Spec{
		Name:            p.Name,
		Code:            code,
		Universe:        p.Universe,
		Start:           start,
		End:             end,
		InitialCash:     100000,
		AllowShort:      p.AllowShort,
		AllowFractional: true,
		Warmup:          p.Warmup,
	}
	eng := engine.New(spec, c.store)
	// The smoke run must not spend money on model calls, so ai() and web()
	// are left nil; the bindings degrade to null and an empty list.
	eng.MaxAICalls = 0

	smokeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	res, err := eng.Run(smokeCtx)
	if err != nil {
		return []string{"the strategy failed to run: " + err.Error()}
	}
	if res.StrategyErrors > 0 {
		// Report the first distinct error; that is what needs fixing.
		for _, d := range res.Days {
			if d.Error != "" {
				return []string{fmt.Sprintf(
					"the strategy threw an error on %s: %s", d.Date, d.Error)}
			}
		}
	}
	p.Warnings = res.Warnings
	return nil
}

// parsePlan decodes the model's JSON reply, tolerating fenced code blocks and
// surrounding prose.
func parsePlan(raw string) (*Plan, error) {
	text := extractJSONObject(raw)
	var p Plan
	dec := json.NewDecoder(strings.NewReader(text))
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("expected a JSON object: %w", err)
	}
	if strings.TrimSpace(p.Code) == "" {
		return nil, fmt.Errorf("the reply contained no code")
	}
	p.Code = stripCodeFence(p.Code)
	return &p, nil
}

// extractJSONObject finds the outermost {...} in a reply.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	start := strings.Index(s, "{")
	if start < 0 {
		return s
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		ch := s[i]
		switch {
		case esc:
			esc = false
		case ch == '\\' && inStr:
			esc = true
		case ch == '"':
			inStr = !inStr
		case inStr:
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:]
}

// stripCodeFence removes markdown fencing the model may wrap code in.
func stripCodeFence(code string) string {
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(code, "```") {
		return code
	}
	if i := strings.Index(code, "\n"); i >= 0 {
		code = code[i+1:]
	}
	if i := strings.LastIndex(code, "```"); i >= 0 {
		code = code[:i]
	}
	return strings.TrimSpace(code)
}
