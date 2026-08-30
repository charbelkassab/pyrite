package market

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DiskCache persists fetched series as gzip-free JSON files, one per symbol.
//
// Daily bars are immutable once printed, so a simple whole-series file with a
// freshness stamp is enough: re-fetching only happens when the requested
// window extends beyond what is stored, or when today's bar may have changed.
type DiskCache struct {
	dir string
	mu  sync.Mutex
	// TTL controls how long the most recent bar is trusted before a refetch
	// is allowed. Historical bars never expire.
	TTL time.Duration
}

type cacheFile struct {
	Symbol    string    `json:"symbol"`
	Name      string    `json:"name,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
	// RequestedFrom is the earliest date ever asked for. A symbol that
	// listed in 1993 has no bars before then, so comparing the requested
	// window against the first bar would report a miss forever and refetch
	// the whole history on every run. This records what was asked, not what
	// came back.
	RequestedFrom Day   `json:"requested_from,omitempty"`
	Bars          []Bar `json:"bars"`
}

// NewDiskCache creates the cache directory if needed.
func NewDiskCache(dir string) (*DiskCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &DiskCache{dir: dir, TTL: 6 * time.Hour}, nil
}

// safeName maps a ticker to a filesystem-safe file name. Tickers contain
// characters like ^ = - . / which are either illegal or awkward in paths.
func safeName(symbol string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(symbol) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == '^':
			b.WriteString("IDX_")
		case r == '=':
			b.WriteString("_F_")
		case r == '.':
			b.WriteString("_D_")
		default:
			b.WriteString("_")
		}
	}
	return b.String()
}

func (c *DiskCache) path(symbol string) string {
	return filepath.Join(c.dir, safeName(symbol)+".json")
}

// Load reads a cached series, returning nil if absent. The second result is
// the earliest date previously requested for this symbol.
func (c *DiskCache) Load(symbol string) (*Series, Day, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	b, err := os.ReadFile(c.path(symbol))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	var cf cacheFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, "", nil // corrupt cache entry: treat as a miss
	}
	if len(cf.Bars) == 0 {
		return nil, "", nil
	}
	s := NewSeries(symbol, cf.Bars)
	s.Name = cf.Name
	return s, cf.RequestedFrom, nil
}

// Save writes a series to disk atomically, remembering the earliest window
// requested so far.
func (c *DiskCache) Save(s *Series, requestedFrom Day) error {
	if s == nil || len(s.Bars) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	b, err := json.Marshal(cacheFile{
		Symbol:        s.Symbol,
		Name:          s.Name,
		FetchedAt:     time.Now().UTC(),
		RequestedFrom: requestedFrom,
		Bars:          s.Bars,
	})
	if err != nil {
		return err
	}
	final := c.path(s.Symbol)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// Clear removes every cached series.
func (c *DiskCache) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			_ = os.Remove(filepath.Join(c.dir, e.Name()))
		}
	}
	return nil
}

// Stats reports how many symbols are cached and the total size on disk.
func (c *DiskCache) Stats() (symbols int, bytes int64) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		symbols++
		if info, err := e.Info(); err == nil {
			bytes += info.Size()
		}
	}
	return symbols, bytes
}
