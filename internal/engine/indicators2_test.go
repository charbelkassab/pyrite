package engine

import (
	"math"
	"testing"
)

// ramp builds a strictly rising series, which pins several indicators to
// their extreme values and makes off-by-one errors obvious.
func ramp(n int, start, step float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = start + float64(i)*step
	}
	return out
}

func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) {
		t.Fatalf("%s returned NaN, want %v", name, want)
	}
	if math.Abs(got-want) > tol {
		t.Errorf("%s: got %v, want %v (±%v)", name, got, want, tol)
	}
}

func TestWMAWeightsRecentBarsMore(t *testing.T) {
	// (1*1 + 2*2 + 3*3) / 6 = 14/6
	approx(t, "WMA", WMA([]float64{1, 2, 3}, 3), 14.0/6.0, 1e-9)
	// A flat series must return the level itself whatever the weights.
	approx(t, "WMA flat", WMA([]float64{5, 5, 5, 5}, 4), 5, 1e-9)
	if !math.IsNaN(WMA([]float64{1, 2}, 5)) {
		t.Error("WMA should be NaN without enough history")
	}
}

func TestHMATracksARampCloserThanSMA(t *testing.T) {
	v := ramp(60, 100, 1)
	h := HMA(v, 16)
	s := SMA(v, 16)
	last := v[len(v)-1]
	if math.IsNaN(h) {
		t.Fatal("HMA returned NaN with ample history")
	}
	// On a straight line the Hull average sits essentially on the last bar,
	// while an SMA lags by half the window.
	if math.Abs(h-last) >= math.Abs(s-last) {
		t.Errorf("HMA should lag less than SMA: HMA %v, SMA %v, last %v", h, s, last)
	}
	approx(t, "HMA on a ramp", h, last, 1.0)
}

func TestROCIsAFraction(t *testing.T) {
	approx(t, "ROC", ROC([]float64{100, 110}, 1), 0.10, 1e-9)
	approx(t, "ROC over 3", ROC([]float64{100, 0, 0, 150}, 3), 0.50, 1e-9)
	if !math.IsNaN(ROC([]float64{1}, 1)) {
		t.Error("ROC needs n+1 values")
	}
}

func TestTRIXIsZeroOnAFlatSeries(t *testing.T) {
	flat := make([]float64, 100)
	for i := range flat {
		flat[i] = 42
	}
	approx(t, "TRIX flat", TRIX(flat, 9), 0, 1e-9)
	// A rising series must give a positive rate of change.
	if got := TRIX(ramp(100, 100, 1), 9); !(got > 0) {
		t.Errorf("TRIX on a rising series: got %v, want > 0", got)
	}
}

func TestStochasticPinsAtTheExtremes(t *testing.T) {
	// The window's high and low, not the bar's, are what %K measures against.
	// A flat band makes the two coincide so the extremes are unambiguous —
	// on a rising series, closing at today's low is still far above the low
	// of twenty bars ago.
	n := 20
	const hi, lo = 110.0, 100.0
	high := make([]float64, 40)
	low := make([]float64, 40)
	closes := make([]float64, 40)
	for i := range high {
		high[i], low[i] = hi, lo
		closes[i] = hi
	}
	s := Stochastic(high, low, closes, n, 3)
	approx(t, "%K at the high", s.K, 100, 1e-6)

	for i := range closes {
		closes[i] = lo
	}
	s = Stochastic(high, low, closes, n, 3)
	approx(t, "%K at the low", s.K, 0, 1e-6)

	for i := range closes {
		closes[i] = (hi + lo) / 2
	}
	s = Stochastic(high, low, closes, n, 3)
	approx(t, "%K mid-range", s.K, 50, 1e-6)
	approx(t, "%D mid-range", s.D, 50, 1e-6)

	// And on a rising series it should read high but need not pin, which is
	// the behaviour the flat case above deliberately isolates away from.
	rh := ramp(40, 100, 1)
	rl := make([]float64, 40)
	rc := make([]float64, 40)
	for i := range rh {
		rl[i] = rh[i] - 10
		rc[i] = rh[i]
	}
	if got := Stochastic(rh, rl, rc, n, 3).K; got < 90 {
		t.Errorf("%%K on a rising series closing at the high: got %v, want >= 90", got)
	}
}

func TestWilliamsRIsTheInvertedStochastic(t *testing.T) {
	// Same reasoning as the stochastic: measured against the window, so the
	// band has to be flat for the extremes to be exact.
	const hi, lo = 110.0, 100.0
	high := make([]float64, 30)
	low := make([]float64, 30)
	closes := make([]float64, 30)
	for i := range high {
		high[i], low[i] = hi, lo
		closes[i] = hi
	}
	approx(t, "Williams %R at the high", WilliamsR(high, low, closes, 14), 0, 1e-6)
	for i := range closes {
		closes[i] = lo
	}
	approx(t, "Williams %R at the low", WilliamsR(high, low, closes, 14), -100, 1e-6)

	// It is the stochastic on a -100..0 scale, so the two must agree.
	for i := range closes {
		closes[i] = 103
	}
	k := Stochastic(high, low, closes, 14, 1).K
	r := WilliamsR(high, low, closes, 14)
	approx(t, "Williams %R equals %K - 100", r, k-100, 1e-6)
}

func TestCCIIsZeroWhenPriceSitsOnItsMean(t *testing.T) {
	flat := make([]float64, 40)
	for i := range flat {
		flat[i] = 50
	}
	// Zero deviation is the degenerate case and must not divide by zero.
	approx(t, "CCI flat", CCI(flat, flat, flat, 20), 0, 1e-9)

	// A steadily rising series sits above its own average, so CCI is positive.
	h := ramp(60, 100, 1)
	if got := CCI(h, h, h, 20); !(got > 0) {
		t.Errorf("CCI on a rising series: got %v, want > 0", got)
	}
}

func TestADXRisesInATrendAndDirectionsAgree(t *testing.T) {
	n := 40
	high := ramp(n, 100, 2)
	low := make([]float64, n)
	closes := make([]float64, n)
	for i := range high {
		low[i] = high[i] - 1
		closes[i] = high[i] - 0.5
	}
	r := ADX(high, low, closes, 14)
	if math.IsNaN(r.ADX) {
		t.Fatal("ADX returned NaN with ample history")
	}
	if r.ADX < 40 {
		t.Errorf("a clean uptrend should read as a strong trend: ADX %v", r.ADX)
	}
	if !(r.PlusDI > r.MinusDI) {
		t.Errorf("+DI should dominate in an uptrend: +DI %v, -DI %v", r.PlusDI, r.MinusDI)
	}

	// Mirror it: a downtrend must flip the directional components.
	for i := range high {
		high[i] = 200 - float64(i)*2
		low[i] = high[i] - 1
		closes[i] = high[i] - 0.5
	}
	r = ADX(high, low, closes, 14)
	if !(r.MinusDI > r.PlusDI) {
		t.Errorf("-DI should dominate in a downtrend: +DI %v, -DI %v", r.PlusDI, r.MinusDI)
	}
}

func TestOBVSignsVolumeByDirection(t *testing.T) {
	closes := []float64{10, 11, 10, 12}
	vol := []float64{100, 200, 300, 400}
	// +200 (up), -300 (down), +400 (up) = 300
	approx(t, "OBV", OBV(closes, vol), 300, 1e-9)
	// An unchanged close contributes nothing.
	approx(t, "OBV flat bar", OBV([]float64{10, 10}, []float64{100, 500}), 0, 1e-9)
}

func TestMFIBoundsAndDirection(t *testing.T) {
	n := 30
	h := ramp(n, 100, 1)
	vol := make([]float64, n)
	for i := range vol {
		vol[i] = 1000
	}
	// Every bar up: all flow is positive, so MFI pins at 100.
	approx(t, "MFI rising", MFI(h, h, h, vol, 14), 100, 1e-6)

	for i := range h {
		h[i] = 200 - float64(i)
	}
	approx(t, "MFI falling", MFI(h, h, h, vol, 14), 0, 1e-6)
}

func TestVWAPSitsInsideTheRange(t *testing.T) {
	h := []float64{10, 12, 14, 16}
	l := []float64{8, 10, 12, 14}
	c := []float64{9, 11, 13, 15}
	v := []float64{100, 100, 100, 700}
	got := VWAP(h, l, c, v, 4)
	if got < 9 || got > 15 {
		t.Errorf("VWAP outside the price range: %v", got)
	}
	// Weighted heavily to the last bar, it must sit near that bar's typical
	// price of 15.
	if got < 13 {
		t.Errorf("VWAP ignored the volume weighting: %v", got)
	}
	if !math.IsNaN(VWAP(h, l, c, []float64{0, 0, 0, 0}, 4)) {
		t.Error("zero volume should be undefined, not a division by zero")
	}
}

func TestCMFSignsWithCloseLocation(t *testing.T) {
	n := 25
	h := make([]float64, n)
	l := make([]float64, n)
	c := make([]float64, n)
	v := make([]float64, n)
	for i := 0; i < n; i++ {
		h[i], l[i], v[i] = 110, 100, 1000
		c[i] = 110 // closing at the high every bar
	}
	approx(t, "CMF at the high", CMF(h, l, c, v, 20), 1, 1e-6)
	for i := range c {
		c[i] = 100
	}
	approx(t, "CMF at the low", CMF(h, l, c, v, 20), -1, 1e-6)
}

func TestDonchianIsTheWindowExtremes(t *testing.T) {
	h := []float64{10, 20, 15, 12}
	l := []float64{5, 8, 6, 9}
	d := Donchian(h, l, 4)
	approx(t, "Donchian upper", d.Upper, 20, 1e-9)
	approx(t, "Donchian lower", d.Lower, 5, 1e-9)
	approx(t, "Donchian middle", d.Middle, 12.5, 1e-9)
}

func TestKeltnerWidensWithRange(t *testing.T) {
	n := 60
	c := ramp(n, 100, 0.5)
	tight := make([]float64, n)
	wide := make([]float64, n)
	for i := range c {
		tight[i] = c[i]
		wide[i] = c[i]
	}
	narrowH, narrowL := make([]float64, n), make([]float64, n)
	wideH, wideL := make([]float64, n), make([]float64, n)
	for i := range c {
		narrowH[i], narrowL[i] = c[i]+1, c[i]-1
		wideH[i], wideL[i] = c[i]+10, c[i]-10
	}
	a := Keltner(narrowH, narrowL, c, 20, 2)
	b := Keltner(wideH, wideL, c, 20, 2)
	if !((b.Upper - b.Lower) > (a.Upper - a.Lower)) {
		t.Errorf("wider bars should widen the channel: %v vs %v",
			b.Upper-b.Lower, a.Upper-a.Lower)
	}
	if !(a.Upper > a.Middle && a.Middle > a.Lower) {
		t.Errorf("channel is not ordered: %+v", a)
	}
}

func TestSuperTrendFollowsTheTrend(t *testing.T) {
	n := 120
	h, l, c := make([]float64, n), make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		base := 100 + float64(i)
		h[i], l[i], c[i] = base+1, base-1, base
	}
	up := SuperTrend(h, l, c, 10, 3)
	if up.Trend != 1 {
		t.Errorf("a steady uptrend should read +1, got %d", up.Trend)
	}
	if !(up.Value < c[n-1]) {
		t.Errorf("in an uptrend the stop sits below price: %v vs %v", up.Value, c[n-1])
	}

	for i := 0; i < n; i++ {
		base := 300 - float64(i)
		h[i], l[i], c[i] = base+1, base-1, base
	}
	down := SuperTrend(h, l, c, 10, 3)
	if down.Trend != -1 {
		t.Errorf("a steady downtrend should read -1, got %d", down.Trend)
	}
	if !(down.Value > c[n-1]) {
		t.Errorf("in a downtrend the stop sits above price: %v vs %v", down.Value, c[n-1])
	}
}

func TestAroonPinsWhenExtremesAreFresh(t *testing.T) {
	n := 30
	h := ramp(n, 100, 1) // every bar a new high
	l := ramp(n, 90, 1)
	a := Aroon(h, l, 25)
	approx(t, "Aroon up", a.Up, 100, 1e-9)
	// The lowest low is the oldest bar in the window.
	approx(t, "Aroon down", a.Down, 0, 1e-9)
	approx(t, "Aroon oscillator", a.Oscillator, 100, 1e-9)
}

func TestPSARTrailsBelowInAnUptrend(t *testing.T) {
	n := 60
	h, l := make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		h[i] = 100 + float64(i)*2
		l[i] = h[i] - 2
	}
	sar := PSAR(h, l, 0.02, 0.2)
	if math.IsNaN(sar) {
		t.Fatal("PSAR returned NaN")
	}
	if !(sar < l[n-1]) {
		t.Errorf("in an uptrend the stop trails below the low: %v vs %v", sar, l[n-1])
	}

	for i := 0; i < n; i++ {
		h[i] = 300 - float64(i)*2
		l[i] = h[i] - 2
	}
	sar = PSAR(h, l, 0.02, 0.2)
	if !(sar > h[n-1]) {
		t.Errorf("in a downtrend the stop trails above the high: %v vs %v", sar, h[n-1])
	}
}

func TestIchimokuUsesMidpoints(t *testing.T) {
	n := 60
	h := ramp(n, 100, 1)
	l := ramp(n, 90, 1)
	r := Ichimoku(h, l, 9, 26, 52)
	if math.IsNaN(r.Conversion) || math.IsNaN(r.SpanB) {
		t.Fatalf("Ichimoku returned NaN with ample history: %+v", r)
	}
	// Conversion uses a shorter window than base, so on a rising series it
	// sits above it.
	if !(r.Conversion > r.Base) {
		t.Errorf("conversion should lead base in an uptrend: %v vs %v", r.Conversion, r.Base)
	}
	approx(t, "span A is the midpoint of the two lines", r.SpanA, (r.Conversion+r.Base)/2, 1e-9)
}

func TestChoppinessSeparatesTrendFromRange(t *testing.T) {
	n := 60
	th, tl, tc := make([]float64, n), make([]float64, n), make([]float64, n)
	rh, rl, rc := make([]float64, n), make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		b := 100 + float64(i)*3
		th[i], tl[i], tc[i] = b+1, b-1, b
		// A range: the same band revisited every bar.
		osc := 100.0
		if i%2 == 0 {
			osc = 110
		}
		rh[i], rl[i], rc[i] = 111, 99, osc
	}
	trend := Choppiness(th, tl, tc, 14)
	rangey := Choppiness(rh, rl, rc, 14)
	if math.IsNaN(trend) || math.IsNaN(rangey) {
		t.Fatalf("choppiness returned NaN: trend %v range %v", trend, rangey)
	}
	if !(rangey > trend) {
		t.Errorf("a range should read choppier than a trend: %v vs %v", rangey, trend)
	}
}

func TestLinRegRecoversAKnownLine(t *testing.T) {
	// y = 3x + 7 exactly.
	v := make([]float64, 20)
	for i := range v {
		v[i] = 3*float64(i) + 7
	}
	r := LinReg(v, 20)
	approx(t, "slope", r.Slope, 3, 1e-9)
	approx(t, "intercept", r.Intercept, 7, 1e-9)
	approx(t, "R²", r.R2, 1, 1e-9)
	approx(t, "forecast", r.Forecast, 3*19+7, 1e-9)

	// Pure noise around a flat level should explain almost nothing.
	flat := []float64{5, 5, 5, 5, 5, 5}
	if got := LinReg(flat, 6); !math.IsNaN(got.Slope) && math.Abs(got.Slope) > 1e-9 {
		t.Errorf("a flat series should have zero slope: %v", got.Slope)
	}
}

func TestIndicatorsRefuseShortHistory(t *testing.T) {
	short := []float64{1, 2, 3}
	cases := map[string]float64{
		"WMA":        WMA(short, 10),
		"HMA":        HMA(short, 16),
		"ROC":        ROC(short, 10),
		"TRIX":       TRIX(short, 9),
		"CCI":        CCI(short, short, short, 20),
		"WilliamsR":  WilliamsR(short, short, short, 14),
		"MFI":        MFI(short, short, short, short, 14),
		"VWAP":       VWAP(short, short, short, short, 20),
		"CMF":        CMF(short, short, short, short, 20),
		"Choppiness": Choppiness(short, short, short, 14),
		"PSAR":       PSAR(short, short, 0.02, 0.2),
	}
	for name, v := range cases {
		if !math.IsNaN(v) {
			t.Errorf("%s should be NaN without enough history, got %v", name, v)
		}
	}
	if !math.IsNaN(ADX(short, short, short, 14).ADX) {
		t.Error("ADX should be NaN without enough history")
	}
	if !math.IsNaN(Stochastic(short, short, short, 20, 3).K) {
		t.Error("Stochastic should be NaN without enough history")
	}
	if !math.IsNaN(LinReg(short, 20).Slope) {
		t.Error("LinReg should be NaN without enough history")
	}
	if !math.IsNaN(SuperTrend(short, short, short, 10, 3).Value) {
		t.Error("SuperTrend should be NaN without enough history")
	}
	if !math.IsNaN(Aroon(short, short, 25).Up) {
		t.Error("Aroon should be NaN without enough history")
	}
	if !math.IsNaN(Ichimoku(short, short, 9, 26, 52).SpanB) {
		t.Error("Ichimoku should be NaN without enough history")
	}
}
