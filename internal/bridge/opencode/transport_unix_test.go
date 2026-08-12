//go:build !windows

// Tests for the SSE parser and the client HTTP wrapper.
//
// We do not spawn a real `opencode` process. Instead we stand up an
// in-process httptest.Server that speaks the opencode wire format
// (JSON over HTTP for calls; SSE for the event stream). The bridge
// sees a baseURL + workspace + password and the rest is plain HTTP.
package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── deliver stub ─────────────────────────────────────────────────

// recorder captures events delivered by the translator so tests can
// introspect them without leaking the agent package surface.
type recorder struct {
	mu    sync.Mutex
	evs   []agent.AgentEvent
}

func (r *recorder) deliver(ev agent.AgentEvent) agent.AgentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, ev)
	return ev
}

func (r *recorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.evs))
	for i, ev := range r.evs {
		out[i] = ev.Kind.String()
	}
	return out
}

func (r *recorder) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.evs))
	for _, ev := range r.evs {
		if ev.Kind == agent.EventAgentText {
			out = append(out, ev.Text)
		}
	}
	return out
}

// ─── decodeSSE ───────────────────────────────────────────────────

func TestDecodeSSE_HappyPath(t *testing.T) {
	input := "data: {\"type\":\"message.part.updated\",\"properties\":{\"part\":{\"type\":\"text\",\"text\":\"hi\"}}}\n\n"
	var got SessionEvent
	if err := decodeSSE(strings.NewReader(input), func(ev SessionEvent) error {
		got = ev
		return nil
	}); err != nil {
		t.Fatalf("decodeSSE: %v", err)
	}
	if got.Type != "message.part.updated" {
		t.Errorf("type = %q, want message.part.updated", got.Type)
	}
}

func TestDecodeSSE_MultiEvent(t *testing.T) {
	input := "data: {\"type\":\"session.idle\",\"properties\":{}}\n\n" +
		"data: {\"type\":\"message.part.updated\",\"properties\":{\"part\":{\"type\":\"text\",\"text\":\"a\"}}}\n\n" +
		"data: {\"type\":\"message.part.updated\",\"properties\":{\"part\":{\"type\":\"text\",\"text\":\"b\"}}}\n\n"
	var types []string
	if err := decodeSSE(strings.NewReader(input), func(ev SessionEvent) error {
		types = append(types, ev.Type)
		return nil
	}); err != nil {
		t.Fatalf("decodeSSE: %v", err)
	}
	if len(types) != 3 {
		t.Fatalf("got %d events, want 3", len(types))
	}
	want := []string{"session.idle", "message.part.updated", "message.part.updated"}
	for i, w := range want {
		if types[i] != w {
			t.Errorf("event %d: type = %q, want %q", i, types[i], w)
		}
	}
}

func TestDecodeSSE_IgnoresCommentsAndUnknownFields(t *testing.T) {
	input := ": keepalive\n" +
		"id: 1\n" +
		"event: ping\n" +
		"retry: 5000\n" +
		"data: {\"type\":\"session.idle\",\"properties\":{}}\n\n"
	var types []string
	if err := decodeSSE(strings.NewReader(input), func(ev SessionEvent) error {
		types = append(types, ev.Type)
		return nil
	}); err != nil {
		t.Fatalf("decodeSSE: %v", err)
	}
	if len(types) != 1 || types[0] != "session.idle" {
		t.Errorf("got %v, want [session.idle]", types)
	}
}

func TestDecodeSSE_BadEventKeepsStreamAlive(t *testing.T) {
	input := "data: not-json\n\n" +
		"data: {\"type\":\"message.part.updated\",\"properties\":{\"part\":{\"type\":\"text\",\"text\":\"x\"}}}\n\n"
	var types []string
	if err := decodeSSE(strings.NewReader(input), func(ev SessionEvent) error {
		types = append(types, ev.Type)
		return nil
	}); err != nil {
		t.Fatalf("decodeSSE: %v", err)
	}
	if len(types) != 1 || types[0] != "message.part.updated" {
		t.Errorf("got %v, want to skip the bad event", types)
	}
}

func TestDecodeSSE_LeadingSpace(t *testing.T) {
	input := "data: {\"type\":\"session.idle\",\"properties\":{}}\n\n"
	var got SessionEvent
	if err := decodeSSE(strings.NewReader(input), func(ev SessionEvent) error {
		got = ev
		return nil
	}); err != nil {
		t.Fatalf("decodeSSE: %v", err)
	}
	if got.Type != "session.idle" {
		t.Errorf("type = %q, want session.idle", got.Type)
	}
}

// ─── translator ──────────────────────────────────────────────────

func TestTranslator_TextPart(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")
	props, _ := json.Marshal(map[string]any{"part": map[string]any{"type": "text", "text": "Hello, world"}})
	if err := tr.handleEvent(SessionEvent{Type: "message.part.updated", Properties: props}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	if got := rec.kinds(); len(got) != 1 || got[0] != "text" {
		t.Errorf("kinds = %v, want [text]", got)
	}
	if got := rec.texts(); len(got) != 1 || got[0] != "Hello, world" {
		t.Errorf("texts = %v, want [Hello, world]", got)
	}
}

func TestTranslator_ReasoningPart(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")
	props, _ := json.Marshal(map[string]any{"part": map[string]any{"type": "reasoning", "text": "deep thought"}})
	if err := tr.handleEvent(SessionEvent{Type: "message.part.updated", Properties: props}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	if got := rec.texts(); len(got) != 1 || got[0] != "[思考] deep thought" {
		t.Errorf("texts = %v, want [[思考] deep thought]", got)
	}
}

func TestTranslator_ToolLifecycle(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")

	states := []struct {
		status string
		input  any
		output any
	}{
		{"pending", map[string]any{"command": "ls"}, nil},
		{"running", nil, nil},
		{"completed", nil, "out"},
	}
	for _, s := range states {
		stateObj := map[string]any{"status": s.status}
		if s.input != nil {
			stateObj["input"] = s.input
		}
		if s.output != nil {
			stateObj["output"] = s.output
		}
		props, _ := json.Marshal(map[string]any{
			"part": map[string]any{
				"type":   "tool",
				"tool":   "bash",
				"callID": "call_1",
				"state":  stateObj,
			},
		})
		if err := tr.handleEvent(SessionEvent{Type: "message.part.updated", Properties: props}); err != nil {
			t.Fatalf("handleEvent(%s): %v", s.status, err)
		}
	}
	want := []string{"tool_start", "tool_start", "tool_end"}
	if got := rec.kinds(); len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	} else {
		for i, w := range want {
			if got[i] != w {
				t.Errorf("event %d: kind = %q, want %q", i, got[i], w)
			}
		}
	}
}

func TestTranslator_SessionIdle(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")
	// Synthesise a prior text event so the terminal event
	// records Reason=settled rather than the empty-response
	// hint (Reason=empty). Empty detection is exercised by
	// TestTranslator_SessionIdle_EmptyReason below.
	textProps, _ := json.Marshal(map[string]any{"text": "hi"})
	if err := tr.handleEvent(SessionEvent{Type: "session.next.text.delta", Properties: textProps}); err != nil {
		t.Fatalf("handleEvent text: %v", err)
	}
	idleProps, _ := json.Marshal(map[string]any{})
	if err := tr.handleEvent(SessionEvent{Type: "session.idle", Properties: idleProps}); err != nil {
		t.Fatalf("handleEvent idle: %v", err)
	}
	if len(rec.evs) != 2 || rec.evs[1].Done == nil || rec.evs[1].Done.Reason != "settled" {
		t.Errorf("events = %+v, want [..., Done{Reason:settled}]", rec.evs)
	}
}

// TestTranslator_SessionIdle_EmptyReason asserts the
// empty-response hint: a turn that ends with no text / tool
// events emits EventAgentDone{Reason:"empty"} so the runtime
// can surface "(empty response)" to the user instead of an
// ambiguous silent success.
func TestTranslator_SessionIdle_EmptyReason(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")
	props, _ := json.Marshal(map[string]any{})
	if err := tr.handleEvent(SessionEvent{Type: "session.next.step.ended", Properties: props}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	if len(rec.evs) != 1 || rec.evs[0].Done == nil || rec.evs[0].Done.Reason != "empty" {
		t.Errorf("event = %+v, want Done{Reason:empty}", rec.evs)
	}
}

// TestTranslator_StepStartedCountsAsActivity guards the refinement
// added in stage 8.3: a turn that fires session.next.step.started
// (proving the model took a turn) but produces no payload-bearing
// events must NOT be marked "empty". This happens on opencode 1.18
// when the model calls tools but the step event payload doesn't
// include the callIDs we need to render them — the bridge sees a
// "ghost" turn. Marking it "empty" would surface a misleading
// "(empty response)" hint in the channel footer.
func TestTranslator_StepStartedCountsAsActivity(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")
	stepStarted, _ := json.Marshal(map[string]any{})
	if err := tr.handleEvent(SessionEvent{Type: "session.next.step.started", Properties: stepStarted}); err != nil {
		t.Fatalf("handleEvent step.started: %v", err)
	}
	stepEnded, _ := json.Marshal(map[string]any{})
	if err := tr.handleEvent(SessionEvent{Type: "session.next.step.ended", Properties: stepEnded}); err != nil {
		t.Fatalf("handleEvent step.ended: %v", err)
	}
	if len(rec.evs) != 1 || rec.evs[0].Done == nil {
		t.Fatalf("events = %+v, want one Done event", rec.evs)
	}
	if rec.evs[0].Done.Reason != "settled" {
		t.Errorf("Done.Reason = %q, want settled (step.started proves activity)",
			rec.evs[0].Done.Reason)
	}
}

// TestTranslator_ResetTurnClearsStepFlag asserts ResetTurn resets
// both turnHadContent AND turnHadStep so a subsequent turn doesn't
// inherit the previous turn's step signal.
func TestTranslator_ResetTurnClearsStepFlag(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")
	stepStarted, _ := json.Marshal(map[string]any{})
	_ = tr.handleEvent(SessionEvent{Type: "session.next.step.started", Properties: stepStarted})

	tr.ResetTurn()

	// Now fire step.ended WITHOUT re-firing step.started. The turn
	// is empty (no content, no fresh step.started).
	stepEnded, _ := json.Marshal(map[string]any{})
	if err := tr.handleEvent(SessionEvent{Type: "session.next.step.ended", Properties: stepEnded}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	if len(rec.evs) != 1 || rec.evs[0].Done == nil || rec.evs[0].Done.Reason != "empty" {
		t.Errorf("after ResetTurn, Done.Reason = %v, want empty (flags were cleared)",
			rec.evs)
	}
}

func TestTranslator_PermissionAsked(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")
	props, _ := json.Marshal(map[string]any{
		"sessionID":  "ses_1",
		"id":         "perm_1",
		"permission": "bash",
		"patterns":   []string{"rm -rf build"},
	})
	if err := tr.handleEvent(SessionEvent{Type: "permission.asked", Properties: props}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	if len(rec.evs) != 1 || rec.evs[0].Permission == nil {
		t.Fatalf("no permission event recorded")
	}
	if rec.evs[0].Permission.Tool != "bash" {
		t.Errorf("tool = %q, want bash", rec.evs[0].Permission.Tool)
	}
	want := []string{"once", "always", "reject"}
	got := rec.evs[0].Permission.Options
	if len(got) != len(want) {
		t.Fatalf("options = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("option %d = %q, want %q", i, got[i], w)
		}
	}
}

var _ = json.Marshal

func TestTranslator_UnknownEventKeepsStreamAlive(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")
	props, _ := json.Marshal(map[string]any{})
	if err := tr.handleEvent(SessionEvent{Type: "made.up.event", Properties: props}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	if len(rec.evs) != 0 {
		t.Errorf("unknown event must not deliver, got %v", rec.evs)
	}
}

// TestTranslator_NoiseEventsSilentlyDropped covers the allowlist of
// known-noise event types opencode 1.18 emits (plugin.added,
// catalog.updated, etc). They must NOT trigger delivery, and they
// must NOT panic. The previous behaviour (log every occurrence at
// info level) was correct but noisy; we now drop them silently.
func TestTranslator_NoiseEventsSilentlyDropped(t *testing.T) {
	for _, evType := range []string{
		"plugin.added",
		"catalog.updated",
		"reference.updated",
		"integration.updated",
		"server.connected",
		"message.updated",
		"message.removed",
	} {
		t.Run(evType, func(t *testing.T) {
			rec := &recorder{}
			tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")
			props, _ := json.Marshal(map[string]any{})
			if err := tr.handleEvent(SessionEvent{Type: evType, Properties: props}); err != nil {
				t.Fatalf("handleEvent: %v", err)
			}
			if len(rec.evs) != 0 {
				t.Errorf("noise event %q must not deliver, got %v", evType, rec.evs)
			}
		})
	}
}

// TestTranslator_PromptLifecycleHandled covers the opencode 1.18
// prompt lifecycle markers. Neither prompt.admitted nor prompted
// carries payload we consume today, but the bridge must accept
// them without crashing so a forward-compat opencode release that
// starts emitting them does not break the stream.
func TestTranslator_PromptLifecycleHandled(t *testing.T) {
	for _, evType := range []string{
		"session.next.prompt.admitted",
		"session.next.prompted",
	} {
		t.Run(evType, func(t *testing.T) {
			rec := &recorder{}
			tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")
			props, _ := json.Marshal(map[string]any{"promptID": "p_42"})
			if err := tr.handleEvent(SessionEvent{Type: evType, Properties: props}); err != nil {
				t.Fatalf("handleEvent: %v", err)
			}
			if len(rec.evs) != 0 {
				t.Errorf("prompt lifecycle event %q must not deliver (no consumer yet), got %v", evType, rec.evs)
			}
		})
	}
}

// TestTranslator_AvailableCommandsCache exercises the stage 8.2
// available_commands_update caching: the bridge stores every
// advertised command (name + raw payload), so the runtime shim
// can introspect via Agent.AvailableBuiltinCommands() /
// IsBuiltinCommand(). The list is sorted alphabetically and
// resets on every new available_commands_update (so a server-
// side deletion propagates).
func TestTranslator_AvailableCommandsCache(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")

	// Initial: no commands.
	if got := tr.AvailableBuiltinCommands(); got != nil {
		t.Errorf("initial AvailableBuiltinCommands = %v, want nil", got)
	}
	if tr.IsBuiltinCommand("clear") {
		t.Error("IsBuiltinCommand should return false before any update")
	}

	// First update — three commands.
	props1, _ := json.Marshal(map[string]any{
		"availableCommands": []map[string]any{
			{"name": "clear", "description": "reset session"},
			{"name": "compact", "description": "compress history"},
			{"name": "help", "description": "show help"},
		},
	})
	if err := tr.handleEvent(SessionEvent{Type: "available_commands_update", Properties: props1}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}

	got := tr.AvailableBuiltinCommands()
	want := []string{"clear", "compact", "help"}
	if len(got) != len(want) {
		t.Fatalf("AvailableBuiltinCommands = %v, want %v", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("AvailableBuiltinCommands[%d] = %q, want %q", i, got[i], name)
		}
	}

	// IsBuiltinCommand lookup: with and without leading slash,
	// and a missing name.
	if !tr.IsBuiltinCommand("clear") {
		t.Error(`IsBuiltinCommand("clear") = false, want true`)
	}
	if !tr.IsBuiltinCommand("/clear") {
		t.Error(`IsBuiltinCommand("/clear") = false, want true`)
	}
	if tr.IsBuiltinCommand("nonexistent") {
		t.Error(`IsBuiltinCommand("nonexistent") = true, want false`)
	}

	// Second update — server removes "help", adds "editor".
	props2, _ := json.Marshal(map[string]any{
		"availableCommands": []map[string]any{
			{"name": "clear", "description": "reset session"},
			{"name": "compact", "description": "compress history"},
			{"name": "editor", "description": "open editor"},
		},
	})
	if err := tr.handleEvent(SessionEvent{Type: "available_commands_update", Properties: props2}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	got = tr.AvailableBuiltinCommands()
	want = []string{"clear", "compact", "editor"}
	if len(got) != len(want) {
		t.Fatalf("after reset AvailableBuiltinCommands = %v, want %v", got, want)
	}
	if tr.IsBuiltinCommand("help") {
		t.Error("after reset IsBuiltinCommand(help) = true, want false")
	}
	if !tr.IsBuiltinCommand("editor") {
		t.Error("after reset IsBuiltinCommand(editor) = false, want true")
	}

	// No events were delivered to the runtime (introspection only).
	if len(rec.evs) != 0 {
		t.Errorf("available_commands_update must not deliver, got %v", rec.evs)
	}
}

// ─── Client against httptest.Server ───────────────────────────────

func TestClient_CreateSession(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ses_42","slug":"hello","directory":"/tmp"}`))
	}))
	defer srv.Close()

	c := newClient(&serverProc{baseURL: srv.URL}, "/tmp")
	s, err := c.CreateSession(context.Background(), CreateSessionOpts{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if s.ID != "ses_42" {
		t.Errorf("id = %q, want ses_42", s.ID)
	}
	if gotMethod != "POST" || gotPath != "/api/session" {
		t.Errorf("got %s %s, want POST /api/session", gotMethod, gotPath)
	}
}

func TestClient_DirectoryHeader(t *testing.T) {
	var gotDir string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDir = r.Header.Get("x-opencode-directory")
		_, _ = w.Write([]byte(`{"id":"ses_1"}`))
	}))
	defer srv.Close()
	c := newClient(&serverProc{baseURL: srv.URL}, "/tmp/workspace")
	_, _ = c.GetSession(context.Background(), "ses_1")
	if gotDir != "%2Ftmp%2Fworkspace" {
		t.Errorf("directory header = %q, want /tmp/workspace URL-encoded", gotDir)
	}
}

func TestClient_PasswordAuth(t *testing.T) {
	var gotUser, gotPw string
	var gotOk bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPw, gotOk = r.BasicAuth()
		_, _ = w.Write([]byte(`{"id":"ses_1"}`))
	}))
	defer srv.Close()
	c := newClient(&serverProc{baseURL: srv.URL}, "/tmp")
	c.setPassword("secret")
	_, _ = c.GetSession(context.Background(), "ses_1")
	if !gotOk {
		t.Fatalf("no basic auth on request")
	}
	if gotUser != "opencode" || gotPw != "secret" {
		t.Errorf("auth = %s:%s, want opencode:secret", gotUser, gotPw)
	}
}

func TestClient_PromptReturns503SurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`server busy`))
	}))
	defer srv.Close()
	c := newClient(&serverProc{baseURL: srv.URL}, "/tmp")
	_, err := c.Prompt(context.Background(), "ses_1", []PartInput{TextPart("hi")})
	if err == nil {
		t.Fatalf("expected error on 503")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %v, want contains 503", err)
	}
}

func TestClient_SubscribeSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"type\":\"session.idle\",\"properties\":{}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()
	c := newClient(&serverProc{baseURL: srv.URL}, "/tmp")
	body, err := c.Subscribe(context.Background(), "ses_1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer body.Close()
	br := bufio.NewReader(body)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString: %v", err)
	}
	if !strings.HasPrefix(line, "data:") {
		t.Errorf("first line = %q, want data: prefix", line)
	}
}

func TestReplyPermission_RoutesToRequestID(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	c := newClient(&serverProc{baseURL: srv.URL}, "/tmp")
	if err := c.ReplyPermission(context.Background(), "ses_1", "perm_42", "once"); err != nil {
		t.Fatalf("ReplyPermission: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/session/ses_1/permission/perm_42/reply" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody["response"] != "once" {
		t.Errorf("body = %v, want response=once", gotBody)
	}
}
