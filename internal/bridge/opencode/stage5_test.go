// Tests for the stage 5 endpoints: Compact, ListSessions, ListProviders,
// ListModels. Exercises the wire shape (URL + method + body) using
// in-process httptest servers.
package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClient_Compact_NoBodyReturned verifies Compact fires
// POST /api/session/{id}/compact with no body and accepts 204.
func TestClient_Compact_NoBodyReturned(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody = nil
		// Read body to drain pipe; we don't expect any.
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = buf[:n]
		w.WriteHeader(204)
	}))
	defer srv.Close()

	c := newClient(&serverProc{baseURL: srv.URL}, "/tmp")
	if err := c.Compact(context.Background(), "ses_1"); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/session/ses_1/compact" {
		t.Errorf("path = %q", gotPath)
	}
	if len(gotBody) != 0 {
		t.Errorf("body = %q, want empty", gotBody)
	}
}

// TestClient_Compact_WrappedAuthorizeError verifies non-2xx
// responses are surfaced as errors.
func TestClient_Compact_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`service unavailable`))
	}))
	defer srv.Close()
	c := newClient(&serverProc{baseURL: srv.URL}, "/tmp")
	if err := c.Compact(context.Background(), "ses_1"); err == nil {
		t.Errorf("Compact on 503 = nil, want error")
	}
}

// TestAgent_Compact_NoServerReturnsError verifies Compact on an
// unstarted Agent returns an error.
func TestAgent_Compact_NoServerReturnsError(t *testing.T) {
	a := &driver{name: "opencode"}
	if err := a.Compact(context.Background()); err == nil {
		t.Errorf("Compact on unstarted = nil, want error")
	}
}

// TestClient_ListSessions_Wrapped verifies the wrapped
// {data:[...]} shape is parsed.
func TestClient_ListSessions_Wrapped(t *testing.T) {
	var gotPath string
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"ses_1","slug":"a","directory":"/tmp"},
			{"id":"ses_2","slug":"b","directory":"/tmp"}
		],"cursor":{"next":""}}`))
	}))
	defer srv.Close()
	c := newClient(&serverProc{baseURL: srv.URL}, "/tmp")
	got, err := c.ListSessions(context.Background(), 50)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
	if got[0].ID != "ses_1" || got[1].ID != "ses_2" {
		t.Errorf("ids = %s %s, want ses_1 ses_2", got[0].ID, got[1].ID)
	}
	if gotPath != "/api/session" {
		t.Errorf("path = %q", gotPath)
	}
	if gotQuery != "limit=50" {
		t.Errorf("query = %q, want limit=50", gotQuery)
	}
}

// TestClient_ListSessions_Bare verifies the bare array shape
// also works (older opencode versions).
func TestClient_ListSessions_Bare(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"ses_1","slug":"a","directory":"/tmp"}]`))
	}))
	defer srv.Close()
	c := newClient(&serverProc{baseURL: srv.URL}, "/tmp")
	got, err := c.ListSessions(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ses_1" {
		t.Errorf("got %+v, want 1 entry ses_1", got)
	}
}

// TestClient_ListProviders verifies the provider list endpoint.
func TestClient_ListProviders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/provider" {
			t.Errorf("path = %q, want /api/provider", r.URL.Path)
		}
		if r.Method != "GET" {
			t.Errorf("method = %q, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"anthropic","name":"Anthropic","source":"env"},
			{"id":"openai","name":"OpenAI","source":"env"}
		]}`))
	}))
	defer srv.Close()
	c := newClient(&serverProc{baseURL: srv.URL}, "/tmp")
	got, err := c.ListProviders(context.Background())
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
	if got[0].ID != "anthropic" || got[1].ID != "openai" {
		t.Errorf("ids = %s %s, want anthropic openai", got[0].ID, got[1].ID)
	}
}

// TestClient_ListProviders_EmptyDataFallsBackToBare verifies the
// fallback path when the server returns a bare array.
func TestClient_ListProviders_EmptyDataFallsBackToBare(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"anthropic","name":"Anthropic","source":"env"}]`))
	}))
	defer srv.Close()
	c := newClient(&serverProc{baseURL: srv.URL}, "/tmp")
	got, err := c.ListProviders(context.Background())
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(got) != 1 || got[0].ID != "anthropic" {
		t.Errorf("got %+v, want 1 entry anthropic", got)
	}
}

// TestClient_ListModels verifies the model list endpoint returns
// a JSON object.
func TestClient_ListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/model" {
			t.Errorf("path = %q, want /api/model", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"anthropic":[{"id":"claude-sonnet-4"}]}`))
	}))
	defer srv.Close()
	c := newClient(&serverProc{baseURL: srv.URL}, "/tmp")
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if _, ok := got["anthropic"]; !ok {
		t.Errorf("model map = %v, want key anthropic", got)
	}
}

// TestAgent_ListSessions_NoClientReturnsError verifies the agent
// guards against nil client.
func TestAgent_ListSessions_NoClientReturnsError(t *testing.T) {
	a := &driver{name: "opencode"}
	if _, err := a.ListSessions(context.Background(), 10); err == nil {
		t.Errorf("ListSessions on unstarted = nil, want error")
	}
	if _, err := a.ListProviders(context.Background()); err == nil {
		t.Errorf("ListProviders on unstarted = nil, want error")
	}
	if _, err := a.ListModels(context.Background()); err == nil {
		t.Errorf("ListModels on unstarted = nil, want error")
	}
}

// TestClient_Compact_NoSessionID verifies the empty-id guard.
func TestClient_Compact_NoSessionID(t *testing.T) {
	c := newClient(&serverProc{baseURL: "http://localhost:9999"}, "/tmp")
	if err := c.Compact(context.Background(), ""); err == nil {
		t.Errorf("Compact with empty sessionID = nil, want error")
	}
}

// TestCompact_EmptyJSONBody verifies the body capture in the
// handler (we don't send JSON, but the test should still parse).
func TestCompact_EmptyJSONBody(t *testing.T) {
	var bodyRead []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		bodyRead = buf[:n]
		w.WriteHeader(204)
	}))
	defer srv.Close()
	c := newClient(&serverProc{baseURL: srv.URL}, "/tmp")
	if err := c.Compact(context.Background(), "ses_xyz"); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(bodyRead) != 0 {
		t.Errorf("server saw body = %q, want empty", bodyRead)
	}
}

// force-import safety: keep json reachable even if the test file
// refactors away the only json-using assertion.
var _ = json.Marshal
