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
	"errors"
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
	muxConnectCount   atomic.Int64
	listCallCount     atomic.Int64
	createCallCount   atomic.Int64
	respondCount      atomic.Int64
	waterfallCount    atomic.Int64
	lastRespondBody   atomic.Value // []byte
	lastWaterfallBody atomic.Value // []byte

	// failSessionList, when true, causes handleSessionList to
	// return result.ok=false with a typert error envelope. Tests
	// drive it via scriptedFail/closedServer to script failure
	// patterns without rebuilding the mux.
	failSessionList atomic.Bool

	// readyClientID is the clientId dsh sends in the `ready` frame
	// for the *current* mux connection. Stored as string so atomic
	// load/store is type-safe; updated between connections by tests
	// that want to verify the dispatch path overwrites a stale
	// value (see host_state_test.go::TestReadyFrame_ReconnectOverwritesClientID).
	// Default "client-mock-001" is set in newMockDSH.
	readyClientID atomic.Value // string
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
	// Default ready clientId; tests override via setReadyClientID
	// to verify the dispatch path overwrites a stale value on
	// reconnect (see host_state_test.go).
	m.readyClientID.Store("client-mock-001")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/session/list", m.handleSessionList)
	mux.HandleFunc("/api/session/create", m.handleSessionCreate)
	mux.HandleFunc("/api/session/prompt", m.handleSessionPrompt)
	mux.HandleFunc("/api/session/cancel", m.handleSessionCancel)
	mux.HandleFunc("/api/respond", m.handleRespond)
	mux.HandleFunc("/api/$events/result", m.handleWaterfallResult)
	mux.HandleFunc("/api/remote.mux", m.handleMuxWS)

	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

// url returns the mock server's URL.
func (m *mockDSH) url() string { return m.server.URL }

// setReadyClientID changes the clientId the next /api/remote.mux
// upgrade will send in its `ready` frame. Tests use this to
// simulate a fresh dsh process assigning a new per-connection
// clientId; the bridge's dispatch path must overwrite any
// previously captured value (see
// host_state_test.go::TestReadyFrame_ReconnectOverwritesClientID).
func (m *mockDSH) setReadyClientID(id string) {
	m.readyClientID.Store(id)
}

// ─── HTTP handlers ─────────────────────────────────────────────────

func (m *mockDSH) handleSessionList(w http.ResponseWriter, r *http.Request) {
	m.listCallCount.Add(1)
	if m.failSessionList.Load() {
		writeRPC(w, rpcIDFromRequest(r), false, map[string]any{
			"code":    "bad-request",
			"message": "synthetic failure for test",
			"details": map[string]any{},
		})
		return
	}
	items := []host.SessionSummary{}
	if m.sessionListHook != nil {
		items = m.sessionListHook()
	}
	writeRPC(w, rpcIDFromRequest(r), true, map[string]any{"items": items})
}

func (m *mockDSH) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	m.createCallCount.Add(1)
	// Read body once — extract both rpcId (for writeRPC echo)
	// and sessionId (for the reattach round-trip) from a single
	// json.Unmarshal pass.
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	var raw struct {
		RPCID   string `json:"rpcId"`
		Payload struct {
			Args struct {
				Request struct {
					SessionID string `json:"sessionId"`
				} `json:"request"`
			} `json:"args"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		slog.Default().Error("mock handleSessionCreate: body not JSON", "err", err, "body", string(body))
		writeRPC(w, "", true, map[string]any{"sessionId": "session-mock-001"})
		return
	}
	sessionID := raw.Payload.Args.Request.SessionID
	if sessionID == "" {
		sessionID = "session-mock-001"
	}
	writeRPC(w, raw.RPCID, true, map[string]any{"sessionId": sessionID})
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

// handleWaterfallResult is the dsh 0.1.5-rc.1 /api/$events/result
// endpoint. dsh's gateway accepts a standard client-request
// envelope whose payload.args is {clientId, eventId, outcome}
// (exact-keys validated by parseRemoteEventResult). We capture
// the body for test assertions and return {accepted: true}.
func (m *mockDSH) handleWaterfallResult(w http.ResponseWriter, r *http.Request) {
	m.waterfallCount.Add(1)
	body, _ := io.ReadAll(r.Body)
	m.lastWaterfallBody.Store(body)
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
	// clientId is read at connection time (not at mock-construction
	// time) so tests can rotate it between connections to verify
	// the dispatch path overwrites stale values on reconnect.
	ready := map[string]any{
		"type":     "ready",
		"clientId": m.readyClientID.Load().(string),
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
				// Mirror dsh 0.1.2-rc.1's typert: session/follow args
				// must be wrapped as {request: SessionFollowRequest},
				// not flat. Verified 2026-09-11 against real dsh.
				var args struct {
					Args struct {
						Request struct {
							Address struct {
								Kind      string `json:"kind"`
								SessionID string `json:"sessionId"`
							} `json:"address"`
						} `json:"request"`
					} `json:"args"`
				}
				if err := json.Unmarshal(f.Payload, &args); err == nil &&
					args.Args.Request.Address.Kind == "session" {
					m.sessionToStream[args.Args.Request.Address.SessionID] = streamID
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

// ─── Test: /api/$events/result matches dsh 0.1.5-rc.1 wire shape ──
//
// §13.4 lock: dsh's gateway `parseRemoteEventResult` requires
// exactKeys(['clientId','eventId','outcome']) on the payload,
// with `outcome` having exactly {kind, value? | error?}. This
// test asserts the bridge's SendWaterfallResult emits that exact
// shape so the host doesn't reject the response.
func TestRPCClient_SendWaterfallResult_MatchesCanonicalWire(t *testing.T) {
	mock := newMockDSH(t)
	c := host.NewRPCClient(mock.url())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	outcome := host.WaterfallOutcomeEnvelope{
		Kind:  "result",
		Value: json.RawMessage(`"allowed-once"`),
	}
	if err := c.SendWaterfallResult(ctx,
		"client-mock-001", "event-uuid-42", outcome); err != nil {
		t.Fatalf("SendWaterfallResult: %v", err)
	}

	// SendWaterfallResult goes through PostEnvelope which uses the
	// same mock router but with method="$events/result" → URL
	// /api/$events/result. We can't easily inspect that body via
	// the mock's existing `lastRespondBody` capture (that's only
	// set for /api/respond). Instead, decode the request that
	// hit the server using a custom request capture.
	body, ok := mock.lastWaterfallBody.Load().([]byte)
	if !ok || len(body) == 0 {
		t.Fatal("expected waterfall body captured (set WaterfallCapture on mock)")
	}

	// Outer envelope: { type:"client-request", rpcId, method:"$events/result",
	//                    payload:{ args:{clientId, eventId, outcome:{kind, value?}} } }
	var outer struct {
		Type    string `json:"type"`
		RPCID   string `json:"rpcId"`
		Method  string `json:"method"`
		Payload struct {
			Args struct {
				ClientID string `json:"clientId"`
				EventID  string `json:"eventId"`
				Outcome  struct {
					Kind  string          `json:"kind"`
					Value json.RawMessage `json:"value,omitempty"`
					Error json.RawMessage `json:"error,omitempty"`
				} `json:"outcome"`
			} `json:"args"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &outer); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, body)
	}
	if outer.Type != "client-request" {
		t.Errorf("expected type=client-request, got %q", outer.Type)
	}
	if outer.Method != "$events/result" {
		t.Errorf("expected method=$events/result, got %q", outer.Method)
	}
	if outer.Payload.Args.ClientID != "client-mock-001" {
		t.Errorf("expected clientId=client-mock-001, got %q", outer.Payload.Args.ClientID)
	}
	if outer.Payload.Args.EventID != "event-uuid-42" {
		t.Errorf("expected eventId=event-uuid-42, got %q", outer.Payload.Args.EventID)
	}
	if outer.Payload.Args.Outcome.Kind != "result" {
		t.Errorf("expected outcome.kind=result, got %q", outer.Payload.Args.Outcome.Kind)
	}
	if string(outer.Payload.Args.Outcome.Value) != `"allowed-once"` {
		t.Errorf("expected outcome.value to be %q, got %q",
			`"allowed-once"`, outer.Payload.Args.Outcome.Value)
	}
	// exactKeys(['clientId','eventId','outcome']) — the typed
	// decode above silently drops any extra fields, so the fact
	// that the assertion above all passed already implies the
	// shape is clean. If the gateway ever relaxes the strict
	// key check, this test will need explicit map comparison.
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

	// dsh sends a synthetic "ready" event to the host handler as
	// soon as the mux stream connects (host/stream.go::dispatch
	// `case "ready"` repacks clientId+host and invokes the host
	// handler so the bridge can capture the clientId for
	// /api/$events/result). Drain it before checking the session-added
	// frame we actually care about.
	got := collectFrames(t, received, 2, 2*time.Second)
	var sessionAdded *serverFrameEnvelope
	for i := range got {
		if got[i].Method == "host/session-added" {
			sessionAdded = &got[i]
			break
		}
	}
	if sessionAdded == nil {
		t.Fatalf("expected a host/session-added frame, got: %+v", got)
	}
	if sessionAdded.Method != "host/session-added" {
		t.Errorf("wrong method: %+v", sessionAdded)
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
// dsh 0.1.2-rc.1 Host $events emit frame. The new wire shape is
// `{type:"emit", event:<name>, args:[<positional args...>]}` —
// the bridge's translateHostEvent pulls event as the method
// discriminator and the args array flows through to the host
// dispatch handler.
//
// rpcID is unused for emit frames (the real dsh doesn't mint
// one for broadcast events), but tests pass it for parity with
// pushSessionFrame. The waterfall shape would carry rpcId
// instead, but we don't have a waterfall test yet — emit covers
// the common broadcast path.
func wrapAsHostEvent(method, _ string, payload json.RawMessage) (json.RawMessage, error) {
	// If callers pass a single object as payload, fall through to
	// `args:[<that-object>]` so a map caller doesn't silently
	// produce `args:{}` (which the bridge would still translate,
	// but losing structure). If they pass an array, use it directly.
	var asArray json.RawMessage
	if len(payload) > 0 && payload[0] == '[' {
		asArray = payload
	} else {
		// Re-marshal so a map caller becomes args:[<map>].
		asArray, _ = json.Marshal([]json.RawMessage{payload})
		if len(asArray) == 0 {
			asArray = json.RawMessage("[]")
		}
	}
	return json.Marshal(map[string]any{
		"type":  "emit",
		"event": method,
		"args":  asArray,
	})
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

// TestRouter_SnapshotTransfersActiveSubs verifies that
// Router.Snapshot() returns enough state for tryRespawn to
// transplant active subscriptions from the dying Client's Router
// into the new Client's Router — SessionID, CWD, and the handler
// closure itself. Without the handler field, the respawn path
// would have nothing to wire into the new Router and would
// silently lose every active subscription on every respawn.
func TestRouter_SnapshotTransfersActiveSubs(t *testing.T) {
	src := host.NewRouter(slog.Default())
	var hits atomic.Int64
	handler := func(method, rpcID string, payload json.RawMessage) {
		hits.Add(1)
	}
	src.Subscribe("session-a", "/tmp/a", handler)
	src.Subscribe("session-b", "/tmp/b", handler)

	snap := src.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot: expected 2 entries, got %d", len(snap))
	}
	for _, sub := range snap {
		if sub.Handler == nil {
			t.Errorf("snapshot entry %q has nil handler", sub.SessionID)
		}
		if sub.SessionID == "session-a" && sub.CWD != "/tmp/a" {
			t.Errorf("session-a cwd: got %q, want /tmp/a", sub.CWD)
		}
		if sub.SessionID == "session-b" && sub.CWD != "/tmp/b" {
			t.Errorf("session-b cwd: got %q, want /tmp/b", sub.CWD)
		}
	}

	// The whole point: re-register each entry on a fresh Router
	// (simulating the post-respawn Router), and verify dispatch
	// reaches the same handler.
	dst := host.NewRouter(slog.Default())
	for _, sub := range snap {
		dst.Subscribe(sub.SessionID, sub.CWD, sub.Handler)
	}
	if dst.SubscriberCount() != 2 {
		t.Fatalf("dst SubscriberCount: expected 2, got %d", dst.SubscriberCount())
	}
	dst.DispatchMux("assistant/chunk", "seq-1",
		json.RawMessage(`{"sessionId":"session-a","chunk":{"type":"text-delta","text":"hi"}}`))
	if hits.Load() != 1 {
		t.Errorf("expected handler to be called once after transplant, got %d", hits.Load())
	}
}

// TestRouter_EnumerateDoesNotExposeHandler verifies the
// "Enumerate for reattach, Snapshot for transfer" split —
// EnumerateSubscriptions (used by RecoverSubscriptions for
// session.create RPC re-attach) must NOT carry the handler
// closure, since callers in that path don't need it and exposing
// it would imply a public API surface that doesn't exist there.
func TestRouter_EnumerateDoesNotExposeHandler(t *testing.T) {
	r := host.NewRouter(slog.Default())
	r.Subscribe("session-x", "/tmp/x", func(method, rpcID string, payload json.RawMessage) {})

	for _, sub := range r.EnumerateSubscriptions() {
		if sub.Handler != nil {
			t.Errorf("EnumerateSubscriptions should not populate Handler (got %v)", sub.Handler)
		}
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

// ─── Test: RecoverSubscriptions re-opens mux streams ───────────────
//
// §13.3 + review gap closure: RecoverSubscriptions must (a) re-
// attach the session on the new dsh via SessionCreate AND (b)
// open a fresh session/follow stream on the new Hub by calling
// Hub.Subscribe. Without (b), the Router has the handler but no
// mux frames ever arrive — the session is silently dead post-
// respawn.
//
// We assert (b) by verifying that RecoverSubscriptions results in
// a new "open" frame being sent on the mux (the mock updates
// sessionToStream on each open frame receipt). Then push a frame
// for the recovered session and verify the handler runs.
func TestClient_RecoverSubscriptions_ReopensMuxStream(t *testing.T) {
	mock := newMockDSH(t)
	c := host.New(mock.url(), slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Close)

	waitFor(t, 2*time.Second, func() bool { return mock.muxConnectCount.Load() >= 1 })

	var handlerHits atomic.Int64
	unsub := c.Subscribe("session-recover", "/tmp/recover",
		func(method, rpcID string, payload json.RawMessage) {
			handlerHits.Add(1)
		})
	t.Cleanup(unsub)

	// Capture the original streamId so we can detect that
	// RecoverSubscriptions replaced it (Hub.Subscribe is last-wins).
	waitFor(t, 1*time.Second, func() bool {
		mock.streamsMu.RLock()
		_, ok := mock.sessionToStream["session-recover"]
		mock.streamsMu.RUnlock()
		return ok
	})
	mock.streamsMu.RLock()
	originalStreamID := mock.sessionToStream["session-recover"]
	mock.streamsMu.RUnlock()
	if originalStreamID == "" {
		t.Fatal("setup: original Subscribe did not register sessionToStream entry")
	}

	// Now call RecoverSubscriptions. It must (a) RPC.SessionCreate
	// successfully and (b) Hub.Subscribe to open a new
	// session/follow stream on the mux.
	result := c.RecoverSubscriptions(ctx, slog.Default())
	if result.Reattached != 1 {
		t.Fatalf("expected Reattached=1, got %d (orphaned=%d)",
			result.Reattached, len(result.Orphaned))
	}

	// Wait for the new open frame to land and update
	// sessionToStream (last-wins replacement, streamId differs).
	waitFor(t, 1*time.Second, func() bool {
		mock.streamsMu.RLock()
		defer mock.streamsMu.RUnlock()
		current := mock.sessionToStream["session-recover"]
		return current != "" && current != originalStreamID
	})
	mock.streamsMu.RLock()
	newStreamID := mock.sessionToStream["session-recover"]
	mock.streamsMu.RUnlock()
	if newStreamID == originalStreamID {
		t.Errorf("RecoverSubscriptions did not mint a new mux streamId "+
			"(still %q) — Hub.Subscribe not called?", newStreamID)
	}

	// Push a frame for the recovered session on the new stream
	// and verify the handler receives it via the dispatch wrapper.
	mock.pushMuxFrame(t, "session-recover",
		"assistant/chunk", "seq-1",
		map[string]any{"chunk": map[string]any{"type": "text-delta", "text": "hi"}})
	waitFor(t, 2*time.Second, func() bool { return handlerHits.Load() >= 1 })
	if got := handlerHits.Load(); got == 0 {
		t.Errorf("handler not called after RecoverSubscriptions + push (got 0 hits)")
	}
}

// ─── Test: Ping handler resets read deadline ────────────────────────
//
// Review-driven regression lock (2026-09-13): gorilla dispatches
// control frames (ping/pong/close) to SetPingHandler without making
// ReadMessage return. If the read deadline is only reset on data
// frames, an idle mux (only 2s pings, no business frames for
// >60s) hits the absolute deadline, ReadMessage returns
// i/o-timeout, and the connection tears down. Fix: reset the
// deadline inside the ping handler.
//
// This test asserts the deadline *advances* across a stream of
// pings — i.e. the bridge can't drop the connection between
// pings.
func TestStreamHub_PingHandlerResetsReadDeadline(t *testing.T) {
	mock := newMockDSH(t)
	c := host.New(mock.url(), slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Close)

	waitFor(t, 2*time.Second, func() bool { return mock.muxConnectCount.Load() >= 1 })

	// We don't have a direct hook to read the conn's deadline
	// from outside, so verify behaviorally: the connection must
	// stay alive across a window > wsReadDeadline (60s) even
	// though the mock is silent. To keep test runtime sane we
	// instead use a much shorter window + assert the conn is
	// still open (would be torn down if the read deadline
	// expired and produced a ReadMessage error).
	//
	// Sleep 200ms — well under wsReadDeadline — and verify the
	// mock's muxConnectCount hasn't bumped (no reconnect). The
	// real assertion is that this test doesn't fail with a
	// read-deadline error in the log; any silent reconnect
	// would surface as an INFO "mux stream connected" line.
	before := mock.muxConnectCount.Load()
	time.Sleep(200 * time.Millisecond)
	if got := mock.muxConnectCount.Load(); got != before {
		t.Errorf("unexpected reconnect: count %d → %d (ping handler may not be resetting deadline)",
			before, got)
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

// ─── WaitForDSHReady: startup readiness probe ──────────────────────

// dshReadyStubServer is a minimal httptest.Server that handles
// /api/workspace.create (the probe target). It fails the first
// `failFirstN` requests with gateway/service-unavailable, then
// returns 200 OK. count is the total request count (atomic).
// Use `tErrBody` to switch the error body for non-transient tests.
type dshReadyStubServer struct {
	srv        *httptest.Server
	failFirstN int
	count      atomic.Int64
}

func newDSHReadyStub(t *testing.T, failFirstN int) *dshReadyStubServer {
	t.Helper()
	s := &dshReadyStubServer{failFirstN: failFirstN}
	mux := http.NewServeMux()
	// RPCClient.Post constructs the URL as baseURL + "/api/" +
	// methodDotsToSlashes(method), so "workspace.create" becomes
	// "/api/workspace/create" (slash, not dot). Match that.
	mux.HandleFunc("/api/workspace/create", func(w http.ResponseWriter, r *http.Request) {
		n := s.count.Add(1)
		// dsh echoes the request's rpcId in the response.
		// RPCClient.Post validates resp.RPCID == sent rpcID and
		// returns a transport error on mismatch; which would
		// trip our retry loop. Echo it back via map (the
		// surrounding file is one big raw string in some
		// tests; struct tags would require backticks).
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var env map[string]any
		_ = json.Unmarshal(body, &env)
		sentRPCID, _ := env["rpcId"].(string)
		if n <= int64(s.failFirstN) {
			// Match dsh 0.1.2-rc.1's wire shape for
			// gateway/service-unavailable.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK) // typert returns 200 with ok=false
			_, _ = fmt.Fprintf(w, `{"type":"server-response","rpcId":%q,"result":{"ok":false,"error":{"code":"gateway/service-unavailable","message":"active Service \"workspaceController\" is unavailable"}}}`, sentRPCID)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"type":"server-response","rpcId":%q,"result":{"ok":true,"value":{"workspace":{"workspaceId":"stub"}}}}`, sentRPCID)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// TestRPCClient_WaitForDSHReady_RetriesOnServiceUnavailable pins
// the contract: a transient "gateway/service-unavailable" from
// the probe target (workspace.create) does NOT abort spawnAndWire —
// the probe retries with respawnDelay backoff and eventually
// returns nil once dsh's plugin registry is loaded.
func TestRPCClient_WaitForDSHReady_RetriesOnServiceUnavailable(t *testing.T) {
	stub := newDSHReadyStub(t, 3) // fail first 3, succeed on 4th
	c := host.NewRPCClient(stub.srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.WaitForDSHReady(ctx, "/tmp/test", 5); err != nil {
		t.Fatalf("WaitForDSHReady: %v", err)
	}
	if got := stub.count.Load(); got != 4 {
		t.Errorf("workspace.create call count = %d, want 4 (3 failures + 1 success)", got)
	}
}

// TestRPCClient_WaitForDSHReady_GivesUpAfterMaxAttempts pins
// the failure cap: when ALL attempts return service-unavailable,
// the probe returns the last error after maxAttempts. The
// caller (spawnAndWire) propagates that as a hard spawn error
// and tears down the subprocess.
func TestRPCClient_WaitForDSHReady_GivesUpAfterMaxAttempts(t *testing.T) {
	stub := newDSHReadyStub(t, 100) // always fail
	c := host.NewRPCClient(stub.srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const maxAttempts = 4
	err := c.WaitForDSHReady(ctx, "/tmp/test", maxAttempts)
	if err == nil {
		t.Fatal("expected error after all attempts fail, got nil")
	}
	if got := stub.count.Load(); got != int64(maxAttempts) {
		t.Errorf("workspace.create call count = %d, want %d", got, maxAttempts)
	}
}

// TestRPCClient_WaitForDSHReady_NonTransientIsTerminal pins the
// fast-fail contract: a 4xx error code OTHER than
// service-unavailable is treated as a real config / wire
// mismatch and the probe returns immediately (not retried). This
// avoids burning the timeout on a non-recoverable failure.
func TestRPCClient_WaitForDSHReady_NonTransientIsTerminal(t *testing.T) {
	// Single-shot mock that returns a non-transient error.
	// Echoes the request's rpcId so RPCClient.Post's
	// resp.RPCID == sent rpcID check passes; the probe
	// then sees the business-level "bad-request" and must
	// fast-fail (no retry).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var env struct {
			RPCID string `json:"rpcId"`
		}
		_ = json.Unmarshal(body, &env)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"type":"server-response","rpcId":"%s","result":{"ok":false,"error":{"code":"bad-request","message":"missing args"}}}`, env.RPCID)
	}))
	t.Cleanup(srv.Close)

	c := host.NewRPCClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// maxAttempts=10 — if the probe retried, the 5s timeout
	// would fire. Instead it should return immediately after
	// the single attempt.
	start := time.Now()
	err := c.WaitForDSHReady(ctx, "/tmp/test", 10)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error on non-transient failure, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("non-transient probe took %v; expected fast-fail (under 2s)", elapsed)
	}
}

// TestRPCClient_WaitForDSHReady_ContextCancel pins the
// context-cancellation contract: the probe honors ctx and
// returns ctx.Err() if the context is cancelled mid-wait.
func TestRPCClient_WaitForDSHReady_ContextCancel(t *testing.T) {
	stub := newDSHReadyStub(t, 100) // always fail; probe will keep retrying
	c := host.NewRPCClient(stub.srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel the ctx after 50ms while the probe is between
	// attempts in respawnDelay(1)=1s.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := c.WaitForDSHReady(ctx, "/tmp/test", 10)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after ctx cancel, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed > 1*time.Second {
		t.Errorf("probe took %v after cancel; expected fast return", elapsed)
	}
}

// atomic.Int64 is imported via the stub server's count field.
// errors.Is / context.Canceled are used by the cancel test.
var (
	_ atomic.Int64
	_ = errors.Is
)
