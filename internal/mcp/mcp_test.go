package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/charbelkassab/pyrite/internal/app"
	"github.com/charbelkassab/pyrite/internal/config"
)

// newTestServer builds a server backed by a throwaway data directory and the
// synthetic provider.
//
// Offline mode is what makes these tests worth having in CI: the whole
// protocol surface, up to and including a real backtest with a real critique,
// is reachable with no network, no API key and no market vendor.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.OfflineMode = true

	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	return New(a, "test")
}

// frame is one message off the wire, kept deliberately loose so these tests
// assert on the JSON-RPC contract rather than on internal types.
type frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// session drives a whole Serve call the way a client does: lines in, lines
// out. Every test goes through this rather than calling handlers directly, so
// the framing is exercised too.
func session(t *testing.T, s *Server, requests ...string) ([]frame, string) {
	t.Helper()
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")
	var out bytes.Buffer
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}

	var frames []frame
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var f frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("a line on stdout was not a protocol frame: %v\nline: %.300s", err, line)
		}
		frames = append(frames, f)
	}
	return frames, out.String()
}

const initRequest = `{"jsonrpc":"2.0","id":1,"method":"initialize",` +
	`"params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`

// call builds a tools/call request.
func call(id int, name, args string) string {
	if args == "" {
		args = "{}"
	}
	req := map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": json.RawMessage(args)},
	}
	b, err := json.Marshal(req)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// result decodes the payload of the frame with this id, failing on a protocol
// error, since every test that asks for one expects the call to have worked.
func result(t *testing.T, frames []frame, id int, into any) {
	t.Helper()
	for _, f := range frames {
		if f.ID == nil || *f.ID != id {
			continue
		}
		if f.Error != nil {
			t.Fatalf("request %d failed: %d %s", id, f.Error.Code, f.Error.Message)
		}
		if err := json.Unmarshal(f.Result, into); err != nil {
			t.Fatalf("decode result %d: %v\n%.400s", id, err, f.Result)
		}
		return
	}
	t.Fatalf("no response to request %d", id)
}

// toolCallResult is the shape of a tools/call reply.
type toolCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

func TestInitializeHandshake(t *testing.T) {
	s := newTestServer(t)
	frames, _ := session(t, s, initRequest)

	var got struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools *struct {
				ListChanged bool `json:"listChanged"`
			} `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	result(t, frames, 1, &got)

	if got.ProtocolVersion != "2025-06-18" {
		t.Errorf("protocol version %q, want the one the client asked for", got.ProtocolVersion)
	}
	if got.Capabilities.Tools == nil {
		t.Error("the handshake did not advertise the tools capability, so no client will call one")
	}
	if got.ServerInfo.Name != "pyrite" || got.ServerInfo.Version != "test" {
		t.Errorf("serverInfo = %+v", got.ServerInfo)
	}
	// The instructions are where a calling model is told the critique is the
	// point. Losing them silently turns this into an ordinary backtester.
	if !strings.Contains(got.Instructions, "critique") {
		t.Errorf("the instructions do not mention the critique: %.200s", got.Instructions)
	}
}

// A client asking for a revision this server does not know gets ours back,
// rather than an agreement to speak something unimplemented.
func TestUnknownProtocolVersionFallsBackToOurs(t *testing.T) {
	s := newTestServer(t)
	frames, _ := session(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)

	var got struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	result(t, frames, 1, &got)
	if got.ProtocolVersion != protocolVersion {
		t.Errorf("protocol version %q, want %q", got.ProtocolVersion, protocolVersion)
	}
}

func TestToolsListReturnsUsableSchemas(t *testing.T) {
	s := newTestServer(t)
	frames, _ := session(t, s, initRequest,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)

	var got struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			InputSchema struct {
				Type       string `json:"type"`
				Properties map[string]struct {
					Type        string `json:"type"`
					Description string `json:"description"`
				} `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	result(t, frames, 2, &got)

	want := map[string]bool{
		"backtest": false, "sweep": false, "walkforward": false,
		"list_examples": false, "strategy_api": false,
	}
	for _, tool := range got.Tools {
		if _, ok := want[tool.Name]; !ok {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		want[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("%s has no description, so a model has to guess what it does", tool.Name)
		}
		if tool.InputSchema.Type != "object" {
			t.Errorf("%s input schema is %q, want object", tool.Name, tool.InputSchema.Type)
		}
		if tool.InputSchema.Properties == nil {
			t.Errorf("%s has no properties member, which is not a valid object schema", tool.Name)
		}
		// A property with no type is not something a client can validate
		// against, and one with no description is a field a model will fill
		// in from its name alone.
		for name, p := range tool.InputSchema.Properties {
			if p.Type == "" {
				t.Errorf("%s.%s has no type", tool.Name, name)
			}
			if p.Description == "" {
				t.Errorf("%s.%s has no description", tool.Name, name)
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %q was not listed", name)
		}
	}
}

// The round trip that matters: a real offline backtest, returning numbers and
// the objections to them in the same payload.
func TestBacktestRoundTripReturnsACritique(t *testing.T) {
	s := newTestServer(t)
	args := `{
		"name": "buy and hold",
		"code": "function setup(ctx){ctx.universe([\"SPY\"]);}\nfunction onDay(ctx){if(!ctx.hasPosition(\"SPY\"))ctx.buy(\"SPY\",{pctCash:1});}",
		"universe": ["SPY"],
		"start": "2021-01-04",
		"end": "2021-12-30"
	}`
	frames, _ := session(t, s, initRequest, call(2, "backtest", args))

	var res toolCallResult
	result(t, frames, 2, &res)
	if res.IsError {
		t.Fatalf("the backtest reported an error: %.400s", res.Content[0].Text)
	}

	var payload struct {
		TrustScore int `json:"trust_score"`
		Headline   string
		Critique   []struct {
			Severity string `json:"severity"`
			Title    string `json:"title"`
			Detail   string `json:"detail"`
		} `json:"critique"`
		Metrics struct {
			TotalReturn *float64 `json:"total_return"`
			Sharpe      *float64 `json:"sharpe"`
			TradingDays int      `json:"trading_days"`
		} `json:"metrics"`
		Benchmarks []struct {
			Label string `json:"label"`
		} `json:"benchmarks"`
		Data struct {
			Provider string `json:"provider"`
			Offline  bool   `json:"offline"`
			Note     string `json:"note"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.StructuredContent, &payload); err != nil {
		t.Fatalf("decode the structured result: %v", err)
	}

	if payload.Metrics.TradingDays == 0 {
		t.Error("the backtest reported no trading days, so nothing was simulated")
	}
	if payload.Metrics.TotalReturn == nil {
		t.Error("no total return in the result")
	}
	// The critique is the product. A result without one is a failure even
	// when the backtest itself succeeded.
	if len(payload.Critique) == 0 {
		t.Fatal("the result carried no critique findings")
	}
	for _, f := range payload.Critique {
		if f.Title == "" || f.Detail == "" || f.Severity == "" {
			t.Errorf("an incomplete finding reached the caller: %+v", f)
		}
	}
	if payload.Headline == "" {
		t.Error("the result carried no headline objection")
	}
	if payload.TrustScore < 0 || payload.TrustScore > 100 {
		t.Errorf("trust score %d is outside 0-100", payload.TrustScore)
	}
	if len(payload.Benchmarks) == 0 {
		t.Error("no benchmark was reported, so there is nothing to judge the return against")
	}
	// Synthetic prices produce a real-looking Sharpe from data that is not the
	// market, and the payload is the only place an agent could learn that.
	if !payload.Data.Offline || payload.Data.Note == "" {
		t.Errorf("an offline run did not say so: %+v", payload.Data)
	}

	// The text block exists for a client that cannot read structured content,
	// and it must carry the same thing rather than only the numbers.
	if len(res.Content) == 0 || res.Content[0].Type != "text" {
		t.Fatalf("no text content block: %+v", res.Content)
	}
	if !strings.Contains(res.Content[0].Text, payload.Critique[0].Title) {
		t.Error("the text block did not include the critique")
	}
}

// A search must come back with the overfitting statistics and with the
// winner's own critique, because "the best cell scored 1.4" is exactly the
// claim those numbers exist to qualify.
func TestSweepReturnsRobustnessAndTheWinnersCritique(t *testing.T) {
	s := newTestServer(t)
	args := `{
		"code": "function setup(ctx){ctx.universe([\"SPY\"]);ctx.param(\"n\",10,{grid:[5,10,20]});ctx.warmup(25);}\nfunction onDay(ctx){var m=ctx.sma(\"SPY\",ctx.params.n);if(m===null)return;if(ctx.price(\"SPY\")>m){if(!ctx.hasPosition(\"SPY\"))ctx.buy(\"SPY\",{pctCash:1});}else if(ctx.hasPosition(\"SPY\"))ctx.close(\"SPY\");}",
		"universe": ["SPY"],
		"start": "2021-01-04",
		"end": "2021-12-30",
		"top": 2
	}`
	frames, _ := session(t, s, initRequest, call(2, "sweep", args))

	var res toolCallResult
	result(t, frames, 2, &res)
	if res.IsError {
		t.Fatalf("the sweep reported an error: %.400s", res.Content[0].Text)
	}

	var payload struct {
		Combos  int `json:"combos"`
		TopRows []struct {
			Label string   `json:"label"`
			Score *float64 `json:"score"`
		} `json:"top_rows"`
		Robustness struct {
			Trials         int      `json:"trials"`
			DeflatedSharpe *float64 `json:"deflated_sharpe"`
			PBO            *float64 `json:"pbo"`
			PlateauRatio   *float64 `json:"plateau_ratio"`
			Verdict        string   `json:"verdict"`
		} `json:"robustness"`
		Best *struct {
			TrustScore int               `json:"trust_score"`
			Critique   []json.RawMessage `json:"critique"`
		} `json:"best"`
	}
	if err := json.Unmarshal(res.StructuredContent, &payload); err != nil {
		t.Fatalf("decode the structured result: %v", err)
	}

	if payload.Combos != 3 {
		t.Errorf("searched %d combinations, want 3", payload.Combos)
	}
	if len(payload.TopRows) != 2 {
		t.Errorf("returned %d rows, want the 2 asked for", len(payload.TopRows))
	}
	if payload.Robustness.Trials != 3 {
		t.Errorf("robustness measured %d trials, want 3", payload.Robustness.Trials)
	}
	if payload.Robustness.Verdict == "" {
		t.Error("the search came back without a verdict")
	}
	if payload.Best == nil || len(payload.Best.Critique) == 0 {
		t.Error("the winning combination arrived without its critique")
	}
}

// Walk-forward is the one number that means what it appears to, so the verdict
// and the efficiency both have to arrive.
func TestWalkForwardReturnsEfficiencyAndAVerdict(t *testing.T) {
	s := newTestServer(t)
	args := `{
		"code": "function setup(ctx){ctx.universe([\"SPY\"]);ctx.param(\"n\",10,{grid:[5,20]});ctx.warmup(25);}\nfunction onDay(ctx){var m=ctx.sma(\"SPY\",ctx.params.n);if(m===null)return;if(ctx.price(\"SPY\")>m){if(!ctx.hasPosition(\"SPY\"))ctx.buy(\"SPY\",{pctCash:1});}else if(ctx.hasPosition(\"SPY\"))ctx.close(\"SPY\");}",
		"universe": ["SPY"],
		"start": "2019-01-02",
		"end": "2021-12-30",
		"train_days": 250,
		"test_days": 120
	}`
	frames, _ := session(t, s, initRequest, call(2, "walkforward", args))

	var res toolCallResult
	result(t, frames, 2, &res)
	if res.IsError {
		t.Fatalf("the walk-forward reported an error: %.400s", res.Content[0].Text)
	}

	var payload struct {
		Verdict    string   `json:"verdict"`
		Efficiency *float64 `json:"efficiency"`
		TotalFolds int      `json:"total_folds"`
		Folds      []struct {
			TestStart string `json:"test_start"`
			TestEnd   string `json:"test_end"`
		} `json:"folds"`
	}
	if err := json.Unmarshal(res.StructuredContent, &payload); err != nil {
		t.Fatalf("decode the structured result: %v", err)
	}
	if payload.TotalFolds == 0 || len(payload.Folds) == 0 {
		t.Fatal("no folds were evaluated")
	}
	if payload.Verdict == "" {
		t.Error("the evaluation came back without a verdict")
	}
	if payload.Efficiency == nil {
		t.Error("no out-of-sample efficiency was reported")
	}
	for i, f := range payload.Folds {
		if f.TestStart == "" || f.TestEnd == "" {
			t.Errorf("fold %d has no test window", i)
		}
	}
}

func TestListExamplesAndStrategyAPI(t *testing.T) {
	s := newTestServer(t)
	frames, _ := session(t, s, initRequest,
		call(2, "list_examples", `{}`),
		call(3, "list_examples", `{"name":"golden-cross"}`),
		call(4, "strategy_api", `{}`))

	var listed toolCallResult
	result(t, frames, 2, &listed)
	var all struct {
		Examples []struct {
			Name string `json:"name"`
			Code string `json:"code"`
		} `json:"examples"`
	}
	if err := json.Unmarshal(listed.StructuredContent, &all); err != nil {
		t.Fatalf("decode the listing: %v", err)
	}
	if len(all.Examples) < 5 {
		t.Fatalf("only %d bundled examples were listed", len(all.Examples))
	}
	for _, ex := range all.Examples {
		if ex.Code != "" {
			t.Errorf("the listing carried the source of %s, which nobody asked for", ex.Name)
		}
	}

	var one toolCallResult
	result(t, frames, 3, &one)
	if err := json.Unmarshal(one.StructuredContent, &all); err != nil {
		t.Fatalf("decode the single example: %v", err)
	}
	if len(all.Examples) != 1 || !strings.Contains(all.Examples[0].Code, "function onDay") {
		t.Error("asking for one example did not return it with runnable code")
	}

	// The API reference is prose, so it travels as text with no structured
	// copy: a JSON string of a markdown document helps nobody.
	var api toolCallResult
	result(t, frames, 4, &api)
	if len(api.Content) == 0 || !strings.Contains(api.Content[0].Text, "ctx.") {
		t.Error("the strategy API reference did not come back")
	}
	if len(api.StructuredContent) != 0 {
		t.Errorf("the reference was wrapped in structured content: %.100s", api.StructuredContent)
	}
}

// Everything a client can get wrong must come back as a JSON-RPC error it can
// act on, rather than a crash or a silence.
func TestProtocolErrors(t *testing.T) {
	s := newTestServer(t)
	frames, _ := session(t, s,
		initRequest,
		`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`,
		call(4, "backtest", `{"code":"function onDay(ctx){}","start":"not-a-date"}`),
		call(5, "backtest", `{"cod":"a plausible misspelling"}`),
		call(6, "backtest", `{}`),
		`{"jsonrpc":"2.0","id":7,"method":`,
		`{"jsonrpc":"2.0","id":8,"method":"ping"}`,
	)

	byID := map[int]frame{}
	var parseErrors int
	for _, f := range frames {
		if f.ID == nil {
			if f.Error != nil && f.Error.Code == codeParse {
				parseErrors++
			}
			continue
		}
		byID[*f.ID] = f
	}

	for _, tc := range []struct {
		id   int
		code int
		name string
	}{
		{2, codeMethodNotFound, "a method this server does not implement"},
		{3, codeInvalidParams, "a tool that does not exist"},
		{4, codeInvalidParams, "a date that is not a date"},
		{5, codeInvalidParams, "a misspelled argument"},
		{6, codeInvalidParams, "no strategy at all"},
	} {
		f, ok := byID[tc.id]
		if !ok {
			t.Errorf("%s got no response", tc.name)
			continue
		}
		if f.Error == nil {
			t.Errorf("%s was accepted", tc.name)
			continue
		}
		if f.Error.Code != tc.code {
			t.Errorf("%s = %d, want %d (%s)", tc.name, f.Error.Code, tc.code, f.Error.Message)
		}
		// An error a model cannot act on is as good as no error.
		if len(f.Error.Message) < 20 {
			t.Errorf("%s came back with an unhelpful message: %q", tc.name, f.Error.Message)
		}
	}

	if parseErrors != 1 {
		t.Errorf("%d parse errors reported, want exactly 1", parseErrors)
	}
	// A malformed line must not end the session: the specification requires
	// answering it and carrying on, and a client that has to reconnect after
	// every bad frame is unusable.
	if f, ok := byID[8]; !ok || f.Error != nil {
		t.Error("the request after a malformed line was not served")
	}
}

// JSON-RPC forbids answering a notification, and a client that gets a response
// with no id it recognises reports it as a protocol violation.
func TestNotificationsAreNotAnswered(t *testing.T) {
	s := newTestServer(t)
	frames, out := session(t, s,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		`{"jsonrpc":"2.0","method":"tools/list"}`)

	if len(frames) != 0 {
		t.Errorf("a notification was answered: %.300s", out)
	}
}

// stdout carries protocol frames and nothing else. A single stray line
// desynchronises the stream, and the client reports a parse error with no clue
// where it came from — the classic way to break an MCP server.
func TestOnlyProtocolFramesReachStdout(t *testing.T) {
	s := newTestServer(t)
	args := `{
		"code": "function setup(ctx){ctx.universe([\"SPY\",\"QQQ\"]);}\nfunction onDay(ctx){ctx.log(\"a strategy log line, which belongs in the run and not on stdout\");if(!ctx.hasPosition(\"SPY\"))ctx.buy(\"SPY\",{pctCash:0.5});}",
		"universe": ["SPY", "QQQ"],
		"start": "2021-01-04",
		"end": "2021-06-30"
	}`
	_, out := session(t, s,
		initRequest,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		call(3, "backtest", args),
		// A run that leaves most ratios undefined: a NaN reaching the wire
		// would refuse to encode and truncate the frame, which over stdio is
		// far harder to diagnose than it is over HTTP.
		call(4, "backtest", `{"code":"function setup(ctx){ctx.universe([\"SPY\"]);}\nfunction onDay(ctx){}","start":"2021-01-04","end":"2021-06-30"}`),
	)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// Four requests carry an id; the notification does not and must not be
	// answered, so four frames is the whole of stdout.
	if len(lines) != 4 {
		t.Fatalf("%d lines on stdout, want one frame per request that carried an id", len(lines))
	}
	for i, line := range lines {
		var f frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("line %d is not JSON: %v\n%.300s", i, err, line)
		}
		if f.JSONRPC != "2.0" {
			t.Errorf("line %d is not a JSON-RPC message: %.200s", i, line)
		}
		if f.ID == nil {
			t.Errorf("line %d answers nothing: %.200s", i, line)
		}
		if len(f.Result) == 0 && f.Error == nil {
			t.Errorf("line %d is neither a result nor an error: %.200s", i, line)
		}
	}
	// Non-finite literals parse nowhere, so their absence is worth asserting
	// directly rather than relying on a decoder to notice.
	for _, bad := range []string{"NaN", "Infinity", "-Inf"} {
		if strings.Contains(out, bad) {
			t.Errorf("a %s literal reached the wire", bad)
		}
	}
}

// A strategy that fails is a real answer to the question the agent asked, so
// it comes back as a tool result it can read and act on rather than as a
// protocol error, which most clients surface as a broken connection.
func TestARunThatFailsArrivesAsAResult(t *testing.T) {
	s := newTestServer(t)
	frames, _ := session(t, s, initRequest,
		call(2, "backtest", `{"code":"function onDay(ctx){}","start":"2021-01-04","end":"2021-06-30"}`))

	var res toolCallResult
	result(t, frames, 2, &res)
	if !res.IsError {
		t.Fatal("a strategy with nothing to trade was reported as a success")
	}
	if len(res.Content) == 0 || res.Content[0].Text == "" {
		t.Fatal("the failure came back with no explanation")
	}
}
