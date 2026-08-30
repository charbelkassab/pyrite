package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func modelsServer(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[`))
		for i, id := range ids {
			if i > 0 {
				w.Write([]byte(","))
			}
			w.Write([]byte(`{"id":"` + id + `"}`))
		}
		w.Write([]byte(`]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDetectLocalEnablesARunningRuntime(t *testing.T) {
	srv := modelsServer(t, "qwen2.5-coder:7b", "llama3.2:3b")
	c := Defaults()
	c.Providers["ollama"].BaseURL = srv.URL
	// Nothing is listening for LM Studio in this test.
	c.Providers["lmstudio"].BaseURL = "http://127.0.0.1:1"

	c.DetectLocal(context.Background())

	if !c.Providers["ollama"].Enabled {
		t.Fatal("a running local runtime should be enabled")
	}
	if c.Providers["lmstudio"].Enabled {
		t.Error("a runtime that is not running must stay disabled")
	}
	// With no paid key, every tier should route to the local runtime — that
	// is what makes the tool work with no account at all.
	for _, tier := range []Tier{TierFast, TierBalanced, TierQuality} {
		if c.Routes[tier] != "ollama" {
			t.Errorf("tier %s routes to %q, want ollama", tier, c.Routes[tier])
		}
	}
	if !c.AnyProviderEnabled() {
		t.Error("a detected local runtime counts as a provider")
	}
	if c.AnyCloudProviderEnabled() {
		t.Error("a local runtime is not a cloud provider")
	}
}

func TestDetectLocalDoesNotStealRoutingFromAPaidKey(t *testing.T) {
	srv := modelsServer(t, "llama3.2:3b")
	c := Defaults()
	c.Providers["ollama"].BaseURL = srv.URL
	c.Providers["lmstudio"].BaseURL = "http://127.0.0.1:1"
	// Someone who has paid for a key expects it to be used.
	c.Providers["openai"].APIKey = "sk-test"
	c.Providers["openai"].Enabled = true

	c.DetectLocal(context.Background())

	if c.Routes[TierQuality] != "openai" {
		t.Errorf("a configured key should keep its routing, got %q", c.Routes[TierQuality])
	}
	// The local runtime is still enabled, so it can be selected explicitly.
	if !c.Providers["ollama"].Enabled {
		t.Error("the local runtime should still be available to pin")
	}
}

func TestDetectLocalIsHarmlessWithNothingRunning(t *testing.T) {
	c := Defaults()
	c.Providers["ollama"].BaseURL = "http://127.0.0.1:1"
	c.Providers["lmstudio"].BaseURL = "http://127.0.0.1:2"
	c.DetectLocal(context.Background())

	if c.AnyProviderEnabled() {
		t.Error("nothing running and no key means no provider")
	}
	// Routing must be untouched, so the error messages stay accurate.
	if c.Routes[TierQuality] != "openai" {
		t.Errorf("routing changed with nothing detected: %q", c.Routes[TierQuality])
	}
}

func TestPickLocalModelPrefersACoder(t *testing.T) {
	cases := []struct {
		have []string
		want string
	}{
		// An embedding model cannot answer a chat completion at all.
		{[]string{"nomic-embed-text:latest", "qwen2.5-coder:7b"}, "qwen2.5-coder:7b"},
		// Bigger wins among comparable models.
		{[]string{"llama3.2:3b", "llama3.1:70b"}, "llama3.1:70b"},
		// Code-tuned beats general at the same size.
		{[]string{"llama3.1:8b", "qwen2.5-coder:7b"}, "qwen2.5-coder:7b"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := pickLocalModel(c.have); got != c.want {
			t.Errorf("pickLocalModel(%v) = %q, want %q", c.have, got, c.want)
		}
	}
}

func TestPickLocalModelReturnsSomethingRatherThanNothing(t *testing.T) {
	// If embeddings are all that is installed, one is still returned. The
	// compile then fails with the runtime's own error, which is more useful
	// than pyrite claiming no model exists.
	if got := pickLocalModel([]string{"nomic-embed-text:latest"}); got != "nomic-embed-text:latest" {
		t.Errorf("got %q", got)
	}
}

func TestLocalProviderNeedsNoKey(t *testing.T) {
	c := Defaults()
	p := c.Providers["ollama"]
	if !p.Local {
		t.Fatal("ollama should be marked local")
	}
	p.Detected = true
	c.applyEnv()
	if !p.Enabled {
		t.Error("a detected local provider should be enabled without a key")
	}
}
