// Package ledger remembers how much searching a dataset has already absorbed.
//
// The deflated Sharpe ratio and the probability of backtest overfitting both
// correct for the number of trials in one search. A researcher who runs forty
// sweeps over the same symbols and the same period across three weeks has
// performed thousands of trials, and every statistic on every one of those
// runs silently assumed the trials began that morning. The ledger is the only
// thing in the project that survives the session, and its whole purpose is to
// make the true number visible: nobody remembers how many configurations they
// have tried, and everybody underestimates it.
//
// The store is an append-only JSONL file. Appending is the one operation that
// cannot lose earlier history to a crash, and the reader skips a truncated
// line rather than refusing to read the log — a research history is worth more
// than the last record written to it.
package ledger

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charbelkassab/pyrite/internal/engine"
)

// fileName is the append-only log inside the ledger directory.
const fileName = "trials.jsonl"

// Entry is one invocation recorded against a dataset.
type Entry struct {
	At         time.Time `json:"at"`
	DatasetKey string    `json:"dataset_key"`
	Strategy   string    `json:"strategy,omitempty"`
	// CodeSHA256 identifies the strategy code, so a dataset searched by six
	// rewrites of one idea can be told from one searched by a single strategy
	// six times.
	CodeSHA256 string `json:"code_sha256,omitempty"`
	// Trials is how many parameter combinations this invocation tried: 1 for
	// a plain run, N for a sweep.
	Trials    int    `json:"trials"`
	Objective string `json:"objective,omitempty"`
	// BestScore is the best score this invocation reached under Objective.
	BestScore engine.Ratio `json:"best_score"`
	// ScoreSpread is the standard deviation of the scores this invocation
	// produced, which is what the luck threshold needs and what only a search
	// wide enough to have a spread can supply.
	ScoreSpread engine.Ratio `json:"score_spread"`
	// RunID ties the entry back to a saved run or sweep, where there is one.
	RunID string `json:"run_id,omitempty"`
}

// Ledger is an append-only research history on disk.
//
// It is safe for concurrent use. The mutex serialises this process; a second
// process appending at the same time is handled by the append-only format
// itself, which is why the format was chosen.
type Ledger struct {
	mu   sync.Mutex
	path string
}

// Open prepares the ledger held in dir, creating the directory if needed.
func Open(dir string) (*Ledger, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("open ledger: no directory given")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("open ledger %s: %w", dir, err)
	}
	return &Ledger{path: filepath.Join(dir, fileName)}, nil
}

// Record appends one invocation.
func (l *Ledger) Record(e Entry) error {
	if strings.TrimSpace(e.DatasetKey) == "" {
		return errors.New("record ledger entry: no dataset key, so there is nothing to count it against")
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if e.Trials < 1 {
		e.Trials = 1
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode ledger entry: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open ledger %s: %w", l.path, err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("append to ledger %s: %w", l.path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close ledger %s: %w", l.path, err)
	}
	return nil
}

// Query summarises everything recorded against one dataset. An unknown key
// gives an empty summary rather than an error: a dataset nobody has searched
// is a legitimate answer.
func (l *Ledger) Query(datasetKey string) (Summary, error) {
	entries, err := l.read()
	if err != nil {
		return Summary{DatasetKey: datasetKey}, err
	}
	var mine []Entry
	for _, e := range entries {
		if e.DatasetKey == datasetKey {
			mine = append(mine, e)
		}
	}
	return summarise(datasetKey, mine), nil
}

// Datasets summarises every dataset in the ledger, most recently searched
// first.
func (l *Ledger) Datasets() ([]Summary, error) {
	entries, err := l.read()
	if err != nil {
		return nil, err
	}
	byKey := map[string][]Entry{}
	var order []string
	for _, e := range entries {
		if _, seen := byKey[e.DatasetKey]; !seen {
			order = append(order, e.DatasetKey)
		}
		byKey[e.DatasetKey] = append(byKey[e.DatasetKey], e)
	}
	out := make([]Summary, 0, len(order))
	for _, k := range order {
		out = append(out, summarise(k, byKey[k]))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out, nil
}

// Reset forgets one dataset.
//
// It is a rewrite rather than an edit in place, through a temporary file, so
// an interrupted reset leaves the original history intact instead of a
// half-erased one.
func (l *Ledger) Reset(datasetKey string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	entries, err := l.readLocked()
	if err != nil {
		return err
	}
	kept := entries[:0]
	for _, e := range entries {
		if e.DatasetKey != datasetKey {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(entries) {
		return nil
	}
	if len(kept) == 0 {
		return l.removeLocked()
	}

	tmp, err := os.CreateTemp(filepath.Dir(l.path), fileName+".*")
	if err != nil {
		return fmt.Errorf("rewrite ledger %s: %w", l.path, err)
	}
	w := bufio.NewWriter(tmp)
	for _, e := range kept {
		line, err := json.Marshal(e)
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return fmt.Errorf("encode ledger entry: %w", err)
		}
		w.Write(append(line, '\n'))
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("rewrite ledger %s: %w", l.path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("rewrite ledger %s: %w", l.path, err)
	}
	if err := os.Rename(tmp.Name(), l.path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("replace ledger %s: %w", l.path, err)
	}
	return nil
}

// ResetAll forgets every dataset.
func (l *Ledger) ResetAll() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.removeLocked()
}

func (l *Ledger) removeLocked() error {
	if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear ledger %s: %w", l.path, err)
	}
	return nil
}

func (l *Ledger) read() ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readLocked()
}

// readLocked parses the log, skipping anything it cannot make sense of.
//
// A line that fails to parse is a line an interrupted write left behind. The
// history either side of it is still true, and refusing to read a research
// history because its last record was cut in half would lose far more than
// the crash did.
func (l *Ledger) readLocked() ([]Entry, error) {
	f, err := os.Open(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open ledger %s: %w", l.path, err)
	}
	defer f.Close()

	var out []Entry
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			var e Entry
			if json.Unmarshal([]byte(trimmed), &e) == nil && e.DatasetKey != "" && e.Trials > 0 {
				out = append(out, e)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return out, nil
			}
			return out, fmt.Errorf("read ledger %s: %w", l.path, readErr)
		}
	}
}

// Summary is everything the ledger knows about one dataset.
type Summary struct {
	DatasetKey string `json:"dataset_key"`
	// Invocations is how many separate runs and sweeps have touched this
	// dataset; Trials is the total number of parameter combinations across
	// all of them, which is the number every per-run statistic ignores.
	Invocations int `json:"invocations"`
	Trials      int `json:"trials"`
	// First and Last are zero when nothing has been recorded.
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	// Strategies and CodeHashes are the distinct names and distinct code
	// hashes seen. Six versions of one idea are six strategies, and the
	// hashes are what say so.
	Strategies []string `json:"strategies,omitempty"`
	CodeHashes []string `json:"code_hashes,omitempty"`

	// Objective is the metric the most recent invocation ranked by, and
	// BestScore and ScoreSpread cover only the invocations that used it.
	// Scores under different objectives are different units and a best across
	// them would mean nothing — whereas the trial count is shared, because
	// searching by Calmar spends the same multiple-testing budget on the same
	// data as searching by Sharpe.
	Objective   string       `json:"objective,omitempty"`
	Objectives  []string     `json:"objectives,omitempty"`
	BestScore   engine.Ratio `json:"best_score"`
	ScoreSpread engine.Ratio `json:"score_spread"`
	// LuckThreshold is the score the best of Trials tries reaches by chance
	// alone, and is undefined when nothing recorded has enough spread to say.
	LuckThreshold engine.Ratio `json:"luck_threshold"`
	Verdict       string       `json:"verdict"`
}

// Empty reports whether anything has been recorded against the dataset.
func (s Summary) Empty() bool { return s.Invocations == 0 }

func summarise(key string, entries []Entry) Summary {
	s := Summary{
		DatasetKey:    key,
		Invocations:   len(entries),
		BestScore:     engine.Ratio(math.NaN()),
		ScoreSpread:   engine.Ratio(math.NaN()),
		LuckThreshold: engine.Ratio(math.NaN()),
	}
	if len(entries) == 0 {
		s.Verdict = verdict(s)
		return s
	}

	names := map[string]bool{}
	hashes := map[string]bool{}
	objectives := map[string]bool{}
	var latestObjective time.Time
	for _, e := range entries {
		s.Trials += e.Trials
		if s.First.IsZero() || e.At.Before(s.First) {
			s.First = e.At
		}
		if e.At.After(s.Last) {
			s.Last = e.At
		}
		if e.Strategy != "" {
			names[e.Strategy] = true
		}
		if e.CodeSHA256 != "" {
			hashes[e.CodeSHA256] = true
		}
		// The most recent objective is the one the researcher is currently
		// working to, which is the one a verdict should speak in.
		if e.Objective != "" {
			objectives[e.Objective] = true
			if s.Objective == "" || !e.At.Before(latestObjective) {
				s.Objective, latestObjective = e.Objective, e.At
			}
		}
	}
	s.Strategies, s.CodeHashes, s.Objectives = sortedKeys(names), sortedKeys(hashes), sortedKeys(objectives)

	var matching []Entry
	for _, e := range entries {
		if e.Objective == s.Objective {
			matching = append(matching, e)
		}
	}
	best := math.Inf(-1)
	for _, e := range matching {
		if e.BestScore.Defined() && float64(e.BestScore) > best {
			best = float64(e.BestScore)
		}
	}
	if !math.IsInf(best, -1) {
		s.BestScore = engine.Ratio(best)
	}
	if spread := scoreSpread(matching); spread > 0 {
		s.ScoreSpread = engine.Ratio(spread)
		s.LuckThreshold = LuckThreshold(s.Trials, spread)
	}
	s.Verdict = verdict(s)
	return s
}

// scoreSpread estimates how much one trial's score varies against this
// dataset, which is the input the luck threshold cannot do without.
//
// A sweep measured that spread across its own combinations and recorded it,
// so those measurements are used first, weighted by trial count: a search over
// 400 combinations describes the distribution better than one over four. A
// history of plain runs recorded no spread, and what is left is how much their
// scores differed from each other — which is a fair estimate rather than a
// fallback, because a single-trial invocation's score is one draw and not a
// maximum.
func scoreSpread(entries []Entry) float64 {
	var weighted, weight float64
	var draws []float64
	for _, e := range entries {
		if e.Trials > 1 && e.ScoreSpread.Defined() && float64(e.ScoreSpread) > 0 {
			weighted += float64(e.ScoreSpread) * float64(e.Trials)
			weight += float64(e.Trials)
		}
		if e.Trials == 1 && e.BestScore.Defined() {
			draws = append(draws, float64(e.BestScore))
		}
	}
	if weight > 0 {
		return weighted / weight
	}
	return stdev(draws)
}

func stdev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Disabled reports whether the researcher has opted out of the ledger with
// PYRITE_NO_LEDGER.
func Disabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PYRITE_NO_LEDGER"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
