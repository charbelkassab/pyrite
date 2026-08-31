package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charbelkassab/pyrite/internal/app"
	"github.com/charbelkassab/pyrite/internal/config"
)

// newTestServer builds a server backed by a throwaway data directory and the
// synthetic provider.
//
// Offline mode is what makes these tests worth having in CI: the whole
// package is reachable with no network, no API key and no market vendor, so
// the HTTP surface is exercised on every push rather than only when someone
// happens to run the binary.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.OfflineMode = true

	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	return New(a)
}

// do issues a request against the server's own mux, which is the same routing
// a real client hits.
func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
		t.Fatalf("decode %s: %v\nbody: %.400s", w.Result().Status, err, w.Body.String())
	}
}

func TestReadEndpointsAnswerWithoutAKeyOrANetwork(t *testing.T) {
	s := newTestServer(t)

	for _, tc := range []struct {
		path string
		want string // a substring the payload must contain
	}{
		{"/api/health", "provider"},
		{"/api/universes", "megacap"},
		{"/api/examples", ""},
		{"/api/bundled", "golden-cross"},
		{"/api/objectives", "sharpe"},
		{"/api/strategy-api", "ctx."},
	} {
		w := do(t, s, http.MethodGet, tc.path, "")
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (body %.200s)", tc.path, w.Code, w.Body.String())
			continue
		}
		if tc.want != "" && !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("GET %s did not mention %q", tc.path, tc.want)
		}
	}
}

// Every bundled example must be offered with the fields the front end needs to
// run it. A keyless visitor can only use the examples, so an example missing
// its code is the difference between a usable app and a dead one.
func TestBundledExamplesArriveReadyToRun(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, http.MethodGet, "/api/bundled", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}

	var got []struct {
		Name  string   `json:"name"`
		Title string   `json:"title"`
		Code  string   `json:"code"`
		Univ  []string `json:"universe"`
	}
	decode(t, w, &got)
	if len(got) < 5 {
		t.Fatalf("only %d bundled examples served", len(got))
	}
	for _, e := range got {
		if e.Name == "" || e.Code == "" {
			t.Errorf("example %q arrived without code", e.Name)
		}
		if !strings.Contains(e.Code, "function onDay") {
			t.Errorf("example %q has no onDay", e.Name)
		}
	}
}

// A run is created, executed in the background and then readable by id. This
// covers the whole lifecycle the front end depends on.
func TestRunLifecycle(t *testing.T) {
	s := newTestServer(t)

	body := `{
		"code": "function setup(ctx){ctx.universe([\"SPY\"]);}\nfunction onDay(ctx){if(!ctx.hasPosition(\"SPY\"))ctx.buy(\"SPY\",{pctCash:1});}",
		"name": "buy and hold",
		"start": "2021-01-04",
		"end": "2021-06-30"
	}`
	w := do(t, s, http.MethodPost, "/api/runs", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create run = %d, want 202 (body %.300s)", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	decode(t, w, &created)
	if created.ID == "" {
		t.Fatal("no run id returned")
	}

	run := waitForRun(t, s, created.ID)
	if run.Status != string(StatusDone) {
		t.Fatalf("run finished as %q: %s", run.Status, run.Error)
	}
	if run.Result == nil {
		t.Fatal("a completed run carried no result")
	}
	if len(run.Result.Curve) == 0 {
		t.Error("the result has an empty equity curve")
	}
	// The critique is the product, so its absence is a failure even when the
	// backtest itself succeeded.
	if run.Result.Critique == nil || len(run.Result.Critique.Findings) == 0 {
		t.Error("a completed run carried no critique findings")
	}

	// It must also appear in the listing, and survive deletion correctly.
	w = do(t, s, http.MethodGet, "/api/runs", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), created.ID) {
		t.Errorf("run %s missing from the listing (status %d)", created.ID, w.Code)
	}
	if w := do(t, s, http.MethodDelete, "/api/runs/"+created.ID, ""); w.Code >= 400 {
		t.Errorf("delete = %d", w.Code)
	}
	if w := do(t, s, http.MethodGet, "/api/runs/"+created.ID, ""); w.Code != http.StatusNotFound {
		t.Errorf("deleted run still readable: %d", w.Code)
	}
}

// A parameter search shares the run lifecycle, and reports a sweep rather than
// a single result.
func TestSweepLifecycle(t *testing.T) {
	s := newTestServer(t)

	body := `{
		"code": "function setup(ctx){ctx.universe([\"SPY\"]);ctx.param(\"n\",10,{grid:[5,10,20]});ctx.warmup(25);}\nfunction onDay(ctx){var m=ctx.sma(\"SPY\",ctx.params.n);if(m===null)return;if(ctx.price(\"SPY\")>m){if(!ctx.hasPosition(\"SPY\"))ctx.buy(\"SPY\",{pctCash:1});}else if(ctx.hasPosition(\"SPY\"))ctx.close(\"SPY\");}",
		"name": "sma filter",
		"start": "2021-01-04",
		"end": "2021-12-30"
	}`
	w := do(t, s, http.MethodPost, "/api/sweeps", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create sweep = %d (body %.300s)", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	decode(t, w, &created)

	run := waitForRun(t, s, created.ID)
	if run.Status != string(StatusDone) {
		t.Fatalf("sweep finished as %q: %s", run.Status, run.Error)
	}
	if run.Sweep == nil || len(run.Sweep.Rows) == 0 {
		t.Fatal("a completed sweep carried no rows")
	}
	if len(run.Sweep.Rows) != 3 {
		t.Errorf("swept %d combinations, want 3", len(run.Sweep.Rows))
	}
}

// The API must reject what it cannot do, with a status a client can act on
// rather than a 500 or a silent empty body.
func TestBadRequestsAreRejectedClearly(t *testing.T) {
	s := newTestServer(t)

	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"malformed json", "/api/runs", `{"code":`, http.StatusBadRequest},
		{"nothing to run", "/api/runs", `{}`, http.StatusBadRequest},
		{"nothing to sweep", "/api/sweeps", `{}`, http.StatusBadRequest},
		// No key is configured in these tests, so a prompt cannot be compiled.
		// That is a precondition failure, not a bad request: the client sent
		// something valid that this server is not equipped to serve.
		{"prompt without a model", "/api/runs", `{"prompt":"buy spy"}`, http.StatusPreconditionFailed},
		{"unknown run", "/api/runs/nope", "", http.StatusNotFound},
	} {
		method := http.MethodPost
		if tc.body == "" {
			method = http.MethodGet
		}
		w := do(t, s, method, tc.path, tc.body)
		if w.Code != tc.want {
			t.Errorf("%s: %s %s = %d, want %d (body %.160s)",
				tc.name, method, tc.path, w.Code, tc.want, w.Body.String())
		}
		if w.Code >= 400 && !strings.Contains(w.Header().Get("Content-Type"), "json") {
			t.Errorf("%s: error response was not JSON", tc.name)
		}
	}
}

// The front end is embedded, so the binary must serve it without any files on
// disk. An unknown path falls through to the single-page app rather than 404,
// which is what makes client-side routing work.
func TestStaticAssetsAreServedFromTheBinary(t *testing.T) {
	s := newTestServer(t)

	w := do(t, s, http.MethodGet, "/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET / = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<html") {
		t.Error("GET / did not return a document")
	}
	if w := do(t, s, http.MethodGet, "/app.js", ""); w.Code != http.StatusOK {
		t.Errorf("GET /app.js = %d", w.Code)
	}
}

// Symbol search backs the chart's comparison picker.
func TestSymbolSearch(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, http.MethodGet, "/api/symbols?q=SPY", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if !strings.Contains(strings.ToUpper(w.Body.String()), "SPY") {
		t.Errorf("searching for SPY did not return it: %.200s", w.Body.String())
	}
}

// waitForRun polls until the run leaves its in-flight states.
func waitForRun(t *testing.T, s *Server, id string) runView {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last runView
	for time.Now().Before(deadline) {
		w := do(t, s, http.MethodGet, "/api/runs/"+id, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET run %s = %d", id, w.Code)
		}
		decode(t, w, &last)
		switch RunStatus(last.Status) {
		case StatusDone, StatusError, StatusCancelled:
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s never finished (last status %q)", id, last.Status)
	return last
}

// runView is the shape a client actually reads back, kept deliberately loose
// so these tests assert on the JSON contract rather than on internal types.
type runView struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Result *struct {
		Curve    []json.RawMessage `json:"curve"`
		Critique *struct {
			Findings []json.RawMessage `json:"findings"`
		} `json:"critique"`
	} `json:"result"`
	Sweep *struct {
		Rows []json.RawMessage `json:"rows"`
	} `json:"sweep"`
}
