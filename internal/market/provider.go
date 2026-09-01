package market

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrNotFound indicates a symbol has no data at the provider.
var ErrNotFound = errors.New("symbol not found")

// Provider fetches historical daily bars for a symbol.
//
// Three methods, deliberately. Writing one for a new vendor should be an
// afternoon, and a provider that only serves daily bars is a complete and
// useful provider.
type Provider interface {
	// Name identifies the provider in logs and the UI.
	Name() string
	// Fetch returns daily bars covering [from, to] inclusive. Providers may
	// return more than requested; callers must slice.
	Fetch(ctx context.Context, symbol string, from, to Day) (*Series, error)
	// Search resolves a free-text query to candidate symbols. Providers that
	// cannot search may return nil, nil.
	Search(ctx context.Context, query string) ([]Quote, error)
}

// IntervalProvider is an optional capability: a provider that can serve bar
// sizes other than daily.
//
// It is a separate interface rather than an extra parameter on Fetch so that
// Provider stays three methods. A vendor without intraday data — or a
// contributor who only needs daily — implements Provider and nothing else,
// and the store reports clearly when a run asks for a size nobody can serve.
type IntervalProvider interface {
	Provider
	// FetchInterval returns bars of the requested size.
	FetchInterval(ctx context.Context, symbol string, from, to Day, iv Interval) (*Series, error)
	// SupportedIntervals lists what this provider can actually serve.
	SupportedIntervals() []Interval
}

// SupportsInterval reports whether a provider can serve a bar size.
func SupportsInterval(p Provider, iv Interval) bool {
	if iv == "" || iv == Interval1d {
		return true
	}
	ip, ok := p.(IntervalProvider)
	if !ok {
		return false
	}
	for _, s := range ip.SupportedIntervals() {
		if s == iv {
			return true
		}
	}
	return false
}

// fetchAt fetches at a bar size, using the optional capability when present.
func fetchAt(ctx context.Context, p Provider, symbol string, from, to Day, iv Interval) (*Series, error) {
	if iv == "" || iv == Interval1d {
		return p.Fetch(ctx, symbol, from, to)
	}
	ip, ok := p.(IntervalProvider)
	if !ok {
		return nil, fmt.Errorf("%s serves daily bars only, so %s is unavailable", p.Name(), iv)
	}
	return ip.FetchInterval(ctx, symbol, from, to, iv)
}

// Store is a Provider wrapped with an in-memory and on-disk cache. It is the
// type the rest of the application depends on.
type Store struct {
	provider Provider
	cache    *DiskCache
	fund     *Fundamentals
	// members holds lazily loaded point-in-time index membership tables,
	// keyed by index name.
	memberMu sync.Mutex
	members  map[string]*Membership
	// dataDir lets a user override a bundled membership table.
	dataDir string

	mu sync.RWMutex
	// Keyed by symbol and bar size together: 5-minute AAPL and daily AAPL are
	// different series, and one must never be served for the other.
	mem  map[string]*Series
	cold map[string]bool // symbols known to have no data, to avoid refetch
	// span records the earliest date already requested per key, so a
	// full-history request is served from cache rather than refetched every
	// run just because the symbol has no bars back that far.
	span map[string]Day
}

// NewStore builds a caching store around a provider.
func NewStore(p Provider, cache *DiskCache, fund *Fundamentals) *Store {
	return &Store{
		provider: p,
		cache:    cache,
		fund:     fund,
		mem:      map[string]*Series{},
		cold:     map[string]bool{},
		span:     map[string]Day{},
	}
}

// SupportsInterval reports whether the configured provider can serve a size.
func (s *Store) SupportsInterval(iv Interval) bool { return SupportsInterval(s.provider, iv) }

// ProviderName reports the underlying provider.
func (s *Store) ProviderName() string { return s.provider.Name() }

// Fundamentals exposes the share-count table used for market-cap ranking.
func (s *Store) Fundamentals() *Fundamentals { return s.fund }

// SetDataDir tells the store where to look for user overrides of bundled
// reference data.
func (s *Store) SetDataDir(dir string) { s.dataDir = dir }

// SetMembership installs a constituent table, so that nothing is loaded from
// disk or from the embedded copy for that index.
//
// A reproducibility bundle carries the table the original run used. Without
// this, re-running a bundle on a machine with its own membership override
// would silently trade a different universe and blame the difference on the
// strategy.
func (s *Store) SetMembership(m *Membership) {
	if m == nil {
		return
	}
	name := IndexUniverse(m.Index)
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(m.Index))
	}
	s.memberMu.Lock()
	defer s.memberMu.Unlock()
	if s.members == nil {
		s.members = map[string]*Membership{}
	}
	s.members[name] = m
}

// Membership returns the point-in-time constituent table for an index,
// loading it on first use.
func (s *Store) Membership(index string) (*Membership, error) {
	name := IndexUniverse(index)
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(index))
	}
	s.memberMu.Lock()
	defer s.memberMu.Unlock()
	if s.members == nil {
		s.members = map[string]*Membership{}
	}
	if m, ok := s.members[name]; ok {
		return m, nil
	}
	m, err := LoadMembership(name, s.dataDir)
	if err != nil {
		return nil, err
	}
	s.members[name] = m
	return m, nil
}

// Get returns the full cached series for a symbol, fetching if needed.
//
// The store always fetches and caches the widest range it has seen for a
// symbol, so repeated backtests over overlapping windows hit the cache.
func (s *Store) Get(ctx context.Context, symbol string, from, to Day) (*Series, error) {
	return s.GetInterval(ctx, symbol, from, to, Interval1d)
}

// GetInterval is Get at a specific bar size.
func (s *Store) GetInterval(ctx context.Context, symbol string, from, to Day, iv Interval) (*Series, error) {
	symbol = NormalizeSymbol(symbol)
	if iv == "" {
		iv = Interval1d
	}
	key := seriesKey(symbol, iv)

	s.mu.RLock()
	if s.cold[key] {
		s.mu.RUnlock()
		return nil, fmt.Errorf("%w: %s", ErrNotFound, symbol)
	}
	ser, ok := s.mem[key]
	haveFrom := s.span[key]
	s.mu.RUnlock()
	if ok && covers(ser, haveFrom, from, to) {
		return ser, nil
	}

	// Try the on-disk cache before hitting the network.
	if s.cache != nil && ser == nil {
		if cached, cachedFrom, err := s.cache.Load(key); err == nil && cached != nil {
			ser = cached
			haveFrom = cachedFrom
			s.mu.Lock()
			s.mem[key] = cached
			s.span[key] = cachedFrom
			s.mu.Unlock()
			if covers(cached, cachedFrom, from, to) {
				return cached, nil
			}
		}
	}

	fetched, err := fetchAt(ctx, s.provider, symbol, from, to, iv)
	if err != nil {
		// A stale-but-usable cache beats a hard failure when offline.
		if ser != nil {
			return ser, nil
		}
		if errors.Is(err, ErrNotFound) {
			s.mu.Lock()
			s.cold[key] = true
			s.mu.Unlock()
		}
		return nil, err
	}

	merged := merge(ser, fetched)
	widest := from
	if haveFrom != "" && haveFrom < widest {
		widest = haveFrom
	}
	s.mu.Lock()
	s.mem[key] = merged
	s.span[key] = widest
	s.mu.Unlock()
	if s.cache != nil {
		_ = s.cache.Save2(key, merged, widest)
	}
	return merged, nil
}

// seriesKey identifies a symbol at a bar size.
//
// Daily keeps the bare symbol so existing cache files stay valid and the
// common case has no suffix to read past.
func seriesKey(symbol string, iv Interval) string {
	if iv == "" || iv == Interval1d {
		return symbol
	}
	return symbol + "@" + string(iv)
}

// GetMany fetches several symbols concurrently, returning the successes and a
// map of per-symbol errors. A single bad ticker must not fail a whole run.
func (s *Store) GetMany(ctx context.Context, symbols []string, from, to Day) (map[string]*Series, map[string]error) {
	return s.GetManyInterval(ctx, symbols, from, to, Interval1d)
}

// GetManyInterval is GetMany at a specific bar size.
func (s *Store) GetManyInterval(ctx context.Context, symbols []string, from, to Day, iv Interval) (map[string]*Series, map[string]error) {
	out := make(map[string]*Series, len(symbols))
	errs := make(map[string]error)
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Bound concurrency; public data endpoints throttle aggressively.
	sem := make(chan struct{}, 8)
	for _, sym := range symbols {
		sym = NormalizeSymbol(sym)
		if sym == "" {
			continue
		}
		wg.Add(1)
		go func(sym string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ser, err := s.GetInterval(ctx, sym, from, to, iv)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[sym] = err
				return
			}
			out[sym] = ser
		}(sym)
	}
	wg.Wait()
	return out, errs
}

// Search proxies to the provider.
func (s *Store) Search(ctx context.Context, q string) ([]Quote, error) {
	return s.provider.Search(ctx, q)
}

// covers reports whether the cached series already answers a request for
// [from, to]. haveFrom is the earliest date previously requested, which is what
// makes a symbol that simply has no history that far back a cache hit rather
// than a permanent miss.
func covers(s *Series, haveFrom, from, to Day) bool {
	if s == nil || len(s.Bars) == 0 {
		return false
	}
	first, _ := s.First()
	last, _ := s.Last()
	if last.Date < to {
		return false
	}
	if first.Date <= from {
		return true
	}
	// No bars back to `from`, but we already asked for at least that far and
	// this is everything the provider has.
	return haveFrom != "" && haveFrom <= from
}

// merge combines two series, preferring bars from the newer fetch.
func merge(old, fresh *Series) *Series {
	if old == nil || len(old.Bars) == 0 {
		return fresh
	}
	if fresh == nil || len(fresh.Bars) == 0 {
		return old
	}
	byDate := make(map[Day]Bar, len(old.Bars)+len(fresh.Bars))
	for _, b := range old.Bars {
		byDate[b.Date] = b
	}
	for _, b := range fresh.Bars {
		byDate[b.Date] = b
	}
	bars := make([]Bar, 0, len(byDate))
	for _, b := range byDate {
		bars = append(bars, b)
	}
	name := fresh.Name
	if name == "" {
		name = old.Name
	}
	s := NewSeries(fresh.Symbol, bars)
	s.Name = name
	return s
}

// TradingCalendar returns the sorted union of dates across the given series,
// restricted to [from, to]. The union (rather than the intersection) is used
// so that a symbol which is halted or newly listed does not silently truncate
// the whole backtest.
func TradingCalendar(series map[string]*Series, from, to Day) []Day {
	seen := map[Day]bool{}
	for _, s := range series {
		for _, b := range s.Bars {
			if b.Date >= from && b.Date <= to {
				seen[b.Date] = true
			}
		}
	}
	days := make([]Day, 0, len(seen))
	for d := range seen {
		days = append(days, d)
	}
	sortDays(days)
	return days
}

func sortDays(d []Day) {
	// Days are ISO strings, so lexicographic order is chronological order.
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}
