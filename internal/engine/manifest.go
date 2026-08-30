package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime"
	"sort"
	"time"

	"github.com/charbelkassab/pyrite/internal/market"
)

// Version is the build identity of the binary, set from main at startup so
// that a saved run records which build produced it.
var Version = "dev"

// SymbolCoverage records exactly what data a symbol contributed.
//
// "I ran it over 2015 to 2024" is not reproducible on its own: a vendor that
// silently returns a short history turns the same request into a different
// backtest. Recording the first and last bar actually used makes that visible
// on re-run instead of invisible.
type SymbolCoverage struct {
	FirstBar market.Day `json:"first_bar"`
	LastBar  market.Day `json:"last_bar"`
	Bars     int        `json:"bars"`
}

// Manifest is everything needed to know whether two runs are comparable.
//
// A backtest result without provenance is a screenshot. With it, a re-run
// either reproduces exactly or reports precisely which input moved — the data
// vendor, the model, the cost model, or the code.
type Manifest struct {
	Version   string    `json:"version"`
	GoVersion string    `json:"go_version"`
	CreatedAt time.Time `json:"created_at"`

	// Data provenance.
	DataProvider string                    `json:"data_provider"`
	Coverage     map[string]SymbolCoverage `json:"coverage,omitempty"`
	// CalendarDays is the number of sessions in the union calendar.
	CalendarDays int        `json:"calendar_days"`
	CalendarFrom market.Day `json:"calendar_from,omitempty"`
	CalendarTo   market.Day `json:"calendar_to,omitempty"`

	// Strategy provenance. Hashes rather than bodies, so a manifest stays
	// small enough to sit on every result.
	CodeSHA256   string `json:"code_sha256"`
	PromptSHA256 string `json:"prompt_sha256,omitempty"`
	// CompilerProvider and CompilerModel identify what wrote the code.
	CompilerProvider string `json:"compiler_provider,omitempty"`
	CompilerModel    string `json:"compiler_model,omitempty"`

	// Execution parameters that change results.
	Fill            FillModel `json:"fill"`
	Costs           Costs     `json:"costs"`
	Seed            int64     `json:"seed"`
	InitialCash     float64   `json:"initial_cash"`
	AllowShort      bool      `json:"allow_short"`
	AllowFractional bool      `json:"allow_fractional"`
	MaxLeverage     float64   `json:"max_leverage"`
	RiskFreeRate    float64   `json:"risk_free_rate"`
	Warmup          int       `json:"warmup"`

	// In-run model usage. Listed because a strategy that consulted a model is
	// only reproducible while the AI cache survives, and the reader deserves
	// to know that.
	AICallCount int      `json:"ai_call_count"`
	AIModels    []string `json:"ai_models,omitempty"`
	AICacheHits int      `json:"ai_cache_hits"`
}

// Reproducible reports whether a re-run should produce identical numbers.
//
// A run that called a model and missed cache is not reproducible: the reply
// was generated once and may differ next time. Everything else in this engine
// is deterministic given the same data.
func (m Manifest) Reproducible() bool {
	return m.AICallCount == 0 || m.AICacheHits == m.AICallCount
}

// hashString returns the hex SHA-256 of s, or "" for empty input.
func hashString(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// buildManifest assembles the manifest from engine state after a run.
func (e *Engine) buildManifest(res *Result) Manifest {
	m := Manifest{
		Version:         Version,
		GoVersion:       runtime.Version(),
		CreatedAt:       time.Now().UTC(),
		CodeSHA256:      hashString(e.spec.Code),
		PromptSHA256:    hashString(e.spec.Prompt),
		Fill:            e.spec.Fill,
		Costs:           e.spec.Costs,
		Seed:            e.spec.Seed,
		InitialCash:     e.spec.InitialCash,
		AllowShort:      e.spec.AllowShort,
		AllowFractional: e.spec.AllowFractional,
		MaxLeverage:     e.spec.MaxLeverage,
		RiskFreeRate:    e.spec.RiskFreeRate,
		Warmup:          e.spec.Warmup,
		AICallCount:     e.aiCalls,
		Coverage:        map[string]SymbolCoverage{},
	}
	if e.store != nil {
		m.DataProvider = e.store.ProviderName()
	}
	for sym, s := range e.series {
		if s == nil || len(s.Bars) == 0 {
			continue
		}
		m.Coverage[sym] = SymbolCoverage{
			FirstBar: s.Bars[0].Date,
			LastBar:  s.Bars[len(s.Bars)-1].Date,
			Bars:     len(s.Bars),
		}
	}
	m.CalendarDays = len(e.days)
	if len(e.days) > 0 {
		m.CalendarFrom, m.CalendarTo = e.days[0], e.days[len(e.days)-1]
	}

	m.AICacheHits = e.aiCacheHits
	for k := range e.aiModels {
		m.AIModels = append(m.AIModels, k)
	}
	sort.Strings(m.AIModels)
	return m
}
