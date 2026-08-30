package market

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// edgarServer stands in for the SEC so the tests need no network.
func edgarServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/files/company_tickers.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("request reached the SEC without a User-Agent")
		}
		w.Write([]byte(`{
			"0": {"cik_str": 320193, "ticker": "AAPL", "title": "Apple Inc."},
			"1": {"cik_str": 789019, "ticker": "MSFT", "title": "Microsoft Corporation"},
			"2": {"cik_str": 1326801, "ticker": "META", "title": "Meta Platforms, Inc."}
		}`))
	})

	mux.HandleFunc("/api/xbrl/companyfacts/CIK0000320193.json",
		func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{
				"cik": 320193,
				"facts": {"dei": {"EntityCommonStockSharesOutstanding": {"units": {"shares": [
					{"end":"2020-03-28","val":4334335000,"accn":"0000320193-20-000052","form":"10-Q","filed":"2020-05-01"},
					{"end":"2020-06-27","val":4275634000,"accn":"0000320193-20-000062","form":"10-Q","filed":"2020-07-31"},
					{"end":"2020-09-26","val":16788096000,"accn":"0000320193-20-000096","form":"10-K","filed":"2020-10-30"}
				]}}}}
			}`))
		})

	// Microsoft's preferred tag is present but empty — the exact shape the
	// live API returns for some filers — so the client must skip past it
	// rather than failing the symbol.
	mux.HandleFunc("/api/xbrl/companyfacts/CIK0000789019.json",
		func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{
				"cik": 789019,
				"facts": {
					"dei": {"EntityCommonStockSharesOutstanding": {"units": {"shares": {}}}},
					"us-gaap": {"CommonStockSharesOutstanding": {"units": {"shares": [
						{"end":"2020-06-30","val":7571000000,"accn":"0001564590-20-034944","form":"10-K","filed":"2020-07-30"}
					]}}}
				}
			}`))
		})

	// A multi-class filer with nothing but a weighted average available.
	mux.HandleFunc("/api/xbrl/companyfacts/CIK0001326801.json",
		func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{
				"cik": 1326801,
				"facts": {"us-gaap": {"WeightedAverageNumberOfSharesOutstandingBasic": {"units": {"shares": [
					{"end":"2020-06-30","val":2849000000,"accn":"0001326801-20-000076","form":"10-Q","filed":"2020-07-30"}
				]}}}}
			}`))
		})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testEDGAR(t *testing.T, srv *httptest.Server) *EDGAR {
	e := NewEDGAR("pyrite test suite test@example.com")
	e.DataURL = srv.URL
	e.WWWURL = srv.URL
	e.MinInterval = 0 // no throttling against a local server
	return e
}

func TestEDGARResolvesTickersToCIK(t *testing.T) {
	e := testEDGAR(t, edgarServer(t))
	got, err := e.Companies(context.Background())
	if err != nil {
		t.Fatalf("companies: %v", err)
	}
	aapl, ok := got["AAPL"]
	if !ok {
		t.Fatal("AAPL missing from the directory")
	}
	// The API demands a zero-padded ten-digit CIK.
	if aapl.CIK != "CIK0000320193" {
		t.Errorf("CIK: got %q, want CIK0000320193", aapl.CIK)
	}
	if aapl.Name != "Apple Inc." {
		t.Errorf("name: got %q", aapl.Name)
	}
}

func TestEDGARDatesRowsByFilingNotMeasurement(t *testing.T) {
	e := testEDGAR(t, edgarServer(t))
	obs, err := e.SharesOutstanding(context.Background(), "CIK0000320193")
	if err != nil {
		t.Fatalf("shares: %v", err)
	}
	if len(obs) != 3 {
		t.Fatalf("want 3 observations, got %d", len(obs))
	}
	first := obs[0]
	// This is the whole point: the count measured on 28 March was not public
	// until 1 May, and a backtest must not see it before then.
	if first.Filed != "2020-05-01" {
		t.Errorf("row should be dated by filing: got %v, want 2020-05-01", first.Filed)
	}
	if first.AsOf != "2020-03-28" {
		t.Errorf("measurement date lost: got %v", first.AsOf)
	}
	if first.Accession != "0000320193-20-000052" {
		t.Errorf("accession not preserved: %q", first.Accession)
	}
	if first.Tag != "dei:EntityCommonStockSharesOutstanding" {
		t.Errorf("tag not recorded: %q", first.Tag)
	}
}

func TestEDGARSkipsEmptyUnitsAndFallsBack(t *testing.T) {
	e := testEDGAR(t, edgarServer(t))
	obs, err := e.SharesOutstanding(context.Background(), "CIK0000789019")
	if err != nil {
		t.Fatalf("shares: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("want 1 observation from the fallback tag, got %d", len(obs))
	}
	if obs[0].Tag != "us-gaap:CommonStockSharesOutstanding" {
		t.Errorf("fallback tag not recorded: %q", obs[0].Tag)
	}
	if !obs[0].Exact {
		t.Error("a stated outstanding count should be marked exact")
	}
}

func TestEDGARMarksApproximateBasis(t *testing.T) {
	e := testEDGAR(t, edgarServer(t))
	obs, err := e.SharesOutstanding(context.Background(), "CIK0001326801")
	if err != nil {
		t.Fatalf("shares: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("want 1 observation, got %d", len(obs))
	}
	if obs[0].Exact {
		t.Error("a weighted period average must not be reported as exact")
	}
	if obs[0].Tag != "us-gaap:WeightedAverageNumberOfSharesOutstandingBasic" {
		t.Errorf("basis not recorded: %q", obs[0].Tag)
	}
}

func TestBuildSharesTableFlagsApproximateSymbols(t *testing.T) {
	e := testEDGAR(t, edgarServer(t))
	var buf bytes.Buffer
	rep, err := e.BuildSharesTable(context.Background(), []string{"AAPL", "META"}, 0.005, &buf, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := rep.Approximate["META"]; !ok {
		t.Errorf("META should be flagged as approximate, got %v", rep.Approximate)
	}
	if _, ok := rep.Approximate["AAPL"]; ok {
		t.Error("AAPL publishes an exact count and should not be flagged")
	}
	if !strings.Contains(buf.String(), "approximate") {
		t.Error("the generated file should warn about approximate rows in its header")
	}
}

func TestCompressObservationsKeepsMaterialMoves(t *testing.T) {
	obs := []ShareObservation{
		{Filed: "2020-01-01", Shares: 1000},
		{Filed: "2020-04-01", Shares: 1001}, // +0.1%: noise
		{Filed: "2020-07-01", Shares: 1002}, // still noise
		{Filed: "2020-10-01", Shares: 2000}, // doubled: a split, keep it
		{Filed: "2021-01-01", Shares: 2001},
	}
	got := CompressObservations(obs, 0.005)
	if len(got) != 3 {
		t.Fatalf("want first, the split and last: got %d rows %+v", len(got), got)
	}
	if got[0].Filed != "2020-01-01" || got[len(got)-1].Filed != "2021-01-01" {
		t.Errorf("endpoints must be preserved: %+v", got)
	}
	if got[1].Shares != 2000 {
		t.Errorf("the material move was dropped: %+v", got)
	}
}

func TestCompressObservationsShortInputUntouched(t *testing.T) {
	obs := []ShareObservation{{Filed: "2020-01-01", Shares: 10}, {Filed: "2020-06-01", Shares: 11}}
	if got := CompressObservations(obs, 0.005); len(got) != 2 {
		t.Errorf("two rows should survive intact, got %d", len(got))
	}
}

func TestBuildSharesTableRoundTripsThroughTheParser(t *testing.T) {
	e := testEDGAR(t, edgarServer(t))
	var buf bytes.Buffer
	rep, err := e.BuildSharesTable(context.Background(),
		[]string{"AAPL", "MSFT", "NOTREAL"}, 0.005, &buf, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rep.Rows == 0 {
		t.Fatal("no rows written")
	}
	if len(rep.Symbols) != 2 {
		t.Errorf("want 2 ingested symbols, got %v", rep.Symbols)
	}
	if _, ok := rep.Skipped["NOTREAL"]; !ok {
		t.Error("an unknown ticker should be reported as skipped, not fatal")
	}

	// The generated file must be readable by the loader it is meant to feed.
	f := &Fundamentals{shares: map[string][]sharesRow{}, names: map[string]string{}}
	if err := f.parse(strings.NewReader(buf.String())); err != nil {
		t.Fatalf("generated CSV does not parse: %v\n%s", err, buf.String())
	}
	syms := f.Symbols()
	if len(syms) != 2 {
		t.Fatalf("parsed symbols: %v", syms)
	}

	// And it must answer point-in-time questions correctly. Dates before the
	// first filing extrapolate backwards from the earliest row, which is the
	// loader's documented behaviour, so the assertion that matters is that a
	// date between two filings gets the earlier one and not the later.
	got, ok := f.SharesOutstanding("AAPL", "2020-06-01")
	if !ok {
		t.Fatal("no share count after the first filing")
	}
	if got != 4334335000 {
		t.Errorf("share count as of 2020-06-01: got %v, want 4334335000", got)
	}
	// The 10-K filed on 30 October must not be visible in September.
	if v, _ := f.SharesOutstanding("AAPL", "2020-09-30"); v != 4275634000 {
		t.Errorf("a later filing leaked backwards: got %v, want 4275634000", v)
	}
	if v, _ := f.SharesOutstanding("AAPL", "2020-11-01"); v != 16788096000 {
		t.Errorf("share count after the 10-K: got %v, want 16788096000", v)
	}
}

func TestEDGARRefusesWithoutUserAgent(t *testing.T) {
	srv := edgarServer(t)
	e := testEDGAR(t, srv)
	e.UserAgent = ""
	if _, err := e.Companies(context.Background()); err == nil {
		t.Fatal("expected an error when no User-Agent is set")
	} else if !strings.Contains(err.Error(), "User-Agent") {
		t.Errorf("error should explain the requirement: %v", err)
	}
}
