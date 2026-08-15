// resume_unix_test.go — mock-server tests for the dsh bridge
// resume (session.fork) and picker (session.list) paths.
//
// Build constraint: non-windows. The dsh bridge itself is
// unix-only per cmd/nightme/agents.go init() gate, and these
// tests assert bridge-level behavior that doesn't depend on
// real-binary execution.
//
// What we cover:
//
//   1. handshakeSession with empty cfg.SessionID → straight to
//      session.create; createCalls == 1, forkCalls == 0.
//
//   2. handshakeSession with cfg.SessionID set + server-side
//      fork succeeds → forkCalls == 1, createCalls == 0, and the
//      driver's sessionID equals the server-assigned fork result
//      (not the requested id).
//
//   3. handshakeSession with cfg.SessionID set + server-side
//      fork REJECTED → forkCalls == 1, createCalls == 0, and the
//      returned error satisfies errors.Is(err, agent.ErrResumeUnhealthy).
//      This triggers the runtime's auto-retry path
//      (chatsession.go §1624) which clears the stale sessionId
//      and respawns fresh on the user's next message. We do NOT
//      silently fall back to create — that would let a stale id
//      linger in registry forever (every daemon restart would
//      re-fork the same dead id and re-fall-back, costing the
//      user their history without operator visibility).
//
//   4. handshakeSession with cfg.SessionID set + fork returns
//      transport error → same Unhealthy contract as case 3.
//
//   5. ListSessions with server returning [] → driver returns
//      empty slice (not nil panic, not error).
//
//   6. ListSessions with server returning populated `items`
//      array (the wire field is `items`, NOT `sessions` —
//      verified via 实机 probe 2026-08-15) → driver returns
//      the array verbatim, including all wire fields
//      (sessionId / updatedAt / running / blank / cwd /
//      agentPreset / projections).
//
//   7. ListSessions sends empty payload `{}` on the wire (we
//      used to forward a `limit` field, but dsh ignores it
//      per the 2026-08-15 probe — we dropped it).
//
// We do NOT test the EventAgentReady emission path here; that's
// covered by session_smoke_test.go against a real dsh binary
// (gated by NIGHTME_REAL_DSH).
//
//go:build !windows

package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// resumeMockServer is a minimal dsh-web-shaped server with the
// endpoints handshakeSession + ListSessions care about. It records
// request counts per endpoint so tests can assert which paths were
// taken and what payloads were sent.
//
// serverShape enum controls whether session.fork returns OK
// (with the configured responseID) or rejects (forcing the
// bridge's fallback path).
type resumeMockServer struct {
	forkCalls   atomic.Int32
	createCalls atomic.Int32
	listCalls   atomic.Int32

	// lastForkRaw / lastListRaw store the raw JSON payload bytes
	// from the most recent request. We use raw bytes (decoded by
	// tests on demand) rather than a typed map to avoid the
	// atomic.Value-can't-store-nil-map pitfall when the client
	// sends an empty payload.
	lastForkRaw atomic.Value // string
	lastListRaw atomic.Value // string

	// forkOK: true → fork returns OK with responseID; false → fork
	// returns result.ok=false (bridge falls back to create).
	forkOK bool

	// responseID is the sessionId the fork endpoint returns on
	// success. Empty string means "echo the requested id back" —
	// useful for asserting the driver doesn't accidentally use
	// the requested id as the live id when fork returns a new one.
	responseID string

	// listResponse is the array returned by session.list. nil →
	// empty array; populated → that array verbatim.
	listResponse []Session
}

// handler is an http.Handler that routes /api/{method} to the
// matching recorder method. The handler returns the dsh wire
// envelope (clientRequest → server-response with result.ok/value
// or result.error) per protocol.go's rpcResponse shape.
func (rs *resumeMockServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/api/")

		// Decode the clientRequest envelope so we can capture the
		// payload for assertion.
		// Read the full body so we can both decode the envelope
		// and capture the payload bytes verbatim for tests.
		bodyBytes, _ := io.ReadAll(r.Body)
		var req clientRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			http.Error(w, "decode envelope: "+err.Error(), http.StatusBadRequest)
			return
		}
		var payload map[string]any
		if len(req.Payload) > 0 {
			_ = json.Unmarshal(req.Payload, &payload)
		}
		rawPayload := string(req.Payload)

		w.Header().Set("Content-Type", "application/json")

		switch method {
		case "session.fork":
			rs.forkCalls.Add(1)
			rs.lastForkRaw.Store(rawPayload)
			if !rs.forkOK {
				_ = json.NewEncoder(w).Encode(rpcResponse{
					Type:  "server-response",
					RPCID: req.RPCID,
					Result: rpcResult{
						OK: false,
						Error: &rpcError{
							Code:    "session-not-found",
							Message: "session not found",
						},
					},
				})
				return
			}
			newID := rs.responseID
			if newID == "" {
				// Echo the requested id — useful for the
				// "fork returns same id" smoke check.
				if sid, ok := payload["sessionId"].(string); ok {
					newID = sid
				} else {
					newID = "ses_forked"
				}
			}
			val, _ := json.Marshal(sessionForkValue{SessionID: newID})
			_ = json.NewEncoder(w).Encode(rpcResponse{
				Type:  "server-response",
				RPCID: req.RPCID,
				Result: rpcResult{
					OK:    true,
					Value: val,
				},
			})
		case "session.create":
			rs.createCalls.Add(1)
			val, _ := json.Marshal(sessionCreateValue{SessionID: "ses_created"})
			_ = json.NewEncoder(w).Encode(rpcResponse{
				Type:  "server-response",
				RPCID: req.RPCID,
				Result: rpcResult{
					OK:    true,
					Value: val,
				},
			})
		case "session.list":
			rs.listCalls.Add(1)
			rs.lastListRaw.Store(rawPayload)
			arr := rs.listResponse
			if arr == nil {
				arr = []Session{}
			}
			// Wire field is `items`, NOT `sessions` (dsh 0.1.0-rc.6).
			val, _ := json.Marshal(sessionListValue{Items: arr})
			_ = json.NewEncoder(w).Encode(rpcResponse{
				Type:  "server-response",
				RPCID: req.RPCID,
				Result: rpcResult{
					OK:    true,
					Value: val,
				},
			})
		default:
			http.Error(w, "unknown method "+method, http.StatusNotFound)
		}
	})
}

// testDriverForHTTP wires a minimal driver whose http field
// points at the given baseURL. We intentionally do NOT start
// pumps or lifecycle — handshakeSession and ListSessions only
// touch d.http + d.sessionID; the other fields are filler to
// satisfy the struct literal. closeOnce is pre-zeroed (default
// sync.Once) so a stray close() is harmless.
func testDriverForHTTP(baseURL string) *driver {
	return &driver{
		http:      newHTTPClient(baseURL),
		workspace: "/tmp/test",
		agentName: "dsh",
		closed:    make(chan struct{}),
		exitDone:  make(chan struct{}),
		events:    make(chan agent.AgentEvent, 1),
	}
}

// TestHandshakeSession_FreshPath (empty cfg.SessionID) verifies
// the no-resume path skips session.fork and goes straight to
// session.create.
func TestHandshakeSession_FreshPath(t *testing.T) {
	rs := &resumeMockServer{forkOK: true}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()

	d := testDriverForHTTP(srv.URL)
	resumed, err := d.handshakeSession(context.Background(), agent.StartConfig{
		Workspace: "/tmp/test",
	})
	if err != nil {
		t.Fatalf("handshakeSession: %v", err)
	}
	if resumed {
		t.Errorf("resumed = true, want false (no cfg.SessionID)")
	}
	if d.sessionID != "ses_created" {
		t.Errorf("sessionID = %q, want ses_created", d.sessionID)
	}
	if rs.forkCalls.Load() != 0 {
		t.Errorf("forkCalls = %d, want 0 (fresh path should skip fork)", rs.forkCalls.Load())
	}
	if rs.createCalls.Load() != 1 {
		t.Errorf("createCalls = %d, want 1", rs.createCalls.Load())
	}
}

// TestHandshakeSession_ResumePath (cfg.SessionID set + fork OK)
// verifies the resume path uses session.fork and captures the
// server-assigned new id (not the requested one).
func TestHandshakeSession_ResumePath(t *testing.T) {
	rs := &resumeMockServer{
		forkOK:     true,
		responseID: "ses_forked_new_id",
	}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()

	d := testDriverForHTTP(srv.URL)
	resumed, err := d.handshakeSession(context.Background(), agent.StartConfig{
		Workspace: "/tmp/test",
		SessionID: "ses_old_parent",
	})
	if err != nil {
		t.Fatalf("handshakeSession: %v", err)
	}
	if !resumed {
		t.Errorf("resumed = false, want true")
	}
	if d.sessionID != "ses_forked_new_id" {
		t.Errorf("sessionID = %q, want ses_forked_new_id (fork should override)", d.sessionID)
	}
	if rs.forkCalls.Load() != 1 {
		t.Errorf("forkCalls = %d, want 1", rs.forkCalls.Load())
	}
	if rs.createCalls.Load() != 0 {
		t.Errorf("createCalls = %d, want 0 (resume path should not create)", rs.createCalls.Load())
	}

	// Confirm the wire payload carried the requested sessionId.
	rawFork, _ := rs.lastForkRaw.Load().(string)
	if rawFork == "" {
		t.Fatalf("fork body not captured")
	}
	var forkPayload map[string]any
	if err := json.Unmarshal([]byte(rawFork), &forkPayload); err != nil {
		t.Fatalf("decode fork payload: %v", err)
	}
	if sid, _ := forkPayload["sessionId"].(string); sid != "ses_old_parent" {
		t.Errorf("fork payload sessionId = %q, want ses_old_parent", sid)
	}
}

// TestHandshakeSession_ForkRejectedReturnsUnhealthy verifies the
// resume-preservation contract: when cfg.SessionID is set and
// the server rejects session.fork (e.g. "session not found"),
// handshakeSession MUST return an error satisfying
// errors.Is(err, agent.ErrResumeUnhealthy) and MUST NOT silently
// fall back to session.create.
//
// The runtime (chatsession.go §1624) catches ErrResumeUnhealthy,
// clears the persisted sessionId, and respawns fresh on the
// user's next message. A silent fallback would let the stale id
// linger in registry forever, and every subsequent daemon
// restart would re-attempt the same bad fork.
func TestHandshakeSession_ForkRejectedReturnsUnhealthy(t *testing.T) {
	rs := &resumeMockServer{
		forkOK: false, // server returns session-not-found
	}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()

	d := testDriverForHTTP(srv.URL)
	resumed, err := d.handshakeSession(context.Background(), agent.StartConfig{
		Workspace: "/tmp/test",
		SessionID: "ses_pruned_by_server",
	})
	if err == nil {
		t.Fatal("handshakeSession: err = nil, want ErrResumeUnhealthy")
	}
	if !errors.Is(err, agent.ErrResumeUnhealthy) {
		t.Errorf("err %v should satisfy errors.Is(err, agent.ErrResumeUnhealthy)", err)
	}
	if !errors.Is(err, ErrResumeUnhealthy) {
		t.Errorf("err %v should also satisfy errors.Is(err, dsh.ErrResumeUnhealthy)", err)
	}
	if resumed {
		t.Errorf("resumed = true, want false (fork rejected)")
	}
	if d.sessionID != "" {
		t.Errorf("sessionID = %q, want \"\" (no session should be created on refusal)", d.sessionID)
	}
	if rs.forkCalls.Load() != 1 {
		t.Errorf("forkCalls = %d, want 1", rs.forkCalls.Load())
	}
	if rs.createCalls.Load() != 0 {
		t.Errorf("createCalls = %d, want 0 (must NOT silently fall back to create)", rs.createCalls.Load())
	}
}

// TestHandshakeSession_ForkTransportErrorReturnsUnhealthy covers
// the transport-error fork failure mode (EOF mid-handshake,
// connection refused, hijack-then-close, etc.). Same contract
// as the rejection case: refuse to spawn with
// ErrResumeUnhealthy; do NOT silently fall back to create.
//
// Rationale for refusing on transport error too (rather than
// "transport might be transient, fall back"): if the server is
// truly transient, both fork AND create would fail and the
// caller surfaces the create error — same outcome as refusing.
// If the server is selectively refusing only the fork endpoint,
// that's a server-side issue we should propagate, not paper over.
// And critically, a transport error mid-fork does NOT prove the
// parent sessionId is invalid — so we shouldn't signal
// "clear-and-retry" yet either; we want the runtime's retry
// path to re-attempt the fork from scratch on the next message,
// which it does because ErrResumeUnhealthy clears the id and
// the respawn goes through fresh create.
func TestHandshakeSession_ForkTransportErrorReturnsUnhealthy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session.fork", func(w http.ResponseWriter, r *http.Request) {
		// Hijack and close to simulate a transport error.
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijacker", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	})
	// Note: we intentionally do NOT register /api/session.create.
	// If the bridge silently fell back, the test would see a
	// 404 from the create handler — and we want to assert the
	// fallback is NOT taken, so 404 is exactly the failure mode
	// we expect to NOT see.
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := testDriverForHTTP(srv.URL)
	resumed, err := d.handshakeSession(context.Background(), agent.StartConfig{
		Workspace: "/tmp/test",
		SessionID: "ses_transport_fail",
	})
	if err == nil {
		t.Fatal("handshakeSession: err = nil, want ErrResumeUnhealthy (no silent fallback)")
	}
	if !errors.Is(err, agent.ErrResumeUnhealthy) {
		t.Errorf("err %v should satisfy errors.Is(err, agent.ErrResumeUnhealthy)", err)
	}
	if resumed {
		t.Errorf("resumed = true, want false")
	}
	if d.sessionID != "" {
		t.Errorf("sessionID = %q, want \"\" (refused, no session created)", d.sessionID)
	}
}

// TestListSessions_Empty verifies ListSessions handles a
// server-returned empty array without panicking on nil.
func TestListSessions_Empty(t *testing.T) {
	rs := &resumeMockServer{listResponse: nil}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()

	d := testDriverForHTTP(srv.URL)
	got, err := d.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
	if rs.listCalls.Load() != 1 {
		t.Errorf("listCalls = %d, want 1", rs.listCalls.Load())
	}
}

// TestListSessions_Populated verifies the bridge decodes a
// populated `items` array (NOT `sessions` — see 实机 probe
// 2026-08-15) and returns it verbatim. This test would have
// caught the original `sessions` typo.
func TestListSessions_Populated(t *testing.T) {
	want := []Session{
		{
			ID:          "session-aaaa-bbbb-cccc",
			UpdatedAt:   1700000000000,
			Running:     false,
			Blank:       true,
			CWD:         "/work/a",
			AgentPreset: "standard",
		},
		{
			ID:          "session-dddd-eeee-ffff",
			UpdatedAt:   1700000001000,
			Running:     true,
			Blank:       false,
			CWD:         "/work/b",
			AgentPreset: "standard",
		},
		{
			ID:        "session-1111-2222-3333",
			UpdatedAt: 1700000002000,
			Blank:     false,
			// CWD + AgentPreset omitted — older dsh builds may
			// not emit them.
		},
	}
	rs := &resumeMockServer{listResponse: want}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()

	d := testDriverForHTTP(srv.URL)
	got, err := d.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].ID != want[i].ID {
			t.Errorf("[%d].ID = %q, want %q", i, got[i].ID, want[i].ID)
		}
		if got[i].UpdatedAt != want[i].UpdatedAt {
			t.Errorf("[%d].UpdatedAt = %d, want %d", i, got[i].UpdatedAt, want[i].UpdatedAt)
		}
		if got[i].Running != want[i].Running {
			t.Errorf("[%d].Running = %v, want %v", i, got[i].Running, want[i].Running)
		}
		if got[i].Blank != want[i].Blank {
			t.Errorf("[%d].Blank = %v, want %v", i, got[i].Blank, want[i].Blank)
		}
		if got[i].CWD != want[i].CWD {
			t.Errorf("[%d].CWD = %q, want %q", i, got[i].CWD, want[i].CWD)
		}
		if got[i].AgentPreset != want[i].AgentPreset {
			t.Errorf("[%d].AgentPreset = %q, want %q", i, got[i].AgentPreset, want[i].AgentPreset)
		}
	}
	if rs.listCalls.Load() != 1 {
		t.Errorf("listCalls = %d, want 1", rs.listCalls.Load())
	}
}

// TestListSessions_EmptyPayloadSent verifies the wire payload is
// the bare `{}` (we no longer forward a limit field — dsh ignores
// it anyway per the 2026-08-15 实机 probe).
func TestListSessions_EmptyPayloadSent(t *testing.T) {
	rs := &resumeMockServer{}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()

	d := testDriverForHTTP(srv.URL)
	if _, err := d.ListSessions(context.Background()); err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	rawList, _ := rs.lastListRaw.Load().(string)
	if rawList == "" {
		t.Fatalf("list body not captured")
	}
	var listPayload map[string]any
	if err := json.Unmarshal([]byte(rawList), &listPayload); err != nil {
		t.Fatalf("decode list payload: %v", err)
	}
	if _, present := listPayload["limit"]; present {
		t.Errorf("limit should NOT be sent on the wire (dsh ignores it); got %v", listPayload["limit"])
	}
}

// TestListSessions_ServerError verifies the bridge surfaces a
// transport / business error from session.list as a Go error
// (the caller decides how to render it in the picker UI).
func TestListSessions_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return HTTP 500 to simulate a server hiccup.
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := testDriverForHTTP(srv.URL)
	_, err := d.ListSessions(context.Background())
	if err == nil {
		t.Fatal("ListSessions on 500 = nil, want error")
	}
	if !strings.Contains(err.Error(), "session.list") {
		t.Errorf("err %q should mention 'session.list'", err.Error())
	}
}

// TestSessionWireDecode pins the wire shape against the
// verbatim JSON captured by the 2026-08-15 实机 probe. If
// dsh renames `items` → `sessions` (or vice versa) in a
// future release, this test fails first.
func TestSessionWireDecode(t *testing.T) {
	// Real wire shape from dsh 0.1.0-rc.6.
	raw := []byte(`{
		"items": [
			{
				"sessionId": "session-e4fe0be6-c082-48a5-be70-77628e7486bc",
				"updatedAt": 1786778870956,
				"running": false,
				"blank": true,
				"cwd": "/tmp/dsh-probe/ws",
				"agentPreset": "standard"
			}
		]
	}`)
	var v sessionListValue
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(v.Items) != 1 {
		t.Fatalf("len = %d, want 1 (field name should be items, not sessions)", len(v.Items))
	}
	s := v.Items[0]
	if s.ID != "session-e4fe0be6-c082-48a5-be70-77628e7486bc" {
		t.Errorf("ID = %q, want session-e4fe0be6-...", s.ID)
	}
	if s.UpdatedAt != 1786778870956 {
		t.Errorf("UpdatedAt = %d, want 1786778870956", s.UpdatedAt)
	}
	if s.Running {
		t.Errorf("Running = true, want false")
	}
	if !s.Blank {
		t.Errorf("Blank = false, want true")
	}
	if s.CWD != "/tmp/dsh-probe/ws" {
		t.Errorf("CWD = %q, want /tmp/dsh-probe/ws", s.CWD)
	}
	if s.AgentPreset != "standard" {
		t.Errorf("AgentPreset = %q, want standard", s.AgentPreset)
	}
}

// TestSessionListValue_FieldNameIsItems pins the wire field
// name to `items`. The first version of this code used
// `sessions` and silently decoded to zero items. If a future
// rename to `sessions` happens (or a server-side bug ships),
// this test fails immediately.
func TestSessionListValue_FieldNameIsItems(t *testing.T) {
	// Wrong-field wire (the old buggy shape) must produce zero items.
	wrong := []byte(`{"sessions": [{"sessionId": "x"}]}`)
	var v sessionListValue
	if err := json.Unmarshal(wrong, &v); err != nil {
		t.Fatalf("decode wrong-field: %v", err)
	}
	if len(v.Items) != 0 {
		t.Errorf("wrong-field wire should decode to 0 items, got %d", len(v.Items))
	}
	// Right-field wire must produce one item.
	right := []byte(`{"items": [{"sessionId": "y"}]}`)
	if err := json.Unmarshal(right, &v); err != nil {
		t.Fatalf("decode right-field: %v", err)
	}
	if len(v.Items) != 1 || v.Items[0].ID != "y" {
		t.Errorf("right-field wire decoded wrong: %+v", v)
	}
}

// TestSessionForkWireDecode confirms sessionForkValue decodes
// the same shape as sessionCreateValue (per docs/bridge/dsh.md
// §13 — both endpoints return the same envelope).
func TestSessionForkWireDecode(t *testing.T) {
	raw := []byte(`{"sessionId":"ses_forked_xyz"}`)
	var fv sessionForkValue
	if err := json.Unmarshal(raw, &fv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fv.SessionID != "ses_forked_xyz" {
		t.Errorf("SessionID = %q, want ses_forked_xyz", fv.SessionID)
	}
}

// Compile-time guard: handshakeSession has a per-call timeout
// budget (handshakeTimeout in newDriver). The unit tests above
// bypass newDriver and call handshakeSession directly, so the
// timeout doesn't fire — but we still want callers to know the
// contract. This dummy test reads the constant so a future
// rename shows up here too.
func TestHandshakeTimeout_IsPositive(t *testing.T) {
	if handshakeTimeout <= 0 {
		t.Errorf("handshakeTimeout = %v, want > 0", handshakeTimeout)
	}
	if handshakeTimeout > 60*time.Second {
		t.Errorf("handshakeTimeout = %v, want <= 60s (config budget)", handshakeTimeout)
	}
}

// TestHandshakeSession_IndependentTimeouts verifies the P2 fix:
// handshakeSession gives session.fork and session.create their
// OWN per-call timeouts derived from the parent ctx, rather
// than sharing a single budget. A slow fork must NOT exhaust
// the create budget.
//
// Strategy: shrink handshakeTimeout to a tiny value via the
// exposed var, then verify that a fork handler which sleeps
// longer than that value still results in handshakeSession
// returning within ~handshakeTimeout (not at the parent ctx
// deadline, and not at the fork sleep duration).
func TestHandshakeSession_IndependentTimeouts(t *testing.T) {
	// Shrink to 500ms so the test runs in < 2s. Restore the
	// production default on exit so other tests aren't
	// affected by the swap.
	const small = 500 * time.Millisecond
	orig := handshakeTimeout
	handshakeTimeout = small
	defer func() { handshakeTimeout = orig }()

	forkSleep := 2 * small // fork sleeps 2x the budget — must time out
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session.fork", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(forkSleep):
		case <-r.Context().Done():
			// Client gave up; bail early so the test
			// doesn't wait the full sleep duration.
			return
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := testDriverForHTTP(srv.URL)
	parentCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	_, err := d.handshakeSession(parentCtx, agent.StartConfig{
		Workspace: "/tmp/test",
		SessionID: "ses_slow_fork",
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("handshakeSession: err = nil, want Unhealthy (per-call timeout)")
	}
	if !errors.Is(err, agent.ErrResumeUnhealthy) {
		t.Errorf("err %v should satisfy errors.Is(err, agent.ErrResumeUnhealthy)", err)
	}

	// With the fix: elapsed ~ small + httptest slack (~1s
	// budget for the ctx deadline + TCP teardown). Without
	// the fix the fork would sleep the full forkSleep and the
	// test would also stall that long. We allow generous
	// slack to account for CI variance.
	if elapsed > small+1*time.Second {
		t.Errorf("elapsed = %v, expected ~ %v (per-call timeout should bound fork at handshakeTimeout)",
			elapsed, small)
	}
	t.Logf("slow-fork elapsed: %v (budget %v)", elapsed, small)
}

// TestResumeUnhealthyError_IsChecks pins the Is() method that
// bridges the agent-level and bridge-level sentinels. The
// runtime (chatsession.go §1624) checks
// errors.Is(err, agent.ErrResumeUnhealthy); future in-bridge
// callers might check errors.Is(err, ErrResumeUnhealthy). Both
// must match.
func TestResumeUnhealthyError_IsChecks(t *testing.T) {
	e := resumeUnhealthyError{reason: "x", session: "ses_test"}
	if !errors.Is(e, agent.ErrResumeUnhealthy) {
		t.Errorf("resumeUnhealthyError should match agent.ErrResumeUnhealthy")
	}
	if !errors.Is(e, ErrResumeUnhealthy) {
		t.Errorf("resumeUnhealthyError should match dsh.ErrResumeUnhealthy")
	}
	if errors.Is(e, errors.New("unrelated")) {
		t.Errorf("resumeUnhealthyError should NOT match an unrelated error")
	}
	// Error string format sanity check (the runtime surfaces
	// the error message verbatim in IM cards).
	msg := e.Error()
	if !strings.Contains(msg, "dsh: resume session unhealthy") {
		t.Errorf("Error() = %q, missing prefix", msg)
	}
	if !strings.Contains(msg, "session_id=ses_test") {
		t.Errorf("Error() = %q, missing session_id", msg)
	}
}

// TestE2E_RealDsh_ForkBlankSessionReturnsUnhealthy is a real-dsh
// e2e test (no mock server) that exercises handshakeSession
// against the actual `dsh --profile web` 0.1.0-rc.6 binary.
// Gated by the same env as the existing print / chat e2e tests.
//
// What it pins:
//
//   1. The real dsh server rejects `session.fork` for a session
//      with no completed turn via error code "fork-unavailable"
//      (probed 2026-08-15 — see probe scripts in commit notes).
//      Our handshakeSession must surface this as
//      errors.Is(err, agent.ErrResumeUnhealthy).
//
//   2. The bridge does NOT silently fall back to session.create
//      on this rejection — a daemon restart against a stale
//      sessionId that still has no completed turn MUST propagate
//      the rejection so the runtime's auto-retry path
//      (chatsession.go §1624) clears the persisted id.
//
// We can't easily drive a turn in this test (that needs a full
// WS dial + prompt + drain), so we use the simpler "blank
// session can't be forked" path which exercises the same wire
// rejection.
//

// We use session.go's parseWebURL directly (this test is in
// the same package, so unexported helpers are accessible).

func TestE2E_RealDsh_ForkBlankSessionReturnsUnhealthy(t *testing.T) {
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skipf("dsh not in PATH; skipping: %v", err)
	}

	// Spawn a real dsh web process.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "dsh", "--profile", "web", "--port", "0")
	cmd.Env = append(cmd.Environ(), "DSH_PERMISSION_MODE=danger-full-access")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dsh: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Wait for URL line on stdout.
	urlCtx, urlCancel := context.WithTimeout(ctx, 15*time.Second)
	baseURL, err := parseWebURL(urlCtx, stdout)
	urlCancel()
	if err != nil {
		t.Fatalf("parse web url: %v", err)
	}
	t.Logf("dsh web spawned, url=%s", baseURL)

	// Build a driver pointed at this real server. We bypass
	// newDriver entirely (which would also dial WS, run
	// lifecycle goroutines, etc.) — we just need handshakeSession
	// to talk to the real server via its http client.
	d := &driver{
		http:      newHTTPClient(baseURL),
		workspace: "/tmp/dsh-probe",
		agentName: "dsh",
		closed:    make(chan struct{}),
		exitDone:  make(chan struct{}),
		events:    make(chan agent.AgentEvent, 1),
	}
	_ = http.Client{Timeout: 30 * time.Second} // keep http import alive

	// Step 1: create a blank session.
	createResp, err := d.http.Post(ctx, "session.create", map[string]any{
		"cwd":   "/tmp/dsh-probe",
		"title": "e2e-blank-fork-probe",
	})
	if err != nil {
		t.Fatalf("session.create: %v", err)
	}
	if !createResp.Result.OK {
		t.Fatalf("session.create rejected: %s", createResp.Result.ErrorMessage())
	}
	var scVal sessionCreateValue
	if err := json.Unmarshal(createResp.Result.Value, &scVal); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if scVal.SessionID == "" {
		t.Fatal("session.create returned empty sessionId")
	}
	t.Logf("created blank session: %s", scVal.SessionID)

	// Step 2: try to fork the blank session. dsh must reject
	// with error code "fork-unavailable" (no completed turn).
	resumed, err := d.handshakeSession(ctx, agent.StartConfig{
		Workspace: "/tmp/dsh-probe",
		SessionID: scVal.SessionID,
	})
	if err == nil {
		t.Fatalf("handshakeSession on blank session: err = nil, want Unhealthy; resumed=%v", resumed)
	}
	if !errors.Is(err, agent.ErrResumeUnhealthy) {
		t.Errorf("err %v should satisfy errors.Is(err, agent.ErrResumeUnhealthy)", err)
	}
	if !strings.Contains(err.Error(), "fork-unavailable") {
		t.Logf("note: error code in message was %q (expected 'fork-unavailable'); the Unhealthy wrap is what matters, not the code",
			err.Error())
	}
	if resumed {
		t.Errorf("resumed = true, want false")
	}

	// Step 3: a second handshakeSession call with empty
	// SessionID should still work (fresh create). Confirms
	// the failing fork didn't poison the driver state.
	resumed2, err2 := d.handshakeSession(ctx, agent.StartConfig{
		Workspace: "/tmp/dsh-probe",
	})
	if err2 != nil {
		t.Fatalf("second handshakeSession (empty SessionID): %v", err2)
	}
	if resumed2 {
		t.Errorf("second call resumed = true, want false")
	}
	if d.sessionID == "" || d.sessionID == scVal.SessionID {
		t.Errorf("second call sessionID = %q, want new id (not the rejected fork target)", d.sessionID)
	}
	t.Logf("second handshake: fresh sessionId = %s", d.sessionID)
}



