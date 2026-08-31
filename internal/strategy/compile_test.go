package strategy

import (
	"strings"
	"testing"
)

// Everything in this file runs with no API key and no network. The corpus
// test beside it is the real regression suite for compilation, but it costs
// money and only runs on demand — which left the reply-parsing layer, the
// part that has to survive whatever a model actually emits, with no routine
// coverage at all.

func TestParsePlanAcceptsTheShapesModelsActuallyReturn(t *testing.T) {
	const code = `function onDay(ctx) { ctx.log("hi"); }`

	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "a bare object",
			raw:  `{"name":"Test","code":` + quote(code) + `}`,
		},
		{
			name: "wrapped in a json code fence",
			raw:  "```json\n{\"name\":\"Test\",\"code\":" + quote(code) + "}\n```",
		},
		{
			name: "wrapped in an unlabelled fence",
			raw:  "```\n{\"name\":\"Test\",\"code\":" + quote(code) + "}\n```",
		},
		{
			name: "with prose before and after",
			raw: "Sure! Here is the strategy you asked for:\n\n" +
				`{"name":"Test","code":` + quote(code) + "}\n\nLet me know if you want changes.",
		},
		{
			name: "code itself fenced inside the json",
			raw:  `{"name":"Test","code":` + quote("```js\n"+code+"\n```") + `}`,
		},
		{
			name: "nested objects before the code field",
			raw:  `{"name":"Test","params":{"fast":{"grid":[1,2]}},"code":` + quote(code) + `}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parsePlan(tc.raw)
			if err != nil {
				t.Fatalf("parsePlan: %v", err)
			}
			if p.Code != code {
				t.Errorf("code = %q, want %q", p.Code, code)
			}
			if p.Name != "Test" {
				t.Errorf("name = %q", p.Name)
			}
		})
	}
}

func TestParsePlanRejectsRepliesWithNothingToRun(t *testing.T) {
	for _, raw := range []string{
		"",
		"I cannot help with that.",
		`{"name":"Test"}`,
		`{"name":"Test","code":"   "}`,
		`{"name":"Test","code":`, // truncated mid-object
	} {
		if p, err := parsePlan(raw); err == nil {
			t.Errorf("parsePlan(%.40q) returned a plan with code %q, want an error", raw, p.Code)
		}
	}
}

// A brace inside a string literal must not be mistaken for the end of the
// object. Strategy code is full of them, so getting this wrong truncates the
// reply at the first `}` in a comment or a string.
func TestExtractJSONObjectIgnoresBracesInsideStrings(t *testing.T) {
	raw := `prefix {"code":"if (x) { y(); } // }","name":"T"} suffix`
	got := extractJSONObject(raw)
	want := `{"code":"if (x) { y(); } // }","name":"T"}`
	if got != want {
		t.Errorf("extractJSONObject:\n got %s\nwant %s", got, want)
	}
}

func TestExtractJSONObjectHandlesEscapedQuotes(t *testing.T) {
	raw := `{"code":"ctx.log(\"}\");","name":"T"}`
	if got := extractJSONObject(raw); got != raw {
		t.Errorf("an escaped quote broke extraction:\n got %s\nwant %s", got, raw)
	}
}

func TestStripCodeFence(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"function onDay() {}", "function onDay() {}"},
		{"```js\nfunction onDay() {}\n```", "function onDay() {}"},
		{"```javascript\nfunction onDay() {}\n```", "function onDay() {}"},
		{"```\nfunction onDay() {}\n```", "function onDay() {}"},
		{"  \n```js\nfunction onDay() {}\n```  \n", "function onDay() {}"},
	} {
		if got := stripCodeFence(tc.in); got != tc.want {
			t.Errorf("stripCodeFence(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The sandbox has no module loader, no network and no timers. Code that
// reaches for them fails at runtime with a message about an undefined
// variable, which tells the model nothing useful — so the compiler names the
// missing capability instead, and that message is what gets fed back for a
// repair attempt.
func TestValidateNamesTheMissingCapability(t *testing.T) {
	c := &Compiler{}
	// `import` is deliberately absent from this table: it is a reserved word,
	// so the syntax check rejects it before the capability check is reached
	// and the specific message never fires. The code is still refused, which
	// is what matters — see TestImportIsRejectedAsASyntaxError.
	for _, tc := range []struct{ code, want string }{
		{`const x = require("fs"); function onDay(ctx) {}`, "require"},
		{`function onDay(ctx) { fetch("http://x"); }`, "fetch"},
		{`function onDay(ctx) { setTimeout(function(){}, 10); }`, "setTimeout"},
		{`function onDay(ctx) { process.exit(1); }`, "process."},
	} {
		problems := c.validate(t.Context(), &Plan{Code: tc.code}, Request{})
		if len(problems) == 0 {
			t.Errorf("%.40q was accepted", tc.code)
			continue
		}
		if !strings.Contains(strings.Join(problems, " "), strings.TrimSuffix(tc.want, "(")) {
			t.Errorf("%.40q reported %v, expected it to mention %q", tc.code, problems, tc.want)
		}
	}
}

// ES module syntax is rejected, just not by the capability check: goja treats
// `import` as a reserved word and fails to parse. Worth pinning, because the
// two paths produce different messages and only this one is reachable.
func TestImportIsRejectedAsASyntaxError(t *testing.T) {
	c := &Compiler{}
	problems := c.validate(t.Context(), &Plan{Code: `import x from "y"; function onDay(ctx) {}`}, Request{})
	if len(problems) == 0 {
		t.Fatal("ES module syntax was accepted")
	}
	if !strings.Contains(problems[0], "syntax error") {
		t.Errorf("expected a syntax error, got %v", problems)
	}
}

func TestValidateRejectsCodeWithoutOnDay(t *testing.T) {
	c := &Compiler{}
	problems := c.validate(t.Context(), &Plan{Code: `function setup(ctx) {}`}, Request{})
	if len(problems) == 0 {
		t.Fatal("code with no onDay was accepted")
	}
	if !strings.Contains(strings.Join(problems, " "), "onDay") {
		t.Errorf("problems did not mention onDay: %v", problems)
	}
}

func TestValidateReportsSyntaxErrorsAsSuch(t *testing.T) {
	c := &Compiler{}
	problems := c.validate(t.Context(), &Plan{Code: `function onDay(ctx) { if (`}, Request{})
	if len(problems) != 1 || !strings.Contains(problems[0], "syntax error") {
		t.Errorf("expected a single syntax-error problem, got %v", problems)
	}
}

func TestValidateRejectsEmptyCode(t *testing.T) {
	c := &Compiler{}
	if problems := c.validate(t.Context(), &Plan{Code: "   \n\t"}, Request{}); len(problems) == 0 {
		t.Error("empty code was accepted")
	}
}

// A plan with no universe must still be runnable, and must say what it
// assumed rather than silently picking one.
func TestApplyDefaultsFillsTheGapsAndSaysSo(t *testing.T) {
	c := &Compiler{}

	p := &Plan{Code: "x"}
	c.applyDefaults(p, Request{})
	if len(p.Universe) == 0 {
		t.Fatal("no universe was chosen")
	}
	if len(p.Assumptions) == 0 {
		t.Error("a universe was invented without recording the assumption")
	}
	if len(p.Benchmarks) != 1 || p.Benchmarks[0] != "SPY" {
		t.Errorf("benchmarks = %v, want [SPY]", p.Benchmarks)
	}
	if p.Warmup != 30 {
		t.Errorf("warmup = %d, want 30", p.Warmup)
	}
	if p.Name == "" {
		t.Error("an unnamed plan kept an empty name")
	}

	// A caller-supplied universe wins over whatever the model chose.
	p = &Plan{Code: "x", Universe: []string{"NVDA"}}
	c.applyDefaults(p, Request{Universe: []string{"aapl", "AAPL", "msft"}})
	if len(p.Universe) != 2 || p.Universe[0] != "AAPL" || p.Universe[1] != "MSFT" {
		t.Errorf("universe = %v, want [AAPL MSFT] deduped and normalised", p.Universe)
	}

	// A named built-in universe is expanded to its symbols.
	p = &Plan{Code: "x", Universe: []string{"megacap"}}
	c.applyDefaults(p, Request{})
	if len(p.Universe) < 5 {
		t.Errorf("megacap expanded to only %v", p.Universe)
	}

	// Warm-up is clamped: an 800-bar indicator is already implausible, and a
	// larger one silently eats the whole test window.
	p = &Plan{Code: "x", Warmup: 5000}
	c.applyDefaults(p, Request{})
	if p.Warmup != 800 {
		t.Errorf("warmup = %d, want it clamped to 800", p.Warmup)
	}
}

// The reference the model writes against is the same document a user reads.
// If it ever ships empty the model loses its entire API description, and the
// failure would look like the model getting worse rather than an asset going
// missing.
func TestAPIReferenceIsEmbedded(t *testing.T) {
	ref := APIReference()
	if len(ref) < 2000 {
		t.Fatalf("the API reference is only %d bytes", len(ref))
	}
	for _, must := range []string{"ctx.buy", "ctx.sma", "onDay", "setup"} {
		if !strings.Contains(ref, must) {
			t.Errorf("the API reference never mentions %q", must)
		}
	}
}

// quote renders s as a JSON string literal.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
