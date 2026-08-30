package engine

import "math"

// Technical indicators operate on a slice of closes ordered oldest to newest
// and return the value for the most recent bar. They return NaN when there is
// not enough history, which the JS layer converts to null so a strategy can
// test for it naturally.

// SMA is the simple moving average of the last n values.
func SMA(v []float64, n int) float64 {
	if n <= 0 || len(v) < n {
		return math.NaN()
	}
	var sum float64
	for _, x := range v[len(v)-n:] {
		sum += x
	}
	return sum / float64(n)
}

// EMA is the exponential moving average with the standard 2/(n+1) smoothing.
// It is seeded with the SMA of the first n values.
func EMA(v []float64, n int) float64 {
	if n <= 0 || len(v) < n {
		return math.NaN()
	}
	k := 2.0 / float64(n+1)
	ema := SMA(v[:n], n)
	for _, x := range v[n:] {
		ema = x*k + ema*(1-k)
	}
	return ema
}

// emaSeries returns the full EMA series, needed by MACD.
func emaSeries(v []float64, n int) []float64 {
	if n <= 0 || len(v) < n {
		return nil
	}
	out := make([]float64, 0, len(v)-n+1)
	k := 2.0 / float64(n+1)
	ema := SMA(v[:n], n)
	out = append(out, ema)
	for _, x := range v[n:] {
		ema = x*k + ema*(1-k)
		out = append(out, ema)
	}
	return out
}

// RSI is Wilder's relative strength index over n periods.
func RSI(v []float64, n int) float64 {
	if n <= 0 || len(v) < n+1 {
		return math.NaN()
	}
	var gain, loss float64
	// Seed with the first n changes.
	for i := 1; i <= n; i++ {
		d := v[i] - v[i-1]
		if d > 0 {
			gain += d
		} else {
			loss -= d
		}
	}
	gain /= float64(n)
	loss /= float64(n)
	// Wilder smoothing over the remainder.
	for i := n + 1; i < len(v); i++ {
		d := v[i] - v[i-1]
		g, l := 0.0, 0.0
		if d > 0 {
			g = d
		} else {
			l = -d
		}
		gain = (gain*float64(n-1) + g) / float64(n)
		loss = (loss*float64(n-1) + l) / float64(n)
	}
	if loss == 0 {
		if gain == 0 {
			return 50
		}
		return 100
	}
	rs := gain / loss
	return 100 - 100/(1+rs)
}

// MACDResult holds the three MACD lines.
type MACDResult struct {
	MACD      float64 `json:"macd"`
	Signal    float64 `json:"signal"`
	Histogram float64 `json:"histogram"`
}

// MACD returns the moving average convergence divergence values.
func MACD(v []float64, fast, slow, signal int) MACDResult {
	nan := MACDResult{math.NaN(), math.NaN(), math.NaN()}
	if fast <= 0 || slow <= 0 || signal <= 0 || fast >= slow || len(v) < slow+signal {
		return nan
	}
	fastSeries := emaSeries(v, fast)
	slowSeries := emaSeries(v, slow)
	if fastSeries == nil || slowSeries == nil {
		return nan
	}
	// Align: the slow series starts (slow-fast) entries later.
	offset := len(fastSeries) - len(slowSeries)
	if offset < 0 {
		return nan
	}
	macdLine := make([]float64, len(slowSeries))
	for i := range slowSeries {
		macdLine[i] = fastSeries[i+offset] - slowSeries[i]
	}
	if len(macdLine) < signal {
		return nan
	}
	sig := EMA(macdLine, signal)
	m := macdLine[len(macdLine)-1]
	return MACDResult{MACD: m, Signal: sig, Histogram: m - sig}
}

// Stdev is the sample standard deviation of the last n values.
func Stdev(v []float64, n int) float64 {
	if n <= 1 || len(v) < n {
		return math.NaN()
	}
	w := v[len(v)-n:]
	mean := 0.0
	for _, x := range w {
		mean += x
	}
	mean /= float64(n)
	var ss float64
	for _, x := range w {
		d := x - mean
		ss += d * d
	}
	return math.Sqrt(ss / float64(n-1))
}

// BollingerResult holds the three Bollinger bands.
type BollingerResult struct {
	Upper  float64 `json:"upper"`
	Middle float64 `json:"middle"`
	Lower  float64 `json:"lower"`
}

// Bollinger returns bands at k standard deviations around the n-period SMA.
func Bollinger(v []float64, n int, k float64) BollingerResult {
	mid := SMA(v, n)
	sd := Stdev(v, n)
	if math.IsNaN(mid) || math.IsNaN(sd) {
		return BollingerResult{math.NaN(), math.NaN(), math.NaN()}
	}
	return BollingerResult{Upper: mid + k*sd, Middle: mid, Lower: mid - k*sd}
}

// ATR is the average true range over n periods, computed from OHLC triples.
func ATR(high, low, close []float64, n int) float64 {
	if n <= 0 || len(close) < n+1 || len(high) != len(close) || len(low) != len(close) {
		return math.NaN()
	}
	trs := make([]float64, 0, len(close)-1)
	for i := 1; i < len(close); i++ {
		tr := math.Max(high[i]-low[i],
			math.Max(math.Abs(high[i]-close[i-1]), math.Abs(low[i]-close[i-1])))
		trs = append(trs, tr)
	}
	if len(trs) < n {
		return math.NaN()
	}
	// Wilder smoothing.
	atr := SMA(trs[:n], n)
	for _, tr := range trs[n:] {
		atr = (atr*float64(n-1) + tr) / float64(n)
	}
	return atr
}

// Highest and Lowest return extremes over the last n values.
func Highest(v []float64, n int) float64 {
	if n <= 0 || len(v) < 1 {
		return math.NaN()
	}
	if n > len(v) {
		n = len(v)
	}
	m := v[len(v)-n]
	for _, x := range v[len(v)-n:] {
		if x > m {
			m = x
		}
	}
	return m
}

func Lowest(v []float64, n int) float64 {
	if n <= 0 || len(v) < 1 {
		return math.NaN()
	}
	if n > len(v) {
		n = len(v)
	}
	m := v[len(v)-n]
	for _, x := range v[len(v)-n:] {
		if x < m {
			m = x
		}
	}
	return m
}

// Momentum is the simple return over the last n bars.
func Momentum(v []float64, n int) float64 {
	if n <= 0 || len(v) < n+1 {
		return math.NaN()
	}
	past := v[len(v)-1-n]
	if past == 0 {
		return math.NaN()
	}
	return v[len(v)-1]/past - 1
}

// ZScore measures how many standard deviations the latest value sits from the
// n-period mean.
func ZScore(v []float64, n int) float64 {
	mean := SMA(v, n)
	sd := Stdev(v, n)
	if math.IsNaN(mean) || math.IsNaN(sd) || sd == 0 {
		return math.NaN()
	}
	return (v[len(v)-1] - mean) / sd
}

// Returns converts a price series to simple period returns.
func Returns(v []float64) []float64 {
	if len(v) < 2 {
		return nil
	}
	out := make([]float64, 0, len(v)-1)
	for i := 1; i < len(v); i++ {
		if v[i-1] == 0 {
			out = append(out, 0)
			continue
		}
		out = append(out, v[i]/v[i-1]-1)
	}
	return out
}

// Volatility is the annualised standard deviation of daily returns over the
// last n bars.
func Volatility(v []float64, n int, periodsPerYear float64) float64 {
	if len(v) < n+1 {
		return math.NaN()
	}
	rets := Returns(v[len(v)-n-1:])
	sd := Stdev(rets, len(rets))
	if math.IsNaN(sd) {
		return math.NaN()
	}
	return sd * math.Sqrt(periodsPerYear)
}

// Drawdown is the current decline from the running maximum over n bars,
// expressed as a negative fraction.
func Drawdown(v []float64, n int) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	if n > len(v) || n <= 0 {
		n = len(v)
	}
	w := v[len(v)-n:]
	peak := w[0]
	for _, x := range w {
		if x > peak {
			peak = x
		}
	}
	if peak == 0 {
		return math.NaN()
	}
	return w[len(w)-1]/peak - 1
}

// Correlation is the Pearson correlation of two equal-length return series.
func Correlation(a, b []float64) float64 {
	n := len(a)
	if n != len(b) || n < 2 {
		return math.NaN()
	}
	var ma, mb float64
	for i := 0; i < n; i++ {
		ma += a[i]
		mb += b[i]
	}
	ma /= float64(n)
	mb /= float64(n)
	var num, da, db float64
	for i := 0; i < n; i++ {
		x, y := a[i]-ma, b[i]-mb
		num += x * y
		da += x * x
		db += y * y
	}
	if da == 0 || db == 0 {
		return math.NaN()
	}
	return num / math.Sqrt(da*db)
}

// Beta regresses series a on series b (asset on benchmark).
func Beta(a, b []float64) float64 {
	n := len(a)
	if n != len(b) || n < 2 {
		return math.NaN()
	}
	var ma, mb float64
	for i := 0; i < n; i++ {
		ma += a[i]
		mb += b[i]
	}
	ma /= float64(n)
	mb /= float64(n)
	var cov, varb float64
	for i := 0; i < n; i++ {
		x, y := a[i]-ma, b[i]-mb
		cov += x * y
		varb += y * y
	}
	if varb == 0 {
		return math.NaN()
	}
	return cov / varb
}
