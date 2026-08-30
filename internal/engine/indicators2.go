package engine

import "math"

// A second file of indicators, following the same contract as the first: input
// is oldest-to-newest, output is the value for the most recent bar, and NaN
// means there was not enough history.
//
// These exist so the compiler does not have to improvise them in JavaScript.
// Every indicator a model hand-rolls is a fresh chance to hand-roll it subtly
// wrong — an EMA seeded from the wrong bar, a Wilder average smoothed as a
// simple one — and the failure is silent, because the output is still a
// plausible number.

// WMA is the linearly weighted moving average: the most recent bar carries n
// times the weight of the oldest in the window.
func WMA(v []float64, n int) float64 {
	if n <= 0 || len(v) < n {
		return math.NaN()
	}
	w := v[len(v)-n:]
	var num, den float64
	for i, x := range w {
		weight := float64(i + 1)
		num += x * weight
		den += weight
	}
	return num / den
}

// HMA is Hull's moving average: 2*WMA(n/2) - WMA(n), smoothed over sqrt(n).
// It tracks price far more closely than an SMA of the same length at the cost
// of overshooting turns.
func HMA(v []float64, n int) float64 {
	if n <= 1 {
		return math.NaN()
	}
	half := n / 2
	root := int(math.Round(math.Sqrt(float64(n))))
	if half < 1 || root < 1 || len(v) < n+root {
		return math.NaN()
	}
	// The final smoothing needs `root` values of the raw Hull series.
	raw := make([]float64, 0, root)
	for i := len(v) - root; i < len(v); i++ {
		w := v[:i+1]
		a, b := WMA(w, half), WMA(w, n)
		if math.IsNaN(a) || math.IsNaN(b) {
			return math.NaN()
		}
		raw = append(raw, 2*a-b)
	}
	return WMA(raw, root)
}

// ROC is the rate of change over n bars, as a fraction.
func ROC(v []float64, n int) float64 {
	if n <= 0 || len(v) < n+1 {
		return math.NaN()
	}
	prev := v[len(v)-1-n]
	if prev == 0 {
		return math.NaN()
	}
	return v[len(v)-1]/prev - 1
}

// TRIX is the rate of change of a triple-smoothed EMA, which strips most of
// the noise an ordinary momentum reading carries.
func TRIX(v []float64, n int) float64 {
	if n <= 0 || len(v) < n*3+2 {
		return math.NaN()
	}
	single := emaSeries(v, n)
	double := emaSeries(single, n)
	triple := emaSeries(double, n)
	if len(triple) < 2 {
		return math.NaN()
	}
	prev := triple[len(triple)-2]
	if prev == 0 {
		return math.NaN()
	}
	return triple[len(triple)-1]/prev - 1
}

// StochasticResult is the %K and %D pair.
type StochasticResult struct {
	K float64 `json:"k"`
	D float64 `json:"d"`
}

// Stochastic reports where the close sits in the recent high-low range, and a
// smoothed version of the same. Both are percentages in 0..100.
func Stochastic(high, low, close []float64, n, smooth int) StochasticResult {
	nan := StochasticResult{math.NaN(), math.NaN()}
	if n <= 0 || smooth <= 0 || len(close) < n+smooth-1 {
		return nan
	}
	if len(high) != len(close) || len(low) != len(close) {
		return nan
	}
	ks := make([]float64, 0, smooth)
	for i := len(close) - smooth; i < len(close); i++ {
		hi := Highest(high[:i+1], n)
		lo := Lowest(low[:i+1], n)
		if math.IsNaN(hi) || math.IsNaN(lo) || hi == lo {
			ks = append(ks, 50)
			continue
		}
		ks = append(ks, (close[i]-lo)/(hi-lo)*100)
	}
	return StochasticResult{K: ks[len(ks)-1], D: SMA(ks, smooth)}
}

// WilliamsR is the stochastic inverted onto -100..0.
func WilliamsR(high, low, close []float64, n int) float64 {
	if n <= 0 || len(close) < n || len(high) != len(close) || len(low) != len(close) {
		return math.NaN()
	}
	hi, lo := Highest(high, n), Lowest(low, n)
	if math.IsNaN(hi) || math.IsNaN(lo) || hi == lo {
		return math.NaN()
	}
	return (hi - close[len(close)-1]) / (hi - lo) * -100
}

// CCI is the commodity channel index: how far the typical price sits from its
// average, scaled by mean absolute deviation.
func CCI(high, low, close []float64, n int) float64 {
	if n <= 0 || len(close) < n || len(high) != len(close) || len(low) != len(close) {
		return math.NaN()
	}
	tp := typicalPrices(high, low, close)
	mean := SMA(tp, n)
	if math.IsNaN(mean) {
		return math.NaN()
	}
	var dev float64
	for _, x := range tp[len(tp)-n:] {
		dev += math.Abs(x - mean)
	}
	dev /= float64(n)
	if dev == 0 {
		return 0
	}
	// 0.015 is Lambert's constant, chosen so roughly 70-80% of readings fall
	// inside -100..100.
	return (tp[len(tp)-1] - mean) / (0.015 * dev)
}

func typicalPrices(high, low, close []float64) []float64 {
	out := make([]float64, len(close))
	for i := range close {
		out[i] = (high[i] + low[i] + close[i]) / 3
	}
	return out
}

// ADXResult carries the trend strength and its two directional components.
type ADXResult struct {
	ADX     float64 `json:"adx"`
	PlusDI  float64 `json:"plus_di"`
	MinusDI float64 `json:"minus_di"`
}

// ADX measures how strongly a market is trending, in either direction. Above
// 25 is conventionally a trend; below 20 is chop.
func ADX(high, low, close []float64, n int) ADXResult {
	nan := ADXResult{math.NaN(), math.NaN(), math.NaN()}
	if n <= 0 || len(close) < n*2+1 || len(high) != len(close) || len(low) != len(close) {
		return nan
	}

	plusDM := make([]float64, 0, len(close)-1)
	minusDM := make([]float64, 0, len(close)-1)
	tr := make([]float64, 0, len(close)-1)
	for i := 1; i < len(close); i++ {
		up := high[i] - high[i-1]
		down := low[i-1] - low[i]
		p, m := 0.0, 0.0
		// Only the larger of the two counts, and only when it is positive:
		// an inside bar contributes no directional movement at all.
		if up > down && up > 0 {
			p = up
		}
		if down > up && down > 0 {
			m = down
		}
		plusDM = append(plusDM, p)
		minusDM = append(minusDM, m)
		tr = append(tr, math.Max(high[i]-low[i],
			math.Max(math.Abs(high[i]-close[i-1]), math.Abs(low[i]-close[i-1]))))
	}

	sTR := wilderSeries(tr, n)
	sPlus := wilderSeries(plusDM, n)
	sMinus := wilderSeries(minusDM, n)
	if len(sTR) == 0 || len(sTR) != len(sPlus) || len(sTR) != len(sMinus) {
		return nan
	}

	dx := make([]float64, 0, len(sTR))
	for i := range sTR {
		if sTR[i] == 0 {
			dx = append(dx, 0)
			continue
		}
		pdi := sPlus[i] / sTR[i] * 100
		mdi := sMinus[i] / sTR[i] * 100
		if pdi+mdi == 0 {
			dx = append(dx, 0)
			continue
		}
		dx = append(dx, math.Abs(pdi-mdi)/(pdi+mdi)*100)
	}
	if len(dx) < n {
		return nan
	}
	adx := SMA(dx[:n], n)
	for _, x := range dx[n:] {
		adx = (adx*float64(n-1) + x) / float64(n)
	}

	last := len(sTR) - 1
	res := ADXResult{ADX: adx}
	if sTR[last] != 0 {
		res.PlusDI = sPlus[last] / sTR[last] * 100
		res.MinusDI = sMinus[last] / sTR[last] * 100
	}
	return res
}

// wilderSeries applies Wilder's smoothing, which is an EMA with 1/n rather
// than 2/(n+1) and is what the classic indicators actually specify.
func wilderSeries(v []float64, n int) []float64 {
	if n <= 0 || len(v) < n {
		return nil
	}
	out := make([]float64, 0, len(v)-n+1)
	var sum float64
	for _, x := range v[:n] {
		sum += x
	}
	out = append(out, sum)
	for _, x := range v[n:] {
		sum = sum - sum/float64(n) + x
		out = append(out, sum)
	}
	return out
}

// OBV is on-balance volume: volume signed by the day's direction, accumulated.
func OBV(close, volume []float64) float64 {
	if len(close) < 2 || len(volume) != len(close) {
		return math.NaN()
	}
	var obv float64
	for i := 1; i < len(close); i++ {
		switch {
		case close[i] > close[i-1]:
			obv += volume[i]
		case close[i] < close[i-1]:
			obv -= volume[i]
		}
	}
	return obv
}

// MFI is the money flow index: RSI computed on volume-weighted typical price.
func MFI(high, low, close, volume []float64, n int) float64 {
	if n <= 0 || len(close) < n+1 || len(volume) != len(close) {
		return math.NaN()
	}
	tp := typicalPrices(high, low, close)
	var pos, neg float64
	for i := len(close) - n; i < len(close); i++ {
		flow := tp[i] * volume[i]
		switch {
		case tp[i] > tp[i-1]:
			pos += flow
		case tp[i] < tp[i-1]:
			neg += flow
		}
	}
	if neg == 0 {
		if pos == 0 {
			return math.NaN()
		}
		return 100
	}
	return 100 - 100/(1+pos/neg)
}

// VWAP is the volume-weighted average price over the last n bars.
func VWAP(high, low, close, volume []float64, n int) float64 {
	if n <= 0 || len(close) < n || len(volume) != len(close) {
		return math.NaN()
	}
	tp := typicalPrices(high, low, close)
	var pv, v float64
	for i := len(close) - n; i < len(close); i++ {
		pv += tp[i] * volume[i]
		v += volume[i]
	}
	if v == 0 {
		return math.NaN()
	}
	return pv / v
}

// CMF is Chaikin money flow: where each bar closed within its range, weighted
// by volume. Positive means accumulation.
func CMF(high, low, close, volume []float64, n int) float64 {
	if n <= 0 || len(close) < n || len(volume) != len(close) {
		return math.NaN()
	}
	var mfv, vol float64
	for i := len(close) - n; i < len(close); i++ {
		rng := high[i] - low[i]
		if rng == 0 {
			continue
		}
		mult := ((close[i] - low[i]) - (high[i] - close[i])) / rng
		mfv += mult * volume[i]
		vol += volume[i]
	}
	if vol == 0 {
		return math.NaN()
	}
	return mfv / vol
}

// ChannelResult is any upper/middle/lower band.
type ChannelResult struct {
	Upper  float64 `json:"upper"`
	Middle float64 `json:"middle"`
	Lower  float64 `json:"lower"`
}

// Donchian is the highest high and lowest low over n bars — the channel the
// original turtle traders broke out of.
func Donchian(high, low []float64, n int) ChannelResult {
	hi, lo := Highest(high, n), Lowest(low, n)
	if math.IsNaN(hi) || math.IsNaN(lo) {
		return ChannelResult{math.NaN(), math.NaN(), math.NaN()}
	}
	return ChannelResult{Upper: hi, Middle: (hi + lo) / 2, Lower: lo}
}

// Keltner is an EMA with bands set by ATR rather than standard deviation, so
// the width tracks realised range instead of dispersion around the mean.
func Keltner(high, low, close []float64, n int, mult float64) ChannelResult {
	nan := ChannelResult{math.NaN(), math.NaN(), math.NaN()}
	mid := EMA(close, n)
	atr := ATR(high, low, close, n)
	if math.IsNaN(mid) || math.IsNaN(atr) {
		return nan
	}
	if mult <= 0 {
		mult = 2
	}
	return ChannelResult{Upper: mid + mult*atr, Middle: mid, Lower: mid - mult*atr}
}

// SuperTrendResult is the trailing stop level and which side of it price sits.
type SuperTrendResult struct {
	Value float64 `json:"value"`
	// Trend is +1 when price is above the band and -1 when below.
	Trend int `json:"trend"`
}

// SuperTrend is an ATR-based trailing band that flips sides on a close through
// it. The stateful part matters: the band only ever tightens while the trend
// holds, so it has to be walked forward rather than computed pointwise.
func SuperTrend(high, low, close []float64, n int, mult float64) SuperTrendResult {
	nan := SuperTrendResult{math.NaN(), 0}
	if n <= 0 || len(close) < n*3 || len(high) != len(close) || len(low) != len(close) {
		return nan
	}
	if mult <= 0 {
		mult = 3
	}

	var prevUpper, prevLower, prevClose float64
	trend := 1
	started := false
	for i := n + 1; i < len(close); i++ {
		atr := ATR(high[:i+1], low[:i+1], close[:i+1], n)
		if math.IsNaN(atr) {
			continue
		}
		mid := (high[i] + low[i]) / 2
		upper := mid + mult*atr
		lower := mid - mult*atr

		if started {
			// A band holds until price closes through it.
			if upper > prevUpper && prevClose <= prevUpper {
				upper = prevUpper
			}
			if lower < prevLower && prevClose >= prevLower {
				lower = prevLower
			}
			if trend > 0 && close[i] < prevLower {
				trend = -1
			} else if trend < 0 && close[i] > prevUpper {
				trend = 1
			}
		} else {
			started = true
			if close[i] < lower {
				trend = -1
			}
		}
		prevUpper, prevLower, prevClose = upper, lower, close[i]
	}
	if !started {
		return nan
	}
	if trend > 0 {
		return SuperTrendResult{Value: prevLower, Trend: 1}
	}
	return SuperTrendResult{Value: prevUpper, Trend: -1}
}

// AroonResult is the pair plus their difference.
type AroonResult struct {
	Up         float64 `json:"up"`
	Down       float64 `json:"down"`
	Oscillator float64 `json:"oscillator"`
}

// Aroon measures how recently the window's extremes occurred, which reads
// trend age rather than trend strength.
func Aroon(high, low []float64, n int) AroonResult {
	nan := AroonResult{math.NaN(), math.NaN(), math.NaN()}
	if n <= 0 || len(high) < n+1 || len(low) != len(high) {
		return nan
	}
	w := n + 1
	hi, lo := high[len(high)-w:], low[len(low)-w:]

	hiIdx, loIdx := 0, 0
	for i := range hi {
		if hi[i] >= hi[hiIdx] {
			hiIdx = i
		}
		if lo[i] <= lo[loIdx] {
			loIdx = i
		}
	}
	up := float64(hiIdx) / float64(n) * 100
	down := float64(loIdx) / float64(n) * 100
	return AroonResult{Up: up, Down: down, Oscillator: up - down}
}

// PSAR is Wilder's parabolic stop and reverse: an accelerating trailing stop
// that tightens with every new extreme in the trend's favour.
func PSAR(high, low []float64, step, max float64) float64 {
	if len(high) < 5 || len(low) != len(high) {
		return math.NaN()
	}
	if step <= 0 {
		step = 0.02
	}
	if max <= 0 {
		max = 0.2
	}

	rising := high[1] >= high[0]
	sar := low[0]
	ep := high[0]
	if !rising {
		sar, ep = high[0], low[0]
	}
	af := step

	for i := 1; i < len(high); i++ {
		sar += af * (ep - sar)
		if rising {
			// The stop may never move above the prior two lows.
			sar = math.Min(sar, math.Min(low[i-1], low[max0(i-2)]))
			if low[i] < sar {
				rising = false
				sar = ep
				ep = low[i]
				af = step
				continue
			}
			if high[i] > ep {
				ep = high[i]
				af = math.Min(af+step, max)
			}
		} else {
			sar = math.Max(sar, math.Max(high[i-1], high[max0(i-2)]))
			if high[i] > sar {
				rising = true
				sar = ep
				ep = high[i]
				af = step
				continue
			}
			if low[i] < ep {
				ep = low[i]
				af = math.Min(af+step, max)
			}
		}
	}
	return sar
}

func max0(i int) int {
	if i < 0 {
		return 0
	}
	return i
}

// IchimokuResult is the four lines that matter for a daily strategy. The
// leading spans are reported at their computed value, not shifted forward:
// shifting them is a plotting convention, and a strategy that acted on a
// shifted span would be reading a value from the future.
type IchimokuResult struct {
	Conversion float64 `json:"conversion"`
	Base       float64 `json:"base"`
	SpanA      float64 `json:"span_a"`
	SpanB      float64 `json:"span_b"`
}

// Ichimoku computes the cloud's components over the usual 9/26/52 windows.
func Ichimoku(high, low []float64, conv, base, span int) IchimokuResult {
	nan := IchimokuResult{math.NaN(), math.NaN(), math.NaN(), math.NaN()}
	if conv <= 0 || base <= 0 || span <= 0 || len(high) < span || len(low) != len(high) {
		return nan
	}
	mid := func(n int) float64 {
		hi, lo := Highest(high, n), Lowest(low, n)
		if math.IsNaN(hi) || math.IsNaN(lo) {
			return math.NaN()
		}
		return (hi + lo) / 2
	}
	c, b, s := mid(conv), mid(base), mid(span)
	return IchimokuResult{Conversion: c, Base: b, SpanA: (c + b) / 2, SpanB: s}
}

// Choppiness is 0..100: high means the market is ranging, low means trending.
// Useful as a filter in front of a breakout rule.
func Choppiness(high, low, close []float64, n int) float64 {
	if n <= 1 || len(close) < n+1 || len(high) != len(close) || len(low) != len(close) {
		return math.NaN()
	}
	var trSum float64
	for i := len(close) - n; i < len(close); i++ {
		trSum += math.Max(high[i]-low[i],
			math.Max(math.Abs(high[i]-close[i-1]), math.Abs(low[i]-close[i-1])))
	}
	hi, lo := Highest(high, n), Lowest(low, n)
	rng := hi - lo
	if rng <= 0 || trSum <= 0 {
		return math.NaN()
	}
	return 100 * math.Log10(trSum/rng) / math.Log10(float64(n))
}

// LinRegResult is a least-squares fit over the window.
type LinRegResult struct {
	// Slope is per bar, in price units.
	Slope float64 `json:"slope"`
	// Intercept is the fitted value at the window's first bar.
	Intercept float64 `json:"intercept"`
	// R2 is how much of the movement the straight line explains.
	R2 float64 `json:"r2"`
	// Forecast is the fitted value at the most recent bar.
	Forecast float64 `json:"forecast"`
}

// LinReg fits a straight line to the last n values.
func LinReg(v []float64, n int) LinRegResult {
	nan := LinRegResult{math.NaN(), math.NaN(), math.NaN(), math.NaN()}
	if n < 3 || len(v) < n {
		return nan
	}
	w := v[len(v)-n:]
	fn := float64(n)
	var sx, sy, sxx, sxy, syy float64
	for i, y := range w {
		x := float64(i)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
		syy += y * y
	}
	den := fn*sxx - sx*sx
	if den == 0 {
		return nan
	}
	slope := (fn*sxy - sx*sy) / den
	intercept := (sy - slope*sx) / fn

	r2 := 0.0
	if d := den * (fn*syy - sy*sy); d > 0 {
		r := (fn*sxy - sx*sy) / math.Sqrt(d)
		r2 = r * r
	}
	return LinRegResult{
		Slope: slope, Intercept: intercept, R2: r2,
		Forecast: intercept + slope*(fn-1),
	}
}
