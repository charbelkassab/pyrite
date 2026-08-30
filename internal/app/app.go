// Package app wires the pieces of natural-quant together: configuration,
// market data, the model router, web search, the strategy compiler and the
// backtest engine.
package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charbelkassab/natural-quant/internal/config"
	"github.com/charbelkassab/natural-quant/internal/engine"
	"github.com/charbelkassab/natural-quant/internal/llm"
	"github.com/charbelkassab/natural-quant/internal/market"
	"github.com/charbelkassab/natural-quant/internal/strategy"
	"github.com/charbelkassab/natural-quant/internal/websearch"
)

// App holds the long-lived services.
type App struct {
	Cfg      *config.Config
	Store    *market.Store
	LLM      *llm.Client
	Compiler *strategy.Compiler
	Search   *websearch.Searcher
	AICache  *llm.Cache
}

// New constructs the application from configuration.
func New(cfg *config.Config) (*App, error) {
	cacheDir, err := cfg.CacheDir("market-cache")
	if err != nil {
		return nil, err
	}
	diskCache, err := market.NewDiskCache(cacheDir)
	if err != nil {
		return nil, err
	}

	fund, err := market.LoadFundamentals(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("load fundamentals: %w", err)
	}

	provider := buildProvider(cfg)
	store := market.NewStore(provider, diskCache, fund)
	store.SetDataDir(cfg.DataDir)

	aiCacheDir, err := cfg.CacheDir("ai-cache")
	if err != nil {
		return nil, err
	}
	aiCache, err := llm.NewCache(aiCacheDir)
	if err != nil {
		return nil, err
	}
	client := llm.New(cfg, aiCache)

	searchEnabled := !cfg.OfflineMode && cfg.SearchProvider != "" && cfg.SearchProvider != "none"

	searchCacheDir, err := cfg.CacheDir("search-cache")
	if err != nil {
		return nil, err
	}
	searcher, err := websearch.NewWithCache(searchEnabled, searchCacheDir)
	if err != nil {
		return nil, err
	}

	return &App{
		Cfg:      cfg,
		Store:    store,
		LLM:      client,
		Compiler: strategy.NewCompiler(client, store),
		Search:   searcher,
		AICache:  aiCache,
	}, nil
}

// buildProvider assembles the market data source from configuration.
//
// Offline mode is absolute: it swaps in synthetic data and never reaches the
// network, which is what makes the whole tool usable and testable with no
// keys and no connection.
func buildProvider(cfg *config.Config) market.Provider {
	if cfg.OfflineMode {
		return market.NewSyntheticProvider()
	}

	names := cfg.DataProviders
	if len(names) == 0 {
		names = []string{"yahoo"}
	}
	var chain []market.Provider
	// A local directory always goes first when configured: it is explicit,
	// free, instant, and the only place delisted names can come from.
	if cfg.CSVDir != "" {
		chain = append(chain, market.NewCSVProvider(cfg.CSVDir))
	}
	for _, name := range names {
		switch name {
		case "yahoo":
			chain = append(chain, market.NewYahooProvider())
		case "stooq":
			chain = append(chain, market.NewStooqProvider())
		case "csv":
			if cfg.CSVDir == "" {
				continue // already added above when configured
			}
		case "synthetic":
			chain = append(chain, market.NewSyntheticProvider())
		}
	}
	if len(chain) == 0 {
		chain = append(chain, market.NewYahooProvider())
	}
	if len(chain) == 1 {
		return chain[0]
	}
	return market.NewChain(chain...)
}

// RunOptions are the knobs a caller may set for a backtest.
type RunOptions struct {
	Start       market.Day
	End         market.Day
	InitialCash float64
	Benchmarks  []string
	Universe    []string
	// Index names a point-in-time index universe such as "sp500", resolved
	// per session rather than flattened to a fixed list.
	Index       string
	Fill        engine.FillModel
	Costs       *engine.Costs
	MaxLeverage float64
	RiskFree    float64
	Params      map[string]any
	Progress    engine.ProgressFunc
	// AITier overrides which model tier serves in-strategy ai() calls. The
	// default is the fast tier: these calls happen once per simulated day, so
	// latency and price dominate.
	AITier config.Tier
}

// MaxAIYears bounds the automatic full-history window for strategies that call
// a model or the web on a schedule. Running one of those over thirty years
// would make tens of thousands of network calls, blow past the per-run budget
// and quietly return nothing for most of the period.
const MaxAIYears = 6

// ClampForAI shortens a full-history window for AI-driven strategies, and
// reports whether it did.
func ClampForAI(opts *RunOptions, needsAI bool) bool {
	if !needsAI || opts.Start != "" {
		return false
	}
	opts.Start = opts.End.Add(-365 * MaxAIYears)
	return true
}

// ApplyDefaults fills unset run options.
func (o *RunOptions) ApplyDefaults() {
	if o.End == "" {
		o.End = market.NewDay(time.Now())
	}
	// Start is deliberately left empty when unset. The engine reads that as
	// "begin as early as the data allows", which is almost always what someone
	// comparing strategies wants; the chart is where you narrow the window.
	if o.InitialCash <= 0 {
		o.InitialCash = 100000
	}
	if o.Fill == "" {
		o.Fill = engine.FillNextOpen
	}
	if o.AITier == "" {
		o.AITier = config.TierFast
	}
}

// BuildSpec turns a compiled plan plus options into an engine spec.
func BuildSpec(plan *strategy.Plan, prompt string, opts RunOptions) engine.Spec {
	opts.ApplyDefaults()

	universe := plan.Universe
	if len(opts.Universe) > 0 {
		universe = market.DedupeSymbols(opts.Universe)
	}
	// A point-in-time index cannot be flattened to a fixed list here, because
	// which symbols it holds depends on the day. It travels on the spec and
	// the engine resolves it per session.
	index := opts.Index
	if index == "" {
		for _, u := range append(append([]string{}, opts.Universe...), plan.Universe...) {
			if idx := market.IndexUniverse(u); idx != "" {
				index = idx
				break
			}
		}
	}
	if index != "" {
		// Anything else named alongside the index stays as an extra symbol,
		// so "sp500 plus TLT" works.
		kept := universe[:0]
		for _, u := range universe {
			if market.IndexUniverse(u) == "" {
				kept = append(kept, u)
			}
		}
		universe = kept
	}
	benchmarks := plan.Benchmarks
	if len(opts.Benchmarks) > 0 {
		benchmarks = market.DedupeSymbols(opts.Benchmarks)
	}

	spec := engine.Spec{
		Name:            plan.Name,
		Prompt:          prompt,
		Code:            plan.Code,
		Universe:        universe,
		Benchmarks:      benchmarks,
		Start:           opts.Start,
		End:             opts.End,
		InitialCash:     opts.InitialCash,
		Fill:            opts.Fill,
		AllowShort:      plan.AllowShort,
		AllowFractional: true,
		MaxLeverage:     opts.MaxLeverage,
		RiskFreeRate:    opts.RiskFree,
		Warmup:          plan.Warmup,
		Params:          opts.Params,
		Index:           index,
	}
	if opts.Costs != nil {
		spec.Costs = *opts.Costs
	}
	spec.ApplyDefaults()
	return spec
}

// Backtest runs a spec, wiring in the AI and search callbacks.
func (a *App) Backtest(ctx context.Context, spec engine.Spec, opts RunOptions) (*engine.Result, error) {
	opts.ApplyDefaults()

	eng := engine.New(spec, a.Store)
	eng.MaxAICalls = a.Cfg.MaxAICallsPerRun
	eng.Progress = opts.Progress

	if a.Cfg.AnyProviderEnabled() {
		eng.AI = a.makeAIFunc(opts.AITier)
	}
	if a.Search != nil && a.Search.Enabled {
		eng.Search = a.Search.Search
	}
	return eng.Run(ctx)
}

// makeAIFunc builds the ctx.ai() backend.
//
// The cache key is the simulated day plus the prompt. That makes a strategy's
// model calls deterministic across re-runs, which is what allows an AI-driven
// backtest to be compared fairly against itself after a parameter change.
func (a *App) makeAIFunc(tier config.Tier) engine.AIFunc {
	return func(ctx context.Context, day market.Day, prompt string, opts engine.AIOptions) (string, string, string, bool, error) {
		t := tier
		if opts.Tier != "" {
			t = config.Tier(opts.Tier)
		}
		system := opts.System
		if system == "" {
			system = "You are a trading assistant inside a backtest. " +
				"The simulated date is " + string(day) + ". " +
				"Answer concisely and decisively. When asked for a single word or a number, reply with only that."
		}
		// Reasoning models emit their reasoning before any visible answer, so
		// a tiny budget yields an empty reply rather than a short one. A
		// strategy asking for a single word will naturally request very few
		// tokens, so treat its value as a hint and keep enough headroom.
		maxTokens := opts.MaxTokens
		if maxTokens < minAITokens {
			maxTokens = minAITokens
		}
		temp := 0.0
		resp, err := a.LLM.Complete(ctx, llm.Request{
			Tier: t,
			Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: system},
				{Role: llm.RoleUser, Content: prompt},
			},
			Temperature: &temp,
			MaxTokens:   maxTokens,
			JSONMode:    opts.JSON,
			// Pinning the key to the simulated day keeps re-runs free and
			// byte-identical.
			CacheKey: string(day) + "\x00" + prompt + "\x00" + fmt.Sprint(opts.JSON),
		})
		if err != nil {
			return "", "", "", false, err
		}
		return resp.Text, resp.Provider, resp.Model, resp.Cached, nil
	}
}

// minAITokens is the floor applied to in-strategy model calls.
const minAITokens = 512

// Health describes the readiness of each subsystem, for the UI banner and the
// CLI doctor command.
type Health struct {
	Providers     []ProviderHealth  `json:"providers"`
	DataProvider  string            `json:"data_provider"`
	OfflineMode   bool              `json:"offline_mode"`
	SearchOK      bool              `json:"search_enabled"`
	Fundamentals  string            `json:"fundamentals_source"`
	CachedSymbols int               `json:"cached_symbols"`
	CacheBytes    int64             `json:"cache_bytes"`
	DataDir       string            `json:"data_dir"`
	Routes        map[string]string `json:"routes"`
}

// ProviderHealth is one model provider's status.
type ProviderHealth struct {
	Name      string   `json:"name"`
	Enabled   bool     `json:"enabled"`
	Model     string   `json:"model"`
	Reachable *bool    `json:"reachable,omitempty"`
	Models    []string `json:"models,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// Health reports subsystem status. When probe is true it makes a live call to
// each configured provider's /models endpoint.
func (a *App) Health(ctx context.Context, probe bool) Health {
	h := Health{
		DataProvider: a.Store.ProviderName(),
		OfflineMode:  a.Cfg.OfflineMode,
		SearchOK:     a.Search != nil && a.Search.Enabled,
		Fundamentals: a.Store.Fundamentals().Source(),
		DataDir:      a.Cfg.DataDir,
		Routes:       map[string]string{},
	}
	for tier, name := range a.Cfg.Routes {
		h.Routes[string(tier)] = name
	}

	for _, name := range []string{"openai", "cerebras", "kimi"} {
		p, ok := a.Cfg.Providers[name]
		if !ok {
			continue
		}
		ph := ProviderHealth{Name: name, Enabled: p.Enabled, Model: p.Model}
		if probe && p.Enabled {
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			models, err := a.LLM.ListModels(cctx, name)
			cancel()
			ok := err == nil
			ph.Reachable = &ok
			if err != nil {
				ph.Error = err.Error()
			}
			for _, m := range models {
				ph.Models = append(ph.Models, m.ID)
			}
		}
		h.Providers = append(h.Providers, ph)
	}

	if dir, err := a.Cfg.CacheDir("market-cache"); err == nil {
		if c, err := market.NewDiskCache(dir); err == nil {
			h.CachedSymbols, h.CacheBytes = c.Stats()
		}
	}
	return h
}

// ClearCaches removes cached market data, model replies, or both.
func (a *App) ClearCaches(marketData, ai bool) error {
	if marketData {
		dir, err := a.Cfg.CacheDir("market-cache")
		if err != nil {
			return err
		}
		c, err := market.NewDiskCache(dir)
		if err != nil {
			return err
		}
		if err := c.Clear(); err != nil {
			return err
		}
	}
	if ai && a.AICache != nil {
		if err := a.AICache.Clear(); err != nil {
			return err
		}
	}
	return nil
}

// RunsDir is where saved runs live.
func (a *App) RunsDir() string { return filepath.Join(a.Cfg.DataDir, "runs") }

// DescribeRoutes renders a one-line summary of tier routing, for the CLI.
func (a *App) DescribeRoutes() string {
	var parts []string
	for _, tier := range []config.Tier{config.TierFast, config.TierBalanced, config.TierQuality} {
		p := a.Cfg.ResolveTier(tier)
		if p == nil {
			parts = append(parts, fmt.Sprintf("%s=none", tier))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s/%s", tier, p.Name, p.Model))
	}
	return strings.Join(parts, "  ")
}
