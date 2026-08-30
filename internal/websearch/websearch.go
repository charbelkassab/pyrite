// Package websearch gives strategies a keyless way to read the internet.
//
// Two backends are provided: DuckDuckGo's lite endpoint for general queries,
// and Yahoo Finance's RSS headline feed for per-symbol news. Neither needs an
// API key, which keeps natural-quant runnable straight from a clone.
//
// A warning that belongs next to every use of this package: these searches
// return the internet as it is today, not as it was on the simulated date. A
// strategy that reads the web during a backtest of 2019 is being handed 2026
// information. That is lookahead bias of the most severe kind. The engine
// records every call so the effect is at least visible, and the UI and docs
// say so plainly, but the package itself cannot prevent it.
package websearch

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/charbelkassab/natural-quant/internal/engine"
	"github.com/charbelkassab/natural-quant/internal/llm"
	"github.com/charbelkassab/natural-quant/internal/market"
)

const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0 Safari/537.36"

// Searcher performs web and news lookups.
//
// Results are cached in memory and, when a directory is configured, on disk.
// Persistence matters as much here as it does for model replies: a strategy
// reading headlines once a week over four years makes ~200 network round
// trips, and without a durable cache every re-run pays that again in minutes.
type Searcher struct {
	HTTP    *http.Client
	Enabled bool

	mu    sync.Mutex
	cache map[string][]engine.SearchResult
	disk  *llm.Cache
	// MinInterval throttles outbound requests so a long backtest does not
	// hammer a free endpoint.
	MinInterval time.Duration
	lastCall    time.Time
}

// New builds a Searcher with an in-memory cache only.
func New(enabled bool) *Searcher {
	return &Searcher{
		HTTP:        &http.Client{Timeout: 20 * time.Second},
		Enabled:     enabled,
		cache:       map[string][]engine.SearchResult{},
		MinInterval: 250 * time.Millisecond,
	}
}

// NewWithCache builds a Searcher that also persists results under dir.
func NewWithCache(enabled bool, dir string) (*Searcher, error) {
	s := New(enabled)
	if dir == "" {
		return s, nil
	}
	c, err := llm.NewCache(dir)
	if err != nil {
		return nil, err
	}
	s.disk = c
	return s, nil
}

// Search implements engine.SearchFunc.
func (s *Searcher) Search(ctx context.Context, day market.Day, query string, limit int, news bool) ([]engine.SearchResult, error) {
	if !s.Enabled {
		return nil, fmt.Errorf("web search is disabled")
	}
	if limit <= 0 {
		limit = 5
	}

	key := fmt.Sprintf("%v|%d|%s", news, limit, strings.ToLower(strings.TrimSpace(query)))
	s.mu.Lock()
	if hit, ok := s.cache[key]; ok {
		s.mu.Unlock()
		return hit, nil
	}
	s.mu.Unlock()

	if s.disk != nil {
		if raw, ok := s.disk.Get(key); ok {
			var hit []engine.SearchResult
			if err := json.Unmarshal([]byte(raw), &hit); err == nil {
				s.mu.Lock()
				s.cache[key] = hit
				s.mu.Unlock()
				return hit, nil
			}
		}
	}

	s.throttle()

	var (
		res []engine.SearchResult
		err error
	)
	if news {
		res, err = s.yahooNews(ctx, query, limit)
		if err != nil || len(res) == 0 {
			// Fall back to a general search so a news query still returns
			// something useful for non-ticker topics.
			res, err = s.duckduckgo(ctx, query+" news", limit)
		}
	} else {
		res, err = s.duckduckgo(ctx, query, limit)
	}
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.cache[key] = res
	s.mu.Unlock()
	if s.disk != nil {
		if b, err := json.Marshal(res); err == nil {
			_ = s.disk.Put(key, string(b))
		}
	}
	return res, nil
}

func (s *Searcher) throttle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if wait := s.MinInterval - time.Since(s.lastCall); wait > 0 {
		time.Sleep(wait)
	}
	s.lastCall = time.Now()
}

// DuckDuckGo lite returns compact HTML with single-quoted class attributes.
var (
	ddgLinkRe    = regexp.MustCompile(`(?is)<a[^>]+href=["']([^"']+)["'][^>]*class=['"]result-link['"][^>]*>(.*?)</a>`)
	ddgSnippetRe = regexp.MustCompile(`(?is)<td[^>]*class=['"]result-snippet['"][^>]*>(.*?)</td>`)
	tagRe        = regexp.MustCompile(`(?s)<[^>]*>`)
	wsRe         = regexp.MustCompile(`\s+`)
)

func (s *Searcher) duckduckgo(ctx context.Context, query string, limit int) ([]engine.SearchResult, error) {
	form := url.Values{"q": {query}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://lite.duckduckgo.com/lite/", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web search request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("web search returned http %d", resp.StatusCode)
	}

	text := string(body)
	links := ddgLinkRe.FindAllStringSubmatch(text, limit*2)
	snips := ddgSnippetRe.FindAllStringSubmatch(text, limit*2)

	out := make([]engine.SearchResult, 0, limit)
	for i, m := range links {
		if len(out) >= limit {
			break
		}
		r := engine.SearchResult{
			URL:   cleanHTML(m[1]),
			Title: cleanHTML(m[2]),
		}
		if i < len(snips) {
			r.Snippet = cleanHTML(snips[i][1])
		}
		if r.Title == "" {
			continue
		}
		r.Source = hostOf(r.URL)
		out = append(out, r)
	}
	return out, nil
}

// rssFeed is the subset of RSS 2.0 we need.
type rssFeed struct {
	Channel struct {
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			Description string `xml:"description"`
			PubDate     string `xml:"pubDate"`
		} `xml:"item"`
	} `xml:"channel"`
}

// yahooNews reads the per-symbol headline feed. The query is treated as a
// ticker; non-ticker queries simply return nothing and the caller falls back.
func (s *Searcher) yahooNews(ctx context.Context, query string, limit int) ([]engine.SearchResult, error) {
	sym := extractTicker(query)
	if sym == "" {
		return nil, fmt.Errorf("no ticker in %q", query)
	}
	endpoint := fmt.Sprintf(
		"https://feeds.finance.yahoo.com/rss/2.0/headline?s=%s&region=US&lang=en-US",
		url.QueryEscape(sym))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("news feed returned http %d", resp.StatusCode)
	}

	var feed rssFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, err
	}
	out := make([]engine.SearchResult, 0, limit)
	for _, item := range feed.Channel.Items {
		if len(out) >= limit {
			break
		}
		out = append(out, engine.SearchResult{
			Title:     cleanHTML(item.Title),
			URL:       item.Link,
			Snippet:   cleanHTML(item.Description),
			Published: item.PubDate,
			Source:    hostOf(item.Link),
		})
	}
	return out, nil
}

// tickerRe matches a plausible exchange ticker: 1-5 capitals, optionally with
// a class or pair suffix such as BRK-B or BTC-USD, or a leading ^ for indices.
var tickerRe = regexp.MustCompile(`^\^?[A-Z]{1,5}(-[A-Z]{1,4})?$`)

// extractTicker finds the symbol inside a free-text news query.
//
// Strategies rarely pass a bare ticker — "Apple AAPL stock latest headlines"
// is far more typical. Taking the first word would look up "APPLE", which is
// not a symbol, so the headline feed returns nothing and the caller silently
// degrades to a generic web search. Scanning for a token that actually looks
// like a ticker recovers the real feed.
func extractTicker(query string) string {
	fields := strings.Fields(query)
	for _, f := range fields {
		f = strings.Trim(f, ".,:;!?()[]\"'")
		// Only an already-capitalised token is considered, so "Apple" does
		// not masquerade as a ticker while "AAPL" does.
		if tickerRe.MatchString(f) {
			return market.NormalizeSymbol(f)
		}
	}
	// Fall back to a known alias, which catches "bitcoin", "gold" and similar.
	for _, f := range fields {
		if s := market.NormalizeSymbol(f); strings.HasPrefix(s, "^") || strings.Contains(s, "-USD") || strings.Contains(s, "=F") {
			return s
		}
	}
	if len(fields) == 1 {
		return market.NormalizeSymbol(fields[0])
	}
	return ""
}

func cleanHTML(s string) string {
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = wsRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Host, "www.")
}
