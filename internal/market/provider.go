package market

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrNotFound indicates a symbol has no data at the provider.
var ErrNotFound = errors.New("symbol not found")

// Provider fetches historical daily bars for a symbol.
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

// Store is a Provider wrapped with an in-memory and on-disk cache. It is the
// type the rest of the application depends on.
type Store struct {
	provider Provider
	cache    *DiskCache
	fund     *Fundamentals

	mu   sync.RWMutex
	mem  map[string]*Series
	cold map[string]bool // symbols known to have no data, to avoid refetch
	// span records the earliest date already requested per symbol, so a
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

// ProviderName reports the underlying provider.
func (s *Store) ProviderName() string { return s.provider.Name() }

// Fundamentals exposes the share-count table used for market-cap ranking.
func (s *Store) Fundamentals() *Fundamentals { return s.fund }

// Get returns the full cached series for a symbol, fetching if needed.
//
// The store always fetches and caches the widest range it has seen for a
// symbol, so repeated backtests over overlapping windows hit the cache.
func (s *Store) Get(ctx context.Context, symbol string, from, to Day) (*Series, error) {
	symbol = NormalizeSymbol(symbol)

	s.mu.RLock()
	if s.cold[symbol] {
		s.mu.RUnlock()
		return nil, fmt.Errorf("%w: %s", ErrNotFound, symbol)
	}
	ser, ok := s.mem[symbol]
	haveFrom := s.span[symbol]
	s.mu.RUnlock()
	if ok && covers(ser, haveFrom, from, to) {
		return ser, nil
	}

	// Try the on-disk cache before hitting the network.
	if s.cache != nil && ser == nil {
		if cached, cachedFrom, err := s.cache.Load(symbol); err == nil && cached != nil {
			ser = cached
			haveFrom = cachedFrom
			s.mu.Lock()
			s.mem[symbol] = cached
			s.span[symbol] = cachedFrom
			s.mu.Unlock()
			if covers(cached, cachedFrom, from, to) {
				return cached, nil
			}
		}
	}

	fetched, err := s.provider.Fetch(ctx, symbol, from, to)
	if err != nil {
		// A stale-but-usable cache beats a hard failure when offline.
		if ser != nil {
			return ser, nil
		}
		if errors.Is(err, ErrNotFound) {
			s.mu.Lock()
			s.cold[symbol] = true
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
	s.mem[symbol] = merged
	s.span[symbol] = widest
	s.mu.Unlock()
	if s.cache != nil {
		_ = s.cache.Save(merged, widest)
	}
	return merged, nil
}

// GetMany fetches several symbols concurrently, returning the successes and a
// map of per-symbol errors. A single bad ticker must not fail a whole run.
func (s *Store) GetMany(ctx context.Context, symbols []string, from, to Day) (map[string]*Series, map[string]error) {
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
			ser, err := s.Get(ctx, sym, from, to)
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
