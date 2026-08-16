// Package opencode — unit tests for the sessionUpdate →
// AgentEvent translator (update.go).
//
// All tests are pure-function tests: no live opencode binary,
// no mockTransport, no real PTY. They construct a SessionView
// backed by a buffered channel, then drive the translator with
// hand-crafted sessionUpdate JSON.
//
// Integration tests against a real `opencode acp` binary live in
// real_e2e_test.go (build-tagged opencode_real_e2e).
package opencode

import (
	"encoding/json"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
)

// captureView returns a SessionView whose Emit writes to a
// buffered channel and whose SessionID returns a fixed value.
//
// The view is intentionally minimal — the translator does not
// need access to the underlying driver.
func captureView(t *testing.T, sessionID string) (*acp.SessionView, chan agent.AgentEvent) {
	t.Helper()
	events := make(chan agent.AgentEvent, 32)
	view := &acp.SessionView{
		Emit:      func(ev agent.AgentEvent) { events <- ev },
		SessionID: func() string { return sessionID },
		AgentName: "opencode",
		Workspace: "/tmp/test",
	}
	return view, events
}

// TestHandleUpdate_AgentMessageChunk verifies a plain
// agent_message_chunk emits EventAgentText with the text payload
// verbatim (no "[思考] " prefix).
func TestHandleUpdate_AgentMessageChunk(t *testing.T) {
	view, events := captureView(t, "ses_test")
	h := newUpdateHandler("/tmp/test")

	raw := json.RawMessage(`{
		"sessionUpdate": "agent_message_chunk",
		"content": {"type": "text", "text": "Hello, world!"}
	}`)
	if err := h(view, raw); err != nil {
		t.Fatalf("handle() error = %v", err)
	}

	select {
	case ev := <-events:
		if ev.Kind != agent.EventAgentText {
			t.Fatalf("event kind = %v, want EventAgentText", ev.Kind)
		}
		if ev.Text != "Hello, world!" {
			t.Errorf("Text = %q, want %q", ev.Text, "Hello, world!")
		}
		if ev.SessionID != "ses_test" {
			t.Errorf("SessionID = %q, want ses_test", ev.SessionID)
		}
		if ev.AgentName != "opencode" {
			t.Errorf("AgentName = %q, want opencode", ev.AgentName)
		}
	default:
		t.Fatal("no event emitted")
	}
}

// TestHandleUpdate_AgentMessageChunk_EmptyTextDrops asserts
// that empty text produces no event (matches the
// opencode-serve bridge's contract — empty text chunks are
// noise).
func TestHandleUpdate_AgentMessageChunk_EmptyTextDrops(t *testing.T) {
	view, events := captureView(t, "ses_test")
	h := newUpdateHandler("/tmp/test")

	raw := json.RawMessage(`{
		"sessionUpdate": "agent_message_chunk",
		"content": {"type": "text", "text": ""}
	}`)
	if err := h(view, raw); err != nil {
		t.Fatalf("handle() error = %v", err)
	}

	select {
	case ev := <-events:
		t.Fatalf("expected no event for empty text, got %+v", ev)
	default:
		// good — no event
	}
}

// TestHandleUpdate_AgentThoughtChunk_PrependsThinkPrefix
// verifies agent_thought_chunk emits EventAgentText with the
// "[思考] " prefix so the channel renderer treats it as
// reasoning.
func TestHandleUpdate_AgentThoughtChunk_PrependsThinkPrefix(t *testing.T) {
	view, events := captureView(t, "ses_test")
	h := newUpdateHandler("/tmp/test")

	raw := json.RawMessage(`{
		"sessionUpdate": "agent_thought_chunk",
		"content": {"type": "text", "text": "thinking..."}
	}`)
	if err := h(view, raw); err != nil {
		t.Fatalf("handle() error = %v", err)
	}

	select {
	case ev := <-events:
		if ev.Kind != agent.EventAgentText {
			t.Fatalf("event kind = %v, want EventAgentText", ev.Kind)
		}
		if ev.Text != "[思考] thinking..." {
			t.Errorf("Text = %q, want %q", ev.Text, "[思考] thinking...")
		}
	default:
		t.Fatal("no event emitted")
	}
}

// TestHandleUpdate_UserMessageChunk_Drops asserts the replay of
// the user's own message (after session/load) does NOT emit an
// event — the channel already rendered the inbound.
func TestHandleUpdate_UserMessageChunk_Drops(t *testing.T) {
	view, events := captureView(t, "ses_test")
	h := newUpdateHandler("/tmp/test")

	raw := json.RawMessage(`{
		"sessionUpdate": "user_message_chunk",
		"content": {"type": "text", "text": "what I asked"}
	}`)
	if err := h(view, raw); err != nil {
		t.Fatalf("handle() error = %v", err)
	}

	select {
	case ev := <-events:
		t.Fatalf("expected no event for user_message_chunk, got %+v", ev)
	default:
		// good — dropped
	}
}

// TestHandleUpdate_ToolCall_EmitsStart verifies a tool_call
// emits EventAgentToolStart with the title and rawInput.
func TestHandleUpdate_ToolCall_EmitsStart(t *testing.T) {
	view, events := captureView(t, "ses_test")
	h := newUpdateHandler("/tmp/test")

	raw := json.RawMessage(`{
		"sessionUpdate": "tool_call",
		"toolCallId": "tc_1",
		"title": "Bash",
		"kind": "execute",
		"status": "pending",
		"rawInput": {"command": "ls -la"}
	}`)
	if err := h(view, raw); err != nil {
		t.Fatalf("handle() error = %v", err)
	}

	select {
	case ev := <-events:
		if ev.Kind != agent.EventAgentToolStart {
			t.Fatalf("event kind = %v, want EventAgentToolStart", ev.Kind)
		}
		if ev.ToolStart == nil {
			t.Fatal("ToolStart is nil")
		}
		if ev.ToolStart.ID != "tc_1" {
			t.Errorf("ID = %q, want tc_1", ev.ToolStart.ID)
		}
		if ev.ToolStart.Name != "Bash" {
			t.Errorf("Name = %q, want Bash", ev.ToolStart.Name)
		}
		if ev.ToolStart.Args != `{"command": "ls -la"}` {
			t.Errorf("Args = %q, want %q", ev.ToolStart.Args, `{"command": "ls -la"}`)
		}
	default:
		t.Fatal("no event emitted")
	}
}

// TestHandleUpdate_ToolCallUpdate_Completed_EmitsEnd verifies
// a tool_call_update with status=completed emits
// EventAgentToolEnd with the rawOutput.
func TestHandleUpdate_ToolCallUpdate_Completed_EmitsEnd(t *testing.T) {
	view, events := captureView(t, "ses_test")
	h := newUpdateHandler("/tmp/test")

	raw := json.RawMessage(`{
		"sessionUpdate": "tool_call_update",
		"toolCallId": "tc_1",
		"title": "Bash",
		"status": "completed",
		"rawOutput": {"stdout": "file.txt\n", "exitCode": 0}
	}`)
	if err := h(view, raw); err != nil {
		t.Fatalf("handle() error = %v", err)
	}

	select {
	case ev := <-events:
		if ev.Kind != agent.EventAgentToolEnd {
			t.Fatalf("event kind = %v, want EventAgentToolEnd", ev.Kind)
		}
		if ev.ToolEnd == nil {
			t.Fatal("ToolEnd is nil")
		}
		if ev.ToolEnd.ID != "tc_1" {
			t.Errorf("ID = %q, want tc_1", ev.ToolEnd.ID)
		}
		if ev.Err != nil {
			t.Errorf("Err = %v, want nil on completed", ev.Err)
		}
		if ev.ToolEnd.Output != `{"stdout": "file.txt\n", "exitCode": 0}` {
			t.Errorf("Output = %q", ev.ToolEnd.Output)
		}
	default:
		t.Fatal("no event emitted")
	}
}

// TestHandleUpdate_ToolCallUpdate_Failed_EmitsEndWithErr
// verifies a tool_call_update with status=failed emits
// EventAgentToolEnd with the top-level Err populated and
// rawOutput on ToolEnd.Output for diagnostic visibility.
func TestHandleUpdate_ToolCallUpdate_Failed_EmitsEndWithErr(t *testing.T) {
	view, events := captureView(t, "ses_test")
	h := newUpdateHandler("/tmp/test")

	raw := json.RawMessage(`{
		"sessionUpdate": "tool_call_update",
		"toolCallId": "tc_2",
		"title": "Bash",
		"status": "failed",
		"rawOutput": {"error": "permission denied"}
	}`)
	if err := h(view, raw); err != nil {
		t.Fatalf("handle() error = %v", err)
	}

	select {
	case ev := <-events:
		if ev.Kind != agent.EventAgentToolEnd {
			t.Fatalf("event kind = %v, want EventAgentToolEnd", ev.Kind)
		}
		if ev.Err == nil {
			t.Fatal("Err is nil; want non-nil on failed")
		}
		if ev.ToolEnd == nil || ev.ToolEnd.ID != "tc_2" {
			t.Errorf("ToolEnd = %+v, want ID=tc_2", ev.ToolEnd)
		}
	default:
		t.Fatal("no event emitted")
	}
}

// TestHandleUpdate_ToolCallUpdate_IntermediateStatus_LogsOnly
// verifies that tool_call_update statuses other than completed
// and failed (i.e. running, pending, and any future status)
// do not emit a runtime-visible event today; v2 may add
// EventAgentToolProgress.
func TestHandleUpdate_ToolCallUpdate_IntermediateStatus_LogsOnly(t *testing.T) {
	for _, status := range []string{"running", "pending", "unknown_future_status"} {
		t.Run(status, func(t *testing.T) {
			view, events := captureView(t, "ses_test")
			h := newUpdateHandler("/tmp/test")
			raw := json.RawMessage(`{
				"sessionUpdate": "tool_call_update",
				"toolCallId": "tc_3",
				"title": "Bash",
				"status": "` + status + `"
			}`)
			if err := h(view, raw); err != nil {
				t.Fatalf("handle() error = %v", err)
			}
			select {
			case ev := <-events:
				t.Fatalf("expected no event for status=%s, got %+v", status, ev)
			default:
				// good
			}
		})
	}
}

// TestHandleUpdate_UnknownKind_Tolerated verifies that
// sessionUpdate variants the opencode acp server does not
// currently emit (usage_update, available_commands_update,
// current_mode_update, config_option_update,
// session_info_update, plan, plus hypothetical future
// variants) do not break the bridge — every kind is logged
// and dropped, no event is emitted, no readpump abort.
func TestHandleUpdate_UnknownKind_Tolerated(t *testing.T) {
	for _, kind := range []string{
		"usage_update",
		"available_commands_update",
		"current_mode_update",
		"config_option_update",
		"session_info_update",
		"plan",
		"future_session_update_v2",
	} {
		t.Run(kind, func(t *testing.T) {
			view, events := captureView(t, "ses_test")
			h := newUpdateHandler("/tmp/test")
			raw := json.RawMessage(`{"sessionUpdate":"` + kind + `","someField":"..."}`)
			if err := h(view, raw); err != nil {
				t.Fatalf("handle() error = %v, want nil", err)
			}
			select {
			case ev := <-events:
				t.Fatalf("expected no event for %s, got %+v", kind, ev)
			default:
				// good
			}
		})
	}
}

// TestHandleUpdate_MalformedJSON_ReturnsError verifies the
// translator returns an error on a malformed update so the
// generic acp bridge logs it but keeps the stream alive (per
// F-OPENCODE-ACP-MIGRATION §4.4 — wire decoding stays tolerant).
func TestHandleUpdate_MalformedJSON_ReturnsError(t *testing.T) {
	view, _ := captureView(t, "ses_test")
	h := newUpdateHandler("/tmp/test")

	// Missing the sessionUpdate field AND missing the content
	// field. We need to trip a json.Unmarshal error. Since the
	// translator is permissive (treats absence as unknown kind),
	// supply a non-JSON payload that fails to unmarshal as
	// struct.
	raw := json.RawMessage(`{not-json`)
	if err := h(view, raw); err == nil {
		t.Fatal("handle() error = nil, want non-nil on malformed JSON")
	}
}

// TestDecodeTextChunk_NonTextTypeDrops verifies the helper
// drops non-text ContentChunk shapes (image / resource) —
// inline image rendering is a v2 concern.
func TestDecodeTextChunk_NonTextTypeDrops(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"text", `{"content":{"type":"text","text":"hi"}}`, "hi"},
		{"image", `{"content":{"type":"image","data":"..."}}`, ""},
		{"empty text", `{"content":{"type":"text","text":""}}`, ""},
		{"missing content", `{}`, ""},
		{"empty payload", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeTextChunk(json.RawMessage(tc.in))
			if got != tc.want {
				t.Errorf("decodeTextChunk(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

