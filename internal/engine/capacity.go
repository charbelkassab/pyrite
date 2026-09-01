package engine

import (
	"context"
	"math"
	"runtime"
	"sync"

	"github.com/charbelkassab/pyrite/internal/market"
)

// CapacityPoint is one run of the same strategy at a different account size.
type CapacityPoint struct {
	Capital     float64 `json:"capital"`
	TotalReturn float64 `json:"total_return"`
	CAGR        float64 `json:"cagr"`
	Sharpe      Ratio   `json:"sharpe"`
	MaxDrawdown float64 `json:"max_drawdown"`
	TotalCosts  float64 `json:"total_costs"`
	// CostBps is friction as basis points of the notional actually traded,
	// which is the form that isolates what this ladder is testing. The dollar
	// total grows with the account whatever happens, and friction against
	// starting capital falls once a rung has lost enough money to trade
	// smaller. Cost per dollar traded does neither: under the square-root law
	// it can only rise with order size.
	CostBps float64 `json:"cost_bps"`
	// Traded is the gross notional the rung put through the market.
	Traded float64 `json:"traded"`
	// Turnover is carried so that a strategy which never deployed the larger
	// accounts can be told apart from one that deployed them and coped.
	Turnover float64 `json:"turnover"`
	Trades   int     `json:"trades"`
	Error    string  `json:"error,omitempty"`
}

// Capacity is what the same idea is worth at several account sizes.
//
// A backtest on $100,000 says nothing about whether the idea survives at size,
// and nothing in the headline numbers hints at the difference. Slippage as a
// flat spread is a fixed cost per share, so with it alone a $1bn order costs
// the same fraction as a $1,000 one and every strategy scales for ever. With
// the square-root impact model wired in, this ladder answers the question the
// small-account backtest cannot ask: how much money can the idea take before
// its own trading eats the edge?
//
// The threshold it reports is an estimate off five rungs, not a measured
// limit. It is stated that way everywhere it appears.
type Capacity struct {
	Points []CapacityPoint `json:"points"`
	// ImpactCoefficient is the k every rung was run with. The ladder means
	// nothing without it, so it travels with the result.
	ImpactCoefficient float64 `json:"impact_coefficient"`

	// ZeroReturnCapital is the largest account that still finishes above
	// zero, interpolated between the two rungs that bracket the crossing.
	// Null when the return never crosses zero inside the ladder.
	ZeroReturnCapital Ratio `json:"zero_return_capital"`
	// BenchmarkCapital is the same figure measured against the first
	// benchmark rather than against zero. It is the harder and more useful
	// test: a strategy that beats zero and loses to an index fund has no
	// capacity worth the name.
	BenchmarkCapital Ratio  `json:"benchmark_capital"`
	BenchmarkLabel   string `json:"benchmark_label,omitempty"`
	BenchmarkReturn  Ratio  `json:"benchmark_return"`

	// Degradation is the cumulative return given up between the smallest and
	// the largest rung.
	Degradation Ratio `json:"degradation"`
	// Bites reports whether size cost anything at all across the ladder.
	// When it is false there is no threshold to report, and inventing one
	// from the nearest rung is exactly the sort of plausible wrong number
	// this tool exists to catch.
	Bites bool `json:"bites"`
	// Deployed reports whether the larger accounts were actually put to
	// work. A strategy that buys a fixed dollar amount submits the same
	// order at every rung, so its ladder measures nothing about capacity.
	Deployed bool `json:"deployed"`

	// Verdict states the finding.
	Verdict string `json:"verdict"`
}

// DefaultCapacityLadder spans the sizes the answer changes between: a retail
// account, a serious private one, a small fund, a large one, and the point at
// which a single-name equity strategy is trading a meaningful share of the
// tape. Each rung is ten times the last, because capacity is a question about
// orders of magnitude and a finer ladder would cost ten runs to sharpen an
// estimate the model underneath does not support.
var DefaultCapacityLadder = []float64{1e5, 1e6, 1e7, 1e8, 1e9}

// DefaultImpactCoefficient is the k used when the ladder is run against a spec
// that had impact switched off. One is the usual empirical estimate: an order
// for a full day's volume moves the price by about one daily deviation.
const DefaultImpactCoefficient = 1.0

// capacityBiteThreshold and capacityBiteShare are how much cumulative return
// has to be lost between the smallest and largest rung before size is worth
// calling a constraint. Both must be exceeded.
//
// Two points of cumulative return over a whole backtest, across a ten
// thousandfold increase in capital, is inside the noise of the fill model.
// The share is there because two points means something different to a
// strategy that returned 3% and one that returned 300%: the first has lost
// most of its edge and the second has lost a rounding error. Failing either
// test means there is no capacity limit this ladder can see, and saying so
// plainly is more use than a threshold interpolated out of noise.
const (
	capacityBiteThreshold = 0.02
	capacityBiteShare     = 0.05
)

// capacityDeployRatio is how far turnover may fall from the smallest rung to
// the largest before the ladder is judged not to have deployed the capital.
//
// A strategy sized as a share of the account holds turnover constant across
// the ladder by construction. One that buys a fixed dollar amount divides it
// by ten at every rung, and the flat return that follows is a fact about the
// position sizing rather than about capacity.
const capacityDeployRatio = 0.5

// RunCapacity re-runs a spec at each account size on the ladder, in parallel,
// with the market impact model switched on.
//
// Impact is forced on rather than inherited: a capacity ladder run without it
// measures nothing, because the only cost that scales with order size is the
// one it models. k comes from the caller, then from the spec, then from the
// default.
func RunCapacity(ctx context.Context, base Spec, store *market.Store, ladder []float64, k float64) (*Capacity, error) {
	if len(ladder) == 0 {
		ladder = DefaultCapacityLadder
	}
	base.ApplyDefaults()
	if k <= 0 {
		k = base.Costs.ImpactCoefficient
	}
	if k <= 0 {
		k = DefaultImpactCoefficient
	}

	nan := Ratio(math.NaN())
	out := &Capacity{
		Points:            make([]CapacityPoint, len(ladder)),
		ImpactCoefficient: k,
		ZeroReturnCapital: nan, BenchmarkCapital: nan,
		BenchmarkReturn: nan, Degradation: nan,
	}
	// The benchmark is a buy-and-hold curve, so its return is identical at
	// every rung. It is collected per rung anyway because the runs are
	// concurrent and any one of them may have failed.
	benchLabels := make([]string, len(ladder))
	benchReturns := make([]float64, len(ladder))

	sem := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup

	for i, capital := range ladder {
		wg.Add(1)
		go func(i int, capital float64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			spec := base
			spec.OmitDayRecords = true
			spec.InitialCash = capital
			spec.Costs.ImpactCoefficient = k
			p := CapacityPoint{Capital: capital, Sharpe: Ratio(math.NaN())}

			res, err := New(spec, store).Run(ctx)
			if err != nil {
				p.Error = truncateErr(err.Error())
			} else {
				p.TotalReturn = res.Metrics.TotalReturn
				p.CAGR = res.Metrics.CAGR
				p.Sharpe = res.Metrics.Sharpe
				p.MaxDrawdown = res.Metrics.MaxDrawdown
				p.TotalCosts = res.Metrics.TotalCosts
				p.Turnover = res.Metrics.Turnover
				p.Trades = res.TradeStats.Closed
				for _, f := range res.Fills {
					p.Traded += f.Value
				}
				if p.Traded > 0 {
					p.CostBps = 1e4 * res.Metrics.TotalCosts / p.Traded
				}
				if len(res.Benchmarks) > 0 {
					benchLabels[i] = res.Benchmarks[0].Label
					benchReturns[i] = res.Benchmarks[0].Metric.TotalReturn
				}
			}
			out.Points[i] = p
		}(i, capital)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i := range benchLabels {
		if benchLabels[i] != "" {
			out.BenchmarkLabel = benchLabels[i]
			out.BenchmarkReturn = Ratio(benchReturns[i])
			break
		}
	}

	first, last := capacityEnds(out.Points)
	if first != nil && last != nil && first != last {
		lost := first.TotalReturn - last.TotalReturn
		out.Degradation = Ratio(lost)
		// A rung that finished at exactly zero has no return to take a share
		// of, so the absolute test decides it alone.
		share := math.Inf(1)
		if magnitude := math.Abs(first.TotalReturn); magnitude > 0 {
			share = lost / magnitude
		}
		out.Bites = lost > capacityBiteThreshold && share > capacityBiteShare
		// Turnover of zero at both ends means the strategy never traded, and
		// calling that "did not deploy the capital" would blame the ladder
		// for a strategy that did nothing at any size.
		out.Deployed = first.Turnover <= 0 ||
			last.Turnover >= first.Turnover*capacityDeployRatio
	}
	out.ZeroReturnCapital = capacityCrossing(out.Points, 0)
	if out.BenchmarkReturn.Defined() {
		out.BenchmarkCapital = capacityCrossing(out.Points, float64(out.BenchmarkReturn))
	}
	out.Verdict = capacityVerdict(out)
	return out, nil
}

// capacityEnds returns the smallest and largest rungs that produced a result.
func capacityEnds(points []CapacityPoint) (first, last *CapacityPoint) {
	for i := range points {
		if points[i].Error != "" {
			continue
		}
		if first == nil {
			first = &points[i]
		}
		last = &points[i]
	}
	return first, last
}

// capacityCrossing interpolates the account size at which the ladder's return
// falls through a threshold.
//
// The interpolation is linear in the logarithm of capital, because the ladder
// is geometric. Interpolating in dollars would put a crossing halfway between
// $100m and $1bn at $550m whatever the returns said, which is a property of
// the rung spacing rather than of the strategy.
func capacityCrossing(points []CapacityPoint, target float64) Ratio {
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		if a.Error != "" || b.Error != "" || a.Capital <= 0 || b.Capital <= 0 {
			continue
		}
		above, below := a.TotalReturn-target, b.TotalReturn-target
		if above <= 0 || below > 0 {
			continue
		}
		span := above - below
		if span == 0 {
			return Ratio(a.Capital)
		}
		t := above / span
		lo, hi := math.Log10(a.Capital), math.Log10(b.Capital)
		return Ratio(math.Pow(10, lo+t*(hi-lo)))
	}
	return Ratio(math.NaN())
}

// capacityVerdict states what the ladder found, in the terms a reader needs.
func capacityVerdict(c *Capacity) string {
	first, last := capacityEnds(c.Points)
	if first == nil || last == nil || first == last {
		return ""
	}
	small, large := money(first.Capital), money(last.Capital)

	if !c.Deployed {
		return "the ladder measures nothing here: turnover fell from " +
			fmtFloat1(first.Turnover) + "x to " + fmtFloat1(last.Turnover) +
			"x as the account grew, so the larger accounts were never put to work. " +
			"Position sizes are fixed in dollars rather than as a share of capital, " +
			"and a strategy that trades the same order at " + small + " and at " +
			large + " has no capacity question to answer"
	}
	if !c.Bites {
		return "size never bites on this ladder: " + large + " returns " +
			fmtPercent1(last.TotalReturn) + " against " + fmtPercent1(first.TotalReturn) +
			" at " + small + ". The strategy trades too rarely, or in names too " +
			"liquid, for impact to reach it below " + large + " — there is no " +
			"capacity limit here to report"
	}
	if first.TotalReturn <= 0 {
		return "the strategy loses money at the smallest size on the ladder, so " +
			"capacity is not what is wrong with it — though size makes it worse: " +
			fmtPercent1(first.TotalReturn) + " at " + small + " becomes " +
			fmtPercent1(last.TotalReturn) + " at " + large
	}
	if c.ZeroReturnCapital.Defined() {
		v := "the edge is gone above roughly " + money(float64(c.ZeroReturnCapital)) +
			"; at " + large + " this returns " + fmtPercent1(last.TotalReturn)
		return v + capacityBenchmarkClause(c) +
			". Both figures are estimates interpolated from a five-rung ladder"
	}
	v := "the edge survives the whole ladder: " + large + " still returns " +
		fmtPercent1(last.TotalReturn) + ", down from " + fmtPercent1(first.TotalReturn) +
		" at " + small
	return v + capacityBenchmarkClause(c)
}

// capacityBenchmarkClause adds the size at which the strategy stops beating
// the thing anyone could have bought instead.
func capacityBenchmarkClause(c *Capacity) string {
	if !c.BenchmarkCapital.Defined() || c.BenchmarkLabel == "" {
		return ""
	}
	return ", and it stops beating " + c.BenchmarkLabel + " above roughly " +
		money(float64(c.BenchmarkCapital))
}
