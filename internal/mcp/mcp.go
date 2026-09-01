// Package mcp serves pyrite to an agent over the Model Context Protocol.
//
// What makes the adapter worth having is the direction it faces. Everywhere
// else a model is a component pyrite calls; here pyrite is a tool a model
// calls, and an agent that can run a backtest and read only the Sharpe will
// happily search until it finds a flattering one. So every result that came
// from a backtest carries its critique, its trust score and its verdict in the
// same payload as the numbers, and there is no way to ask for one without the
// other.
//
// The protocol is implemented here against the specification rather than taken
// as a dependency. pyrite ships as a single binary with one module
// requirement, and JSON-RPC over a line-delimited stdio transport is a few
// hundred lines of standard library.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/charbelkassab/pyrite/internal/app"
)

// protocolVersion is the specification revision this server implements.
const protocolVersion = "2025-06-18"

// supportedVersions are the revisions this server will agree to speak. A
// client asking for one of these gets it echoed back; anything else gets ours
// and the client decides whether to carry on.
var supportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// JSON-RPC 2.0 error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// Server answers MCP requests by running pyrite.
type Server struct {
	app     *app.App
	version string
	tools   []*tool
}

// New builds a server over an already-wired application.
func New(a *app.App, version string) *Server {
	s := &Server{app: a, version: version}
	s.tools = s.toolset()
	return s
}

// request is one incoming JSON-RPC message. ID is left raw because JSON-RPC
// allows a string or a number and the value is only ever echoed back; absent
// entirely means a notification, which must not be answered.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

// invalidParams reports an argument the caller can fix: a missing field, a
// date that is not a date, a tool that does not exist. It is separated from an
// ordinary failure because the two want different answers — this one is a
// protocol error, whereas a backtest that ran and failed is a tool result the
// agent should read and react to.
func invalidParams(format string, args ...any) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

// Serve reads requests from in and writes responses to out until in is
// exhausted or ctx is cancelled.
//
// out carries protocol frames and nothing else. A single stray print on stdout
// desynchronises the stream and the client reports a parse error with no clue
// where it came from, so everything diagnostic in this package goes to stderr.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	// The transport is one JSON object per line, so the reader is a line
	// reader rather than a json.Decoder: a decoder cannot resynchronise after
	// a malformed message, and the specification requires answering one with
	// -32700 and carrying on.
	r := bufio.NewReader(in)
	w := bufio.NewWriter(out)

	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			resp, send := s.handle(ctx, line)
			if send {
				if werr := writeMessage(w, resp); werr != nil {
					return werr
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read from the client: %w", err)
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

func writeMessage(w *bufio.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode a response: %w", err)
	}
	// Marshal never emits a bare newline, so appending one is enough to frame
	// the message for a line-delimited transport.
	b = append(b, '\n')
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("write to the client: %w", err)
	}
	return w.Flush()
}

// handle turns one line into at most one response. The second return value is
// false for a notification, which JSON-RPC forbids answering.
func (s *Server) handle(ctx context.Context, line []byte) (*response, bool) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return &response{
			JSONRPC: "2.0",
			Error: &rpcError{Code: codeParse,
				Message: "the message was not valid JSON: " + err.Error()},
		}, true
	}
	notification := len(req.ID) == 0

	if req.Method == "" {
		if notification {
			return nil, false
		}
		return errorResponse(req.ID, &rpcError{Code: codeInvalidRequest,
			Message: "the message named no method"}), true
	}

	result, rerr := s.dispatch(ctx, req)
	if notification {
		return nil, false
	}
	if rerr != nil {
		return errorResponse(req.ID, rerr), true
	}
	if result == nil {
		// An empty object, not null: JSON-RPC requires a result member on a
		// success and some clients reject a null one.
		result = struct{}{}
	}
	return &response{JSONRPC: "2.0", ID: req.ID, Result: result}, true
}

func errorResponse(id json.RawMessage, err *rpcError) *response {
	return &response{JSONRPC: "2.0", ID: id, Error: err}
}

func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params), nil
	case "notifications/initialized", "notifications/cancelled":
		// Nothing to do, and nothing to say: a notification takes no reply.
		return nil, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools}, nil
	case "tools/call":
		return s.callTool(ctx, req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: fmt.Sprintf(
			"unknown method %q. This server implements initialize, tools/list, tools/call and ping.",
			req.Method)}
	}
}

// initParams is the handshake the client sends. Only the version is used: the
// client's capabilities describe what it can do, and this server needs none of
// them.
type initParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

func (s *Server) initialize(raw json.RawMessage) any {
	var p initParams
	// A malformed handshake is not worth refusing over. The version is the
	// only field read, and the answer when it is missing is the same as the
	// answer when it is unrecognised.
	_ = json.Unmarshal(raw, &p)

	version := protocolVersion
	for _, v := range supportedVersions {
		if p.ProtocolVersion == v {
			version = v
			break
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			// The tool list is fixed for the life of the process, so there is
			// nothing to notify about.
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    "pyrite",
			"title":   "pyrite backtester",
			"version": s.version,
		},
		"instructions": instructions,
	}
}

// instructions are handed to the calling model at the handshake. They exist to
// pre-empt the failure this whole tool is built against: an agent that runs
// backtests until one looks good and reports that one.
const instructions = `pyrite backtests trading strategies and reports the evidence against its own results.

Write the strategy as JavaScript with setup(ctx) and onDay(ctx) functions; call
strategy_api first for the functions available inside them, and list_examples for
working strategies to start from.

Every result carries a critique: specific, computed objections with the numbers
behind them, and a trust score out of 100. Read it before the headline metrics
and report it alongside them. A high Sharpe with a low trust score is not a
finding, it is a warning.

One backtest is one point in a space. sweep searches the parameter space and
returns the deflated Sharpe, the probability of backtest overfitting and how
isolated the winning cell is; walkforward chooses parameters on one period and
reports on the next, which is the only number that means what it appears to.
Trying settings until something looks good is the failure these measure.`
