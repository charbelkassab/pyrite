// Package server exposes natural-quant over HTTP: a JSON API, a server-sent
// event stream for run progress, and the embedded single-page front end.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charbelkassab/natural-quant/internal/app"
	"github.com/charbelkassab/natural-quant/internal/engine"
	"github.com/charbelkassab/natural-quant/internal/market"
	"github.com/charbelkassab/natural-quant/internal/strategy"
	webassets "github.com/charbelkassab/natural-quant/web"
)

// Server serves the API and the front end.
type Server struct {
	app  *app.App
	runs *RunStore
	mux  *http.ServeMux
	// assets serves the front end. It is the embedded filesystem in a normal
	// build, and the working tree when --dev is passed, so editing app.js or
	// styles.css does not require rebuilding the binary.
	assets fs.FS
}

// UseDevAssets serves the front end from dir instead of the embedded copy.
func (s *Server) UseDevAssets(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		return fmt.Errorf("no index.html under %s: %w", dir, err)
	}
	s.assets = os.DirFS(dir)
	return nil
}

// New builds a server.
func New(a *app.App) *Server {
	runs, err := NewRunStore(a.RunsDir())
	if err != nil {
		log.Printf("warning: run history is disabled (%v)", err)
	}
	s := &Server{app: a, runs: runs, mux: http.NewServeMux(), assets: webassets.FS}
	s.routes()
	return s
}

func (s *Server) routes() {
	m := s.mux

	m.HandleFunc("GET /api/health", s.handleHealth)
	m.HandleFunc("GET /api/universes", s.handleUniverses)
	m.HandleFunc("GET /api/examples", s.handleExamples)
	m.HandleFunc("GET /api/strategy-api", s.handleStrategyAPI)
	m.HandleFunc("GET /api/symbols", s.handleSymbolSearch)
	m.HandleFunc("GET /api/series", s.handleSeries)

	m.HandleFunc("POST /api/compile", s.handleCompile)
	m.HandleFunc("POST /api/runs", s.handleCreateRun)
	m.HandleFunc("GET /api/runs", s.handleListRuns)
	m.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	m.HandleFunc("DELETE /api/runs/{id}", s.handleDeleteRun)
	m.HandleFunc("GET /api/runs/{id}/events", s.handleRunEvents)
	m.HandleFunc("POST /api/runs/{id}/cancel", s.handleCancelRun)

	// Static front end, served from the embedded filesystem.
	m.HandleFunc("/", s.handleStatic)
}

// ListenAndServe starts the HTTP server and shuts it down when ctx is done.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              announceAddr(addr),
		Handler:           s.mux,
		ReadHeaderTimeout: 15 * time.Second,
		// Backtests stream progress for minutes, so no write timeout.
		IdleTimeout: 120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		fmt.Println("\nshutting down")
		return srv.Shutdown(shutdownCtx)
	}
}

func announceAddr(addr string) string {
	if addr == "" {
		return "127.0.0.1:8080"
	}
	return addr
}

// ---- static assets -------------------------------------------------------

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	data, err := fs.ReadFile(s.assets, path)
	if err != nil {
		// Unknown paths fall back to the app shell so deep links work.
		data, err = fs.ReadFile(s.assets, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		path = "index.html"
	}
	switch {
	case strings.HasSuffix(path, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(path, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	}
	if path != "index.html" {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	_, _ = w.Write(data)
}

// ---- simple JSON endpoints ----------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	probe := r.URL.Query().Get("probe") == "1"
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.app.Health(ctx, probe))
}

func (s *Server) handleUniverses(w http.ResponseWriter, r *http.Request) {
	out := make([]*market.Universe, 0, len(market.Universes))
	for _, k := range market.UniverseKeys() {
		out = append(out, market.Universes[k])
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleStrategyAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(strategy.APIReference()))
}

func (s *Server) handleSymbolSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, []market.Quote{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	results, err := s.app.Store.Search(ctx, q)
	if err != nil {
		// A failed lookup should still let the user type a ticker directly.
		writeJSON(w, http.StatusOK, []market.Quote{{Symbol: market.NormalizeSymbol(q), Name: "use as typed"}})
		return
	}
	writeJSON(w, http.StatusOK, results)
}

// handleSeries returns price history for arbitrary symbols, which is what the
// chart uses to overlay a stock or index next to a strategy.
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	symbols := market.DedupeSymbols(strings.Split(q.Get("symbols"), ","))
	if len(symbols) == 0 {
		writeError(w, http.StatusBadRequest, "pass ?symbols=AAPL,MSFT")
		return
	}
	if len(symbols) > 20 {
		symbols = symbols[:20]
	}
	from, to := market.Day(q.Get("from")), market.Day(q.Get("to"))
	if to == "" {
		to = market.NewDay(time.Now())
	}
	if from == "" {
		from = to.Add(-365 * 5)
	}
	base := 100000.0
	if v, err := strconv.ParseFloat(q.Get("base"), 64); err == nil && v > 0 {
		base = v
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	series, errs := s.app.Store.GetMany(ctx, symbols, from, to)

	type out struct {
		Symbol  string               `json:"symbol"`
		Label   string               `json:"label"`
		Curve   []engine.EquityPoint `json:"curve"`
		Metrics engine.Metrics       `json:"metrics"`
	}
	resp := make([]out, 0, len(series))
	// Use the union calendar so overlaid series align with each other.
	days := market.TradingCalendar(series, from, to)
	for _, sym := range symbols {
		ser, ok := series[sym]
		if !ok {
			continue
		}
		curve := engine.BuyAndHoldCurve(ser, days, base)
		if len(curve) == 0 {
			continue
		}
		label := ser.Name
		if label == "" {
			label = sym
		}
		resp = append(resp, out{
			Symbol: sym, Label: label, Curve: curve,
			Metrics: engine.ComputeMetrics(curve, 0),
		})
	}
	failed := map[string]string{}
	for sym, err := range errs {
		failed[sym] = err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": resp, "failed": failed})
}

// ---- compile -------------------------------------------------------------

type compileRequest struct {
	Prompt   string   `json:"prompt"`
	Universe []string `json:"universe,omitempty"`
	Tier     string   `json:"tier,omitempty"`
	Start    string   `json:"start,omitempty"`
	End      string   `json:"end,omitempty"`
}

func (s *Server) handleCompile(w http.ResponseWriter, r *http.Request) {
	var req compileRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request: "+err.Error())
		return
	}
	if !s.app.Cfg.AnyProviderEnabled() {
		writeError(w, http.StatusPreconditionFailed,
			"No model API key is configured. Set OPENAI_API_KEY, CEREBRAS_API_KEY or KIMI_API_KEY and restart.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	plan, err := s.app.Compiler.Compile(ctx, strategy.Request{
		Prompt:   req.Prompt,
		Universe: market.DedupeSymbols(req.Universe),
		Start:    market.Day(req.Start),
		End:      market.Day(req.End),
	})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// ---- runs ----------------------------------------------------------------

type runRequest struct {
	Prompt string `json:"prompt"`
	// Code, when supplied, skips compilation and runs the given strategy.
	Code     string   `json:"code,omitempty"`
	Name     string   `json:"name,omitempty"`
	Universe []string `json:"universe,omitempty"`

	Start       string   `json:"start,omitempty"`
	End         string   `json:"end,omitempty"`
	InitialCash float64  `json:"initial_cash,omitempty"`
	Benchmarks  []string `json:"benchmarks,omitempty"`
	Fill        string   `json:"fill,omitempty"`
	AllowShort  bool     `json:"allow_short,omitempty"`
	MaxLeverage float64  `json:"max_leverage,omitempty"`
	RiskFree    float64  `json:"risk_free_rate,omitempty"`
	Warmup      int      `json:"warmup,omitempty"`

	SlippageBps   float64 `json:"slippage_bps,omitempty"`
	CommissionPct float64 `json:"commission_pct,omitempty"`

	Params map[string]any `json:"params,omitempty"`
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" && strings.TrimSpace(req.Code) == "" {
		writeError(w, http.StatusBadRequest, "describe a strategy, or supply code directly")
		return
	}
	if strings.TrimSpace(req.Code) == "" && !s.app.Cfg.AnyProviderEnabled() {
		writeError(w, http.StatusPreconditionFailed,
			"No model API key is configured, so plain-language strategies cannot be compiled. Set OPENAI_API_KEY, CEREBRAS_API_KEY or KIMI_API_KEY and restart.")
		return
	}

	label := req.Name
	if label == "" {
		label = firstLine(req.Prompt, 80)
	}
	run := s.runs.Create(req.Prompt, label)

	// The run outlives this request, so it gets its own context.
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(s.app.Cfg.StrategyTimeoutSec)*time.Second)
	run.cancel = cancel

	go s.execute(ctx, run, req)

	writeJSON(w, http.StatusAccepted, map[string]string{"id": run.ID})
}

// execute compiles and runs a backtest, publishing progress as it goes.
func (s *Server) execute(ctx context.Context, run *Run, req runRequest) {
	defer func() {
		if run.cancel != nil {
			run.cancel()
		}
		close(run.finished)
		if s.runs != nil {
			_ = s.runs.Save(run)
		}
	}()
	// A panic inside a strategy or the engine must not take the server down.
	defer func() {
		if rec := recover(); rec != nil {
			run.update(func(r *Run) {
				r.Status = StatusError
				r.Error = fmt.Sprintf("the run failed unexpectedly: %v", rec)
			})
			run.publish(Event{Type: "error", Run: run.snapshot(false)})
		}
	}()

	opts := app.RunOptions{
		InitialCash: req.InitialCash,
		Benchmarks:  market.DedupeSymbols(req.Benchmarks),
		Universe:    market.DedupeSymbols(req.Universe),
		MaxLeverage: req.MaxLeverage,
		RiskFree:    req.RiskFree,
		Params:      req.Params,
	}
	if req.Start != "" {
		opts.Start = market.Day(req.Start)
	}
	if req.End != "" {
		opts.End = market.Day(req.End)
	}
	if req.Fill == string(engine.FillClose) {
		opts.Fill = engine.FillClose
	}
	opts.ApplyDefaults()

	var plan *strategy.Plan
	if strings.TrimSpace(req.Code) != "" {
		// Running supplied code directly: no model call needed.
		plan = &strategy.Plan{
			Name:       defaultString(req.Name, "Custom strategy"),
			Code:       req.Code,
			Universe:   opts.Universe,
			Benchmarks: opts.Benchmarks,
			Warmup:     req.Warmup,
			AllowShort: req.AllowShort,
		}
		if len(plan.Universe) == 0 {
			plan.Universe = market.ResolveUniverse("megacap")
		}
	} else {
		run.update(func(r *Run) { r.Status = StatusCompiling; r.Stage = "compiling strategy" })
		run.publish(Event{Type: "status", Run: run.snapshot(false)})

		var err error
		plan, err = s.app.Compiler.Compile(ctx, strategy.Request{
			Prompt:   req.Prompt,
			Universe: opts.Universe,
			Start:    opts.Start,
			End:      opts.End,
		})
		if err != nil {
			run.update(func(r *Run) { r.Status = StatusError; r.Error = err.Error() })
			run.publish(Event{Type: "error", Run: run.snapshot(false)})
			return
		}
	}

	// A strategy that consults a model every week cannot be run over the full
	// history: say so rather than silently truncating or exhausting the budget.
	if app.ClampForAI(&opts, plan.NeedsAI || plan.NeedsWeb) {
		plan.Assumptions = append(plan.Assumptions, fmt.Sprintf(
			"This strategy calls a model or the web, so it was run over the last %d years rather than the full history. "+
				"Set an explicit period on the chart to change that.", app.MaxAIYears))
	}

	run.update(func(r *Run) {
		r.Plan = plan
		r.Status = StatusRunning
		r.Stage = "loading market data"
		if r.Label == "" {
			r.Label = plan.Name
		}
	})
	run.publish(Event{Type: "status", Run: run.snapshot(false)})

	spec := app.BuildSpec(plan, req.Prompt, opts)
	if req.Warmup > 0 {
		spec.Warmup = req.Warmup
	}
	if req.AllowShort {
		spec.AllowShort = true
	}
	if spec.Start == "" && opts.Start != "" {
		spec.Start = opts.Start
	}
	if req.SlippageBps > 0 {
		spec.Costs.SlippageBps = req.SlippageBps
	}
	if req.CommissionPct > 0 {
		spec.Costs.CommissionPct = req.CommissionPct
	}

	opts.Progress = func(done, total int, day market.Day) {
		pct := 0
		if total > 0 {
			pct = done * 100 / total
		}
		run.update(func(r *Run) {
			r.Progress = pct
			r.Day = day
			r.Stage = "simulating"
		})
		run.publish(Event{Type: "progress", Run: run.snapshot(false)})
	}

	res, err := s.app.Backtest(ctx, spec, opts)
	if err != nil {
		status := StatusError
		if errors.Is(err, context.Canceled) {
			status = StatusCancelled
		}
		run.update(func(r *Run) { r.Status = status; r.Error = err.Error() })
		run.publish(Event{Type: "error", Run: run.snapshot(false)})
		return
	}

	run.update(func(r *Run) {
		r.Result = res
		r.Status = StatusDone
		r.Progress = 100
		r.Stage = "done"
	})
	run.publish(Event{Type: "done", Run: run.snapshot(true)})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	limit := 30
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	live := s.runs.List(limit)
	seen := map[string]bool{}
	for _, r := range live {
		seen[r.ID] = true
	}
	for _, r := range s.runs.ListSaved(limit) {
		if !seen[r.ID] {
			live = append(live, r)
		}
	}
	writeJSON(w, http.StatusOK, live)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runs.Get(r.PathValue("id"))
	if !ok || run == nil {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	writeJSON(w, http.StatusOK, run.snapshot(true))
}

func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	if err := s.runs.Delete(r.PathValue("id")); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runs.Get(r.PathValue("id"))
	if !ok || run == nil {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	if run.cancel != nil {
		run.cancel()
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRunEvents streams progress as server-sent events.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runs.Get(r.PathValue("id"))
	if !ok || run == nil {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this connection")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch := run.subscribe()
	defer run.unsubscribe(ch)

	send := func(ev Event) bool {
		b, err := json.Marshal(ev)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Send the current state immediately so a late subscriber is not stuck
	// waiting for the next update.
	snap := run.snapshot(false)
	if snap.Status == StatusDone || snap.Status == StatusError || snap.Status == StatusCancelled {
		send(Event{Type: terminalEventType(snap.Status), Run: run.snapshot(true)})
		return
	}
	send(Event{Type: "status", Run: snap})

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev := <-ch:
			if !send(ev) {
				return
			}
			if ev.Type == "done" || ev.Type == "error" {
				return
			}
		case <-run.finished:
			// Drain anything still queued, then send the terminal state.
			for {
				select {
				case ev := <-ch:
					send(ev)
					continue
				default:
				}
				break
			}
			final := run.snapshot(true)
			send(Event{Type: terminalEventType(final.Status), Run: final})
			return
		}
	}
}

func terminalEventType(st RunStatus) string {
	if st == StatusDone {
		return "done"
	}
	return "error"
}

// ---- examples ------------------------------------------------------------

// Example is a ready-made prompt shown in the UI to get people started.
type Example struct {
	Title  string `json:"title"`
	Prompt string `json:"prompt"`
	Note   string `json:"note,omitempty"`
	Tag    string `json:"tag,omitempty"`
}

func (s *Server) handleExamples(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Examples)
}

// Examples are curated to demonstrate the range of the platform, from a plain
// moving-average rule to strategies that call a model mid-backtest.
var Examples = []Example{
	{
		Tag:   "market cap",
		Title: "Own whatever is biggest",
		Prompt: "Buy $100 of the biggest company in the US by market cap every day, " +
			"and sell it when that company is no longer number one.",
	},
	{
		Tag:    "trend",
		Title:  "Golden cross with a trailing stop",
		Prompt: "Buy SPY when its 50 day moving average crosses above the 200 day, sell on the reverse cross, and use a 12% trailing stop.",
	},
	{
		Tag:    "momentum",
		Title:  "Monthly momentum rotation",
		Prompt: "Every month, hold an equal weight basket of the 3 best performing big tech stocks over the previous 6 months.",
	},
	{
		Tag:    "mean reversion",
		Title:  "Buy the dip on quality names",
		Prompt: "Buy any mega cap stock whose RSI drops below 30, size each position at 10% of the portfolio, and sell when RSI goes back above 55. Use an 8% stop loss.",
	},
	{
		Tag:    "allocation",
		Title:  "Classic 60/40, rebalanced quarterly",
		Prompt: "Hold 60% SPY and 40% AGG, rebalancing back to those weights every quarter.",
	},
	{
		Tag:    "risk",
		Title:  "Go to cash in a downtrend",
		Prompt: "Hold QQQ while it is above its 200 day moving average, and move entirely to cash whenever it closes below.",
	},
	{
		Tag:    "long/short",
		Title:  "Pairs trade Coke against Pepsi",
		Prompt: "Trade KO against PEP. When the ratio of their prices is more than 2 standard deviations below its 60 day average, go long KO and short PEP, and close both when the ratio returns to its average.",
		Note:   "Requires shorting.",
	},
	{
		Tag:    "sectors",
		Title:  "Rotate into the strongest sectors",
		Prompt: "Each month, hold the 2 strongest S&P 500 sector ETFs by 3 month momentum, equally weighted.",
	},
	{
		Tag:    "volatility",
		Title:  "Hide when volatility spikes",
		Prompt: "Hold SPY normally, but move to cash for the next month whenever the VIX closes above 30.",
	},
	{
		Tag:    "AI",
		Title:  "Trade on the weekly news mood",
		Prompt: "Once a week, read the latest news headlines about Apple and ask the AI whether the tone is positive or negative. Hold AAPL when it is positive and stay in cash when it is negative.",
		Note:   "Uses the model and the internet during the backtest. Read the lookahead warning before trusting the result.",
	},
	{
		Tag:    "AI",
		Title:  "Do the opposite of the headlines",
		Prompt: "Every Monday, look up the second article on Yahoo Finance about the S&P 500, ask the AI whether it is bullish or bearish, and take the opposite position in SPY for the week.",
		Note:   "A contrarian take on the same mechanism.",
	},
	{
		Tag:    "dollar cost averaging",
		Title:  "Buy a little every month",
		Prompt: "Invest $500 into VTI on the first trading day of every month and never sell.",
	},
}

// ---- helpers -------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func defaultString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max-1] + "…"
	}
	return s
}
