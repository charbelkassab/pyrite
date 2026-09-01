package engine

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"

	"github.com/charbelkassab/pyrite/internal/market"
)

// This is a second renderer over the same Report the Markdown writer uses. It
// lives in this package rather than its own because everything it needs —
// the struct, the formatters, the Ratio guard — is already here, and moving it
// out would mean exporting a dozen formatting helpers that nothing else has
// any business calling.
//
// The document it produces is one file with no network dependency of any
// kind: no fonts, no scripts, no charting library, no images. The charts are
// SVG generated below. That is the whole point — a report somebody can attach
// to an email and still open in five years.

// HTML renders the report as a single self-contained document.
func (r *Report) HTML() (string, error) {
	var b strings.Builder
	if err := reportPage.Execute(&b, r.htmlModel()); err != nil {
		return "", fmt.Errorf("render html report: %w", err)
	}
	return b.String(), nil
}

// ── formatting ──────────────────────────────────────────────────────────────

// finite reports whether a float can be put on the page or into a coordinate.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// pctSafe, numSafe and moneySafe are the guarded forms of the Markdown
// writer's formatters.
//
// That document is assembled from fields the engine has already checked. This
// one reaches further into the result — every risk statistic, every symbol
// row — and one NaN printed as "NaN%" beside real figures does more damage
// than admitting the number is not defined.
func pctSafe(v float64) string {
	if !finite(v) {
		return "n/a"
	}
	return pctText(v)
}

func numSafe(v float64, dp int) string {
	if !finite(v) {
		return "n/a"
	}
	return strconv.FormatFloat(v, 'f', dp, 64)
}

func moneySafe(v float64) string {
	if !finite(v) {
		return "n/a"
	}
	return money(v)
}

// pctTick and moneyTick are the axis forms of the two formatters above.
//
// Full precision belongs in the tables. On a gridline, "$150.0k" and "0.00%"
// are three characters of noise per label repeated down the side of every
// chart, and the reader is reading the line, not the label.
func pctTick(v float64) string {
	if !finite(v) {
		return ""
	}
	p := v * 100
	if math.Abs(p-math.Round(p)) < 0.05 {
		return strconv.FormatFloat(math.Round(p), 'f', 0, 64) + "%"
	}
	return strconv.FormatFloat(p, 'f', 1, 64) + "%"
}

func moneyTick(v float64) string {
	if !finite(v) {
		return ""
	}
	return strings.Replace(money(v), ".0", "", 1)
}

// dayText renders a session date the way a reader would write it.
func dayText(d market.Day) string {
	t := d.Time()
	if d == "" || t.IsZero() {
		return string(d)
	}
	return t.Format("2 January 2006")
}

// ── charts ──────────────────────────────────────────────────────────────────

// Charts are built as strings, not floats, and the template does nothing but
// copy them into attributes. A NaN coordinate does not raise an error in a
// browser: it makes the path disappear, and a blank chart looks deliberate.
// Formatting here means every coordinate is checked once, in Go, where it can
// be tested.

const (
	chartW      = 900.0
	chartGutL   = 66.0
	chartGutR   = 14.0
	equityH     = 300.0
	drawdownH   = 150.0
	yearH       = 260.0
	chartGutT   = 14.0
	xLabelSpace = 30.0
)

// chartTick is one gridline and its label, both already positioned.
type chartTick struct {
	Line  string
	Text  string
	Label string
}

// chartSeries is one plotted series. Stroke and Fill are class names, so the
// colours live in the stylesheet and follow the reader's colour scheme.
type chartSeries struct {
	Stroke string
	Fill   string
	Line   string
	Area   string
}

// chartBar is one rectangle in a bar chart.
type chartBar struct {
	Class      string
	X, Y, W, H string
}

// legendKey names a series in the chart's key.
type legendKey struct {
	Swatch string
	Label  string
}

// lineChart is a finished line or area chart.
type lineChart struct {
	Title          string
	Frame          string
	ViewBox        string
	X0, X1, Y0, Y1 string
	YLabelX        string
	XLabelY        string
	ShowX          bool
	Zero           string
	YTicks         []chartTick
	XTicks         []chartTick
	Series         []chartSeries
}

// barChart is the calendar-year view.
type barChart struct {
	Title          string
	ViewBox        string
	X0, X1, Y0, Y1 string
	YLabelX        string
	XLabelY        string
	Zero           string
	YTicks         []chartTick
	XTicks         []chartTick
	Bars           []chartBar
}

// axis maps a data range onto a pixel range.
//
// The degenerate case is the one worth writing down: a flat equity curve, a
// benchmark that never moved, a single calendar year. Dividing by a zero span
// gives NaN, so a flat span puts the whole series down the middle of the plot
// instead, which is both finite and true.
type axis struct{ lo, hi, p0, p1 float64 }

func newAxis(lo, hi, p0, p1 float64) axis {
	if !finite(lo) || !finite(hi) {
		lo, hi = 0, 1
	}
	return axis{lo: lo, hi: hi, p0: p0, p1: p1}
}

func (a axis) at(v float64) float64 {
	if !finite(v) {
		return math.NaN()
	}
	span := a.hi - a.lo
	if span <= 0 {
		return (a.p0 + a.p1) / 2
	}
	return a.p0 + (v-a.lo)/span*(a.p1-a.p0)
}

// pos formats a coordinate for an attribute, or returns "" when it has none.
func pos(v float64) string {
	if !finite(v) {
		return ""
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// padRange widens a data range so the curve does not touch the frame, and
// gives a flat series something to sit in the middle of.
func padRange(lo, hi float64) (float64, float64) {
	if !finite(lo) || !finite(hi) {
		return 0, 1
	}
	span := hi - lo
	if span <= 0 {
		if lo == 0 {
			return -1, 1
		}
		m := math.Abs(lo) * 0.1
		return lo - m, hi + m
	}
	return lo - span*0.06, hi + span*0.06
}

// niceTicks picks round values covering a range.
func niceTicks(lo, hi float64, want int) []float64 {
	if !finite(lo) || !finite(hi) || want < 2 || hi <= lo {
		return nil
	}
	raw := (hi - lo) / float64(want-1)
	if !(raw > 0) {
		return nil
	}
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	step := mag
	switch n := raw / mag; {
	case n > 5:
		step = 10 * mag
	case n > 2:
		step = 5 * mag
	case n > 1:
		step = 2 * mag
	}
	if !(step > 0) || !finite(step) {
		return nil
	}
	first := math.Ceil(lo/step) * step
	out := make([]float64, 0, want+2)
	for i := 0; i < 64; i++ {
		v := first + float64(i)*step
		if v > hi+step*1e-6 {
			break
		}
		// Floating-point accumulation turns an exact zero into -1e-17, which
		// prints as "-0.00%".
		if math.Abs(v) < step*1e-9 {
			v = 0
		}
		out = append(out, v)
	}
	return out
}

// chartPoint is one plotted position, already in SVG user units.
type chartPoint struct{ X, Y float64 }

// pathOf joins plotted points into path data, breaking the line wherever a
// point could not be placed rather than emitting a coordinate that would make
// the browser drop the whole path.
func pathOf(pts []chartPoint) string {
	var b strings.Builder
	pen := "M"
	for _, p := range pts {
		x, y := pos(p.X), pos(p.Y)
		if x == "" || y == "" {
			pen = "M"
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(pen)
		b.WriteString(x)
		b.WriteByte(',')
		b.WriteString(y)
		pen = "L"
	}
	return b.String()
}

// areaOf closes a line down to a baseline so it can be filled.
func areaOf(pts []chartPoint, base float64) string {
	line := pathOf(pts)
	if line == "" || !finite(base) {
		return ""
	}
	var first, last chartPoint
	found := false
	for _, p := range pts {
		if !finite(p.X) || !finite(p.Y) {
			continue
		}
		if !found {
			first, found = p, true
		}
		last = p
	}
	if !found {
		return ""
	}
	return fmt.Sprintf("M%s,%s L%s L%s,%s Z",
		pos(first.X), pos(base), strings.TrimPrefix(line, "M"), pos(last.X), pos(base))
}

// thin keeps at most one high and one low per column of the plot.
//
// Nine years of daily bars is more points than the chart is wide. Dropping
// every other one would erase the single-session spikes a drawdown chart
// exists to show; keeping each column's extremes keeps the shape, and keeps
// the file a tenth of the size.
func thin(pts []chartPoint, cols int) []chartPoint {
	if cols < 2 || len(pts) <= cols*2 {
		return pts
	}
	x0, x1 := pts[0].X, pts[len(pts)-1].X
	w := (x1 - x0) / float64(cols)
	if !finite(w) || w <= 0 {
		return pts
	}
	out := make([]chartPoint, 0, cols*2+2)
	bucket, lo, hi := 0, -1, -1
	flush := func() {
		if lo < 0 {
			return
		}
		a, b := lo, hi
		if a > b {
			a, b = b, a
		}
		out = append(out, pts[a])
		if b != a {
			out = append(out, pts[b])
		}
	}
	for i, p := range pts {
		if !finite(p.X) || !finite(p.Y) {
			continue
		}
		k := int((p.X - x0) / w)
		if lo >= 0 && k != bucket {
			flush()
			bucket, lo, hi = k, i, i
			continue
		}
		bucket = k
		if lo < 0 || p.Y < pts[lo].Y {
			lo = i
		}
		if hi < 0 || p.Y > pts[hi].Y {
			hi = i
		}
	}
	flush()
	if len(out) == 0 {
		return pts
	}
	// The extremes of the first and last columns are rarely the endpoints, so
	// without this the line stops short of both frames.
	if out[0] != pts[0] && finite(pts[0].X) {
		out = append([]chartPoint{pts[0]}, out...)
	}
	if end := pts[len(pts)-1]; out[len(out)-1] != end && finite(end.X) {
		out = append(out, end)
	}
	return out
}

// timeTicks labels the x axis at calendar boundaries.
//
// Years first, because that is what a reader wants from a multi-year curve;
// months only when the run is too short to have crossed two new years.
func timeTicks(days []market.Day, ax axis, max int) []chartTick {
	if len(days) < 2 || max < 2 {
		return nil
	}
	pick := func(unit, label string) []chartTick {
		var out []chartTick
		prev := days[0].Time().Format(unit)
		for _, d := range days[1:] {
			t := d.Time()
			u := t.Format(unit)
			if u == prev {
				continue
			}
			prev = u
			p := ax.at(float64(t.Unix()))
			s := pos(p)
			if s == "" {
				continue
			}
			out = append(out, chartTick{Line: s, Text: s, Label: t.Format(label)})
		}
		return out
	}
	ticks := pick("2006", "2006")
	if len(ticks) < 2 {
		ticks = pick("2006-01", "Jan 2006")
	}
	if len(ticks) == 0 {
		return nil
	}
	// Keeping every kth from the start leaves an even spacing; dropping from
	// the end would crowd the left of the axis and empty the right.
	if k := (len(ticks) + max - 1) / max; k > 1 {
		kept := ticks[:0:0]
		for i := 0; i < len(ticks); i += k {
			kept = append(kept, ticks[i])
		}
		ticks = kept
	}
	return ticks
}

// seriesClass is the fixed order chart colours are handed out in, so a report
// with one benchmark and a report with two do not disagree about which line is
// the strategy.
var seriesClass = []string{"s1", "s2", "s3"}

// equityCharts builds the equity curve and the drawdown beneath it.
//
// They share an x axis — the same domain and the same left and right gutters —
// so a trough in one sits directly under the same session in the other. That
// is the only reason the two are built together rather than separately.
func (r *Report) equityCharts() (*lineChart, *lineChart, []legendKey) {
	if r.Run == nil || len(r.Run.Curve) < 2 {
		return nil, nil, nil
	}
	type input struct {
		label string
		curve []EquityPoint
	}
	inputs := []input{{label: "Strategy", curve: r.Run.Curve}}
	for _, bm := range r.Run.Benchmarks {
		if len(inputs) >= len(seriesClass) || len(bm.Curve) < 2 {
			continue
		}
		inputs = append(inputs, input{label: bm.Label, curve: bm.Curve})
	}

	xLo, xHi := math.Inf(1), math.Inf(-1)
	vLo, vHi := math.Inf(1), math.Inf(-1)
	dLo := 0.0
	for _, in := range inputs {
		for _, p := range in.curve {
			x := float64(p.Date.Time().Unix())
			xLo, xHi = math.Min(xLo, x), math.Max(xHi, x)
			if finite(p.Value) {
				vLo, vHi = math.Min(vLo, p.Value), math.Max(vHi, p.Value)
			}
			if finite(p.Drawdown) {
				dLo = math.Min(dLo, p.Drawdown)
			}
		}
	}
	if !finite(xLo) || !finite(xHi) {
		return nil, nil, nil
	}
	vLo, vHi = padRange(vLo, vHi)

	left, right := chartGutL, chartW-chartGutR
	xa := newAxis(xLo, xHi, left, right)

	// Equity.
	eqTop, eqBot := chartGutT, equityH-12
	ya := newAxis(vLo, vHi, eqBot, eqTop)
	eq := &lineChart{
		Title:   "Equity curve",
		Frame:   "chart-top",
		ViewBox: fmt.Sprintf("0 0 %.0f %.0f", chartW, equityH),
		X0:      pos(left), X1: pos(right), Y0: pos(eqTop), Y1: pos(eqBot),
		YLabelX: pos(left - 10),
	}
	for _, v := range niceTicks(vLo, vHi, 6) {
		y := ya.at(v)
		eq.YTicks = append(eq.YTicks, chartTick{
			Line: pos(y), Text: pos(y + 3.6), Label: moneyTick(v)})
	}
	eq.XTicks = timeTicks(dateSpan(inputs[0].curve), xa, 10)

	// Drawdown.
	ddTop, ddBot := chartGutT, drawdownH-xLabelSpace
	dda := newAxis(dLo*1.08, 0, ddBot, ddTop)
	dd := &lineChart{
		Title:   "Drawdown from the running peak",
		Frame:   "chart-bottom",
		ViewBox: fmt.Sprintf("0 0 %.0f %.0f", chartW, drawdownH),
		X0:      pos(left), X1: pos(right), Y0: pos(ddTop), Y1: pos(ddBot),
		YLabelX: pos(left - 10), XLabelY: pos(ddBot + 18), ShowX: true,
		Zero: pos(dda.at(0)),
	}
	for _, v := range niceTicks(dLo*1.08, 0, 5) {
		y := dda.at(v)
		dd.YTicks = append(dd.YTicks, chartTick{
			Line: pos(y), Text: pos(y + 3.6), Label: pctTick(v)})
	}
	dd.XTicks = eq.XTicks

	var legend []legendKey
	for i, in := range inputs {
		class := seriesClass[i]
		ePts := make([]chartPoint, 0, len(in.curve))
		dPts := make([]chartPoint, 0, len(in.curve))
		for _, p := range in.curve {
			x := xa.at(float64(p.Date.Time().Unix()))
			ePts = append(ePts, chartPoint{X: x, Y: ya.at(p.Value)})
			dPts = append(dPts, chartPoint{X: x, Y: dda.at(p.Drawdown)})
		}
		eq.Series = append(eq.Series, chartSeries{
			Stroke: class, Line: pathOf(thin(ePts, 300))})

		dThin := thin(dPts, 300)
		ds := chartSeries{Stroke: class, Line: pathOf(dThin)}
		// Only the strategy's drawdown is filled. Two overlapping translucent
		// areas read as a third colour that means nothing.
		if i == 0 {
			ds.Fill = "a" + class[1:]
			ds.Area = areaOf(dThin, dda.at(0))
		}
		dd.Series = append(dd.Series, ds)
		legend = append(legend, legendKey{Swatch: "sw" + class[1:], Label: in.label})
	}
	return eq, dd, legend
}

// dateSpan pulls the dates out of a curve for the axis labeller.
func dateSpan(curve []EquityPoint) []market.Day {
	out := make([]market.Day, len(curve))
	for i, p := range curve {
		out[i] = p.Date
	}
	return out
}

// yearChart draws the calendar years side by side with the benchmark.
func (r *Report) yearChart() (*barChart, []legendKey) {
	if r.Run == nil {
		return nil, nil
	}
	years := r.Run.Attribution.ByYear
	if len(years) < 2 {
		return nil, nil
	}
	hasBench := false
	lo, hi := 0.0, 0.0
	for _, y := range years {
		if finite(y.Return) {
			lo, hi = math.Min(lo, y.Return), math.Max(hi, y.Return)
		}
		if y.BenchmarkReturn != 0 && finite(y.BenchmarkReturn) {
			hasBench = true
			lo, hi = math.Min(lo, y.BenchmarkReturn), math.Max(hi, y.BenchmarkReturn)
		}
	}
	lo, hi = padRange(lo, hi)

	left, right := chartGutL, chartW-chartGutR
	top, bottom := chartGutT, yearH-xLabelSpace
	ya := newAxis(lo, hi, bottom, top)
	zero := ya.at(0)

	c := &barChart{
		Title:   "Return by calendar year",
		ViewBox: fmt.Sprintf("0 0 %.0f %.0f", chartW, yearH),
		X0:      pos(left), X1: pos(right), Y0: pos(top), Y1: pos(bottom),
		YLabelX: pos(left - 10), XLabelY: pos(bottom + 18), Zero: pos(zero),
	}
	for _, v := range niceTicks(lo, hi, 7) {
		y := ya.at(v)
		c.YTicks = append(c.YTicks, chartTick{
			Line: pos(y), Text: pos(y + 3.6), Label: pctTick(v)})
	}

	group := (right - left) / float64(len(years))
	bw := group * 0.52
	if hasBench {
		bw = group * 0.30
	}
	gap := group * 0.06
	labelEvery := (len(years) + 13) / 14
	if labelEvery < 1 {
		labelEvery = 1
	}
	for i, y := range years {
		centre := left + (float64(i)+0.5)*group
		vals := []struct {
			v     float64
			class string
		}{{y.Return, "f1"}}
		if hasBench {
			vals = append(vals, struct {
				v     float64
				class string
			}{y.BenchmarkReturn, "f2"})
		}
		span := float64(len(vals))*bw + float64(len(vals)-1)*gap
		x := centre - span/2
		for _, val := range vals {
			if yv := ya.at(val.v); finite(yv) && finite(zero) {
				c.Bars = append(c.Bars, chartBar{
					Class: val.class,
					X:     pos(x), Y: pos(math.Min(yv, zero)),
					W: pos(bw), H: pos(math.Max(math.Abs(yv-zero), 0.6)),
				})
			}
			x += bw + gap
		}
		if i%labelEvery == 0 {
			c.XTicks = append(c.XTicks, chartTick{Text: pos(centre), Label: y.Label})
		}
	}

	legend := []legendKey{{Swatch: "sw1", Label: "Strategy"}}
	if hasBench {
		legend = append(legend, legendKey{Swatch: "sw2", Label: benchLabel(r.Run)})
	}
	return c, legend
}

// benchLabel names the benchmark the year table compares against.
func benchLabel(run *Result) string {
	if run != nil && len(run.Benchmarks) > 0 {
		return run.Benchmarks[0].Label
	}
	return "Benchmark"
}

// ── the document model ──────────────────────────────────────────────────────

// cell is one table cell: its text plus the class that aligns and colours it.
type cell struct {
	Text  string
	Class string
}

func txt(s string) cell         { return cell{Text: s} }
func num(s string) cell         { return cell{Text: s, Class: "num"} }
func head(s, class string) cell { return cell{Text: s, Class: class} }
func rowHead(s string) cell     { return cell{Text: s, Class: "rowhead"} }
func strongCell(s string) cell  { return cell{Text: s, Class: "num total"} }
func signed(v float64, s string) cell {
	switch {
	case !finite(v) || v == 0:
		return num(s)
	case v > 0:
		return cell{Text: s, Class: "num pos"}
	default:
		return cell{Text: s, Class: "num neg"}
	}
}

// table is every tabular block on the page. One shape means one template.
type table struct {
	Head []cell
	Rows [][]cell
}

// labelled is a term and its value, for the verdict list and the short lists
// that are not worth a table.
type labelled struct {
	Label string
	Text  string
}

// htmlFinding is one critique finding, with its severity resolved to a class
// name here rather than interpolated into the page.
//
// Severity is engine-owned and can only be one of three constants, but a
// class attribute assembled from a value that travels through JSON is exactly
// the kind of thing that stops being true later.
type htmlFinding struct {
	Class  string
	Label  string
	Title  string
	Detail string
}

// navItem links to a section that this particular report actually has.
type navItem struct {
	ID    string
	Label string
}

// textRun is a span of narrative prose.
type textRun struct {
	Text string
	Bold bool
	Code bool
}

// narrBlock is one paragraph or one bullet list of the model's summary.
type narrBlock struct {
	List  bool
	Items [][]textRun
}

// htmlDoc is the whole page, with every number already formatted. The
// template branches on presence and copies strings; it does no arithmetic and
// makes no decisions.
type htmlDoc struct {
	Title  string
	Prompt string
	Meta   string
	Nav    []navItem

	HasRun     bool
	TrustScore int
	ScoreTone  string
	ScoreWidth string
	Headline   string
	Verdicts   []labelled
	Narrative  []narrBlock

	Findings []htmlFinding

	Period       string
	Results      *table
	Distribution string
	Trades       string
	Equity       *lineChart
	Drawdown     *lineChart
	EquityKey    []legendKey
	EquityNote   string

	OOSIntro    string
	OutOfSample *table

	SearchIntro string
	Search      *table

	YearBars   *barChart
	YearKey    []legendKey
	Years      *table
	Regimes    *table
	StressLead string
	Stress     []string
	Symbols    *table

	Costs    *table
	CostNote string

	FactorIntro   string
	Factors       *table
	FactorNote    string
	FactorDropped []string
	FactorVerdict string

	BootIntro string
	Boot      *table
	BootNote  string

	Mechanics   *table
	Assumptions []string
	Limitations []string
	Closing     string

	Code string
}

func (r *Report) htmlModel() *htmlDoc {
	d := &htmlDoc{
		Title:  r.Title,
		Prompt: r.Prompt,
		Meta: fmt.Sprintf("Generated %s by pyrite.",
			r.Generated.UTC().Format("2 January 2006")),
		Narrative: narrativeBlocks(r.Narrative),
	}
	if d.Title == "" {
		d.Title = "Strategy report"
	}
	if r.Run == nil {
		return d
	}
	d.HasRun = true

	r.buildVerdict(d)
	r.buildResults(d)
	r.buildOutOfSample(d)
	r.buildSearch(d)
	r.buildAttribution(d)
	r.buildCosts(d)
	r.buildFactors(d)
	r.buildBootstrap(d)
	r.buildMechanics(d)
	d.Code = strings.TrimSpace(r.Run.Spec.Code)
	d.Nav = buildNav(d)
	return d
}

// buildVerdict leads with the conclusion and the objections behind it.
//
// The Markdown document puts the findings last, where a reader arrives at
// them having already absorbed the numbers. On a page that is scrolled rather
// than read straight through that ordering buries them, so here they sit
// immediately under the score they produced.
func (r *Report) buildVerdict(d *htmlDoc) {
	c := r.Run.Critique
	d.TrustScore = c.TrustScore
	d.Headline = c.Headline
	d.ScoreWidth = numSafe(float64(c.TrustScore)*2, 1)
	switch {
	case c.TrustScore >= 70:
		d.ScoreTone = "tone-good"
	case c.TrustScore >= 40:
		d.ScoreTone = "tone-warn"
	default:
		d.ScoreTone = "tone-bad"
	}

	add := func(label, text string) {
		if text != "" {
			d.Verdicts = append(d.Verdicts, labelled{Label: label, Text: upperFirst(text) + "."})
		}
	}
	if r.WalkForward != nil {
		add("Out of sample", r.WalkForward.Verdict)
	}
	if r.Sweep != nil {
		add("Across the parameter space", r.Sweep.Robustness.Verdict)
	}
	if r.Costs != nil {
		add("On costs", r.Costs.Verdict)
	}
	if r.Factors != nil {
		add("Against known factors", r.Factors.Verdict)
	}

	for _, f := range c.Findings {
		class := "sev-note"
		switch f.Severity {
		case SeverityCritical:
			class = "sev-critical"
		case SeverityWarning:
			class = "sev-warning"
		}
		d.Findings = append(d.Findings, htmlFinding{
			Class:  class,
			Label:  strings.ToUpper(string(f.Severity)),
			Title:  f.Title,
			Detail: f.Detail,
		})
	}
}

func (r *Report) buildResults(d *htmlDoc) {
	run := r.Run
	m := run.Metrics
	d.Period = fmt.Sprintf("%s to %s", dayText(run.Spec.Start), dayText(run.Spec.End))

	t := &table{Head: []cell{head("", ""), head("Strategy", "num")}}
	for _, bm := range run.Benchmarks {
		t.Head = append(t.Head, head(bm.Label, "num"))
	}
	row := func(label string, get func(Metrics) string) {
		cells := []cell{rowHead(label), num(get(m))}
		for _, bm := range run.Benchmarks {
			cells = append(cells, num(get(bm.Metric)))
		}
		t.Rows = append(t.Rows, cells)
	}
	row("Total return", func(m Metrics) string { return pctSafe(m.TotalReturn) })
	row("Annualised", func(m Metrics) string { return pctSafe(m.CAGR) })
	row("Volatility", func(m Metrics) string { return pctSafe(m.Volatility) })
	row("Sharpe", func(m Metrics) string { return ratioText(m.Sharpe) })
	row("Sortino", func(m Metrics) string { return ratioText(m.Sortino) })
	row("Max drawdown", func(m Metrics) string { return pctSafe(m.MaxDrawdown) })
	d.Results = t

	risk := run.Risk
	d.Distribution = fmt.Sprintf("Return distribution: skew %s, excess kurtosis %s, "+
		"ulcer index %s, daily CVaR at 95%% %s.",
		numSafe(risk.Skew, 2), numSafe(risk.ExcessKurtosis, 1),
		pctSafe(risk.UlcerIndex), pctSafe(risk.CVaR95))

	if ts := run.TradeStats; ts.Closed > 0 {
		d.Trades = fmt.Sprintf("%d closed round trips, %s win rate, payoff ratio %s, "+
			"expectancy %s per trade, average hold %s bars.",
			ts.Closed, pctSafe(ts.WinRate), ratioText(ts.PayoffRatio),
			moneySafe(ts.Expectancy), numSafe(ts.AvgBarsHeld, 0))
	}

	d.Equity, d.Drawdown, d.EquityKey = r.equityCharts()
	if d.Equity != nil {
		d.EquityNote = "Equity above, decline from the running peak below, on the same " +
			"dates. Both are the reported result, before any of the checks that follow."
	}
}

func (r *Report) buildOutOfSample(d *htmlDoc) {
	wf := r.WalkForward
	if wf == nil || len(wf.Folds) == 0 {
		return
	}
	d.OOSIntro = "Parameters were chosen on each training window and applied unchanged " +
		"to the window that followed it. The figures below are the only ones in this " +
		"document that were never fitted to."

	m := wf.StitchedMetrics
	t := &table{Head: []cell{head("", ""), head("Value", "num")}}
	add := func(label, value string) {
		t.Rows = append(t.Rows, []cell{rowHead(label), num(value)})
	}
	add("Out-of-sample total return", pctSafe(m.TotalReturn))
	add("Out-of-sample annualised", pctSafe(m.CAGR))
	add("Out-of-sample Sharpe", ratioText(m.Sharpe))
	add("Out-of-sample max drawdown", pctSafe(m.MaxDrawdown))
	add("Mean in-sample return per fold", pctSafe(wf.InSampleReturn))
	add("Mean out-of-sample return per fold", pctSafe(wf.OutOfSampleMean))
	if wf.Efficiency.Defined() {
		add("Walk-forward efficiency", pctSafe(float64(wf.Efficiency)))
	}
	add("Positive test windows", fmt.Sprintf("%d of %d", wf.ConsistentFolds, len(wf.Folds)))
	add("Parameter stability", pctSafe(wf.ParamStability))
	d.OutOfSample = t
}

func (r *Report) buildSearch(d *htmlDoc) {
	sw := r.Sweep
	if sw == nil {
		return
	}
	rb := sw.Robustness
	d.SearchIntro = fmt.Sprintf("%d configurations were tried. The point of testing "+
		"that many is not to find the best one; it is to find out whether the best "+
		"one means anything.", rb.Trials)

	t := &table{Head: []cell{head("", ""), head("Value", "num"), head("", "")}}
	add := func(label, value, note string) {
		t.Rows = append(t.Rows, []cell{rowHead(label), num(value), cell{Text: note, Class: "wrap"}})
	}
	add("Best "+sw.Objective, numSafe(rb.BestScore, 3), "")
	add("Median "+sw.Objective, numSafe(rb.MedianScore, 3), "")
	if rb.ExpectedMaxScore != 0 {
		add("Expected best from luck alone", numSafe(rb.ExpectedMaxScore, 3),
			fmt.Sprintf("what the top of %d trials scores with no skill at all", rb.Trials))
	}
	add("Configurations above zero", pctSafe(rb.PositiveShare), "")
	if rb.PlateauRatio.Defined() {
		add("Neighbour support", pctSafe(float64(rb.PlateauRatio)),
			"how the cells beside the winner scored")
	}
	if rb.PBO.Defined() {
		add("Probability of backtest overfitting", pctSafe(float64(rb.PBO)),
			fmt.Sprintf("over %d train/test splits; 50%% is a coin flip", rb.PBOSplits))
	}
	if rb.DeflatedSharpe.Defined() {
		add("Deflated Sharpe", pctSafe(float64(rb.DeflatedSharpe)),
			"confidence the edge survives the trial count")
	}
	if rc := rb.RealityCheck; rc.RealityCheckP.Defined() {
		add("Reality check p-value", FormatPValue(float64(rc.RealityCheckP), rc.Bootstraps),
			fmt.Sprintf("White's, over %d stationary-bootstrap resamples of all %d trials",
				rc.Bootstraps, rc.Trials))
		add("Hansen SPA p-value", FormatPValue(float64(rc.SPAP), rc.Bootstraps),
			"the studentised version, which the search's dead cells do not drag down")
	}
	if ns := rb.NullStrategy; ns.Percentile.Defined() {
		add("Beats random entries", pctSafe(float64(ns.Percentile)),
			fmt.Sprintf("against %d random strategies with the same %d holds, holding "+
				"periods and %s exposure", ns.Trials, ns.Episodes, pctSafe(ns.AvgExposure)))
		add("Random entries, median", ratioText(ns.NullMedian),
			fmt.Sprintf("what arbitrary timing with those habits scored, against %s for "+
				"the strategy", ratioText(ns.Score)))
	}
	d.Search = t
}

func (r *Report) buildAttribution(d *htmlDoc) {
	a := r.Run.Attribution
	if len(a.ByYear) < 2 {
		return
	}
	d.YearBars, d.YearKey = r.yearChart()

	yt := &table{Head: []cell{head("Year", ""), head("Return", "num"),
		head("Benchmark", "num"), head("Excess", "num"), head("Drawdown", "num")}}
	for _, y := range a.ByYear {
		yt.Rows = append(yt.Rows, []cell{
			txt(y.Label),
			signed(y.Return, pctSafe(y.Return)),
			num(pctSafe(y.BenchmarkReturn)),
			signed(y.Excess, pctSafe(y.Excess)),
			num(pctSafe(y.MaxDrawdown)),
		})
	}
	d.Years = yt

	if len(a.ByRegime) > 0 {
		rt := &table{Head: []cell{head("Market regime", ""), head("Return", "num"),
			head("Drawdown", "num"), head("Sessions", "num")}}
		for _, g := range a.ByRegime {
			rt.Rows = append(rt.Rows, []cell{
				txt(g.Label),
				signed(g.Return, pctSafe(g.Return)),
				num(pctSafe(g.MaxDrawdown)),
				num(strconv.Itoa(g.TradingDays)),
			})
		}
		d.Regimes = rt
	}

	if len(a.Stress) > 0 {
		d.StressLead = "How concentrated is the edge?"
		for _, s := range a.Stress {
			d.Stress = append(d.Stress, fmt.Sprintf("%s: %s, which is %s of the total gain.",
				s.Label, pctSafe(s.Return), pctSafe(s.ShareOfTotal)))
		}
	}

	if len(a.BySymbol) > 1 {
		st := &table{Head: []cell{head("Holding", ""), head("Net P&L", "num"),
			head("Share", "num"), head("Trades", "num"), head("Win rate", "num")}}
		n := len(a.BySymbol)
		if n > 10 {
			n = 10
		}
		for _, s := range a.BySymbol[:n] {
			st.Rows = append(st.Rows, []cell{
				txt(s.Symbol),
				signed(s.NetPnL, moneySafe(s.NetPnL)),
				num(pctSafe(s.Contribution)),
				num(strconv.Itoa(s.Trades)),
				num(pctSafe(s.WinRate)),
			})
		}
		d.Symbols = st
	}
}

func (r *Report) buildCosts(d *htmlDoc) {
	c := r.Costs
	if c == nil || len(c.Points) == 0 {
		return
	}
	t := &table{Head: []cell{head("Slippage", ""), head("Return", "num"),
		head("Annualised", "num"), head("Sharpe", "num"), head("Costs paid", "num")}}
	for _, p := range c.Points {
		bps := numSafe(p.SlippageBps, 0) + " bps"
		if p.Error != "" {
			t.Rows = append(t.Rows, []cell{txt(bps), num("failed"), num(""), num(""), num("")})
			continue
		}
		t.Rows = append(t.Rows, []cell{
			txt(bps),
			signed(p.TotalReturn, pctSafe(p.TotalReturn)),
			num(pctSafe(p.CAGR)),
			num(ratioText(p.Sharpe)),
			num(moneySafe(p.TotalCosts)),
		})
	}
	d.Costs = t
	if c.BreakEvenBps.Defined() {
		d.CostNote = fmt.Sprintf("Break-even slippage: %s bps.",
			numSafe(float64(c.BreakEvenBps), 1))
	} else {
		d.CostNote = "The strategy did not cross into a loss anywhere in the range scanned."
	}
}

func (r *Report) buildFactors(d *htmlDoc) {
	f := r.Factors
	if f == nil || len(f.Factors) == 0 {
		return
	}
	d.FactorIntro = "The strategy's excess returns, regressed on tradable proxies for " +
		"the factors that already explain most cross-sectional equity returns. A loading " +
		"says the strategy is taking a risk somebody has already named and priced. The " +
		"intercept is what is left over, and its t-statistic is whether that remainder " +
		"can be told apart from zero."

	t := &table{Head: []cell{head("Factor", ""), head("Proxy", ""),
		head("Beta", "num"), head("Std error", "num"), head("t-stat", "num")}}
	for _, l := range f.Factors {
		t.Rows = append(t.Rows, []cell{
			txt(l.Name), txt(l.Proxy),
			num(ratioText(l.Beta)), num(ratioText(l.StdErr)), num(ratioText(l.TStat)),
		})
	}
	t.Rows = append(t.Rows, []cell{
		cell{Text: "Alpha, annualised", Class: "total"}, txt(""),
		strongCell(pctOrNAText(f.Alpha)),
		num(pctOrNAText(f.AlphaStdErr)),
		strongCell(ratioText(f.AlphaTStat)),
	})
	d.Factors = t

	note := fmt.Sprintf("R² %s (adjusted %s) over %d observations, with Newey-West "+
		"standard errors at a lag of %d bars to allow for the autocorrelation in daily "+
		"strategy returns.", ratioText(f.RSquared), ratioText(f.AdjRSquared),
		f.Observations, f.NeweyWestLag)
	if f.AvgExposure.Defined() {
		// The market beta is unreadable without this: a beta well under the
		// average exposure means the strategy was flat during the volatile
		// stretches, not that it found something uncorrelated.
		note += fmt.Sprintf(" Average gross exposure over the same bars was %s, which is "+
			"what the market beta should be read against.", ratioText(f.AvgExposure))
	}
	d.FactorNote = note + " " + f.ProxyNote

	for _, dr := range f.Dropped {
		d.FactorDropped = append(d.FactorDropped,
			fmt.Sprintf("%s (%s): %s.", dr.Name, dr.Proxy, dr.Reason))
	}
	if f.Verdict != "" {
		d.FactorVerdict = upperFirst(f.Verdict) + "."
	}
}

func (r *Report) buildBootstrap(d *htmlDoc) {
	bs := r.Bootstrap
	if bs.Trials == 0 {
		return
	}
	d.BootIntro = fmt.Sprintf("The backtest produced one sequence of returns. Resampling "+
		"it in blocks %d times — long enough to preserve the volatility clustering that "+
		"makes drawdowns what they are — gives the range of outcomes the same process "+
		"could plausibly have produced.", bs.Trials)

	t := &table{Head: []cell{head("", ""), head("Value", "num")}}
	add := func(label string, v float64) {
		t.Rows = append(t.Rows, []cell{rowHead(label), num(pctSafe(v))})
	}
	add("Total return, 5th percentile", bs.ReturnP05)
	add("Total return, median", bs.ReturnMedian)
	add("Total return, 95th percentile", bs.ReturnP95)
	add("Max drawdown, median", bs.DrawdownMedian)
	add("Max drawdown, 5th percentile", bs.DrawdownP05)
	add("Chance of finishing down", bs.LossProbability)
	d.Boot = t
	d.BootNote = "The fifth-percentile drawdown is the number to plan around, not the " +
		"one that happened to occur."
}

func (r *Report) buildMechanics(d *htmlDoc) {
	m := r.Run.Manifest
	t := &table{}
	add := func(label, value string) {
		t.Rows = append(t.Rows, []cell{rowHead(label), cell{Text: value, Class: "wrap"}})
	}
	add("Data", fmt.Sprintf("%s, %s, %d sessions",
		m.DataProvider, plural(len(m.Coverage), "symbol"), m.CalendarDays))
	add("Fills", fillText(m.Fill))
	add("Costs", fmt.Sprintf("%s bps slippage, %s%% commission, %s%% short borrow",
		numSafe(m.Costs.SlippageBps, 0), numSafe(m.Costs.CommissionPct*100, 2),
		numSafe(m.Costs.ShortBorrowAnnualPct*100, 1)))
	add("Starting capital", moneySafe(m.InitialCash))
	t.Rows = append(t.Rows, []cell{rowHead("Code hash"),
		{Text: shortHash(m.CodeSHA256), Class: "wrap mono"}})
	add("Build", fmt.Sprintf("%s, %s", m.Version, m.GoVersion))
	if m.AICallCount > 0 {
		add("Model calls inside the backtest",
			fmt.Sprintf("%d (%d cached)", m.AICallCount, m.AICacheHits))
	}
	add("Exactly reproducible", fmt.Sprintf("%v", m.Reproducible()))
	d.Mechanics = t

	d.Assumptions = r.Assumptions
	d.Limitations = r.Limitations
	d.Closing = "Past performance says very little about future returns, and an " +
		"overfitted backtest says nothing at all. The statistics above exist to put a " +
		"number on how much of this result is the second case."
}

// buildNav lists only the sections this report actually has, so a run with no
// sweep does not offer a link to an empty one.
func buildNav(d *htmlDoc) []navItem {
	var nav []navItem
	add := func(present bool, id, label string) {
		if present {
			nav = append(nav, navItem{ID: id, Label: label})
		}
	}
	add(d.HasRun, "verdict", "Verdict")
	add(len(d.Findings) > 0, "objections", "Objections")
	add(d.Results != nil, "results", "Results")
	add(d.OutOfSample != nil, "out-of-sample", "Out of sample")
	add(d.Search != nil, "search", "The search")
	add(d.Years != nil, "attribution", "Where it came from")
	add(d.Costs != nil, "costs", "Friction")
	add(d.Factors != nil, "factors", "Alpha")
	add(d.Boot != nil, "distribution", "Distribution")
	add(d.Mechanics != nil, "provenance", "Provenance")
	add(d.Code != "", "code", "The strategy")
	return nav
}

// ── the narrative ───────────────────────────────────────────────────────────

// narrativeBlocks splits the model's prose into paragraphs and bullet lists.
//
// This is not a Markdown implementation and does not try to be. The narrate
// prompt asks for three or four paragraphs of plain prose, so the only
// constructs worth handling are the ones a model writes anyway. Anything else
// reaches the page as the characters the model typed, which a reader would
// rather see than a half-applied transformation.
func narrativeBlocks(s string) []narrBlock {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []narrBlock
	var para []string
	flushPara := func() {
		if len(para) == 0 {
			return
		}
		out = append(out, narrBlock{Items: [][]textRun{inlineRuns(strings.Join(para, " "))}})
		para = nil
	}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			flushPara()
		case strings.HasPrefix(line, "- "), strings.HasPrefix(line, "* "):
			flushPara()
			item := inlineRuns(strings.TrimSpace(line[2:]))
			if n := len(out); n > 0 && out[n-1].List {
				out[n-1].Items = append(out[n-1].Items, item)
				continue
			}
			out = append(out, narrBlock{List: true, Items: [][]textRun{item}})
		default:
			para = append(para, line)
		}
	}
	flushPara()
	return out
}

// inlineRuns splits a line on bold and inline-code markers. The text of every
// run reaches the template unescaped and is escaped there, so a strategy name
// inside a model's summary is still just text.
func inlineRuns(s string) []textRun {
	var out []textRun
	var plain strings.Builder
	flush := func() {
		if plain.Len() > 0 {
			out = append(out, textRun{Text: plain.String()})
			plain.Reset()
		}
	}
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "**") {
			if j := strings.Index(s[i+2:], "**"); j > 0 {
				flush()
				out = append(out, textRun{Text: s[i+2 : i+2+j], Bold: true})
				i += j + 4
				continue
			}
		}
		if s[i] == '`' {
			if j := strings.IndexByte(s[i+1:], '`'); j > 0 {
				flush()
				out = append(out, textRun{Text: s[i+1 : i+1+j], Code: true})
				i += j + 2
				continue
			}
		}
		plain.WriteByte(s[i])
		i++
	}
	flush()
	return out
}

// ── the page ────────────────────────────────────────────────────────────────

var reportPage = template.Must(template.New("report").Parse(reportPageSource))

const reportPageSource = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>{{.Title}}</title>
<style>
:root {
  color-scheme: light dark;
  --bg: #f6f5f2;
  --surface: #ffffff;
  --surface-2: #efeee9;
  --border: #e0ded7;
  --border-strong: #c7c4ba;
  --text: #191917;
  --text-2: #55534d;
  --text-3: #7e7c73;
  --good: #17795a;
  --bad: #bd3b2c;
  --warn: #96650a;
  --gold: #9a6f21;
  --grid: #e8e6df;
  --code-bg: #f4f3ee;
  --s1: #2a6fc4;
  --s2: #c2591f;
  --s3: #7d7a72;
}

@media (prefers-color-scheme: dark) {
  :root {
    --bg: #131312;
    --surface: #1b1b19;
    --surface-2: #232320;
    --border: #33322d;
    --border-strong: #46453d;
    --text: #f1efe9;
    --text-2: #c1bfb4;
    --text-3: #8a887d;
    --good: #26a97c;
    --bad: #e2705e;
    --warn: #cf8f16;
    --gold: #d9a441;
    --grid: #2a2926;
    --code-bg: #171716;
    --s1: #5c9df0;
    --s2: #e07a45;
    --s3: #8f8d83;
  }
}

* { box-sizing: border-box; }
html { -webkit-text-size-adjust: 100%; }
body {
  margin: 0;
  background: var(--bg);
  color: var(--text);
  font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto,
               "Helvetica Neue", Arial, sans-serif;
  font-size: 15px;
  line-height: 1.62;
  -webkit-font-smoothing: antialiased;
  /* A strategy name is whatever somebody typed, and one unbroken 200-character
     word would otherwise push the whole page sideways. */
  overflow-wrap: break-word;
}

.page { max-width: 960px; margin: 0 auto; padding: 52px 28px 96px; }

h1 { font-size: 34px; line-height: 1.16; letter-spacing: -0.021em; font-weight: 650;
     margin: 0 0 12px; overflow-wrap: anywhere; }
h2 { font-size: 21px; letter-spacing: -0.012em; font-weight: 600; margin: 0 0 16px; }
h3 { font-size: 12px; text-transform: uppercase; letter-spacing: 0.08em;
     color: var(--text-3); font-weight: 650; margin: 30px 0 10px; }
p { margin: 0 0 14px; max-width: 74ch; }

section { margin: 0 0 46px; padding-top: 30px; border-top: 1px solid var(--border); }
section:first-of-type { border-top: none; }

.brand { font-size: 11px; letter-spacing: 0.18em; text-transform: uppercase;
         color: var(--gold); font-weight: 700; margin: 0 0 20px; }
.prompt { font-size: 17px; color: var(--text-2); max-width: 62ch; margin: 0 0 12px; }
.meta { font-size: 13px; color: var(--text-3); margin: 0; }

.nav { list-style: none; display: flex; flex-wrap: wrap; gap: 4px 16px;
       margin: 24px 0 0; padding: 0; font-size: 13px; }
.nav a { color: var(--text-3); text-decoration: none; border-bottom: 1px solid transparent; }
.nav a:hover { color: var(--text); border-bottom-color: var(--border-strong); }

.score { display: flex; gap: 26px; align-items: flex-start;
         background: var(--surface); border: 1px solid var(--border);
         border-radius: 10px; padding: 22px 24px; margin-bottom: 22px; }
.score-num { font-size: 54px; line-height: 1; font-weight: 700;
             letter-spacing: -0.03em; font-variant-numeric: tabular-nums; }
.score-den { font-size: 19px; font-weight: 500; color: var(--text-3); letter-spacing: 0; }
.score-body { flex: 1; min-width: 0; }
.score-label { font-size: 12px; text-transform: uppercase; letter-spacing: 0.08em;
               color: var(--text-3); margin: 0 0 10px; }
.meter { display: block; width: 100%; height: 6px; border-radius: 3px;
         overflow: hidden; margin: 0 0 14px; }
.meter-track { fill: var(--surface-2); }
.headline { margin: 0; font-size: 17px; max-width: none; }

.tone-good { color: var(--good); }
.tone-warn { color: var(--warn); }
.tone-bad { color: var(--bad); }
.meter-fill.tone-good { fill: var(--good); }
.meter-fill.tone-warn { fill: var(--warn); }
.meter-fill.tone-bad { fill: var(--bad); }

.verdicts { display: grid; grid-template-columns: minmax(0, max-content) minmax(0, 1fr);
            gap: 6px 22px; margin: 0; font-size: 14.5px; }
.verdicts dt { color: var(--text-3); }
.verdicts dd { margin: 0; color: var(--text-2); }

.finding { --tone: var(--text-3);
           border-left: 3px solid var(--tone); background: var(--surface);
           border-radius: 0 8px 8px 0; padding: 13px 18px; margin: 0 0 10px; }
.finding.sev-critical { --tone: var(--bad); }
.finding.sev-warning { --tone: var(--warn); }
.finding.sev-note { --tone: var(--text-3); }
.finding-head { display: flex; gap: 11px; align-items: baseline;
                flex-wrap: wrap; margin-bottom: 3px; }
.sev { font-size: 10.5px; letter-spacing: 0.1em; text-transform: uppercase;
       font-weight: 700; color: var(--tone); }
.finding-title { font-weight: 600; }
.finding p { margin: 0; color: var(--text-2); font-size: 14.5px; }

.scroll { overflow-x: auto; background: var(--surface); border: 1px solid var(--border);
          border-radius: 8px; margin: 0 0 16px; }
table { border-collapse: collapse; width: 100%; font-size: 13.5px; }
th, td { padding: 9px 15px; text-align: left; border-bottom: 1px solid var(--border);
         white-space: nowrap; }
thead th { font-size: 11px; text-transform: uppercase; letter-spacing: 0.06em;
           color: var(--text-3); font-weight: 650; background: var(--surface-2); }
tbody tr:last-child td { border-bottom: none; }
td.num, th.num { text-align: right; font-variant-numeric: tabular-nums; }
td.rowhead { color: var(--text-2); }
td.total, th.total { font-weight: 650; color: var(--text); }
td.pos { color: var(--good); }
td.neg { color: var(--bad); }
td.wrap { white-space: normal; color: var(--text-3); font-size: 12.5px; min-width: 18ch; }

figure { margin: 0 0 20px; }
.chart { display: block; width: 100%; height: auto; background: var(--surface);
         border: 1px solid var(--border); border-radius: 8px; }
.chart-top { border-radius: 8px 8px 0 0; border-bottom: none; }
.chart-bottom { border-radius: 0 0 8px 8px; }
.grid { stroke: var(--grid); stroke-width: 1; }
.axis { stroke: var(--border-strong); stroke-width: 1; }
.tick { fill: var(--text-3); font-size: 11px; }
.tick-y { text-anchor: end; }
.tick-x { text-anchor: middle; }
.line { fill: none; stroke-width: 1.6; stroke-linejoin: round; stroke-linecap: round; }
.line.s1 { stroke: var(--s1); }
.line.s2 { stroke: var(--s2); }
.line.s3 { stroke: var(--s3); }
.area { stroke: none; }
.area.a1 { fill: var(--s1); fill-opacity: 0.14; }
.bar.f1 { fill: var(--s1); }
.bar.f2 { fill: var(--s2); }

.key { list-style: none; display: flex; flex-wrap: wrap; gap: 4px 20px;
       margin: 10px 0 0; padding: 0; font-size: 12.5px; color: var(--text-2); }
.key li { display: flex; align-items: center; gap: 8px; }
.sw { width: 15px; height: 3px; border-radius: 2px; display: inline-block; }
.sw1 { background: var(--s1); }
.sw2 { background: var(--s2); }
.sw3 { background: var(--s3); }

figcaption, .note { font-size: 12.5px; color: var(--text-3); max-width: 74ch; }
figcaption { margin-top: 10px; }
.note { margin: -4px 0 16px; }
.intro { color: var(--text-2); }

ul.plain { list-style: none; padding: 0; margin: 0 0 16px; max-width: 74ch; }
ul.plain li { position: relative; padding-left: 19px; margin-bottom: 6px; color: var(--text-2); }
ul.plain li::before { content: ""; position: absolute; left: 2px; top: 0.78em;
                      width: 7px; height: 1px; background: var(--border-strong); }

pre { background: var(--code-bg); border: 1px solid var(--border); border-radius: 8px;
      padding: 16px 18px; overflow-x: auto; margin: 0; }
code { font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas,
       "Liberation Mono", monospace; font-size: 12.5px; line-height: 1.55; }
p code { background: var(--code-bg); border: 1px solid var(--border);
         border-radius: 4px; padding: 1px 5px; font-size: 0.88em; }
td.mono { font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas,
          "Liberation Mono", monospace; font-size: 12.5px; }

.closing { margin-top: 34px; padding-top: 20px; border-top: 1px solid var(--border);
           color: var(--text-3); font-size: 13.5px; }

@media (max-width: 640px) {
  .page { padding: 30px 16px 60px; }
  h1 { font-size: 27px; }
  .score { flex-direction: column; gap: 12px; }
  .verdicts { grid-template-columns: minmax(0, 1fr); gap: 0; }
  .verdicts dt { margin-top: 10px; }
}

@media print {
  :root {
    --bg: #ffffff;
    --surface: #ffffff;
    --surface-2: #f2f2f2;
    --border: #cccccc;
    --border-strong: #999999;
    --text: #000000;
    --text-2: #2c2c2c;
    --text-3: #565656;
    --good: #0f5c3f;
    --bad: #8c2a1e;
    --warn: #6b4707;
    --gold: #6b4d16;
    --grid: #dddddd;
    --code-bg: #f5f5f5;
    --s1: #14508f;
    --s2: #8c4415;
    --s3: #666666;
  }
  body { background: #ffffff; font-size: 11pt; }
  .page { max-width: none; padding: 0; }
  .nav { display: none; }
  .score, .finding, figure, .scroll, pre { break-inside: avoid; }
  h1, h2, h3 { break-after: avoid; }
  pre, code { white-space: pre-wrap; word-break: break-word; }
}
</style>
</head>
<body>
<div class="page">

<header>
  <p class="brand">pyrite</p>
  <h1>{{.Title}}</h1>
  {{with .Prompt}}<p class="prompt">{{.}}</p>{{end}}
  <p class="meta">{{.Meta}}</p>
  {{with .Nav}}
  <ul class="nav">{{range .}}<li><a href="#{{.ID}}">{{.Label}}</a></li>{{end}}</ul>
  {{end}}
</header>

{{with .Narrative}}
<section id="summary">
  {{range .}}{{if .List}}<ul class="plain">{{range .Items}}<li>{{template "runs" .}}</li>{{end}}</ul>
  {{else}}{{range .Items}}<p>{{template "runs" .}}</p>{{end}}{{end}}{{end}}
</section>
{{end}}

{{if .HasRun}}
<section id="verdict">
  <h2>Verdict</h2>
  <div class="score">
    <div class="score-num {{.ScoreTone}}">{{.TrustScore}}<span class="score-den">/100</span></div>
    <div class="score-body">
      <p class="score-label">How much should you believe this</p>
      <svg class="meter" viewBox="0 0 200 6" preserveAspectRatio="none" role="presentation">
        <rect class="meter-track" x="0" y="0" width="200" height="6" />
        <rect class="meter-fill {{.ScoreTone}}" x="0" y="0" width="{{.ScoreWidth}}" height="6" />
      </svg>
      {{with .Headline}}<p class="headline">The largest single objection is that {{.}}.</p>{{end}}
    </div>
  </div>
  {{with .Verdicts}}
  <dl class="verdicts">{{range .}}<dt>{{.Label}}</dt><dd>{{.Text}}</dd>{{end}}</dl>
  {{end}}
</section>
{{end}}

{{with .Findings}}
<section id="objections">
  <h2>Objections</h2>
  <p class="intro">Every one of these is computed from the run rather than asked of a
  model, so none of them can be a number that was never there.</p>
  {{range .}}
  <div class="finding {{.Class}}">
    <div class="finding-head"><span class="sev">{{.Label}}</span><span class="finding-title">{{.Title}}</span></div>
    <p>{{.Detail}}</p>
  </div>
  {{end}}
</section>
{{end}}

{{if .Results}}
<section id="results">
  <h2>Results, {{.Period}}</h2>
  {{template "table" .Results}}
  {{with .Distribution}}<p class="note">{{.}}</p>{{end}}
  {{with .Trades}}<p class="note">{{.}}</p>{{end}}
  {{with .Equity}}
  <figure>
    {{template "linechart" .}}
    {{with $.Drawdown}}{{template "linechart" .}}{{end}}
    {{with $.EquityKey}}<ul class="key">{{range .}}<li><span class="sw {{.Swatch}}"></span>{{.Label}}</li>{{end}}</ul>{{end}}
    {{with $.EquityNote}}<figcaption>{{.}}</figcaption>{{end}}
  </figure>
  {{end}}
</section>
{{end}}

{{if .OutOfSample}}
<section id="out-of-sample">
  <h2>Out of sample</h2>
  <p class="intro">{{.OOSIntro}}</p>
  {{template "table" .OutOfSample}}
</section>
{{end}}

{{if .Search}}
<section id="search">
  <h2>How much of this is the search?</h2>
  <p class="intro">{{.SearchIntro}}</p>
  {{template "table" .Search}}
</section>
{{end}}

{{if .Years}}
<section id="attribution">
  <h2>Where the return came from</h2>
  {{with .YearBars}}
  <figure>
    {{template "barchart" .}}
    {{with $.YearKey}}<ul class="key">{{range .}}<li><span class="sw {{.Swatch}}"></span>{{.Label}}</li>{{end}}</ul>{{end}}
  </figure>
  {{end}}
  {{template "table" .Years}}
  {{with .Regimes}}<h3>By market regime</h3>{{template "table" .}}{{end}}
  {{with .Stress}}
  <h3>{{$.StressLead}}</h3>
  <ul class="plain">{{range .}}<li>{{.}}</li>{{end}}</ul>
  {{end}}
  {{with .Symbols}}<h3>By holding</h3>{{template "table" .}}{{end}}
</section>
{{end}}

{{if .Costs}}
<section id="costs">
  <h2>How much survives friction</h2>
  {{template "table" .Costs}}
  {{with .CostNote}}<p class="note">{{.}}</p>{{end}}
</section>
{{end}}

{{if .Factors}}
<section id="factors">
  <h2>Is any of this alpha?</h2>
  <p class="intro">{{.FactorIntro}}</p>
  {{template "table" .Factors}}
  {{with .FactorVerdict}}<p>{{.}}</p>{{end}}
  <p class="note">{{.FactorNote}}</p>
  {{with .FactorDropped}}
  <h3>Not measured over this period</h3>
  <ul class="plain">{{range .}}<li>{{.}}</li>{{end}}</ul>
  {{end}}
</section>
{{end}}

{{if .Boot}}
<section id="distribution">
  <h2>One path is not the distribution</h2>
  <p class="intro">{{.BootIntro}}</p>
  {{template "table" .Boot}}
  <p class="note">{{.BootNote}}</p>
</section>
{{end}}

{{if .Mechanics}}
<section id="provenance">
  <h2>How this was produced</h2>
  {{template "table" .Mechanics}}
  {{with .Assumptions}}
  <h3>Assumptions made where the request was open</h3>
  <ul class="plain">{{range .}}<li>{{.}}</li>{{end}}</ul>
  {{end}}
  {{with .Limitations}}
  <h3>Stated limitations</h3>
  <ul class="plain">{{range .}}<li>{{.}}</li>{{end}}</ul>
  {{end}}
  <p class="closing">{{.Closing}}</p>
</section>
{{end}}

{{with .Code}}
<section id="code">
  <h2>The strategy</h2>
  <pre><code>{{.}}</code></pre>
</section>
{{end}}

</div>
</body>
</html>
{{define "runs"}}{{range .}}{{if .Bold}}<strong>{{.Text}}</strong>{{else if .Code}}<code>{{.Text}}</code>{{else}}{{.Text}}{{end}}{{end}}{{end}}
{{define "table"}}<div class="scroll"><table>
{{if .Head}}<thead><tr>{{range .Head}}<th class="{{.Class}}">{{.Text}}</th>{{end}}</tr></thead>{{end}}
<tbody>{{range .Rows}}<tr>{{range .}}<td class="{{.Class}}">{{.Text}}</td>{{end}}</tr>{{end}}</tbody>
</table></div>{{end}}
{{define "linechart"}}<svg class="chart {{.Frame}}" viewBox="{{.ViewBox}}" role="img">
<title>{{.Title}}</title>
{{range .YTicks}}<line class="grid" x1="{{$.X0}}" x2="{{$.X1}}" y1="{{.Line}}" y2="{{.Line}}" /><text class="tick tick-y" x="{{$.YLabelX}}" y="{{.Text}}">{{.Label}}</text>
{{end}}{{range .XTicks}}<line class="grid" x1="{{.Line}}" x2="{{.Line}}" y1="{{$.Y0}}" y2="{{$.Y1}}" />{{if $.ShowX}}<text class="tick tick-x" x="{{.Text}}" y="{{$.XLabelY}}">{{.Label}}</text>{{end}}
{{end}}{{with .Zero}}<line class="axis" x1="{{$.X0}}" x2="{{$.X1}}" y1="{{.}}" y2="{{.}}" />
{{end}}{{range .Series}}{{if .Area}}<path class="area {{.Fill}}" d="{{.Area}}" />{{end}}{{if .Line}}<path class="line {{.Stroke}}" d="{{.Line}}" />{{end}}
{{end}}</svg>{{end}}
{{define "barchart"}}<svg class="chart" viewBox="{{.ViewBox}}" role="img">
<title>{{.Title}}</title>
{{range .YTicks}}<line class="grid" x1="{{$.X0}}" x2="{{$.X1}}" y1="{{.Line}}" y2="{{.Line}}" /><text class="tick tick-y" x="{{$.YLabelX}}" y="{{.Text}}">{{.Label}}</text>
{{end}}{{range .Bars}}<rect class="bar {{.Class}}" x="{{.X}}" y="{{.Y}}" width="{{.W}}" height="{{.H}}" />
{{end}}{{with .Zero}}<line class="axis" x1="{{$.X0}}" x2="{{$.X1}}" y1="{{.}}" y2="{{.}}" />
{{end}}{{range .XTicks}}<text class="tick tick-x" x="{{.Text}}" y="{{$.XLabelY}}">{{.Label}}</text>
{{end}}</svg>{{end}}
`
