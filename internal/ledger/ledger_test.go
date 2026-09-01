package ledger

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charbelkassab/pyrite/internal/engine"
)

const testKey = "SPY:2019-01-02:2023-12-29:1d"

func openTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return l
}

func record(t *testing.T, l *Ledger, e Entry) {
	t.Helper()
	if err := l.Record(e); err != nil {
		t.Fatalf("record: %v", err)
	}
}

func TestQueryOfAnUnsearchedDatasetIsEmptyNotAnError(t *testing.T) {
	s, err := openTestLedger(t).Query(testKey)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !s.Empty() || s.Trials != 0 {
		t.Errorf("want an empty summary, got %+v", s)
	}
	if s.Verdict == "" {
		t.Error("even an empty summary should say so in words")
	}
}

func TestTrialsAccumulateAcrossLedgerInstances(t *testing.T) {
	// The point of the whole feature: the count survives the process that
	// wrote it, because a researcher's forty sweeps are forty sessions.
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		l, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		record(t, l, Entry{
			At: time.Now().Add(time.Duration(i) * time.Minute), DatasetKey: testKey,
			Strategy: "golden cross", CodeSHA256: "abc", Trials: 40,
			Objective: "sharpe", BestScore: engine.Ratio(1.1 + float64(i)/10),
			ScoreSpread: engine.Ratio(0.4),
		})
	}

	l, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s, err := l.Query(testKey)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if s.Invocations != 3 || s.Trials != 120 {
		t.Fatalf("want 3 invocations and 120 trials, got %d and %d", s.Invocations, s.Trials)
	}
	if math.Abs(float64(s.BestScore)-1.3) > 1e-9 {
		t.Errorf("best score = %v, want 1.3", float64(s.BestScore))
	}
	if len(s.CodeHashes) != 1 || len(s.Strategies) != 1 {
		t.Errorf("one strategy searched three times is one code hash: %+v", s)
	}
	if s.First.After(s.Last) {
		t.Errorf("first %s is after last %s", s.First, s.Last)
	}
	// 120 trials of this spread reach well above any one of the recorded
	// scores, which is the sentence the ledger exists to be able to say.
	if !s.LuckThreshold.Defined() || float64(s.LuckThreshold) <= 0 {
		t.Fatalf("luck threshold = %v", float64(s.LuckThreshold))
	}
}

func TestDatasetsAreKeptApart(t *testing.T) {
	l := openTestLedger(t)
	other := "QQQ:2019-01-02:2023-12-29:1d"
	record(t, l, Entry{DatasetKey: testKey, Trials: 10, Objective: "sharpe", BestScore: 1})
	record(t, l, Entry{DatasetKey: other, Trials: 3, Objective: "sharpe", BestScore: 2})

	s, err := l.Query(testKey)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if s.Trials != 10 {
		t.Errorf("trials leaked between datasets: %d", s.Trials)
	}
	all, err := l.Datasets()
	if err != nil {
		t.Fatalf("datasets: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 datasets, got %d", len(all))
	}
}

func TestCorruptLinesAreSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	record(t, l, Entry{DatasetKey: testKey, Trials: 5, Objective: "sharpe", BestScore: 1})
	record(t, l, Entry{DatasetKey: testKey, Trials: 7, Objective: "sharpe", BestScore: 2})

	// A crash mid-append leaves a half-written line, and blank lines and
	// rubbish are what a partially flushed file looks like on disk.
	path := filepath.Join(dir, fileName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	f.WriteString("\n{\"dataset_key\":\"" + testKey + "\",\"trials\":3\n")
	f.WriteString("not json at all\n")
	f.WriteString("{\"dataset_key\":\"\",\"trials\":9}\n")
	f.WriteString("{\"dataset_key\":\"" + testKey + "\",\"trials\":11,\"objective\":\"sharpe\",\"best_score\":3}")
	f.Close()

	s, err := l.Query(testKey)
	if err != nil {
		t.Fatalf("query over a damaged log: %v", err)
	}
	if s.Invocations != 3 || s.Trials != 23 {
		t.Fatalf("want the 3 readable entries and 23 trials, got %d and %d", s.Invocations, s.Trials)
	}
}

func TestUndefinedScoresSurviveTheRoundTrip(t *testing.T) {
	// A Sharpe of NaN reaches JSON as null. Recorded as a bare float it would
	// refuse to encode and take the entry with it.
	l := openTestLedger(t)
	record(t, l, Entry{
		DatasetKey: testKey, Trials: 1, Objective: "sharpe",
		BestScore: engine.Ratio(math.NaN()), ScoreSpread: engine.Ratio(math.Inf(1)),
	})
	s, err := l.Query(testKey)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if s.Invocations != 1 {
		t.Fatalf("the entry did not survive: %+v", s)
	}
	if s.BestScore.Defined() || s.LuckThreshold.Defined() {
		t.Errorf("nothing measurable was recorded, so nothing should be reported: %+v", s)
	}
}

func TestResetForgetsOneDatasetAndKeepsTheRest(t *testing.T) {
	l := openTestLedger(t)
	other := "QQQ:2019-01-02:2023-12-29:1d"
	record(t, l, Entry{DatasetKey: testKey, Trials: 10, Objective: "sharpe", BestScore: 1})
	record(t, l, Entry{DatasetKey: other, Trials: 3, Objective: "sharpe", BestScore: 2})

	if err := l.Reset(testKey); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if s, _ := l.Query(testKey); !s.Empty() {
		t.Errorf("dataset survived the reset: %+v", s)
	}
	if s, _ := l.Query(other); s.Trials != 3 {
		t.Errorf("the untouched dataset lost history: %+v", s)
	}

	if err := l.ResetAll(); err != nil {
		t.Fatalf("reset all: %v", err)
	}
	all, err := l.Datasets()
	if err != nil {
		t.Fatalf("datasets: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("want an empty ledger, got %d datasets", len(all))
	}
	// Resetting an empty ledger is not an error, and neither is resetting a
	// dataset nobody has searched.
	if err := l.ResetAll(); err != nil {
		t.Errorf("reset all on an empty ledger: %v", err)
	}
	if err := l.Reset("nothing:*:*:1d"); err != nil {
		t.Errorf("reset of an unknown dataset: %v", err)
	}
}

func TestConcurrentRecordsAreAllKept(t *testing.T) {
	l := openTestLedger(t)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Record(Entry{DatasetKey: testKey, Trials: 2, Objective: "sharpe", BestScore: 1}); err != nil {
				t.Errorf("record: %v", err)
			}
		}()
	}
	wg.Wait()

	s, err := l.Query(testKey)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if s.Invocations != 24 || s.Trials != 48 {
		t.Errorf("want 24 invocations and 48 trials, got %d and %d", s.Invocations, s.Trials)
	}
}

func TestRecordRefusesAnEntryWithNoDataset(t *testing.T) {
	err := openTestLedger(t).Record(Entry{Trials: 1})
	if err == nil {
		t.Fatal("an entry with no dataset key counts against nothing and must be refused")
	}
	if !strings.Contains(err.Error(), "dataset key") {
		t.Errorf("the error should say what is missing: %v", err)
	}
}

func TestLuckThresholdRisesWithTrialCount(t *testing.T) {
	// The reason to keep a ledger at all: the bar a result has to clear goes
	// up the longer you search, and a threshold that ignored the count would
	// make the whole feature decorative.
	small := LuckThreshold(10, 0.5)
	large := LuckThreshold(1000, 0.5)
	if !small.Defined() || !large.Defined() {
		t.Fatalf("both thresholds should be defined: %v and %v", float64(small), float64(large))
	}
	if !(float64(large) > float64(small)) || float64(small) <= 0 {
		t.Fatalf("threshold should rise with trials: %v then %v", float64(small), float64(large))
	}
	// A wider spread of outcomes means luck reaches further.
	if !(float64(LuckThreshold(100, 1.0)) > float64(LuckThreshold(100, 0.5))) {
		t.Error("threshold should rise with spread")
	}
	for name, r := range map[string]engine.Ratio{
		"one trial":    LuckThreshold(1, 0.5),
		"no spread":    LuckThreshold(100, 0),
		"a NaN spread": LuckThreshold(100, math.NaN()),
	} {
		if r.Defined() {
			t.Errorf("%s cannot produce a threshold, got %v", name, float64(r))
		}
	}
	// It must agree with what a single sweep reports, because it is the same
	// question asked of a longer history.
	if got, want := float64(LuckThreshold(120, 0.4)), engine.ExpectedMaxScore(0.4, 120); got != want {
		t.Errorf("ledger and sweep disagree: %v against %v", got, want)
	}
}

func TestWarningStaysQuietUntilTheHistoryMatters(t *testing.T) {
	s := Summary{Trials: 4, Invocations: 4, Objective: "sharpe"}
	if w := s.Warning(1); w != "" {
		t.Errorf("a handful of runs is not news: %q", w)
	}
	s = Summary{Trials: 200, Invocations: 2, Objective: "sharpe"}
	if w := s.Warning(160); w != "" {
		t.Errorf("a history barely larger than this sweep is not news: %q", w)
	}

	s = Summary{
		Trials: 847, Invocations: 12, Objective: "sharpe",
		LuckThreshold: engine.Ratio(1.42),
	}
	w := s.Warning(40)
	for _, want := range []string{"847 configurations", "12 sessions", "a Sharpe below 1.42", "luck alone"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q is missing %q", w, want)
		}
	}
	// With no measured spread there is no threshold, and the warning must say
	// what it knows rather than invent the number it does not.
	s.LuckThreshold = engine.Ratio(math.NaN())
	if w := s.Warning(40); strings.Contains(w, "luck alone") || !strings.Contains(w, "847 configurations") {
		t.Errorf("warning without a threshold reads wrong: %q", w)
	}
}

func TestVerdictNamesTheDatasetsWorstCase(t *testing.T) {
	beaten := Summary{
		Invocations: 12, Trials: 847, Objective: "sharpe", CodeHashes: []string{"a", "b"},
		BestScore: engine.Ratio(1.10), LuckThreshold: engine.Ratio(1.42),
	}
	if v := verdict(beaten); !strings.Contains(v, "below the 1.42") {
		t.Errorf("a best score under the luck threshold must be called out: %q", v)
	}
	survived := beaten
	survived.BestScore = engine.Ratio(2.10)
	if v := verdict(survived); strings.Contains(v, "below the") {
		t.Errorf("a best score above the threshold is not a failure: %q", v)
	}
}

func TestDisabledHonoursTheOptOut(t *testing.T) {
	for value, want := range map[string]bool{"": false, "0": false, "no": false, "1": true, "true": true, "ON": true} {
		t.Setenv("PYRITE_NO_LEDGER", value)
		if got := Disabled(); got != want {
			t.Errorf("PYRITE_NO_LEDGER=%q: got %v, want %v", value, got, want)
		}
	}
}
