package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// callAI implements ctx.ai(prompt, opts).
//
// Every call is recorded against the simulated day, counted against the run
// budget, and cached by (day, prompt) so a re-run costs nothing and returns
// byte-identical answers. Without that caching a five-year daily strategy
// would be neither affordable nor reproducible.
func (v *strategyVM) callAI(call goja.FunctionCall) goja.Value {
	e := v.e
	rt := v.rt

	if e.AI == nil {
		e.warnOnce("no-ai", "ctx.ai() was called but no AI provider is configured; it returned null")
		return goja.Null()
	}
	if len(call.Arguments) == 0 {
		return goja.Null()
	}
	prompt := strings.TrimSpace(call.Argument(0).String())
	if prompt == "" {
		return goja.Null()
	}

	if e.aiCalls >= e.MaxAICalls {
		e.warnOnce("ai-budget", fmt.Sprintf(
			"the run hit its limit of %d AI calls; further ctx.ai() calls return null", e.MaxAICalls))
		return goja.Null()
	}

	opts := AIOptions{}
	wantJSON := false
	if len(call.Arguments) > 1 {
		if m, ok := call.Argument(1).Export().(map[string]any); ok {
			if b, ok := m["json"].(bool); ok {
				opts.JSON, wantJSON = b, b
			}
			if s, ok := m["tier"].(string); ok {
				opts.Tier = s
			}
			if s, ok := m["system"].(string); ok {
				opts.System = s
			}
			if n := toFloat(firstKey(m, "maxTokens", "max_tokens")); n > 0 {
				opts.MaxTokens = int(n)
			}
		}
	}

	e.aiCalls++
	started := time.Now()
	text, provider, model, cached, err := e.AI(e.ctx, e.today, prompt, opts)
	rec := AICall{
		Kind: "ai", Prompt: prompt, Response: text, Provider: provider,
		Model: model, Cached: cached, Millis: time.Since(started).Milliseconds(),
	}
	if err != nil {
		rec.Error = err.Error()
		e.recordAI(rec)
		e.warnOnce("ai-error", "an AI call failed: "+truncateErr(err.Error()))
		return goja.Null()
	}
	e.recordAI(rec)

	if wantJSON {
		var parsed any
		if err := json.Unmarshal([]byte(extractJSON(text)), &parsed); err == nil {
			return rt.ToValue(parsed)
		}
		// Fall through to the raw string rather than failing the day: the
		// strategy can still salvage something from it.
		e.warnOnce("ai-json", "an AI call requested JSON but the reply did not parse; the raw text was returned")
	}
	return rt.ToValue(text)
}

// callSearch implements ctx.web(query, opts) and ctx.news(query, opts).
func (v *strategyVM) callSearch(call goja.FunctionCall, news bool) goja.Value {
	e := v.e
	rt := v.rt

	kind := "web"
	if news {
		kind = "news"
	}
	if e.Search == nil {
		e.warnOnce("no-search", "ctx."+kind+"() was called but web search is disabled; it returned an empty list")
		return rt.ToValue([]any{})
	}
	if len(call.Arguments) == 0 {
		return rt.ToValue([]any{})
	}
	query := strings.TrimSpace(call.Argument(0).String())
	if query == "" {
		return rt.ToValue([]any{})
	}

	if e.aiCalls >= e.MaxAICalls {
		e.warnOnce("ai-budget", fmt.Sprintf(
			"the run hit its limit of %d external calls; further ctx.%s() calls return empty", e.MaxAICalls, kind))
		return rt.ToValue([]any{})
	}

	limit := 5
	if len(call.Arguments) > 1 {
		if m, ok := call.Argument(1).Export().(map[string]any); ok {
			if n := toFloat(firstKey(m, "limit", "count", "n")); n > 0 {
				limit = int(n)
			}
		} else if n := call.Argument(1).ToInteger(); n > 0 {
			limit = int(n)
		}
	}
	if limit > 20 {
		limit = 20
	}

	e.aiCalls++
	started := time.Now()
	results, err := e.Search(e.ctx, e.today, query, limit, news)
	rec := AICall{
		Kind: kind, Prompt: query, Millis: time.Since(started).Milliseconds(),
	}
	if err != nil {
		rec.Error = err.Error()
		e.recordAI(rec)
		e.warnOnce("search-error", "a search call failed: "+truncateErr(err.Error()))
		return rt.ToValue([]any{})
	}

	out := make([]any, 0, len(results))
	var summary strings.Builder
	for i, r := range results {
		out = append(out, map[string]any{
			"title": r.Title, "url": r.URL, "snippet": r.Snippet,
			"published": r.Published, "source": r.Source,
		})
		fmt.Fprintf(&summary, "%d. %s — %s\n", i+1, r.Title, r.Snippet)
	}
	rec.Response = summary.String()
	e.recordAI(rec)
	return rt.ToValue(out)
}

// extractJSON pulls a JSON object or array out of a reply that may be wrapped
// in prose or a fenced code block.
func extractJSON(s string) string {
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
	// Trim to the outermost bracket pair.
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return s
	}
	open := s[start]
	closeCh := byte('}')
	if open == '[' {
		closeCh = ']'
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inStr:
			escaped = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// skip
		case c == open:
			depth++
		case c == closeCh:
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:]
}
