// Package config loads natural-quant settings from environment variables,
// an optional JSON config file, and built-in defaults (in that order of
// precedence: env > file > default).
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
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
	// Enabled is false when no API key is configured, or — for a local
	// provider — when nothing is answering at its address.
	Enabled bool `json:"enabled"`
	// Local marks a provider that runs on this machine and needs no key.
	//
	// This is what lets natural-quant work with no account at all: point it
	// at Ollama or LM Studio and the plain-English compiler, which is
	// otherwise the one feature a key gates, works for free.
	Local bool `json:"local,omitempty"`
	// Detected records that a local provider actually answered a probe.
	Detected bool `json:"detected,omitempty"`
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
			// Local runtimes, both of which speak the OpenAI protocol on a
			// fixed port. They are probed rather than assumed, so a machine
			// without one is unaffected.
			"ollama": {
				Name:    "ollama",
				BaseURL: "http://127.0.0.1:11434/v1",
				Model:   "", // chosen from whatever is actually pulled
				Local:   true,
			},
			"lmstudio": {
				Name:    "lmstudio",
				BaseURL: "http://127.0.0.1:1234/v1",
				Model:   "",
				Local:   true,
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
		// A local runtime needs no key. Whether it is actually running is
		// settled by DetectLocal, not here.
		p.Enabled = p.APIKey != "" || (p.Local && p.Detected)
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

// AnyCloudProviderEnabled reports whether a keyed, paid provider is
// configured, as opposed to something running on this machine.
func (c *Config) AnyCloudProviderEnabled() bool {
	for _, p := range c.Providers {
		if p.Enabled && !p.Local {
			return true
		}
	}
	return false
}

// LocalProviders lists the providers that run on this machine, sorted.
func (c *Config) LocalProviders() []*Provider {
	var out []*Provider
	for _, p := range c.Providers {
		if p.Local {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DetectLocal probes each local runtime and enables the ones that answer.
//
// The probe is deliberately short. It runs on every startup, and a machine
// with nothing listening should not pay for the check — a refused connection
// on loopback returns immediately, and the timeout only matters for the
// pathological case of something accepting and never replying.
//
// When no paid provider is configured and a local one answers, every tier is
// routed to it. That is the whole point: the tool then works with no account,
// which is the difference between trying it and not.
func (c *Config) DetectLocal(ctx context.Context) {
	type result struct {
		name   string
		models []string
	}
	found := make(chan result, len(c.Providers))
	var wg sync.WaitGroup

	for _, p := range c.LocalProviders() {
		if p.BaseURL == "" {
			continue
		}
		wg.Add(1)
		go func(p *Provider) {
			defer wg.Done()
			models := probeLocalModels(ctx, p.BaseURL)
			if len(models) > 0 {
				found <- result{p.Name, models}
			}
		}(p)
	}
	wg.Wait()
	close(found)

	var detected []string
	for r := range found {
		p := c.Providers[r.name]
		p.Detected = true
		p.Enabled = true
		if p.Model == "" {
			p.Model = pickLocalModel(r.models)
		}
		detected = append(detected, r.name)
	}
	if len(detected) == 0 || c.AnyCloudProviderEnabled() {
		return
	}
	sort.Strings(detected)
	for _, tier := range []Tier{TierFast, TierBalanced, TierQuality} {
		c.Routes[tier] = detected[0]
	}
}

// probeLocalModels asks a local runtime what it has loaded.
func probeLocalModels(ctx context.Context, baseURL string) []string {
	ctx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil
	}
	out := make([]string, 0, len(doc.Data))
	for _, m := range doc.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	sort.Strings(out)
	return out
}

// pickLocalModel chooses the most suitable model from what is installed.
//
// Compiling a strategy is a code-generation task, so a code-tuned or
// instruction-tuned model is preferred over a base one, and a larger
// parameter count over a smaller one. This is a heuristic over model names
// because there is nothing else to go on, and it is only a default: the user
// can always pin one with NQ_OLLAMA_MODEL.
func pickLocalModel(models []string) string {
	if len(models) == 0 {
		return ""
	}
	best, bestScore := models[0], -1
	for _, m := range models {
		lower := strings.ToLower(m)
		score := 0
		for _, hint := range []string{"coder", "code", "qwen", "deepseek", "llama", "mistral", "instruct"} {
			if strings.Contains(lower, hint) {
				score += 2
			}
		}
		// Embedding models cannot answer a chat completion at all.
		for _, bad := range []string{"embed", "bge-", "nomic", "vision", "clip"} {
			if strings.Contains(lower, bad) {
				score -= 10
			}
		}
		// Prefer bigger, where the name says so.
		for _, size := range []struct {
			token string
			bonus int
		}{{"70b", 5}, {"32b", 4}, {"27b", 4}, {"14b", 3}, {"13b", 3}, {"8b", 2}, {"7b", 2}, {"3b", 1}} {
			if strings.Contains(lower, size.token) {
				score += size.bonus
				break
			}
		}
		if score > bestScore {
			best, bestScore = m, score
		}
	}
	return best
}

// CacheDir returns a named subdirectory of the data dir, creating it.
func (c *Config) CacheDir(name string) (string, error) {
	d := filepath.Join(c.DataDir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return d, nil
}
