package engine

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/charbelkassab/pyrite/internal/market"
)

// The self-test corpus: strategies built to be wrong in one specific way,
// each paired with the finding the critique has to produce for it.
//
// The critique is the product. It is also the one part of this tool that
// nothing exercises adversarially, because every strategy written for a test
// or an example is written to work. If a refactor stopped it detecting
// lookahead, every run would go on looking confident and nobody would notice:
// a smoke detector with a flat battery. This is the battery test.
//
// The control case at the end matters as much as the nine defective ones. A
// critic that flags everything is as useless as one that flags nothing, and
// without a negative control the corpus would only prove the critic is noisy.

// SelfTestCase is one deliberately defective strategy and the finding the
// critique has to produce for it.
type SelfTestCase struct {
	// Name identifies the case on the command line.
	Name string
	// Defect is what is wrong with this strategy, in one line.
	Defect string
	// Expect is the exact title of the finding the critique must produce.
	// Empty on the control, which must produce no critical finding at all.
	Expect string
	// Severity is the severity that finding has to carry. A defect demoted
	// to a note is a defect nobody reads.
	Severity Severity
	// Control marks the negative case: a sound backtest, which must come
	// back with nothing critical said about it.
	Control bool
	// MustScoreZero requires a trust score of exactly zero, which only the
	// run that produced no result at all should reach. Subtracting a penalty
	// from 100 would score an empty run above a real one with two flaws.
	MustScoreZero bool
	// Contrast describes the paired variant with the defect removed, which
	// must not produce Expect. It is what separates a critic that detects
	// the defect from one that fires on the strategy regardless.
	Contrast string

	spec Spec
	// bars supplies hand-built series when the defect needs a specific price
	// shape. Nil uses the synthetic provider.
	bars func() map[string]*market.Series
	// clean turns the spec into the variant Contrast describes.
	clean func(*Spec)
}

// SelfTestOutcome is what happened when one case was put through the engine.
type SelfTestOutcome struct {
	Case SelfTestCase `json:"-"`
	Name string       `json:"name"`
	// Caught is the finding that matched, nil when nothing did.
	Caught *Finding `json:"caught,omitempty"`
	// Findings is the whole critique, so a failure can be read rather than
	// guessed at.
	Findings   []Finding `json:"findings"`
	TrustScore int       `json:"trust_score"`
	// Return and ContrastReturn are the two variants' total returns, which
	// is how a case can show that the defect it embodies is real: the costs
	// artefact only makes money in the run that charges nothing.
	Return         float64 `json:"total_return"`
	ContrastReturn float64 `json:"contrast_return,omitempty"`
	Pass           bool    `json:"pass"`
	// Why states what went wrong, empty when the case passed. A failure here
	// is a finding about the critique, not about the strategy.
	Why string `json:"why,omitempty"`
}

// RunSelfTest puts the whole corpus through the engine.
//
// Everything runs on synthetic or hand-built bars, so it needs no network, no
// vendor and no key, and returns the same answer every time.
func RunSelfTest(ctx context.Context) ([]SelfTestOutcome, error) {
	cases := SelfTestCases()
	out := make([]SelfTestOutcome, 0, len(cases))
	for _, c := range cases {
		o, err := c.Run(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// Run executes one case and judges the critique it produced.
func (c SelfTestCase) Run(ctx context.Context) (SelfTestOutcome, error) {
	out := SelfTestOutcome{Case: c, Name: c.Name}

	store, err := c.store()
	if err != nil {
		return out, err
	}
	res, err := New(c.spec, store).Run(ctx)
	if err != nil {
		out.Why = "the run itself failed: " + err.Error()
		return out, nil
	}
	out.Findings = res.Critique.Findings
	out.TrustScore = res.Critique.TrustScore
	out.Return = res.Metrics.TotalReturn
	out.Caught = findingTitled(res.Critique, c.Expect)

	// The contrast runs first so that a critic which fires on the strategy
	// rather than on the defect fails here rather than passing on the
	// strength of a finding it would have produced either way.
	if c.clean != nil {
		spec := c.spec
		c.clean(&spec)
		clean, err := New(spec, store).Run(ctx)
		if err != nil {
			out.Why = "the contrast run failed: " + err.Error()
			return out, nil
		}
		out.ContrastReturn = clean.Metrics.TotalReturn
		if f := findingTitled(clean.Critique, c.Expect); f != nil {
			out.Why = fmt.Sprintf("%q was also reported for %s, so it is not this defect that produced it",
				c.Expect, c.Contrast)
			return out, nil
		}
	}

	switch {
	case c.Control:
		for _, f := range res.Critique.Findings {
			if f.Severity == SeverityCritical {
				out.Why = fmt.Sprintf("a sound backtest was called critical: %q — %s", f.Title, f.Detail)
				return out, nil
			}
		}
	case out.Caught == nil:
		// What the critique did say is on the outcome, so the caller can
		// print it in the shape its reader wants rather than reading it out
		// of the middle of this sentence.
		out.Why = fmt.Sprintf("no finding titled %q", c.Expect)
		return out, nil
	case out.Caught.Severity != c.Severity:
		out.Why = fmt.Sprintf("%q was reported as a %s, not as a %s", c.Expect, out.Caught.Severity, c.Severity)
		return out, nil
	}
	if c.MustScoreZero && out.TrustScore != 0 {
		out.Why = fmt.Sprintf("a run that produced no result scored %d/100, not 0", out.TrustScore)
		return out, nil
	}

	out.Pass = true
	return out, nil
}

// store serves the case its data: hand-built bars where the defect needs a
// particular price shape, and the synthetic provider otherwise.
func (c SelfTestCase) store() (*market.Store, error) {
	fund, err := market.LoadFundamentals("")
	if err != nil {
		return nil, fmt.Errorf("load bundled fundamentals: %w", err)
	}
	if c.bars == nil {
		return market.NewStore(market.NewSyntheticProvider(), nil, fund), nil
	}
	return market.NewStore(&staticProvider{series: c.bars()}, nil, fund), nil
}

// findingTitled returns the finding with exactly this title. The titles are
// the contract: a critique that renames one has changed what it tells a
// reader, and the corpus should notice.
func findingTitled(c Critique, title string) *Finding {
	if title == "" {
		return nil
	}
	for i := range c.Findings {
		if c.Findings[i].Title == title {
			return &c.Findings[i]
		}
	}
	return nil
}

// titles lists what the critique did say, for a failure message that is
// readable without a debugger.
func titles(c Critique) string {
	if len(c.Findings) == 0 {
		return "nothing at all"
	}
	out := ""
	for i, f := range c.Findings {
		if i > 0 {
			out += "; "
		}
		out += string(f.Severity) + " " + f.Title
	}
	return out
}

// staticProvider serves pre-built bars.
//
// The test files have their own copy of this. It cannot be shared: the corpus
// ships inside the binary so that `pyrite selftest` runs it, and a test file
// is not compiled into one.
type staticProvider struct{ series map[string]*market.Series }

func (p *staticProvider) Name() string { return "hand-built" }

func (p *staticProvider) Fetch(_ context.Context, symbol string, _, _ market.Day) (*market.Series, error) {
	if s, ok := p.series[symbol]; ok {
		return s, nil
	}
	return nil, market.ErrNotFound
}

func (p *staticProvider) Search(context.Context, string) ([]market.Quote, error) { return nil, nil }

// SelfTestCases is the corpus. Each case embodies one defect and names the
// finding that has to catch it.
func SelfTestCases() []SelfTestCase {
	return []SelfTestCase{
		{
			// A track record of two trades. The statistics computed from it
			// are arithmetic on a sample too small to carry any of them.
			Name:     "too-few-trades",
			Defect:   "two round trips in ten years",
			Expect:   "too few trades to mean anything",
			Severity: SeverityCritical,
			spec: selfTestSpec("2014-01-02", "2023-12-29", []string{"SPY"}, `
				function onDay(ctx) {
					if (ctx.dayIndex === 20) ctx.buy("SPY", { pctCash: 0.9 });
					if (ctx.dayIndex === 260) ctx.sell("SPY");
					if (ctx.dayIndex === 1300) ctx.buy("SPY", { pctCash: 0.9 });
					if (ctx.dayIndex === 1500) ctx.sell("SPY");
				}
			`),
		},
		{
			// Buys the dip at the dip's own closing price, which is a price
			// nobody could have traded at: it is how far the day fell that
			// identifies the day. The contrast is the identical rule filled
			// at the next open, and it is the whole of the return.
			Name:     "lookahead-close-fill",
			Defect:   "buys a 6% fall at that day's own close",
			Expect:   "fills at a price the strategy had already seen",
			Severity: SeverityCritical,
			Contrast: "the same strategy filled at the next session's open",
			spec: func() Spec {
				s := selfTestSpec("2018-01-02", "2022-12-30", []string{"SAW"}, `
					function onDay(ctx) {
						var r = ctx.ret("SAW", 1);
						if (r === null) return;
						if (r <= -0.04 && !ctx.hasPosition("SAW")) ctx.buy("SAW", { pctCash: 0.95 });
						if (r >= 0.04 && ctx.hasPosition("SAW")) ctx.sell("SAW");
					}
				`)
				s.Fill = FillClose
				return s
			}(),
			bars: func() map[string]*market.Series {
				return map[string]*market.Series{"SAW": dipSeries()}
			},
			clean: func(s *Spec) { s.Fill = FillNextOpen },
		},
		{
			// Trades its whole book every session and is charged nothing for
			// it. The contrast charges the tool's default five basis points,
			// which is the entire difference between a profit and a loss.
			Name:     "costs-artefact",
			Defect:   "churns daily and is charged no slippage",
			Expect:   "frictionless",
			Severity: SeverityCritical,
			Contrast: "the same churn with the default five basis points charged",
			spec: func() Spec {
				s := selfTestSpec("2018-01-02", "2022-12-30", []string{"AAPL", "MSFT", "NVDA"}, `
					function onDay(ctx) {
						if (ctx.dayIndex % 2 === 0) ctx.equalWeight(ctx.universe(), 0.98);
						else ctx.liquidate();
					}
				`)
				s.Costs = DefaultCosts()
				s.Costs.SlippageBps = 0
				return s
			}(),
			clean: func(s *Spec) { s.Costs = DefaultCosts() },
		},
		{
			// Everything the strategy earned arrived on a handful of days.
			// The average day contributed nothing, so the result is a bet on
			// those episodes happening again rather than an edge.
			Name:     "few-good-days",
			Defect:   "six sessions carry six years of gain",
			Expect:   "the return is concentrated in a few sessions",
			Severity: SeverityCritical,
			spec: selfTestSpec("2016-01-04", "2021-12-31", []string{"BURST"}, `
				function onDay(ctx) {
					if (ctx.dayIndex === 0) ctx.buy("BURST", { pctCash: 0.99 });
				}
			`),
			bars: func() map[string]*market.Series {
				return map[string]*market.Series{"BURST": burstSeries()}
			},
		},
		{
			// Many small gains and an occasional large loss: the payoff of a
			// sold option, whatever the strategy calls itself. Sharpe reads
			// the small gains and misses the shape.
			Name:     "short-volatility",
			Defect:   "0.25% most days, -9% every tenth week",
			Expect:   "this is short volatility in disguise",
			Severity: SeverityCritical,
			spec: selfTestSpec("2016-01-04", "2021-12-31", []string{"PREM"}, `
				function onDay(ctx) {
					if (ctx.dayIndex === 0) ctx.buy("PREM", { pctCash: 0.99 });
				}
			`),
			bars: func() map[string]*market.Series {
				return map[string]*market.Series{"PREM": premiumSeries()}
			},
		},
		{
			// An empty onDay. There is no result to have confidence in, which
			// is a different thing from a bad one, and it has to score zero
			// rather than be marked down from a hundred.
			Name:          "never-traded",
			Defect:        "an empty onDay: nothing ever fills",
			Expect:        "the strategy never traded",
			Severity:      SeverityCritical,
			MustScoreZero: true,
			spec: selfTestSpec("2018-01-02", "2022-12-30", []string{"AAPL", "MSFT"}, `
				function onDay(ctx) {}
			`),
		},
		{
			// A universe of names chosen because they are worth holding
			// today. Three of the four have no price at the start date, which
			// is the signature of a list assembled after the fact; the names
			// that failed in between were never in the test.
			//
			// That signature is the whole of what the critique looks for. A
			// hand-picked list whose names all did exist on day one is the
			// same bias with nothing to detect it by, and the critique says
			// nothing about it: see TestSurvivorshipWithFullHistoryIsNotCaught.
			Name:     "survivorship",
			Defect:   "today's winners, three listed after the start",
			Expect:   "survivorship in the symbol list",
			Severity: SeverityWarning,
			spec: selfTestSpec("2015-01-05", "2021-12-31",
				[]string{"OLDCO", "LATE1", "LATE2", "LATE3"}, `
				function onDay(ctx) {
					if (!ctx.isFirstTradingDayOfMonth()) return;
					var have = ctx.universe().filter(function (s) { return ctx.hasData(s); });
					if (have.length) ctx.equalWeight(have, 0.95);
				}
			`),
			bars: survivorshipSeries,
		},
		{
			// Worse than holding the thing it trades, on both of the two
			// numbers anyone looks at. The benchmark is the same asset, so
			// there is nowhere for the comparison to hide.
			Name:     "beaten-by-buy-and-hold",
			Defect:   "churns an asset that holding would beat",
			Expect:   "beaten by the benchmark on both counts",
			Severity: SeverityCritical,
			spec: func() Spec {
				s := selfTestSpec("2016-01-04", "2021-12-31", []string{"SPY"}, `
					function onDay(ctx) {
						if (ctx.dayIndex % 2 === 0) ctx.buy("SPY", { pctCash: 0.98 });
						else ctx.liquidate();
					}
				`)
				s.Benchmarks = []string{"SPY"}
				return s
			}(),
		},
		{
			// A 2:1 split the vendor never adjusted for. Every return through
			// that date is arithmetic on two different units, so the strategy
			// is trading the defect rather than the market.
			Name:     "unadjusted-split",
			Defect:   "the price halves overnight and stays halved",
			Expect:   "the price data is not what it claims to be",
			Severity: SeverityCritical,
			spec: selfTestSpec("2021-01-04", "2023-03-31", []string{"SPLIT"}, `
				function onDay(ctx) {
					if (ctx.dayIndex === 0) ctx.buy("SPLIT", { pctCash: 0.9 });
				}
			`),
			bars: func() map[string]*market.Series {
				return map[string]*market.Series{"SPLIT": splitSeries()}
			},
		},
		{
			// The negative control: next-open fills, costs charged, a long
			// window, a diversified book and enough trades to measure. It is
			// not a good strategy and nothing here claims it is — only that
			// there is nothing critical to say about how it was measured.
			Name:    "clean-control",
			Defect:  "nothing: costs charged, next-open fills, 8 years",
			Control: true,
			spec: func() Spec {
				s := selfTestSpec("2014-01-02", "2021-12-31",
					[]string{"AAPL", "MSFT", "GOOGL", "AMZN", "XOM"}, `
					function onDay(ctx) {
						if (!ctx.isFirstTradingDayOfMonth()) return;
						ctx.equalWeight(ctx.universe(), 0.95);
					}
				`)
				s.Benchmarks = []string{"SPY"}
				return s
			}(),
		},
	}
}

// selfTestSpec is the shape every case shares, so that what differs between
// them is the defect and nothing else.
func selfTestSpec(from, to market.Day, universe []string, code string) Spec {
	return Spec{
		Name:            "selftest",
		Code:            code,
		Universe:        universe,
		Start:           from,
		End:             to,
		InitialCash:     100000,
		AllowFractional: true,
		Costs:           DefaultCosts(),
		Fill:            FillNextOpen,
		Warmup:          30,
	}
}

// --- hand-built price series ------------------------------------------------
//
// Three of the defects are properties of the data rather than of the code, and
// synthetic bars are deliberately well behaved. These build the shape each one
// needs, and nothing more.

// sessions lists the weekdays in [from, to]. The auditor reads a symbol with
// weekend bars as one the market calendar does not apply to at all, which is a
// different finding from the one these series are for.
func sessions(from, to market.Day) []market.Day {
	var out []market.Day
	for d := from; d <= to; d = d.Add(1) {
		if wd := d.Time().Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		out = append(out, d)
	}
	return out
}

// session builds one bar from its open and close. The high and low bracket
// both, so a large move reads as a day that traded the whole way there rather
// than as the overnight step an unadjusted split leaves behind.
func session(d market.Day, open, close float64) market.Bar {
	hi, lo := math.Max(open, close), math.Min(open, close)
	return market.Bar{
		Date: d, Open: open, High: hi * 1.001, Low: lo * 0.999,
		Close: close, AdjClose: close, Volume: 1e6,
	}
}

// dipSeries goes nowhere, in sharp V-shaped dips: a 6% fall recovered in full
// the next session. Every bar opens where it closes, so the difference between
// buying at the fall's close and buying at the next open is the whole of the
// move — which is exactly the difference a close-fill run helps itself to.
func dipSeries() *market.Series {
	days := sessions("2017-11-01", "2022-12-30")
	bars := make([]market.Bar, 0, len(days))
	price := 100.0
	for i, d := range days {
		switch {
		case i > 0 && i%40 == 0:
			price *= 0.94
		case i > 0 && i%40 == 1:
			price /= 0.94
		default:
			price *= 1.0002
		}
		bars = append(bars, session(d, price*0.9995, price))
	}
	return market.NewSeries("SAW", bars)
}

// burstSeries goes almost nowhere except on six days that carry the lot.
func burstSeries() *market.Series {
	days := sessions("2015-11-02", "2021-12-31")
	bars := make([]market.Bar, 0, len(days))
	price := 100.0
	for i, d := range days {
		open := price
		// A drift small enough that the average session contributes nothing,
		// and non-zero so the auditor never sees a frozen price.
		price *= 1.00002
		if i > 20 && i%250 == 7 {
			price *= 1.12
		}
		bars = append(bars, session(d, open, price))
	}
	return market.NewSeries("BURST", bars)
}

// premiumSeries pays a little most days and loses a lot occasionally, which is
// the return shape of a sold option however the strategy describes itself.
func premiumSeries() *market.Series {
	days := sessions("2015-11-02", "2021-12-31")
	bars := make([]market.Bar, 0, len(days))
	price := 100.0
	for i, d := range days {
		open := price
		if i > 0 && i%50 == 0 {
			price *= 0.91
		} else {
			price *= 1.0025
		}
		bars = append(bars, session(d, open, price))
	}
	return market.NewSeries("PREM", bars)
}

// splitSeries halves overnight and stays halved, with the day's range as tight
// as every other day's: the arithmetic signature of a corporate action the
// vendor did not adjust for.
func splitSeries() *market.Series {
	days := sessions("2020-11-02", "2023-03-31")
	bars := make([]market.Bar, 0, len(days))
	price := 400.0
	for i, d := range days {
		price *= 1.0005
		p := price
		if i >= 300 {
			p /= 2
		}
		bars = append(bars, session(d, p*0.999, p))
	}
	return market.NewSeries("SPLIT", bars)
}

// survivorshipSeries is a universe picked for what it is worth today: one name
// with a full history and three that had not listed when the test starts.
func survivorshipSeries() map[string]*market.Series {
	rise := func(symbol string, from market.Day, start, daily float64) *market.Series {
		days := sessions(from, "2021-12-31")
		bars := make([]market.Bar, 0, len(days))
		price := start
		for _, d := range days {
			open := price
			price *= daily
			bars = append(bars, session(d, open, price))
		}
		return market.NewSeries(symbol, bars)
	}
	return map[string]*market.Series{
		"OLDCO": rise("OLDCO", "2014-01-02", 50, 1.0002),
		"LATE1": rise("LATE1", "2016-03-01", 30, 1.0006),
		"LATE2": rise("LATE2", "2017-09-01", 80, 1.0007),
		"LATE3": rise("LATE3", "2019-02-01", 120, 1.0008),
	}
}
