package engine

import (
	"fmt"
	"strings"
	"time"
)

// Report is everything known about one strategy, assembled into a document.
//
// The point is that a backtest result is not the deliverable. Somebody has to
// decide whether to act on it, and that decision needs the equity curve, the
// out-of-sample evidence, the robustness statistics, the cost sensitivity and
// the specific objections — together, in one place, with the caveats attached
// to the numbers they qualify rather than in a footnote nobody reads.
type Report struct {
	Title     string    `json:"title"`
	Prompt    string    `json:"prompt,omitempty"`
	Generated time.Time `json:"generated"`

	// Run is the full-period backtest.
	Run *Result `json:"-"`
	// Sweep, WalkForward, Costs and Bootstrap are each optional: the report
	// renders what it was given and says what is missing rather than
	// pretending the question was not asked.
	Sweep       *SweepResult       `json:"-"`
	WalkForward *WalkForwardResult `json:"-"`
	Costs       *CostScan          `json:"-"`
	Bootstrap   BootstrapBands     `json:"bootstrap"`

	// Narrative is a model's prose summary. Everything else in the document
	// stands without it.
	Narrative string `json:"narrative,omitempty"`
	// Assumptions and Limitations come from the compiler.
	Assumptions []string `json:"assumptions,omitempty"`
	Limitations []string `json:"limitations,omitempty"`
}

// Markdown renders the report.
func (r *Report) Markdown() string {
	var b strings.Builder
	title := r.Title
	if title == "" {
		title = "Strategy report"
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	if r.Prompt != "" {
		fmt.Fprintf(&b, "> %s\n\n", r.Prompt)
	}
	fmt.Fprintf(&b, "*Generated %s by pyrite.*\n\n",
		r.Generated.UTC().Format("2 January 2006"))

	if r.Narrative != "" {
		b.WriteString(r.Narrative)
		b.WriteString("\n\n")
	}

	r.writeVerdict(&b)
	r.writeHeadline(&b)
	r.writeOutOfSample(&b)
	r.writeRobustness(&b)
	r.writeAttribution(&b)
	r.writeCosts(&b)
	r.writeBootstrap(&b)
	r.writeObjections(&b)
	r.writeMechanics(&b)
	r.writeCode(&b)
	return b.String()
}

// writeVerdict leads with the conclusion, because a reader who stops after
// one section should stop having read the important part.
func (r *Report) writeVerdict(b *strings.Builder) {
	if r.Run == nil {
		return
	}
	b.WriteString("## Verdict\n\n")

	c := r.Run.Critique
	fmt.Fprintf(b, "**How much should you believe this: %d/100.**", c.TrustScore)
	if c.Headline != "" {
		fmt.Fprintf(b, " The largest single objection is that %s.", c.Headline)
	}
	b.WriteString("\n\n")

	if r.WalkForward != nil && r.WalkForward.Verdict != "" {
		fmt.Fprintf(b, "Out of sample: %s.\n\n", r.WalkForward.Verdict)
	}
	if r.Sweep != nil && r.Sweep.Robustness.Verdict != "" {
		fmt.Fprintf(b, "Across the parameter space: %s.\n\n", r.Sweep.Robustness.Verdict)
	}
	if r.Costs != nil && r.Costs.Verdict != "" {
		fmt.Fprintf(b, "On costs: %s.\n\n", r.Costs.Verdict)
	}
}

func (r *Report) writeHeadline(b *strings.Builder) {
	if r.Run == nil {
		return
	}
	m := r.Run.Metrics
	fmt.Fprintf(b, "## Results, %s to %s\n\n", r.Run.Spec.Start, r.Run.Spec.End)
	b.WriteString("| | Strategy |")
	for _, bm := range r.Run.Benchmarks {
		fmt.Fprintf(b, " %s |", bm.Label)
	}
	b.WriteString("\n| --- | ---: |")
	for range r.Run.Benchmarks {
		b.WriteString(" ---: |")
	}
	b.WriteString("\n")

	row := func(label string, get func(Metrics) string) {
		fmt.Fprintf(b, "| %s | %s |", label, get(m))
		for _, bm := range r.Run.Benchmarks {
			fmt.Fprintf(b, " %s |", get(bm.Metric))
		}
		b.WriteString("\n")
	}
	row("Total return", func(m Metrics) string { return pctText(m.TotalReturn) })
	row("Annualised", func(m Metrics) string { return pctText(m.CAGR) })
	row("Volatility", func(m Metrics) string { return pctText(m.Volatility) })
	row("Sharpe", func(m Metrics) string { return ratioText(m.Sharpe) })
	row("Sortino", func(m Metrics) string { return ratioText(m.Sortino) })
	row("Max drawdown", func(m Metrics) string { return pctText(m.MaxDrawdown) })

	risk := r.Run.Risk
	fmt.Fprintf(b, "\nReturn distribution: skew %.2f, excess kurtosis %.1f, "+
		"ulcer index %s, daily CVaR at 95%% %s.\n\n",
		risk.Skew, risk.ExcessKurtosis, pctText(risk.UlcerIndex), pctText(risk.CVaR95))

	t := r.Run.TradeStats
	if t.Closed > 0 {
		fmt.Fprintf(b, "%d closed round trips, %s win rate, payoff ratio %s, "+
			"expectancy %s per trade, average hold %.0f bars.\n\n",
			t.Closed, pctText(t.WinRate), ratioText(t.PayoffRatio),
			money(t.Expectancy), t.AvgBarsHeld)
	}
}

func (r *Report) writeOutOfSample(b *strings.Builder) {
	wf := r.WalkForward
	if wf == nil || len(wf.Folds) == 0 {
		return
	}
	b.WriteString("## Out of sample\n\n")
	b.WriteString("Parameters were chosen on each training window and applied unchanged " +
		"to the window that followed it. The curve below is the only one in this " +
		"document that was never fitted to.\n\n")

	m := wf.StitchedMetrics
	fmt.Fprintf(b, "| | Value |\n| --- | ---: |\n")
	fmt.Fprintf(b, "| Out-of-sample total return | %s |\n", pctText(m.TotalReturn))
	fmt.Fprintf(b, "| Out-of-sample annualised | %s |\n", pctText(m.CAGR))
	fmt.Fprintf(b, "| Out-of-sample Sharpe | %s |\n", ratioText(m.Sharpe))
	fmt.Fprintf(b, "| Out-of-sample max drawdown | %s |\n", pctText(m.MaxDrawdown))
	fmt.Fprintf(b, "| Mean in-sample return per fold | %s |\n", pctText(wf.InSampleReturn))
	fmt.Fprintf(b, "| Mean out-of-sample return per fold | %s |\n", pctText(wf.OutOfSampleMean))
	if wf.Efficiency.Defined() {
		fmt.Fprintf(b, "| Walk-forward efficiency | %s |\n", pctText(float64(wf.Efficiency)))
	}
	fmt.Fprintf(b, "| Positive test windows | %d of %d |\n", wf.ConsistentFolds, len(wf.Folds))
	fmt.Fprintf(b, "| Parameter stability | %s |\n\n", pctText(wf.ParamStability))
}

func (r *Report) writeRobustness(b *strings.Builder) {
	sw := r.Sweep
	if sw == nil {
		return
	}
	rb := sw.Robustness
	b.WriteString("## How much of this is the search?\n\n")
	fmt.Fprintf(b, "%d configurations were tried.\n\n", rb.Trials)

	fmt.Fprintf(b, "| | Value | |\n| --- | ---: | --- |\n")
	fmt.Fprintf(b, "| Best %s | %.3f | |\n", sw.Objective, rb.BestScore)
	fmt.Fprintf(b, "| Median %s | %.3f | |\n", sw.Objective, rb.MedianScore)
	if rb.ExpectedMaxScore != 0 {
		fmt.Fprintf(b, "| Expected best from luck alone | %.3f | what the top of %d trials "+
			"scores with no skill at all |\n", rb.ExpectedMaxScore, rb.Trials)
	}
	fmt.Fprintf(b, "| Configurations above zero | %s | |\n", pctText(rb.PositiveShare))
	if rb.PlateauRatio.Defined() {
		fmt.Fprintf(b, "| Neighbour support | %s | how the cells beside the winner scored |\n",
			pctText(float64(rb.PlateauRatio)))
	}
	if rb.PBO.Defined() {
		fmt.Fprintf(b, "| Probability of backtest overfitting | %s | over %d train/test "+
			"splits; 50%% is a coin flip |\n", pctText(float64(rb.PBO)), rb.PBOSplits)
	}
	if rb.DeflatedSharpe.Defined() {
		fmt.Fprintf(b, "| Deflated Sharpe | %s | confidence the edge survives the trial count |\n",
			pctText(float64(rb.DeflatedSharpe)))
	}
	b.WriteString("\n")
}

func (r *Report) writeAttribution(b *strings.Builder) {
	if r.Run == nil {
		return
	}
	a := r.Run.Attribution
	if len(a.ByYear) < 2 {
		return
	}
	b.WriteString("## Where the return came from\n\n")

	b.WriteString("| Year | Return | Benchmark | Excess | Drawdown |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: |\n")
	for _, y := range a.ByYear {
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", y.Label,
			pctText(y.Return), pctText(y.BenchmarkReturn), pctText(y.Excess), pctText(y.MaxDrawdown))
	}
	b.WriteString("\n")

	if len(a.ByRegime) > 0 {
		b.WriteString("| Market regime | Return | Drawdown | Sessions |\n")
		b.WriteString("| --- | ---: | ---: | ---: |\n")
		for _, g := range a.ByRegime {
			fmt.Fprintf(b, "| %s | %s | %s | %d |\n", g.Label,
				pctText(g.Return), pctText(g.MaxDrawdown), g.TradingDays)
		}
		b.WriteString("\n")
	}

	if len(a.Stress) > 0 {
		b.WriteString("**How concentrated is the edge?**\n\n")
		for _, s := range a.Stress {
			fmt.Fprintf(b, "- %s: %s, which is %s of the total gain.\n",
				s.Label, pctText(s.Return), pctText(s.ShareOfTotal))
		}
		b.WriteString("\n")
	}

	if len(a.BySymbol) > 1 {
		b.WriteString("| Holding | Net P&L | Share | Trades | Win rate |\n")
		b.WriteString("| --- | ---: | ---: | ---: | ---: |\n")
		n := len(a.BySymbol)
		if n > 10 {
			n = 10
		}
		for _, s := range a.BySymbol[:n] {
			fmt.Fprintf(b, "| %s | %s | %s | %d | %s |\n", s.Symbol,
				money(s.NetPnL), pctText(s.Contribution), s.Trades, pctText(s.WinRate))
		}
		b.WriteString("\n")
	}
}

func (r *Report) writeCosts(b *strings.Builder) {
	c := r.Costs
	if c == nil || len(c.Points) == 0 {
		return
	}
	b.WriteString("## How much survives friction\n\n")
	b.WriteString("| Slippage | Return | Annualised | Sharpe | Costs paid |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: |\n")
	for _, p := range c.Points {
		if p.Error != "" {
			fmt.Fprintf(b, "| %.0f bps | failed | | | |\n", p.SlippageBps)
			continue
		}
		fmt.Fprintf(b, "| %.0f bps | %s | %s | %s | %s |\n", p.SlippageBps,
			pctText(p.TotalReturn), pctText(p.CAGR), ratioText(p.Sharpe), money(p.TotalCosts))
	}
	if c.BreakEvenBps.Defined() {
		fmt.Fprintf(b, "\nBreak-even slippage: **%.1f bps**.\n", float64(c.BreakEvenBps))
	}
	b.WriteString("\n")
}

func (r *Report) writeBootstrap(b *strings.Builder) {
	bs := r.Bootstrap
	if bs.Trials == 0 {
		return
	}
	b.WriteString("## One path is not the distribution\n\n")
	fmt.Fprintf(b, "The backtest produced one sequence of returns. Resampling it in "+
		"blocks %d times — long enough to preserve the volatility clustering that "+
		"makes drawdowns what they are — gives the range of outcomes the same "+
		"process could plausibly have produced.\n\n", bs.Trials)

	fmt.Fprintf(b, "| | Value |\n| --- | ---: |\n")
	fmt.Fprintf(b, "| Total return, 5th percentile | %s |\n", pctText(bs.ReturnP05))
	fmt.Fprintf(b, "| Total return, median | %s |\n", pctText(bs.ReturnMedian))
	fmt.Fprintf(b, "| Total return, 95th percentile | %s |\n", pctText(bs.ReturnP95))
	fmt.Fprintf(b, "| Max drawdown, median | %s |\n", pctText(bs.DrawdownMedian))
	fmt.Fprintf(b, "| Max drawdown, 5th percentile | %s |\n", pctText(bs.DrawdownP05))
	fmt.Fprintf(b, "| Chance of finishing down | %s |\n\n", pctText(bs.LossProbability))
	fmt.Fprintf(b, "The fifth-percentile drawdown is the number to plan around, not "+
		"the one that happened to occur.\n\n")
}

func (r *Report) writeObjections(b *strings.Builder) {
	if r.Run == nil || len(r.Run.Critique.Findings) == 0 {
		return
	}
	b.WriteString("## Objections\n\n")
	for _, f := range r.Run.Critique.Findings {
		fmt.Fprintf(b, "**%s — %s**\n\n%s\n\n",
			strings.ToUpper(string(f.Severity)), f.Title, f.Detail)
	}
}

func (r *Report) writeMechanics(b *strings.Builder) {
	if r.Run == nil {
		return
	}
	m := r.Run.Manifest
	b.WriteString("## How this was produced\n\n")
	fmt.Fprintf(b, "| | |\n| --- | --- |\n")
	fmt.Fprintf(b, "| Data | %s, %s, %d sessions |\n",
		m.DataProvider, plural(len(m.Coverage), "symbol"), m.CalendarDays)
	fmt.Fprintf(b, "| Fills | %s |\n", fillText(m.Fill))
	fmt.Fprintf(b, "| Costs | %.0f bps slippage, %.2f%% commission, %.1f%% short borrow |\n",
		m.Costs.SlippageBps, m.Costs.CommissionPct*100, m.Costs.ShortBorrowAnnualPct*100)
	fmt.Fprintf(b, "| Starting capital | %s |\n", money(m.InitialCash))
	fmt.Fprintf(b, "| Code hash | `%s` |\n", shortHash(m.CodeSHA256))
	fmt.Fprintf(b, "| Build | %s, %s |\n", m.Version, m.GoVersion)
	if m.AICallCount > 0 {
		fmt.Fprintf(b, "| Model calls inside the backtest | %d (%d cached) |\n",
			m.AICallCount, m.AICacheHits)
	}
	fmt.Fprintf(b, "| Exactly reproducible | %v |\n\n", m.Reproducible())

	if len(r.Assumptions) > 0 {
		b.WriteString("**Assumptions made where the request was open:**\n\n")
		for _, a := range r.Assumptions {
			fmt.Fprintf(b, "- %s\n", a)
		}
		b.WriteString("\n")
	}
	if len(r.Limitations) > 0 {
		b.WriteString("**Stated limitations:**\n\n")
		for _, l := range r.Limitations {
			fmt.Fprintf(b, "- %s\n", l)
		}
		b.WriteString("\n")
	}

	b.WriteString("Past performance says very little about future returns, and an " +
		"overfitted backtest says nothing at all. The statistics above exist to " +
		"put a number on how much of this result is the second case.\n\n")
}

func (r *Report) writeCode(b *strings.Builder) {
	if r.Run == nil || r.Run.Spec.Code == "" {
		return
	}
	b.WriteString("## The strategy\n\n```javascript\n")
	b.WriteString(strings.TrimSpace(r.Run.Spec.Code))
	b.WriteString("\n```\n")
}

func pctText(v float64) string {
	return fmt.Sprintf("%.2f%%", v*100)
}

func ratioText(r Ratio) string {
	if !r.Defined() {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", float64(r))
}

func fillText(f FillModel) string {
	if f == FillClose {
		return "same-day close (optimistic: the strategy trades at a price it has already seen)"
	}
	return "next session's open"
}

// plural renders a count with its noun, singular when the count is one.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
