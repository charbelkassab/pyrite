package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
)

// GDELT is a point-in-time news backend.
//
// This is the one thing that turns ctx.news() from a demonstration into
// evidence. Every other keyless source returns the internet as it is now, so a
// backtest of 2019 reading headlines is handed 2026 information — including,
// unavoidably, articles written *because* of what happened next. GDELT's
// article index can be queried with an explicit publication-date window, so a
// strategy standing on 4 March 2019 can be shown what was published up to
// 4 March 2019 and nothing after it.
//
// It is not perfect and the limits are stated in docs/limitations.md: the
// index reaches back a few years rather than decades, relevance ranking is
// GDELT's rather than a reader's, and coverage of any given company varies.
// But "what was actually published by then" is a different kind of answer from
// "what the web says today", and only one of them belongs in a backtest.
type GDELT struct {
	HTTP    *http.Client
	BaseURL string
	// LookbackDays is how far before the simulated day to search.
	LookbackDays int
	// Language restricts results, because an English-language strategy
	// reading Arabic headlines learns nothing. Empty means no restriction.
	Language string
}

// NewGDELT builds a client.
func NewGDELT() *GDELT {
	return &GDELT{
		HTTP:         &http.Client{Timeout: 30 * time.Second},
		BaseURL:      "https://api.gdeltproject.org/api/v2/doc/doc",
		LookbackDays: 7,
		Language:     "english",
	}
}

// gdeltResponse is the artlist payload.
type gdeltResponse struct {
	Articles []struct {
		URL           string `json:"url"`
		Title         string `json:"title"`
		SeenDate      string `json:"seendate"`
		Domain        string `json:"domain"`
		Language      string `json:"language"`
		SourceCountry string `json:"sourcecountry"`
	} `json:"articles"`
}

// News returns articles published in the window ending at the simulated day.
//
// The end of the window is the whole point. It is set to the end of `day` and
// never later, so nothing a strategy reads was published after the moment it
// is standing on.
func (g *GDELT) News(ctx context.Context, day market.Day, query string, limit int) ([]engine.SearchResult, error) {
	if day == "" {
		return nil, fmt.Errorf("gdelt needs a simulated date; without one it cannot be point-in-time")
	}
	if limit <= 0 {
		limit = 5
	}
	lookback := g.LookbackDays
	if lookback <= 0 {
		lookback = 7
	}

	end := day.Date().Time().Add(24*time.Hour - time.Minute)
	start := end.Add(-time.Duration(lookback) * 24 * time.Hour)

	q := strings.TrimSpace(query)
	if g.Language != "" {
		q += " sourcelang:" + g.Language
	}

	params := url.Values{}
	params.Set("query", q)
	params.Set("mode", "artlist")
	params.Set("format", "json")
	params.Set("maxrecords", fmt.Sprint(min(limit*3, 75)))
	params.Set("sort", "hybridrel")
	params.Set("startdatetime", start.UTC().Format("20060102150405"))
	params.Set("enddatetime", end.UTC().Format("20060102150405"))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.BaseURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gdelt returned %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	// An empty body is how the endpoint reports both throttling and a window
	// it has no index for. Neither is an error worth failing a backtest over,
	// but the caller must be able to tell it apart from "no news that week".
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, errThrottledOrUncovered
	}

	var doc gdeltResponse
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("gdelt: %w", err)
	}

	out := make([]engine.SearchResult, 0, limit)
	seen := map[string]bool{}
	for _, a := range doc.Articles {
		if a.Title == "" || a.URL == "" {
			continue
		}
		// One story syndicated across a wire service's clients is one story.
		key := strings.ToLower(strings.TrimSpace(a.Title))
		if seen[key] {
			continue
		}
		seen[key] = true

		published := a.SeenDate
		if t, err := time.Parse("20060102T150405Z", a.SeenDate); err == nil {
			// Belt and braces: the API filters by date, and this checks it.
			// A single article from after the simulated day would silently
			// reintroduce exactly the bias this backend exists to remove.
			if t.After(end.Add(time.Minute)) {
				continue
			}
			published = t.UTC().Format("2006-01-02 15:04")
		}

		out = append(out, engine.SearchResult{
			Title:     cleanHTML(a.Title),
			URL:       a.URL,
			Snippet:   "",
			Published: published,
			Source:    a.Domain,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// errThrottledOrUncovered marks an empty response, which GDELT returns both
// when it is rate-limiting and when it has no index for the window.
var errThrottledOrUncovered = fmt.Errorf("gdelt returned nothing: either the window is outside its index or the request was throttled")
