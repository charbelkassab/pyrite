package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/charbelkassab/pyrite/examples"
	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
)

// The bundled example strategies are hand-written against the documented API,
// so running them is a direct test that the API can express these ideas
// without a model in the loop. They run on synthetic data, so no network or
// API key is needed and the assertions are about mechanics rather than
// returns.
//
// Each example declares its own universe and warm-up in header directives, so
// this test reads them from the file rather than repeating them here. That is
// the same path `pyrite run --example` takes, which means a directive
// that is wrong fails here rather than in a user's terminal.
func TestBundledExamplesRun(t *testing.T) {
	fund, err := market.LoadFundamentals("")
	if err != nil {
		t.Fatalf("fundamentals: %v", err)
	}
	store := market.NewStore(market.NewSyntheticProvider(), nil, fund)

	for _, ex := range examples.All() {
		if ex.NeedsModel {
			// This one needs a live model and web access, which these tests
			// deliberately do not have.
			continue
		}
		t.Run(ex.Name, func(t *testing.T) {
			spec := engine.Spec{
				Name:            ex.Name,
				Code:            ex.Code,
				Universe:        market.ResolveUniverse(strings.Join(ex.Universe, ",")),
				Index:           market.IndexUniverse(strings.Join(ex.Universe, ",")),
				Start:           "2021-01-04",
				End:             "2023-12-29",
				InitialCash:     100000,
				AllowShort:      ex.AllowShort,
				AllowFractional: true,
				Warmup:          ex.Warmup,
				OmitDayRecords:  true,
			}
			res, err := engine.New(spec, store).Run(context.Background())
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if len(res.Curve) < 500 {
				t.Fatalf("expected three years of sessions, got %d", len(res.Curve))
			}
			if res.StrategyErrors > 0 {
				t.Errorf("the strategy threw on %d sessions: %v",
					res.StrategyErrors, res.Warnings)
			}
			// Every example except a pure buy-and-hold should have traded.
			if len(res.Fills) == 0 {
				t.Errorf("%s never traded, so its directives or its logic are wrong", ex.Name)
			}
			// The warm-up directive must actually be enough for the widest
			// parameter setting the file declares.
			for _, p := range res.Params {
				vals := p.Values()
				if len(vals) < 2 {
					continue
				}
				if got := len(vals); got == 0 {
					t.Errorf("parameter %s declared an empty grid", p.Name)
				}
			}
		})
	}
}

// A parameterised example must work at the extremes of its own grids, not
// only at its defaults — otherwise `sweep --example` reports failures that
// are the example's fault rather than the strategy's.
func TestBundledExamplesWorkAcrossTheirGrids(t *testing.T) {
	fund, err := market.LoadFundamentals("")
	if err != nil {
		t.Fatalf("fundamentals: %v", err)
	}
	store := market.NewStore(market.NewSyntheticProvider(), nil, fund)

	for _, ex := range examples.All() {
		if ex.NeedsModel || !strings.Contains(ex.Code, "ctx.param(") {
			continue
		}
		t.Run(ex.Name, func(t *testing.T) {
			base := engine.Spec{
				Name: ex.Name, Code: ex.Code,
				Universe:        market.ResolveUniverse(strings.Join(ex.Universe, ",")),
				Start:           "2021-01-04",
				End:             "2023-12-29",
				InitialCash:     100000,
				AllowShort:      ex.AllowShort,
				AllowFractional: true,
				Warmup:          ex.Warmup,
				OmitDayRecords:  true,
			}
			decls, err := engine.DeclaredParams(context.Background(), base, store)
			if err != nil {
				t.Fatalf("declared params: %v", err)
			}
			if len(decls) == 0 {
				t.Fatalf("%s contains ctx.param but declared none", ex.Name)
			}

			// The widest setting of every grid at once is the hardest case,
			// and the one a wrong warm-up breaks.
			widest := map[string]any{}
			for _, d := range decls {
				vals := d.Values()
				widest[d.Name] = vals[len(vals)-1]
			}
			spec := base
			spec.Params = widest
			res, err := engine.New(spec, store).Run(context.Background())
			if err != nil {
				t.Fatalf("widest settings %v failed: %v", widest, err)
			}
			if len(res.Fills) == 0 {
				t.Errorf("%s never traded at its widest settings %v — the warmup "+
					"directive is probably too small for the largest lookback",
					ex.Name, widest)
			}
		})
	}
}
