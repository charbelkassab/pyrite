package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cache is what makes a model-reading backtest affordable to re-run. A
// strategy that calls ctx.ai() once a day over four years is ~1000 requests,
// so a cache that silently misses turns a free second run into an expensive
// one, and a cache that wrongly hits serves one day's answer for every day.

func TestCacheRoundTrips(t *testing.T) {
	c, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}

	if _, ok := c.Get("absent"); ok {
		t.Error("an empty cache reported a hit")
	}
	if err := c.Put("k1", "the answer"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := c.Get("k1")
	if !ok {
		t.Fatal("a value just written was not found")
	}
	if got != "the answer" {
		t.Errorf("Get = %q, want %q", got, "the answer")
	}

	hits, misses := c.Stats()
	if hits != 1 || misses != 1 {
		t.Errorf("stats = %d hits / %d misses, want 1 / 1", hits, misses)
	}
}

// A second process must see what the first one wrote, which is the whole point
// of persisting to disk rather than keeping a map.
func TestCacheSurvivesReopening(t *testing.T) {
	dir := t.TempDir()
	c1, err := NewCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := c1.Put("shared", "written by the first run"); err != nil {
		t.Fatal(err)
	}

	c2, err := NewCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c2.Get("shared")
	if !ok {
		t.Fatal("a reopened cache lost its contents")
	}
	if got != "written by the first run" {
		t.Errorf("Get = %q", got)
	}
}

func TestCacheClearEmptiesIt(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if err := c.Put(k, "v"); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, ok := c.Get(k); ok {
			t.Errorf("%q survived Clear", k)
		}
	}
}

// Keys become file names, and not every key is a hash: websearch builds one
// from the search query, which comes from strategy code. Before the key was
// hashed, ctx.news("../../../../<a path>") wrote a JSON file wherever the
// traversal landed — a filesystem write from inside a sandbox whose entire
// purpose is that strategies have no filesystem.
func TestCacheKeysCannotEscapeTheDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b", "cache")
	c, err := NewCache(dir)
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(root, "ESCAPED")
	keys := []string{
		"../../escaped",
		"true|5|2024-01-02|" + strings.Repeat("../", 16) + target,
		"/etc/passwd",
		"a/b/c",
		"", // must not panic either
		"x",
	}
	for _, k := range keys {
		if err := c.Put(k, "value"); err != nil {
			t.Errorf("Put(%.30q): %v", k, err)
		}
	}

	if _, err := os.Stat(target + ".json"); err == nil {
		t.Errorf("a key escaped to %s.json", target)
	}

	// Everything written must live under the cache directory.
	var strays []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			strays = append(strays, p)
		}
		return nil
	})
	if len(strays) > 0 {
		t.Errorf("the cache wrote outside %s: %v", dir, strays)
	}

	// And the values must still be retrievable by their original keys.
	for _, k := range keys {
		if _, ok := c.Get(k); !ok {
			t.Errorf("Get(%.30q) missed after Put", k)
		}
	}
}

// Two different requests must not share an answer, and the same request must
// always produce the same key — otherwise every re-run is a cache miss.
func TestCacheKeyIsStableAndDiscriminating(t *testing.T) {
	base := Request{Messages: []Message{{Role: "user", Content: "hello"}}}

	k1 := cacheKey("openai", "gpt", base)
	if k1 != cacheKey("openai", "gpt", base) {
		t.Error("the same request produced two different keys")
	}

	for _, tc := range []struct {
		name             string
		provider, model  string
		req              Request
		wantDifferentKey bool
	}{
		{"another provider", "cerebras", "gpt", base, true},
		{"another model", "openai", "other", base, true},
		{"another message", "openai", "gpt",
			Request{Messages: []Message{{Role: "user", Content: "goodbye"}}}, true},
		{"json mode on", "openai", "gpt",
			Request{Messages: base.Messages, JSONMode: true}, true},
		{"identical", "openai", "gpt", base, false},
	} {
		got := cacheKey(tc.provider, tc.model, tc.req)
		if (got != k1) != tc.wantDifferentKey {
			t.Errorf("%s: key differs = %v, want %v", tc.name, got != k1, tc.wantDifferentKey)
		}
	}

	// An explicit cache key overrides the message contents entirely, which is
	// how callers pin an answer to a simulated day rather than to the prompt.
	pinned := Request{Messages: base.Messages, CacheKey: "2019-03-04|AAPL"}
	other := Request{
		Messages: []Message{{Role: "user", Content: "completely different"}},
		CacheKey: "2019-03-04|AAPL",
	}
	if cacheKey("openai", "gpt", pinned) != cacheKey("openai", "gpt", other) {
		t.Error("an explicit CacheKey did not override the message contents")
	}
}
