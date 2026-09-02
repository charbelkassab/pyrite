package market

import (
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed assets/sp500_membership.csv
var membershipFS embed.FS

// Tenure is one stretch of a symbol's membership in an index.
type Tenure struct {
	Symbol string `json:"symbol"`
	// From is the first day the symbol was a member.
	From Day `json:"from"`
	// To is the last day, empty while the symbol is still a member. A symbol
	// that left and rejoined has several tenures.
	To Day `json:"to,omitempty"`
}

// Membership answers which symbols an index held on a given day.
//
// This is the fix for what docs/limitations.md calls the single largest
// distortion in the tool. A universe of "companies in the S&P 500" that means
// today's list is a universe chosen with hindsight: it already knows which
// companies survived. Real-time you would have been picking from a list
// containing names that went on to fail, and the difference is not small.
type Membership struct {
	Index   string
	tenures map[string][]Tenure
	// Earliest is the first date the record covers. Before it, membership is
	// unknown rather than empty, and callers are told which.
	Earliest Day
	Source   string
}

// LoadMembership reads the bundled table for an index, then overlays a
// user-supplied file at dir/<index>_membership.csv if present.
func LoadMembership(index, dir string) (*Membership, error) {
	index = strings.ToLower(strings.TrimSpace(index))
	if index == "" {
		index = "sp500"
	}
	m := &Membership{Index: index, tenures: map[string][]Tenure{}, Source: "bundled"}

	if dir != "" {
		override := filepath.Join(dir, index+"_membership.csv")
		if f, err := os.Open(override); err == nil {
			defer func() { _ = f.Close() }()
			if err := m.parse(f); err != nil {
				return nil, fmt.Errorf("parse %s: %w", override, err)
			}
			m.Source = override
			m.finalise()
			return m, nil
		}
	}

	b, err := membershipFS.ReadFile("assets/" + index + "_membership.csv")
	if err != nil {
		return nil, fmt.Errorf("no bundled membership table for %q", index)
	}
	if err := m.parse(strings.NewReader(string(b))); err != nil {
		return nil, fmt.Errorf("parse bundled %s membership: %w", index, err)
	}
	m.finalise()
	return m, nil
}

func (m *Membership) parse(r io.Reader) error {
	cr := csv.NewReader(r)
	cr.Comment = '#'
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true

	records, err := cr.ReadAll()
	if err != nil {
		return err
	}
	for i, rec := range records {
		if len(rec) < 2 {
			continue
		}
		sym := NormalizeSymbol(rec[0])
		if sym == "" || strings.EqualFold(sym, "SYMBOL") {
			continue
		}
		from, err := ParseDay(strings.TrimSpace(rec[1]))
		if err != nil {
			return fmt.Errorf("row %d (%s): %w", i+1, sym, err)
		}
		t := Tenure{Symbol: sym, From: from}
		if len(rec) >= 3 && strings.TrimSpace(rec[2]) != "" {
			to, err := ParseDay(strings.TrimSpace(rec[2]))
			if err != nil {
				return fmt.Errorf("row %d (%s): %w", i+1, sym, err)
			}
			t.To = to
		}
		m.tenures[sym] = append(m.tenures[sym], t)
	}
	return nil
}

func (m *Membership) finalise() {
	m.Earliest = ""
	for sym := range m.tenures {
		ts := m.tenures[sym]
		sort.Slice(ts, func(i, j int) bool { return ts[i].From < ts[j].From })
		m.tenures[sym] = ts
		if m.Earliest == "" || ts[0].From < m.Earliest {
			m.Earliest = ts[0].From
		}
	}
}

// MembersOn returns the index constituents as of a day, sorted.
func (m *Membership) MembersOn(d Day) []string {
	out := make([]string, 0, 512)
	for sym, ts := range m.tenures {
		for _, t := range ts {
			if t.From <= d && (t.To == "" || d <= t.To) {
				out = append(out, sym)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// WasMember reports whether a symbol was in the index on a day.
func (m *Membership) WasMember(symbol string, d Day) bool {
	for _, t := range m.tenures[NormalizeSymbol(symbol)] {
		if t.From <= d && (t.To == "" || d <= t.To) {
			return true
		}
	}
	return false
}

// EverMembers lists every symbol that held membership at any point inside
// [from, to].
//
// This is what a backtest must load data for: the union over the window, not
// the snapshot at its start. A strategy that rebalances monthly needs every
// name that was ever eligible, including the ones that were later dropped —
// which are precisely the names survivorship bias removes.
func (m *Membership) EverMembers(from, to Day) []string {
	out := make([]string, 0, 1024)
	for sym, ts := range m.tenures {
		for _, t := range ts {
			if t.From <= to && (t.To == "" || t.To >= from) {
				out = append(out, sym)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// Symbols lists every symbol the table knows about.
func (m *Membership) Symbols() []string {
	out := make([]string, 0, len(m.tenures))
	for sym := range m.tenures {
		out = append(out, sym)
	}
	sort.Strings(out)
	return out
}

// Covers reports whether the table can speak to a date at all.
func (m *Membership) Covers(d Day) bool {
	return m.Earliest != "" && d >= m.Earliest
}

// Tenures returns every row in the table, ordered by symbol then start date.
//
// The whole table rather than the window a run used: a tenure that ended
// before the window still decides that a symbol was not a member on day one,
// and dropping it would change the universe on re-run.
func (m *Membership) Tenures() []Tenure {
	out := make([]Tenure, 0, len(m.tenures))
	for _, sym := range m.Symbols() {
		out = append(out, m.tenures[sym]...)
	}
	return out
}

// ParseMembership reads a membership table from a reader.
//
// LoadMembership reads the embedded copy or a file on disk. A reproducibility
// bundle carries its own table and must serve it without writing it out
// first, so that a re-run cannot pick up whichever copy the local machine
// happens to have.
func ParseMembership(index string, r io.Reader) (*Membership, error) {
	index = strings.ToLower(strings.TrimSpace(index))
	if index == "" {
		index = "sp500"
	}
	m := &Membership{Index: index, tenures: map[string][]Tenure{}, Source: "bundle"}
	if err := m.parse(r); err != nil {
		return nil, err
	}
	m.finalise()
	return m, nil
}

// ---------------------------------------------------------------------------
// Building the table from Wikipedia.

// IndexChange is one add/remove event.
type IndexChange struct {
	Date    Day    `json:"date"`
	Added   string `json:"added,omitempty"`
	Removed string `json:"removed,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// WikipediaIndex fetches index membership history from Wikipedia.
//
// Wikipedia is not an authoritative source, and this is the honest reason to
// use it anyway: S&P's own constituent history is licensed and expensive, and
// the alternative on offer is not "better data" but "today's list pretending
// to be history". A citable, checkable approximation of what the index
// actually held beats a snapshot that silently knows the future.
type WikipediaIndex struct {
	HTTP      *http.Client
	BaseURL   string
	UserAgent string
}

// NewWikipediaIndex builds a client.
func NewWikipediaIndex(userAgent string) *WikipediaIndex {
	if userAgent == "" {
		userAgent = "pyrite (github.com/charbelkassab/pyrite)"
	}
	return &WikipediaIndex{
		HTTP:      &http.Client{Timeout: 60 * time.Second},
		BaseURL:   "https://en.wikipedia.org",
		UserAgent: userAgent,
	}
}

// wikitext fetches one page's source.
func (w *WikipediaIndex) wikitext(ctx context.Context, page string) (string, error) {
	url := fmt.Sprintf("%s/w/api.php?action=parse&page=%s&prop=wikitext&format=json",
		w.BaseURL, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", w.UserAgent)

	resp, err := w.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("wikipedia returned %s for %s", resp.Status, page)
	}

	var doc struct {
		Parse struct {
			Wikitext struct {
				Text string `json:"*"`
			} `json:"wikitext"`
		} `json:"parse"`
		Error *struct {
			Info string `json:"info"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", err
	}
	if doc.Error != nil {
		return "", fmt.Errorf("wikipedia: %s", doc.Error.Info)
	}
	if doc.Parse.Wikitext.Text == "" {
		return "", fmt.Errorf("wikipedia returned no wikitext for %s", page)
	}
	return doc.Parse.Wikitext.Text, nil
}

// tickerRe pulls a ticker out of a wikitext cell, which may be bare, wrapped
// in a symbol template, or a wiki link.
var tickerRe = regexp.MustCompile(`(?:\{\{[A-Za-z]*[Ss]ymbol\|([A-Za-z.\-]{1,8})\}\})|(?:\[\[[^\]|]*\|([A-Z.\-]{1,8})\]\])|^([A-Z][A-Z.\-]{0,7})$`)

// cellTicker extracts a ticker from one table cell.
func cellTicker(cell string) string {
	cell = strings.TrimSpace(cell)
	cell = strings.TrimPrefix(cell, "|")
	cell = strings.TrimSpace(cell)
	if cell == "" {
		return ""
	}
	if m := tickerRe.FindStringSubmatch(cell); m != nil {
		for _, g := range m[1:] {
			if g != "" {
				return NormalizeSymbol(g)
			}
		}
	}
	// Fall back to stripping wiki markup and taking a bare token.
	cell = regexp.MustCompile(`\[\[|\]\]|'''|''`).ReplaceAllString(cell, "")
	cell = strings.TrimSpace(strings.SplitN(cell, "|", 2)[0])
	if len(cell) <= 8 && cell != "" && strings.ToUpper(cell) == cell {
		return NormalizeSymbol(cell)
	}
	return ""
}

// wikiDate parses the date formats the tables use.
func wikiDate(s string) (Day, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "|"))
	s = regexp.MustCompile(`<ref.*?</ref>|<ref[^>]*/>`).ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	for _, layout := range []string{"January 2, 2006", "2 January 2006", "2006-01-02", "January 2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return NewDay(t), nil
		}
	}
	return "", fmt.Errorf("unrecognised date %q", s)
}

// splitRows breaks a wikitable into its rows.
func splitRows(table string) []string {
	parts := strings.Split(table, "\n|-")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitCells breaks one row into cells, handling both the "|| a || b" and
// leading-pipe-per-line conventions Wikipedia allows interchangeably.
func splitCells(row string) []string {
	var cells []string
	for _, line := range strings.Split(row, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "!") {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			// A continuation of the previous cell.
			if len(cells) > 0 {
				cells[len(cells)-1] += " " + line
			}
			continue
		}
		line = strings.TrimPrefix(line, "|")
		for _, c := range strings.Split(line, "||") {
			cells = append(cells, strings.TrimSpace(c))
		}
	}
	return cells
}

// CurrentMembers parses the current-constituents table, returning each
// symbol's date added where the page records one.
func (w *WikipediaIndex) CurrentMembers(ctx context.Context) (map[string]Day, error) {
	text, err := w.wikitext(ctx, "List_of_S%26P_500_companies")
	if err != nil {
		return nil, err
	}
	i := strings.Index(text, `id="constituents"`)
	if i < 0 {
		return nil, fmt.Errorf("could not find the constituents table")
	}
	table := text[i:]
	if j := strings.Index(table, "\n|}"); j > 0 {
		table = table[:j]
	}

	out := map[string]Day{}
	for _, row := range splitRows(table) {
		cells := splitCells(row)
		if len(cells) < 6 {
			continue
		}
		sym := cellTicker(cells[0])
		if sym == "" {
			continue
		}
		// Column 5 is "Date added"; a missing or unparseable one is not fatal.
		day, err := wikiDate(cells[5])
		if err != nil {
			day = ""
		}
		out[sym] = day
	}
	if len(out) < 100 {
		return nil, fmt.Errorf("only parsed %d constituents, which cannot be right", len(out))
	}
	return out, nil
}

// Changes parses the historical add/remove table, newest first.
func (w *WikipediaIndex) Changes(ctx context.Context) ([]IndexChange, error) {
	text, err := w.wikitext(ctx, "Historical_components_of_the_S%26P_500")
	if err != nil {
		return nil, err
	}
	i := strings.Index(text, `id="changes"`)
	if i < 0 {
		return nil, fmt.Errorf("could not find the changes table")
	}
	table := text[i:]
	if j := strings.Index(table, "\n|}"); j > 0 {
		table = table[:j]
	}

	var out []IndexChange
	for _, row := range splitRows(table) {
		cells := splitCells(row)
		if len(cells) < 5 {
			continue
		}
		day, err := wikiDate(cells[0])
		if err != nil {
			continue // the header rows, and any malformed entry
		}
		c := IndexChange{
			Date:    day,
			Added:   cellTicker(cells[1]),
			Removed: cellTicker(cells[3]),
		}
		if len(cells) >= 6 {
			c.Reason = strings.TrimSpace(cells[5])
		}
		if c.Added == "" && c.Removed == "" {
			continue
		}
		out = append(out, c)
	}
	if len(out) < 50 {
		return nil, fmt.Errorf("only parsed %d changes, which cannot be right", len(out))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}

// BuildMembership reconstructs tenures by walking the change log backwards
// from today's constituents.
//
// The direction matters. Today's list is the only membership we know for
// certain, so history is recovered by undoing each change in reverse: before
// the date "AAA replaced BBB", AAA was not a member and BBB was.
func BuildMembership(current map[string]Day, changes []IndexChange) []Tenure {
	// Walking backwards, `open` holds the date each currently-known member's
	// tenure ends — empty for a present member.
	type span struct{ from, to Day }
	spans := map[string][]span{}

	member := map[string]bool{}
	endsAt := map[string]Day{}
	for sym := range current {
		member[sym] = true
		endsAt[sym] = "" // still in the index
	}

	sort.SliceStable(changes, func(i, j int) bool { return changes[i].Date > changes[j].Date })
	for _, c := range changes {
		// Undo the addition: before this date, the added symbol was not a
		// member, so its tenure started here.
		if c.Added != "" {
			if member[c.Added] {
				spans[c.Added] = append(spans[c.Added], span{from: c.Date, to: endsAt[c.Added]})
				delete(member, c.Added)
				delete(endsAt, c.Added)
			}
		}
		// Undo the removal: before this date, the removed symbol was a member,
		// and its tenure ended the day before this change took effect.
		if c.Removed != "" && !member[c.Removed] {
			member[c.Removed] = true
			endsAt[c.Removed] = c.Date.Add(-1)
		}
	}

	// Anything still open at the far end of the change log started at or
	// before the earliest date we can speak to.
	var earliest Day
	if len(changes) > 0 {
		earliest = changes[len(changes)-1].Date
	}
	for sym := range member {
		// The current table's "date added" is a stated fact and always wins
		// over the change log's reach, which is only a lower bound.
		//
		// It also covers the case the change log structurally cannot: a
		// company whose ticker changed. Meta joined as FB in 2013 and is
		// listed as META today, and Wikipedia deliberately keeps ticker
		// changes out of the change table — so the log never records META
		// being added, and only the current table knows when it happened.
		from := earliest
		if d, ok := current[sym]; ok && d != "" {
			from = d
		}
		if from == "" {
			from = earliest
		}
		if from == "" {
			continue
		}
		spans[sym] = append(spans[sym], span{from: from, to: endsAt[sym]})
	}

	var out []Tenure
	for sym, ss := range spans {
		for _, s := range ss {
			if s.from == "" {
				continue
			}
			out = append(out, Tenure{Symbol: sym, From: s.from, To: s.to})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].From < out[j].From
	})
	return out
}

// WriteMembershipCSV renders tenures in the format LoadMembership reads.
func WriteMembershipCSV(w io.Writer, index string, tenures []Tenure, changes int) error {
	fmt.Fprintf(w, "# pyrite — point-in-time %s membership\n#\n", strings.ToUpper(index))
	fmt.Fprintf(w, "# Generated %s by `pyrite ingest index`.\n", time.Now().UTC().Format("2006-01-02"))
	fmt.Fprintf(w, "# Reconstructed from the current constituent list and %d recorded\n", changes)
	fmt.Fprintf(w, "# add/remove events, by undoing each change in reverse from today.\n#\n")
	fmt.Fprintf(w, "# WHY THIS FILE EXISTS\n")
	fmt.Fprintf(w, "# A universe of \"companies in the index\" that means today's list is a\n")
	fmt.Fprintf(w, "# universe chosen with hindsight: it already knows which companies\n")
	fmt.Fprintf(w, "# survived. This table says who was actually in the index on a given day,\n")
	fmt.Fprintf(w, "# including the names that were later dropped.\n#\n")
	fmt.Fprintf(w, "# WHAT IS STILL WRONG\n")
	fmt.Fprintf(w, "#  * The source is Wikipedia, not S&P. It is citable and checkable, but it\n")
	fmt.Fprintf(w, "#    is not authoritative, and the change log thins out the further back\n")
	fmt.Fprintf(w, "#    you go.\n")
	fmt.Fprintf(w, "#  * Membership is only half the problem. Backtesting a dropped name also\n")
	fmt.Fprintf(w, "#    needs its prices, and free vendors do not serve delisted securities.\n")
	fmt.Fprintf(w, "#    Point PYRITE_CSV_DIR at your own data for those.\n")
	fmt.Fprintf(w, "#  * A symbol that left and rejoined has several rows.\n#\n")
	fmt.Fprintf(w, "# format: symbol,from,to   (an empty `to` means still a member)\n")
	fmt.Fprintf(w, "symbol,from,to\n")

	cw := csv.NewWriter(w)
	for _, t := range tenures {
		if err := cw.Write([]string{t.Symbol, string(t.From), string(t.To)}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
