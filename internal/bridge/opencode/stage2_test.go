// Stage 2 tests: Abort, SetModel, usage tracking, tool name mapping,
// current_mode_update, available_commands_update.
//
// These tests build on the helpers in transport_test.go and
// agent_e2e_test.go (recorder, fakeServer). All run with the in-
// process httptest server — no real `opencode` subprocess.
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── Abort / SetModel ────────────────────────────────────────────

// TestAgent_AbortCallsInterrupt verifies Abort hits the
// /api/session/{id}/interrupt endpoint.
func TestAgent_AbortCallsInterrupt(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(204)
	}))
	defer srv.Close()

	a := &driver{
		name:      "opencode",
		server:    &serverProc{baseURL: srv.URL},
		client:    newClient(&serverProc{baseURL: srv.URL}, "/tmp"),
		sessionID: "ses_1",
	}
	if err := a.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/session/ses_1/interrupt" {
		t.Errorf("path = %q", gotPath)
	}
}

// TestAgent_SetModelCallsSwitch verifies SetModel posts to
// /api/session/{id}/model with providerID + modelID.
func TestAgent_SetModelCallsSwitch(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	a := &driver{
		name:      "opencode",
		server:    &serverProc{baseURL: srv.URL},
		client:    newClient(&serverProc{baseURL: srv.URL}, "/tmp"),
		sessionID: "ses_1",
	}
	if err := a.SetModel(context.Background(), "anthropic", "claude-sonnet-4"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if gotBody["providerID"] != "anthropic" {
		t.Errorf("providerID = %v, want anthropic", gotBody["providerID"])
	}
	if gotBody["modelID"] != "claude-sonnet-4" {
		t.Errorf("modelID = %v", gotBody["modelID"])
	}
}

// TestAgent_AbortNoServerReturnsError verifies Abort on an unstarted
// Agent returns an error rather than crashing.
func TestAgent_AbortNoServerReturnsError(t *testing.T) {
	a := &driver{name: "opencode"}
	if err := a.Abort(context.Background()); err == nil {
		t.Errorf("Abort on unstarted agent = nil, want error")
	}
}

// TestAgent_SetModelNoSessionReturnsError verifies SetModel on a
// bridge without a session returns an error.
func TestAgent_SetModelNoSessionReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	a := &driver{
		name:   "opencode",
		server: &serverProc{baseURL: srv.URL},
		client: newClient(&serverProc{baseURL: srv.URL}, "/tmp"),
	}
	if err := a.SetModel(context.Background(), "anthropic", "claude-sonnet-4"); err == nil {
		t.Errorf("SetModel with no session = nil, want error")
	}
}

// ─── usage_update ────────────────────────────────────────────────

// TestTranslator_UsageUpdate_PopulatesLastUsage verifies the
// /usage_update event populates the translator's lastUsage field.
func TestTranslator_UsageUpdate_PopulatesLastUsage(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")
	props, _ := json.Marshal(map[string]any{
		"used": 53000,
		"size": 200000,
		"cost": map[string]any{"amount": 0.045, "currency": "USD"},
		"tokens": map[string]any{
			"input":  49000,
			"output": 4000,
			"cache":  map[string]any{"read": 1000, "write": 500},
		},
	})
	if err := tr.handleEvent(SessionEvent{Type: "usage_update", Properties: props}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	if tr.lastUsage == nil {
		t.Fatalf("lastUsage not populated")
	}
	if got := tr.lastUsage.InputTokens; got != 49000 {
		t.Errorf("InputTokens = %d, want 49000", got)
	}
	if got := tr.lastUsage.OutputTokens; got != 4000 {
		t.Errorf("OutputTokens = %d, want 4000", got)
	}
	if got := tr.lastUsage.CacheReadInputTokens; got != 1000 {
		t.Errorf("CacheReadInputTokens = %d, want 1000", got)
	}
	if got := tr.lastUsage.CostUSD; got != 0.045 {
		t.Errorf("CostUSD = %f, want 0.045", got)
	}
	if got := tr.lastUsage.ContextWindow; got != 200000 {
		t.Errorf("ContextWindow = %d, want 200000", got)
	}
}

// TestTranslator_SessionIdleCarriesUsage verifies session.idle
// emits Done.Usage with the last-seen usage_update payload.
func TestTranslator_SessionIdleCarriesUsage(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")

	// Push a usage_update first.
	usageProps, _ := json.Marshal(map[string]any{
		"used": 1000,
		"size": 200000,
		"tokens": map[string]any{"input": 800, "output": 200},
	})
	if err := tr.handleEvent(SessionEvent{Type: "usage_update", Properties: usageProps}); err != nil {
		t.Fatalf("usage_update: %v", err)
	}

	// Then session.idle — should carry the cached usage.
	idleProps, _ := json.Marshal(map[string]any{})
	if err := tr.handleEvent(SessionEvent{Type: "session.idle", Properties: idleProps}); err != nil {
		t.Fatalf("session.idle: %v", err)
	}
	if len(rec.evs) != 1 || rec.evs[0].Done == nil {
		t.Fatalf("expected one Done event, got %d", len(rec.evs))
	}
	if rec.evs[0].Done.Usage == nil {
		t.Fatalf("Done.Usage not carried on session.idle")
	}
	if got := rec.evs[0].Done.Usage.InputTokens; got != 800 {
		t.Errorf("Done.Usage.InputTokens = %d, want 800", got)
	}
	if got := rec.evs[0].Done.Usage.ContextWindow; got != 200000 {
		t.Errorf("Done.Usage.ContextWindow = %d, want 200000", got)
	}
}

// TestTranslator_UsageUpdateThenLaterOverrides verifies that the
// most recent usage_update wins on the next session.idle.
func TestTranslator_UsageUpdateThenLaterOverrides(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")

	first, _ := json.Marshal(map[string]any{"tokens": map[string]any{"input": 100}})
	_ = tr.handleEvent(SessionEvent{Type: "usage_update", Properties: first})

	second, _ := json.Marshal(map[string]any{"tokens": map[string]any{"input": 999}})
	_ = tr.handleEvent(SessionEvent{Type: "usage_update", Properties: second})

	idle, _ := json.Marshal(map[string]any{})
	_ = tr.handleEvent(SessionEvent{Type: "session.idle", Properties: idle})

	if got := rec.evs[0].Done.Usage.InputTokens; got != 999 {
		t.Errorf("Done.Usage.InputTokens = %d, want 999 (latest)", got)
	}
}

// ─── translate: tool name normalization ───────────────────────────

// TestNormalizeToolName covers the known aliases and the default
// capitalisation fallback.
func TestNormalizeToolName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"bash", "Bash"},
		{"read", "Read"},
		{"write", "Write"},
		{"edit", "Edit"},
		{"glob", "Glob"},
		{"grep", "Grep"},
		{"task", "Task"},
		{"webfetch", "WebFetch"},
		{"websearch", "WebSearch"},
		{"todowrite", "TodoWrite"},
		{"todoread", "TodoRead"},
		// Unknown slug → capitalise first letter.
		{"unknown", "Unknown"},
		{"mytool", "Mytool"},
		// Already canonical (mixed case) → pass through.
		{"MyTool", "MyTool"},
		// Empty stays empty.
		{"", ""},
	}
	for _, tc := range cases {
		got := normalizeToolName(tc.in)
		if got != tc.want {
			t.Errorf("normalizeToolName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTranslator_ToolPart_NormalizesName verifies the handleToolPart
// path applies the normalizer so the channel layer sees "Bash"
// instead of "bash".
func TestTranslator_ToolPart_NormalizesName(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")

	props, _ := json.Marshal(map[string]any{
		"part": map[string]any{
			"type":   "tool",
			"tool":   "bash",
			"callID": "call_1",
			"state":  map[string]any{"status": "pending", "input": map[string]any{"command": "ls"}},
		},
	})
	if err := tr.handleEvent(SessionEvent{Type: "message.part.updated", Properties: props}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	if len(rec.evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(rec.evs))
	}
	if got := rec.evs[0].ToolStart.Name; got != "Bash" {
		t.Errorf("ToolStart.Name = %q, want Bash", got)
	}
}

// TestTranslator_ToolPart_EndEmitsNormalizedName verifies the end
// event also uses the normalized name.
func TestTranslator_ToolPart_EndEmitsNormalizedName(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")

	// Drive pending → completed.
	pending, _ := json.Marshal(map[string]any{
		"part": map[string]any{
			"type":   "tool",
			"tool":   "read",
			"callID": "call_2",
			"state":  map[string]any{"status": "pending", "input": map[string]any{"path": "/tmp/x"}},
		},
	})
	_ = tr.handleEvent(SessionEvent{Type: "message.part.updated", Properties: pending})

	completed, _ := json.Marshal(map[string]any{
		"part": map[string]any{
			"type":   "tool",
			"tool":   "read",
			"callID": "call_2",
			"state":  map[string]any{"status": "completed", "output": "hello"},
		},
	})
	_ = tr.handleEvent(SessionEvent{Type: "message.part.updated", Properties: completed})

	if len(rec.evs) != 2 {
		t.Fatalf("expected 2 events, got %d", len(rec.evs))
	}
	if got := rec.evs[1].ToolEnd.Name; got != "Read" {
		t.Errorf("ToolEnd.Name = %q, want Read", got)
	}
	if got := rec.evs[1].ToolEnd.Output; got != "hello" {
		t.Errorf("ToolEnd.Output = %q, want hello", got)
	}
}

// ─── translate: current_mode_update / available_commands_update ───

// TestTranslator_CurrentModeUpdateEmitsReady verifies the
// current_mode_update event surfaces an EventAgentReady so the
// channel header refreshes.
func TestTranslator_CurrentModeUpdateEmitsReady(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")
	props, _ := json.Marshal(map[string]any{
		"currentModeId": "build",
	})
	if err := tr.handleEvent(SessionEvent{Type: "current_mode_update", Properties: props}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	if len(rec.evs) != 1 || rec.evs[0].Kind != agent.EventAgentReady {
		t.Errorf("event = %+v, want EventAgentReady", rec.evs)
	}
}

// TestTranslator_CurrentModeUpdateEmptyIgnored verifies an empty
// mode id does not emit a phantom EventAgentReady.
func TestTranslator_CurrentModeUpdateEmptyIgnored(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")
	props, _ := json.Marshal(map[string]any{"currentModeId": ""})
	if err := tr.handleEvent(SessionEvent{Type: "current_mode_update", Properties: props}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	if len(rec.evs) != 0 {
		t.Errorf("empty mode update should not deliver, got %v", rec.evs)
	}
}

// TestTranslator_AvailableCommandsUpdateDoesNotDeliver verifies
// the available_commands_update event is logged but does not emit
// any AgentEvent (stage 2 keeps the list internally for future use).
func TestTranslator_AvailableCommandsUpdateDoesNotDeliver(t *testing.T) {
	rec := &recorder{}
	tr := newTranslator(rec.deliver, "opencode", "/tmp", "main", "ses_1", "")
	props, _ := json.Marshal(map[string]any{
		"availableCommands": []map[string]any{
			{"name": "init", "description": "create a new session"},
			{"name": "undo", "description": "undo last action"},
		},
	})
	if err := tr.handleEvent(SessionEvent{Type: "available_commands_update", Properties: props}); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	if len(rec.evs) != 0 {
		t.Errorf("available_commands_update should not deliver, got %v", rec.evs)
	}
}

// ─── SendBlocks: image size guard ────────────────────────────────

// TestSendBlocks_ImageTooLarge verifies large images are rejected
// before the prompt is sent.
//
// Renamed in stage 5 to make room for the inline-base64 variant
// that captures the request body. The behaviour this used to
// verify is preserved by stage5_image_test.go's TestSendBlocks_ImageTooLarge
// (which additionally asserts no HTTP request was made).
func TestSendBlocks_ImageTooLarge_Stage2(t *testing.T) {
	// Stage 2 uses os.Stat to pre-check the file size. We
	// synthesize a "big" file by writing to a temp path.
	dir := t.TempDir()
	bigPath := dir + "/big.bin"
	f, err := os.Create(bigPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 1 byte over the limit.
	if _, err := f.Write(make([]byte, maxImageBytes+1)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = f.Close()

	a := &driver{
		name:        "opencode",
		server:      &serverProc{baseURL: ""},
		events:      make(chan agent.AgentEvent, 16),
		closed:      make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone:    make(chan struct{}),
		sessionID:   "ses_1",
	}
	err = a.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentImage, Path: bigPath, MediaType: "image/png"},
	})
	if err == nil {
		t.Fatalf("SendBlocks with oversized image = nil, want error")
	}
	if !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("error = %v, want ErrImageTooLarge", err)
	}
}

// TestSendBlocks_MissingImagePath verifies the bridge does not
// crash when ContentImage has an empty path.
func TestSendBlocks_MissingImagePath(t *testing.T) {
	a := &driver{
		name:        "opencode",
		server:      &serverProc{baseURL: ""},
		events:      make(chan agent.AgentEvent, 16),
		closed:      make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone:    make(chan struct{}),
		sessionID:   "ses_1",
	}
	// No active transport — call should short-circuit OK with
	// (empty parts → no-op).
	if err := a.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentImage, Path: "", MediaType: "image/png"},
	}); err != nil {
		t.Errorf("SendBlocks with empty path = %v, want nil", err)
	}
	// pendingTurnActive should NOT be set when nothing was sent.
	a.pendingMu.Lock()
	if a.pendingTurnActive {
		t.Errorf("pendingTurnActive set on no-op SendBlocks")
	}
	a.pendingMu.Unlock()
}
