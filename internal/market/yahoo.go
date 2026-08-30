package market

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// YahooProvider reads daily bars from Yahoo Finance's public chart endpoint.
//
// This endpoint requires no API key and returns split- and dividend-adjusted
// closes alongside raw OHLCV, which is everything a daily backtest needs. It
// is an undocumented endpoint, so the parsing below is defensive.
type YahooProvider struct {
	HTTP    *http.Client
	BaseURL string
}

// NewYahooProvider builds a provider with sensible timeouts.
func NewYahooProvider() *YahooProvider {
	return &YahooProvider{
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     60 * time.Second,
			},
		},
		BaseURL: "https://query1.finance.yahoo.com",
	}
}

func (y *YahooProvider) Name() string { return "yahoo" }

// userAgent is required; the endpoint rejects requests without a browser-like
// user agent.
const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0 Safari/537.36"

type yahooChartResponse struct {
	Chart struct {
		Result []struct {
			Meta struct {
				Symbol             string  `json:"symbol"`
				LongName           string  `json:"longName"`
				ShortName          string  `json:"shortName"`
				GMTOffset          int     `json:"gmtoffset"`
				Currency           string  `json:"currency"`
				RegularMarketPrice float64 `json:"regularMarketPrice"`
			} `json:"meta"`
			Timestamp  []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Open   []*float64 `json:"open"`
					High   []*float64 `json:"high"`
					Low    []*float64 `json:"low"`
					Close  []*float64 `json:"close"`
					Volume []*float64 `json:"volume"`
				} `json:"quote"`
				AdjClose []struct {
					AdjClose []*float64 `json:"adjclose"`
				} `json:"adjclose"`
			} `json:"indicators"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

// Fetch downloads daily bars for [from, to].
func (y *YahooProvider) Fetch(ctx context.Context, symbol string, from, to Day) (*Series, error) {
	// Pad the window generously. Fetching a wider range costs nothing extra
	// on this endpoint and means indicator warm-up periods and later,
	// longer backtests are served from cache.
	p1 := from.Add(-800).Time().Unix()
	p2 := to.Add(5).Time().Unix()
	if p1 < 0 {
		p1 = 0
	}

	endpoint := fmt.Sprintf("%s/v8/finance/chart/%s?period1=%d&period2=%d&interval=1d&events=div%%2Csplit&includeAdjustedClose=true",
		y.BaseURL, url.PathEscape(symbol), p1, p2)

	body, err := y.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	var resp yahooChartResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("yahoo: decode %s: %w", symbol, err)
	}
	if resp.Chart.Error != nil {
		if strings.Contains(strings.ToLower(resp.Chart.Error.Code), "notfound") {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, symbol)
		}
		return nil, fmt.Errorf("yahoo: %s: %s", symbol, resp.Chart.Error.Description)
	}
	if len(resp.Chart.Result) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, symbol)
	}

	r := resp.Chart.Result[0]
	if len(r.Indicators.Quote) == 0 {
		return nil, fmt.Errorf("%w: %s (no quote data)", ErrNotFound, symbol)
	}
	q := r.Indicators.Quote[0]

	var adj []*float64
	if len(r.Indicators.AdjClose) > 0 {
		adj = r.Indicators.AdjClose[0].AdjClose
	}

	// Yahoo stamps each daily bar at the exchange's opening instant. Shifting
	// by the exchange's UTC offset before taking the date yields the correct
	// local trading day regardless of the server's timezone.
	loc := time.FixedZone("exch", r.Meta.GMTOffset)

	bars := make([]Bar, 0, len(r.Timestamp))
	for i, ts := range r.Timestamp {
		c := at(q.Close, i)
		if c == nil || math.IsNaN(*c) {
			continue // holiday padding or a halted session
		}
		bar := Bar{
			Date:   NewDay(time.Unix(ts, 0).In(loc)),
			Open:   deref(at(q.Open, i), *c),
			High:   deref(at(q.High, i), *c),
			Low:    deref(at(q.Low, i), *c),
			Close:  *c,
			Volume: deref(at(q.Volume, i), 0),
		}
		bar.AdjClose = deref(at(adj, i), bar.Close)
		if bar.AdjClose <= 0 {
			bar.AdjClose = bar.Close
		}
		bars = append(bars, bar)
	}
	if len(bars) == 0 {
		return nil, fmt.Errorf("%w: %s (no usable bars)", ErrNotFound, symbol)
	}

	s := NewSeries(symbol, bars)
	s.Name = firstNonEmpty(r.Meta.LongName, r.Meta.ShortName, symbol)
	return s, nil
}

type yahooSearchResponse struct {
	Quotes []struct {
		Symbol    string `json:"symbol"`
		ShortName string `json:"shortname"`
		LongName  string `json:"longname"`
		Exchange  string `json:"exchDisp"`
		Type      string `json:"quoteType"`
	} `json:"quotes"`
}

// Search resolves free text to tickers using Yahoo's symbol lookup.
func (y *YahooProvider) Search(ctx context.Context, query string) ([]Quote, error) {
	endpoint := fmt.Sprintf("%s/v1/finance/search?q=%s&quotesCount=12&newsCount=0",
		y.BaseURL, url.QueryEscape(query))
	body, err := y.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var resp yahooSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("yahoo: decode search: %w", err)
	}
	out := make([]Quote, 0, len(resp.Quotes))
	for _, q := range resp.Quotes {
		if q.Symbol == "" {
			continue
		}
		out = append(out, Quote{
			Symbol:   q.Symbol,
			Name:     firstNonEmpty(q.LongName, q.ShortName, q.Symbol),
			Exchange: q.Exchange,
			Type:     q.Type,
		})
	}
	return out, nil
}

// get performs an HTTP GET with retries on transient failures.
func (y *YahooProvider) get(ctx context.Context, endpoint string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 400 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json")

		resp, err := y.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return nil, ErrNotFound
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("yahoo: http %d", resp.StatusCode)
			continue
		case resp.StatusCode != http.StatusOK:
			return nil, fmt.Errorf("yahoo: http %d: %s", resp.StatusCode, truncate(string(body), 200))
		}
		return body, nil
	}
	return nil, fmt.Errorf("yahoo: request failed after retries: %w", lastErr)
}

func at(s []*float64, i int) *float64 {
	if i < 0 || i >= len(s) {
		return nil
	}
	return s[i]
}

func deref(p *float64, fallback float64) float64 {
	if p == nil || math.IsNaN(*p) {
		return fallback
	}
	return *p
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
