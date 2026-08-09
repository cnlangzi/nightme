// HTTP client for the opencode server. Only the 9 endpoints we
// actually use are wired up; the rest of the OpenAPI surface is
// ignored. Every session-scoped call includes the
// `x-opencode-directory` header so the server can route requests to
// the right project (mirrors packages/sdk/js/src/client.ts `rewrite`).
//
// Endpoints implemented:
//
//	POST /api/session                                  CreateSession
//	GET  /api/session/{id}                             GetSession
//	POST /api/session/{id}/prompt                      Prompt
//	GET  /api/session/{id}/event  (SSE)                Subscribe
//	POST /api/session/{id}/interrupt                   Interrupt
//	POST /api/session/{id}/permission/{reqID}/reply    ReplyPermission
//	POST /api/session/{id}/model                       SetModel
//	GET  /api/config                                   GetConfig
//	GET  /api/health                                   Health
//
// Auth: if password is non-empty, every request carries basic auth
// (`opencode / password`), matching the opencode server's default
// scheme.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is the HTTP client wrapper. It is stateless apart from the
// base URL + workspace + password, so it is safe to share across
// goroutines within a single session.
type Client struct {
	baseURL   string
	workspace string
	password  string
	http      *http.Client
}

// newClient builds a Client targeting the given serverProc. The HTTP
// client uses a sensible per-request timeout that the caller can
// override via ctx if needed.
func newClient(proc *serverProc, workspace string) *Client {
	return &Client{
		baseURL:   proc.baseURL,
		workspace: workspace,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// setPassword (optional) wires basic auth into subsequent requests.
// Opencode's default `OPENCODE_SERVER_PASSWORD` auth scheme is HTTP
// basic with username "opencode".
func (c *Client) setPassword(pw string) { c.password = pw }

// ─── request helpers ───

func (c *Client) newRequest(ctx context.Context, method, p string, body any) (*http.Request, error) {
	full := c.baseURL + p
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("opencode: marshal body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, rdr)
	if err != nil {
		return nil, fmt.Errorf("opencode: new request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.password != "" {
		req.SetBasicAuth("opencode", c.password)
	}
	if c.workspace != "" {
		req.Header.Set("x-opencode-directory", url.QueryEscape(c.workspace))
	}
	req.Header.Set("User-Agent", "nightme-opencode-bridge/"+version)
	return req, nil
}

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opencode: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("opencode: %s %s: %d: %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		// Drain body to allow connection reuse.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("opencode: decode: %w", err)
	}
	return nil
}

// ─── payload types (subset of OpenAPI) ───

// Session is the subset of GET /api/session/{id} we read. Anything
// else opencode wants to surface can be added here.
type Session struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	ProjectID  string `json:"projectID"`
	Directory  string `json:"directory"`
	ParentID   string `json:"parentID"`
	Title      string `json:"title"`
	Version    string `json:"version"`
	Summary    any    `json:"summary"`
	Cost       any    `json:"cost"`
	Tokens     any    `json:"tokens"`
	Share      any    `json:"share"`
	WorkspaceID string `json:"workspaceID"`
}

// ModelRef is the model field on a Session.
type ModelRef struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// PartInput is the union of prompt part types. We only emit text and
// file (Phase 1). opencode accepts `text`, `file`, and a few other
// shapes that we don't yet exercise.
type PartInput struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MIME     string `json:"mime,omitempty"`
	Filename string `json:"filename,omitempty"`
	URL      string `json:"url,omitempty"`
}

// TextPart builds a `{type:"text", text:t}` input.
func TextPart(text string) PartInput {
	return PartInput{Type: "text", Text: text}
}

// FilePart builds a `{type:"file", mime, url}` input. The URL is the
// file:// URL the runtime hands us; opencode reads the bytes itself.
func FilePart(mime, url string) PartInput {
	return PartInput{Type: "file", MIME: mime, URL: url}
}

// CreateSessionOpts is the body for POST /api/session.
type CreateSessionOpts struct {
	ParentID string `json:"parentID,omitempty"`
	Title    string `json:"title,omitempty"`
}

// PromptResult is the parsed response of POST /api/session/{id}/prompt.
// The endpoint returns a full SessionMessagesResponse; we only need
// the last assistant message's tokens / cost for the usage footer.
type PromptResult struct {
	Info any `json:"info"`
}

// CreateSession calls POST /api/session.
func (c *Client) CreateSession(ctx context.Context, opts CreateSessionOpts) (*Session, error) {
	req, err := c.newRequest(ctx, "POST", "/api/session", opts)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := c.doJSON(req, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// GetSession calls GET /api/session/{id}.
func (c *Client) GetSession(ctx context.Context, id string) (*Session, error) {
	if id == "" {
		return nil, fmt.Errorf("opencode: empty session id")
	}
	req, err := c.newRequest(ctx, "GET", "/api/session/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := c.doJSON(req, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Prompt calls POST /api/session/{id}/prompt. The server returns when
// the turn is fully complete (synchronous). We use this rather than
// prompt_async so the bridge's existing turn-completion signaling
// (session.idle SSE) lines up with the prompt response.
func (c *Client) Prompt(ctx context.Context, sessionID string, parts []PartInput) (*PromptResult, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("opencode: empty session id")
	}
	body := map[string]any{"parts": parts}
	req, err := c.newRequest(ctx, "POST", "/api/session/"+url.PathEscape(sessionID)+"/prompt", body)
	if err != nil {
		return nil, err
	}
	var r PromptResult
	if err := c.doJSON(req, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Interrupt calls POST /api/session/{id}/interrupt.
func (c *Client) Interrupt(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("opencode: empty session id")
	}
	req, err := c.newRequest(ctx, "POST", "/api/session/"+url.PathEscape(sessionID)+"/interrupt", nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}

// ReplyPermission calls POST /api/session/{id}/permission/{reqID}/reply
// with the user's decision string. The server accepts "once" /
// "always" / "reject" verbatim.
func (c *Client) ReplyPermission(ctx context.Context, sessionID, requestID, response string) error {
	if sessionID == "" || requestID == "" {
		return fmt.Errorf("opencode: empty session or request id")
	}
	body := map[string]any{"response": response}
	req, err := c.newRequest(ctx, "POST",
		"/api/session/"+url.PathEscape(sessionID)+"/permission/"+url.PathEscape(requestID)+"/reply",
		body)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}

// SetModel calls POST /api/session/{id}/model.
func (c *Client) SetModel(ctx context.Context, sessionID, providerID, modelID string) error {
	if sessionID == "" {
		return fmt.Errorf("opencode: empty session id")
	}
	body := map[string]any{
		"providerID": providerID,
		"modelID":    modelID,
	}
	req, err := c.newRequest(ctx, "POST", "/api/session/"+url.PathEscape(sessionID)+"/model", body)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}

// GetConfig calls GET /api/config. Useful for surfacing the active
// provider / model in the EventAgentReady payload.
func (c *Client) GetConfig(ctx context.Context) (map[string]any, error) {
	req, err := c.newRequest(ctx, "GET", "/api/config", nil)
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	if err := c.doJSON(req, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Health calls GET /api/health and returns nil iff the server
// answered 200.
func (c *Client) Health(ctx context.Context) error {
	req, err := c.newRequest(ctx, "GET", "/api/health", nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}
