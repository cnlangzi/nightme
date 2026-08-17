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
	h := newUpdateHandler("/tmp/test").asUpdateHandler()

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
	h := newUpdateHandler("/tmp/test").asUpdateHandler()

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
	h := newUpdateHandler("/tmp/test").asUpdateHandler()

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
	h := newUpdateHandler("/tmp/test").asUpdateHandler()

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
	h := newUpdateHandler("/tmp/test").asUpdateHandler()

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
	h := newUpdateHandler("/tmp/test").asUpdateHandler()

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
	h := newUpdateHandler("/tmp/test").asUpdateHandler()

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
			h := newUpdateHandler("/tmp/test").asUpdateHandler()
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
			h := newUpdateHandler("/tmp/test").asUpdateHandler()
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
	h := newUpdateHandler("/tmp/test").asUpdateHandler()

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


// ─── text-buffering tests (F-OPENCODE-ACP-MIGRATION §5.2) ──────
//
// The bridge accumulates agent_message_chunk payloads into a
// single per-turn buffer and only emits one EventAgentText per
// sentence / tool-boundary / explicit Flush — instead of one
// EventAgentText per token, which used to surface as a send_card
// per token in the chat channel (and, with the opencode bridge's
// pre-fix double-emit, doubled to two cards per token).
//
// These tests pin the buffering behaviour so future refactors do
// not regress it. The existing per-chunk-emit tests above still
// pass because their payloads happen to end with a terminator.

func agentMessageChunk(text string) json.RawMessage {
	return json.RawMessage(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":` + jsonQuote(text) + `}}`)
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestHandleUpdate_ChunksWithoutPunctuation_Accumulate verifies
// a run of agent_message_chunk updates that does NOT end with a
// sentence terminator only emits ONE EventAgentText once the
// flush trigger fires (sentence end / tool boundary / explicit
// Flush). Pre-fix the same sequence emitted N cards for N chunks.
func TestHandleUpdate_ChunksWithoutPunctuation_Accumulate(t *testing.T) {
	view, events := captureView(t, "ses_test")
	h := newUpdateHandler("/tmp/test")
	handle := h.asUpdateHandler()

	for _, chunk := range []string{"hello", " ", "world"} {
		if err := handle(view, agentMessageChunk(chunk)); err != nil {
			t.Fatalf("handle() error = %v", err)
		}
	}

	select {
	case ev := <-events:
		t.Fatalf("expected no event before flush trigger, got %+v", ev)
	default:
	}

	// Explicit drain — turn-end flush path. Mirrors what the
	// generic acp bridge invokes via SessionView.FlushPending
	// right before EventAgentDone.
	h.Flush(view)

	select {
	case ev := <-events:
		if ev.Kind != agent.EventAgentText {
			t.Fatalf("event kind = %v, want EventAgentText", ev.Kind)
		}
		if ev.Text != "hello world" {
			t.Errorf("Text = %q, want %q", ev.Text, "hello world")
		}
	default:
		t.Fatal("no event emitted after Flush")
	}
}

// TestHandleUpdate_ChunksEndingWithPunctuation_FlushOnSentence
// verifies the sentence-boundary trigger fires as soon as the
// accumulated text ends in ". ? ! 。 ！ ？". Mirrors the
// pi / dsh bridges' flush granularity contract.
func TestHandleUpdate_ChunksEndingWithPunctuation_FlushOnSentence(t *testing.T) {
	for _, tc := range []struct {
		name string
		chunks []string
		wantFlush1 string // emitted after the punctuation chunk
	}{
		{"ascii period", []string{"foo", " ", "bar", "."}, "foo bar."},
		{"ascii question", []string{"why", "?"}, "why?"},
		{"ascii bang", []string{"wow", "!"}, "wow!"},
		{"full-width period", []string{"你好", "世界", "。"}, "你好世界。"},
		{"full-width question", []string{"怎么了", "？"}, "怎么了？"},
		{"full-width bang", []string{"太棒了", "！"}, "太棒了！"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, events := captureView(t, "ses_test")
			h := newUpdateHandler("/tmp/test")
			handle := h.asUpdateHandler()
			for _, chunk := range tc.chunks {
				if err := handle(view, agentMessageChunk(chunk)); err != nil {
					t.Fatalf("handle() error = %v", err)
				}
			}
			select {
			case ev := <-events:
				if ev.Text != tc.wantFlush1 {
					t.Errorf("flushed text = %q, want %q", ev.Text, tc.wantFlush1)
				}
			default:
				t.Fatalf("expected flush on punctuation, got no event (buffer = %q)", h.textBuf.String())
			}
		})
	}
}

// TestHandleUpdate_ChunkSequenceAcrossTwoSentences pins that
// each sentence flushes independently (no double-flush, no
// missing flush).
func TestHandleUpdate_ChunkSequenceAcrossTwoSentences(t *testing.T) {
	view, events := captureView(t, "ses_test")
	h := newUpdateHandler("/tmp/test").asUpdateHandler()

	for _, chunk := range []string{"first sentence.", " ", "second sentence"} {
		if err := h(view, agentMessageChunk(chunk)); err != nil {
			t.Fatalf("handle() error = %v", err)
		}
	}
	// First sentence should have already flushed; the third
	// chunk (no terminator yet) keeps "second sentence" in the
	// buffer.
	select {
	case ev := <-events:
		if ev.Text != "first sentence." {
			t.Errorf("first flush = %q, want %q", ev.Text, "first sentence. ")
		}
	default:
		t.Fatal("expected first flush after first sentence")
	}
	select {
	case ev := <-events:
		t.Fatalf("expected no further event until second sentence ends, got %+v", ev)
	default:
	}

	// Complete the second sentence.
	if err := h(view, agentMessageChunk(".")); err != nil {
		t.Fatalf("handle() error = %v", err)
	}
	select {
	case ev := <-events:
		if ev.Text != "second sentence." {
			t.Errorf("second flush = %q, want %q", ev.Text, "second sentence.")
		}
	default:
		t.Fatal("expected second flush after sentence end")
	}
}

// TestHandleUpdate_ToolCallAfterText_FlushesBuffer verifies the
// tool-boundary pre-flush in handle() drains the buffered
// reply text BEFORE the tool event is emitted (F-52 invariant).
func TestHandleUpdate_ToolCallAfterText_FlushesBuffer(t *testing.T) {
	view, events := captureView(t, "ses_test")
	h := newUpdateHandler("/tmp/test")
	handle := h.asUpdateHandler()

	if err := handle(view, agentMessageChunk("partial answer")); err != nil {
		t.Fatalf("handle() error = %v", err)
	}
	// Drain pending: tool-boundary
	toolRaw := json.RawMessage(`{"sessionUpdate":"tool_call","toolCallId":"tc_1","title":"Bash","kind":"execute","status":"pending","rawInput":{"command":"ls"}}`)
	if err := handle(view, toolRaw); err != nil {
		t.Fatalf("handle() error = %v", err)
	}

	// First event from channel must be the buffered text.
	select {
	case ev := <-events:
		if ev.Kind != agent.EventAgentText || ev.Text != "partial answer" {
			t.Fatalf("first event = %+v, want EventAgentText/partial answer", ev)
		}
	default:
		t.Fatalf("expected buffered text flush on tool boundary; buf = %q", h.textBuf.String())
	}
	select {
	case ev := <-events:
		if ev.Kind != agent.EventAgentToolStart {
			t.Fatalf("second event kind = %v, want EventAgentToolStart", ev.Kind)
		}
	default:
		t.Fatal("expected EventAgentToolStart after buffered text flush")
	}
}

// TestFlush_EmptyBufferIsNoop verifies Flush on an empty buffer
// does NOT emit an EventAgentText (so the chat channel never
// receives a stray "" / whitespace card at turn-end).
func TestFlush_EmptyBufferIsNoop(t *testing.T) {
	view, events := captureView(t, "ses_test")
	h := newUpdateHandler("/tmp/test")
	h.Flush(view)
	select {
	case ev := <-events:
		t.Fatalf("expected no event on empty flush, got %+v", ev)
	default:
	}
}

// TestEndsWithSentencePunctuation exhaustively checks the
// terminator set (ASCII + full-width + whitespace tolerance).
func TestEndsWithSentencePunctuation(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want bool
	}{
		{"", false},
		{"hello", false},
		{"hello.", true},
		{"hello?", true},
		{"hello!", true},
		{"hello。", true},
		{"hello？", true},
		{"hello！", true},
		{"hello. ", true},   // trailing whitespace tolerated
		{"hello", false},    // no terminator
		{"hello...", true},  // ellipsis still ends with "."
	} {
		t.Run(tc.s, func(t *testing.T) {
			if got := endsWithSentencePunctuation(tc.s); got != tc.want {
				t.Errorf("endsWithSentencePunctuation(%q) = %v, want %v", tc.s, got, tc.want)
			}
		})
	}
}
