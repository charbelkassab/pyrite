package websearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
)

// gdeltServer records what was asked and answers with what it is given.
func gdeltServer(t *testing.T, body string, seen *url.Values) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = r.URL.Query()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func articles(items ...[2]string) string {
	type a struct {
		URL      string `json:"url"`
		Title    string `json:"title"`
		SeenDate string `json:"seendate"`
		Domain   string `json:"domain"`
	}
	var out []a
	for i, it := range items {
		out = append(out, a{
			URL:   "https://example.com/" + string(rune('a'+i)),
			Title: it[0], SeenDate: it[1], Domain: "example.com",
		})
	}
	b, _ := json.Marshal(map[string]any{"articles": out})
	return string(b)
}

// The whole point of this backend: the window it asks for must end at the
// simulated day and never later.
func TestGDELTBoundsTheWindowAtTheSimulatedDay(t *testing.T) {
	var q url.Values
	g := NewGDELT()
	g.BaseURL = gdeltServer(t, articles([2]string{"A headline", "20190304T120000Z"}), &q).URL
	g.LookbackDays = 7

	if _, err := g.News(context.Background(), "2019-03-05", "Apple", 5); err != nil {
		t.Fatalf("news: %v", err)
	}
	end := q.Get("enddatetime")
	start := q.Get("startdatetime")
	if !strings.HasPrefix(end, "20190305") {
		t.Errorf("the window must end on the simulated day, got %q", end)
	}
	if !strings.HasPrefix(start, "20190226") {
		t.Errorf("a 7-day lookback from 2019-03-05 starts 2019-02-26, got %q", start)
	}
	if start >= end {
		t.Errorf("window is inverted: %s .. %s", start, end)
	}
	// And the language filter, without which an English strategy reads
	// headlines it cannot use.
	if !strings.Contains(q.Get("query"), "sourcelang:english") {
		t.Errorf("query should restrict language: %q", q.Get("query"))
	}
}

// Belt and braces: even if the index returned something from after the
// simulated day, it must not reach the strategy. One such article would
// silently reintroduce exactly the bias this backend removes.
func TestGDELTDropsArticlesFromTheFuture(t *testing.T) {
	body := articles(
		[2]string{"Published before", "20190304T120000Z"},
		[2]string{"Published after", "20190309T120000Z"},
	)
	g := NewGDELT()
	g.BaseURL = gdeltServer(t, body, nil).URL

	res, err := g.News(context.Background(), "2019-03-05", "Apple", 5)
	if err != nil {
		t.Fatalf("news: %v", err)
	}
	for _, r := range res {
		if strings.Contains(r.Title, "after") {
			t.Fatalf("an article published after the simulated day reached the strategy: %+v", r)
		}
	}
	if len(res) != 1 {
		t.Fatalf("want the one in-window article, got %d", len(res))
	}
}

func TestGDELTDeduplicatesSyndicatedStories(t *testing.T) {
	body := articles(
		[2]string{"Apple beats expectations", "20190304T120000Z"},
		[2]string{"Apple beats expectations", "20190304T130000Z"},
		[2]string{"A different story", "20190304T140000Z"},
	)
	g := NewGDELT()
	g.BaseURL = gdeltServer(t, body, nil).URL

	res, err := g.News(context.Background(), "2019-03-05", "Apple", 5)
	if err != nil {
		t.Fatalf("news: %v", err)
	}
	if len(res) != 2 {
		t.Errorf("one story on two wires is one story, got %d: %+v", len(res), res)
	}
}

func TestGDELTRefusesWithoutADate(t *testing.T) {
	g := NewGDELT()
	g.BaseURL = gdeltServer(t, articles(), nil).URL
	if _, err := g.News(context.Background(), "", "Apple", 5); err == nil {
		t.Fatal("without a simulated date it cannot be point-in-time, so it must refuse")
	}
}

func TestGDELTReportsAnEmptyResponseDistinctly(t *testing.T) {
	// An empty body means throttled or out of index. The caller has to be
	// able to tell that apart from "no news that week".
	g := NewGDELT()
	g.BaseURL = gdeltServer(t, "", nil).URL
	_, err := g.News(context.Background(), "2019-03-05", "Apple", 5)
	if err == nil {
		t.Fatal("an empty response should be reported")
	}
	if !strings.Contains(err.Error(), "throttled") {
		t.Errorf("the error should name both causes: %v", err)
	}
}

// A dated backend must never silently fall back to a live one: the strategy
// would see plausible headlines from the future and never know.
func TestSearcherDoesNotFallBackFromDatedNews(t *testing.T) {
	s := New(true)
	s.MinInterval = 0
	g := NewGDELT()
	g.BaseURL = gdeltServer(t, "", nil).URL // always empty
	s.GDELT = g

	res, err := s.Search(context.Background(), "2019-03-05", "Apple", 5, true)
	if err != nil {
		t.Fatalf("an empty index is not a failure: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("nothing indexed means nothing returned, got %d results", len(res))
	}
}

// Without the simulated day in the cache key, the first week's headlines
// would be served for every later week — wrong, and indistinguishable from
// the lookahead the backend exists to remove.
func TestSearcherCachesDatedNewsPerDay(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("enddatetime"))
		w.Write([]byte(articles([2]string{"Headline", "20190304T120000Z"})))
	}))
	defer srv.Close()

	s := New(true)
	s.MinInterval = 0
	g := NewGDELT()
	g.BaseURL = srv.URL
	s.GDELT = g

	ctx := context.Background()
	for _, day := range []string{"2019-03-05", "2019-03-12", "2019-03-05"} {
		if _, err := s.Search(ctx, market.Day(day), "Apple", 5, true); err != nil {
			t.Fatalf("%s: %v", day, err)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("two distinct days plus a repeat should make 2 requests, got %d", len(seen))
	}
	if seen[0] == seen[1] {
		t.Errorf("the two requests used the same window: %v", seen)
	}
	if !s.NewsIsPointInTime() {
		t.Error("a searcher with a dated backend reports point-in-time news")
	}
}

func TestLiveNewsIsNotPointInTime(t *testing.T) {
	s := New(true)
	if s.NewsIsPointInTime() {
		t.Error("without a dated backend, news is today's internet")
	}
	var nilSearcher *Searcher
	if nilSearcher.NewsIsPointInTime() {
		t.Error("a nil searcher should be safe and report false")
	}
}

var _ = engine.SearchResult{}
