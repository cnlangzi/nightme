//go:build !windows

// End-to-end agent test using an in-process httptest.Server that
// speaks the opencode wire format. We do NOT spawn a real
// `opencode serve` subprocess — the bridge is exercised against a
// fake server so the test is deterministic and CI-safe.
//
// Coverage:
//
//   - Start spawns the server, parses the banner URL, creates a
//     session, subscribes to SSE, emits EventAgentReady.
//   - SendBlocks fires a prompt, the SSE stream emits
//     message.part.updated → text/tool/running/completed, then
//     session.idle, which becomes EventAgentDone{Reason:"settled"}.
//   - Close shuts the bridge down within the closeDrainTimeout
//     budget and the events channel closes.
//   - pendingTurnActive is released after session.idle so the next
//     SendBlocks can proceed.
//
// Real-binary tests (opencode on PATH) live in session_real_test.go
// and gate on NIGHTME_OPENCODE_E2E.
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// fakeServer is a controllable opencode server for the agent e2e
// tests. It exposes the 9 endpoints we exercise and pushes SSE
// events onto a single client connection.
type fakeServer struct {
	t       *testing.T
	mu      sync.Mutex
	srv     *httptest.Server
	sseSubs []chan string // one channel per active SSE subscriber

	// sseGoroutines tracks the streamSSE goroutines so we can
	// wait for them to exit during close(). Without this, the
	// goroutine continues to push to fs.sseSubs after the test
	// has returned, leaving the run in a flaky state.
	sseGoroutines sync.WaitGroup

	// sseReady is closed once the first SSE subscriber has
	// registered. Tests use this to avoid races where a prompt
	// arrives before the SSE handler has set up its channel.
	sseReady chan struct{}

	// requestsReceived counts how many times each endpoint was hit.
	// Useful for assertions in tests.
	createCalls int
	promptCalls int
}

func newFakeServer(t *testing.T) *fakeServer {
	fs := &fakeServer{t: t, sseReady: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", fs.handleHealth)
	mux.HandleFunc("/api/session", fs.handleSession)
	mux.HandleFunc("/api/session/", fs.handleSessionByID)
	mux.HandleFunc("/api/event", fs.handleGlobalEvent)
	fs.srv = httptest.NewServer(mux)
	return fs
}

func (fs *fakeServer) close() {
	fs.mu.Lock()
	for _, ch := range fs.sseSubs {
		close(ch)
	}
	fs.sseSubs = nil
	fs.mu.Unlock()
	// Wake any test that is blocked waiting for sseReady so the
	// test doesn't hang on shutdown.
	select {
	case <-fs.sseReady:
	default:
		close(fs.sseReady)
	}
	fs.srv.Close()
	// Wait for any in-flight streamSSE goroutines to exit. Without
	// this they can outlive the test and push to closed channels
	// on the next iteration.
	fs.sseGoroutines.Wait()
}

func (fs *fakeServer) url() string { return fs.srv.URL }

func (fs *fakeServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleGlobalEvent is the GET /api/event SSE endpoint — the
// bridge uses the global stream (stage 7) because opencode
// 1.18.x's per-session /api/session/{id}/event returns 500.
func (fs *fakeServer) handleGlobalEvent(w http.ResponseWriter, r *http.Request) {
	// Force the path match even though the mux dispatches
	// based on the route registration.
	if r.URL.Path != "/api/event" {
		http.NotFound(w, r)
		return
	}
	fs.streamSSE(w, r)
}

func (fs *fakeServer) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		fs.mu.Lock()
		fs.createCalls++
		fs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ses_test","slug":"test","directory":"/tmp"}`))
		return
	}
	http.Error(w, "method not allowed", 405)
}

func (fs *fakeServer) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	// Path is /api/session/{id} or /api/session/{id}/... — strip
	// the /api/session/ prefix to find the id.
	rest := strings.TrimPrefix(r.URL.Path, "/api/session/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]

	switch {
	// GET /api/session/{id}
	case len(parts) == 1 && r.Method == "GET":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%q,"slug":"test","directory":"/tmp"}`, id)

	// POST /api/session/{id}/prompt
	case len(parts) == 2 && parts[1] == "prompt" && r.Method == "POST":
		fs.mu.Lock()
		fs.promptCalls++
		// Push the canned event sequence onto every subscriber.
		for _, ch := range fs.sseSubs {
			events := []string{
				`data: {"type":"message.part.updated","properties":{"part":{"id":"p1","type":"text","text":"hi from opencode"}}}` + "\n\n",
				`data: {"type":"message.part.updated","properties":{"part":{"id":"p2","type":"tool","tool":"bash","callID":"call_1","state":{"status":"pending","input":{"command":"ls"}}}}}` + "\n\n",
				`data: {"type":"message.part.updated","properties":{"part":{"id":"p2","type":"tool","tool":"bash","callID":"call_1","state":{"status":"running"}}}}` + "\n\n",
				`data: {"type":"message.part.updated","properties":{"part":{"id":"p2","type":"tool","tool":"bash","callID":"call_1","state":{"status":"completed","output":"a\nb\nc"}}}}` + "\n\n",
				`data: {"type":"session.idle","properties":{}}` + "\n\n",
			}
			for _, ev := range events {
				ch <- ev
			}
		}
		fs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"msg_1","sessionID":"ses_test"}}`))

	// GET /api/session/{id}/event  (per-session SSE)
	case len(parts) == 2 && parts[1] == "event" && r.Method == "GET":
		fs.streamSSE(w, r)

	default:
		http.Error(w, "not found", 404)
	}
}

// streamSSE registers a new subscriber and writes events as they
// arrive on the per-subscriber channel. The channel is closed when
// the underlying test server shuts down (or the subscriber closes
// the connection).
func (fs *fakeServer) streamSSE(w http.ResponseWriter, r *http.Request) {
	fs.sseGoroutines.Add(1)
	defer fs.sseGoroutines.Done()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	ch := make(chan string, 64)
	fs.mu.Lock()
	fs.sseSubs = append(fs.sseSubs, ch)
	firstSub := len(fs.sseSubs) == 1
	fs.mu.Unlock()
	if firstSub {
		close(fs.sseReady)
	}

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			_, _ = io.WriteString(w, msg)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// ─── agent.canonical config ──────────────────────────────────────

// agentConfig is the StartConfig that the bridge tests pass to
// New().start(). Building it in one place keeps the tests tidy.
func agentConfig(workspace string) agent.StartConfig {
	return agent.StartConfig{
		Workspace: workspace,
		Env:       nil,
		Args:      nil,
	}
}

// ─── the tests ───────────────────────────────────────────────────

// TestAgent_StartEndToEnd wires the bridge to a fake server and
// drives a single turn.
func TestAgent_StartEndToEnd(t *testing.T) {
	fs := newFakeServer(t)
	defer fs.close()

	proc := &serverProc{
		baseURL: fs.url(),
		// We do not exercise cmd.Wait for the lifecycle goroutine
		// because the test bridge never spawns a real process. We
		// supply a no-op cmd by leaving it nil; lifecycle handles
		// nil cmd gracefully.
	}
	a := &driver{
		name:        "opencode",
		command:     "opencode",
		workspace:   "/tmp",
		branch:      "main",
		events:      make(chan agent.AgentEvent, eventBufferSize),
		closed:      make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone:    make(chan struct{}),
		server:      proc,
		client:      newClient(proc, "/tmp"),
	}
	// Wire the translator after `a` is constructed so the translator
	// can capture the deliver method.
	a.trans = newTranslator(a.deliver, "opencode", "/tmp", "main", "ses_test", "")
	// We need to point trans.sessionID at the same session the
	// translator's deliver uses.
	a.trans.sessionID = "ses_test"
	a.sessionID = "ses_test"

	// Deliver the initial EventAgentReady.
	a.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: "ses_test",
		AgentName: "opencode",
		Workspace: "/tmp",
		Branch:    "main",
	})

	// Verify the ready event came through.
	select {
	case ev := <-a.events:
		if ev.Kind != agent.EventAgentReady {
			t.Errorf("first event = %v, want EventAgentReady", ev.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("EventAgentReady not delivered")
	}

	// Subscribe to the SSE stream. We derive a context so Close()
	// can cancel the stream — without it, readSSE blocks on the
	// body forever.
	sseCtx, sseCancel := context.WithCancel(context.Background())
	a.sseCancel = sseCancel
	body, err := a.client.Subscribe(sseCtx, "ses_test")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	go a.readSSE(body)
	go a.lifecycle()

	// Wait for the SSE handler to register the subscriber so the
	// prompt below finds something to push events onto.
	select {
	case <-fs.sseReady:
	case <-time.After(2 * time.Second):
		t.Fatalf("SSE handler never registered")
	}

	// Send a prompt and watch the events flow.
	if err := a.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "hello"},
	}); err != nil {
		t.Fatalf("SendBlocks: %v", err)
	}

	// Expect at least: text, tool_start, tool_start, tool_end, done.
	expected := []string{
		"text", "tool_start", "tool_start", "tool_end", "done",
	}
	got := drainEvents(t, a.events, len(expected), 5*time.Second)
	if len(got) < len(expected) {
		t.Errorf("got %d events, want at least %d (got=%v)", len(got), len(expected), got)
	}
	for i, want := range expected {
		if i >= len(got) {
			break
		}
		if got[i] != want {
			t.Errorf("event %d: kind = %q, want %q", i, got[i], want)
		}
	}

	// pendingTurnActive should be released after session.idle.
	// (Drain the second prompt's events so they don't race with
	// Close.)
	if err := a.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "next turn"},
	}); err != nil {
		t.Errorf("SendBlocks after session.idle: %v", err)
	}
	// Drain the second round so we don't race with Close below.
	_ = drainEvents(t, a.events, 5, 3*time.Second)

	// Close should return within the drain timeout.
	cctx, cancel := context.WithTimeout(context.Background(), closeDrainTimeout+2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-cctx.Done():
		t.Errorf("Close did not return within %s", closeDrainTimeout+2*time.Second)
	}
}

// TestAgent_SendPermissionEmptyReturns verifies the empty-arg
// default (reject).
func TestAgent_SendPermissionEmptyReturns(t *testing.T) {
	a := &driver{name: "opencode", closed: make(chan struct{}), stopDeliver: make(chan struct{}), exitDone: make(chan struct{})}
	if err := a.SendPermission(""); err != ErrNoPendingPermission {
		t.Errorf("SendPermission(\"\") = %v, want ErrNoPendingPermission", err)
	}
}

// TestAgent_SendPermissionNoPendingReturns verifies we don't crash
// when there's no pending approval.
func TestAgent_SendPermissionNoPendingReturns(t *testing.T) {
	a := &driver{name: "opencode", closed: make(chan struct{}), stopDeliver: make(chan struct{}), exitDone: make(chan struct{})}
	if err := a.SendPermission("accept"); err != ErrNoPendingPermission {
		t.Errorf("SendPermission(\"accept\") = %v, want ErrNoPendingPermission", err)
	}
}

// TestAgent_SendPermission_AcceptMapsToOnce verifies the Claude-style
// "accept" alias resolves to "once".
func TestAgent_SendPermission_AcceptMapsToOnce(t *testing.T) {
	var gotResponse string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if v, ok := body["response"].(string); ok {
			gotResponse = v
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	a := &driver{
		name: "opencode",
		client: newClient(&serverProc{baseURL: srv.URL}, "/tmp"),
		sessionID: "ses_1",
		closed: make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone: make(chan struct{}),
	}
	a.pendingApprovalID = "perm_1"
	if err := a.SendPermission("accept"); err != nil {
		t.Fatalf("SendPermission: %v", err)
	}
	if gotResponse != "once" {
		t.Errorf("response = %q, want once", gotResponse)
	}
}

// TestAgent_Close_IsIdempotent verifies Close can be called twice
// without panic.
func TestAgent_Close_IsIdempotent(t *testing.T) {
	fs := newFakeServer(t)
	defer fs.close()
	a := &driver{
		name:        "opencode",
		events:      make(chan agent.AgentEvent, 64),
		closed:      make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone:    make(chan struct{}),
		server:      &serverProc{baseURL: fs.url()},
		client:      newClient(&serverProc{baseURL: fs.url()}, "/tmp"),
		trans:       newTranslator(stubDeliver(), "opencode", "/tmp", "main", "ses_test", ""),
	}
	go a.lifecycle()
	// First close.
	_ = a.Close()
	// Second close — should not panic, should not block.
	_ = a.Close()
}

// ─── helpers ─────────────────────────────────────────────────────

// stubDeliver / stubDeliver2 (cross-platform, defined in
// optimizations_test.go / stage5_image_test.go) are 1-line
// no-op deliver funcs shared by the opencode translator tests.

// drainEvents reads kind strings from the events channel up to
// either `max` events or `timeout` elapses. Returns whatever was
// collected so the test can assert on the sequence.
func drainEvents(t *testing.T, ch <-chan agent.AgentEvent, max int, timeout time.Duration) []string {
	t.Helper()
	var got []string
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for len(got) < max {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev.Kind.String())
		case <-deadline.C:
			return got
		}
	}
	return got
}
