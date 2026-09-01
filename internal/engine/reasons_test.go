package engine

import (
	"context"
	"fmt"
	"math"
	"testing"
)

// reasonFill is a fill that says why it happened, which is the whole input to
// this file.
func reasonFill(day, sym string, side Side, shares, price float64, reason string) Fill {
	f := fill(day, sym, side, shares, price)
	f.Reason = reason
	return f
}

// findReason looks a row up by its displayed label.
func findReason(rows []ReasonStats, label string) *ReasonStats {
	for i := range rows {
		if rows[i].Reason == label {
			return &rows[i]
		}
	}
	return nil
}

func TestReasonAttributionSplitsRulesWithKnownPnL(t *testing.T) {
	// Two entry rules and two exit rules over three round trips with
	// arithmetic simple enough to check by eye: +300, -100, +200.
	fills := []Fill{
		reasonFill("2024-01-02", "AAA", SideBuy, 10, 100, "50 day crossed above 200 day"),
		reasonFill("2024-01-12", "AAA", SideSell, 10, 130, "trailing stop hit"),
		reasonFill("2024-01-03", "BBB", SideBuy, 10, 100, "RSI 30, oversold"),
		reasonFill("2024-01-13", "BBB", SideSell, 10, 90, "trailing stop hit"),
		reasonFill("2024-01-04", "CCC", SideBuy, 5, 200, "RSI 30, oversold"),
		reasonFill("2024-01-14", "CCC", SideSell, 5, 240, "target reached"),
	}
	trades := BuildTrades(fills, nil)
	ra := ComputeReasonAttribution(trades)
	tbl := ra.Verbatim

	if len(tbl.ByEntry) != 2 {
		t.Fatalf("want 2 entry rules, got %d: %+v", len(tbl.ByEntry), tbl.ByEntry)
	}
	cross := findReason(tbl.ByEntry, "50 day crossed above 200 day")
	rsi := findReason(tbl.ByEntry, "RSI 30, oversold")
	if cross == nil || rsi == nil {
		t.Fatalf("both entry rules must appear: %+v", tbl.ByEntry)
	}
	if cross.Trades != 1 || math.Abs(cross.NetPnL-300) > 1e-9 {
		t.Errorf("crossover rule: got %d trades and %v, want 1 and 300", cross.Trades, cross.NetPnL)
	}
	// The RSI rule made 200 on one trade and lost 100 on the other, so its
	// total is the thing being tested and its win rate is a half.
	if rsi.Trades != 2 || math.Abs(rsi.NetPnL-100) > 1e-9 {
		t.Errorf("RSI rule: got %d trades and %v, want 2 and 100", rsi.Trades, rsi.NetPnL)
	}
	if math.Abs(rsi.MeanPnL-50) > 1e-9 || math.Abs(rsi.WinRate-0.5) > 1e-9 {
		t.Errorf("RSI rule mean %v win rate %v, want 50 and 0.5", rsi.MeanPnL, rsi.WinRate)
	}
	if rsi.MeanDaysHeld != 10 {
		t.Errorf("mean holding period: got %v, want 10", rsi.MeanDaysHeld)
	}

	// The decomposition has to be exhaustive in both directions: a trade
	// dropped or counted twice shows up here and nowhere else.
	var want float64
	for _, tr := range trades {
		if !tr.Open {
			want += tr.NetPnL
		}
	}
	if got := SumNetPnL(tbl.ByEntry); math.Abs(got-want) > 1e-9 {
		t.Errorf("entry rows sum to %v, closed round trips to %v", got, want)
	}
	if got := SumNetPnL(tbl.ByExit); math.Abs(got-want) > 1e-9 {
		t.Errorf("exit rows sum to %v, closed round trips to %v", got, want)
	}
	// And the share column has to be a decomposition of the same total, or
	// it is a set of numbers that merely look like percentages.
	var share float64
	for _, r := range tbl.ByEntry {
		share += float64(r.Share)
	}
	if math.Abs(share-1) > 1e-9 {
		t.Errorf("entry shares sum to %v, want 1", share)
	}

	stop := findReason(tbl.ByExit, "trailing stop hit")
	if stop == nil || stop.Trades != 2 || math.Abs(stop.NetPnL-200) > 1e-9 {
		t.Errorf("exit rule grouping is wrong: %+v", tbl.ByExit)
	}
}

func TestReasonSharesSumToTheWhole(t *testing.T) {
	// Every trade a winner, so the signed shares have nothing to net off
	// against and must account for the entire result.
	fills := []Fill{
		reasonFill("2024-01-02", "AAA", SideBuy, 10, 100, "breakout"),
		reasonFill("2024-01-12", "AAA", SideSell, 10, 130, "target reached"),
		reasonFill("2024-01-03", "BBB", SideBuy, 10, 100, "pullback"),
		reasonFill("2024-01-13", "BBB", SideSell, 10, 120, "target reached"),
	}
	ra := ComputeReasonAttribution(BuildTrades(fills, nil))

	var share float64
	for _, r := range ra.Verbatim.ByEntry {
		if !r.Share.Defined() {
			t.Fatalf("share undefined with 500 of realised P&L: %+v", r)
		}
		share += float64(r.Share)
	}
	if math.Abs(share-1) > 1e-9 {
		t.Errorf("shares sum to %v, want 1", share)
	}
	if got := SumNetPnL(ra.Verbatim.ByEntry); math.Abs(got-500) > 1e-9 {
		t.Errorf("net P&L sums to %v, want 500", got)
	}
	if b := findReason(ra.Verbatim.ByEntry, "breakout"); b == nil ||
		math.Abs(float64(b.Share)-0.6) > 1e-9 {
		t.Errorf("breakout made 300 of 500 and should hold 60%% of it: %+v", b)
	}
	// A rule can be worth more than the whole result when another rule is
	// losing money, which is the case the column exists to make visible.
	loser := []Fill{
		reasonFill("2024-01-04", "CCC", SideBuy, 10, 100, "knife catching"),
		reasonFill("2024-01-14", "CCC", SideSell, 10, 60, "stop loss"),
	}
	mixed := ComputeReasonAttribution(BuildTrades(append(fills, loser...), nil))
	var over float64
	for _, r := range mixed.Verbatim.ByEntry {
		over += float64(r.Share)
	}
	if math.Abs(over-1) > 1e-9 {
		t.Errorf("shares over a mixed table sum to %v, want 1", over)
	}
	if b := findReason(mixed.Verbatim.ByEntry, "breakout"); b == nil || float64(b.Share) <= 1 {
		t.Errorf("300 of a 100 result is more than all of it: %+v", b)
	}
}

func TestReasonAttributionKeepsUnreasonedOrders(t *testing.T) {
	// A rebalance queues orders with nothing to say for them. They are
	// trades like any other, and dropping them would break the sum.
	fills := []Fill{
		reasonFill("2024-01-02", "AAA", SideBuy, 10, 100, ""),
		reasonFill("2024-01-12", "AAA", SideSell, 10, 90, "   "),
		reasonFill("2024-01-03", "BBB", SideBuy, 10, 100, "breakout"),
		reasonFill("2024-01-13", "BBB", SideSell, 10, 130, "target reached"),
	}
	ra := ComputeReasonAttribution(BuildTrades(fills, nil))

	silent := findReason(ra.Verbatim.ByEntry, NoReasonLabel)
	if silent == nil {
		t.Fatalf("an unreasoned entry needs a bucket of its own: %+v", ra.Verbatim.ByEntry)
	}
	if silent.Trades != 1 || math.Abs(silent.NetPnL+100) > 1e-9 {
		t.Errorf("unreasoned bucket: got %d trades and %v, want 1 and -100", silent.Trades, silent.NetPnL)
	}
	// Whitespace is not a reason either, so the exit lands in the same
	// bucket rather than in one called " ".
	if findReason(ra.Verbatim.ByExit, NoReasonLabel) == nil {
		t.Errorf("a whitespace-only exit reason is still no reason: %+v", ra.Verbatim.ByExit)
	}
	if got := SumNetPnL(ra.Verbatim.ByEntry); math.Abs(got-200) > 1e-9 {
		t.Errorf("sum with the unreasoned trade included: got %v, want 200", got)
	}
}

func TestReasonNormalisationFoldsCaseAndWhitespace(t *testing.T) {
	fills := []Fill{
		reasonFill("2024-01-02", "AAA", SideBuy, 10, 100, "  Golden   cross "),
		reasonFill("2024-01-12", "AAA", SideSell, 10, 130, "exit"),
		reasonFill("2024-01-03", "BBB", SideBuy, 10, 100, "golden cross"),
		reasonFill("2024-01-13", "BBB", SideSell, 10, 120, "exit"),
	}
	ra := ComputeReasonAttribution(BuildTrades(fills, nil))
	if len(ra.Verbatim.ByEntry) != 1 {
		t.Fatalf("the same rule typed twice is one rule: %+v", ra.Verbatim.ByEntry)
	}
	row := ra.Verbatim.ByEntry[0]
	if row.Reason != "Golden cross" {
		t.Errorf("display should keep the first spelling seen, got %q", row.Reason)
	}
	if row.Trades != 2 {
		t.Errorf("both trades belong to it, got %d", row.Trades)
	}
}

func TestNumberlessGroupingMergesSweptParameters(t *testing.T) {
	fills := []Fill{
		reasonFill("2024-01-02", "AAA", SideBuy, 10, 100, "50 day crossed above 200 day"),
		reasonFill("2024-01-12", "AAA", SideSell, 10, 130, "trend broke"),
		reasonFill("2024-01-03", "BBB", SideBuy, 10, 100, "20 day crossed above 100 day"),
		reasonFill("2024-01-13", "BBB", SideSell, 10, 90, "trend broke"),
		reasonFill("2024-01-04", "CCC", SideBuy, 10, 100, "RSI 28, oversold"),
		reasonFill("2024-01-14", "CCC", SideSell, 10, 120, "trend broke"),
	}
	ra := ComputeReasonAttribution(BuildTrades(fills, nil))
	if ra.Numberless == nil {
		t.Fatalf("three interpolated reasons collapse to two, so the table is worth having")
	}

	merged := findReason(ra.Numberless.ByEntry, "N day crossed above N day")
	if merged == nil {
		t.Fatalf("the two crossover settings are one rule: %+v", ra.Numberless.ByEntry)
	}
	if merged.Trades != 2 || math.Abs(merged.NetPnL-200) > 1e-9 {
		t.Errorf("merged rule: got %d trades and %v, want 2 and 200", merged.Trades, merged.NetPnL)
	}
	// A different sentence is a different rule whatever the numbers do.
	if findReason(ra.Numberless.ByEntry, "RSI N, oversold") == nil {
		t.Errorf("the RSI rule must stay on its own row: %+v", ra.Numberless.ByEntry)
	}
	if len(ra.Numberless.ByEntry) != 2 {
		t.Errorf("want 2 rules after collapsing, got %d", len(ra.Numberless.ByEntry))
	}
	if got := SumNetPnL(ra.Numberless.ByEntry); math.Abs(got-400) > 1e-9 {
		t.Errorf("collapsing must not change the total: got %v, want 400", got)
	}

	// Four rows is not the problem the collapsed table solves, so the
	// verbatim one stays in front of the reader.
	if ra.Preferred != GroupingVerbatim {
		t.Errorf("preferred grouping over a small table: got %q", ra.Preferred)
	}
	if ra.Table().Grouping != GroupingVerbatim {
		t.Errorf("Table() must follow Preferred, got %q", ra.Table().Grouping)
	}
}

func TestNumberlessGroupingTakesOverWhenReasonsExplode(t *testing.T) {
	// One rule reporting the indicator value it fired at: twelve trades,
	// twelve strings, and a verbatim table that is just the trade list.
	var fills []Fill
	for i := 0; i < 12; i++ {
		day := fmt.Sprintf("2024-01-%02d", i+1)
		exit := fmt.Sprintf("2024-02-%02d", i+1)
		sym := fmt.Sprintf("S%02d", i)
		fills = append(fills,
			reasonFill(day, sym, SideBuy, 10, 100, fmt.Sprintf("RSI %d, oversold", 20+i)),
			reasonFill(exit, sym, SideSell, 10, 110, "recovered"))
	}
	ra := ComputeReasonAttribution(BuildTrades(fills, nil))
	if len(ra.Verbatim.ByEntry) != 12 {
		t.Fatalf("verbatim should keep every string apart, got %d", len(ra.Verbatim.ByEntry))
	}
	if ra.Preferred != GroupingNumberless {
		t.Fatalf("twelve rows for one rule is the case this exists for, got %q", ra.Preferred)
	}
	tbl := ra.Table()
	if tbl.Grouping != GroupingNumberless || len(tbl.ByEntry) != 1 {
		t.Fatalf("want a single collapsed rule, got %d rows under %q", len(tbl.ByEntry), tbl.Grouping)
	}
	if tbl.ByEntry[0].Trades != 12 {
		t.Errorf("collapsed rule should hold all 12 trades, got %d", tbl.ByEntry[0].Trades)
	}
}

func TestReasonAttributionIgnoresOpenPositions(t *testing.T) {
	fills := []Fill{
		reasonFill("2024-01-02", "AAA", SideBuy, 10, 100, "breakout"),
		reasonFill("2024-01-12", "AAA", SideSell, 4, 150, "target reached"),
	}
	trades := BuildTrades(fills, nil)
	ra := ComputeReasonAttribution(trades)
	row := findReason(ra.Verbatim.ByEntry, "breakout")
	if row == nil {
		t.Fatalf("entry rule missing: %+v", ra.Verbatim.ByEntry)
	}
	// Six shares are still held. Their paper gain is not this rule's
	// realised P&L, however good it looks.
	if row.Trades != 1 || math.Abs(row.NetPnL-200) > 1e-9 {
		t.Errorf("only the closed part counts: got %d trades and %v, want 1 and 200",
			row.Trades, row.NetPnL)
	}
}

func TestReasonAttributionSurvivesJSON(t *testing.T) {
	// Two trades that net to nothing leave every share undefined, which is
	// the shape that used to truncate the whole response.
	fills := []Fill{
		reasonFill("2024-01-02", "AAA", SideBuy, 10, 100, "breakout"),
		reasonFill("2024-01-12", "AAA", SideSell, 10, 100, "target reached"),
	}
	ra := ComputeReasonAttribution(BuildTrades(fills, nil))
	if ra.Verbatim.ByEntry[0].Share.Defined() {
		t.Errorf("a share of nothing is undefined, not zero: %+v", ra.Verbatim.ByEntry[0])
	}
	mustEncode(t, "ReasonAttribution", ra)
}

func TestRunAttributesEveryClosedTradeToARule(t *testing.T) {
	spec := baseSpec(`
		function onDay(ctx) {
			for (const sym of ctx.universe()) {
				const rsi = ctx.rsi(sym, 14);
				if (rsi === null) continue;
				if (!ctx.hasPosition(sym) && rsi < 45 && ctx.cash > 20000) {
					ctx.buy(sym, { pctEquity: 0.2 }, "RSI " + rsi.toFixed(0) + ", oversold");
				} else if (ctx.hasPosition(sym) && rsi > 55) {
					ctx.close(sym, "RSI " + rsi.toFixed(0) + ", recovered");
				}
			}
		}
	`)
	res, err := New(spec, newTestStore(t)).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TradeStats.Closed == 0 {
		t.Fatalf("the fixture has to trade for this to test anything")
	}
	var want float64
	for _, tr := range res.Trades {
		if !tr.Open {
			want += tr.NetPnL
		}
	}
	for _, tbl := range []ReasonTable{res.Reasons.Verbatim, res.Reasons.Table()} {
		if got := SumNetPnL(tbl.ByEntry); math.Abs(got-want) > 1e-6 {
			t.Errorf("%s: entry rows sum to %v, closed round trips to %v", tbl.Grouping, got, want)
		}
		if got := SumNetPnL(tbl.ByExit); math.Abs(got-want) > 1e-6 {
			t.Errorf("%s: exit rows sum to %v, closed round trips to %v", tbl.Grouping, got, want)
		}
	}
	// Every reason here carries an indicator value, so the collapsed table
	// is the one worth printing.
	if res.Reasons.Numberless == nil {
		t.Errorf("interpolated reasons should have produced a collapsed table")
	}
}

func TestTopAndBottomKeepsBothEnds(t *testing.T) {
	rows := make([]ReasonStats, 9)
	for i := range rows {
		rows[i] = ReasonStats{Reason: fmt.Sprintf("rule %d", i), NetPnL: float64(9 - i)}
	}
	head, dropped, tail := TopAndBottom(rows, 4)
	if len(head) != 2 || len(tail) != 2 || dropped != 5 {
		t.Fatalf("got %d head, %d dropped, %d tail", len(head), dropped, len(tail))
	}
	// The worst rule is the one the reader is looking for, so it must be
	// the row that survives, not the one that is trimmed.
	if tail[len(tail)-1].Reason != "rule 8" {
		t.Errorf("the last row must be the worst rule, got %q", tail[len(tail)-1].Reason)
	}
	if all, dropped, _ := TopAndBottom(rows, 20); dropped != 0 || len(all) != 9 {
		t.Errorf("a table that fits is not trimmed: got %d rows, %d dropped", len(all), dropped)
	}
}
