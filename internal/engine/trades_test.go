package engine

import (
	"math"
	"testing"

	"github.com/charbelkassab/pyrite/internal/market"
)

func fill(day, sym string, side Side, shares, price float64) Fill {
	return Fill{Date: market.Day(day), Symbol: sym, Side: side, Shares: shares, Price: price,
		Value: shares * price}
}

func TestBuildTradesPairsFIFO(t *testing.T) {
	// Two entries at different prices, then one exit that closes both. FIFO
	// means the first trade must use the 100 entry, not an average.
	fills := []Fill{
		fill("2024-01-02", "AAPL", SideBuy, 10, 100),
		fill("2024-01-03", "AAPL", SideBuy, 10, 120),
		fill("2024-01-10", "AAPL", SideSell, 20, 130),
	}
	trades := BuildTrades(fills, nil)
	if len(trades) != 2 {
		t.Fatalf("want 2 round trips, got %d", len(trades))
	}
	if trades[0].EntryPrice != 100 || trades[0].ExitPrice != 130 {
		t.Errorf("first trade should pair the oldest lot: %+v", trades[0])
	}
	if trades[1].EntryPrice != 120 {
		t.Errorf("second trade should use the newer lot: %+v", trades[1])
	}
	if got := trades[0].NetPnL; math.Abs(got-300) > 1e-9 {
		t.Errorf("net pnl: got %v, want 300", got)
	}
	if trades[0].DaysHeld != 8 {
		t.Errorf("days held: got %d, want 8", trades[0].DaysHeld)
	}
}

func TestBuildTradesPartialExit(t *testing.T) {
	fills := []Fill{
		fill("2024-01-02", "MSFT", SideBuy, 10, 100),
		fill("2024-01-05", "MSFT", SideSell, 4, 110),
	}
	trades := BuildTrades(fills, nil)
	if len(trades) != 2 {
		t.Fatalf("want one closed and one open trade, got %d", len(trades))
	}
	closed, open := trades[0], trades[1]
	if closed.Open || !open.Open {
		closed, open = trades[1], trades[0]
	}
	if closed.Shares != 4 {
		t.Errorf("closed quantity: got %v, want 4", closed.Shares)
	}
	if open.Shares != 6 {
		t.Errorf("remaining quantity should stay open: got %v, want 6", open.Shares)
	}
	st := ComputeTradeStats(trades)
	if st.Closed != 1 || st.Open != 1 {
		t.Errorf("open trades must not count as closed: %+v", st)
	}
}

func TestBuildTradesShort(t *testing.T) {
	fills := []Fill{
		fill("2024-02-01", "TSLA", SideShort, 5, 200),
		fill("2024-02-08", "TSLA", SideCover, 5, 180),
	}
	trades := BuildTrades(fills, nil)
	if len(trades) != 1 {
		t.Fatalf("want 1 trade, got %d", len(trades))
	}
	tr := trades[0]
	if tr.Direction != DirShort {
		t.Errorf("direction: got %v, want short", tr.Direction)
	}
	// A short that fell 200 -> 180 made money.
	if tr.NetPnL <= 0 {
		t.Errorf("profitable short reported a loss: %v", tr.NetPnL)
	}
	if math.Abs(tr.NetPnL-100) > 1e-9 {
		t.Errorf("net pnl: got %v, want 100", tr.NetPnL)
	}
}

func TestBuildTradesFlipThroughZero(t *testing.T) {
	// Long 10, then sell 15: closes the long and opens a 5-share short.
	fills := []Fill{
		fill("2024-03-01", "NVDA", SideBuy, 10, 50),
		fill("2024-03-04", "NVDA", SideSell, 15, 60),
	}
	trades := BuildTrades(fills, nil)
	if len(trades) != 2 {
		t.Fatalf("want a closed long and an open short, got %d", len(trades))
	}
	var closed, open *Trade
	for i := range trades {
		if trades[i].Open {
			open = &trades[i]
		} else {
			closed = &trades[i]
		}
	}
	if closed == nil || open == nil {
		t.Fatalf("expected one closed and one open trade: %+v", trades)
	}
	if closed.Direction != DirLong || closed.Shares != 10 {
		t.Errorf("closed leg wrong: %+v", closed)
	}
	if open.Direction != DirShort || open.Shares != 5 {
		t.Errorf("flipped leg should be a 5-share short: %+v", open)
	}
}

func TestTradeCostsReduceNetPnL(t *testing.T) {
	entry := fill("2024-01-02", "AAPL", SideBuy, 10, 100)
	entry.Commission, entry.Slippage = 1, 2
	exit := fill("2024-01-09", "AAPL", SideSell, 10, 110)
	exit.Commission, exit.Slippage = 1, 2

	trades := BuildTrades([]Fill{entry, exit}, nil)
	if len(trades) != 1 {
		t.Fatalf("want 1 trade, got %d", len(trades))
	}
	tr := trades[0]
	if math.Abs(tr.GrossPnL-100) > 1e-9 {
		t.Errorf("gross: got %v, want 100", tr.GrossPnL)
	}
	if math.Abs(tr.Costs-6) > 1e-9 {
		t.Errorf("both legs' costs should be charged: got %v, want 6", tr.Costs)
	}
	if math.Abs(tr.NetPnL-94) > 1e-9 {
		t.Errorf("net: got %v, want 94", tr.NetPnL)
	}
}

func TestExcursionUsesIntrabarExtremes(t *testing.T) {
	bars := []market.Bar{
		{Date: "2024-01-02", Open: 100, High: 101, Low: 99, Close: 100, AdjClose: 100},
		// Dipped to 90 intraday but closed at 100: MAE must see the 90.
		{Date: "2024-01-03", Open: 100, High: 105, Low: 90, Close: 100, AdjClose: 100},
		{Date: "2024-01-04", Open: 100, High: 120, Low: 100, Close: 110, AdjClose: 110},
	}
	series := map[string]*market.Series{"AAPL": market.NewSeries("AAPL", bars)}
	fills := []Fill{
		fill("2024-01-02", "AAPL", SideBuy, 10, 100),
		fill("2024-01-04", "AAPL", SideSell, 10, 110),
	}
	trades := BuildTrades(fills, series)
	if len(trades) != 1 {
		t.Fatalf("want 1 trade, got %d", len(trades))
	}
	tr := trades[0]
	if math.Abs(tr.MAEPct-(-0.10)) > 1e-9 {
		t.Errorf("MAE should use the intrabar low: got %v, want -0.10", tr.MAEPct)
	}
	if math.Abs(tr.MFEPct-0.20) > 1e-9 {
		t.Errorf("MFE should use the intrabar high: got %v, want 0.20", tr.MFEPct)
	}
	if tr.BarsHeld != 3 {
		t.Errorf("bars held: got %d, want 3", tr.BarsHeld)
	}
}

func TestExcursionForShortMirrors(t *testing.T) {
	bars := []market.Bar{
		{Date: "2024-01-02", Open: 100, High: 110, Low: 100, Close: 100, AdjClose: 100},
		{Date: "2024-01-03", Open: 100, High: 100, Low: 80, Close: 85, AdjClose: 85},
	}
	series := map[string]*market.Series{"X": market.NewSeries("X", bars)}
	fills := []Fill{
		fill("2024-01-02", "X", SideShort, 1, 100),
		fill("2024-01-03", "X", SideCover, 1, 85),
	}
	tr := BuildTrades(fills, series)[0]
	// Price rose to 110 against the short (-10%) and fell to 80 for it (+20%).
	if math.Abs(tr.MAEPct-(-0.10)) > 1e-9 {
		t.Errorf("short MAE: got %v, want -0.10", tr.MAEPct)
	}
	if math.Abs(tr.MFEPct-0.20) > 1e-9 {
		t.Errorf("short MFE: got %v, want 0.20", tr.MFEPct)
	}
}

func TestTradeStatsExpectancyAndStreaks(t *testing.T) {
	trades := []Trade{
		{Symbol: "A", NetPnL: 100, ReturnPct: 0.10, BarsHeld: 5, MFEPct: 0.12, MAEPct: -0.02},
		{Symbol: "A", NetPnL: 50, ReturnPct: 0.05, BarsHeld: 3, MFEPct: 0.07, MAEPct: -0.01},
		{Symbol: "B", NetPnL: -30, ReturnPct: -0.03, BarsHeld: 7, MFEPct: 0.06, MAEPct: -0.05},
	}
	s := ComputeTradeStats(trades)
	if s.Closed != 3 || s.Wins != 2 || s.Losses != 1 {
		t.Fatalf("counts wrong: %+v", s)
	}
	if math.Abs(s.WinRate-2.0/3.0) > 1e-9 {
		t.Errorf("win rate: got %v", s.WinRate)
	}
	if math.Abs(s.Expectancy-40) > 1e-9 {
		t.Errorf("expectancy: got %v, want 40", s.Expectancy)
	}
	if s.MaxConsecWins != 2 || s.MaxConsecLosses != 1 {
		t.Errorf("streaks: %d/%d", s.MaxConsecWins, s.MaxConsecLosses)
	}
	if !s.ProfitFactor.Defined() || math.Abs(float64(s.ProfitFactor)-5) > 1e-9 {
		t.Errorf("profit factor: got %v, want 5", s.ProfitFactor)
	}
	// The loser peaked at +6% and finished at -3%: it gave back all of it and
	// then some, so give-back exceeds 1.
	if s.GiveBack <= 1 {
		t.Errorf("give-back should exceed 1 for a loser that was up: got %v", s.GiveBack)
	}
	if !s.EdgeRatio.Defined() {
		t.Error("edge ratio should be defined when MAE is non-zero")
	}
}

func TestBuildTradesEmpty(t *testing.T) {
	if got := BuildTrades(nil, nil); got != nil {
		t.Errorf("no fills should produce no trades, got %v", got)
	}
	s := ComputeTradeStats(nil)
	if s.Closed != 0 || s.WinRate != 0 {
		t.Errorf("empty stats should be zero: %+v", s)
	}
}
