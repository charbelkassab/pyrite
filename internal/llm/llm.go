// Package llm provides a single OpenAI-compatible client that talks to
// OpenAI, Cerebras and Moonshot (Kimi), plus tier-based routing and an
// on-disk response cache.
//
// All three vendors expose the same /chat/completions contract, so one client
// with a configurable base URL covers them. The differences that do exist —
// notably OpenAI's newer models rejecting max_tokens and temperature — are
// handled by adaptive retry rather than by per-vendor code paths.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/charbelkassab/natural-quant/internal/config"
)

// Role constants for chat messages.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Message is one chat turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is a provider-agnostic completion request.
type Request struct {
	// Tier selects which provider serves this request. Ignored when Model
	// and Provider are set explicitly.
	Tier config.Tier `json:"tier,omitempty"`
	// Provider optionally pins a specific provider by name.
	Provider string `json:"provider,omitempty"`
	// Model optionally overrides the provider's configured model.
	Model string `json:"model,omitempty"`

	Messages    []Message `json:"messages"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	// JSONMode requests a strict JSON object response where supported.
	JSONMode bool `json:"json_mode,omitempty"`
	// CacheKey, when non-empty, replaces the content hash used for caching.
	// Strategies use this to make an ai() call cache per simulated day.
	CacheKey string `json:"cache_key,omitempty"`
	// NoCache bypasses the response cache entirely.
	NoCache bool `json:"no_cache,omitempty"`
}

// Response is a completion result plus accounting metadata.
type Response struct {
	Text     string `json:"text"`
	Provider string `json:"provider"`
	Model    string `json:"model"`

	PromptTokens     int           `json:"prompt_tokens"`
	CompletionTokens int           `json:"completion_tokens"`
	Latency          time.Duration `json:"latency_ns"`
	Cached           bool          `json:"cached"`
}

// Client routes requests to configured providers and caches responses.
type Client struct {
	cfg   *config.Config
	http  *http.Client
	cache *Cache
}

// New builds a client. cache may be nil to disable caching.
func New(cfg *config.Config, cache *Cache) *Client {
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout: 180 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		cache: cache,
	}
}

// maxTokenCeiling bounds the automatic budget growth applied when a reasoning
// model truncates before emitting any visible content.
const maxTokenCeiling = 4096

// ErrNoProvider is returned when no API key is configured.
var ErrNoProvider = errors.New("no LLM provider configured: set OPENAI_API_KEY, CEREBRAS_API_KEY or KIMI_API_KEY")

// chatRequest mirrors the OpenAI chat completions payload. Token-limit fields
// are pointers so exactly one can be emitted per request.
type chatRequest struct {
	Model               string      `json:"model"`
	Messages            []Message   `json:"messages"`
	Temperature         *float64    `json:"temperature,omitempty"`
	MaxTokens           *int        `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int        `json:"max_completion_tokens,omitempty"`
	ResponseFormat      interface{} `json:"response_format,omitempty"`
	Stream              bool        `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// Complete runs a chat completion, consulting the cache first.
func (c *Client) Complete(ctx context.Context, req Request) (*Response, error) {
	p, err := c.resolve(req)
	if err != nil {
		return nil, err
	}
	model := req.Model
	if model == "" {
		model = p.Model
	}

	var key string
	if c.cache != nil && !req.NoCache {
		key = cacheKey(p.Name, model, req)
		if hit, ok := c.cache.Get(key); ok {
			return &Response{
				Text: hit, Provider: p.Name, Model: model, Cached: true,
			}, nil
		}
	}

	start := time.Now()
	text, usage, err := c.call(ctx, p, model, req)
	if err != nil {
		return nil, err
	}

	if key != "" {
		_ = c.cache.Put(key, text)
	}
	return &Response{
		Text:             text,
		Provider:         p.Name,
		Model:            model,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		Latency:          time.Since(start),
	}, nil
}

type usageInfo struct {
	PromptTokens     int
	CompletionTokens int
}

// call performs the HTTP request with retries and parameter adaptation.
func (c *Client) call(ctx context.Context, p *config.Provider, model string, req Request) (string, usageInfo, error) {
	// OpenAI's GPT-5 and o-series reject max_tokens and any temperature other
	// than the default. Start with the dialect the model most likely wants
	// and let the error path correct a wrong guess.
	useCompletionTokens := isReasoningModel(model)
	allowTemperature := !useCompletionTokens

	maxTokens := req.MaxTokens

	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * 600 * time.Millisecond
			select {
			case <-ctx.Done():
				return "", usageInfo{}, ctx.Err()
			case <-time.After(delay):
			}
		}

		body := chatRequest{Model: model, Messages: req.Messages}
		if maxTokens > 0 {
			n := maxTokens
			if useCompletionTokens {
				body.MaxCompletionTokens = &n
			} else {
				body.MaxTokens = &n
			}
		}
		if req.Temperature != nil && allowTemperature {
			body.Temperature = req.Temperature
		}
		if req.JSONMode {
			body.ResponseFormat = map[string]string{"type": "json_object"}
		}

		raw, status, err := c.post(ctx, p, "/chat/completions", body)
		if err != nil {
			lastErr = err
			continue
		}

		var resp chatResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			lastErr = fmt.Errorf("%s: decode response: %w", p.Name, err)
			continue
		}

		if status != http.StatusOK || resp.Error != nil {
			msg := fmt.Sprintf("http %d", status)
			if resp.Error != nil {
				msg = resp.Error.Message
			}
			low := strings.ToLower(msg)

			// Adapt to the parameter dialect the endpoint actually wants.
			switch {
			case strings.Contains(low, "max_completion_tokens"):
				useCompletionTokens = true
				continue
			case strings.Contains(low, "max_tokens") && strings.Contains(low, "not support"):
				useCompletionTokens = true
				continue
			case strings.Contains(low, "temperature"):
				allowTemperature = false
				continue
			case strings.Contains(low, "response_format"), strings.Contains(low, "json_object"):
				req.JSONMode = false
				continue
			}

			if status == http.StatusTooManyRequests || status >= 500 {
				lastErr = fmt.Errorf("%s: %s", p.Name, msg)
				continue
			}
			// 4xx that we cannot adapt to is fatal: retrying wastes time.
			return "", usageInfo{}, fmt.Errorf("%s (%s): %s", p.Name, model, msg)
		}

		if len(resp.Choices) == 0 {
			lastErr = fmt.Errorf("%s: empty response", p.Name)
			continue
		}
		text := resp.Choices[0].Message.Content
		if strings.TrimSpace(text) == "" {
			// A reasoning model spends tokens thinking before it emits any
			// visible content. Asked for one word with a five-token budget,
			// it burns the whole allowance reasoning and returns nothing at
			// all. Retrying identically would fail identically, so grow the
			// budget instead.
			if resp.Choices[0].FinishReason == "length" && maxTokens > 0 && maxTokens < maxTokenCeiling {
				maxTokens *= 8
				if maxTokens > maxTokenCeiling {
					maxTokens = maxTokenCeiling
				}
				lastErr = fmt.Errorf("%s: reply truncated before any content; retrying with a %d token budget", p.Name, maxTokens)
				continue
			}
			lastErr = fmt.Errorf("%s: blank completion (finish_reason=%s)", p.Name, resp.Choices[0].FinishReason)
			continue
		}
		return text, usageInfo{resp.Usage.PromptTokens, resp.Usage.CompletionTokens}, nil
	}
	return "", usageInfo{}, fmt.Errorf("%s: request failed after retries: %w", p.Name, lastErr)
}

func (c *Client) post(ctx context.Context, p *config.Provider, path string, payload any) ([]byte, int, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	url := strings.TrimSuffix(p.BaseURL, "/") + path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Only send an Authorization header when there is something to send. A
	// local runtime has no key, and "Bearer " with nothing after it is a
	// malformed credential that some servers reject outright.
	if p.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return raw, resp.StatusCode, nil
}

// resolve picks the provider for a request.
func (c *Client) resolve(req Request) (*config.Provider, error) {
	if req.Provider != "" {
		p, ok := c.cfg.Providers[req.Provider]
		if !ok {
			return nil, fmt.Errorf("unknown provider %q", req.Provider)
		}
		if !p.Enabled {
			if p.Local {
				return nil, fmt.Errorf("nothing is answering at %s. Start %s, or set "+
					"NQ_%s_BASE_URL if it listens elsewhere", p.BaseURL, p.Name,
					strings.ToUpper(p.Name))
			}
			return nil, fmt.Errorf("provider %q has no API key configured", req.Provider)
		}
		return p, nil
	}
	tier := req.Tier
	if tier == "" {
		tier = config.TierBalanced
	}
	p := c.cfg.ResolveTier(tier)
	if p == nil {
		return nil, ErrNoProvider
	}
	return p, nil
}

// isReasoningModel reports whether a model id belongs to a family that uses
// max_completion_tokens and rejects a custom temperature.
func isReasoningModel(model string) bool {
	m := strings.ToLower(model)
	for _, prefix := range []string{"gpt-5", "o1", "o3", "o4"} {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}

// ModelInfo is one entry from a provider's model listing.
type ModelInfo struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// ListModels queries a provider's /models endpoint. Used by the health check
// so users can see what their key actually grants without guessing.
func (c *Client) ListModels(ctx context.Context, providerName string) ([]ModelInfo, error) {
	p, ok := c.cfg.Providers[providerName]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", providerName)
	}
	if !p.Enabled {
		return nil, fmt.Errorf("provider %q has no API key configured", providerName)
	}
	url := strings.TrimSuffix(p.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d", providerName, resp.StatusCode)
	}
	var out struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// Config exposes the configuration the client was built with.
func (c *Client) Config() *config.Config { return c.cfg }
