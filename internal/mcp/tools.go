package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/charbelkassab/pyrite/examples"
	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
	"github.com/charbelkassab/pyrite/internal/strategy"
)

// tool is one callable, with the schema a client validates arguments against
// before sending them. call is unexported so it does not reach the wire.
type tool struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description"`
	InputSchema schema `json:"inputSchema"`

	call func(ctx context.Context, args json.RawMessage) (any, error)
}

// schema and property are the subset of JSON Schema a tool input needs.
// Writing them as types rather than as literal maps means a malformed schema
// is a compile error, and the shape stays legible next to the descriptions.
//
// There is no required list because no field is unconditionally required: a
// strategy arrives as either code or example, and a schema that demanded one of
// them would refuse the other. The pair is checked when the call runs, with a
// message saying which to send.
type schema struct {
	Type       string              `json:"type"`
	Properties map[string]property `json:"properties"`
}

type property struct {
	Type                 string    `json:"type,omitempty"`
	Description          string    `json:"description,omitempty"`
	Items                *property `json:"items,omitempty"`
	Enum                 []string  `json:"enum,omitempty"`
	AdditionalProperties *property `json:"additionalProperties,omitempty"`
}

// text marks a tool result that is prose rather than data, so it travels as a
// text block with no structured copy alongside it.
type text string

// callParams is the tools/call request body.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolResult struct {
	Content           []contentBlock `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *Server) callTool(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p callParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams("could not read the tools/call parameters: %v", err)
	}
	var t *tool
	for _, candidate := range s.tools {
		if candidate.Name == p.Name {
			t = candidate
			break
		}
	}
	if t == nil {
		return nil, invalidParams("no tool named %q. Available: %s", p.Name, strings.Join(s.toolNames(), ", "))
	}

	out, err := t.call(ctx, p.Arguments)
	if err != nil {
		// A bad argument is a protocol error the client should not have sent.
		// Anything else is a real answer to the question asked — a strategy
		// that threw, a symbol with no data — and the agent needs to read it
		// and try again, which it can only do if it arrives as a result.
		var rerr *rpcError
		if errors.As(err, &rerr) {
			return nil, rerr
		}
		return &toolResult{
			Content: []contentBlock{{Type: "text", Text: err.Error()}},
			IsError: true,
		}, nil
	}

	if prose, ok := out.(text); ok {
		return &toolResult{Content: []contentBlock{{Type: "text", Text: string(prose)}}}, nil
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, &rpcError{Code: codeInternal,
			Message: fmt.Sprintf("the %s result could not be encoded: %v", p.Name, err)}
	}
	// The same payload twice: structured for a client that can use it, and
	// serialised as text for one that cannot.
	return &toolResult{
		Content:           []contentBlock{{Type: "text", Text: string(body)}},
		StructuredContent: out,
	}, nil
}

func (s *Server) toolNames() []string {
	out := make([]string, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t.Name)
	}
	return out
}

// decodeArgs reads tool arguments strictly.
//
// Unknown fields are rejected rather than ignored, because the caller is a
// model: a plausible-looking misspelling that is silently dropped produces a
// run with the wrong period or the wrong costs and no indication that anything
// was ignored. An error it can read and correct is worth more.
func decodeArgs(raw json.RawMessage, into any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return invalidParams("could not read the arguments: %v", err)
	}
	return nil
}

func (s *Server) toolset() []*tool {
	return []*tool{
		{
			Name:  "backtest",
			Title: "Run one backtest",
			Description: "Run a JavaScript strategy over a symbol list and a period. Returns the " +
				"headline metrics, a trust score out of 100 and the critique: the specific, " +
				"computed objections to the result, with the numbers behind them. Read the " +
				"critique before the metrics.",
			InputSchema: schema{Type: "object", Properties: strategyProperties()},
			call: func(ctx context.Context, args json.RawMessage) (any, error) {
				var in strategyInput
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return s.runBacktest(ctx, in)
			},
		},
		{
			Name:  "sweep",
			Title: "Search the parameter space",
			Description: "Run every combination of a strategy's declared parameters and rank them. " +
				"Returns the top rows, the winning run with its critique, and the robustness " +
				"block: deflated Sharpe, probability of backtest overfitting, and whether the " +
				"winner sits on a plateau or alone on a spike. The best cell of a large search " +
				"is usually noise, and these numbers say how much.",
			InputSchema: schema{
				Type: "object",
				Properties: mergeProperties(strategyProperties(), map[string]property{
					"grids": {
						Type: "object",
						Description: "Override the grids the strategy declared, e.g. " +
							"{\"fast\": [10, 20, 50]}. A parameter absent here keeps its own declaration.",
						AdditionalProperties: &property{Type: "array"},
					},
					"objective": {
						Type:        "string",
						Description: "Metric rows are ranked by. Default sharpe.",
						Enum:        engine.ObjectiveNames(),
					},
					"max_combos": {Type: "integer", Description: "Refuse a search larger than this. Default 5000."},
					"top":        {Type: "integer", Description: "How many ranked rows to return. Default 10."},
				}),
			},
			call: func(ctx context.Context, args json.RawMessage) (any, error) {
				var in sweepInput
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return s.runSweep(ctx, in)
			},
		},
		{
			Name:  "walkforward",
			Title: "Choose on one period, report on the next",
			Description: "Roll a train/test window through the data: parameters are chosen on the " +
				"training window and applied unchanged to the window after it. Returns the " +
				"out-of-sample efficiency, the per-fold results and the verdict. This is the " +
				"only equity curve in the tool that was never fitted to.",
			InputSchema: schema{
				Type: "object",
				Properties: mergeProperties(strategyProperties(), map[string]property{
					"grids": {
						Type:                 "object",
						Description:          "Override the grids the strategy declared, e.g. {\"fast\": [10, 20, 50]}.",
						AdditionalProperties: &property{Type: "array"},
					},
					"objective":  {Type: "string", Description: "Metric each fold is optimised for. Default sharpe.", Enum: engine.ObjectiveNames()},
					"train_days": {Type: "integer", Description: "Training window in trading sessions. Default 504."},
					"test_days":  {Type: "integer", Description: "Test window in trading sessions. Default 126."},
					"embargo": {Type: "integer", Description: "Sessions dropped between train and test. " +
						"Defaults to the strategy's warm-up, which is the horizon over which a long " +
						"indicator would otherwise carry training data into the test window."},
					"anchored":   {Type: "boolean", Description: "Grow the training window from the start instead of rolling it."},
					"max_combos": {Type: "integer", Description: "Refuse a per-fold search larger than this. Default 5000."},
				}),
			},
			call: func(ctx context.Context, args json.RawMessage) (any, error) {
				var in walkForwardInput
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return s.runWalkForward(ctx, in)
			},
		},
		{
			Name:  "list_examples",
			Title: "List the bundled strategies",
			Description: "List the strategies compiled into the binary. They run with no API key " +
				"and are the fastest way to see a working strategy. Pass a name to get one " +
				"back with its source, ready to edit and pass to backtest.",
			InputSchema: schema{
				Type: "object",
				Properties: map[string]property{
					"name": {Type: "string", Description: "Return this one example including its source code."},
				},
			},
			call: func(ctx context.Context, args json.RawMessage) (any, error) {
				var in struct {
					Name string `json:"name"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return listExamples(in.Name)
			},
		},
		{
			Name:  "strategy_api",
			Title: "Strategy API reference",
			Description: "The reference for the JavaScript a strategy is written in: every function " +
				"on ctx, the indicators, order types and the shape setup() and onDay() take. " +
				"Read this before writing a strategy.",
			InputSchema: schema{Type: "object", Properties: map[string]property{}},
			call: func(ctx context.Context, args json.RawMessage) (any, error) {
				var in struct{}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return text(strategy.APIReference()), nil
			},
		},
	}
}

// strategyProperties are the inputs every backtest-shaped tool accepts. They
// live in one place because the three tools must describe the same field the
// same way; an agent that learns "start" from one and finds it means something
// else in another has been misled by the schema.
func strategyProperties() map[string]property {
	return map[string]property{
		"code": {Type: "string", Description: "The strategy as JavaScript: a setup(ctx) and an onDay(ctx). " +
			"Call strategy_api for what is available inside them. Required unless example is given."},
		"example": {Type: "string", Description: "Run a bundled strategy by name instead of supplying code. " +
			"list_examples has the names."},
		"name": {Type: "string", Description: "A label for the run."},
		"universe": {Type: "array", Items: &property{Type: "string"},
			Description: "Symbols to trade, or a universe name: megacap, tech, dow, sectors, or sp500 " +
				"for point-in-time index membership. Omit if setup() names its own."},
		"benchmarks": {Type: "array", Items: &property{Type: "string"},
			Description: "Symbols to compare against. Default SPY."},
		"start":        {Type: "string", Description: "First day, YYYY-MM-DD. Omit to start as early as the data allows."},
		"end":          {Type: "string", Description: "Last day, YYYY-MM-DD. Default today."},
		"initial_cash": {Type: "number", Description: "Starting capital. Default 100000."},
		"interval": {Type: "string", Description: "Bar size. Default 1d. Intraday history from free vendors " +
			"reaches back weeks, not years.", Enum: market.IntervalNames()},
		"fill": {Type: "string", Enum: []string{string(engine.FillNextOpen), string(engine.FillClose)},
			Description: "Where orders fill. Default next_open. close lets a strategy trade at a price " +
				"it has already seen, which is lookahead bias and is reported as such."},
		"slippage_bps":   {Type: "number", Description: "Slippage charged on every fill, in basis points. Default 5."},
		"commission_pct": {Type: "number", Description: "Commission as a fraction of notional, e.g. 0.0005."},
		"impact": {Type: "number", Description: "Market impact coefficient; 1 is the usual empirical estimate, " +
			"0 disables the model. Without it, position size is free."},
		"allow_short":    {Type: "boolean", Description: "Permit short positions."},
		"warmup":         {Type: "integer", Description: "Bars of history loaded before the start date so indicators are valid on day one."},
		"risk_free_rate": {Type: "number", Description: "Annual risk-free rate as a fraction, used by Sharpe and Sortino."},
		"params":         {Type: "object", Description: "Values for the tunables the strategy declared with ctx.param()."},
	}
}

func mergeProperties(base, extra map[string]property) map[string]property {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

// listExamples returns the bundled strategies, or one of them with its source.
func listExamples(name string) (any, error) {
	type entry struct {
		Name       string   `json:"name"`
		Title      string   `json:"title,omitempty"`
		Summary    string   `json:"summary,omitempty"`
		Universe   []string `json:"universe,omitempty"`
		Benchmarks []string `json:"benchmarks,omitempty"`
		Warmup     int      `json:"warmup,omitempty"`
		AllowShort bool     `json:"allow_short,omitempty"`
		NeedsModel bool     `json:"needs_model,omitempty"`
		Code       string   `json:"code,omitempty"`
	}
	convert := func(ex examples.Example, withCode bool) entry {
		e := entry{
			Name: ex.Name, Title: ex.Title, Summary: ex.Summary,
			Universe: ex.Universe, Benchmarks: ex.Benchmarks, Warmup: ex.Warmup,
			AllowShort: ex.AllowShort, NeedsModel: ex.NeedsModel,
		}
		if withCode {
			e.Code = ex.Code
		}
		return e
	}

	if name != "" {
		ex, err := examples.Get(name)
		if err != nil {
			return nil, invalidParams("%v", err)
		}
		return map[string]any{"examples": []entry{convert(ex, true)}}, nil
	}
	all := examples.All()
	out := make([]entry, 0, len(all))
	for _, ex := range all {
		// The source is omitted from the listing on purpose: nine strategies
		// of JavaScript is a large answer to "what is available", and
		// list_examples with a name returns the one that is wanted.
		out = append(out, convert(ex, false))
	}
	return map[string]any{"examples": out}, nil
}
