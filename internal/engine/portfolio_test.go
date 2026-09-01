package engine

import (
	"sort"
	"testing"
)

// One bug has now appeared four times in this engine: iterating a Go map
// whose order reaches the output. The order queue, the portfolio sums, the
// stop evaluation and the borrow accrual were each found separately, by
// something else breaking.
//
// Floating point addition is not associative, so a sum over a map is only
// stable if the map's order is. The divergence is largest when the terms
// differ wildly in magnitude, which is exactly a real book: a large index
// position beside a handful of small ones.
//
// This guards the primitive rather than a whole backtest, because a run is
// far too coarse to catch it — the first version of this test drove an eight
// run backtest and passed happily with the bug put back.
func TestBookSumsDoNotDependOnMapOrder(t *testing.T) {
	p := NewPortfolio(0, Costs{})
	prices := map[string]float64{}

	// Magnitudes chosen to span the range where association bites: adding a
	// cent to a billion loses the cent, and whether it is lost depends on
	// when it arrives.
	sizes := []float64{1e9, 1e-2, 5e8, 3e-3, 7e7, 1e-4, 2e6, 9e-5, 4e5, 6e-6,
		1.7e9, 2.3e-2, 8.9e7, 3.1e-3, 5.5e6, 1.1e-5}
	for i, v := range sizes {
		sym := string(rune('A'+i%26)) + string(rune('A'+i/26)) + "X"
		p.Positions[sym] = &Position{Shares: v}
		prices[sym] = 1.0000000001
		p.symDirty = true
	}

	// Every call must agree with every other, across enough calls that Go's
	// map randomisation would have shown itself.
	wantMV := p.MarketValue(prices)
	wantGE := p.GrossExposure(prices)
	for i := 0; i < 200; i++ {
		if got := p.MarketValue(prices); got != wantMV {
			t.Fatalf("MarketValue call %d = %v, first call = %v (delta %v): "+
				"the sum depends on map order", i, got, wantMV, got-wantMV)
		}
		if got := p.GrossExposure(prices); got != wantGE {
			t.Fatalf("GrossExposure call %d = %v, first call = %v (delta %v): "+
				"the sum depends on map order", i, got, wantGE, got-wantGE)
		}
	}

	// And the order it sums in must be the sorted one, which is the property
	// that makes the answer stable across processes rather than merely
	// within one.
	syms := p.sortedSymbols()
	if !sort.StringsAreSorted(syms) {
		t.Errorf("sortedSymbols returned %v, which is not sorted", syms)
	}
	if len(syms) != len(sizes) {
		t.Errorf("sortedSymbols returned %d symbols, want %d", len(syms), len(sizes))
	}
}
