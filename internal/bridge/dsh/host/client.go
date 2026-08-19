// Package host is the multiplexed client for a single shared dsh web
// daemon. It is the Phase 0 (F-DSH-MULTI) stepping stone toward the
// 1:N architecture where one long-lived `dsh --profile web` instance
// serves every ChatSession / AgentSession in the nightme daemon via
// sessionId multiplexing.
//
// # Phase 0 scope (this commit)
//
//   - RPCClient: shared HTTP RPC transport for /api/{method}.
//   - StreamHub: single mux + host WebSocket connection pair with
//     reconnect-on-loss + backoff. Frames emitted via callback.
//   - Router: per-session mux subscription table + shared pending
//     approval/question channel table keyed by (sessionId, frame.rpcId).
//   - Client: facade wiring RPC + Hub + Router into one struct.
//
// Phase 0 deliberately does NOT touch the existing per-driver code
// in the parent dsh package. The two coexist: production paths still
// use the per-driver model, while Phase 0 builds the new shape in
// isolation. Phase 3 will consolidate.
//
// # Wire protocol references
//
//   - docs/bridge/dsh-api.md §0–§5: full HTTP/WS contract
//   - docs/bridge/dsh-api.md §3.4.1: mux stream is one-stream-N-sessions
//   - docs/bridge/dsh-api.md §3.4.6 / §3.4.9: rpcId is the answer key
package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"github.com/cnlangzi/nightme/internal/httpclient"
)

// ─── Envelope types (mirrors of dsh/protocol.go) ────────────────────
//
// These intentionally mirror the parent package's unexported envelope
// types rather than re-export them. Phase 0 keeps the two packages
// decoupled; Phase 3 will move the wire types to a shared internal/dshwire
// package and delete these duplicates.

// clientRequest is the wire envelope for every POST /api/{method}.
// `Type` MUST be "client-request" — the server validates against
// clientRequestSchema (dsh-api.md §1.1).
type clientRequest struct {
	Type    string          `json:"type"`
	RPCID   string          `json:"rpcId"`
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// rpcResponse is the envelope every POST /api/{method} returns.
// `Type` is "server-response"; `Result.OK` discriminates success
// from business error.
type rpcResponse struct {
	Type   string    `json:"type"`
	RPCID  string    `json:"rpcId"`
	Result rpcResult `json:"result"`
}

// rpcResult is the inner envelope of rpcResponse.result.
type rpcResult struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value,omitempty"`
	Error *rpcError       `json:"error,omitempty"`
}

// rpcError is the bridge's view of a server-side business error.
// `Code` strings come from dsh-api.md §6 (RpcErrorDetailsMap).
type rpcError struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

// ErrorMessage returns a one-line human-readable form. "" when nil
// or when the inner Error is absent (defensive — shouldn't happen).
func (r *rpcResult) ErrorMessage() string {
	if r == nil || r.Error == nil {
		return ""
	}
	return r.Error.Code + ": " + r.Error.Message
}

// ─── RPCClient ─────────────────────────────────────────────────────

// httpClientTimeout bounds every individual POST. Long-running
// actions (session.prompt, session.history) carry their own per-call
// deadline via ctx — this constant is the upper bound when the caller
// passes no deadline. Matches the existing dsh/http.go value so
// concurrent calls in both layers behave identically.
const httpClientTimeout = 30 * time.Second

// httpBodyReadLimit caps how many bytes we read off the wire per
// response. 16 MiB matches the existing dsh/http.go ceiling.
const httpBodyReadLimit = 16 * 1024 * 1024

// httpErrorBodyLimit caps how much of a non-200 body we surface
// in an error message. 1 KiB matches the existing dsh/http.go.
const httpErrorBodyLimit = 1024

// RPCClient is the shared HTTP RPC transport. It is stateless apart
// from baseURL + the underlying *http.Client, so it is safe to share
// across goroutines and across all session-bearing code paths.
//
// One instance per nightme daemon — every /api/{method} call from
// any ChatSession / AgentSession flows through this single client.
type RPCClient struct {
	baseURL string
	http    *http.Client
}

// NewRPCClient constructs a client rooted at baseURL (e.g.
// "http://127.0.0.1:3080"). The trailing slash, if any, is dropped
// because URL joining adds one (matches dsh/http.go newHTTPClient).
func NewRPCClient(baseURL string) *RPCClient {
	baseURL = strings.TrimRight(baseURL, "/")
	return &RPCClient{
		baseURL: baseURL,
		http:    httpclient.DefaultWithTimeout(httpClientTimeout),
	}
}

// BaseURL returns the root URL the client POSTs against. Useful for
// tests + observability.
func (c *RPCClient) BaseURL() string {
	return c.baseURL
}

// Post issues one RPC. `payload` is JSON-marshaled into the
// clientRequest envelope; the response is decoded into rpcResponse
// and returned.
//
// Returns:
//   - (resp, nil) on transport OK + business OK (resp.Result.OK == true)
//   - (resp, nil) on transport OK + business failure — caller reads resp.Result.Error
//   - (nil, err) on transport / decode / id-mismatch failure
//
// Mirrors dsh/http.go Post so concurrent callers see the same wire
// contract. Phase 0 keeps the two implementations separate so
// existing tests don't break; Phase 3 will collapse.
func (c *RPCClient) Post(ctx context.Context, method string, payload any) (*rpcResponse, error) {
	rpcID := newRPCID()

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: marshal payload for %s: %w", method, err)
	}

	envelope := clientRequest{
		Type:    "client-request",
		RPCID:   rpcID,
		Method:  method,
		Payload: payloadBytes,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: marshal envelope for %s: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("dsh.host: build request %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	httpResp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: POST %s: %w", method, err)
	}
	defer httpResp.Body.Close()

	// Non-200 = transport / framework failure (dsh-api.md §1.2).
	// 404 unknown method, 415 non-JSON, 500 handler crash, 403 CORS.
	if httpResp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(httpResp.Body, httpErrorBodyLimit))
		return nil, fmt.Errorf("dsh.host: POST %s: HTTP %d: %s",
			method, httpResp.StatusCode, string(bodyBytes))
	}

	respBytes, err := io.ReadAll(io.LimitReader(httpResp.Body, httpBodyReadLimit))
	if err != nil {
		return nil, fmt.Errorf("dsh.host: read body for %s: %w", method, err)
	}

	var resp rpcResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, fmt.Errorf("dsh.host: decode %s response: %w (body=%s)",
			method, err, truncate(string(respBytes), 200))
	}

	if resp.RPCID != rpcID {
		return nil, fmt.Errorf("dsh.host: POST %s rpcId mismatch (sent %s, got %s)",
			method, rpcID, resp.RPCID)
	}
	if resp.Type != "server-response" {
		return nil, fmt.Errorf("dsh.host: POST %s unexpected response type=%q",
			method, resp.Type)
	}

	return &resp, nil
}

// PostRaw is like Post but accepts a pre-marshaled RawMessage payload.
// Used by typed wrappers that build the payload inline (e.g. Respond
// constructs ApprovalResponsePayload without going through map[string]any).
//
// NOTE: PostRaw still wraps in the standard clientRequest envelope —
// it just lets you pass a pre-marshaled payload instead of a Go value.
// For envelopes that don't fit clientRequest (e.g. /api/respond's
// client-response shape), use PostEnvelope instead.
func (c *RPCClient) PostRaw(ctx context.Context, method string, payload json.RawMessage) (*rpcResponse, error) {
	return c.Post(ctx, method, payload)
}

// PostEnvelope POSTs a pre-built raw JSON body without wrapping in
// the standard clientRequest envelope. Used for /api/respond which
// uses the client-response envelope (dsh-api.md §2.12) — type:"client-response",
// rpcId echoing the server-frame's rpcId, result:{ok,value}.
//
// IMPORTANT: /api/respond's response body is an RpcReceipt
// ({accepted: true} or {accepted: false, reason: …}), NOT the
// standard server-response envelope (dsh-api.md §2.12). So this
// method returns nil error on HTTP 200 + {accepted:true}; the
// {accepted: false} case surfaces as an error so callers can decide.
//
// The server still expects HTTP 200 + a reasonable response on
// the response side; the difference from Post is purely on the
// request body shape.
func (c *RPCClient) PostEnvelope(ctx context.Context, method string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("dsh.host: build request %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	httpResp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("dsh.host: POST %s: %w", method, err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(httpResp.Body, httpErrorBodyLimit))
		return fmt.Errorf("dsh.host: POST %s: HTTP %d: %s",
			method, httpResp.StatusCode, string(bodyBytes))
	}

	respBytes, err := io.ReadAll(io.LimitReader(httpResp.Body, httpBodyReadLimit))
	if err != nil {
		return fmt.Errorf("dsh.host: read body for %s: %w", method, err)
	}

	var receipt struct {
		Accepted bool   `json:"accepted"`
		Reason   string `json:"reason,omitempty"`
	}
	if err := json.Unmarshal(respBytes, &receipt); err != nil {
		return fmt.Errorf("dsh.host: decode %s receipt: %w (body=%s)",
			method, err, truncate(string(respBytes), 200))
	}
	if !receipt.Accepted {
		return fmt.Errorf("dsh.host: %s: server rejected response: %s",
			method, receipt.Reason)
	}
	return nil
}

// ─── Typed wrappers (the ones Phase 0 actually needs) ──────────────

// SessionSummary is one entry in session.list. Mirrors dsh.Session
// (parent package's exported shape — kept identical so Phase 3
// consolidation can drop the duplicate).
type SessionSummary struct {
	SessionID   string          `json:"sessionId"`
	UpdatedAt   int64           `json:"updatedAt"`
	Running     bool            `json:"running"`
	Blank       bool            `json:"blank"`
	CWD         string          `json:"cwd,omitempty"`
	AgentPreset string          `json:"agentPreset,omitempty"`
	Projections json.RawMessage `json:"projections,omitempty"`
}

// SessionList queries /api/session.list. Used by Phase 4 restart-
// recovery: match persisted sessionIds against current server state.
func (c *RPCClient) SessionList(ctx context.Context) ([]SessionSummary, error) {
	resp, err := c.Post(ctx, "session.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	if !resp.Result.OK {
		return nil, fmt.Errorf("dsh.host: session.list: %s", resp.Result.ErrorMessage())
	}
	var value struct {
		Items []SessionSummary `json:"items"`
	}
	if err := json.Unmarshal(resp.Result.Value, &value); err != nil {
		return nil, fmt.Errorf("dsh.host: session.list decode: %w", err)
	}
	return value.Items, nil
}

// SessionCreateOpts is the wire body for /api/session.create.
// Exactly one of WorkspaceID / CWD may be set (dsh-api.md §2.1.3).
type SessionCreateOpts struct {
	WorkspaceID string `json:"workspaceId,omitempty"`
	CWD         string `json:"cwd,omitempty"`
	SessionID   string `json:"sessionId,omitempty"` // preallocate id
	AgentPreset string `json:"agentPreset,omitempty"`
}

// WorkspaceList queries /api/workspace.list. Used by EnsureWorkspace
// to dedupe: rather than blindly create a new workspace, we look
// up an existing one with the same path and reuse it.
func (c *RPCClient) WorkspaceList(ctx context.Context) ([]WorkspaceSummary, error) {
	resp, err := c.Post(ctx, "workspace.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	if !resp.Result.OK {
		return nil, fmt.Errorf("dsh.host: workspace.list: %s", resp.Result.ErrorMessage())
	}
	var value struct {
		Items []WorkspaceSummary `json:"items"`
	}
	if err := json.Unmarshal(resp.Result.Value, &value); err != nil {
		return nil, fmt.Errorf("dsh.host: workspace.list decode: %w", err)
	}
	return value.Items, nil
}

// WorkspaceSummary is the on-wire shape of one workspace.list row
// (and the per-workspace sub-object of workspace.create responses).
// The dsh host-apiproxy dsh-host-apiproxy/lib/types/api/workspace.schema
// emits `workspaceId` (not `id`) — see workspaceView() in that file.
// The earlier "id" tag here made handshakeSession blow up on
// /reset because the empty ID propagated up as "empty workspaceId
// in response" and the bridge refused start.
type WorkspaceSummary struct {
	WorkspaceID string   `json:"workspaceId"`
	Path        string   `json:"path"`
	Title       string   `json:"title"`
	SessionIDs  []string `json:"sessionIds,omitempty"`
	// dsh returns ISO 8601 strings for createdAt / updatedAt /
	// archivedAt (e.g. "2026-08-16T08:24:45.015Z") even though the
	// dsh-host-apiproxy zod schema declares them as z.number().
	// The wire is the source of truth — match what the server
	// sends, not what the schema thinks. Using *int64 here makes
	// Unmarshal fail with "cannot unmarshal string into...field
	// WorkspaceSummary.workspace.createdAt of type int64" and the
	// whole workspace.create round-trips fails.
	CreatedAt  string `json:"createdAt,omitempty"`
	UpdatedAt  string `json:"updatedAt,omitempty"`
	ArchivedAt string `json:"archivedAt,omitempty"`
}

// WorkspaceCreate resolves or creates one workspace for `path`. The
// dsh server itself dedupes by path (workspaceRegistry.resolveByPath +
// create) so a single call from the bridge is enough; we just call
// workspace.create and trust the server's response.
func (c *RPCClient) WorkspaceCreate(ctx context.Context, path string) (WorkspaceSummary, error) {
	resp, err := c.Post(ctx, "workspace.create", map[string]any{"path": path})
	if err != nil {
		return WorkspaceSummary{}, err
	}
	if !resp.Result.OK {
		return WorkspaceSummary{}, fmt.Errorf("dsh.host: workspace.create: %s", resp.Result.ErrorMessage())
	}
	var value struct {
		Workspace WorkspaceSummary `json:"workspace"`
	}
	if err := json.Unmarshal(resp.Result.Value, &value); err != nil {
		return WorkspaceSummary{}, fmt.Errorf("dsh.host: workspace.create decode: %w", err)
	}
	if value.Workspace.WorkspaceID == "" {
		return WorkspaceSummary{}, fmt.Errorf("dsh.host: workspace.create: empty workspaceId in response")
	}
	return value.Workspace, nil
}

// WorkspaceArchiveSession is the dashboard session-row context menu
// "Archive Session": POST /api/workspace.archiveSession {sessionId}
// (dsh-api.md §2.4.7). The session drops off every grouping surface
// (left list, workspace sessionIds) but keeps its log and workspace
// accounting slot. Idempotent for an already-archived id.
// session-not-found when the id is neither live nor persisted.
func (c *RPCClient) WorkspaceArchiveSession(ctx context.Context, sessionID string) error {
	resp, err := c.Post(ctx, "workspace.archiveSession", map[string]any{
		"sessionId": sessionID,
	})
	if err != nil {
		return err
	}
	if !resp.Result.OK {
		return fmt.Errorf("dsh.host: workspace.archiveSession: %s", resp.Result.ErrorMessage())
	}
	return nil
}

// WorkspaceDelete removes one workspace and every session attached to
// it (the dsh server tears down sessions in workspaceRegistry.delete).
// Best-effort: callers log the error but don't propagate, since
// shutdown still proceeds even if dsh is unreachable.
func (c *RPCClient) WorkspaceDelete(ctx context.Context, workspaceID string) error {
	resp, err := c.Post(ctx, "workspace.delete", map[string]any{"workspaceId": workspaceID})
	if err != nil {
		return err
	}
	if !resp.Result.OK {
		return fmt.Errorf("dsh.host: workspace.delete: %s", resp.Result.ErrorMessage())
	}
	return nil
}

// SessionCreate invokes /api/session.create. Returns the new
// sessionId. Phase 2 will call this from ChatSession.Spawner; Phase 0
// is just plumbing.
func (c *RPCClient) SessionCreate(ctx context.Context, opts SessionCreateOpts) (string, error) {
	resp, err := c.Post(ctx, "session.create", opts)
	if err != nil {
		return "", err
	}
	if !resp.Result.OK {
		return "", fmt.Errorf("dsh.host: session.create: %s", resp.Result.ErrorMessage())
	}
	var value struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(resp.Result.Value, &value); err != nil {
		return "", fmt.Errorf("dsh.host: session.create decode: %w", err)
	}
	if value.SessionID == "" {
		return "", fmt.Errorf("dsh.host: session.create: empty sessionId in response")
	}
	return value.SessionID, nil
}

// PromptPart mirrors dsh-api.md §2.1.9.1 PromptContentPart.
// Phase 2 will provide typed mappers from agent.ContentBlock;
// Phase 0 uses []map[string]any in tests for simplicity.
type PromptPart struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	Data      string `json:"data,omitempty"`
	Name      string `json:"name,omitempty"`
}

// SessionPrompt invokes /api/session.prompt. `mode` MUST be
// "queue" or "steer" (dsh-api.md §2.1.9 — omitting it returns
// bad-request: invalid input: expected "queue").
func (c *RPCClient) SessionPrompt(ctx context.Context, sessionID, mode string, parts []PromptPart) error {
	resp, err := c.Post(ctx, "session.prompt", map[string]any{
		"sessionId": sessionID,
		"mode":      mode,
		"content":   parts,
	})
	if err != nil {
		return err
	}
	if !resp.Result.OK {
		return fmt.Errorf("dsh.host: session.prompt: %s", resp.Result.ErrorMessage())
	}
	return nil
}

// SessionCancel invokes /api/session.cancel. Best-effort: the server
// may already have settled the turn; a "session-not-found" or
// "agent-busy" reply is still acceptable. Phase 0 surfaces the
// error so callers can decide; Phase 2 will codify the lenient
// semantics the existing bridge uses in session.go:Close.
func (c *RPCClient) SessionCancel(ctx context.Context, sessionID string) error {
	resp, err := c.Post(ctx, "session.cancel", map[string]any{"sessionId": sessionID})
	if err != nil {
		return err
	}
	if !resp.Result.OK {
		return fmt.Errorf("dsh.host: session.cancel: %s", resp.Result.ErrorMessage())
	}
	return nil
}

// ApprovalResponse is the inner body for POST /api/respond when
// answering approval/requested. dsh-api.md §2.12.1: outcome is
// "allowed-once" or "rejected" — the client-giveable subset of
// ApprovalOutcome. Host-side outcomes ("cancelled", "unavailable")
// never ride this envelope.
type ApprovalResponse struct {
	SessionID  string `json:"sessionId"`
	ApprovalID string `json:"approvalId,omitempty"` // audit-only correlation
	Outcome    string `json:"outcome"`              // "allowed-once" | "rejected"
}

// QuestionResponse is the inner body for POST /api/respond when
// answering question/requested (dsh-api.md §2.12.2). Host
// matchesQuestions requires answers.length == questions.length,
// each answer.id echoing AskUserQuestionItem.id, in order.
type QuestionResponse struct {
	SessionID string         `json:"sessionId"`
	Answer    QuestionAnswer `json:"answer"`
}

// QuestionAnswer is AskUserQuestionAnswer: one batch covering every
// question in the originating ask().
type QuestionAnswer struct {
	Answers []QuestionAnswerItem `json:"answers"`
}

// QuestionAnswerItem is one AskUserQuestionAnswerItem. Selected is
// always serialized (empty array, never JSON null). Custom is
// omitted when empty — host rejects custom:"".
type QuestionAnswerItem struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected"`
	Custom   string   `json:"custom,omitempty"`
}

// Respond sends the answer to a server-pushed approval/requested
// or question/requested. Per dsh-api.md §2.12, /api/respond uses
// the client-response envelope; the `rpcId` field is the
// envelope-level echo of the server-frame's rpcId (NOT a fresh
// client-minted id — that is what makes it correlate).
//
// `value` is ApprovalResponse or QuestionResponse depending on the
// originating frame method.
func (c *RPCClient) Respond(ctx context.Context, frameRpcID string, value any) error {
	// envelope: {type:"client-response", rpcId:<echoed>, result:{ok, value}}
	// Built inline as json.RawMessage so we ship the literal envelope
	// shape — PostEnvelope does NOT wrap in clientRequest.
	body, err := json.Marshal(map[string]any{
		"type":   "client-response",
		"rpcId":  frameRpcID,
		"result": map[string]any{"ok": true, "value": value},
	})
	if err != nil {
		return fmt.Errorf("dsh.host: respond marshal: %w", err)
	}
	return c.PostEnvelope(ctx, "respond", body)
}

// ─── helpers ───────────────────────────────────────────────────────

// newRPCID mints a client-request id. crypto/rand + RFC 4122 §4.4
// — same recipe as dsh/http.go so concurrent calls from both layers
// use the same id shape.
func newRPCID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read never errors on Linux/macOS; if it does
		// (sandbox edge case) fall back to nanosecond timestamp.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}

// truncate returns the first n bytes of s, with "…" appended if
// longer. Keeps log lines bounded.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
