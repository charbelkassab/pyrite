package market

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EDGAR is a client for the SEC's XBRL company-facts API.
//
// This exists to replace guesswork with filings. Ranking a universe by market
// capitalisation needs shares outstanding *as of the historical date*, and the
// docs long said no free keyless source provides that series. EDGAR does: every
// 10-K and 10-Q carries a share count on its cover page, tagged as
// dei:EntityCommonStockSharesOutstanding, and the API serves the whole history
// per company with the filing date attached. No key, no account — the SEC asks
// only for a declared User-Agent and no more than ten requests a second.
type EDGAR struct {
	HTTP      *http.Client
	UserAgent string
	// DataURL and WWWURL are split because the SEC serves the ticker
	// directory and the XBRL facts from different hosts. Overridable for
	// tests.
	DataURL string
	WWWURL  string
	// MinInterval throttles requests. The SEC's published ceiling is ten per
	// second; this defaults to a deliberately polite eight.
	MinInterval time.Duration

	last time.Time
}

// NewEDGAR builds a client. userAgent must identify the caller — the SEC
// rejects requests that do not, and a generic string is grounds for a block.
func NewEDGAR(userAgent string) *EDGAR {
	return &EDGAR{
		HTTP:        &http.Client{Timeout: 30 * time.Second},
		UserAgent:   userAgent,
		DataURL:     "https://data.sec.gov",
		WWWURL:      "https://www.sec.gov",
		MinInterval: 125 * time.Millisecond,
	}
}

// ShareObservation is one disclosed share count.
type ShareObservation struct {
	// Filed is the date the number became public. This, not AsOf, is the date
	// a backtest may start using it: a count measured on the quarter end was
	// not knowable until the filing appeared, and dating rows by AsOf would
	// hand every historical run a few weeks of free information.
	Filed Day `json:"filed"`
	// AsOf is the cover date the count was measured on.
	AsOf   Day     `json:"as_of"`
	Shares float64 `json:"shares"`
	// Accession is the filing's SEC accession number, so any row in the
	// generated table can be traced back to the document it came from.
	Accession string `json:"accession"`
	Form      string `json:"form"`
	Tag       string `json:"tag"`
	// Exact distinguishes a stated point-in-time count from an approximation
	// such as a weighted period average, which is the best some filers make
	// machine-readable.
	Exact bool `json:"exact"`
}

// Company identifies a filer.
type Company struct {
	CIK    string `json:"cik"`
	Ticker string `json:"ticker"`
	Name   string `json:"name"`
}

// tickerEntry is one row of the SEC's company_tickers.json.
type tickerEntry struct {
	CIK    json.Number `json:"cik_str"`
	Ticker string      `json:"ticker"`
	Title  string      `json:"title"`
}

// Companies fetches the SEC's ticker-to-CIK directory, keyed by upper-case
// ticker.
func (e *EDGAR) Companies(ctx context.Context) (map[string]Company, error) {
	var raw map[string]tickerEntry
	if err := e.getJSON(ctx, e.WWWURL+"/files/company_tickers.json", &raw); err != nil {
		return nil, fmt.Errorf("fetch SEC ticker directory: %w", err)
	}
	out := make(map[string]Company, len(raw))
	for _, v := range raw {
		if v.Ticker == "" {
			continue
		}
		n, err := v.CIK.Int64()
		if err != nil {
			continue
		}
		sym := strings.ToUpper(v.Ticker)
		out[sym] = Company{
			CIK:    fmt.Sprintf("CIK%010d", n),
			Ticker: sym,
			Name:   v.Title,
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("SEC ticker directory was empty")
	}
	return out, nil
}

// factsDocument is the shape of an XBRL company-facts document.
//
// Units are held as raw JSON rather than decoded eagerly: the API is not
// consistent about them. Some filers return an empty object where an array is
// expected, and a strict decode of the whole document fails on that one field,
// taking every usable tag down with it.
type factsDocument struct {
	CIK   json.Number `json:"cik"`
	Facts map[string]map[string]struct {
		Units map[string]json.RawMessage `json:"units"`
	} `json:"facts"`
}

// factRow is one XBRL observation.
type factRow struct {
	End   string      `json:"end"`
	Val   json.Number `json:"val"`
	Accn  string      `json:"accn"`
	Form  string      `json:"form"`
	Filed string      `json:"filed"`
}

// shareTag is a candidate source of a share count, in descending order of how
// closely it answers the question a market cap actually asks.
type shareTag struct {
	taxonomy string
	tag      string
	// exact marks a tag that states shares outstanding at a point in time.
	// The rest are approximations, and the generated table says which is
	// which so nobody has to guess later.
	exact bool
	note  string
}

// shareTags are tried in order and the first that yields data wins.
//
// The ordering is not arbitrary. The dei cover-page tag means "shares
// outstanding right now", which is exactly right. Weighted-average tags are
// period averages and sit above CommonStockSharesIssued deliberately: issued
// counts include treasury stock, so for a company that has bought back heavily
// they are wrong by a wide margin — Coca-Cola's issued count is over 60% above
// its outstanding count — while the weighted average lands within a fraction
// of a percent.
//
// Multi-class filers such as Meta report their counts against a share-class
// axis, and the SEC's XBRL API does not expose dimensional facts at all. For
// those companies the weighted-average tags are the only thing available, and
// having an approximation beats dropping the symbol from every ranking.
var shareTags = []shareTag{
	{"dei", "EntityCommonStockSharesOutstanding", true, ""},
	{"us-gaap", "CommonStockSharesOutstanding", true, "measured at period end"},
	{"us-gaap", "WeightedAverageNumberOfSharesOutstandingBasic", false, "period average, not a point-in-time count"},
	{"us-gaap", "WeightedAverageNumberOfDilutedSharesOutstanding", false, "diluted period average, slightly overstates"},
	{"us-gaap", "CommonStockSharesIssued", false, "issued, includes treasury stock"},
}

// CompanyFacts fetches every XBRL fact the SEC holds for a filer.
func (e *EDGAR) CompanyFacts(ctx context.Context, cik string) (*factsDocument, error) {
	var doc factsDocument
	url := fmt.Sprintf("%s/api/xbrl/companyfacts/%s.json", e.DataURL, cik)
	if err := e.getJSON(ctx, url, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// SharesOutstanding returns every disclosed share count for a filer, oldest
// filing first, together with the basis it was taken from.
func (e *EDGAR) SharesOutstanding(ctx context.Context, cik string) ([]ShareObservation, error) {
	doc, err := e.CompanyFacts(ctx, cik)
	if err != nil {
		return nil, err
	}

	for _, t := range shareTags {
		fact, ok := doc.Facts[t.taxonomy][t.tag]
		if !ok {
			continue
		}
		raw, ok := fact.Units["shares"]
		if !ok {
			continue
		}
		var rows []factRow
		if err := json.Unmarshal(raw, &rows); err != nil {
			// An empty object where an array belongs. Not an error worth
			// surfacing; just try the next tag.
			continue
		}

		out := make([]ShareObservation, 0, len(rows))
		for _, r := range rows {
			v, err := r.Val.Float64()
			if err != nil || v <= 0 || r.Filed == "" {
				continue
			}
			filed, err := ParseDay(r.Filed)
			if err != nil {
				continue
			}
			asOf := filed
			if r.End != "" {
				if d, err := ParseDay(r.End); err == nil {
					asOf = d
				}
			}
			out = append(out, ShareObservation{
				Filed: filed, AsOf: asOf, Shares: v,
				Accession: r.Accn, Form: r.Form,
				Tag:   t.taxonomy + ":" + t.tag,
				Exact: t.exact,
			})
		}
		if len(out) == 0 {
			continue
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Filed != out[j].Filed {
				return out[i].Filed < out[j].Filed
			}
			// Same filing date: the later measurement wins.
			return out[i].AsOf < out[j].AsOf
		})
		return out, nil
	}
	return nil, fmt.Errorf("no share count published in XBRL under any known tag")
}

// CompressObservations reduces a filing history to piecewise-constant rows.
//
// A large filer discloses a share count four times a year for decades, and
// almost every one of those is a fraction of a percent from the last. Keeping
// them all would bloat the table without changing a single ranking, so a row
// survives only when it moves the count by more than threshold — with the
// first and last always kept so the endpoints stay exact.
func CompressObservations(obs []ShareObservation, threshold float64) []ShareObservation {
	if len(obs) <= 2 {
		return obs
	}
	if threshold <= 0 {
		threshold = 0.005
	}
	out := []ShareObservation{obs[0]}
	last := obs[0].Shares
	for i := 1; i < len(obs)-1; i++ {
		// Same-day duplicates: keep the one already taken.
		if obs[i].Filed == out[len(out)-1].Filed {
			continue
		}
		if last <= 0 {
			continue
		}
		if change := (obs[i].Shares - last) / last; change > threshold || change < -threshold {
			out = append(out, obs[i])
			last = obs[i].Shares
		}
	}
	if tail := obs[len(obs)-1]; tail.Filed != out[len(out)-1].Filed {
		out = append(out, tail)
	}
	return out
}

// IngestReport summarises what an ingest run produced.
type IngestReport struct {
	Symbols []string          `json:"symbols"`
	Rows    int               `json:"rows"`
	Skipped map[string]string `json:"skipped,omitempty"`
	// Approximate names the symbols whose counts came from something other
	// than a stated point-in-time figure, and what was used instead. These
	// are still worth having — an approximate cap ranks a mega cap correctly
	// against its peers — but the reader is told rather than left to assume.
	Approximate map[string]string `json:"approximate,omitempty"`
	Generated   time.Time         `json:"generated"`
}

// BuildSharesTable fetches share histories for the given tickers and writes a
// shares_outstanding.csv compatible with LoadFundamentals.
//
// progress, when non-nil, is called once per symbol.
func (e *EDGAR) BuildSharesTable(ctx context.Context, symbols []string, threshold float64, w io.Writer, progress func(sym string, i, n int)) (*IngestReport, error) {
	companies, err := e.Companies(ctx)
	if err != nil {
		return nil, err
	}

	rep := &IngestReport{
		Skipped:     map[string]string{},
		Approximate: map[string]string{},
		Generated:   time.Now().UTC(),
	}
	type row struct {
		sym  string
		obs  ShareObservation
		name string
	}
	var rows []row

	for i, sym := range DedupeSymbols(symbols) {
		if progress != nil {
			progress(sym, i, len(symbols))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		c, ok := companies[strings.ToUpper(sym)]
		if !ok {
			rep.Skipped[sym] = "not a US filer in the SEC ticker directory"
			continue
		}
		obs, err := e.SharesOutstanding(ctx, c.CIK)
		if err != nil {
			rep.Skipped[sym] = err.Error()
			continue
		}
		if len(obs) == 0 {
			rep.Skipped[sym] = "no share count disclosed in XBRL"
			continue
		}
		up := strings.ToUpper(sym)
		for _, o := range CompressObservations(obs, threshold) {
			rows = append(rows, row{sym: up, obs: o, name: c.Name})
		}
		if !obs[0].Exact {
			rep.Approximate[up] = obs[0].Tag
		}
		rep.Symbols = append(rep.Symbols, up)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].sym != rows[j].sym {
			return rows[i].sym < rows[j].sym
		}
		return rows[i].obs.Filed < rows[j].obs.Filed
	})

	fmt.Fprintf(w, "# pyrite — point-in-time shares outstanding, from SEC EDGAR XBRL\n")
	fmt.Fprintf(w, "#\n")
	fmt.Fprintf(w, "# Generated %s by `pyrite ingest edgar`.\n", rep.Generated.Format("2006-01-02"))
	fmt.Fprintf(w, "# Source: https://data.sec.gov/api/xbrl/companyconcept/...\n")
	fmt.Fprintf(w, "#\n")
	fmt.Fprintf(w, "# Each row's date is the date the filing was PUBLISHED, not the date the\n")
	fmt.Fprintf(w, "# count was measured. A share count on a 31 March cover page did not become\n")
	fmt.Fprintf(w, "# knowable until the 10-Q appeared in May, and dating rows by the measurement\n")
	fmt.Fprintf(w, "# date would hand every historical backtest several weeks of free information.\n")
	fmt.Fprintf(w, "# The as_of and accession columns preserve the measurement date and the\n")
	fmt.Fprintf(w, "# document, so any row can be traced back to its filing.\n")
	fmt.Fprintf(w, "#\n")
	if len(rep.Approximate) > 0 {
		fmt.Fprintf(w, "# The following symbols do not publish a point-in-time count in a form the\n")
		fmt.Fprintf(w, "# SEC API exposes — multi-class filers report against a share-class axis,\n")
		fmt.Fprintf(w, "# and dimensional facts are not served. Their rows come from the tag named\n")
		fmt.Fprintf(w, "# in the last column and are approximate:\n")
		approx := make([]string, 0, len(rep.Approximate))
		for sym := range rep.Approximate {
			approx = append(approx, sym)
		}
		sort.Strings(approx)
		for _, sym := range approx {
			fmt.Fprintf(w, "#   %-8s %s\n", sym, rep.Approximate[sym])
		}
		fmt.Fprintf(w, "#\n")
	}
	fmt.Fprintf(w, "# symbol,from,shares,name,as_of,accession,form,tag\n")

	cw := csv.NewWriter(w)
	for _, r := range rows {
		if err := cw.Write([]string{
			r.sym,
			string(r.obs.Filed),
			strconv.FormatFloat(r.obs.Shares, 'f', -1, 64),
			r.name,
			string(r.obs.AsOf),
			r.obs.Accession,
			r.obs.Form,
			r.obs.Tag,
		}); err != nil {
			return nil, err
		}
	}
	cw.Flush()
	rep.Rows = len(rows)
	return rep, cw.Error()
}

// getJSON performs a throttled, identified GET and decodes the body.
func (e *EDGAR) getJSON(ctx context.Context, url string, into any) error {
	if e.UserAgent == "" {
		return fmt.Errorf("the SEC requires a User-Agent identifying you, e.g. \"Jane Doe jane@example.com\"")
	}
	e.throttle()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", e.UserAgent)
	req.Header.Set("Accept", "application/json")
	// Deliberately no Accept-Encoding. Setting it by hand switches off
	// net/http's transparent gzip handling, and the SEC gzips everything —
	// so the body would arrive compressed and every decode, including the
	// error path, would return binary noise instead of a readable message.

	client := e.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("not filed with the SEC under this tag")
	}
	if resp.StatusCode == http.StatusForbidden {
		// Overwhelmingly the User-Agent, so say so rather than making the
		// caller guess from a bare 403.
		return fmt.Errorf("SEC refused the request (403). It requires a User-Agent naming a real "+
			"contact, in the form \"Organisation name contact@example.com\"; %q was sent", e.UserAgent)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return fmt.Errorf("SEC returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// throttle spaces requests to stay inside the SEC's rate limit.
func (e *EDGAR) throttle() {
	if e.MinInterval <= 0 {
		return
	}
	if !e.last.IsZero() {
		if wait := e.MinInterval - time.Since(e.last); wait > 0 {
			time.Sleep(wait)
		}
	}
	e.last = time.Now()
}
