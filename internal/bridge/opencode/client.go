
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

	// http is the short-lived request client used by Prompt,
	// Interrupt, Health, GetSession, etc. Per-request timeouts are
	// applied at the call sites via context.WithTimeout — the
	// client itself has no overall Timeout because a global Timeout
	// would also kill the long-lived SSE connection (the historical
	// bug we fixed: a 30 s idle cut-off swallowed every event the
	// opencode CLI was about to push).
	http *http.Client

	// httpSSE is the dedicated client for the /api/event SSE
	// stream. It has NO Timeout: the connection's lifetime is
	// governed entirely by the caller's context (sseCancel in the
	// driver). Server liveness is verified separately by the
	// liveness-probe goroutine hitting /api/health — keeping the
	// "is the server alive?" check off the SSE wire so a silent
	// stretch during model thinking never tears down a healthy
	// connection.
	//
	// ResponseHeaderTimeout bounds ONLY the response-header phase
	// (TCP connect + TLS + first byte). Once a 2xx lands the body
	// is handed to decodeSSE which reads until EOF or ctx cancel;
	// that path is unbounded by design.
	httpSSE *http.Client
}

// newClient builds a Client targeting the given serverProc.
//
// Neither client has an http.Client.Timeout: short requests are
// bounded at the call sites (Prompt uses promptTimeout via
// context.WithTimeout; handshake uses handshakeTimeout; etc.), and
// the SSE stream is bounded by sseCancel. The two clients differ
// only in transport-level tuning — httpSSE pins ResponseHeaderTimeout
// so a dead server fails fast on connect instead of hanging forever
// before the SSE body even starts streaming.
func newClient(proc *serverProc, workspace string) *Client {
	return &Client{
		baseURL:   proc.baseURL,
		workspace: workspace,
		http:      &http.Client{},
		httpSSE: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 10 * time.Second,
			},
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
// The endpoint returns {"data": {SessionInputAdmitted}}; we keep only
// the parts we render (the message id for EventAgentResult, the
// timestamps for future use). The runtime doesn't need the full
// SessionInputAdmitted shape.
type PromptResult struct {
	Data struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
	} `json:"data"`
}

// CreateSession calls POST /api/session.
func (c *Client) CreateSession(ctx context.Context, opts CreateSessionOpts) (*Session, error) {
	req, err := c.newRequest(ctx, "POST", "/api/session", opts)
	if err != nil {
		return nil, err
	}
	return c.decodeSession(req)
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
	return c.decodeSession(req)
}

// decodeSession is the shared decoder for single-session responses.
// opencode 1.18 wraps them in a `data` envelope:
//
//	{"data": {"id": "...", ...}}
//
// older versions and the mock test server return the Session
// directly. We read the body once, then try both shapes so the
// caller never sees a redundant HTTP round-trip (the previous
// re-issue path was responsible for the doubled test count).
func (c *Client) decodeSession(req *http.Request) (*Session, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("opencode: %s %s: %d: %s",
			req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("opencode: decode: %w", err)
	}

	// Try wrapped form first.
	var wrapped struct {
		Data *Session `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Data != nil && wrapped.Data.ID != "" {
		return wrapped.Data, nil
	}
	// Fall back to bare form.
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("opencode: decode: %w", err)
	}
	return &s, nil
}

// Prompt calls POST /api/session/{id}/prompt. The server returns when
// the turn is fully complete (synchronous). We use this rather than
// prompt_async so the bridge's existing turn-completion signaling
// (session.idle SSE) lines up with the prompt response.
//
// Body shape (opencode 1.18 OpenAPI):
//
//	{ "prompt": { "text": "...", "files": [...] } }
//
// The bridge flattens the legacy PartInput union (text / file) into
// this single-text-plus-files shape — opencode no longer accepts a
// multi-part array.
//
// The bridge deliberately does NOT pass providerID / modelID: model
// selection is opencode's responsibility, not ours. runtime only
// owns the `/use OpenCode` step (start the agent); everything after
// that — including picking which provider/model to dispatch to —
// is opencode's internal concern. If no model is configured on the
// opencode side, the user fixes it via the opencode TUI (which
// writes ~/.local/state/opencode/model.json); opencode reads that
// file at session creation time.
func (c *Client) Prompt(ctx context.Context, sessionID string, parts []PartInput) (*PromptResult, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("opencode: empty session id")
	}
	text, files := flattenPrompt(parts)
	body := map[string]any{
		"prompt": map[string]any{
			"text":  text,
			"files": files,
		},
	}
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

// flattenPrompt joins multiple text parts with "\n\n" and
// collects file/image parts into a single files array. The
// opencode PromptInput schema is intentionally simpler than the
// legacy multi-part design — we keep the bridge's []PartInput
// surface so callers don't change, but the on-wire shape is
// flat.
//
// `files` is always non-nil because opencode's payload validator
// rejects null with "Expected array, got null" (stage 7 e2e
// regression against real opencode). An empty slice serializes
// as `[]` which passes.
func flattenPrompt(parts []PartInput) (string, []PromptInputFile) {
	var textParts []string
	files := []PromptInputFile{}
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				textParts = append(textParts, p.Text)
			}
		case "file":
			if p.URL == "" {
				continue
			}
			files = append(files, PromptInputFile{
				MIME: p.MIME,
				URL:  p.URL,
				Name: p.Filename,
			})
		}
	}
	return strings.Join(textParts, "\n\n"), files
}

// PromptInputFile mirrors the OpenAPI PromptInputFileAttachment —
// one file per attachment. opencode then uploads/reads it
// server-side.
//
// Note: the on-wire field is `uri`, NOT `url`. `url` only appears
// on the response-shaped FilePart / FilePartInput schemas used to
// describe already-stored parts; the prompt INPUT attachment
// schema requires `uri` (verified against GET /doc on opencode
// 1.x). Sending `url` here returns 400 "Missing key at
// prompt.files[N].uri" from opencode's payload validator.
type PromptInputFile struct {
	MIME string `json:"mime,omitempty"`
	URL  string `json:"uri"`
	Name string `json:"filename,omitempty"`
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

// Compact calls POST /api/session/{id}/compact. The server compacts
// the conversation history (drops intermediate turns, sums the
// context) and returns 204 on success. The session id is unchanged;
// the next /prompt response carries the fresh token totals.
//
// We surface this to the runtime so a "context full" hint in the
// footer can drive a /compact slash command rather than waiting
// for the model to error out.
func (c *Client) Compact(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("opencode: empty session id")
	}
	req, err := c.newRequest(ctx, "POST", "/api/session/"+url.PathEscape(sessionID)+"/compact", nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}

// ListSessions returns the sessions known to the server, scoped
// to the bridge's workspace. Used by the runtime to surface a
// resume picker when the user runs "/use opencode" with a saved
// session id. The query params mirror the OpenAPI spec; the
// `directory` query is enforced by the server-side instance
// context so we don't need to pass it when the client already
// sets the `x-opencode-directory` header.
func (c *Client) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	path := "/api/session"
	if limit > 0 {
		path += fmt.Sprintf("?limit=%d", limit)
	}
	req, err := c.newRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	// List responses are wrapped in {data: [...], cursor: ...}.
	// Older versions return a bare array. We try both.
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode: list sessions: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("opencode: list sessions: %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("opencode: list: %w", err)
	}
	var wrapped struct {
		Data []Session `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Data) > 0 {
		return wrapped.Data, nil
	}
	var bare []Session
	if err := json.Unmarshal(raw, &bare); err != nil {
		return nil, fmt.Errorf("opencode: list decode: %w", err)
	}
	return bare, nil
}

// Provider is the subset of GET /api/provider we read. Only the
// fields needed to drive a /model picker are kept.
type Provider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Source  string `json:"source"`
	Env     []string `json:"env"`
	Key     string `json:"key"`
	Options any  `json:"options"`
	Models  map[string]any `json:"models"`
}

// ListProviders calls GET /api/provider. Returns the configured
// providers so the runtime can populate a /model picker. The
// response shape mirrors the OpenAPI spec; we wrap it in {data:
// [...]} fallback for older versions.
func (c *Client) ListProviders(ctx context.Context) ([]Provider, error) {
	req, err := c.newRequest(ctx, "GET", "/api/provider", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode: list providers: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("opencode: list providers: %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("opencode: list providers: %w", err)
	}
	var wrapped struct {
		Data []Provider `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Data) > 0 {
		return wrapped.Data, nil
	}
	var bare []Provider
	if err := json.Unmarshal(raw, &bare); err != nil {
		return nil, fmt.Errorf("opencode: list providers decode: %w", err)
	}
	return bare, nil
}

// ListModels calls GET /api/model. Convenience for the runtime —
// some call sites want a flat list of (providerID, modelID) pairs.
func (c *Client) ListModels(ctx context.Context) (map[string]any, error) {
	req, err := c.newRequest(ctx, "GET", "/api/model", nil)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := c.doJSON(req, &out); err != nil {
		return nil, err
	}
	return out, nil
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
