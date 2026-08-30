package market

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fredServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "id=") {
			t.Errorf("no series id in the request: %s", r.URL.RawQuery)
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testFRED(t *testing.T, body string) *FRED {
	f := NewFRED()
	f.BaseURL = fredServer(t, body).URL
	return f
}

func TestFREDParsesTheCSV(t *testing.T) {
	f := testFRED(t, "observation_date,DGS10\n2024-01-02,3.95\n2024-01-03,3.91\n2024-01-04,4.00\n")
	s, err := f.Series(context.Background(), "DGS10")
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(s.Obs) != 3 {
		t.Fatalf("want 3 observations, got %d", len(s.Obs))
	}
	if s.Obs[0].Date != "2024-01-02" || s.Obs[0].Value != 3.95 {
		t.Errorf("first observation wrong: %+v", s.Obs[0])
	}
	// A daily market rate is published same day and never restated.
	if s.Revised {
		t.Error("DGS10 is not a revised series")
	}
	if s.LagDays > 1 {
		t.Errorf("DGS10 should have no meaningful release lag, got %d days", s.LagDays)
	}
}

func TestFREDSkipsMissingObservations(t *testing.T) {
	// FRED writes "." on market holidays. Reading that as zero would put a
	// 0% ten-year yield into a strategy's arithmetic.
	f := testFRED(t, "observation_date,DGS10\n2024-01-02,3.95\n2024-01-03,.\n2024-01-04,4.00\n")
	s, err := f.Series(context.Background(), "DGS10")
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(s.Obs) != 2 {
		t.Fatalf("the missing value should be skipped, got %d observations", len(s.Obs))
	}
	// And AsOf on the holiday returns the last real print, not zero.
	v, ok := s.AsOf("2024-01-03")
	if !ok || v != 3.95 {
		t.Errorf("holiday lookup: got %v %v, want 3.95", v, ok)
	}
}

// The lag is what makes a macro series usable in a backtest at all.
func TestReleaseLagPreventsReadingTheFuture(t *testing.T) {
	// CPI for March is stamped 1 March and published in mid-April.
	f := testFRED(t, "observation_date,CPIAUCSL\n2024-02-01,310.0\n2024-03-01,312.0\n2024-04-01,313.0\n")
	s, err := f.Series(context.Background(), "CPIAUCSL")
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if !s.Revised || s.LagDays < 7 {
		t.Fatalf("CPI should be marked revised and lagged: %+v", *s)
	}

	// On 5 March, the March print does not exist yet.
	v, ok := s.AsOf("2024-03-05")
	if !ok {
		t.Fatal("February's figure should be available in early March")
	}
	if v != 310.0 {
		t.Errorf("reading %v on 5 March means the March print leaked; want February's 310.0", v)
	}
	// By late March, past the lag, it does.
	if v, _ := s.AsOf("2024-03-20"); v != 312.0 {
		t.Errorf("March's figure should be available by 20 March, got %v", v)
	}
}

func TestUnknownSeriesIsAssumedLaggedAndRevised(t *testing.T) {
	// Being a month late on a daily rate costs far less than being a month
	// early on a survey, so an unknown series gets the conservative reading.
	f := testFRED(t, "observation_date,MADEUP\n2024-01-01,1.0\n")
	s, err := f.Series(context.Background(), "MADEUP")
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if !s.Revised || s.LagDays < 14 {
		t.Errorf("an unknown series should be treated conservatively: %+v", *s)
	}
}

func TestAsOfReturnsNothingBeforeTheSeriesStarts(t *testing.T) {
	f := testFRED(t, "observation_date,DGS10\n2024-01-02,3.95\n")
	s, _ := f.Series(context.Background(), "DGS10")
	if _, ok := s.AsOf("2020-01-01"); ok {
		t.Error("a date before the first observation should have no value")
	}
	if _, ok := s.AsOf(""); ok {
		t.Error("an empty date should have no value")
	}
	var nilSeries *EconSeries
	if _, ok := nilSeries.AsOf("2024-01-02"); ok {
		t.Error("a nil series should be safe to query")
	}
}

func TestFREDCachesPerProcess(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte("observation_date,DGS10\n2024-01-02,3.95\n"))
	}))
	defer srv.Close()

	f := NewFRED()
	f.BaseURL = srv.URL
	for i := 0; i < 3; i++ {
		if _, err := f.Series(context.Background(), "DGS10"); err != nil {
			t.Fatalf("series: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("a series should be fetched once per process, got %d calls", calls)
	}
}

func TestFREDReportsAnEmptySeries(t *testing.T) {
	f := testFRED(t, "observation_date,NOPE\n")
	if _, err := f.Series(context.Background(), "NOPE"); err == nil {
		t.Error("a series with no observations should be an error, not an empty success")
	}
}

func TestReleaseLagOverride(t *testing.T) {
	f := testFRED(t, "observation_date,DGS10\n2024-01-02,3.95\n")
	f.ReleaseLag["DGS10"] = 45
	s, _ := f.Series(context.Background(), "DGS10")
	if s.LagDays != 45 {
		t.Errorf("an explicit override should win, got %d", s.LagDays)
	}
}
