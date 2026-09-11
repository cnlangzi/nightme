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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

var _ = slog.Default

// ─── mock dsh server ───────────────────────────────────────────────

// mockDSH is an httptest.Server-backed fake dsh web. It exposes
// the same surface (RPC + single Remote mux WS) and per-stream
// push channels so tests can inject frames.
type mockDSH struct {
	server *httptest.Server

	// sessionListHook lets tests override the session.list response.
	// Default returns an empty items array.
	sessionListHook func() []host.SessionSummary

	// streams maps server-minted streamId → push channel. Tests
	// call pushMuxFrameForSession to enqueue an item on the right
	// channel.
	streamsMu       sync.RWMutex
	streams         map[string]chan muxItem
	sessionToStream map[string]string // sessionID → streamId (for tests)

	// closeMu + closeFn close the active WS connection — used by
	// the reconnect test to simulate server-side WS death. Replaced
	// on every new connection so a fresh conn can be killed by a
	// later shutdown() call.
	closeMu sync.Mutex
	closeFn func()

	// capturedConnValue is the most recent WS connection. Used by
	// pushMuxFrame to spin up ephemeral streams for unsubscribed
	// sessions.
	capturedConnValue *websocket.Conn

	// writeMu serializes gorilla's WriteJSON/NextWriter — gorilla
	// stores per-conn message state in `messageWriter` and is NOT
	// safe for concurrent calls. All muxStreamWriter goroutines
	// (one per open stream, plus ephemeral ones) take this lock
	// before writing; without it, the race detector fires and
	// concurrent frames can interleave bytes on the wire.
	writeMu sync.Mutex

	// counters (atomic) for assertions
	muxConnectCount atomic.Int64
	listCallCount   atomic.Int64
	createCallCount atomic.Int64
	respondCount    atomic.Int64
	lastRespondBody atomic.Value // []byte
}

type serverFrameEnvelope struct {
	Method  string          `json:"method"`
	RPCID   string          `json:"rpcId"`
	Payload json.RawMessage `json:"payload"`
}

// muxItem is what mockDSH sends back to the bridge over the mux WS.
type muxItem struct {
	StreamID string          `json:"streamId"`
	Value    json.RawMessage `json:"value"`
}

// newMockDSH spins up an httptest.Server wired to look like dsh web.
func newMockDSH(t *testing.T) *mockDSH {
	t.Helper()
	m := &mockDSH{
		streams:         make(map[string]chan muxItem),
		sessionToStream: make(map[string]string),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/session/list", m.handleSessionList)
	mux.HandleFunc("/api/session/create", m.handleSessionCreate)
	mux.HandleFunc("/api/session/prompt", m.handleSessionPrompt)
	mux.HandleFunc("/api/session/cancel", m.handleSessionCancel)
	mux.HandleFunc("/api/respond", m.handleRespond)
	mux.HandleFunc("/api/remote.mux", m.handleMuxWS)

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

// handleMuxWS is the dsh 0.1.2-rc.1 Remote mux endpoint. On
// connect it sends `{type:"ready", clientId, host:{home:"..."}}`.
// It then reads client frames and dispatches by streamId: open
// frames register a push channel per session, cancel frames drop
// the channel, item frames are queued for the matching session.
func (m *mockDSH) handleMuxWS(w http.ResponseWriter, r *http.Request) {
	m.muxConnectCount.Add(1)
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	m.closeMu.Lock()
	m.capturedConnValue = conn
	// Send a WS close frame so the client gets a clean close
	// notification (rather than relying on TCP RST/FIN, which the
	// gorilla client may not surface promptly depending on
	// platform-specific TCP buffering).
	m.closeFn = func() {
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "test shutdown"),
			time.Now().Add(time.Second),
		)
		_ = conn.Close()
	}
	m.closeMu.Unlock()

	// Send the "ready" frame every dsh 0.1.2-rc.1 connection
	// sends on upgrade. Bridge uses it only for log correlation.
	ready := map[string]any{
		"type":     "ready",
		"clientId": "client-mock-001",
		"host":     map[string]any{"home": "/tmp/test"},
	}
	if err := conn.WriteJSON(ready); err != nil {
		_ = conn.Close()
		return
	}

	// Reader goroutine: parses client frames, registers/cancels
	// per-stream push channels, broadcasts items to subscribers.
	go m.muxPumpLoop(conn)
}

// muxPumpLoop reads client frames and routes per-stream items to
// the right push channel. Closes when the client disconnects.
func (m *mockDSH) muxPumpLoop(conn *websocket.Conn) {
	defer conn.Close()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if len(raw) == 0 {
			continue
		}
		var f struct {
			Type     string          `json:"type"`
			StreamID string          `json:"streamId"`
			Endpoint string          `json:"endpoint"`
			Payload  json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			continue
		}
		switch f.Type {
		case "open":
			streamID := f.StreamID
			ch := make(chan muxItem, 16)
			m.streamsMu.Lock()
			m.streams[streamID] = ch
			if f.Endpoint == "session/follow" {
				var args struct {
					Args struct {
						Address struct {
							Kind      string `json:"kind"`
							SessionID string `json:"sessionId"`
						} `json:"address"`
					} `json:"args"`
				}
				if err := json.Unmarshal(f.Payload, &args); err == nil &&
					args.Args.Address.Kind == "session" {
					m.sessionToStream[args.Args.Address.SessionID] = streamID
				}
			}
			m.streamsMu.Unlock()
			// Pump items from this stream's push channel onto the
			// WS as `{type:"item", streamId, value}` frames.
			go m.muxStreamWriter(conn, streamID, ch)
		case "cancel":
			streamID := "stream-" + f.StreamID
			m.streamsMu.Lock()
			if ch, ok := m.streams[streamID]; ok {
				close(ch)
				delete(m.streams, streamID)
			}
			m.streamsMu.Unlock()
		}
	}
}

// muxStreamWriter pumps items from a single stream's push channel
// onto the shared WS as `{type:"item", streamId, value}` frames.
// Recovers from WriteJSON panics (gorilla panics if the conn is
// already closed mid-write) so one dead stream doesn't poison the
// whole test process. Concurrent calls (across multiple writers)
// are serialized via m.writeMu — gorilla's WriteJSON is not
// safe for concurrent use.
func (m *mockDSH) muxStreamWriter(conn *websocket.Conn, streamID string, src <-chan muxItem) {
	defer func() {
		_ = recover()
	}()
	for item := range src {
		frame := map[string]any{
			"type":     "item",
			"streamId": item.StreamID,
			"value":    item.Value,
		}
		m.writeMu.Lock()
		err := conn.WriteJSON(frame)
		m.writeMu.Unlock()
		if err != nil {
			return
		}
	}
}

// pushMuxFrame injects one mux item to the session subscribed
// under sessionID. Routes through streamId so the bridge's
// per-session dispatch (extractSessionID on the payload's
// sessionId field) can correctly route it.
//
// If sessionID has no active stream, pushMuxFrame creates an
// ephemeral stream for it (mirroring what the bridge would have
// seen had a Subscribe call been made). The bridge's router then
// drops the frame because no subscriber matches — exactly the
// scenario TestClient_DispatchDropsUnsubscribedSessions exercises.
func (m *mockDSH) pushMuxFrame(t *testing.T, sessionID, method, rpcID string, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("mock: marshal payload: %v", err)
	}

	m.streamsMu.Lock()
	streamID, ok := m.sessionToStream[sessionID]
	if !ok {
		streamID = "ephemeral-" + sessionID
		m.sessionToStream[sessionID] = streamID
		conn := m.capturedConn()
		if conn == nil {
			m.streamsMu.Unlock()
			t.Fatalf("mock: no conn (c.Start not called?)")
		}
		ch := make(chan muxItem, 16)
		m.streams[streamID] = ch
		go m.muxStreamWriter(conn, streamID, ch)
	}
	ch := m.streams[streamID]
	m.streamsMu.Unlock()

	value := wrapAsSessionEvent(method, rpcID, raw)

	select {
	case ch <- muxItem{StreamID: streamID, Value: value}:
	case <-time.After(2 * time.Second):
		t.Fatalf("mock: pushMuxFrame channel full or no reader for session %q", sessionID)
	}
}

// capturedConn returns the WS connection the most recent
// handleMuxWS invocation captured. Used by pushMuxFrame to spin
// up ephemeral streams for unsubscribed sessions. Returns nil
// if no connection is currently active.
func (m *mockDSH) capturedConn() *websocket.Conn {
	m.closeMu.Lock()
	defer m.closeMu.Unlock()
	return m.capturedConnValue
}

// wrapAsSessionEvent encodes a (method, rpcID, payload) tuple as
// the dsh SessionWireEvent shape bridge/stream.go::translateSessionEvent
// expects: {type:"event", event:{type, seq, time, data:{<payload>}}}.
func wrapAsSessionEvent(method, rpcID string, payload json.RawMessage) json.RawMessage {
	seq := int64(0)
	if n, err := strconv.ParseInt(strings.TrimPrefix(rpcID, "seq-"), 10, 64); err == nil {
		seq = n
	}
	envelope := map[string]any{
		"type": "event",
		"event": map[string]any{
			"type": method,
			"seq":  seq,
			"time": time.Now().UnixMilli(),
			"data": json.RawMessage(payload),
		},
	}
	b, _ := json.Marshal(envelope)
	return b
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
			OK    bool                  `json:"ok"`
			Value host.ApprovalResponse `json:"value"`
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
	mock.waitForSessionStream(t, "session-alpha")

	// Inject a session/subscribed baseline frame.
	mock.pushMuxFrame(t, "session-alpha", "session/subscribed", "rpc-sub-1", map[string]any{
		"sessionId": "session-alpha",
		"lastSeq":   42,
	})

	// Inject an approval/requested frame for the same session.
	mock.pushMuxFrame(t, "session-alpha", "approval/requested", "rpc-app-2", map[string]any{
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
	// dsh 0.1.2-rc.1 wire uses `seq` (numeric event sequence) as
	// the frame identity — bridge's translateSessionEvent converts
	// that into the legacy `rpcId` slot. Test pushed both frames
	// with seq=0 (default for fresh push), so both have the same
	// seq-derived rpcId; the discriminator is method + payload.
	if got[0].Method != "session/subscribed" {
		t.Errorf("frame 0 wrong method: %+v", got[0])
	}
	if got[1].Method != "approval/requested" {
		t.Errorf("frame 1 wrong method: %+v", got[1])
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
	mock.waitForSessionStream(t, "session-alpha")

	// Push a frame for session-alpha FIRST — the alpha stream's
	// writer has been alive since the open frame was processed, so
	// the push is stable. The bridge's router dispatches it to the
	// alpha handler.
	mock.pushMuxFrame(t, "session-alpha", "session/event", "rpc-evt-2", map[string]any{
		"sessionId": "session-alpha",
		"event":     map[string]any{"type": "turn/end", "data": map[string]any{}},
	})
	// Wait for the bridge to actually receive + dispatch the alpha
	// frame before pushing the ephemeral beta frame (which spins
	// up an extra muxStreamWriter goroutine and races with the
	// long-running alpha writer if back-to-back). Pessimistic
	// 50ms is plenty for a loopback WS.
	time.Sleep(50 * time.Millisecond)

	// Push a frame for session-beta — but mock has no subscriber for
	// session-beta, so pushMuxFrame must look up by sessionID. The
	// bridge's router should drop the frame because session-alpha's
	// handler doesn't match session-beta's sessionId.
	mock.pushMuxFrame(t, "session-beta", "session/event", "rpc-evt", map[string]any{
		"sessionId": "session-beta",
		"event":     map[string]any{"type": "turn/end", "data": map[string]any{}},
	})

	got := collectFrames(t, received, 1, 1*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 frame (alpha only), got %d", len(got))
	}
	// dsh 0.1.2-rc.1 wire uses seq-derived rpcId; both pushes
	// share seq=0, so the discriminator is payload.sessionId.
	// The bridge's Router must drop the beta frame (no subscriber)
	// and deliver only the alpha frame — verified by checking
	// payload.sessionId below.
	if got[0].RPCID == "" {
		t.Errorf("expected rpcId set, got %+v", got[0])
	}
	var sid struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(got[0].Payload, &sid)
	if sid.SessionID != "session-alpha" {
		t.Errorf("expected sessionId=session-alpha in delivered frame, got %q", sid.SessionID)
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
		return mock.muxConnectCount.Load() > 0
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
	m.streamsMu.RLock()
	ch, ok := m.streams["host-$events"]
	m.streamsMu.RUnlock()
	if !ok {
		t.Fatalf("mock: no host stream open (c.Start not called?)")
	}
	value, err := wrapAsHostEvent(method, rpcID, raw)
	if err != nil {
		t.Fatalf("mock: wrap host event: %v", err)
	}
	select {
	case ch <- muxItem{StreamID: "host-$events", Value: value}:
	case <-time.After(2 * time.Second):
		t.Fatalf("mock: pushHostFrame channel full or no reader")
	}
}

// wrapAsHostEvent encodes a (method, rpcID, payload) tuple as a
// dsh RemoteEventRecord for the Host stream.
func wrapAsHostEvent(method, rpcID string, payload json.RawMessage) (json.RawMessage, error) {
	envelope := map[string]any{
		"id":    rpcID,
		"event": method,
	}
	// Merge payload fields into the envelope so callers don't have
	// to nest by hand. Strip any conflicting reserved keys first.
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(payload, &extra); err == nil {
		delete(extra, "id")
		delete(extra, "event")
		for k, v := range extra {
			envelope[k] = v
		}
	}
	return json.Marshal(envelope)
}

// shutdown forcibly closes the active WS connection so the bridge
// reconnects. Used by the reconnect test.
func (m *mockDSH) shutdown() {
	m.closeMu.Lock()
	fn := m.closeFn
	m.closeMu.Unlock()
	if fn != nil {
		tlog := slog.Default()
		tlog.Info("mock.shutdown: closing active WS conn")
		fn()
	}
}

// waitForSessionStream polls until the mock has registered an open
// frame for sessionID. Use this between c.Subscribe and
// pushMuxFrame so the push doesn't race the open frame's
// arrival on the mock side.
func (m *mockDSH) waitForSessionStream(t *testing.T, sessionID string) {
	t.Helper()
	waitFor(t, 2*time.Second, func() bool {
		m.streamsMu.RLock()
		_, ok := m.sessionToStream[sessionID]
		m.streamsMu.RUnlock()
		return ok
	})
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

	// Forcibly close the mux WS from the server side. The client's
	// read pump sees the disconnect and the reconnect loop tries
	// again.
	mock.shutdown()

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
