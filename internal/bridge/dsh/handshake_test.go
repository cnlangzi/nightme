package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

// handshakeMock is a minimal dsh HTTP surface for handshake / Reset
// tests. It does not speak WebSocket — those paths are covered by
// host/host_test.go.
type handshakeMock struct {
	server *httptest.Server

	createCount    atomic.Int64
	workspaceCount atomic.Int64
	historyCount   atomic.Int64
	cancelCount    atomic.Int64
	archiveCount   atomic.Int64 // Close calls workspace.archiveSession (repo-scoped workspace survives)
	promptCount    atomic.Int64
	promptFailNext atomic.Bool

	mu               sync.Mutex
	lastCreate       map[string]any
	createIDs        []string
	conflictOnID     string
	mismatchAttach   bool
	historyEvents    []map[string]any
	nextFreshCounter atomic.Int64
	lastPrompt       atomic.Value // map[string]any

	respondText atomic.Value // string — when set, prompt handler synthesises a complete turn
}

func newHandshakeMock(t *testing.T) *handshakeMock {
	t.Helper()
	m := &handshakeMock{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/workspace.create", m.handleWorkspaceCreate)
	mux.HandleFunc("/api/session.create", m.handleSessionCreate)
	mux.HandleFunc("/api/session.models", m.handleSessionModels)
	mux.HandleFunc("/api/session.history", m.handleSessionHistory)
	mux.HandleFunc("/api/session.cancel", m.handleSessionCancel)
	mux.HandleFunc("/api/workspace.archiveSession", m.handleWorkspaceArchiveSession)
	mux.HandleFunc("/api/session.prompt", m.handleSessionPrompt)
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

func (m *handshakeMock) installGlobal(t *testing.T) *host.Client {
	t.Helper()
	cli := host.New(m.server.URL, nil)
	host.UnsetGlobal()
	host.SetGlobal(cli)
	t.Cleanup(func() {
		cli.Close()
		host.UnsetGlobal()
	})
	return cli
}

// setRespondText toggles the prompt handler's mux-frame synthesis.
// Empty string means "just return OK" (drainForRunResult will
// block on events — caller is responsible for ctx timeout). When
// non-empty, the prompt handler dispatches an assistant/message +
// turn/end pair via the host Router so drainForRunResult sees a
// complete turn and returns cleanly.
func (m *handshakeMock) setRespondText(text string) {
	m.respondText.Store(text)
}

type rpcEnvelope struct {
	Type    string          `json:"type"`
	RPCID   string          `json:"rpcId"`
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload"`
}

func decodeEnvelope(r *http.Request) rpcEnvelope {
	var env rpcEnvelope
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
	_ = json.Unmarshal(body, &env)
	return env
}

func writeOK(w http.ResponseWriter, rpcID string, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "server-response",
		"rpcId": rpcID,
		"result": map[string]any{
			"ok":    true,
			"value": value,
		},
	})
}

func writeErr(w http.ResponseWriter, rpcID, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "server-response",
		"rpcId": rpcID,
		"result": map[string]any{
			"ok": false,
			"error": map[string]any{
				"code":    code,
				"message": msg,
			},
		},
	})
}

// handleWorkspaceCreate mirrors the dsh wire contract:
// `{workspace, created: bool}` (dsh-api.md §2.4.2). The mock
// returns created=true by default; tests that need to exercise
// the dedup-hit path (createFreshSession found an existing
// workspace for the same path) flip m.dedupWorkspace to true.
// handleWorkspaceCreate mirrors the dsh wire contract:
// `{workspace, created: bool}` (dsh-api.md §2.4.2). The `created`
// boolean is logged but otherwise ignored — workspace ownership
// no longer tracked at the bridge level (commit 5a6bee0 reverted
// to repo-scoped workspaces that survive across drivers).
// Each call returns a fresh workspaceId ("ws-mock-1", ...) so
// Reset / multi-RunOnce tests can distinguish workspaces.
func (m *handshakeMock) handleWorkspaceCreate(w http.ResponseWriter, r *http.Request) {
	m.workspaceCount.Add(1)
	env := decodeEnvelope(r)
	idx := m.workspaceCount.Load() - 1
	wsID := fmt.Sprintf("ws-mock-%d", idx+1)
	writeOK(w, env.RPCID, map[string]any{
		"workspace": map[string]any{
			"workspaceId": wsID,
			"path":        "/tmp/ws",
			"title":       "ws",
		},
		"created": true,
	})
}

func (m *handshakeMock) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	m.createCount.Add(1)
	env := decodeEnvelope(r)
	var payload map[string]any
	_ = json.Unmarshal(env.Payload, &payload)

	m.mu.Lock()
	m.lastCreate = payload
	m.mu.Unlock()

	if sid, _ := payload["sessionId"].(string); sid != "" {
		if m.conflictOnID != "" && sid == m.conflictOnID {
			writeErr(w, env.RPCID, "session-conflict", "cwd mismatch")
			return
		}
		if m.mismatchAttach {
			writeOK(w, env.RPCID, map[string]any{"sessionId": "session-other"})
			return
		}
		m.mu.Lock()
		m.createIDs = append(m.createIDs, sid)
		m.mu.Unlock()
		writeOK(w, env.RPCID, map[string]any{"sessionId": sid})
		return
	}

	n := m.nextFreshCounter.Add(1)
	id := fmt.Sprintf("session-fresh-%d", n)
	m.mu.Lock()
	m.createIDs = append(m.createIDs, id)
	m.mu.Unlock()
	writeOK(w, env.RPCID, map[string]any{"sessionId": id})
}

func (m *handshakeMock) handleSessionModels(w http.ResponseWriter, r *http.Request) {
	env := decodeEnvelope(r)
	writeOK(w, env.RPCID, map[string]any{
		"current":  map[string]any{"provider": "mock", "model": "mock-model"},
		"routable": true,
	})
}

func (m *handshakeMock) handleSessionHistory(w http.ResponseWriter, r *http.Request) {
	m.historyCount.Add(1)
	env := decodeEnvelope(r)
	m.mu.Lock()
	events := m.historyEvents
	m.mu.Unlock()
	if events == nil {
		events = []map[string]any{}
	}
	writeOK(w, env.RPCID, map[string]any{"events": events})
}

func (m *handshakeMock) handleSessionCancel(w http.ResponseWriter, r *http.Request) {
	m.cancelCount.Add(1)
	env := decodeEnvelope(r)
	writeOK(w, env.RPCID, map[string]any{"accepted": true})
}

func (m *handshakeMock) handleWorkspaceArchiveSession(w http.ResponseWriter, r *http.Request) {
	m.archiveCount.Add(1)
	env := decodeEnvelope(r)
	var payload struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(env.Payload, &payload)
	writeOK(w, env.RPCID, map[string]any{
		"archivedSessionIds": []string{payload.SessionID},
	})
}



// dispatchAssistantMessage emits a synthetic assistant/message
// mux frame so the driver's readPump pushes EventAgentText into
// d.events. Tests that exercise drainForRunResult's full turn path
// use this.
func dispatchAssistantMessage(r *host.Router, sessionID, text string) {
	payload := []byte(`{"sessionId":"` + sessionID + `","event":{"type":"assistant/message","data":{"message":{"role":"assistant","content":[{"type":"text","text":` + jsonString(text) + `}]}}}}`)
	r.DispatchMux("session/event", "rpc-am-"+sessionID, payload)
}

// dispatchTurnEnd emits a synthetic turn/end mux frame so the
// driver's readPump pushes EventAgentResult + EventAgentDone.
func dispatchTurnEnd(r *host.Router, sessionID, stopReason string) {
	payload := []byte(`{"sessionId":"` + sessionID + `","event":{"type":"turn/end","data":{"stopReason":"` + stopReason + `"}}}`)
	r.DispatchMux("session/event", "rpc-te-"+sessionID, payload)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// handleSessionPrompt returns OK and optionally synthesises a
// complete turn via the host Router when respondText is set on
// the mock. Tests that need drainForRunResult to exit cleanly call
// m.setRespondText(...) before invoking Starter.RunOnce / SendBlocks.
func (m *handshakeMock) handleSessionPrompt(w http.ResponseWriter, r *http.Request) {
	m.promptCount.Add(1)
	env := decodeEnvelope(r)
	var payload map[string]any
	_ = json.Unmarshal(env.Payload, &payload)
	m.lastPrompt.Store(payload)

	if m.promptFailNext.Load() {
		m.promptFailNext.Store(false)
		writeErr(w, env.RPCID, "bad-request", "synthetic failure for test")
		return
	}

	// If respondText is set, dispatch synthetic mux frames so
	// drainForRunResult sees a complete turn and returns.
	if text, ok := m.respondText.Load().(string); ok && text != "" {
		sid, _ := payload["sessionId"].(string)
		if sid != "" {
			if router := host.GetGlobal().Router; router != nil {
				dispatchAssistantMessage(router, sid, text)
				dispatchTurnEnd(router, sid, "stop")
			}
		}
	}

	writeOK(w, env.RPCID, map[string]any{"accepted": true})
}

func (m *handshakeMock) lastCreateCopy() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]any, len(m.lastCreate))
	for k, v := range m.lastCreate {
		out[k] = v
	}
	return out
}

func (m *handshakeMock) createdIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.createIDs...)
}

func newTestDriver(cli *host.Client, workspace string) *driver {
	d := &driver{
		cli:              cli,
		workspace:        workspace,
		agentName:        "dsh",
		pendingApprovals: map[string]chan string{},
		pendingQuestions: map[string][]questionPayload{},
		lastApprovalID:   map[string]string{},
		events:           make(chan agent.AgentEvent, 64),
		translate:        newTranslator("dsh", workspace),
		wireState:        newWireState(),
		closed:           make(chan struct{}),
		lastSeq:          -1,
	}
	d.dispatcher = newDispatcher(d.translate, d.wireState, d, d.deliver)
	return d
}

func TestHandshakeSession_ResumeAttachesViaSessionCreate(t *testing.T) {
	mock := newHandshakeMock(t)
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/feat-review")

	wantID := "session-2fc75979-6cd7-44bc-a0bf-b9680f5ce5c0"
	resumed, err := d.handshakeSession(context.Background(), agent.StartConfig{
		SessionID: wantID,
		Workspace: "/tmp/feat-review",
	})
	if err != nil {
		t.Fatalf("handshakeSession: %v", err)
	}
	if !resumed {
		t.Fatal("want resumed=true")
	}
	if d.sessionID != wantID {
		t.Fatalf("sessionID = %q, want %q", d.sessionID, wantID)
	}
	if mock.createCount.Load() != 1 {
		t.Fatalf("session.create calls = %d, want 1", mock.createCount.Load())
	}
	if mock.workspaceCount.Load() != 0 {
		t.Fatalf("workspace.create calls = %d, want 0 on resume attach", mock.workspaceCount.Load())
	}
	got := mock.lastCreateCopy()
	if got["sessionId"] != wantID {
		t.Errorf("create payload sessionId = %v, want %s", got["sessionId"], wantID)
	}
	if got["cwd"] != "/tmp/feat-review" {
		t.Errorf("create payload cwd = %v, want /tmp/feat-review", got["cwd"])
	}
}

func TestHandshakeSession_ResumeConflictIsUnhealthy(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.conflictOnID = "session-stale"
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/ws")

	_, err := d.handshakeSession(context.Background(), agent.StartConfig{
		SessionID: "session-stale",
		Workspace: "/tmp/ws",
	})
	if err == nil {
		t.Fatal("want ErrResumeUnhealthy, got nil")
	}
	if !errors.Is(err, ErrResumeUnhealthy) {
		t.Errorf("errors.Is(err, ErrResumeUnhealthy) = false; err=%v", err)
	}
	if !errors.Is(err, agent.ErrResumeUnhealthy) {
		t.Errorf("errors.Is(err, agent.ErrResumeUnhealthy) = false; err=%v", err)
	}
}

func TestHandshakeSession_ResumeMismatchedIDIsUnhealthy(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.mismatchAttach = true
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/ws")

	_, err := d.handshakeSession(context.Background(), agent.StartConfig{
		SessionID: "session-want",
		Workspace: "/tmp/ws",
	})
	if !errors.Is(err, agent.ErrResumeUnhealthy) {
		t.Fatalf("err = %v, want agent.ErrResumeUnhealthy", err)
	}
}

func TestHandshakeSession_FreshCreatesOnce(t *testing.T) {
	mock := newHandshakeMock(t)
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/ws")

	resumed, err := d.handshakeSession(context.Background(), agent.StartConfig{
		Workspace: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("handshakeSession: %v", err)
	}
	if resumed {
		t.Fatal("want resumed=false for fresh create")
	}
	if mock.createCount.Load() != 1 {
		t.Fatalf("session.create calls = %d, want 1", mock.createCount.Load())
	}
	if mock.workspaceCount.Load() != 1 {
		t.Fatalf("workspace.create calls = %d, want 1", mock.workspaceCount.Load())
	}
	if d.sessionID != "session-fresh-1" {
		t.Fatalf("sessionID = %q, want session-fresh-1", d.sessionID)
	}
}

func TestNewDriver_ResumeSeedsLastSeqWithoutReplayingHistory(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.historyEvents = []map[string]any{
		{"event": map[string]any{"type": "permission/preset", "seq": 5}},
		{"event": map[string]any{"type": "sandbox/mode", "seq": 12}},
	}
	mock.installGlobal(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := newDriver(ctx, NewStarter("dsh"), agent.StartConfig{
		SessionID: "session-resume-me",
		Workspace: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	defer d.Close()

	if d.sessionID != "session-resume-me" {
		t.Fatalf("sessionID = %q", d.sessionID)
	}
	if got := d.peekLastSeq(); got != 12 {
		t.Fatalf("lastSeq = %d, want 12 (seeded from history, not replayed)", got)
	}

	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentReady {
			t.Fatalf("first event kind = %s, want EventAgentReady (history must not replay)", ev.Kind)
		}
		if ev.SessionID != "session-resume-me" {
			t.Fatalf("ready SessionID = %q", ev.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for EventAgentReady")
	}
}

func TestReset_InPlaceSingleCreate(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.installGlobal(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := newDriver(ctx, NewStarter("dsh"), agent.StartConfig{
		Workspace: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	defer d.Close()

	if mock.createCount.Load() != 1 {
		t.Fatalf("after start: session.create = %d, want 1", mock.createCount.Load())
	}
	oldID := d.sessionID

	// Drain the startup Ready so Reset's Ready is what we observe next.
	select {
	case <-d.events:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out draining startup EventAgentReady")
	}

	if err := d.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if mock.createCount.Load() != 2 {
		t.Fatalf("after Reset: session.create = %d, want 2 (start + reset, not a double spawn)", mock.createCount.Load())
	}
	if d.sessionID == oldID {
		t.Fatalf("Reset kept sessionID %q; want a new session", oldID)
	}
	if d.sessionID != "session-fresh-2" {
		t.Fatalf("sessionID = %q, want session-fresh-2", d.sessionID)
	}
	if mock.cancelCount.Load() < 1 {
		t.Fatalf("session.cancel calls = %d, want >= 1 for the old session", mock.cancelCount.Load())
	}
	if mock.archiveCount.Load() != 0 {
		t.Fatalf("workspace.archiveSession calls = %d, want 0 (Reset does NOT touch the workspace — repo-scoped workspace survives across /new resets in the same repo)", mock.archiveCount.Load())
	}

	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentReady {
			t.Fatalf("post-reset event kind = %s, want EventAgentReady", ev.Kind)
		}
		if ev.SessionID != d.sessionID {
			t.Fatalf("ready SessionID = %q, want %q", ev.SessionID, d.sessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for post-reset EventAgentReady")
	}

	if host.GetGlobal().Router.SubscriberCount() != 1 {
		t.Fatalf("subscribers = %d, want 1 (old id unsubscribed)", host.GetGlobal().Router.SubscriberCount())
	}
}

func TestClose_StopsBackfill(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.installGlobal(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := newDriver(ctx, NewStarter("dsh"), agent.StartConfig{
		Workspace: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for mock.historyCount.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	afterClose := mock.historyCount.Load()
	time.Sleep(2500 * time.Millisecond)
	later := mock.historyCount.Load()
	if later > afterClose+1 {
		t.Fatalf("history polls kept running after Close: before=%d after=%d", afterClose, later)
	}
}

func TestClose_ArchivesSession(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.installGlobal(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := newDriver(ctx, NewStarter("dsh"), agent.StartConfig{
		Workspace: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	sid := d.sessionID
	if sid == "" {
		t.Fatal("empty sessionID after create")
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if mock.archiveCount.Load() < 1 {
		t.Fatal("Close did not POST /api/workspace.archiveSession (workspace survives, session row hidden)")
	}
	if mock.cancelCount.Load() < 1 {
		t.Fatal("Close did not POST /api/session.cancel before archive")
	}
}

func TestStop_CallsSessionCancel(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.installGlobal(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := newDriver(ctx, NewStarter("dsh"), agent.StartConfig{
		Workspace: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	defer d.Close()

	before := mock.cancelCount.Load()
	if err := d.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if mock.cancelCount.Load() <= before {
		t.Fatal("Stop did not POST /api/session.cancel (dashboard stop button)")
	}
}

func TestStop_EmptySessionID_NotSupported(t *testing.T) {
	d := &driver{}
	if err := d.Stop(context.Background()); !errors.Is(err, agent.ErrNotSupported) {
		t.Fatalf("Stop with empty sessionID = %v, want ErrNotSupported", err)
	}
}

func TestIsBenignCancelErr(t *testing.T) {
	if !isBenignCancelErr(fmt.Errorf("dsh.host: session.cancel: session-not-found: session %q not found (not attached)", "x")) {
		t.Fatal("session-not-found should be benign (dashboard .catch)")
	}
	if isBenignCancelErr(fmt.Errorf("dsh.host: session.cancel: internal: boom")) {
		t.Fatal("internal errors must still surface")
	}
}
