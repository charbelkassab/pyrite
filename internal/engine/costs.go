package engine

import (
	"context"
	"math"
	"runtime"
	"sync"

	"github.com/charbelkassab/pyrite/internal/market"
)

// CostPoint is one run of the same strategy at a different friction level.
type CostPoint struct {
	SlippageBps float64 `json:"slippage_bps"`
	TotalReturn float64 `json:"total_return"`
	CAGR        float64 `json:"cagr"`
	Sharpe      Ratio   `json:"sharpe"`
	MaxDrawdown float64 `json:"max_drawdown"`
	TotalCosts  float64 `json:"total_costs"`
	Trades      int     `json:"trades"`
	Error       string  `json:"error,omitempty"`
}

// CostScan is what the same idea is worth at several friction levels.
//
// The failure mode this catches is specific and common: a strategy that trades
// often can post an excellent backtest at zero cost and be flatly unprofitable
// at a realistic one, and nothing in the headline numbers distinguishes the two.
// Turnover tells you it might be a problem; this tells you whether it is.
type CostScan struct {
	Points []CostPoint `json:"points"`
	// BreakEvenBps is where the strategy stops making money, interpolated
	// between the two points that bracket zero return. Null when it never
	// crosses inside the range scanned.
	BreakEvenBps Ratio `json:"break_even_bps"`
	// Verdict states the finding.
	Verdict string `json:"verdict"`
}

// DefaultCostLevels spans free to punitive: zero for comparison with published
// backtests that charge nothing, five as the tool's own default, twenty for a
// small account or a thin name, fifty for something genuinely illiquid.
var DefaultCostLevels = []float64{0, 5, 20, 50}

// RunCostScan re-runs a spec at each friction level, in parallel.
func RunCostScan(ctx context.Context, base Spec, store *market.Store, levels []float64) (*CostScan, error) {
	if len(levels) == 0 {
		levels = DefaultCostLevels
	}
	base.ApplyDefaults()

	out := &CostScan{Points: make([]CostPoint, len(levels)), BreakEvenBps: Ratio(math.NaN())}
	sem := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup

	for i, bps := range levels {
		wg.Add(1)
		go func(i int, bps float64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			spec := base
			spec.OmitDayRecords = true
			spec.Costs.SlippageBps = bps
			p := CostPoint{SlippageBps: bps, Sharpe: Ratio(math.NaN())}

			res, err := New(spec, store).Run(ctx)
			if err != nil {
				p.Error = truncateErr(err.Error())
			} else {
				p.TotalReturn = res.Metrics.TotalReturn
				p.CAGR = res.Metrics.CAGR
				p.Sharpe = res.Metrics.Sharpe
				p.MaxDrawdown = res.Metrics.MaxDrawdown
				p.TotalCosts = res.Metrics.TotalCosts
				p.Trades = res.TradeStats.Closed
			}
			out.Points[i] = p
		}(i, bps)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out.BreakEvenBps = breakEven(out.Points)
	out.Verdict = costVerdict(out)
	return out, nil
}

// breakEven interpolates where total return crosses zero.
func breakEven(points []CostPoint) Ratio {
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		if a.Error != "" || b.Error != "" {
			continue
		}
		if a.TotalReturn > 0 && b.TotalReturn <= 0 {
			span := a.TotalReturn - b.TotalReturn
			if span == 0 {
				return Ratio(a.SlippageBps)
			}
			t := a.TotalReturn / span
			return Ratio(a.SlippageBps + t*(b.SlippageBps-a.SlippageBps))
		}
	}
	return Ratio(math.NaN())
}

// costVerdict states what the scan found, in the terms a reader needs.
func costVerdict(s *CostScan) string {
	var first, last *CostPoint
	for i := range s.Points {
		if s.Points[i].Error != "" {
			continue
		}
		if first == nil {
			first = &s.Points[i]
		}
		last = &s.Points[i]
	}
	if first == nil || last == nil || first == last {
		return ""
	}

	if first.TotalReturn <= 0 {
		return "the strategy does not make money even with zero friction, so costs are not the problem"
	}
	if s.BreakEvenBps.Defined() {
		bps := float64(s.BreakEvenBps)
		if bps < 5 {
			return "the edge disappears below 5 bps of slippage — less than this tool charges by " +
				"default, and far less than a retail account pays. This is a costs artefact, not a strategy"
		}
		if bps < 20 {
			return "the edge breaks even at around " + trimFloat(bps) + " bps of slippage. That is " +
				"inside the range a real account would pay on anything but the most liquid names"
		}
		return "the edge survives to around " + trimFloat(bps) + " bps of slippage before it disappears"
	}
	// Never crossed zero: report how much of the return the friction took.
	if first.TotalReturn > 0 {
		lost := (first.TotalReturn - last.TotalReturn) / first.TotalReturn
		if lost > 0.5 {
			return "friction of " + trimFloat(last.SlippageBps) + " bps removes " +
				fmtPercent(lost) + " of the return, but the strategy stays profitable across the range"
		}
		return "costs barely touch this: " + trimFloat(last.SlippageBps) + " bps of slippage removes only " +
			fmtPercent(lost) + " of the return"
	}
	return ""
}

func trimFloat(v float64) string {
	if v == math.Trunc(v) {
		return fmtInt(int(v))
	}
	return fmtFloat1(v)
}

// Small formatting helpers, kept local so the engine stays free of a
// dependency on how any particular front end wants to render a number.
func fmtInt(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func fmtFloat1(v float64) string {
	whole := int(v)
	frac := int(math.Abs(v-float64(whole))*10 + 0.5)
	if frac >= 10 {
		whole++
		frac = 0
	}
	return fmtInt(whole) + "." + fmtInt(frac)
}

func fmtPercent(frac float64) string {
	return fmtInt(int(frac*100+0.5)) + "%"
}
