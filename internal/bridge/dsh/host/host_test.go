// host_test.go — integration tests for the multiplexed Client.
//
// We use httptest.Server + gorilla/websocket.Upgrader to simulate
// a dsh web server. Tests do NOT depend on a real dsh binary on PATH
// and do NOT use requireBinarySkip — they run on any host with a
// working Go test runner.
//
// Coverage:
//   - RPC envelope round-trip (clientRequest → serverResponse)
//   - /api/respond special envelope (client-response, NOT client-request)
//   - mux WS subscribe + push + dispatch by sessionId
//   - host WS push + handler dispatch
//   - pending approval register/answer/drop
//   - Subscribe/Unsubscribe lifecycle
//   - reconnect after server-side close (smoke)

package host_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

// ─── mock dsh server ───────────────────────────────────────────────

// mockDSH is an httptest.Server-backed fake dsh web. It exposes
// the same surface (RPC + mux + host WS) and a tiny control
// channel (`pushMux` / `pushHost`) so tests can inject frames.
type mockDSH struct {
	server *httptest.Server

	// sessionListHook lets tests override the session.list response.
	// Default returns an empty items array.
	sessionListHook func() []host.SessionSummary

	// pushMux / pushHost are channels that mux-pump / host-pump
	// goroutines read from and forward to the active WS client.
	// Tests send serverFrame JSON to inject frames.
	pushMux  chan serverFrameEnvelope
	pushHost chan serverFrameEnvelope

	// counters (atomic) for assertions
	muxConnectCount  atomic.Int64
	hostConnectCount atomic.Int64
	listCallCount    atomic.Int64
	createCallCount  atomic.Int64
	respondCount     atomic.Int64
	lastRespondBody  atomic.Value // []byte
}

type serverFrameEnvelope struct {
	Method  string          `json:"method"`
	RPCID   string          `json:"rpcId"`
	Payload json.RawMessage `json:"payload"`
}

// newMockDSH spins up an httptest.Server wired to look like dsh web.
func newMockDSH(t *testing.T) *mockDSH {
	t.Helper()
	m := &mockDSH{
		pushMux:  make(chan serverFrameEnvelope, 64),
		pushHost: make(chan serverFrameEnvelope, 64),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/session.list", m.handleSessionList)
	mux.HandleFunc("/api/session.create", m.handleSessionCreate)
	mux.HandleFunc("/api/session.prompt", m.handleSessionPrompt)
	mux.HandleFunc("/api/session.cancel", m.handleSessionCancel)
	mux.HandleFunc("/api/session.fork", m.handleSessionFork)
	mux.HandleFunc("/api/respond", m.handleRespond)
	mux.HandleFunc("/api/events.mux", m.handleMuxWS)
	mux.HandleFunc("/api/events.host", m.handleHostWS)

	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

// url returns the mock server's URL.
func (m *mockDSH) url() string { return m.server.URL }

// ─── HTTP handlers ─────────────────────────────────────────────────

func (m *mockDSH) handleSessionList(w http.ResponseWriter, r *http.Request) {
	m.listCallCount.Add(1)
	items := []host.SessionSummary{}
	if m.sessionListHook != nil {
		items = m.sessionListHook()
	}
	writeRPC(w, rpcIDFromRequest(r), true, map[string]any{"items": items})
}

func (m *mockDSH) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	m.createCallCount.Add(1)
	writeRPC(w, rpcIDFromRequest(r), true, map[string]any{"sessionId": "session-mock-001"})
}

func (m *mockDSH) handleSessionPrompt(w http.ResponseWriter, r *http.Request) {
	writeRPC(w, rpcIDFromRequest(r), true, map[string]any{"accepted": true})
}

func (m *mockDSH) handleSessionCancel(w http.ResponseWriter, r *http.Request) {
	writeRPC(w, rpcIDFromRequest(r), true, map[string]any{"accepted": true})
}

func (m *mockDSH) handleSessionFork(w http.ResponseWriter, r *http.Request) {
	writeRPC(w, rpcIDFromRequest(r), true, map[string]any{"sessionId": "session-mock-fork-001"})
}

// handleRespond validates that the inbound body is a client-response
// envelope (NOT client-request), records the body for assertions,
// and writes the {accepted:true} receipt per dsh-api.md §2.12.
func (m *mockDSH) handleRespond(w http.ResponseWriter, r *http.Request) {
	m.respondCount.Add(1)
	body, _ := io.ReadAll(r.Body)
	m.lastRespondBody.Store(body)
	var env struct {
		Type   string `json:"type"`
		RPCID  string `json:"rpcId"`
		Result struct {
			OK    bool            `json:"ok"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "bad envelope: "+err.Error(), http.StatusBadRequest)
		return
	}
	if env.Type != "client-response" {
		http.Error(w, fmt.Sprintf("expected type:client-response, got %q", env.Type), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true})
}

// ─── WS handlers ───────────────────────────────────────────────────

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (m *mockDSH) handleMuxWS(w http.ResponseWriter, r *http.Request) {
	m.muxConnectCount.Add(1)
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	go m.pumpFrames(conn, m.pushMux)
}

func (m *mockDSH) handleHostWS(w http.ResponseWriter, r *http.Request) {
	m.hostConnectCount.Add(1)
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	go m.pumpFrames(conn, m.pushHost)
}

// pumpFrames forwards frames from the push channel to the WS client.
// Closes when push channel is closed or the client disconnects.
func (m *mockDSH) pumpFrames(conn *websocket.Conn, src <-chan serverFrameEnvelope) {
	defer conn.Close()
	for {
		select {
		case f, ok := <-src:
			if !ok {
				return
			}
			frame := map[string]any{
				"type":    "server-request",
				"rpcId":   f.RPCID,
				"method":  f.Method,
				"payload": json.RawMessage(f.Payload),
			}
			if err := conn.WriteJSON(frame); err != nil {
				return
			}
		case <-time.After(60 * time.Second):
			// No-op timeout so the goroutine eventually exits if
			// tests forget to drain the channel. Tests that need
			// longer lifetimes can replace this.
			return
		}
	}
}

// pushMuxFrame injects one server-request frame to the next mux
// subscriber. Buffered chan (cap 64) means tests don't need to
// coordinate pump goroutines — just send and proceed.
func (m *mockDSH) pushMuxFrame(t *testing.T, method, rpcID string, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("mock: marshal payload: %v", err)
	}
	select {
	case m.pushMux <- serverFrameEnvelope{Method: method, RPCID: rpcID, Payload: raw}:
	case <-time.After(2 * time.Second):
		t.Fatalf("mock: pushMuxFrame channel full or no reader")
	}
}

// ─── HTTP envelope helpers ─────────────────────────────────────────

// writeRPC writes a server-response envelope with the given value
// payload. `ok=true` returns {ok, value}; `ok=false` returns
// {error: {code, message, details}}. The rpcId is echoed from the
// inbound request (per the wire contract — server uses a parallel
// id map keyed on rpcId, mismatch means stale response).
func writeRPC(w http.ResponseWriter, rpcID string, ok bool, value any) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"type":   "server-response",
		"rpcId":  rpcID,
		"result": map[string]any{"ok": ok, "value": value},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// rpcIDFromRequest extracts the rpcId from the inbound clientRequest
// envelope. Returns "" if the body can't be decoded — the response
// will then have rpcId="", which the client treats as a mismatch
// (tests use this as a way to verify the validator catches it).
func rpcIDFromRequest(r *http.Request) string {
	var req struct {
		Type   string `json:"type"`
		RPCID  string `json:"rpcId"`
		Method string `json:"method"`
	}
	// Read body up to a reasonable cap; if it fails, return "".
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	_ = r.Body.Close()
	_ = json.Unmarshal(body, &req)
	return req.RPCID
}

// ─── Test: RPC envelope round-trip ─────────────────────────────────

func TestRPCClient_SessionList(t *testing.T) {
	mock := newMockDSH(t)
	mock.sessionListHook = func() []host.SessionSummary {
		return []host.SessionSummary{
			{SessionID: "session-1", UpdatedAt: 1700000000000, Blank: false, Running: false},
			{SessionID: "session-2", UpdatedAt: 1700000001000, Blank: true, Running: false},
		}
	}

	c := host.NewRPCClient(mock.url())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := c.SessionList(ctx)
	if err != nil {
		t.Fatalf("SessionList: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(items))
	}
	if items[0].SessionID != "session-1" || items[1].SessionID != "session-2" {
		t.Errorf("session ids wrong: %+v", items)
	}
	if mock.listCallCount.Load() != 1 {
		t.Errorf("expected 1 list call, got %d", mock.listCallCount.Load())
	}
}

func TestRPCClient_SessionCreate(t *testing.T) {
	mock := newMockDSH(t)
	c := host.NewRPCClient(mock.url())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	id, err := c.SessionCreate(ctx, host.SessionCreateOpts{CWD: "/tmp/chat"})
	if err != nil {
		t.Fatalf("SessionCreate: %v", err)
	}
	if id != "session-mock-001" {
		t.Errorf("expected session-mock-001, got %q", id)
	}
	if mock.createCallCount.Load() != 1 {
		t.Errorf("expected 1 create call, got %d", mock.createCallCount.Load())
	}
}

// ─── Test: /api/respond uses client-response envelope ──────────────

func TestRPCClient_Respond_UsesClientResponseEnvelope(t *testing.T) {
	mock := newMockDSH(t)
	c := host.NewRPCClient(mock.url())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	value := host.ApprovalResponse{
		SessionID:  "session-x",
		ApprovalID: "approval-y",
		Outcome:    "allowed-once",
	}
	if err := c.Respond(ctx, "stable-rpc-id-42", value); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if mock.respondCount.Load() != 1 {
		t.Fatalf("expected 1 respond call, got %d", mock.respondCount.Load())
	}

	body, _ := mock.lastRespondBody.Load().([]byte)
	if len(body) == 0 {
		t.Fatal("expected respond body captured")
	}
	var env struct {
		Type   string `json:"type"`
		RPCID  string `json:"rpcId"`
		Result struct {
			OK    bool                       `json:"ok"`
			Value host.ApprovalResponse      `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode respond body: %v (body=%s)", err, body)
	}
	if env.Type != "client-response" {
		t.Errorf("expected type=client-response, got %q (BRIDGE BUG-shape)", env.Type)
	}
	if env.RPCID != "stable-rpc-id-42" {
		t.Errorf("expected rpcId echo, got %q", env.RPCID)
	}
	if !env.Result.OK {
		t.Error("expected result.ok=true")
	}
	if env.Result.Value.Outcome != "allowed-once" {
		t.Errorf("expected outcome=allowed-once, got %q", env.Result.Value.Outcome)
	}
	if env.Result.Value.SessionID != "session-x" {
		t.Errorf("expected sessionId=session-x, got %q", env.Result.Value.SessionID)
	}
}

// ─── Test: Client integration — subscribe + dispatch ───────────────

func TestClient_SubscribeAndDispatchBySessionID(t *testing.T) {
	mock := newMockDSH(t)

	c := host.New(mock.url(), slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Close)

	// Subscribe to a session.
	received := make(chan serverFrameEnvelope, 4)
	c.Subscribe("session-alpha", "/tmp/test", func(method, rpcID string, payload json.RawMessage) {
		select {
		case received <- serverFrameEnvelope{Method: method, RPCID: rpcID, Payload: payload}:
		default:
		}
	})

	// Wait for mux WS to connect on the mock side (pump goroutine
	// must be alive before we push, otherwise pushMuxFrame blocks).
	waitFor(t, 2*time.Second, func() bool {
		return mock.muxConnectCount.Load() > 0
	})

	// Inject a session/subscribed baseline frame.
	mock.pushMuxFrame(t, "session/subscribed", "rpc-sub-1", map[string]any{
		"sessionId": "session-alpha",
		"lastSeq":   42,
	})

	// Inject an approval/requested frame for the same session.
	mock.pushMuxFrame(t, "approval/requested", "rpc-app-2", map[string]any{
		"sessionId":  "session-alpha",
		"approvalId": "approval-7",
		"toolName":   "Bash",
		"reason":     "execute shell command",
	})

	// Both frames should arrive on the subscriber channel.
	got := collectFrames(t, received, 2, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(got))
	}
	if got[0].Method != "session/subscribed" || got[0].RPCID != "rpc-sub-1" {
		t.Errorf("frame 0 wrong: %+v", got[0])
	}
	if got[1].Method != "approval/requested" || got[1].RPCID != "rpc-app-2" {
		t.Errorf("frame 1 wrong: %+v", got[1])
	}
}

// ─── Test: Frames for unsubscribed sessions are dropped ────────────

func TestClient_DispatchDropsUnsubscribedSessions(t *testing.T) {
	mock := newMockDSH(t)

	c := host.New(mock.url(), slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Close)

	// Subscribe ONLY to session-alpha.
	received := make(chan serverFrameEnvelope, 4)
	c.Subscribe("session-alpha", "/tmp/test", func(method, rpcID string, payload json.RawMessage) {
		select {
		case received <- serverFrameEnvelope{Method: method, RPCID: rpcID, Payload: payload}:
		default:
		}
	})

	waitFor(t, 2*time.Second, func() bool {
		return mock.muxConnectCount.Load() > 0
	})

	// Push a frame for session-beta — should NOT be delivered to alpha's handler.
	mock.pushMuxFrame(t, "session/event", "rpc-evt", map[string]any{
		"sessionId": "session-beta",
		"event":     map[string]any{"type": "turn/end", "data": map[string]any{}},
	})

	// Push a frame for session-alpha — SHOULD be delivered.
	mock.pushMuxFrame(t, "session/event", "rpc-evt-2", map[string]any{
		"sessionId": "session-alpha",
		"event":     map[string]any{"type": "turn/end", "data": map[string]any{}},
	})

	got := collectFrames(t, received, 1, 1*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 frame (alpha only), got %d", len(got))
	}
	if got[0].RPCID != "rpc-evt-2" {
		t.Errorf("wrong frame delivered: %+v", got[0])
	}
}

// ─── Test: Host stream dispatch ────────────────────────────────────

func TestClient_HostStreamDispatch(t *testing.T) {
	mock := newMockDSH(t)

	c := host.New(mock.url(), slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Close)

	received := make(chan serverFrameEnvelope, 4)
	c.SetHostHandler(func(method, rpcID string, payload json.RawMessage) {
		select {
		case received <- serverFrameEnvelope{Method: method, RPCID: rpcID, Payload: payload}:
		default:
		}
	})

	waitFor(t, 2*time.Second, func() bool {
		return mock.hostConnectCount.Load() > 0
	})

	// Inject a host/session-added frame (no sessionId on host stream).
	mock.pushHostFrame(t, "host/session-added", "rpc-h-1", map[string]any{
		"sessionId": "session-gamma",
		"blank":     true,
	})

	got := collectFrames(t, received, 1, 2*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected 1 host frame, got %d", len(got))
	}
	if got[0].Method != "host/session-added" {
		t.Errorf("wrong method: %+v", got[0])
	}
}

func (m *mockDSH) pushHostFrame(t *testing.T, method, rpcID string, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("mock: marshal payload: %v", err)
	}
	select {
	case m.pushHost <- serverFrameEnvelope{Method: method, RPCID: rpcID, Payload: raw}:
	case <-time.After(2 * time.Second):
		t.Fatalf("mock: pushHostFrame channel full or no reader")
	}
}

// ─── Test: Pending approval register + answer ─────────────────────

func TestRouter_RegisterAndAnswerPending(t *testing.T) {
	r := host.NewRouter(slog.Default())

	ch := r.RegisterPendingApproval("session-x", "rpc-1")
	if r.PendingCount() != 1 {
		t.Fatalf("expected 1 pending, got %d", r.PendingCount())
	}

	// First answer fills the buffered slot (cap 1).
	if !r.AnswerPending("session-x", "rpc-1", "allowed-once") {
		t.Fatal("AnswerPending returned false on first call")
	}

	// Second answer without a receiver — channel is full, should fail.
	if r.AnswerPending("session-x", "rpc-1", "rejected") {
		t.Error("AnswerPending should return false when channel is full and no receiver")
	}

	// Receiver reads the first value.
	select {
	case got := <-ch:
		if got != "allowed-once" {
			t.Errorf("channel got %q, want allowed-once", got)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("channel receive timed out")
	}

	// Now channel is drained; third answer goes through (latest-wins).
	if !r.AnswerPending("session-x", "rpc-1", "rejected") {
		t.Error("AnswerPending should succeed after channel was drained")
	}
	select {
	case got := <-ch:
		if got != "rejected" {
			t.Errorf("channel got %q, want rejected", got)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("second channel receive timed out")
	}
}

func TestRouter_UnsubscribeDropsPending(t *testing.T) {
	r := host.NewRouter(slog.Default())

	_ = r.RegisterPendingApproval("session-x", "rpc-a")
	_ = r.RegisterPendingApproval("session-x", "rpc-b")
	_ = r.RegisterPendingApproval("session-y", "rpc-c") // different session

	if r.PendingCount() != 3 {
		t.Fatalf("setup: expected 3 pending, got %d", r.PendingCount())
	}

	r.Unsubscribe("session-x")

	if r.PendingCount() != 1 {
		t.Errorf("expected 1 pending after Unsubscribe (session-y), got %d", r.PendingCount())
	}
	if r.SubscriberCount() != 0 {
		t.Errorf("expected 0 subscribers, got %d", r.SubscriberCount())
	}
}

func TestRouter_DispatchNoSessionIDDropsFrame(t *testing.T) {
	r := host.NewRouter(slog.Default())
	called := atomic.Int64{}
	r.Subscribe("session-x", "/tmp/test", func(method, rpcID string, payload json.RawMessage) {
		called.Add(1)
	})

	// mux-frame-level approval/asked has no sessionId (per dsh/handle_mux.go:131)
	r.DispatchMux("approval/asked", "rpc-no-sid", json.RawMessage(`{"toolName":"Bash"}`))

	if called.Load() != 0 {
		t.Errorf("expected no dispatch for sessionId-less frame, got %d calls", called.Load())
	}
}

// ─── Test: Reconnect after server close ────────────────────────────

func TestClient_ReconnectAfterServerClose(t *testing.T) {
	mock := newMockDSH(t)
	c := host.New(mock.url(), slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Close)

	// Wait for first mux connect.
	waitFor(t, 2*time.Second, func() bool { return mock.muxConnectCount.Load() >= 1 })
	firstCount := mock.muxConnectCount.Load()

	// Close the mux WS by closing the push channel. pumpFrames
	// returns on the close, ending the WS connection from the
	// server side; the client's read pump sees the disconnect and
	// the reconnect loop tries again.
	close(mock.pushMux)

	// Wait for the reconnect (exponential backoff base is 1s).
	waitFor(t, 5*time.Second, func() bool {
		return mock.muxConnectCount.Load() > firstCount
	})

	if mock.muxConnectCount.Load() <= firstCount {
		t.Fatalf("expected reconnect (count > %d), got %d", firstCount, mock.muxConnectCount.Load())
	}
}

// ─── Test: Close is idempotent ─────────────────────────────────────

func TestClient_CloseIdempotent(t *testing.T) {
	mock := newMockDSH(t)
	c := host.New(mock.url(), slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	c.Close()
	c.Close() // should not panic, not deadlock
	c.Close()
}

// ─── helpers ───────────────────────────────────────────────────────

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitFor timed out after %s", timeout)
}

func collectFrames(t *testing.T, ch <-chan serverFrameEnvelope, n int, timeout time.Duration) []serverFrameEnvelope {
	t.Helper()
	out := make([]serverFrameEnvelope, 0, n)
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case f := <-ch:
			out = append(out, f)
		case <-deadline:
			return out
		}
	}
	return out
}

// ─── import-only sanity (prevent "unused import" drift) ──────────

var (
	_ = url.Parse
	_ = strings.Repeat
	_ = sync.Once{}
	_ io.Reader
)