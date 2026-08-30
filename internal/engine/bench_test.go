package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/charbelkassab/natural-quant/internal/market"
)

// The roadmap's instruction for the vectorised fast path was to budget the
// work before building it: instrument one run, multiply, and decide from the
// measured number rather than from an assumption about interpreters being
// slow. These benchmarks are that measurement.

func BenchmarkSingleBacktest(b *testing.B) {
	store := newBenchStore(b)
	spec := benchSpec()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := New(spec, store).Run(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSweep measures the throughput that actually matters: many runs
// sharing one copy of the price data across a worker pool.
func BenchmarkSweep(b *testing.B) {
	store := newBenchStore(b)
	base := benchSpec()

	for _, combos := range []int{16, 64} {
		b.Run(fmt.Sprintf("combos=%d", combos), func(b *testing.B) {
			grid := make([]any, combos)
			for i := range grid {
				grid[i] = 10 + i
			}
			for i := 0; i < b.N; i++ {
				res, err := RunSweep(context.Background(), SweepSpec{
					Base:      base,
					Grids:     map[string][]any{"fast": grid},
					PBOBlocks: -1,
				}, store, nil)
				if err != nil {
					b.Fatal(err)
				}
				if res.Combos != combos {
					b.Fatalf("expected %d combinations, got %d", combos, res.Combos)
				}
			}
		})
	}
}

func newBenchStore(b *testing.B) *market.Store {
	b.Helper()
	fund, err := market.LoadFundamentals("")
	if err != nil {
		b.Fatalf("load fundamentals: %v", err)
	}
	return market.NewStore(market.NewSyntheticProvider(), nil, fund)
}

func benchSpec() Spec {
	spec := Spec{
		Name: "bench",
		Code: `
			function setup(ctx) {
				ctx.universe(["AAPL"]);
				ctx.param("fast", 20, { grid: [10, 20, 50] });
				ctx.warmup(220);
			}
			function onDay(ctx) {
				const f = ctx.sma("AAPL", ctx.params.fast);
				const s = ctx.sma("AAPL", 200);
				if (f === null || s === null) return;
				if (f > s && !ctx.hasPosition("AAPL")) ctx.buy("AAPL", { pctCash: 0.95 });
				else if (f < s && ctx.hasPosition("AAPL")) ctx.close("AAPL");
			}
		`,
		Universe:        []string{"AAPL"},
		Start:           "2019-01-02",
		End:             "2023-12-29",
		InitialCash:     100000,
		AllowFractional: true,
		Warmup:          220,
		OmitDayRecords:  true,
	}
	spec.ApplyDefaults()
	return spec
}
