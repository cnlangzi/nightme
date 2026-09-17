package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// restClient is the adapter-side abstraction over the Discord HTTP
// API. Methods are intentionally narrow (one per REST verb we
// use in Phase 1 + Phase 2) so tests can implement just the
// surface they exercise.
type restClient interface {
	GetGatewayBot(ctx context.Context) (GatewayBotResponse, error)
	GetMe(ctx context.Context) (User, error)
	CreateMessage(ctx context.Context, channelID string, payload CreateMessagePayload) (Message, error)
	EditMessage(ctx context.Context, channelID, messageID string, payload EditMessagePayload) (Message, error)
	DeleteMessage(ctx context.Context, channelID, messageID string) error
	AddReaction(ctx context.Context, channelID, messageID, emoji string) error
	RemoveOwnReaction(ctx context.Context, channelID, messageID, emoji string) error
	ClearReactions(ctx context.Context, channelID, messageID string) error
	// Download fetches the body of a Discord CDN URL. Auth is not
	// required (CDN URLs are signed per-URL) but the bot header is
	// harmless; tests inject a fake that ignores the URL.
	Download(ctx context.Context, url string) ([]byte, error)
	// AcknowledgeInteraction POSTs the interaction callback within
	// Discord's 3-second window. Callers must use a fresh
	// context.WithTimeout(context.Background(), 2500*time.Millisecond)
	// so a cancelled gateway ctx can't strand the ACK.
	AcknowledgeInteraction(ctx context.Context, id, token string, body InteractionResponse) error
}

// apiError is the structured error returned by httpREST. StatusCode
// is the HTTP status; RetryAfter is parsed from Retry-After /
// X-RateLimit-Reset-After when set.
type apiError struct {
	StatusCode int
	Method     string
	Path       string
	RetryAfter time.Duration
	Message    string
}

func (e *apiError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return fmt.Sprintf("discord api %s %s: status %d: %s",
			e.Method, e.Path, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("discord api %s %s: status %d",
		e.Method, e.Path, e.StatusCode)
}

// httpREST is the production restClient. It holds the bot token,
// a base URL (default https://discord.com/api/v10), a per-bucket
// rate limiter, and a retry config.
type httpREST struct {
	token     string
	baseURL   string
	limiter   *Limiter
	retry     RetryConfig
	client    *http.Client
	userAgent string
}

func newRESTClient(token string, limiter *Limiter, retry RetryConfig, userAgent string) *httpREST {
	if userAgent == "" {
		userAgent = "DiscordBot (https://github.com/cnlangzi/nightme)"
	}
	return &httpREST{
		token:     token,
		baseURL:   "https://discord.com/api/v10",
		limiter:   limiter,
		retry:     retry,
		client:    &http.Client{Timeout: 30 * time.Second},
		userAgent: userAgent,
	}
}

// Call is the lowest-level request primitive. Tests use this
// directly; the per-endpoint methods below wrap it.
func (c *httpREST) Call(ctx context.Context, method, path string, body any, result any) error {
	if c == nil || c.client == nil {
		return errors.New("discord: REST client is nil")
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("discord: marshal %s %s: %w", method, path, err)
		}
		bodyReader = bytes.NewReader(data)
	}

	endpoint := c.baseURL + path
	if !strings.HasPrefix(path, "/") {
		endpoint = c.baseURL + "/" + path
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return fmt.Errorf("discord: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("discord: request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	bucket, remaining, limit, reset, global, retryAfter := parseRateLimitHeaders(resp.Header)
	c.limiter.UpdateFromHeaders(bucket, remaining, limit, reset, global, retryAfter)

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("discord: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiMsg struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		}
		_ = json.Unmarshal(respBody, &apiMsg)
		msg := apiMsg.Message
		if msg == "" {
			msg = strings.TrimSpace(string(respBody))
		}
		return &apiError{
			StatusCode: resp.StatusCode,
			Method:     method,
			Path:       path,
			RetryAfter: retryAfter,
			Message:    msg,
		}
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("discord: decode response: %w", err)
		}
	}
	return nil
}

// GetGatewayBot calls GET /gateway/bot. The returned URL does not
// include the query string — gateway.go appends it.
func (c *httpREST) GetGatewayBot(ctx context.Context) (GatewayBotResponse, error) {
	var out GatewayBotResponse
	err := WithTransientRetry(ctx, RetryOpts{Op: "gateway_bot", Cfg: c.retry}, func() error {
		return c.Call(ctx, http.MethodGet, "/gateway/bot", nil, &out)
	})
	return out, err
}

// GetMe calls GET /users/@me. Used by both NewAdapter (to cache
// the bot user id) and the login provider (to validate the token).
func (c *httpREST) GetMe(ctx context.Context) (User, error) {
	var out User
	err := WithTransientRetry(ctx, RetryOpts{Op: "users_me", Cfg: c.retry}, func() error {
		return c.Call(ctx, http.MethodGet, "/users/@me", nil, &out)
	})
	return out, err
}

func (c *httpREST) CreateMessage(ctx context.Context, channelID string, payload CreateMessagePayload) (Message, error) {
	path := "/channels/" + url.PathEscape(channelID) + "/messages"
	var out Message
	err := WithTransientRetry(ctx, RetryOpts{Op: "create_message", Cfg: c.retry}, func() error {
		return c.Call(ctx, http.MethodPost, path, payload, &out)
	})
	return out, err
}

func (c *httpREST) EditMessage(ctx context.Context, channelID, messageID string, payload EditMessagePayload) (Message, error) {
	path := "/channels/" + url.PathEscape(channelID) + "/messages/" + url.PathEscape(messageID)
	var out Message
	err := WithTransientRetry(ctx, RetryOpts{Op: "edit_message", Cfg: c.retry}, func() error {
		return c.Call(ctx, http.MethodPatch, path, payload, &out)
	})
	return out, err
}

func (c *httpREST) DeleteMessage(ctx context.Context, channelID, messageID string) error {
	path := "/channels/" + url.PathEscape(channelID) + "/messages/" + url.PathEscape(messageID)
	return WithTransientRetry(ctx, RetryOpts{Op: "delete_message", Cfg: c.retry}, func() error {
		return c.Call(ctx, http.MethodDelete, path, nil, nil)
	})
}

// AddReaction uses PUT /channels/{id}/messages/{id}/reactions/{emoji}/@me.
// emoji is URL-encoded by net/url because it can contain arbitrary
// unicode characters (👌, 🎉, …).
func (c *httpREST) AddReaction(ctx context.Context, channelID, messageID, emoji string) error {
	path := "/channels/" + url.PathEscape(channelID) +
		"/messages/" + url.PathEscape(messageID) +
		"/reactions/" + url.PathEscape(emoji) + "/@me"
	return WithTransientRetry(ctx, RetryOpts{Op: "add_reaction", Cfg: c.retry}, func() error {
		return c.Call(ctx, http.MethodPut, path, nil, nil)
	})
}

func (c *httpREST) RemoveOwnReaction(ctx context.Context, channelID, messageID, emoji string) error {
	path := "/channels/" + url.PathEscape(channelID) +
		"/messages/" + url.PathEscape(messageID) +
		"/reactions/" + url.PathEscape(emoji) + "/@me"
	return WithTransientRetry(ctx, RetryOpts{Op: "remove_reaction", Cfg: c.retry}, func() error {
		return c.Call(ctx, http.MethodDelete, path, nil, nil)
	})
}

func (c *httpREST) ClearReactions(ctx context.Context, channelID, messageID string) error {
	path := "/channels/" + url.PathEscape(channelID) +
		"/messages/" + url.PathEscape(messageID) + "/reactions"
	return WithTransientRetry(ctx, RetryOpts{Op: "clear_reactions", Cfg: c.retry}, func() error {
		return c.Call(ctx, http.MethodDelete, path, nil, nil)
	})
}

// Download fetches a Discord CDN attachment body. CDN URLs are
// signed per-URL, so the bot Authorization header is technically
// unnecessary; we still attach it for consistency with the rest of
// the REST surface and to make log lines recognisable. Retries are
// skipped (attachments are a one-shot fetch — retrying a CDN 5xx
// from a deleted attachment only burns the caller's ctx budget).
func (c *httpREST) Download(ctx context.Context, rawURL string) ([]byte, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("discord: REST client is nil")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("discord: build download request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+c.token)
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discord: download: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("discord: read download: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &apiError{
			StatusCode: resp.StatusCode,
			Method:     http.MethodGet,
			Path:       rawURL,
			Message:    strings.TrimSpace(string(body)),
		}
	}
	return body, nil
}

// AcknowledgeInteraction posts the interaction callback response
// within Discord's strict 3-second window. The path is intentionally
// NOT routed through c.Call — the rate limiter and retry are both
// wrong for this surface (no rate limit applies to /interactions
// callbacks, and a retry past 3 s is useless). The body is a plain
// JSON marshal of the supplied InteractionResponse.
func (c *httpREST) AcknowledgeInteraction(ctx context.Context, id, token string, body InteractionResponse) error {
	if c == nil || c.client == nil {
		return errors.New("discord: REST client is nil")
	}
	path := "/interactions/" + url.PathEscape(id) + "/" + url.PathEscape(token) + "/callback"
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("discord: marshal interaction callback: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("discord: build interaction request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("discord: interaction callback: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		return &apiError{
			StatusCode: resp.StatusCode,
			Method:     http.MethodPost,
			Path:       path,
			Message:    strings.TrimSpace(string(msg)),
		}
	}
	return nil
}
