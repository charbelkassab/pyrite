package bundle

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
)

// Divergence is the first place two runs of the same bundle disagree.
//
// "Did not match" is not a useful answer: it gives the reader nothing to look
// at. A date and the two numbers can be taken straight to the bars.
type Divergence struct {
	// What names the quantity that differs — "equity", "cash", or a metric.
	What string `json:"what"`
	// Date is the session it differs on, empty for a whole-run statistic.
	Date market.Day `json:"date,omitempty"`
	// Recorded and Replayed are Ratio because either may legitimately be
	// undefined — a Sortino ratio with no losing days — and a bare NaN
	// reaching JSON truncates the whole document.
	Recorded engine.Ratio `json:"recorded"`
	Replayed engine.Ratio `json:"replayed"`
	// RecordedText and ReplayedText carry the two values when what differs is
	// not a number, such as the date in a calendar position.
	RecordedText string `json:"recorded_text,omitempty"`
	ReplayedText string `json:"replayed_text,omitempty"`
}

// String renders the divergence as the one line worth printing.
func (d *Divergence) String() string {
	rec, rep := d.RecordedText, d.ReplayedText
	if rec == "" && rep == "" {
		rec, rep = number(d.Recorded), number(d.Replayed)
	}
	if d.Date != "" {
		return fmt.Sprintf("diverged on %s, %s %s against %s", d.Date, d.What, rec, rep)
	}
	return fmt.Sprintf("diverged on %s, %s against %s", d.What, rec, rep)
}

// Comparison is the answer a re-run gives.
type Comparison struct {
	// Match is true only when every recorded number came back identical.
	Match      bool        `json:"match"`
	Divergence *Divergence `json:"divergence,omitempty"`

	RecordedSessions int `json:"recorded_sessions"`
	ReplaySessions   int `json:"replay_sessions"`
	Compared         int `json:"compared"`

	// Notes record everything that makes this comparison less than a proof:
	// a bundle edited since it was written, a run that called a model, a
	// strategy that threw. They do not stop the comparison.
	Notes   []string `json:"notes,omitempty"`
	Elapsed int64    `json:"elapsed_ms"`

	// Replayed is the result the re-run produced, for a caller that wants to
	// print it.
	Replayed *engine.Result `json:"-"`
}

// Summary is the sentence to print.
func (c *Comparison) Summary() string {
	if c.Match {
		return fmt.Sprintf("Reproduced exactly: %d sessions and every metric identical.", c.Compared)
	}
	if c.Divergence != nil {
		return "Did not reproduce: " + c.Divergence.String() + "."
	}
	return "Did not reproduce."
}

// Rerun executes the bundled spec against the bundled bars and compares the
// result with the one the bundle recorded.
func (b *Bundle) Rerun(ctx context.Context) (*Comparison, error) {
	started := time.Now()

	// No model, no web, no macro series. Those reach the network, and a
	// bundle that needed the network would not be a bundle. A run that used
	// them was never reproducible in the first place, which the notes say.
	eng := engine.New(b.Spec, b.Store())
	eng.MaxAICalls = 0

	res, err := eng.Run(ctx)
	if err != nil {
		return nil, fmt.Errorf("re-run %s: %w", b.Path, err)
	}

	c := &Comparison{
		Replayed:         res,
		RecordedSessions: len(b.Recorded.Curve),
		ReplaySessions:   len(res.Curve),
		Elapsed:          time.Since(started).Milliseconds(),
	}
	if b.Modified {
		c.Notes = append(c.Notes, fmt.Sprintf(
			"the content hash does not match: the bundle records %s and its contents hash to %s, "+
				"so something in it has been changed since it was written",
			short(b.Manifest.ContentSHA256), short(b.ComputedSHA256)))
	}
	if !b.Recorded.Manifest.Reproducible() {
		c.Notes = append(c.Notes, fmt.Sprintf(
			"the recorded run made %d model or web calls and hit cache on %d of them, so it was "+
				"never reproducible and a difference here need not be anybody's fault",
			b.Recorded.Manifest.AICallCount, b.Recorded.Manifest.AICacheHits))
	}
	if res.StrategyErrors > 0 {
		c.Notes = append(c.Notes, fmt.Sprintf("the strategy threw on %d sessions of the re-run", res.StrategyErrors))
	}
	c.Divergence = compare(b.Recorded, res, &c.Compared)
	c.Match = c.Divergence == nil
	return c, nil
}

// compare walks the two runs in the order a reader cares about: the calendar
// first, then the curve day by day, then the whole-run statistics. The first
// difference wins, so the date reported is the earliest one that moved.
func compare(rec RecordedResult, got *engine.Result, compared *int) *Divergence {
	n := len(rec.Curve)
	if len(got.Curve) < n {
		n = len(got.Curve)
	}
	for i := 0; i < n; i++ {
		a, b := rec.Curve[i], got.Curve[i]
		*compared = i + 1
		if a.Date != b.Date {
			return &Divergence{
				What: "session", Date: a.Date,
				RecordedText: string(a.Date), ReplayedText: string(b.Date),
			}
		}
		for _, f := range []struct {
			name string
			a, b float64
		}{
			{"equity", a.Value, b.Value},
			{"cash", a.Cash, b.Cash},
			{"return", a.Return, b.Return},
			{"drawdown", a.Drawdown, b.Drawdown},
			{"exposure", a.Exposure, b.Exposure},
		} {
			if !identical(f.a, f.b) {
				return &Divergence{
					What: f.name, Date: a.Date,
					Recorded: engine.Ratio(f.a), Replayed: engine.Ratio(f.b),
				}
			}
		}
	}
	if len(rec.Curve) != len(got.Curve) {
		return &Divergence{
			What:     "session count",
			Recorded: engine.Ratio(len(rec.Curve)), Replayed: engine.Ratio(len(got.Curve)),
		}
	}

	for _, f := range metricFields(rec, got) {
		if !identical(f.a, f.b) {
			return &Divergence{What: f.name, Recorded: engine.Ratio(f.a), Replayed: engine.Ratio(f.b)}
		}
	}
	return nil
}

type metricField struct {
	name string
	a, b float64
}

// metricFields lists what is compared beyond the curve, named as the report
// names them so a divergence can be looked up.
//
// Spelled out rather than reflected over, because the list is what the
// comparison promises and it should be readable as such.
func metricFields(rec RecordedResult, got *engine.Result) []metricField {
	r, g := rec.Metrics, got.Metrics
	rt, gt := rec.TradeStats, got.TradeStats
	return []metricField{
		{"final value", r.EndValue, g.EndValue},
		{"total return", r.TotalReturn, g.TotalReturn},
		{"CAGR", r.CAGR, g.CAGR},
		{"volatility", r.Volatility, g.Volatility},
		{"Sharpe ratio", float64(r.Sharpe), float64(g.Sharpe)},
		{"Sortino ratio", float64(r.Sortino), float64(g.Sortino)},
		{"Calmar ratio", float64(r.Calmar), float64(g.Calmar)},
		{"max drawdown", r.MaxDrawdown, g.MaxDrawdown},
		{"win rate", r.WinRate, g.WinRate},
		{"trades", float64(r.TotalTrades), float64(g.TotalTrades)},
		{"trade win rate", r.TradeWinRate, g.TradeWinRate},
		{"profit factor", float64(r.ProfitFactor), float64(g.ProfitFactor)},
		{"costs paid", r.TotalCosts, g.TotalCosts},
		{"turnover", r.Turnover, g.Turnover},
		{"fills", float64(rec.Fills), float64(len(got.Fills))},
		{"round trips closed", float64(rt.Closed), float64(gt.Closed)},
		{"expectancy", rt.Expectancy, gt.Expectancy},
	}
}

// identical compares two numbers exactly, with no tolerance.
//
// The engine is bit-reproducible given the same data — a map-ordered summation
// was fixed for exactly this reason — so an exact-match promise is one that
// can be kept, and comparing to a tolerance would throw away the only thing
// that makes a bundle worth handing to anybody.
//
// The single exception is a value that is not a number at all. A Ratio marshals
// every non-finite value to JSON null and reads null back as NaN, so a profit
// factor of positive infinity — no losing trades — leaves the bundle as null
// and returns as NaN. Reporting that as a divergence would be reporting the
// file format, not the data, so two undefined values count as the same answer.
// What produced them is still compared: the trade counts are separate fields.
func identical(a, b float64) bool {
	if !finite(a) && !finite(b) {
		return true
	}
	return a == b
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// number renders a value at full precision. Rounding for display would let a
// divergence print as "104233.11 against 104233.11", which reads as a bug in
// the tool rather than a difference in the data.
func number(r engine.Ratio) string {
	if !r.Defined() {
		return "undefined"
	}
	return strconv.FormatFloat(float64(r), 'g', -1, 64)
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	if sum == "" {
		return "nothing"
	}
	return sum
}
