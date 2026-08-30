package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// encodable walks a value and reports the first path holding a NaN or an
// infinity that encoding/json would refuse.
//
// This exists because the failure mode is silent and total: net/http has
// already written a 200 by the time the encoder hits the bad float, so the
// client gets an empty body with a success status and no error anywhere.
// One undefined ratio in one cell destroys the whole response.
func mustEncode(t *testing.T, name string, v any) {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("%s does not survive JSON encoding: %v", name, err)
	}
	if strings.Contains(buf.String(), "NaN") || strings.Contains(buf.String(), "Inf") {
		t.Fatalf("%s encoded a non-finite literal, which no JSON parser accepts", name)
	}
}

func TestResultAlwaysEncodes(t *testing.T) {
	// A strategy that never trades leaves most ratios undefined, which is
	// the case that used to truncate the response.
	spec := baseSpec(`function onDay(ctx) {}`)
	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	mustEncode(t, "Result", res)
}

func TestSweepResultAlwaysEncodes(t *testing.T) {
	res, err := RunSweep(context.Background(), SweepSpec{Base: sweepSpec()}, newTestStore(t), nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	mustEncode(t, "SweepResult", res)

	// Now force the undefined cases directly: a failed combination and an
	// undefined objective both have to marshal to null.
	res.Rows = append(res.Rows, SweepRow{
		Params: map[string]any{"fast": 1}, Label: "fast=1",
		Score: Ratio(math.NaN()), Error: "deliberate failure",
	})
	res.Rows = append(res.Rows, SweepRow{
		Params: map[string]any{"fast": 2}, Label: "fast=2",
		Score: Ratio(math.Inf(1)),
	})
	res.Robustness.PBO = Ratio(math.NaN())
	res.Robustness.DeflatedSharpe = Ratio(math.Inf(-1))
	mustEncode(t, "SweepResult with undefined cells", res)

	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(res)
	if !strings.Contains(buf.String(), `"score":null`) {
		t.Error("an undefined score should marshal to null")
	}
}

func TestWalkForwardResultAlwaysEncodes(t *testing.T) {
	spec := sweepSpec()
	spec.Start = "2018-01-03"
	spec.End = "2023-12-29"
	res, err := RunWalkForward(context.Background(), WalkForwardSpec{
		Base: spec, TrainDays: 300, TestDays: 150, Workers: 2,
	}, newTestStore(t), nil)
	if err != nil {
		t.Fatalf("walk-forward: %v", err)
	}
	mustEncode(t, "WalkForwardResult", res)

	res.Folds = append(res.Folds, Fold{
		Index: 99, TrainScore: Ratio(math.NaN()), TestScore: Ratio(math.Inf(1)),
		Error: "deliberate failure",
	})
	res.Efficiency = Ratio(math.NaN())
	mustEncode(t, "WalkForwardResult with a failed fold", res)
}

func TestSortedPutsFailuresLast(t *testing.T) {
	res := &SweepResult{Rows: []SweepRow{
		{Label: "bad", Score: Ratio(math.NaN()), Error: "boom"},
		{Label: "low", Score: 0.1},
		{Label: "high", Score: 0.9},
	}}
	got := res.Sorted()
	if got[0].Label != "high" || got[1].Label != "low" || got[2].Label != "bad" {
		t.Errorf("ranking wrong: %v %v %v", got[0].Label, got[1].Label, got[2].Label)
	}
}
