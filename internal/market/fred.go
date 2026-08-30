package market

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FRED reads economic series from the St. Louis Fed.
//
// The CSV endpoint needs no key, which is why this is here at all: it opens
// macro-conditioned strategies — "hold equities only while the yield curve is
// positive" — to a tool that otherwise has prices and nothing else.
type FRED struct {
	HTTP    *http.Client
	BaseURL string
	// ReleaseLag overrides the built-in lag for a series, in calendar days.
	ReleaseLag map[string]int

	mu     sync.Mutex
	cached map[string]*EconSeries
}

// NewFRED builds a client.
func NewFRED() *FRED {
	return &FRED{
		HTTP: &http.Client{
			Timeout: 45 * time.Second,
			// HTTP/1.1 only. FRED's endpoint resets HTTP/2 streams
			// intermittently ("INTERNAL_ERROR; received from peer"), and a
			// nil TLSNextProto with ForceAttemptHTTP2 off is how Go is told
			// not to negotiate h2 at all.
			Transport: &http.Transport{
				ForceAttemptHTTP2: false,
				TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
			},
		},
		BaseURL:    "https://fred.stlouisfed.org",
		ReleaseLag: map[string]int{},
		cached:     map[string]*EconSeries{},
	}
}

// EconObs is one dated observation.
type EconObs struct {
	Date  Day     `json:"date"`
	Value float64 `json:"value"`
}

// EconSeries is a macro series plus how late it becomes knowable.
type EconSeries struct {
	ID  string    `json:"id"`
	Obs []EconObs `json:"obs"`
	// LagDays is how long after an observation's date the figure is actually
	// published. Reading a series without it is lookahead bias: US CPI for
	// March is stamped 1 March and is not public until mid-April, so a
	// backtest that reads it on 1 March is trading on a number nobody had.
	LagDays int `json:"lag_days"`
	// Revised marks a series whose published values are later restated. The
	// CSV endpoint serves the current vintage, not the vintage as of the
	// simulated day, so for these the value is right but the history is not
	// quite what anyone saw at the time.
	Revised bool `json:"revised"`
}

// defaultLags encode publication delay and revision behaviour for the series
// people actually reach for.
//
// Daily market rates are published same day and never restated, so they are
// safe to read as of the day. Everything derived from a survey or a national
// accounts estimate arrives weeks later and is then revised, and using one
// without a lag is a straightforward look into the future.
var defaultLags = map[string]struct {
	days    int
	revised bool
}{
	// Treasury and policy rates: same-day, final.
	"DGS1": {1, false}, "DGS2": {1, false}, "DGS5": {1, false},
	"DGS10": {1, false}, "DGS30": {1, false}, "DGS3MO": {1, false},
	"DFF": {1, false}, "SOFR": {1, false}, "FEDFUNDS": {32, false},
	"T10Y2Y": {1, false}, "T10Y3M": {1, false}, "T10YIE": {1, false},
	// Credit spreads and financial conditions: daily, final.
	"BAMLH0A0HYM2": {1, false}, "BAMLC0A0CM": {1, false},
	"NFCI": {8, false}, "VIXCLS": {1, false},
	// Surveys and national accounts: weeks late, and revised.
	"CPIAUCSL": {14, true}, "CPILFESL": {14, true},
	"UNRATE": {7, true}, "PAYEMS": {7, true},
	"GDP": {30, true}, "GDPC1": {30, true},
	"INDPRO": {17, true}, "UMCSENT": {14, true},
	"PCEPI": {30, true}, "M2SL": {25, true},
	"USREC": {180, true}, "ICSA": {5, false},
}

// Series fetches an economic series, caching it for the process.
func (f *FRED) Series(ctx context.Context, id string) (*EconSeries, error) {
	id = strings.ToUpper(strings.TrimSpace(id))
	if id == "" {
		return nil, fmt.Errorf("no series id given")
	}

	f.mu.Lock()
	if s, ok := f.cached[id]; ok {
		f.mu.Unlock()
		return s, nil
	}
	f.mu.Unlock()

	url := fmt.Sprintf("%s/graph/fredgraph.csv?id=%s", f.BaseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// Deliberately not the browser User-Agent the Yahoo client needs.
	//
	// FRED sits behind a filter that hangs — no response at all, not a 4xx —
	// on a request claiming to be Chrome, and answers a client that
	// identifies itself honestly. Go's default header does exactly that, so
	// the right move is to set nothing and let net/http supply it.
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("FRED returned %s for %s", resp.Status, id)
	}

	s, err := parseFREDCSV(id, resp.Body)
	if err != nil {
		return nil, err
	}
	if lag, ok := f.ReleaseLag[id]; ok {
		s.LagDays = lag
	} else if d, ok := defaultLags[id]; ok {
		s.LagDays, s.Revised = d.days, d.revised
	} else {
		// An unknown series is assumed to be a lagged, revised survey. That
		// is the conservative reading, and being a month late on a daily rate
		// costs far less than being a month early on a CPI print.
		s.LagDays, s.Revised = 30, true
	}

	f.mu.Lock()
	f.cached[id] = s
	f.mu.Unlock()
	return s, nil
}

// parseFREDCSV reads the two-column CSV the graph endpoint serves.
func parseFREDCSV(id string, r interface{ Read([]byte) (int, error) }) (*EconSeries, error) {
	buf := make([]byte, 0, 1<<20)
	tmp := make([]byte, 32*1024)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
		if len(buf) > 32<<20 {
			return nil, fmt.Errorf("FRED series %s is implausibly large", id)
		}
	}

	s := &EconSeries{ID: id}
	for i, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		if i == 0 {
			continue // header
		}
		day, err := ParseDay(strings.TrimSpace(parts[0]))
		if err != nil {
			continue
		}
		// FRED writes "." for a missing observation, which is common on
		// market holidays. Skipping keeps the series honest: AsOf then
		// returns the last real print rather than a zero.
		raw := strings.TrimSpace(parts[1])
		if raw == "." || raw == "" {
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		s.Obs = append(s.Obs, EconObs{Date: day, Value: v})
	}
	if len(s.Obs) == 0 {
		return nil, fmt.Errorf("FRED series %s had no usable observations", id)
	}
	sort.Slice(s.Obs, func(i, j int) bool { return s.Obs[i].Date < s.Obs[j].Date })
	return s, nil
}

// AsOf returns the most recent value knowable on day d, and whether one
// exists.
//
// "Knowable" is the whole point. An observation dated the 1st of the month
// that is not published until the 14th cannot be read on the 3rd, so the
// series is queried at d minus its release lag rather than at d.
func (s *EconSeries) AsOf(d Day) (float64, bool) {
	if s == nil || len(s.Obs) == 0 || d == "" {
		return 0, false
	}
	cutoff := d
	if s.LagDays > 0 {
		cutoff = d.Add(-s.LagDays)
	}
	i := sort.Search(len(s.Obs), func(i int) bool { return s.Obs[i].Date > cutoff })
	if i == 0 {
		return 0, false
	}
	return s.Obs[i-1].Value, true
}

// First and Last bound the series.
func (s *EconSeries) First() (EconObs, bool) {
	if s == nil || len(s.Obs) == 0 {
		return EconObs{}, false
	}
	return s.Obs[0], true
}

func (s *EconSeries) Last() (EconObs, bool) {
	if s == nil || len(s.Obs) == 0 {
		return EconObs{}, false
	}
	return s.Obs[len(s.Obs)-1], true
}
