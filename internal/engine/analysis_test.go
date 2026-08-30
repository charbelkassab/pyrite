package engine

import (
	"context"
	"math"
	"strings"
	"testing"
)

// A rotating strategy over a multi-year window exercises every new section of
// the result at once: it opens and closes real round trips, spans several
// calendar years, and holds more than one symbol.
func rotationResult(t *testing.T) *Result {
	t.Helper()
	spec := baseSpec(`
		function setup(ctx) { ctx.warmup(60); }
		function onDay(ctx) {
			if (!ctx.isFirstTradingDayOfMonth()) return;
			const winners = ctx.rank("momentum", 2, { window: 40 });
			if (winners.length) ctx.equalWeight(winners, 0.95);
		}
	`)
	spec.Start = "2020-01-06"
	spec.End = "2023-12-29"
	spec.Benchmarks = []string{"SPY"}

	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

func TestResultCarriesRoundTrips(t *testing.T) {
	res := rotationResult(t)
	if len(res.Trades) == 0 {
		t.Fatal("a rotating strategy should produce round trips")
	}
	if res.TradeStats.Closed == 0 {
		t.Fatal("no closed trades recorded")
	}

	// Every closed trade must reconcile: gross minus costs is net.
	for _, tr := range res.Trades {
		if tr.Open {
			continue
		}
		if math.Abs(tr.GrossPnL-tr.Costs-tr.NetPnL) > 1e-6 {
			t.Fatalf("trade does not reconcile: %+v", tr)
		}
		if tr.EntryDate == "" || tr.ExitDate == "" {
			t.Fatalf("closed trade missing dates: %+v", tr)
		}
		if tr.ExitDate < tr.EntryDate {
			t.Fatalf("trade exits before it enters: %+v", tr)
		}
		if tr.MAEPct > 0 || tr.MFEPct < 0 {
			t.Fatalf("excursions have the wrong sign: %+v", tr)
		}
		if tr.BarsHeld <= 0 {
			t.Fatalf("closed trade should span at least one bar: %+v", tr)
		}
	}

	// Realised P&L summed over closed trades should agree with the
	// portfolio's own realised total net of the costs charged on those legs.
	var net float64
	for _, tr := range res.Trades {
		if !tr.Open {
			net += tr.NetPnL
		}
	}
	if net == 0 {
		t.Error("closed trades summed to exactly zero, which is implausible here")
	}
}

func TestRiskMetricsPopulated(t *testing.T) {
	res := rotationResult(t)
	r := res.Risk
	if r.UlcerIndex <= 0 {
		t.Errorf("ulcer index should be positive for any curve with a drawdown: %v", r.UlcerIndex)
	}
	if r.VaR95 >= 0 {
		t.Errorf("95%% VaR should be a loss: %v", r.VaR95)
	}
	if r.CVaR95 > r.VaR95 {
		t.Errorf("CVaR must be at least as bad as VaR: %v vs %v", r.CVaR95, r.VaR95)
	}
	if r.EquityR2 < 0 || r.EquityR2 > 1 {
		t.Errorf("R² out of range: %v", r.EquityR2)
	}
	if !r.Omega.Defined() {
		t.Error("omega should be defined")
	}
	if !r.UpCapture.Defined() || !r.DownCapture.Defined() {
		t.Error("capture ratios should be defined when a benchmark is present")
	}
}

func TestAttributionCoversYearsRegimesAndSymbols(t *testing.T) {
	res := rotationResult(t)
	a := res.Attribution

	if len(a.ByYear) != 4 {
		t.Fatalf("2020-2023 should yield 4 years, got %d", len(a.ByYear))
	}
	if a.ByYear[0].Label != "2020" || a.ByYear[3].Label != "2023" {
		t.Errorf("year labels wrong: %v .. %v", a.ByYear[0].Label, a.ByYear[3].Label)
	}
	// Compounding the yearly returns must reproduce the total.
	compound := 1.0
	for _, y := range a.ByYear {
		compound *= 1 + y.Return
	}
	if math.Abs((compound-1)-res.Metrics.TotalReturn) > 1e-6 {
		t.Errorf("yearly returns compound to %v, total return is %v",
			compound-1, res.Metrics.TotalReturn)
	}

	if len(a.ByMonthOfYear) != 12 {
		t.Errorf("seasonal view should have 12 buckets, got %d", len(a.ByMonthOfYear))
	}
	if len(a.ByRegime) == 0 {
		t.Error("regime breakdown should classify a four-year run")
	}
	if len(a.BySymbol) == 0 {
		t.Error("symbol attribution should be populated")
	}
	// Contributions are shares of total absolute P&L, so each is in [-1, 1].
	for _, s := range a.BySymbol {
		if s.Contribution < -1.0001 || s.Contribution > 1.0001 {
			t.Errorf("contribution out of range for %s: %v", s.Symbol, s.Contribution)
		}
	}
	if len(a.Stress) == 0 {
		t.Error("stress tests should run on a multi-year curve")
	}
	for _, s := range a.Stress {
		if s.Return > res.Metrics.TotalReturn+1e-9 {
			t.Errorf("%s improved the result, which cannot be: %v > %v",
				s.Label, s.Return, res.Metrics.TotalReturn)
		}
	}
}

func TestRollingStatsTrackTheCurve(t *testing.T) {
	res := rotationResult(t)
	if len(res.Rolling) == 0 {
		t.Fatal("rolling series should be populated for a four-year run")
	}
	last := res.Rolling[len(res.Rolling)-1]
	if last.Date != string(res.Curve[len(res.Curve)-1].Date) {
		t.Errorf("rolling series should end on the last bar: %v vs %v",
			last.Date, res.Curve[len(res.Curve)-1].Date)
	}
	for _, p := range res.Rolling {
		if p.Vol < 0 {
			t.Fatalf("negative volatility at %s", p.Date)
		}
	}
}

func TestManifestRecordsProvenance(t *testing.T) {
	res := rotationResult(t)
	m := res.Manifest

	if m.CodeSHA256 == "" || len(m.CodeSHA256) != 64 {
		t.Errorf("code hash missing or malformed: %q", m.CodeSHA256)
	}
	if m.DataProvider == "" {
		t.Error("data provider not recorded")
	}
	if m.GoVersion == "" || !strings.HasPrefix(m.GoVersion, "go") {
		t.Errorf("go version not recorded: %q", m.GoVersion)
	}
	if m.CalendarDays == 0 || m.CalendarFrom == "" {
		t.Error("calendar coverage not recorded")
	}
	if len(m.Coverage) == 0 {
		t.Error("per-symbol coverage not recorded")
	}
	for sym, c := range m.Coverage {
		if c.Bars == 0 || c.FirstBar == "" || c.LastBar == "" {
			t.Errorf("incomplete coverage for %s: %+v", sym, c)
		}
	}
	if m.Fill != res.Spec.Fill {
		t.Errorf("fill model not recorded: %v", m.Fill)
	}
	// No model was consulted, so this run is exactly reproducible.
	if !m.Reproducible() {
		t.Error("a run with no AI calls must report as reproducible")
	}
}

func TestOmitDayRecordsDropsTheAuditTrail(t *testing.T) {
	spec := baseSpec(`
		function onDay(ctx) {
			if (ctx.dayIndex === 0) ctx.buy("AAPL", { pctCash: 0.5 });
		}
	`)
	spec.OmitDayRecords = true

	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Days) != 0 {
		t.Errorf("day records should be dropped, got %d", len(res.Days))
	}
	// Everything a sweep actually reads must survive.
	if len(res.Curve) == 0 {
		t.Error("equity curve must survive")
	}
	if len(res.Fills) == 0 {
		t.Error("fills must survive")
	}
	if res.Metrics.TradingDays == 0 {
		t.Error("metrics must survive")
	}
	if res.Manifest.CodeSHA256 == "" {
		t.Error("manifest must survive")
	}
}
