package engine

import (
	"fmt"
	"strings"
	"time"
	"unicode"
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
	// CPCV is the combinatorial purged cross-validation. It is behind a flag
	// on the command line because it costs one backtest per combination per
	// group, which is the most expensive thing in the document.
	CPCV      *CPCVResult    `json:"-"`
	Costs     *CostScan      `json:"-"`
	Capacity  *Capacity      `json:"-"`
	Bootstrap BootstrapBands `json:"bootstrap"`
	// Factors is the decomposition against known risk premia. It is small
	// enough to travel in the JSON, unlike the four above.
	Factors *FactorExposure `json:"factors,omitempty"`
	// Scenarios is the strategy replayed through named historical crises.
	// Also small: thirteen rows of summary statistics, no curves.
	Scenarios *ScenarioReport `json:"scenarios,omitempty"`

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
	r.writeCrossValidation(&b)
	r.writeRobustness(&b)
	r.writeAttribution(&b)
	r.writeReasons(&b)
	r.writeDecay(&b)
	r.writeScenarios(&b)
	r.writeCosts(&b)
	r.writeCapacity(&b)
	r.writeFactors(&b)
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
	if r.CPCV != nil && r.CPCV.Verdict != "" {
		fmt.Fprintf(b, "Across every held-out combination: %s.\n\n", r.CPCV.Verdict)
	}
	if r.Sweep != nil && r.Sweep.Robustness.Verdict != "" {
		fmt.Fprintf(b, "Across the parameter space: %s.\n\n", r.Sweep.Robustness.Verdict)
	}
	if r.Scenarios != nil && r.Scenarios.Verdict != "" {
		fmt.Fprintf(b, "In the named crises: %s.\n\n", r.Scenarios.Verdict)
	}
	if r.Costs != nil && r.Costs.Verdict != "" {
		fmt.Fprintf(b, "On costs: %s.\n\n", r.Costs.Verdict)
	}
	if r.Capacity != nil && r.Capacity.Verdict != "" {
		fmt.Fprintf(b, "At size: %s.\n\n", r.Capacity.Verdict)
	}
	if r.Run != nil && r.Run.Decay.Verdict != "" {
		fmt.Fprintf(b, "On the holding period: %s.\n\n", r.Run.Decay.Verdict)
	}
	if r.Factors != nil && r.Factors.Verdict != "" {
		fmt.Fprintf(b, "Against known factors: %s.\n\n", r.Factors.Verdict)
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

// writeCrossValidation reports the spread of out-of-sample paths.
//
// The section above it gives one out-of-sample number. This one gives the
// range the same data supports, which is what says whether that number was a
// finding or a draw.
func (r *Report) writeCrossValidation(b *strings.Builder) {
	c := r.CPCV
	if c == nil || c.ValidPaths == 0 {
		return
	}
	b.WriteString("## Every combination held out\n\n")
	fmt.Fprintf(b, "The period was cut into %d groups and every combination of %d of them was "+
		"held out in turn, with %d sessions purged from training either side of each. That is "+
		"%d splits, which reassemble into %d full-length out-of-sample paths — none of them "+
		"fitted to, and all of them covering the same period.\n\n",
		c.Groups, c.TestGroups, c.Embargo, len(c.Splits), c.ValidPaths)

	b.WriteString("| Path | Return | Annualised | Sharpe | Max drawdown |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: |\n")
	for _, p := range c.Paths {
		if p.Error != "" {
			fmt.Fprintf(b, "| %d | %s | | | |\n", p.Index, p.Error)
			continue
		}
		fmt.Fprintf(b, "| %d | %s | %s | %s | %s |\n", p.Index,
			pctText(p.Metrics.TotalReturn), pctText(p.Metrics.CAGR),
			ratioText(p.Metrics.Sharpe), pctText(p.Metrics.MaxDrawdown))
	}
	b.WriteString("\n")

	b.WriteString("| Across the paths | Return | Annualised | Sharpe |\n")
	b.WriteString("| --- | ---: | ---: | ---: |\n")
	row := func(label string, get func(PathSpread) Ratio) {
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", label,
			pctOrNAText(get(c.Return)), pctOrNAText(get(c.CAGR)), ratioText(get(c.Sharpe)))
	}
	row("Mean", func(s PathSpread) Ratio { return s.Mean })
	row("Median", func(s PathSpread) Ratio { return s.Median })
	row("Standard deviation", func(s PathSpread) Ratio { return s.Stdev })
	row("5th percentile", func(s PathSpread) Ratio { return s.P05 })
	row("95th percentile", func(s PathSpread) Ratio { return s.P95 })
	row("Worst path", func(s PathSpread) Ratio { return s.Worst })
	fmt.Fprintf(b, "| Paths profitable | %d of %d | | |\n\n", c.ProfitablePaths, c.ValidPaths)

	fmt.Fprintf(b, "| | Value | |\n| --- | ---: | --- |\n")
	if c.NoSelection.Median.Defined() {
		fmt.Fprintf(b, "| Choosing nothing | %s | one configuration held over the same groups, "+
			"at the median of all %d; the difference from the paths above is all the selection "+
			"can claim |\n", pctOrNAText(c.NoSelection.Median), c.Combos)
	}
	if c.PBO.Defined() {
		fmt.Fprintf(b, "| Probability of backtest overfitting | %s | over %d purged splits; "+
			"50%% is a coin flip |\n", pctOrNAText(c.PBO), c.PBOSplits)
	}
	if c.BlockPBO.Defined() {
		fmt.Fprintf(b, "| ... the sweep's unpurged partition | %s | over %d splits, cut into "+
			"blocks with nothing withheld between them |\n", pctOrNAText(c.BlockPBO), c.BlockPBOSplits)
	}
	if wf := c.WalkForward; wf != nil {
		fmt.Fprintf(b, "| The walk-forward path, annualised | %s | one draw, at the %s of "+
			"these paths |\n", pctOrNAText(wf.CAGR), percentileText(wf.CAGRPercentile))
	}
	b.WriteString("\n")
}

// percentileText names a position in a distribution.
func percentileText(r Ratio) string {
	if !r.Defined() {
		return "an unknown percentile"
	}
	return fmt.Sprintf("%.0fth percentile", float64(r)*100)
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
	if rc := rb.RealityCheck; rc.RealityCheckP.Defined() {
		fmt.Fprintf(b, "| Reality check p-value | %s | White's, over %d stationary-bootstrap "+
			"resamples of all %d trials |\n",
			FormatPValue(float64(rc.RealityCheckP), rc.Bootstraps), rc.Bootstraps, rc.Trials)
		fmt.Fprintf(b, "| Hansen SPA p-value | %s | the studentised version, which the search's "+
			"dead cells do not drag down |\n", FormatPValue(float64(rc.SPAP), rc.Bootstraps))
	}
	if ns := rb.NullStrategy; ns.Percentile.Defined() {
		fmt.Fprintf(b, "| Beats random entries | %s | against %d random strategies with the same "+
			"%d holds, holding periods and %s exposure |\n",
			pctText(float64(ns.Percentile)), ns.Trials, ns.Episodes, pctText(ns.AvgExposure))
		fmt.Fprintf(b, "| Random entries, median | %s | what arbitrary timing with those habits "+
			"scored, against %s for the strategy |\n",
			ratioText(ns.NullMedian), ratioText(ns.Score))
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

// writeReasons attributes the realised P&L to the rules that produced it.
//
// It follows the symbol table because it is the same decomposition asked of
// something the author controls. A holding cannot be removed from a strategy;
// the condition that bought it can.
func (r *Report) writeReasons(b *strings.Builder) {
	if r.Run == nil {
		return
	}
	tbl := r.Run.Reasons.Table()
	if len(tbl.ByEntry) == 0 && len(tbl.ByExit) == 0 {
		return
	}
	b.WriteString("## Which rules made the money\n\n")
	// A run whose orders never said why gets the sentence rather than two
	// tables of one row each, because saying the section is empty and why is
	// more use than a table with nothing to compare in it.
	if tbl.Unattributed() {
		b.WriteString("No order in this run gave a reason, so there is nothing to attribute. " +
			"Pass one to fill this in: `ctx.buy(sym, { ... }, \"why\")`.\n\n")
		return
	}
	fmt.Fprintf(b, "Reasons are grouped %s.\n\n", tbl.Grouping)
	writeReasonTable(b, "Entry rule", tbl.ByEntry)
	writeReasonTable(b, "Exit rule", tbl.ByExit)
	fmt.Fprintf(b, "Every closed round trip appears once in each table, so both sum to %s.\n\n",
		money(SumNetPnL(tbl.ByEntry)))
}

func writeReasonTable(b *strings.Builder, header string, rows []ReasonStats) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(b, "| %s | Net P&L | Share | Trades | Win rate | Mean days | Mean MAE | Mean MFE |\n", header)
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	write := func(s ReasonStats) {
		// A reason is free text written by the strategy author, and one
		// pipe in it would silently break the rest of the table.
		label := strings.ReplaceAll(s.Reason, "|", "\\|")
		fmt.Fprintf(b, "| %s | %s | %s | %d | %s | %.0f | %s | %s |\n", label,
			money(s.NetPnL), pctOrNAText(s.Share), s.Trades, pctText(s.WinRate),
			s.MeanDaysHeld, pctText(s.MeanMAEPct), pctText(s.MeanMFEPct))
	}
	head, dropped, tail := TopAndBottom(rows, 12)
	for _, s := range head {
		write(s)
	}
	if dropped > 0 {
		fmt.Fprintf(b, "| *... %d more rules between* | | | | | | | |\n", dropped)
	}
	for _, s := range tail {
		write(s)
	}
	b.WriteString("\n")
}

// writeScenarios reports the strategy through named historical crises.
//
// It sits after the calendar attribution because it answers the question that
// section raises and cannot settle: a year is an arbitrary slice, and "2020
// was fine" is not an answer to "what happened in March".
func (r *Report) writeScenarios(b *strings.Builder) {
	s := r.Scenarios
	if s == nil || len(s.Runs) == 0 {
		return
	}
	b.WriteString("## What happened in each crisis\n\n")
	fmt.Fprintf(b, "Each window was run on its own, preceded by %d bars of indicator "+
		"warm-up and %d sessions of ordinary trading, so the strategy enters the window "+
		"holding whatever it would have been holding rather than starting in cash. Only "+
		"the window itself is measured.\n\n", s.Warmup, s.LeadIn)
	b.WriteString("The last column is the S&P 500's own peak-to-trough decline inside the " +
		"window, from published closes. It is fixed reference data rather than something " +
		"measured here, and the column to compare it against is the benchmark's drawdown " +
		"beside it: a large gap between those two says the price data is wrong, not that " +
		"the strategy is. The benchmark return is the whole window's return, which differs " +
		"from a drawdown wherever the trough was not the last session.\n\n")

	b.WriteString("| Crisis | Window | Return | Drawdown | Benchmark | Bench. drawdown | Excess | Index then |\n")
	b.WriteString("| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	type skip struct {
		reason string
		names  []string
	}
	var skipped []skip
	for _, run := range s.Runs {
		window := fmt.Sprintf("%s to %s", run.Start, run.End)
		if !run.Measured() {
			// The reason goes below the table rather than into a numeric
			// column, but the row stays: a table of the windows this data
			// happens to reach must not read as a table of every crisis.
			reason := run.SkipReason
			if reason == "" {
				reason = "the run failed: " + run.Error
			}
			found := false
			for i := range skipped {
				if skipped[i].reason == reason {
					skipped[i].names = append(skipped[i].names, run.Name)
					found = true
				}
			}
			if !found {
				skipped = append(skipped, skip{reason, []string{run.Name}})
			}
			fmt.Fprintf(b, "| %s | %s | not measured | | | | | %s |\n",
				run.Name, window, pctText(run.IndexDrawdown))
			continue
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n",
			run.Name, window, pctOrNAText(run.Return), pctOrNAText(run.MaxDrawdown),
			pctOrNAText(run.BenchmarkReturn), pctOrNAText(run.BenchmarkDrawdown),
			pctOrNAText(run.Excess), pctText(run.IndexDrawdown))
	}
	fmt.Fprintf(b, "\n%d of %d windows were measured; the strategy's data runs from %s to %s.\n\n",
		s.Covered, len(s.Runs), s.DataFrom, s.DataTo)

	for _, sk := range skipped {
		fmt.Fprintf(b, "- Not measured — %s: %s.\n", strings.Join(sk.names, ", "), sk.reason)
	}
	if len(skipped) > 0 {
		b.WriteString("\n")
	}

	for _, f := range s.Findings {
		fmt.Fprintf(b, "- **%s** — %s\n", f.Title, f.Detail)
	}
	if len(s.Findings) > 0 {
		b.WriteString("\n")
	}
	if s.Verdict != "" {
		fmt.Fprintf(b, "**%s.**\n\n", upperFirst(s.Verdict))
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

// writeFactors reports what the strategy earned that its exposures do not
// account for.
//
// It sits after the cost section because it asks the same shape of question:
// not how large the return was, but how much of it belongs to the strategy
// rather than to something already available for the price of an index fund.
func (r *Report) writeFactors(b *strings.Builder) {
	f := r.Factors
	if f == nil || len(f.Factors) == 0 {
		return
	}
	b.WriteString("## Is any of this alpha?\n\n")
	b.WriteString("The strategy's excess returns, regressed on tradable proxies for the " +
		"factors that already explain most cross-sectional equity returns. A loading " +
		"says the strategy is taking a risk somebody has already named and priced. " +
		"The intercept is what is left over, and its t-statistic is whether that " +
		"remainder can be told apart from zero.\n\n")

	b.WriteString("| Factor | Proxy | Beta | Std error | t-stat |\n")
	b.WriteString("| --- | --- | ---: | ---: | ---: |\n")
	for _, l := range f.Factors {
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n",
			l.Name, l.Proxy, ratioText(l.Beta), ratioText(l.StdErr), ratioText(l.TStat))
	}
	fmt.Fprintf(b, "| **Alpha, annualised** | | **%s** | %s | **%s** |\n\n",
		pctOrNAText(f.Alpha), pctOrNAText(f.AlphaStdErr), ratioText(f.AlphaTStat))

	fmt.Fprintf(b, "R² %s (adjusted %s) over %d observations, with Newey-West standard "+
		"errors at a lag of %d bars to allow for the autocorrelation in daily strategy "+
		"returns.", ratioText(f.RSquared), ratioText(f.AdjRSquared),
		f.Observations, f.NeweyWestLag)
	if f.AvgExposure.Defined() {
		// The market beta is not readable without this: a beta well under
		// the average exposure means the strategy was flat during the
		// volatile stretches, not that it found something uncorrelated.
		fmt.Fprintf(b, " Average gross exposure over the same bars was %s, which is what "+
			"the market beta should be read against.", ratioText(f.AvgExposure))
	}
	b.WriteString("\n\n")

	if len(f.Dropped) > 0 {
		b.WriteString("Not measured over this period:\n\n")
		for _, d := range f.Dropped {
			fmt.Fprintf(b, "- %s (%s): %s.\n", d.Name, d.Proxy, d.Reason)
		}
		b.WriteString("\n")
	}
	if f.Verdict != "" {
		fmt.Fprintf(b, "**%s.**\n\n", upperFirst(f.Verdict))
	}
	fmt.Fprintf(b, "%s\n\n", f.ProxyNote)
}

// writeCapacity reports what the strategy is worth at each account size.
//
// It sits beside the cost scan because it is the same question with the other
// variable moved: the scan charges a friction level somebody chose, and this
// charges the friction the strategy causes itself by being large.
func (r *Report) writeCapacity(b *strings.Builder) {
	c := r.Capacity
	if c == nil || len(c.Points) == 0 {
		return
	}
	b.WriteString("## How much money can this take\n\n")
	fmt.Fprintf(b, "The same strategy run at each account size with the square-root "+
		"impact model enabled at k = %.2f, so a larger order pays a larger price "+
		"concession for the liquidity it demands. Friction is shown per dollar "+
		"traded, which is the form that isolates what the ladder is testing: the "+
		"dollar total rises with the account whatever happens.\n\n",
		c.ImpactCoefficient)

	b.WriteString("| Capital | Return | Annualised | Sharpe | Max drawdown | Friction |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: |\n")
	for _, p := range c.Points {
		if p.Error != "" {
			fmt.Fprintf(b, "| %s | failed | | | | |\n", money(p.Capital))
			continue
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s |\n", money(p.Capital),
			pctText(p.TotalReturn), pctText(p.CAGR), ratioText(p.Sharpe),
			pctText(p.MaxDrawdown), fmt.Sprintf("%.0f bps", p.CostBps))
	}
	b.WriteString("\n")

	if c.ZeroReturnCapital.Defined() || c.BenchmarkCapital.Defined() {
		if c.ZeroReturnCapital.Defined() {
			fmt.Fprintf(b, "Largest account still above zero: **%s**.\n",
				money(float64(c.ZeroReturnCapital)))
		}
		if c.BenchmarkCapital.Defined() {
			fmt.Fprintf(b, "Largest account still beating %s: **%s**.\n",
				c.BenchmarkLabel, money(float64(c.BenchmarkCapital)))
		}
		b.WriteString("\nBoth are interpolated between ladder rungs a factor of ten " +
			"apart, so they are estimates of an order of magnitude rather than " +
			"measured limits.\n\n")
	}
	if c.Verdict != "" {
		fmt.Fprintf(b, "**%s.**\n\n", upperFirst(c.Verdict))
	}
}

// writeDecay reports when the average trade's edge arrived and when it went.
func (r *Report) writeDecay(b *strings.Builder) {
	if r.Run == nil {
		return
	}
	d := r.Run.Decay
	if len(d.Points) == 0 {
		return
	}
	b.WriteString("## When the edge arrives, and when it goes\n\n")
	b.WriteString("Each closed round trip's cumulative return from its entry price, " +
		"averaged across trades at fixed horizons. Returns are gross, signed by " +
		"direction, on the same basis as the excursion statistics above. A trade " +
		"shorter than a horizon contributes the return it finished with rather than " +
		"a zero, so the sample is the same at every row and the columns are " +
		"comparable; the last column says how many were still open.\n\n")

	b.WriteString("| Bars after entry | Mean cumulative return | Still open |\n")
	b.WriteString("| --- | ---: | ---: |\n")
	for _, p := range d.Points {
		fmt.Fprintf(b, "| %d | %s | %d of %d |\n", p.Bars,
			pctText(p.MeanReturn), p.StillOpen, d.Trades)
	}
	fmt.Fprintf(b, "\nThe curve peaks at **%s after entry**, at %s. The average trade "+
		"is held for %.1f bars and finishes at %s.",
		plural(d.PeakBars, "bar"), pctOrNAText(d.PeakReturn),
		d.MeanBarsHeld, pctOrNAText(d.ExitReturn))
	if d.GivenBack.Defined() {
		fmt.Fprintf(b, " That is %s of the peak handed back by the exit.",
			pctText(float64(d.GivenBack)))
	}
	b.WriteString("\n\n")
	if d.Verdict != "" {
		fmt.Fprintf(b, "**%s.**\n\n", upperFirst(d.Verdict))
	}
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
	fmt.Fprintf(b, "| Annualised on | the %s calendar, %s bars a year |\n",
		m.TradingCalendar.Label(), trimFloat(m.PeriodsPerYear))
	fmt.Fprintf(b, "| Costs | %s |\n", costsText(m.Costs, m.BorrowNames))
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

// upperFirst capitalises a verdict that has to open a paragraph.
//
// The verdicts are written to follow a lead-in — "On costs: ", "Against known
// factors: " — so they start lowercase everywhere else on purpose.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return string(unicode.ToUpper(r[0])) + string(r[1:])
}

// pctOrNAText renders a percentage that may legitimately be undefined.
func pctOrNAText(r Ratio) string {
	if !r.Defined() {
		return "n/a"
	}
	return pctText(float64(r))
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

// costsText states the friction model in one line, and says whether the short
// borrow was one rate for everything or priced per name. The count comes from
// the manifest rather than the schedule, which is hashed rather than carried.
func costsText(c Costs, borrowNames int) string {
	out := fmt.Sprintf("%.0f bps slippage, %.2f%% commission, %.1f%% short borrow",
		c.SlippageBps, c.CommissionPct*100, c.ShortBorrowAnnualPct*100)
	if borrowNames > 0 {
		return out + fmt.Sprintf(" (%s priced separately)", plural(borrowNames, "name"))
	}
	return out + " on every name"
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
