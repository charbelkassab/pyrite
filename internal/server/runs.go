package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
	"github.com/charbelkassab/pyrite/internal/strategy"
)

// RunStatus is the lifecycle state of a backtest.
type RunStatus string

const (
	StatusQueued    RunStatus = "queued"
	StatusCompiling RunStatus = "compiling"
	StatusRunning   RunStatus = "running"
	StatusDone      RunStatus = "done"
	StatusError     RunStatus = "error"
	StatusCancelled RunStatus = "cancelled"
)

// Run is one backtest, tracked from request to result.
type Run struct {
	ID        string    `json:"id"`
	Status    RunStatus `json:"status"`
	Prompt    string    `json:"prompt"`
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Progress is 0..100.
	Progress int        `json:"progress"`
	Stage    string     `json:"stage,omitempty"`
	Day      market.Day `json:"day,omitempty"`

	Plan   *strategy.Plan `json:"plan,omitempty"`
	Result *engine.Result `json:"result,omitempty"`
	// Sweep is set instead of Result when this run was a parameter search.
	// A sweep shares the whole run lifecycle — progress, SSE, cancellation,
	// persistence — because it is the same thing many times over, and a
	// parallel store for it would be duplication rather than design.
	Sweep       *engine.SweepResult       `json:"sweep,omitempty"`
	WalkForward *engine.WalkForwardResult `json:"walk_forward,omitempty"`
	Error       string                    `json:"error,omitempty"`

	mu       sync.RWMutex
	subs     map[chan Event]struct{}
	cancel   context.CancelFunc
	finished chan struct{}
}

// Event is one server-sent update.
type Event struct {
	Type string `json:"type"` // status | progress | done | error | log
	Run  *Run   `json:"run,omitempty"`
	// Message carries human-readable detail for log events.
	Message string `json:"message,omitempty"`
}

// snapshot returns a copy safe to serialise without holding the lock.
func (r *Run) snapshot(includeResult bool) *Run {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c := &Run{
		ID: r.ID, Status: r.Status, Prompt: r.Prompt, Label: r.Label,
		CreatedAt: r.CreatedAt, Progress: r.Progress, Stage: r.Stage,
		Day: r.Day, Plan: r.Plan, Error: r.Error,
	}
	if includeResult {
		c.Result = r.Result
		c.Sweep = r.Sweep
		c.WalkForward = r.WalkForward
	}
	return c
}

func (r *Run) update(fn func(*Run)) {
	r.mu.Lock()
	fn(r)
	r.mu.Unlock()
}

// publish fans an event out to every subscriber, dropping updates for slow
// consumers rather than blocking the backtest.
func (r *Run) publish(ev Event) {
	r.mu.RLock()
	subs := make([]chan Event, 0, len(r.subs))
	for ch := range r.subs {
		subs = append(subs, ch)
	}
	r.mu.RUnlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (r *Run) subscribe() chan Event {
	ch := make(chan Event, 32)
	r.mu.Lock()
	if r.subs == nil {
		r.subs = map[chan Event]struct{}{}
	}
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch
}

func (r *Run) unsubscribe(ch chan Event) {
	r.mu.Lock()
	delete(r.subs, ch)
	r.mu.Unlock()
}

// RunStore keeps runs in memory and persists finished ones to disk so a
// result survives a restart and can be reopened by URL.
type RunStore struct {
	dir string

	mu   sync.RWMutex
	runs map[string]*Run
	// order holds ids newest first.
	order []string
	// MaxInMemory bounds memory use; older runs stay on disk.
	MaxInMemory int
}

// NewRunStore creates the store, making its directory.
func NewRunStore(dir string) (*RunStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &RunStore{dir: dir, runs: map[string]*Run{}, MaxInMemory: 40}, nil
}

// nextID produces a short, sortable, URL-safe identifier.
func (s *RunStore) nextID() string {
	// Millisecond timestamp in base36 is compact, chronologically sortable
	// and collision-free for a single-user local tool.
	return strings.ToLower(fmt.Sprintf("%s", base36(time.Now().UnixMilli())))
}

func base36(n int64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%36]}, b...)
		n /= 36
	}
	return string(b)
}

// Create registers a new run.
func (s *RunStore) Create(prompt, label string) *Run {
	r := &Run{
		ID:        s.nextID(),
		Status:    StatusQueued,
		Prompt:    prompt,
		Label:     label,
		CreatedAt: time.Now().UTC(),
		subs:      map[chan Event]struct{}{},
		finished:  make(chan struct{}),
	}
	s.mu.Lock()
	s.runs[r.ID] = r
	s.order = append([]string{r.ID}, s.order...)
	s.evictLocked()
	s.mu.Unlock()
	return r
}

// evictLocked drops the oldest in-memory results past the cap. Callers must
// hold the write lock.
func (s *RunStore) evictLocked() {
	if len(s.order) <= s.MaxInMemory {
		return
	}
	for _, id := range s.order[s.MaxInMemory:] {
		if r, ok := s.runs[id]; ok {
			r.mu.Lock()
			// Keep the metadata, release the heavy result.
			if r.Status == StatusDone {
				r.Result = nil
			}
			r.mu.Unlock()
		}
	}
}

// Get returns a run, loading it from disk if it is not in memory.
func (s *RunStore) Get(id string) (*Run, bool) {
	s.mu.RLock()
	r, ok := s.runs[id]
	s.mu.RUnlock()
	if ok {
		r.mu.RLock()
		hasResult := r.Result != nil || r.Status != StatusDone
		r.mu.RUnlock()
		if hasResult {
			return r, true
		}
	}
	if loaded, err := s.load(id); err == nil {
		s.mu.Lock()
		s.runs[id] = loaded
		s.mu.Unlock()
		return loaded, true
	}
	return r, ok
}

// List returns run metadata, newest first.
func (s *RunStore) List(limit int) []*Run {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > len(s.order) {
		limit = len(s.order)
	}
	out := make([]*Run, 0, limit)
	for _, id := range s.order[:limit] {
		if r, ok := s.runs[id]; ok {
			out = append(out, r.snapshot(false))
		}
	}
	return out
}

// ListSaved enumerates run files on disk, newest first.
func (s *RunStore) ListSaved(limit int) []*Run {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	out := make([]*Run, 0, len(ids))
	for _, id := range ids {
		if r, err := s.load(id); err == nil {
			out = append(out, r.snapshot(false))
		}
	}
	return out
}

func (s *RunStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// Save writes a finished run to disk.
func (s *RunStore) Save(r *Run) error {
	snap := r.snapshot(true)
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	tmp := s.path(r.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(r.ID))
}

func (s *RunStore) load(id string) (*Run, error) {
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	r.subs = map[chan Event]struct{}{}
	r.finished = make(chan struct{})
	close(r.finished)
	return &r, nil
}

// Delete removes a run from memory and disk.
func (s *RunStore) Delete(id string) error {
	s.mu.Lock()
	delete(s.runs, id)
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	err := os.Remove(s.path(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
