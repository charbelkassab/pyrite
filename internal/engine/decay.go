package engine

import (
	"math"

	"github.com/charbelkassab/pyrite/internal/market"
)

// DefaultDecayHorizons are the bars after entry at which the average round
// trip is measured.
//
// The spacing is roughly geometric because that is the shape of the thing
// being measured: whatever a signal is going to do in its first week decides
// most of the answer, and the difference between the thirty-ninth and the
// fortieth bar decides none of it.
var DefaultDecayHorizons = []int{1, 2, 3, 5, 10, 20, 40}

// DecayPoint is the average cumulative return of the closed round trips this
// many bars after they were entered.
type DecayPoint struct {
	Bars int `json:"bars"`
	// MeanReturn is gross: the price move from the entry price, signed by
	// direction, with no costs taken out. That is the convention MAE and MFE
	// already use in trades.go, and for the same reason — this measures what
	// the signal did, not what the execution kept of it.
	MeanReturn float64 `json:"mean_return"`
	// StillOpen is how many trades were still held at this horizon. A trade
	// shorter than the horizon contributes the return it finished with, so
	// the sample never changes size between horizons; where StillOpen falls
	// away, the curve flattens because exited trades are being carried
	// forward rather than because the signal stopped working.
	StillOpen int `json:"still_open"`
}

// SignalDecay is the shape of the average trade's edge over time.
//
// The equity curve says whether the idea made money. This says when. A
// strategy whose average trade is at its best three bars after entry and is
// then held for forty is a different animal from one that compounds for a
// quarter, and the two can post the same Sharpe. Where the curve peaks is
// where the signal was worth acting on; the distance between the peak and the
// exit is what the exit rule cost.
//
// It is the give-back statistic in trades.go asked the other way round.
// TradeStats.GiveBack asks, of the losing trades, how much of their own peak
// paper profit they surrendered — an answer in per cent. This asks it of every
// closed trade at once on a common clock, so the answer is a holding period.
type SignalDecay struct {
	Points []DecayPoint `json:"points"`
	// Trades is the number of closed round trips the curve averages.
	Trades int `json:"trades"`

	// PeakBars is the horizon at which the mean is highest, and PeakReturn is
	// the mean there.
	PeakBars   int   `json:"peak_bars"`
	PeakReturn Ratio `json:"peak_return"`
	// ExitReturn is the mean gross return the trades actually finished with,
	// which is what the exit rule delivered out of the peak above.
	ExitReturn Ratio `json:"exit_return"`
	// MeanBarsHeld is the average holding period over the same trades,
	// counted the way trades.go counts BarsHeld: inclusive of the entry bar.
	// A horizon is not, so the two differ by one and the code that compares
	// them says so.
	MeanBarsHeld float64 `json:"mean_bars_held"`
	// StillRising reports whether the mean curve was still climbing at the
	// last horizon inside the average holding period. False means the edge
	// had already turned over before the average trade was closed.
	StillRising bool `json:"still_rising"`
	// GivenBack is the fraction of the peak the exit surrendered. Undefined
	// when the peak is too small to have had anything to give back, and when
	// the peak falls on the last horizon — a curve that never turned over
	// inside the window measured has given nothing back yet.
	GivenBack Ratio `json:"given_back"`

	// Verdict states the finding.
	Verdict string `json:"verdict"`
}

// minDecayTrades is how many closed round trips the curve needs before it is
// worth reading. It matches the threshold the critique already uses for trade
// statistics: below twenty, one different outcome moves every point on the
// curve, and a peak read off that is noise with a bar number attached.
const minDecayTrades = 20

// decayGiveBackFloor is the smallest peak worth measuring a give-back
// against, and is the same 1% floor trades.go applies for the same reason: a
// curve that never rose more than a few basis points has no profit to have
// surrendered, and dividing by that sliver produces figures in the hundreds
// of per cent that say nothing about the exit rule.
const decayGiveBackFloor = 0.01

// ComputeDecay builds the decay curve from closed round trips.
//
// Open trades are excluded, as they are from every other trade statistic:
// their outcome is not known yet and folding a paper position into the curve
// would let the run congratulate itself for a trade it has not finished.
func ComputeDecay(trades []Trade, series map[string]*market.Series, horizons []int) SignalDecay {
	if len(horizons) == 0 {
		horizons = DefaultDecayHorizons
	}
	nan := Ratio(math.NaN())
	d := SignalDecay{PeakReturn: nan, ExitReturn: nan, GivenBack: nan}
	if len(trades) == 0 || len(series) == 0 {
		return d
	}

	sums := make([]float64, len(horizons))
	open := make([]int, len(horizons))
	var exitSum, barsSum float64

	for _, t := range trades {
		if t.Open || t.EntryPrice <= 0 || t.ExitDate == "" {
			continue
		}
		s := series[t.Symbol]
		if s == nil {
			continue
		}
		// The same window enrichExcursion walks, so BarsHeld and the horizons
		// are counted off the same bars. bars[0] is the entry bar, so h bars
		// after entry is bars[h].
		bars := s.Range(t.EntryDate, t.ExitDate)
		if len(bars) < 2 {
			continue
		}
		sign := 1.0
		if t.Direction == DirShort {
			sign = -1
		}
		at := func(i int) float64 {
			p := bars[i].AdjClose
			if p <= 0 {
				return math.NaN()
			}
			return sign * (p/t.EntryPrice - 1)
		}
		final := at(len(bars) - 1)
		if math.IsNaN(final) {
			continue
		}

		d.Trades++
		exitSum += final
		barsSum += float64(len(bars))
		for j, h := range horizons {
			// A horizon past the exit contributes the return the trade
			// finished with. Contributing zero would drag the curve towards
			// the axis at every long horizon and manufacture a decay out of
			// the holding period distribution alone.
			i, held := h, true
			if i > len(bars)-1 {
				i, held = len(bars)-1, false
			}
			v := at(i)
			if math.IsNaN(v) {
				v = final
			}
			sums[j] += v
			if held {
				open[j]++
			}
		}
	}
	if d.Trades == 0 {
		return d
	}

	n := float64(d.Trades)
	d.Points = make([]DecayPoint, len(horizons))
	for j, h := range horizons {
		d.Points[j] = DecayPoint{Bars: h, MeanReturn: sums[j] / n, StillOpen: open[j]}
	}
	d.ExitReturn = Ratio(exitSum / n)
	d.MeanBarsHeld = barsSum / n

	peak := 0
	for j := range d.Points {
		if d.Points[j].MeanReturn > d.Points[peak].MeanReturn {
			peak = j
		}
	}
	d.PeakBars = d.Points[peak].Bars
	d.PeakReturn = Ratio(d.Points[peak].MeanReturn)
	d.StillRising = decayStillRising(d.Points, d.MeanBarsHeld)
	// A peak on the last horizon means the curve never turned over inside the
	// window measured. There is nothing given back yet, and dividing an exit
	// hundreds of bars later by it would report the horizon grid rather than
	// the exit rule.
	if pk := d.Points[peak].MeanReturn; pk >= decayGiveBackFloor && peak < len(d.Points)-1 {
		d.GivenBack = Ratio((pk - float64(d.ExitReturn)) / pk)
	}
	d.Verdict = decayVerdict(&d)
	return d
}

// decayStillRising reports whether the mean curve was climbing at the last
// horizon the average trade was still open for.
//
// BarsHeld counts the entry bar and a horizon does not, so a trade held for
// two bars reached horizon 1 and no further. Comparing the two without that
// adjustment would credit the average trade with one bar it never had.
func decayStillRising(points []DecayPoint, meanBarsHeld float64) bool {
	if len(points) < 2 {
		return true
	}
	last := -1
	for i, p := range points {
		if float64(p.Bars) <= meanBarsHeld-1 {
			last = i
		}
	}
	// The average trade closed before the first horizon, so nothing has had
	// the chance to turn over yet.
	if last <= 0 {
		return true
	}
	return points[last].MeanReturn >= points[last-1].MeanReturn
}

// decayVerdict states what the curve found.
func decayVerdict(d *SignalDecay) string {
	if d.Trades == 0 {
		return "no closed round trip had enough price history to measure a decay curve"
	}
	if d.Trades < minDecayTrades {
		return "only " + fmtInt(d.Trades) + " closed round trips, too few to average a " +
			"decay curve that means anything"
	}
	hold := fmtFloat1(d.MeanBarsHeld)
	peak := "the average trade is at its best " + plural(d.PeakBars, "bar") + " after entry"

	if !d.PeakReturn.Defined() || float64(d.PeakReturn) <= 0 {
		return "the average trade never gets in front at any horizon measured, so there " +
			"is no decay to describe — the entries are not finding anything to give back"
	}
	// The horizons stop at 40 bars, which is two months of daily trading. A
	// strategy that holds for longer than that has not been measured to its
	// exit, and reporting a peak against it would compare the signal to the
	// end of the grid rather than to the exit rule.
	if longest := float64(d.Points[len(d.Points)-1].Bars); d.MeanBarsHeld-1 > longest {
		return peak + ", but the average trade is held for " + hold + " bars — past " +
			"the longest horizon measured. This curve describes the opening " +
			fmtInt(int(longest)) + " bars of the trade and says nothing about the exit"
	}
	if d.StillRising {
		return peak + " and is held for " + hold + " bars on average, so the edge is " +
			"still building when the position is closed. If anything the exit is early"
	}
	if !d.GivenBack.Defined() || float64(d.GivenBack) <= 0 {
		return peak + ", against an average hold of " + hold + " bars. The curve turns " +
			"over inside the holding period, but the exits still land above every " +
			"horizon on the grid, so there is no give-back to measure"
	}
	given := float64(d.GivenBack)
	if given >= 0.5 {
		return peak + " at " + fmtPercent1(float64(d.PeakReturn)) + ", and is held for " +
			hold + " bars. By the exit it has given back " + fmtPercent(given) +
			" of that, finishing at " + fmtPercent1(float64(d.ExitReturn)) +
			". The signal is a short-horizon effect and the holding period is not: " +
			"the exit rule is the thing to change, not the entry"
	}
	return peak + " at " + fmtPercent1(float64(d.PeakReturn)) + " and is held for " +
		hold + " bars, giving back " + fmtPercent(given) + " of the peak by the exit"
}
