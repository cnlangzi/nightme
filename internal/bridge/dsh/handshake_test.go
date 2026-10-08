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
	"github.com/cnlangzi/nightme/internal/bridge/dsh/relay"
)

// handshakeMock is a minimal dsh HTTP surface for handshake / Reset
// tests. It does not speak WebSocket — those paths are covered by
// host/host_test.go.
type handshakeMock struct {
	server *httptest.Server

	createCount      atomic.Int64
	workspaceCount   atomic.Int64
	historyCount     atomic.Int64
	cancelCount      atomic.Int64
	archiveCount     atomic.Int64 // Close calls workspace.archiveSession (repo-scoped workspace survives)
	promptCount      atomic.Int64
	promptFailNext   atomic.Bool
	respondCount     atomic.Int64 // /api/respond — RunOnce auto-allow posts here
	commandsCount    atomic.Int64 // /api/commands/execute — /permission danger-full-access priming
	commandsFailNext atomic.Bool  // force the next commands/execute to fail

	mu               sync.Mutex
	lastCreate       map[string]any
	createIDs        []string
	conflictOnID     string
	mismatchAttach   bool
	historyEvents    []map[string]any
	nextFreshCounter atomic.Int64
	lastPrompt       atomic.Value // map[string]any
	lastRespond      atomic.Value // []byte of last /api/respond body
	lastCommand      atomic.Value // map[string]any
	lastArchive      atomic.Value // map[string]any — archive request body

	respondText atomic.Value // string — when set, prompt handler synthesises a complete turn
}

func newHandshakeMock(t *testing.T) *handshakeMock {
	t.Helper()
	m := &handshakeMock{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/workspace/create", m.handleWorkspaceCreate)
	mux.HandleFunc("/api/session/create", m.handleSessionCreate)
	mux.HandleFunc("/api/session/history", m.handleSessionHistory)
	mux.HandleFunc("/api/session/page", m.handleSessionPage)
	mux.HandleFunc("/api/session/cancel", m.handleSessionCancel)
	mux.HandleFunc("/api/workspace/archiveSession", m.handleWorkspaceArchiveSession)
	mux.HandleFunc("/api/session/prompt", m.handleSessionPrompt)
	mux.HandleFunc("/api/commands/execute", m.handleCommandsExecute)
	mux.HandleFunc("/api/respond", m.handleRespond)
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

func (m *handshakeMock) installGlobal(t *testing.T) *host.Client {
	t.Helper()
	cli := host.New(m.server.URL, nil)
	host.UnsetGlobal()
	host.SetGlobal(cli)
	// Reset the relay singleton so a fresh Start in each test
	// doesn't reuse a stale *Relay. Tests that drive Starter
	// via the relay entry point depend on this reset.
	relay.UnsetForTest()
	relay.ResetForTest()
	t.Cleanup(func() {
		cli.Close()
		host.UnsetGlobal()
		relay.UnsetForTest()
		relay.ResetForTest()
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

// unwrapRequest pulls the typed request body out of the typert
// envelope `{args:{request: ...}}` that dsh web's gateway requires.
// Returns an empty map when the payload isn't wrapped (legacy
// shape, preserved so older probes keep working).
func unwrapRequest(payload json.RawMessage) map[string]any {
	if len(payload) == 0 {
		return map[string]any{}
	}
	var wrapped struct {
		Args struct {
			Request map[string]any `json:"request"`
		} `json:"args"`
	}
	if err := json.Unmarshal(payload, &wrapped); err != nil || wrapped.Args.Request == nil {
		// Legacy shape: payload is the request body itself.
		var flat map[string]any
		_ = json.Unmarshal(payload, &flat)
		if flat == nil {
			flat = map[string]any{}
		}
		return flat
	}
	return wrapped.Args.Request
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
	payload := unwrapRequest(env.Payload)

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

// handleSessionPage serves the typed session/page endpoint
// (Phase 1+). Translates mock.historyEvents from the legacy
// {event:{type,seq,...}} shape to the wire-format {type,seq,
// time,data} record shape. Tests can keep populating
// mock.historyEvents without caring which endpoint drives
// the read.
func (m *handshakeMock) handleSessionPage(w http.ResponseWriter, r *http.Request) {
	m.historyCount.Add(1)
	env := decodeEnvelope(r)
	m.mu.Lock()
	events := m.historyEvents
	m.mu.Unlock()
	records := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		inner, _ := ev["event"].(map[string]any)
		if inner == nil {
			continue
		}
		rec := map[string]any{}
		for k, v := range inner {
			rec[k] = v
		}
		// default time/data if not set
		if _, ok := rec["time"]; !ok {
			rec["time"] = int64(0)
		}
		if _, ok := rec["data"]; !ok {
			rec["data"] = map[string]any{}
		}
		records = append(records, rec)
	}
	writeOK(w, env.RPCID, map[string]any{"records": records, "hasMore": false})
}

func (m *handshakeMock) handleSessionCancel(w http.ResponseWriter, r *http.Request) {
	m.cancelCount.Add(1)
	env := decodeEnvelope(r)
	writeOK(w, env.RPCID, map[string]any{"accepted": true})
}

// handleCommandsExecute mimics dsh's /api/commands/execute reply
// shape — a value envelope `{result:{kind, text}}`. Captures the
// last command payload so tests can assert the /permission
// priming line.
//
// commands/execute is a FLAT-ARG method (no `args.request` wrapper);
// the typert descriptor names agentId/line/submittedAttachments
// directly under `args`, so we read `env.Payload.args` instead of
// going through the typed unwrapRequest helper.
func (m *handshakeMock) handleCommandsExecute(w http.ResponseWriter, r *http.Request) {
	m.commandsCount.Add(1)
	env := decodeEnvelope(r)
	var envelope struct {
		Args map[string]any `json:"args"`
	}
	_ = json.Unmarshal(env.Payload, &envelope)
	m.lastCommand.Store(envelope.Args)
	if m.commandsFailNext.Swap(false) {
		writeErr(w, env.RPCID, "command-rejected", "permission priming refused by mock")
		return
	}
	writeOK(w, env.RPCID, map[string]any{
		"result": map[string]any{
			"kind": "success",
			"text": "ok",
		},
	})
}

func (m *handshakeMock) handleWorkspaceArchiveSession(w http.ResponseWriter, r *http.Request) {
	m.archiveCount.Add(1)
	env := decodeEnvelope(r)
	payload := unwrapRequest(env.Payload)
	m.lastArchive.Store(payload)
	sid, _ := payload["sessionId"].(string)
	writeOK(w, env.RPCID, map[string]any{
		"archivedSessionIds": []string{sid},
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
	payload := unwrapRequest(env.Payload)
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

// handleRespond accepts /api/respond (client-response envelope).
// Shape differs from the other /api/* handlers — see host.RPCClient.Respond.
// Needed so drainForRunResult's auto-allow can POST without a 404.
func (m *handshakeMock) handleRespond(w http.ResponseWriter, r *http.Request) {
	m.respondCount.Add(1)
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
	m.lastRespond.Store(append([]byte(nil), body...))
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"accepted":true}`))
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
		pendingSource:    map[string]string{},
		events:           make(chan agent.AgentEvent, 64),
		translate:        newTranslator("dsh", workspace),
		wireState:        newWireState(),
		closed:           make(chan struct{}),
		lastSeq:          -1,
	}
	d.dispatcher = newDispatcher(d.translate, d.wireState, d, d.deliver)
	return d
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
	if got := d.lastSeq; got != 12 {
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

// the resume tests reach for); not redeclared here.

// GetModel returns d.model for tests. Reads under modelMu so
// concurrent callers don't race the projection store's writer.
func (d *driver) GetModel() string {
	d.modelMu.Lock()
	defer d.modelMu.Unlock()
	return d.model
}

// TestNewDriver_StampsModelFromProjectionBaseline verifies the
// new model resolution path: when the host projection store has a
// baseline that already names our session, the driver's startup
// EventAgentReady carries that model id (no /api/session.models
// round-trip — that endpoint is gone in 0.1.5-rc.1).
func TestNewDriver_StampsModelFromProjectionBaseline(t *testing.T) {
	mock := newHandshakeMock(t)
	cli := mock.installGlobal(t)

	wantModel := "mock-model-from-projection"
	cli.Control.ApplyBaseline(map[string]string{
		"session-fresh-1": wantModel,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := newDriver(ctx, NewStarter("dsh"), agent.StartConfig{
		Workspace: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	defer d.Close()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-d.events:
			if ev.Kind != agent.EventAgentReady {
				continue
			}
			if ev.SessionID != "session-fresh-1" {
				t.Fatalf("Ready SessionID = %q", ev.SessionID)
			}
			if ev.Model != wantModel {
				t.Fatalf("Ready Model = %q, want %q (from projection baseline)",
					ev.Model, wantModel)
			}
			if got := d.GetModel(); got != wantModel {
				t.Fatalf("driver.GetModel = %q, want %q", got, wantModel)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for startup EventAgentReady")
		}
	}
}

// TestNewDriver_EmitsReadyOnProjectionChange verifies a
// projection delta after the initial Ready causes a second
// EventAgentReady so the runtime's captured model stays current
// when the user switches model via the dashboard picker.
func TestNewDriver_EmitsReadyOnProjectionChange(t *testing.T) {
	mock := newHandshakeMock(t)
	cli := mock.installGlobal(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := newDriver(ctx, NewStarter("dsh"), agent.StartConfig{
		Workspace: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	defer d.Close()

	// Drain startup Ready (model is "" because the projection
	// store was empty when the driver registered).
	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentReady {
			t.Fatalf("first event kind = %s, want EventAgentReady", ev.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for startup EventAgentReady")
	}

	cli.Control.ApplyProjection(d.sessionID, "modelSelection", "minimax")

	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentReady {
			t.Fatalf("event kind = %s, want EventAgentReady", ev.Kind)
		}
		if ev.Model != "minimax" {
			t.Fatalf("Ready Model = %q, want minimax", ev.Model)
		}
		if got := d.GetModel(); got != "minimax" {
			t.Fatalf("driver.GetModel = %q, want minimax", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for re-emitted EventAgentReady")
	}
}

// TestNewDriver_NoReadyWhenModelUnchanged verifies a projection
// delta with the same value is deduped — the store already
// short-circuits identical values, and onSessionModelChanged
// short-circuits when model == prev so we don't spam the runtime
// with redundant Ready events.
func TestNewDriver_NoReadyWhenModelUnchanged(t *testing.T) {
	mock := newHandshakeMock(t)
	cli := mock.installGlobal(t)

	cli.Control.ApplyBaseline(map[string]string{
		"session-fresh-1": "minimax",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := newDriver(ctx, NewStarter("dsh"), agent.StartConfig{
		Workspace: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	defer d.Close()

	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentReady {
			t.Fatalf("first event kind = %s, want EventAgentReady", ev.Kind)
		}
		if ev.Model != "minimax" {
			t.Fatalf("Ready Model = %q, want minimax", ev.Model)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for startup EventAgentReady")
	}

	cli.Control.ApplyProjection(d.sessionID, "modelSelection", "minimax")

	select {
	case ev := <-d.events:
		t.Fatalf("unexpected re-emitted EventAgentReady with model=%q", ev.Model)
	case <-time.After(500 * time.Millisecond):
		// expected: nothing
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
	req, _ := mock.lastArchive.Load().(map[string]any)
	if req == nil {
		t.Fatal("archive request not captured")
	}
	if stop, _ := req["stopActivity"].(bool); !stop {
		t.Errorf("archive stopActivity = %#v, want true", req["stopActivity"])
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

// TestNewDriver_PrimesPermissionDangerFullAccess verifies that
// after a fresh session.create the bridge fires
// /api/commands/execute with line "/permission danger-full-access"
// (F-dsh-preset-1). The dashboard's "Full access" picker does the
// same — verified live against dsh 0.1.2-rc.1.
func TestNewDriver_PrimesPermissionDangerFullAccess(t *testing.T) {
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

	if mock.commandsCount.Load() != 1 {
		t.Fatalf("after start: commands/execute = %d, want 1",
			mock.commandsCount.Load())
	}
	cmd, _ := mock.lastCommand.Load().(map[string]any)
	if cmd == nil {
		t.Fatal("lastCommand not captured")
	}
	if got, _ := cmd["line"].(string); got != "/permission danger-full-access" {
		t.Errorf("priming line = %q, want %q",
			got, "/permission danger-full-access")
	}
	if got, _ := cmd["agentId"].(string); got != d.sessionID {
		t.Errorf("priming agentId = %q, want %q", got, d.sessionID)
	}
	atts, ok := cmd["submittedAttachments"].([]any)
	if !ok || len(atts) != 0 {
		t.Errorf("submittedAttachments = %#v, want empty array", cmd["submittedAttachments"])
	}
	if _, ok := cmd["images"]; ok {
		t.Errorf("commands/execute still sends images: %#v", cmd["images"])
	}
}

// TestNewDriver_HonorsExplicitPermissionMode verifies a non-default
// cfg.PermissionMode is plumbed through to /permission verbatim.
func TestNewDriver_HonorsExplicitPermissionMode(t *testing.T) {
	mock := newHandshakeMock(t)
	mock.installGlobal(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := newDriver(ctx, NewStarter("dsh"), agent.StartConfig{
		Workspace:      "/tmp/ws",
		PermissionMode: "workspace-write",
	})
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	defer d.Close()

	cmd, _ := mock.lastCommand.Load().(map[string]any)
	if got, _ := cmd["line"].(string); got != "/permission workspace-write" {
		t.Errorf("priming line = %q, want %q",
			got, "/permission workspace-write")
	}
}

// TestNewDriver_PermissionPrimingFailureIsNonFatal verifies a
// failing commands/execute doesn't abort session startup — the
// runtime's auto-allow on RunOnce paths is the safety net for
// any leftover approvals.
func TestNewDriver_PermissionPrimingFailureIsNonFatal(t *testing.T) {
	mock := newHandshakeMock(t)
	// Force commands/execute to fail every call.
	mock.commandsFailNext.Store(true)
	mock.installGlobal(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := newDriver(ctx, NewStarter("dsh"), agent.StartConfig{
		Workspace: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("newDriver should swallow /permission failure: %v", err)
	}
	defer d.Close()

	if mock.createCount.Load() != 1 {
		t.Errorf("session.create = %d, want 1 (handshake still ran)",
			mock.createCount.Load())
	}
}

// TestReset_ReplaysPermissionMode verifies that Reset (/new)
// replays the captured permission mode on the new session —
// without this, every /new would silently drop back to the dsh
// default and the next approval would wedge.
func TestReset_ReplaysPermissionMode(t *testing.T) {
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

	// Drain startup Ready so Reset's Ready is what we observe next.
	select {
	case <-d.events:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out draining startup EventAgentReady")
	}

	before := mock.commandsCount.Load()
	if err := d.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	after := mock.commandsCount.Load()
	if after-before != 1 {
		t.Fatalf("Reset should fire /permission once more (before=%d after=%d)",
			before, after)
	}
	cmd, _ := mock.lastCommand.Load().(map[string]any)
	if got, _ := cmd["agentId"].(string); got != d.sessionID {
		t.Errorf("reset priming agentId = %q, want new sessionID %q",
			got, d.sessionID)
	}
	if got, _ := cmd["line"].(string); got != "/permission danger-full-access" {
		t.Errorf("reset priming line = %q, want %q",
			got, "/permission danger-full-access")
	}
}

// TestFirstNonEmpty exercises the bridge helper used to apply the
// default permission mode when cfg.PermissionMode is empty.
func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "danger-full-access"); got != "danger-full-access" {
		t.Errorf("firstNonEmpty fallback = %q", got)
	}
	if got := firstNonEmpty("workspace-write", "danger-full-access"); got != "workspace-write" {
		t.Errorf("firstNonEmpty explicit = %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty all-empty = %q", got)
	}
}
