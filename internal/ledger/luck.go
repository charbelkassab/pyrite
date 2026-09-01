package ledger

import (
	"fmt"
	"math"
	"strings"

	"github.com/charbelkassab/pyrite/internal/engine"
)

// LuckThreshold is the score the best of trials tries reaches by chance
// alone, given the spread of scores actually observed.
//
// This is the expected maximum a single sweep already reports, asked of a
// whole research history instead of one search. It is deliberately the same
// function: two numbers on the same screen answering the same question by
// different arithmetic would be worse than either of them alone.
func LuckThreshold(trials int, spread float64) engine.Ratio {
	if trials < 2 || spread <= 0 || math.IsNaN(spread) || math.IsInf(spread, 0) {
		return engine.Ratio(math.NaN())
	}
	return engine.Ratio(engine.ExpectedMaxScore(spread, trials))
}

// materialTrials is where a history starts to be worth mentioning. Below it
// the correction moves the threshold by very little, and a warning nobody
// needs is a warning everybody learns to skip.
const materialTrials = 5

// Warning is the line to print after an invocation that tried this many
// combinations, or "" when the history says nothing the invocation has not
// already accounted for.
func (s Summary) Warning(trials int) string {
	if s.Trials < materialTrials || s.Trials < 2*trials {
		return ""
	}
	msg := fmt.Sprintf("you have now tried %s against this dataset across %s",
		count(s.Trials, "configuration", "configurations"),
		count(s.Invocations, "session", "sessions"))
	if !s.LuckThreshold.Defined() {
		return msg + ", and the statistics you just read corrected for " +
			count(trials, "trial", "trials") + " of them"
	}
	return msg + fmt.Sprintf("; %s below %.2f is what the best of %d tries reaches by luck alone",
		withArticle(ObjectiveLabel(s.Objective)), float64(s.LuckThreshold), s.Trials)
}

// verdict is the plain-English reading of a whole dataset's history.
func verdict(s Summary) string {
	if s.Empty() {
		return "nothing has been recorded against this dataset"
	}

	parts := []string{fmt.Sprintf("%s across %s and %s",
		count(s.Trials, "configuration", "configurations"),
		count(s.Invocations, "session", "sessions"),
		count(len(s.CodeHashes), "version of the strategy code", "versions of the strategy code"))}

	label := ObjectiveLabel(s.Objective)
	switch {
	case !s.LuckThreshold.Defined():
		parts = append(parts, "the recorded scores have too little spread to say what luck alone would "+
			"reach over that many tries; a sweep records the spread a plain run cannot")
	case !s.BestScore.Defined():
		parts = append(parts, fmt.Sprintf(
			"the best of %d tries reaches %s of %.2f by luck alone",
			s.Trials, label, float64(s.LuckThreshold)))
	case float64(s.BestScore) <= float64(s.LuckThreshold):
		parts = append(parts, fmt.Sprintf(
			"the best %s ever seen here, %.2f, is below the %.2f that the best of %d tries reaches by luck alone — this dataset has produced nothing that survives the searching done to it",
			label, float64(s.BestScore), float64(s.LuckThreshold), s.Trials))
	default:
		parts = append(parts, fmt.Sprintf(
			"best %s %.2f against %.2f expected from luck alone over %d tries",
			label, float64(s.BestScore), float64(s.LuckThreshold), s.Trials))
	}

	if s.Invocations == 1 {
		parts = append(parts, "one invocation is no history: what that run printed is still the whole story")
	}
	return strings.Join(parts, "; ")
}

var objectiveLabels = map[string]string{
	"sharpe":        "Sharpe",
	"sortino":       "Sortino",
	"calmar":        "Calmar",
	"cagr":          "CAGR",
	"total_return":  "total return",
	"profit_factor": "profit factor",
	"max_drawdown":  "max drawdown",
	"ulcer":         "ulcer index",
	"expectancy":    "expectancy",
}

// ObjectiveLabel names a sweep objective in a way that reads in a sentence.
func ObjectiveLabel(name string) string {
	if l, ok := objectiveLabels[name]; ok {
		return l
	}
	if name == "" {
		return "score"
	}
	return strings.ReplaceAll(name, "_", " ")
}

func withArticle(s string) string {
	if s == "" {
		return s
	}
	if strings.ContainsRune("aeiouAEIOU", rune(s[0])) {
		return "an " + s
	}
	return "a " + s
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
