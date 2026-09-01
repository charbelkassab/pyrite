package bundle

import (
	"context"
	"fmt"

	"github.com/charbelkassab/pyrite/internal/market"
)

// provider serves the bars a bundle carries, and nothing else.
//
// It reaches no network and reads no cache. That is the whole point: a re-run
// that could fall back to the vendor would be measuring today's adjusted
// closes and reporting them as the bundle's, which is the failure the bundle
// exists to remove. A symbol the bundle does not hold is not fetched, it is
// missing, and the run says so.
type provider struct {
	series   map[string]*market.Series
	interval market.Interval
}

func (p *provider) Name() string { return "bundle" }

func (p *provider) Fetch(_ context.Context, symbol string, _, _ market.Day) (*market.Series, error) {
	if s, ok := p.series[market.NormalizeSymbol(symbol)]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("%w: the bundle carries no bars for %s", market.ErrNotFound, symbol)
}

func (p *provider) Search(context.Context, string) ([]market.Quote, error) { return nil, nil }

// SupportedIntervals reports the one bar size the bundle was recorded at.
//
// Serving a different size would mean resampling bars into something the
// original run never saw, and calling the result a reproduction.
func (p *provider) SupportedIntervals() []market.Interval {
	return []market.Interval{p.interval}
}

func (p *provider) FetchInterval(ctx context.Context, symbol string, from, to market.Day, iv market.Interval) (*market.Series, error) {
	if iv != "" && iv != p.interval {
		return nil, fmt.Errorf("this bundle holds %s bars, so %s cannot be served from it", p.interval, iv)
	}
	return p.Fetch(ctx, symbol, from, to)
}

// Store builds a data store that serves only this bundle.
//
// The disk cache is deliberately nil. With one, a re-run on a machine that has
// already fetched these symbols could read the cached copy instead, and the
// bundle would be proving nothing.
func (b *Bundle) Store() *market.Store {
	p := &provider{series: b.Series, interval: b.Spec.Interval}
	s := market.NewStore(p, nil, b.Fundamentals)
	if b.Membership != nil {
		s.SetMembership(b.Membership)
	}
	return s
}
