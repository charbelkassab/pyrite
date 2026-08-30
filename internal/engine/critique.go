package engine

import (
	"fmt"
	"math"
	"sort"
)

// Severity ranks how much a finding should change the reader's mind.
type Severity string

const (
	// SeverityCritical means the headline number does not mean what it
	// appears to mean.
	SeverityCritical Severity = "critical"
	// SeverityWarning means a real weakness that survives explanation.
	SeverityWarning Severity = "warning"
	// SeverityNote is context worth having.
	SeverityNote Severity = "note"
)

// Finding is one specific, evidenced criticism of a result.
type Finding struct {
	Severity Severity `json:"severity"`
	// Title is the claim, in a few words.
	Title string `json:"title"`
	// Detail states the evidence and what it implies, with the numbers in it.
	// A criticism without its number is an opinion.
	Detail string `json:"detail"`
}

// Critique is the deterministic assessment of a single run.
//
// Every finding here is computed from the result, not asked of a model. That
// matters for three reasons: it works with no API key, it costs nothing, and
// it cannot hallucinate a number. A model is a good writer and an unreliable
// arithmetician; this does the arithmetic and leaves the prose to it.
type Critique struct {
	Findings []Finding `json:"findings"`
	// Headline is the single most important thing to say, deterministically
	// chosen as the most severe finding.
	Headline string `json:"headline"`
	// TrustScore is a coarse 0-100 summary. It is deliberately coarse: the
	// findings are the product, and a single number invites exactly the
	// false precision this whole section exists to argue against.
	TrustScore int `json:"trust_score"`
}

// Criticise assesses a completed run.
func Criticise(res *Result) Critique {
	var c Critique
	if res == nil || len(res.Curve) < 2 {
		return c
	}
	add := func(sev Severity, title, format string, args ...any) {
		c.Findings = append(c.Findings, Finding{sev, title, fmt.Sprintf(format, args...)})
	}
	// Set when the run produced nothing to assess, which floors the score
	// rather than discounting it.
	noResult := false
	m := res.Metrics

	// --- Is there enough evidence to be talking about this at all? -------

	if n := res.TradeStats.Closed; n > 0 && n < 20 {
		add(SeverityCritical, "too few trades to mean anything",
			"%d closed round trips. A win rate or a Sharpe over this many trades is "+
				"noise: one different outcome moves every statistic here materially.", n)
	} else if n == 0 && len(res.Fills) == 0 {
		add(SeverityCritical, "the strategy never traded",
			"No orders were filled across %d sessions. Usually the warm-up exceeds the "+
				"data, or an entry condition is never satisfied.", m.TradingDays)
		// This one is disqualifying rather than merely bad. Every other
		// finding reduces confidence in a result; this one says there is no
		// result to have confidence in, and subtracting a fixed penalty from
		// 100 would score an empty run above a real one with two flaws.
		noResult = true
	}
	if m.Years < 2 && m.Years > 0 {
		add(SeverityWarning, "short test period",
			"%.1f years is not long enough to have seen more than one market regime.", m.Years)
	}
	if res.StrategyErrors > 0 {
		pctDays := float64(res.StrategyErrors) / math.Max(1, float64(m.TradingDays)) * 100
		sev := SeverityWarning
		if pctDays > 5 {
			sev = SeverityCritical
		}
		add(sev, "the strategy threw on some days",
			"onDay() failed on %d of %d sessions (%.1f%%). Those days did nothing, so the "+
				"result is of a strategy that was partly absent.", res.StrategyErrors, m.TradingDays, pctDays)
	}

	// --- Where did the return actually come from? ------------------------

	// Only the strongest concentration finding is reported. Both stress
	// tests fire on the same underlying fact, and two identically titled
	// findings read as a bug rather than as emphasis.
	var worstStress *StressResult
	for i := range res.Attribution.Stress {
		if res.Attribution.Stress[i].ShareOfTotal <= 0.4 {
			continue
		}
		if worstStress == nil || res.Attribution.Stress[i].ShareOfTotal > worstStress.ShareOfTotal {
			worstStress = &res.Attribution.Stress[i]
		}
	}
	if worstStress != nil {
		sev := SeverityWarning
		if worstStress.ShareOfTotal > 0.6 {
			sev = SeverityCritical
		}
		add(sev, "the return is concentrated in a few sessions",
			"%.0f%% of the total gain disappears when %s. Whatever this strategy is, "+
				"it is a bet on those episodes recurring.",
			worstStress.ShareOfTotal*100, worstStress.Label)
	}
	if len(res.Attribution.ByYear) >= 3 {
		var positive, negative int
		worst := res.Attribution.ByYear[0]
		for _, y := range res.Attribution.ByYear {
			switch {
			case y.Return > 0:
				positive++
			case y.Return < 0:
				negative++
			}
			if y.Return < worst.Return {
				worst = y
			}
		}
		total := len(res.Attribution.ByYear)
		// Counting "not positive" as "lost money" makes a flat year a losing
		// one, so a strategy that never traded gets told most of its years
		// lost money when none of them did.
		if negative > 0 && float64(positive)/float64(total) < 0.5 {
			add(SeverityWarning, "most years lost money",
				"%d of %d calendar years finished negative and only %d finished "+
					"positive. A good total return built from a minority of good "+
					"years is a timing bet.", negative, total, positive)
		}
		if worst.Return < -0.25 {
			add(SeverityNote, "one very bad year",
				"%s returned %.1f%%. Ask whether the position would have been held through it.",
				worst.Label, worst.Return*100)
		}
	}
	if len(res.Attribution.BySymbol) > 2 {
		top := res.Attribution.BySymbol[0]
		if top.Contribution > 0.5 {
			add(SeverityWarning, "one holding carries the result",
				"%s accounts for %.0f%% of realised P&L across %d symbols. This is a "+
					"single-name bet wearing a portfolio's clothes.",
				top.Symbol, top.Contribution*100, len(res.Attribution.BySymbol))
		}
	}

	// --- What does the return distribution look like? --------------------

	r := res.Risk
	if r.Skew < -0.5 && r.ExcessKurtosis > 3 {
		add(SeverityCritical, "this is short volatility in disguise",
			"Returns are left-skewed (%.2f) with fat tails (excess kurtosis %.1f): many "+
				"small gains and occasional large losses. Sharpe flatters this shape badly, "+
				"because the risk it measures is not the risk being taken.", r.Skew, r.ExcessKurtosis)
	}
	if r.CVaR95 < -0.05 {
		add(SeverityNote, "heavy daily tail",
			"On its worst 5%% of days the strategy averaged %.1f%%. That is what a bad "+
				"week actually feels like.", r.CVaR95*100)
	}
	if m.MaxDrawdown < -0.5 {
		add(SeverityWarning, "a drawdown almost nobody holds through",
			"Peak to trough %.0f%%. Whether the returns are real is a separate question "+
				"from whether anyone would still have been in the position at the bottom.",
			m.MaxDrawdown*100)
	}

	// --- Are the mechanics honest? ---------------------------------------

	if res.Spec.Fill == FillClose {
		add(SeverityCritical, "fills at a price the strategy had already seen",
			"This run used close fills. The strategy decided on today's close and traded "+
				"at today's close, which is lookahead bias and inflates returns.")
	}
	if res.Spec.Costs.SlippageBps == 0 && res.Spec.Costs.CommissionPct == 0 &&
		res.Spec.Costs.CommissionPerShare == 0 {
		sev := SeverityNote
		if m.Turnover > 2 {
			sev = SeverityCritical
		}
		add(sev, "frictionless",
			"No commission and no slippage were charged, at an annual turnover of %.1fx. "+
				"Trading costs scale with turnover, so this omission flatters exactly the "+
				"strategies that trade most.", m.Turnover)
	}
	if res.Spec.Costs.ImpactCoefficient == 0 && m.Turnover > 1 {
		add(SeverityNote, "position size is free",
			"No market impact was modelled, so a $10m order costs the same fraction as "+
				"a $1,000 one. That holds at small size and not at large: re-run with "+
				"impact enabled before concluding this scales.")
	}
	if m.Turnover > 10 {
		add(SeverityWarning, "very high turnover",
			"%.0fx annual turnover. Costs of %s were charged; a small error in the cost "+
				"model is multiplied by that turnover.", m.Turnover, money(m.TotalCosts))
	}
	if res.AICallCount > 0 {
		detail := "%d model or web calls were made inside the backtest. Those answers were " +
			"produced by a model that knows what happened after the simulated date. " +
			"Treat this as a demonstration of a mechanism, not as evidence."
		if res.NewsPointInTime {
			// News was date-bounded, so the remaining exposure is the model's
			// own training, which is milder and worth stating differently
			// rather than repeating the stronger warning that no longer fits.
			detail = "%d model calls were made inside the backtest. Headlines were " +
				"restricted to what had been published by each simulated day, so the " +
				"severe form of this bias is gone — but the model reading them was " +
				"trained on text written afterwards and knows how the period ended."
			add(SeverityWarning, "the model knows how the story ends", detail, res.AICallCount)
		} else {
			add(SeverityCritical, "the strategy consulted a model about the past", detail, res.AICallCount)
		}
	}
	if len(res.Manifest.Coverage) > 0 {
		var late int
		for _, cov := range res.Manifest.Coverage {
			if cov.FirstBar > res.Spec.Start {
				late++
			}
		}
		if late > 0 {
			add(SeverityWarning, "survivorship in the symbol list",
				"%d of %d symbols had no data at the start date, so they were selected "+
					"into the universe by having existed later. The names that failed in "+
					"between are not in this test at all.", late, len(res.Manifest.Coverage))
		}
	}

	// --- Is it better than doing nothing? --------------------------------

	if len(res.Benchmarks) > 0 {
		b := res.Benchmarks[0]
		if m.TotalReturn < b.Metric.TotalReturn && m.MaxDrawdown < b.Metric.MaxDrawdown {
			add(SeverityCritical, "beaten by the benchmark on both counts",
				"%s returned %.1f%% with a %.1f%% drawdown against this strategy's %.1f%% "+
					"and %.1f%%. It is worse on return and worse on risk.",
				b.Label, b.Metric.TotalReturn*100, b.Metric.MaxDrawdown*100,
				m.TotalReturn*100, m.MaxDrawdown*100)
		}
		if r.DownCapture.Defined() && r.UpCapture.Defined() {
			up, down := float64(r.UpCapture), float64(r.DownCapture)
			if down > up && up > 0 {
				add(SeverityWarning, "worse than the benchmark in both directions",
					"It captures %.0f%% of the benchmark's up days and %.0f%% of its down "+
						"days — the wrong way round.", up*100, down*100)
			}
		}
	}

	// --- What could be fixed? --------------------------------------------

	t := res.TradeStats
	if t.Closed >= 20 {
		if t.GiveBack > 0.8 && t.GiveBackTrades >= 5 {
			add(SeverityNote, "the exits are late, not the entries",
				"Losing trades gave back %.0f%% of the paper profit they had reached. "+
					"The entries were finding something; the exit rule is handing it back.",
				t.GiveBack*100)
		}
		if t.WinnerMAEPct < -0.10 {
			add(SeverityNote, "the winners had to survive a lot",
				"Winning trades went %.0f%% underwater on average before working. A stop "+
					"tighter than that would have removed the profitable trades, not the "+
					"losing ones.", -t.WinnerMAEPct*100)
		}
		if t.EdgeRatio.Defined() && float64(t.EdgeRatio) < 1 {
			add(SeverityWarning, "trades spend more time losing than winning",
				"Edge ratio %.2f: the average trade's worst excursion exceeds its best. "+
					"The signal is not finding favourable entries.", float64(t.EdgeRatio))
		}
	}

	sort.SliceStable(c.Findings, func(i, j int) bool {
		return severityRank(c.Findings[i].Severity) < severityRank(c.Findings[j].Severity)
	})
	if len(c.Findings) > 0 {
		c.Headline = c.Findings[0].Title
	} else {
		c.Headline = "nothing obviously wrong, which is not the same as right"
	}
	c.TrustScore = trustScore(c.Findings)
	if noResult {
		c.TrustScore = 0
	}
	return c
}

func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 0
	case SeverityWarning:
		return 1
	default:
		return 2
	}
}

// trustScore collapses the findings into a coarse 0-100 figure.
func trustScore(fs []Finding) int {
	score := 100
	for _, f := range fs {
		switch f.Severity {
		case SeverityCritical:
			score -= 25
		case SeverityWarning:
			score -= 10
		default:
			score -= 3
		}
	}
	if score < 0 {
		score = 0
	}
	// Round to the nearest 5. The inputs do not justify finer resolution and
	// showing 73 rather than 75 implies a precision that is not there.
	return (score / 5) * 5
}

// money is a compact currency formatter for finding text.
func money(v float64) string {
	switch a := math.Abs(v); {
	case a >= 1e9:
		return fmt.Sprintf("$%.1fbn", v/1e9)
	case a >= 1e6:
		return fmt.Sprintf("$%.1fm", v/1e6)
	case a >= 1e3:
		return fmt.Sprintf("$%.1fk", v/1e3)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}
