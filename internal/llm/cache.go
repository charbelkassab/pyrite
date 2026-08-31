package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Cache stores completions on disk, keyed by a hash of the request.
//
// This matters more than it might appear. A strategy that calls ai() once per
// trading day over five years makes roughly 1,250 model calls. Without a
// cache, every re-run — every tweak to position sizing, every comparison
// against a new benchmark — pays that cost again in both money and minutes.
// With one, only the first run is expensive and every subsequent run is
// effectively free and perfectly reproducible.
type Cache struct {
	dir string

	mu  sync.RWMutex
	mem map[string]string
	// Hits and Misses are cumulative counters for reporting.
	hits, misses int
}

// NewCache opens (creating if needed) a cache directory.
func NewCache(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Cache{dir: dir, mem: map[string]string{}}, nil
}

// cacheKey derives a stable key for a request.
//
// When the caller supplies a CacheKey (strategies pass the simulated date
// plus the prompt), that value is hashed instead of the message bodies, so
// that incidental differences in surrounding context do not cause misses.
func cacheKey(provider, model string, req Request) string {
	h := sha256.New()
	h.Write([]byte(provider))
	h.Write([]byte{0})
	h.Write([]byte(model))
	h.Write([]byte{0})
	if req.CacheKey != "" {
		h.Write([]byte(req.CacheKey))
	} else {
		enc, _ := json.Marshal(req.Messages)
		h.Write(enc)
		if req.Temperature != nil {
			enc, _ = json.Marshal(*req.Temperature)
			h.Write(enc)
		}
		if req.JSONMode {
			h.Write([]byte("json"))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// safeName reduces an arbitrary cache key to a fixed-length hex name.
//
// The key is not always a hash. websearch builds one from the search query,
// which comes from strategy code — so before this existed, a strategy could
// call ctx.news("../../../../etc/whatever") and the key went into
// filepath.Join unaltered, writing a JSON file wherever the traversal landed.
// A strategy has no filesystem access by design; the cache was handing it
// one, and the sandbox is the reason anyone can run untrusted generated code
// here at all.
//
// Hashing also fixes the lesser bug that a key shorter than two characters
// panicked when it was sliced for the shard directory.
func safeName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (c *Cache) path(key string) string {
	name := safeName(key)
	// Shard by the first two hex characters to keep directory sizes sane;
	// a long backtest can produce thousands of entries.
	return filepath.Join(c.dir, name[:2], name+".json")
}

type cacheEntry struct {
	Text string `json:"text"`
}

// Get returns a cached completion.
func (c *Cache) Get(key string) (string, bool) {
	c.mu.RLock()
	v, ok := c.mem[key]
	c.mu.RUnlock()
	if ok {
		c.mu.Lock()
		c.hits++
		c.mu.Unlock()
		return v, true
	}

	b, err := os.ReadFile(c.path(key))
	if err != nil {
		c.mu.Lock()
		c.misses++
		c.mu.Unlock()
		return "", false
	}
	var e cacheEntry
	if err := json.Unmarshal(b, &e); err != nil {
		c.mu.Lock()
		c.misses++
		c.mu.Unlock()
		return "", false
	}
	c.mu.Lock()
	c.mem[key] = e.Text
	c.hits++
	c.mu.Unlock()
	return e.Text, true
}

// Put stores a completion.
func (c *Cache) Put(key, text string) error {
	c.mu.Lock()
	c.mem[key] = text
	c.mu.Unlock()

	p := c.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(cacheEntry{Text: text})
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Stats reports hit and miss counters.
func (c *Cache) Stats() (hits, misses int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hits, c.misses
}

// Clear removes every cached completion.
func (c *Cache) Clear() error {
	c.mu.Lock()
	c.mem = map[string]string{}
	c.hits, c.misses = 0, 0
	c.mu.Unlock()
	return os.RemoveAll(c.dir)
}
