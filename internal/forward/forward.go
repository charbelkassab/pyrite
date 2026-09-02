// Package forward records what a strategy says to do before the outcome
// exists, and scores those recordings once it has happened.
//
// Every other honesty mechanism in this project constrains how a past sample
// is used: walk-forward hides the next period from the fit, the blind holdout
// hides a slice of history, the research ledger counts how often a dataset
// has been searched. A determined person defeats all three, because the data
// is already on the disk and nothing stops them looking, changing the rule
// and looking again. A decision written down on Tuesday about Wednesday
// cannot be defeated that way, because on Tuesday Wednesday had not happened.
// It is the only genuinely unfakeable out-of-sample test the tool has.
//
// That argument holds exactly as long as the record cannot be quietly
// rewritten afterwards, which is what the hash chain is for.
package forward

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
)

// fileName is the append-only log inside the forward directory.
const fileName = "decisions.jsonl"

// weightEpsilon is the point below which two recorded weights are the same
// decision. Re-running a strategy can move a weight in the last bit of a
// float without anybody having changed anything, and calling that tampering
// would make the conflict warning worthless through crying wolf.
const weightEpsilon = 1e-9

// Position is one line of the book a strategy wants to be holding.
type Position struct {
	Symbol string `json:"symbol"`
	// Weight is the signed fraction of equity: 0.5 is half the account long,
	// -0.25 a quarter of it short.
	Weight float64 `json:"weight"`
}

// Entry is one decision, written down before its outcome existed.
type Entry struct {
	// At is when the record was written, which is not the same thing as AsOf
	// and is the field that says whether this was a forward record or a
	// backfill.
	At time.Time `json:"at"`
	// Strategy is the label the decisions accumulate under. It is part of the
	// identity of a record, so two strategies may both record on one day.
	Strategy string `json:"strategy"`
	// CodeSHA256 identifies the exact code that produced the decision. An
	// edited strategy recording under the same name on the same day is a
	// different strategy, and this is what says so.
	CodeSHA256 string `json:"code_sha256"`
	// AsOf is the last session the strategy was allowed to see.
	AsOf market.Day `json:"as_of"`
	// Reference is the symbol whose sessions define the holding window.
	//
	// It is needed even when the book is empty. Without a calendar there is
	// no way to tell whether a flat decision's horizon has elapsed, and
	// scoring it as a zero return before the fact would be inventing the
	// outcome the record is waiting for.
	Reference string `json:"reference"`
	// Horizon is how many sessions the decision is held for, counted in
	// sessions of Reference rather than calendar days.
	Horizon int `json:"horizon"`
	// Positions is the book the strategy wants to be holding when the next
	// session opens. An empty list is a decision to hold nothing, which is a
	// decision.
	Positions []Position `json:"positions"`
	Note      string     `json:"note,omitempty"`

	// Prev is the hash of the record before this one, empty for the first.
	Prev string `json:"prev,omitempty"`
	// Hash seals this record. See seal.
	Hash string `json:"hash"`
}

// seal computes a record's hash: sha256 over the whole record with Hash
// cleared, which includes Prev and so links it to everything before it.
//
// The chain is the entire value of this package. An append-only log on its
// own proves nothing about the past, because a file you can append to is a
// file you can rewrite: change one recorded weight, drop the day that went
// badly, and the log still reads as a tidy history. Linking each record to
// the one before means any such edit has to be followed by re-sealing every
// record after it, and that changes the last hash — so tampering stops being
// invisible and becomes something Verify can name, with the index of the
// first record that no longer adds up.
//
// This is not proof against someone who rewrites the whole file from the
// first record onwards. Nothing held on the same disk as the person editing
// it can be. It is proof against the far more common and far more damaging
// case, which is a researcher quietly improving their own record and then
// believing it.
//
// Hashing the marshalled record rather than a hand-written list of fields is
// deliberate: a field added to Entry later is sealed automatically, where a
// hand-written list would leave it unsealed until somebody remembered. The
// price is that changing the encoding of an existing field invalidates
// chains recorded before the change, which is the safe direction to fail.
func (e Entry) seal() (string, error) {
	e.Hash = ""
	body, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("hash the decision recorded for %s on %s: %w", e.Strategy, e.AsOf, err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// SameDecision reports whether two records say the same thing, ignoring when
// they were written.
func (e Entry) SameDecision(other Entry) bool {
	if e.CodeSHA256 != other.CodeSHA256 || e.Horizon != other.Horizon ||
		e.Reference != other.Reference || len(e.Positions) != len(other.Positions) {
		return false
	}
	for i, p := range e.Positions {
		q := other.Positions[i]
		if p.Symbol != q.Symbol || math.Abs(p.Weight-q.Weight) > weightEpsilon {
			return false
		}
	}
	return true
}

// Gross is the total absolute weight of the recorded book.
func (e Entry) Gross() float64 {
	var g float64
	for _, p := range e.Positions {
		g += math.Abs(p.Weight)
	}
	return g
}

// Describe renders the book in one line, for an error or a table.
func (e Entry) Describe() string {
	if len(e.Positions) == 0 {
		return "flat"
	}
	parts := make([]string, 0, len(e.Positions))
	for _, p := range e.Positions {
		parts = append(parts, fmt.Sprintf("%s %.1f%%", p.Symbol, p.Weight*100))
	}
	return strings.Join(parts, ", ")
}

// ConflictError reports a second, different decision for a strategy and day
// that already have one.
type ConflictError struct {
	Recorded Entry
	Offered  Entry
}

func (c *ConflictError) Error() string {
	return fmt.Sprintf("%s already recorded a different decision for %s: %q was written at %s, "+
		"and this run wants to write %q.\n"+
		"  The first record stands. Forward mode is worth something only because what was "+
		"written down before the outcome is what gets scored, so a second, different answer "+
		"for the same day is refused rather than merged.\n"+
		"  If the strategy has genuinely changed, record it under its own name: --name %s-v2",
		strconv.Quote(c.Recorded.Strategy), c.Recorded.AsOf,
		c.Recorded.Describe(), c.Recorded.At.Format(time.RFC3339),
		c.Offered.Describe(), slug(c.Recorded.Strategy))
}

// Log is the append-only, hash-chained record of decisions on disk.
//
// It is safe for concurrent use within a process. The append-only format is
// what makes a second process safe: a short write appends whole lines, and a
// reader that meets a torn one skips it.
type Log struct {
	mu   sync.Mutex
	path string
}

// Open prepares the log held in dir, creating the directory if needed.
func Open(dir string) (*Log, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("open the forward log: no directory given")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("open the forward log in %s: %w", dir, err)
	}
	return &Log{path: filepath.Join(dir, fileName)}, nil
}

// Path is where the log lives, for a command that wants to name it.
func (l *Log) Path() string { return l.path }

// Reading is the contents of the log plus whatever it could not read.
type Reading struct {
	Entries []Entry `json:"entries"`
	// Skipped describes lines that would not parse. A crash part-way through
	// a write leaves half a line behind, and refusing to load a year of
	// recorded decisions over it would lose far more than the crash did. It
	// is still reported, because a line that will not parse is also what a
	// clumsy edit looks like.
	Skipped []string `json:"skipped,omitempty"`
}

// Read returns every record, oldest first.
func (l *Log) Read() (Reading, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readLocked()
}

func (l *Log) readLocked() (Reading, error) {
	var r Reading
	f, err := os.Open(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("open the forward log %s: %w", l.path, err)
	}
	defer f.Close()

	// A recorded book is small, but nothing bounds the number of symbols in
	// it, so the scanner gets room rather than failing on a long line.
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil || e.Hash == "" {
			r.Skipped = append(r.Skipped, fmt.Sprintf("line %d could not be read and was skipped", n))
			continue
		}
		r.Entries = append(r.Entries, e)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return r, fmt.Errorf("read the forward log %s: %w", l.path, err)
	}
	return r, nil
}

// Record appends one decision.
//
// It reports whether anything was written: recording the same decision twice
// on one day is a no-op rather than a second observation, because two entries
// for one day would count that day twice in every statistic downstream. A
// second, *different* decision for the same day is refused with a
// ConflictError — that is precisely the rewriting the chain exists to catch,
// and silently overwriting it would be the tool doing the tampering itself.
func (l *Log) Record(e Entry) (Entry, bool, error) {
	if strings.TrimSpace(e.Strategy) == "" {
		return Entry{}, false, errors.New("record a forward decision: no strategy name, so there is nothing to score it under")
	}
	if e.AsOf == "" {
		return Entry{}, false, errors.New("record a forward decision: no as-of date, so there is no way to know when its outcome arrives")
	}
	if e.Horizon < 1 {
		e.Horizon = 1
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	// UTC because the hash is computed over the encoded record, and a
	// timestamp that renders differently in a different zone would seal to a
	// different value on a laptop that has travelled.
	e.At = e.At.UTC()
	if e.Positions == nil {
		e.Positions = []Position{}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	r, err := l.readLocked()
	if err != nil {
		return Entry{}, false, err
	}
	for _, prior := range r.Entries {
		if prior.Strategy != e.Strategy || prior.AsOf != e.AsOf {
			continue
		}
		if prior.SameDecision(e) {
			return prior, false, nil
		}
		return prior, false, &ConflictError{Recorded: prior, Offered: e}
	}

	if n := len(r.Entries); n > 0 {
		e.Prev = r.Entries[n-1].Hash
	}
	hash, err := e.seal()
	if err != nil {
		return Entry{}, false, err
	}
	e.Hash = hash

	line, err := json.Marshal(e)
	if err != nil {
		return Entry{}, false, fmt.Errorf("encode the forward decision for %s: %w", e.AsOf, err)
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return Entry{}, false, fmt.Errorf("open the forward log %s: %w", l.path, err)
	}
	// A crash part-way through a write leaves a line with no newline on it.
	// Appending straight onto that would glue this record to the wreckage and
	// lose both; starting a fresh line loses only the torn one, which was
	// already lost.
	if info, statErr := f.Stat(); statErr == nil && info.Size() > 0 {
		var tail [1]byte
		if _, readErr := f.ReadAt(tail[:], info.Size()-1); readErr == nil && tail[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return Entry{}, false, fmt.Errorf("append to the forward log %s: %w", l.path, err)
	}
	if err := f.Close(); err != nil {
		return Entry{}, false, fmt.Errorf("close the forward log %s: %w", l.path, err)
	}
	return e, true, nil
}

// Verification is what walking the chain found.
type Verification struct {
	Entries int  `json:"entries"`
	Intact  bool `json:"intact"`
	// BreakAt is the index of the first record that does not add up, counting
	// from zero, or -1 when nothing is wrong.
	BreakAt int `json:"break_at"`
	// Reason names what is wrong with that record, in words.
	Reason string `json:"reason,omitempty"`
	// Skipped repeats whatever the reader could not parse. An unreadable line
	// is not a broken link, but it is a hole in the history and belongs in
	// the same report.
	Skipped []string   `json:"skipped,omitempty"`
	First   time.Time  `json:"first,omitempty"`
	Last    time.Time  `json:"last,omitempty"`
	FirstAs market.Day `json:"first_as_of,omitempty"`
	LastAs  market.Day `json:"last_as_of,omitempty"`
}

// Verify walks the chain and reports the first break.
func (l *Log) Verify() (Verification, error) {
	r, err := l.Read()
	if err != nil {
		return Verification{BreakAt: -1}, err
	}
	return VerifyEntries(r), nil
}

// VerifyEntries checks an already-loaded reading, so a command that has the
// records in hand does not read the file twice.
func VerifyEntries(r Reading) Verification {
	v := Verification{Entries: len(r.Entries), Intact: true, BreakAt: -1, Skipped: r.Skipped}
	if len(r.Entries) > 0 {
		v.First, v.FirstAs = r.Entries[0].At, r.Entries[0].AsOf
		v.Last, v.LastAs = r.Entries[len(r.Entries)-1].At, r.Entries[len(r.Entries)-1].AsOf
	}

	var prev string
	for i, e := range r.Entries {
		// The link is checked before the seal so that a deleted record is
		// reported at the record that should have followed it, which is where
		// the gap actually is.
		if e.Prev != prev {
			v.Intact, v.BreakAt = false, i
			v.Reason = fmt.Sprintf("record %d (%s, as of %s) follows a record hashing to %s, "+
				"but the record before it in the file hashes to %s. Something between them was "+
				"removed or reordered.", i, e.Strategy, e.AsOf, shortHash(e.Prev), shortHash(prev))
			return v
		}
		want, err := e.seal()
		if err != nil {
			v.Intact, v.BreakAt = false, i
			v.Reason = fmt.Sprintf("record %d (%s, as of %s) could not be hashed: %v", i, e.Strategy, e.AsOf, err)
			return v
		}
		if want != e.Hash {
			v.Intact, v.BreakAt = false, i
			v.Reason = fmt.Sprintf("record %d (%s, as of %s) has been altered since it was written: "+
				"its contents hash to %s, but it carries %s.",
				i, e.Strategy, e.AsOf, shortHash(want), shortHash(e.Hash))
			return v
		}
		prev = e.Hash
	}
	return v
}

func shortHash(h string) string {
	if h == "" {
		return "nothing"
	}
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// slug turns a strategy name into something that can be typed after --name.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "strategy"
	}
	return out
}

// Intend projects the book held at the close of the last simulated session
// through the orders that session produced, giving the weights the strategy
// wants to be holding when the next one opens.
//
// What comes out is an intention, not a prediction of fills. The engine sizes
// an order against the next session's open, which has not happened, and may
// cut it for cash or leverage. Recording the ask rather than a guess at the
// fill is the right way round: the ask is the strategy's decision, and the
// decision is the thing being tested.
func Intend(last engine.DayRecord, prices map[string]float64, maxLeverage float64) []Position {
	book := map[string]float64{}
	for _, p := range last.Positions {
		book[p.Symbol] = p.Weight
	}
	equity := last.Equity
	for _, o := range last.Orders {
		before := book[o.Symbol]
		var after float64
		switch {
		case o.Kind == engine.KindWeight && o.IsTarget:
			after = o.Weight
		case o.Kind == engine.KindWeight:
			after = before + o.Weight
		case o.Kind == engine.KindNotional && equity > 0:
			after = before + o.Notional/equity
		case o.Kind == engine.KindShares && equity > 0 && prices[o.Symbol] > 0:
			after = before + o.Shares*prices[o.Symbol]/equity
		default:
			// Nothing here can be turned into a weight, so recording one
			// would be making it up.
			continue
		}
		// cover() and its kind must not carry a position through zero into
		// the opposite direction, the same rule the engine applies at fill.
		if o.NoFlip && before != 0 && after*before < 0 {
			after = 0
		}
		book[o.Symbol] = after
	}

	symbols := make([]string, 0, len(book))
	var gross float64
	for sym, w := range book {
		if math.Abs(w) < weightEpsilon {
			continue
		}
		symbols = append(symbols, sym)
		gross += math.Abs(w)
	}
	sort.Strings(symbols)

	// A strategy that asks for more than the run permits gets scaled back
	// rather than recorded at a size it would never have been given.
	scale := 1.0
	if maxLeverage > 0 && gross > maxLeverage {
		scale = maxLeverage / gross
	}
	out := make([]Position, 0, len(symbols))
	for _, sym := range symbols {
		out = append(out, Position{Symbol: sym, Weight: book[sym] * scale})
	}
	return out
}
