package engine

import (
	"encoding/xml"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charbelkassab/pyrite/internal/market"
)

func htmlFor(t *testing.T, rep *Report) string {
	t.Helper()
	doc, err := rep.HTML()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return doc
}

// parseHTML checks the document is well formed by decoding it as XML.
//
// The page is written to satisfy both grammars — every void element is closed,
// nothing relies on implied end tags — so an XML decoder is a free and strict
// check that no tag was left open and no attribute left unquoted, which is
// exactly the class of mistake a hand-written template makes.
func parseHTML(t *testing.T, doc string) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(doc))
	dec.Strict = true
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the document is not well formed: %v", err)
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	if depth != 0 {
		t.Errorf("unbalanced elements: depth %d at the end of the document", depth)
	}
}

func TestHTMLReportIsWellFormedAndComplete(t *testing.T) {
	doc := htmlFor(t, reportFor(t))
	parseHTML(t, doc)

	for _, want := range []string{
		"<title>Test strategy</title>",
		"How much should you believe this",
		"<h2>Verdict</h2>",
		"<h2>Objections</h2>",
		"Results, 5 January 2016 to 29 December 2023",
		"<h2>Out of sample</h2>",
		"<h2>How much of this is the search?</h2>",
		"<h2>Where the return came from</h2>",
		"<h2>How much survives friction</h2>",
		"<h2>Is any of this alpha?</h2>",
		"<h2>One path is not the distribution</h2>",
		"<h2>How this was produced</h2>",
		"<h2>The strategy</h2>",
		// The charts must actually be drawn, not merely framed.
		`class="line s1"`,
		`class="area a1"`,
		`class="bar f1"`,
		"prefers-color-scheme: dark",
		"@media print",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	if strings.Contains(doc, "%!") {
		t.Error("a formatting verb went unfilled")
	}
	if strings.Contains(doc, "NaN") || strings.Contains(doc, "Inf") {
		t.Error("a non-finite number reached the page")
	}
	if strings.Contains(doc, "ZgotmplZ") {
		t.Error("html/template rejected a value it could not make safe")
	}
}

func TestHTMLReportLeadsWithTheVerdictAndTheObjections(t *testing.T) {
	doc := htmlFor(t, reportFor(t))
	verdict := strings.Index(doc, `id="verdict"`)
	objections := strings.Index(doc, `id="objections"`)
	results := strings.Index(doc, `id="results"`)
	code := strings.Index(doc, `id="code"`)
	if !(verdict > 0 && verdict < objections && objections < results && results < code) {
		t.Errorf("sections are out of order: verdict %d, objections %d, results %d, code %d",
			verdict, objections, results, code)
	}
}

// TestHTMLReportMakesNoNetworkRequests is the property the whole file exists
// for: it has to open from a memory stick, offline, in five years.
func TestHTMLReportMakesNoNetworkRequests(t *testing.T) {
	doc := htmlFor(t, reportFor(t))
	for _, banned := range []string{
		"http://", "https://", "//cdn", "@import", "url(", "<script", "<iframe",
		"<link", "<img", "src=",
	} {
		if strings.Contains(doc, banned) {
			t.Errorf("the page reaches outside itself: found %q", banned)
		}
	}
}

func TestHTMLReportEscapesStrategySuppliedText(t *testing.T) {
	const attack = `<script>alert(1)</script>`
	rep := &Report{
		Title:     attack,
		Prompt:    `" onload="alert(2)`,
		Generated: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Run:       syntheticRun(risingCurve(400)),
	}
	rep.Run.Spec.Code = attack + "\nconst x = 1 < 2 && 3 > 2;"
	rep.Run.Critique = Critique{
		TrustScore: 40,
		Headline:   attack,
		Findings: []Finding{{
			Severity: Severity(`" class="sev-critical`),
			Title:    attack,
			Detail:   `</p><img onerror="alert(3)" />`,
		}},
	}
	doc := htmlFor(t, rep)
	parseHTML(t, doc)

	if strings.Contains(doc, "<script") {
		t.Fatal("a strategy-supplied string became a live tag")
	}
	if strings.Contains(doc, "alert(1)</script>") {
		t.Fatal("the script close tag survived unescaped")
	}
	if !strings.Contains(doc, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("the attacking string should appear as visible, escaped text")
	}
	// The handler text may appear — as characters on the page. What must not
	// appear is the tag that would carry it, or an unescaped quote that would
	// let the prompt close an attribute and open one of its own.
	if strings.Contains(doc, "<img") {
		t.Error("an image tag was assembled from strategy-supplied text")
	}
	if !strings.Contains(doc, "&lt;img onerror=") {
		t.Error("the injected tag should appear as escaped text")
	}
	if !strings.Contains(doc, `&#34; onload=&#34;alert(2)`) {
		t.Error("the prompt's quotes should have been escaped")
	}
	// A severity is engine-owned, but it travels through JSON. An unknown one
	// must fall back to a known class rather than being pasted into the page.
	if strings.Contains(doc, `class="finding " class="sev-critical`) {
		t.Error("a severity broke out of its class attribute")
	}
	if !strings.Contains(doc, `class="finding sev-note"`) {
		t.Error("an unrecognised severity should render as a note")
	}
}

func TestHTMLReportRendersUndefinedMetricsAsNotAvailable(t *testing.T) {
	nan := math.NaN()
	run := syntheticRun(risingCurve(400))
	run.Metrics.Sharpe = Ratio(nan)
	run.Metrics.Sortino = Ratio(math.Inf(1))
	run.Metrics.Volatility = nan
	run.Risk.Skew = nan
	run.Risk.CVaR95 = nan
	run.TradeStats = TradeStats{Closed: 3, PayoffRatio: Ratio(nan), Expectancy: nan}
	run.Attribution.BySymbol = []SymbolStats{
		{Symbol: "AAPL", NetPnL: nan, Contribution: nan, Trades: 2, WinRate: nan},
		{Symbol: "MSFT", NetPnL: 100, Contribution: 1, Trades: 1, WinRate: 1},
	}

	rep := &Report{Title: "Undefined", Generated: time.Now(), Run: run,
		Costs: &CostScan{
			Points:       []CostPoint{{SlippageBps: 5, Sharpe: Ratio(nan), TotalReturn: nan}},
			BreakEvenBps: Ratio(nan),
		},
	}
	doc := htmlFor(t, rep)
	parseHTML(t, doc)

	if strings.Contains(doc, "NaN") || strings.Contains(doc, "Inf") {
		t.Fatal("an undefined metric printed as a non-finite number")
	}
	if n := strings.Count(doc, "n/a"); n < 8 {
		t.Errorf("expected the undefined metrics to say n/a, found %d occurrences", n)
	}
}

// TestHTMLChartCoordinatesAreFinite covers the case that silently produces a
// blank chart: a range with no span, where the naive scale divides by zero.
func TestHTMLChartCoordinatesAreFinite(t *testing.T) {
	cases := map[string][]EquityPoint{
		"flat throughout": flatCurve(400),
		"flat in the middle": append(append(risingCurve(150), flatCurve(150)...),
			risingCurve(100)...),
		"one session": flatCurve(1),
	}
	for name, curve := range cases {
		t.Run(name, func(t *testing.T) {
			// The dates have to be a single increasing sequence for the axis
			// to mean anything, so they are restamped after the join.
			d := market.Day("2015-01-05").Time()
			for i := range curve {
				curve[i].Date = market.NewDay(d.AddDate(0, 0, i))
			}
			rep := &Report{Title: name, Generated: time.Now(), Run: syntheticRun(curve)}
			doc := htmlFor(t, rep)
			parseHTML(t, doc)

			paths := regexp.MustCompile(`[dxy]="([^"]*)"|width="([^"]*)"|height="([^"]*)"`).
				FindAllStringSubmatch(doc, -1)
			checked := 0
			for _, m := range paths {
				for _, field := range m[1:] {
					for _, tok := range strings.FieldsFunc(field, func(r rune) bool {
						return r == ' ' || r == ',' || r == 'M' || r == 'L' || r == 'Z'
					}) {
						v, err := strconv.ParseFloat(tok, 64)
						if err != nil {
							// Not a coordinate — a viewBox word or a class.
							continue
						}
						if !finite(v) {
							t.Fatalf("non-finite coordinate %q in %q", tok, field)
						}
						checked++
					}
				}
			}
			if checked == 0 {
				t.Fatal("no coordinates were checked, so the chart drew nothing")
			}
			if strings.Contains(doc, "NaN") {
				t.Fatal("NaN reached the SVG")
			}
		})
	}
}

func TestAxisPlacesAFlatRangeInTheMiddle(t *testing.T) {
	a := newAxis(5, 5, 100, 0)
	if got := a.at(5); got != 50 {
		t.Errorf("a flat range should map to the centre, got %v", got)
	}
	if got := a.at(math.NaN()); finite(got) {
		t.Errorf("an undefined value must not become a coordinate, got %v", got)
	}
	if got := newAxis(math.NaN(), math.Inf(1), 0, 100).at(1); !finite(got) {
		t.Errorf("an undefined range must still yield a finite pixel, got %v", got)
	}
	if got := pos(math.Inf(-1)); got != "" {
		t.Errorf("pos should refuse a non-finite value, got %q", got)
	}
}

func TestNiceTicksAreRoundAndBounded(t *testing.T) {
	if ticks := niceTicks(5, 5, 5); ticks != nil {
		t.Errorf("a zero span has no ticks, got %v", ticks)
	}
	if ticks := niceTicks(math.NaN(), 1, 5); ticks != nil {
		t.Errorf("an undefined range has no ticks, got %v", ticks)
	}
	ticks := niceTicks(-0.42, 0, 4)
	if len(ticks) < 2 || len(ticks) > 8 {
		t.Fatalf("want a handful of ticks, got %v", ticks)
	}
	for _, v := range ticks {
		if !finite(v) {
			t.Fatalf("non-finite tick in %v", ticks)
		}
	}
	// A drawdown axis has to carry an exact zero, or the baseline label reads
	// "-0.00%".
	last := ticks[len(ticks)-1]
	if last != 0 {
		t.Errorf("the top tick of a drawdown axis should be zero, got %v", last)
	}
}

func TestThinKeepsTheShapeAndTheEndpoints(t *testing.T) {
	pts := make([]chartPoint, 2000)
	for i := range pts {
		pts[i] = chartPoint{X: float64(i), Y: float64(i % 7)}
	}
	// A single spike in the middle is the thing decimation must not lose.
	pts[1000].Y = 999
	out := thin(pts, 100)
	if len(out) >= len(pts) {
		t.Fatalf("thinning did nothing: %d points", len(out))
	}
	if out[0] != pts[0] || out[len(out)-1] != pts[len(pts)-1] {
		t.Error("thinning dropped an endpoint, so the line stops short of the frame")
	}
	spike := false
	for _, p := range out {
		if p.Y == 999 {
			spike = true
		}
	}
	if !spike {
		t.Error("thinning lost the extreme, which is the one point that mattered")
	}
}

func TestNarrativeRendersEmphasisWithoutTrustingIt(t *testing.T) {
	rep := &Report{
		Title:     "Narrated",
		Generated: time.Now(),
		Narrative: "It returned **41%** over the period.\n\n" +
			"Run `pyrite audit SPY` before believing it.\n\n" +
			"- One <b>caveat</b>\n- Another",
	}
	doc := htmlFor(t, rep)
	parseHTML(t, doc)

	for _, want := range []string{
		"<strong>41%</strong>", "<code>pyrite audit SPY</code>",
		"&lt;b&gt;caveat&lt;/b&gt;", "<li>", "Another",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the summary is missing %q", want)
		}
	}
	if strings.Contains(doc, "<b>caveat</b>") {
		t.Error("markup inside the summary reached the page as markup")
	}
}

func TestHTMLReportRendersWithoutARun(t *testing.T) {
	doc := htmlFor(t, &Report{Title: "Nothing", Generated: time.Now()})
	parseHTML(t, doc)
	if !strings.Contains(doc, "<h1>Nothing</h1>") {
		t.Error("even an empty report should render its title")
	}
	for _, absent := range []string{"<h2>Verdict</h2>", "<h2>The strategy</h2>"} {
		if strings.Contains(doc, absent) {
			t.Errorf("%q should be omitted when there is no run", absent)
		}
	}
}

// ── fixtures ────────────────────────────────────────────────────────────────

// syntheticRun wraps a curve in the smallest Result the page will render, so
// the degenerate cases can be built without a backtest.
func syntheticRun(curve []EquityPoint) *Result {
	m := ComputeMetrics(curve, DailyScale(0))
	res := &Result{
		Spec:    Spec{Name: "synthetic", Start: "2015-01-05", End: "2016-12-30", Code: "// none"},
		Curve:   curve,
		Metrics: m,
		Risk:    ComputeRiskMetrics(curve, m.CAGR, DailyScale(0)),
		Manifest: Manifest{
			Version: "test", GoVersion: "go1.25", DataProvider: "synthetic",
			CalendarDays: len(curve), InitialCash: 100000,
		},
		Attribution: ComputeAttribution(curve, nil, nil, DailyScale(0)),
	}
	res.Critique = Criticise(res)
	return res
}

func risingCurve(n int) []EquityPoint {
	out := make([]EquityPoint, n)
	d := market.Day("2015-01-05").Time()
	v := 100000.0
	peak := v
	for i := range out {
		v *= 1.0008
		if v > peak {
			peak = v
		}
		out[i] = EquityPoint{Date: market.NewDay(d.AddDate(0, 0, i)), Value: v,
			Drawdown: v/peak - 1, Exposure: 1}
	}
	return out
}

func flatCurve(n int) []EquityPoint {
	out := make([]EquityPoint, n)
	d := market.Day("2015-01-05").Time()
	for i := range out {
		out[i] = EquityPoint{Date: market.NewDay(d.AddDate(0, 0, i)), Value: 100000}
	}
	return out
}
