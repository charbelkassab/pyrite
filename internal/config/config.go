// Package config loads natural-quant settings from environment variables,
// an optional JSON config file, and built-in defaults (in that order of
// precedence: env > file > default).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Tier selects the speed/price/quality trade-off for an LLM call.
type Tier string

const (
	// TierFast optimises for latency and cost. Used for the thousands of
	// per-day ai() calls a strategy may make during a backtest.
	TierFast Tier = "fast"
	// TierBalanced is a middle ground: good code, moderate price.
	TierBalanced Tier = "balanced"
	// TierQuality optimises for correctness. Used to compile a natural
	// language strategy into code, which happens once and must be right.
	TierQuality Tier = "quality"
)

// Provider describes one OpenAI-compatible LLM endpoint.
type Provider struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"-"` // never serialised
	// Model used when this provider serves a given tier.
	Model string `json:"model"`
	// Enabled is false when no API key is configured.
	Enabled bool `json:"enabled"`
}

// Config is the fully resolved application configuration.
type Config struct {
	// Addr is the host:port the web server listens on.
	Addr string `json:"addr"`
	// DataDir holds the market data cache, AI response cache and saved runs.
	DataDir string `json:"data_dir"`

	Providers map[string]*Provider `json:"providers"`
	// Routes maps a tier to a provider name.
	Routes map[Tier]string `json:"routes"`

	// MaxAICallsPerRun caps how many ai()/web() calls a single backtest may
	// make. Prevents a runaway strategy from burning an API budget.
	MaxAICallsPerRun int `json:"max_ai_calls_per_run"`
	// StrategyTimeout is the wall-clock budget for a whole backtest, seconds.
	StrategyTimeoutSec int `json:"strategy_timeout_sec"`
	// OfflineMode forces the synthetic data provider and disables all
	// network access. Useful for demos, CI and offline development.
	OfflineMode bool `json:"offline_mode"`
	// CacheOnly serves market data only from the local cache (no fetches).
	CacheOnly bool `json:"cache_only"`
	// SearchProvider selects the web search backend: "duckduckgo" or "none".
	SearchProvider string `json:"search_provider"`

	// DataProviders is the ordered fallback chain for market data, e.g.
	// "yahoo,stooq". A vendor that works for most symbols and quietly fails
	// on a few is the normal case for free endpoints, and dropping those
	// names silently changes the backtest — so the next vendor is tried for
	// exactly the symbols that failed.
	DataProviders []string `json:"data_providers"`
	// CSVDir, when set, adds a local directory of per-symbol CSV files to the
	// chain. It is the only source that can hold delisted securities, which
	// no free live endpoint serves.
	CSVDir string `json:"csv_dir,omitempty"`
}

// Defaults returns the built-in configuration.
//
// Model choices reflect the speed/price/quality trade-off:
//   - Cerebras gpt-oss-120b runs at roughly 3000 tokens/sec, which makes it
//     the only sane choice for per-day in-strategy AI calls.
//   - Kimi is strong at code generation for a fraction of frontier pricing.
//   - OpenAI is the default for compiling a strategy, where being wrong is
//     far more expensive than being slow.
func Defaults() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		Addr:    "127.0.0.1:8080",
		DataDir: filepath.Join(home, ".natural-quant"),
		Providers: map[string]*Provider{
			"openai": {
				Name:    "openai",
				BaseURL: "https://api.openai.com/v1",
				Model:   "gpt-5.5",
			},
			"cerebras": {
				Name:    "cerebras",
				BaseURL: "https://api.cerebras.ai/v1",
				Model:   "gpt-oss-120b",
			},
			"kimi": {
				Name:    "kimi",
				BaseURL: "https://api.moonshot.ai/v1",
				Model:   "kimi-k2.7-code-highspeed",
			},
		},
		Routes: map[Tier]string{
			TierFast:     "cerebras",
			TierBalanced: "kimi",
			TierQuality:  "openai",
		},
		MaxAICallsPerRun: 2000,
		// A strategy calling the model once per simulated day makes hundreds
		// of sequential network round trips on its first run, which can take
		// tens of minutes. Later runs are served from the reply cache and
		// finish in seconds, so the budget only needs to be generous once.
		StrategyTimeoutSec: 3600,
		SearchProvider:     "duckduckgo",
		DataProviders:      []string{"yahoo", "stooq"},
	}
}

// Load resolves configuration from defaults, then an optional JSON file, then
// the environment. path may be empty, in which case the default location
// $DATA_DIR/config.json is tried and silently ignored if missing.
func Load(path string) (*Config, error) {
	cfg := Defaults()

	if path == "" {
		path = filepath.Join(cfg.DataDir, "config.json")
	}
	if b, err := os.ReadFile(path); err == nil {
		// Decode over the defaults so partial config files work.
		if err := json.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg.applyEnv()

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return cfg, nil
}

// envKeys lists, per provider, the environment variables checked for an API
// key. The first non-empty one wins.
var envKeys = map[string][]string{
	"openai":   {"OPENAI_API_KEY"},
	"cerebras": {"CEREBRAS_API_KEY"},
	"kimi":     {"KIMI_API_KEY", "MOONSHOT_API_KEY"},
}

func (c *Config) applyEnv() {
	for name, p := range c.Providers {
		for _, k := range envKeys[name] {
			if v := strings.TrimSpace(os.Getenv(k)); v != "" {
				p.APIKey = v
				break
			}
		}
		// Allow per-provider overrides, e.g. NQ_CEREBRAS_MODEL.
		up := strings.ToUpper(name)
		if v := os.Getenv("NQ_" + up + "_MODEL"); v != "" {
			p.Model = v
		}
		if v := os.Getenv("NQ_" + up + "_BASE_URL"); v != "" {
			p.BaseURL = v
		}
		if v := os.Getenv("NQ_" + up + "_API_KEY"); v != "" {
			p.APIKey = v
		}
		p.Enabled = p.APIKey != ""
	}

	if v := os.Getenv("NQ_ADDR"); v != "" {
		c.Addr = v
	}
	if v := os.Getenv("NQ_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("NQ_DATA_PROVIDERS"); v != "" {
		var names []string
		for _, part := range strings.Split(v, ",") {
			if part = strings.ToLower(strings.TrimSpace(part)); part != "" {
				names = append(names, part)
			}
		}
		if len(names) > 0 {
			c.DataProviders = names
		}
	}
	if v := strings.TrimSpace(os.Getenv("NQ_CSV_DIR")); v != "" {
		c.CSVDir = v
	}
	if v := os.Getenv("NQ_SEARCH_PROVIDER"); v != "" {
		c.SearchProvider = v
	}
	for tier, env := range map[Tier]string{
		TierFast:     "NQ_ROUTE_FAST",
		TierBalanced: "NQ_ROUTE_BALANCED",
		TierQuality:  "NQ_ROUTE_QUALITY",
	} {
		if v := os.Getenv(env); v != "" {
			c.Routes[tier] = v
		}
	}
	if v := os.Getenv("NQ_MAX_AI_CALLS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxAICallsPerRun = n
		}
	}
	if boolEnv("NQ_OFFLINE") {
		c.OfflineMode = true
	}
	if boolEnv("NQ_CACHE_ONLY") {
		c.CacheOnly = true
	}
}

func boolEnv(k string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// ResolveTier returns the provider that should serve the given tier, falling
// back through the remaining tiers and then to any enabled provider. It
// returns nil when no provider has an API key configured.
func (c *Config) ResolveTier(t Tier) *Provider {
	if name, ok := c.Routes[t]; ok {
		if p, ok := c.Providers[name]; ok && p.Enabled {
			return p
		}
	}
	// Fall back in an order that degrades gracefully: a quality request is
	// better served by a fast model than by no model at all.
	for _, alt := range []Tier{TierQuality, TierBalanced, TierFast} {
		if alt == t {
			continue
		}
		if name, ok := c.Routes[alt]; ok {
			if p, ok := c.Providers[name]; ok && p.Enabled {
				return p
			}
		}
	}
	for _, p := range c.Providers {
		if p.Enabled {
			return p
		}
	}
	return nil
}

// AnyProviderEnabled reports whether at least one API key is configured.
func (c *Config) AnyProviderEnabled() bool {
	for _, p := range c.Providers {
		if p.Enabled {
			return true
		}
	}
	return false
}

// CacheDir returns a named subdirectory of the data dir, creating it.
func (c *Config) CacheDir(name string) (string, error) {
	d := filepath.Join(c.DataDir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return d, nil
}
