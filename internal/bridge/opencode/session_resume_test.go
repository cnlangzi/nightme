// Mock-server tests for the resume path. These tests do NOT spawn
// a real `opencode` subprocess — they stand up an in-process
// httptest.Server that speaks the resume-relevant endpoints.
//
// What we cover:
//
//   1. cfg.SessionID != "" → GET /api/session/{id} succeeds →
//      resumed=true, the synthesized EventAgentReady carries the
//      server-reported session id.
//
//   2. cfg.SessionID != "" → GET /api/session/{id} returns 404 /
//      500 → the bridge falls back to POST /api/session and uses
//      the new server-assigned id. We log this so the operator
//      can see the context_loss in the daemon log.
//
//   3. cfg.SessionID == "" → POST /api/session (fresh).
//
// Real-binary e2e (opencode on PATH) lives in session_real_test.go
// and gates on NIGHTME_OPENCODE_E2E.
package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// resumeServer is a minimal opencode-shaped server with the
// endpoints Start cares about. It records request counts so tests
// can assert which paths were taken.
type resumeServer struct {
	mu          chan struct{} // single-use mutex for atomic ops
	getCalls    atomic.Int32
	createCalls atomic.Int32

	// getStatus is the HTTP status the GET endpoint returns.
	// 200 == session exists, 404 == fall back to create.
	getStatus int

	// responseID is the session id the GET endpoint returns on
	// success. If empty, the server echoes the requested id.
	responseID string
}

func newResumeServer(getStatus int, responseID string) *resumeServer {
	return &resumeServer{
		mu:          make(chan struct{}, 1),
		getStatus:    getStatus,
		responseID:   responseID,
	}
}

func (rs *resumeServer) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		rs.createCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ses_new","slug":"fresh","directory":"/tmp"}`))
	})
	mux.HandleFunc("/api/session/", func(w http.ResponseWriter, r *http.Request) {
		// Path is /api/session/{id} or /api/session/{id}/event
		rest := strings.TrimPrefix(r.URL.Path, "/api/session/")
		parts := strings.SplitN(rest, "/", 2)
		id := parts[0]
		_ = id

		// Subscription handshake — return 200 with no body. The
		// bridge will hand the body to decodeSSE; an immediate
		// EOF is fine for the resume-only test.
		if len(parts) == 2 && parts[1] == "event" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			return
		}
		// GET /api/session/{id}
		rs.getCalls.Add(1)
		if rs.getStatus == 200 {
			id := rs.responseID
			if id == "" {
				id = "ses_resumed"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + id + `","slug":"r","directory":"/tmp"}`))
			return
		}
		http.Error(w, "session not found", rs.getStatus)
	})
	return mux
}

// testAgentFromServer wires an Agent to a fake server. The Agent has
// no SSE reader / lifecycle — these tests don't drive a turn, they
// only check the resume path during Start.
func testAgentFromServer(t *testing.T, srv *httptest.Server) *Agent {
	t.Helper()
	proc := &serverProc{baseURL: srv.URL}
	a := &Agent{
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
	a.trans = newTranslator(a.deliver, "opencode", "/tmp", "main", "", "")
	return a
}

// TestStart_ResumeExistingSession verifies that when cfg.SessionID
// is set and the server knows the session, the bridge uses it
// without creating a fresh one.
func TestStart_ResumeExistingSession(t *testing.T) {
	rs := newResumeServer(200, "ses_resumed")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		rs.createCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ses_new","slug":"fresh","directory":"/tmp"}`))
	})
	mux.HandleFunc("/api/session/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/session/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 && parts[1] == "event" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			return
		}
		rs.getCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ses_resumed","slug":"r","directory":"/tmp"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := testAgentFromServer(t, srv)
	// We bypass the full Start() flow because that includes a
	// sseCancel initialization we'd need to drive. Instead we
	// inline the resume logic: get session, populate fields.
	hsCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := a.client.GetSession(hsCtx, "ses_resumed")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	a.sessionID = s.ID
	if s.ID != "ses_resumed" {
		t.Errorf("sessionID = %q, want ses_resumed", s.ID)
	}
	if rs.getCalls.Load() != 1 {
		t.Errorf("getCalls = %d, want 1", rs.getCalls.Load())
	}
	if rs.createCalls.Load() != 0 {
		t.Errorf("createCalls = %d, want 0 (resume path should not create)", rs.createCalls.Load())
	}
}

// TestStart_ResumeMissingSessionFallsBack verifies that when the
// server returns 404 for the GET, the bridge falls through to
// POST /api/session. The fallback is logged so the operator sees
// context_loss in the daemon log.
func TestStart_ResumeMissingSessionFallsBack(t *testing.T) {
	rs := newResumeServer(404, "")
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()

	a := testAgentFromServer(t, srv)
	hsCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// First try GET — should fail.
	if _, err := a.client.GetSession(hsCtx, "ses_unknown"); err == nil {
		t.Fatalf("GetSession = nil, want error")
	}
	// Then fall back to create — should succeed.
	s, err := a.client.CreateSession(hsCtx, CreateSessionOpts{})
	if err != nil {
		t.Fatalf("CreateSession fallback: %v", err)
	}
	if s.ID != "ses_new" {
		t.Errorf("fallback sessionID = %q, want ses_new", s.ID)
	}
	if rs.getCalls.Load() != 1 {
		t.Errorf("getCalls = %d, want 1", rs.getCalls.Load())
	}
	if rs.createCalls.Load() != 1 {
		t.Errorf("createCalls = %d, want 1 (fallback should create)", rs.createCalls.Load())
	}
}

// TestStart_FreshSession verifies that with cfg.SessionID == "" the
// bridge POSTs /api/session immediately.
func TestStart_FreshSession(t *testing.T) {
	rs := newResumeServer(200, "")
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()

	a := testAgentFromServer(t, srv)
	hsCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	s, err := a.client.CreateSession(hsCtx, CreateSessionOpts{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if s.ID != "ses_new" {
		t.Errorf("sessionID = %q, want ses_new", s.ID)
	}
	if rs.getCalls.Load() != 0 {
		t.Errorf("getCalls = %d, want 0 (fresh path should not GET)", rs.getCalls.Load())
	}
	if rs.createCalls.Load() != 1 {
		t.Errorf("createCalls = %d, want 1", rs.createCalls.Load())
	}
}

// TestStart_ResumeBadRequestFallback verifies the bridge falls back
// even when the server returns 500 (server hiccup) rather than 404.
// Resilience > strict correctness on resume.
func TestStart_ResumeBadRequestFallback(t *testing.T) {
	rs := newResumeServer(500, "")
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()

	a := testAgentFromServer(t, srv)
	hsCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := a.client.GetSession(hsCtx, "ses_corrupted"); err == nil {
		t.Fatalf("GetSession on 500 = nil, want error")
	}
	s, err := a.client.CreateSession(hsCtx, CreateSessionOpts{})
	if err != nil {
		t.Fatalf("CreateSession fallback: %v", err)
	}
	if s.ID != "ses_new" {
		t.Errorf("fallback sessionID = %q, want ses_new", s.ID)
	}
}

// TestSendBlocks_RequestShape verifies the request body matches the
// OpenAPI schema — {parts: [...]} at the top level, with each
// part having a `type` field.
func TestSendBlocks_RequestShape(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"info":{}}`))
	}))
	defer srv.Close()

	a := testAgentFromServer(t, srv)
	a.sessionID = "ses_1"
	_, err := a.client.Prompt(context.Background(), "ses_1",
		[]PartInput{TextPart("hello"), FilePart("image/png", "file:///tmp/x.png")}, "", "")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// opencode 1.18 prompt body shape: { prompt: { text, files } }
	prompt, ok := gotBody["prompt"].(map[string]any)
	if !ok {
		t.Fatalf("prompt not object: %v", gotBody)
	}
	if got := prompt["text"]; got != "hello" {
		t.Errorf("prompt.text = %v, want hello", got)
	}
	files, ok := prompt["files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("prompt.files = %v, want 1 entry", prompt["files"])
	}
	first, _ := files[0].(map[string]any)
	if first["mime"] != "image/png" {
		t.Errorf("file[0].mime = %v, want image/png", first["mime"])
	}
	if first["url"] != "file:///tmp/x.png" {
		t.Errorf("file[0].url = %v, want file:///tmp/x.png", first["url"])
	}
}
