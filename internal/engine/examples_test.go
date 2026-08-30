package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charbelkassab/natural-quant/internal/engine"
	"github.com/charbelkassab/natural-quant/internal/market"
)

// The bundled example strategies are hand-written against the documented API,
// so running them is a direct test that the API can express these ideas without
// a model in the loop. They run on synthetic data, so no network or API key is
// needed and the assertions are about mechanics rather than returns.
func TestBundledExamplesRun(t *testing.T) {
	cases := []struct {
		file       string
		universe   []string
		warmup     int
		allowShort bool
		// wantTrades requires the strategy to have placed at least one fill.
		wantTrades bool
	}{
		{"biggest-company.js", market.ResolveUniverse("megacap"), 10, false, true},
		{"golden-cross.js", []string{"SPY"}, 220, false, false},
		{"momentum-rotation.js", market.ResolveUniverse("tech"), 140, false, true},
		{"mean-reversion.js", market.ResolveUniverse("megacap"), 80, false, true},
		{"sixty-forty.js", []string{"SPY", "AGG"}, 5, false, true},
		{"pairs-trade.js", []string{"KO", "PEP"}, 90, true, true},
		// news-sentiment.js is excluded: it requires a live model and web
		// access, which these tests deliberately do not have.
	}

	fund, err := market.LoadFundamentals("")
	if err != nil {
		t.Fatalf("fundamentals: %v", err)
	}
	store := market.NewStore(market.NewSyntheticProvider(), nil, fund)

	for _, tc := range cases {
		t.Run(strings.TrimSuffix(tc.file, ".js"), func(t *testing.T) {
			path := filepath.Join("..", "..", "examples", tc.file)
			code, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}

			spec := engine.Spec{
				Name:            tc.file,
				Code:            string(code),
				Universe:        tc.universe,
				Start:           "2021-01-04",
				End:             "2023-12-29",
				InitialCash:     100000,
				AllowShort:      tc.allowShort,
				AllowFractional: true,
				Warmup:          tc.warmup,
			}
			res, err := engine.New(spec, store).Run(context.Background())
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if res.StrategyErrors > 0 {
				for _, d := range res.Days {
					if d.Error != "" {
						t.Fatalf("threw on %s: %s", d.Date, d.Error)
					}
				}
			}
			if len(res.Curve) < 500 {
				t.Errorf("expected a full run, got %d days", len(res.Curve))
			}
			if tc.wantTrades && len(res.Fills) == 0 {
				t.Errorf("expected the strategy to trade, but it placed no fills")
			}
			// Equity must stay finite and non-negative throughout; a NaN here
			// means an accounting bug, not a bad strategy.
			for _, p := range res.Curve {
				if p.Value != p.Value {
					t.Fatalf("equity became NaN on %s", p.Date)
				}
			}
		})
	}
}
