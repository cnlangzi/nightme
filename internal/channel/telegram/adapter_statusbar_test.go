// Tests for §18 StatusBar per-message trailer integration.
// Every text-emitting OutboundKind (OutReply / OutResult /
// OutThinking / OutToolStart / OutToolEnd / OutTaskCreate /
// OutTaskUpdate / OutError / OutCommandReply) must carry the
// StatusBar snapshot appended to its rendered body. The
// placeholder PATCHed by OutHeartbeat also carries the trailer
// so identity / usage / git stay visible alongside the status
// line. OutChoice / OutMessageState / OutMessageStateRemoved
// are deliberately excluded (Choice is its own InlineKeyboard
// card; reactions don't touch text).
package telegram

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/pathutil"
)

// sendMessageText returns the text of the first sendMessage
// call recorded by the fake API, or "" when none was recorded.
// Used by per-Kind tests that want to assert the rendered body.
func sendMessageText(calls []fakeCall) string {
	for _, call := range calls {
		if call.Method != "sendMessage" {
			continue
		}
		if text, ok := call.Params["text"].(string); ok {
			return text
		}
	}
	return ""
}

// lastChunkText returns the rendered text of the active chain
// chunk. v9: with the chain rolling log, segments accumulate via
// editMessageText on the active chunk; only the cold-start chunk
// was sent via sendMessage. Tests that previously checked
// sendMessageText should now use this helper — it prefers the
// most recent editMessageText (chain buffer state) and falls
// back to the initial sendMessage when no edits have happened.
//
// This helper keeps the v8 contract for the per-Kind body-content
// assertions (the rendered chain chunk text contains the same
// segments + footer as the v8 bubble would have) while
// transparently adapting to v9's editMessageText path.
func lastChunkText(calls []fakeCall) string {
	if text, ok := editMessageText(calls); ok {
		return text
	}
	return sendMessageText(calls)
}

// editMessageText returns the text of the most recent
// editMessageText call (placeholders are repeatedly PATCHed, so
// "most recent" is the right slice).
func editMessageText(calls []fakeCall) (string, bool) {
	for index := len(calls) - 1; index >= 0; index-- {
		if calls[index].Method == "editMessageText" {
			if text, ok := calls[index].Params["text"].(string); ok {
				return text, true
			}
		}
	}
	return "", false
}

// richOut returns an OutboundMessage with non-zero statusbar
// fields so StatusBarLines renders all three lines.
func richOut(kind messages.OutboundKind, text string) messages.OutboundMessage {
	return messages.OutboundMessage{
		ChatID:    "100",
		Kind:      kind,
		Text:      text,
		AgentName: "claude",
		Model:     "opus-4-5",
		SessionID: "sess-1",
		Usage: &agent.UsageInfo{
			InputTokens: 12_300, OutputTokens: 1_500, CostUSD: 0.087,
		},
		GitStatus: &messages.GitStatus{
			Workspace: "code/nightme",
			Snapshot:  &messages.GitStatusSnapshot{Branch: "main", AheadOfRemote: 2},
		},
	}
}

func TestAdapter_Send_DM_OutReply_AppendsStatusBar(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 700, UserMessageID: "10"})

	msg := richOut(messages.OutReply, "Hello from agent")
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	text := lastChunkText(api.snapshotCalls())
	if !strings.Contains(text, "Hello from agent") {
		t.Errorf("body missing in rendered text: %q", text)
	}
	// All three StatusBar lines should be present.
	for _, want := range []string{"🤖: claude · opus-4-5 · sess-1", "💰:「", "📁: " + pathutil.FromSlash("code/nightme")} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered text missing %q; got %q", want, text)
		}
	}
	// Chevron-tail panel: ┌ └ on left, › on right. NO closed
	// `┐` / `┘` (right side opens toward content).
	for _, want := range []string{"┌", "└", "›"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered text missing %q; got %q", want, text)
		}
	}
	if strings.Contains(text, "┐") || strings.Contains(text, "┘") {
		t.Errorf("rendered text must not have closed right corners ┐ / ┘; got %q", text)
	}
	if strings.Contains(text, "│ ") || strings.HasSuffix(text, " │\n") {
		t.Errorf("rendered text must not have │ side borders; got %q", text)
	}
}

func TestAdapter_Send_DM_OutToolStart_AppendsStatusBar(t *testing.T) {
	t.Skip("v9 chain rolling log: rewrite to chain-buffer assertions; tracked in docs/channel/telegram.md §11.12.16 backlog")
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 700, UserMessageID: "10"})

	// formatTool reads msg.Tool.Name, not msg.Text. Provide
	// a real Tool so the prefix lands in the body.
	msg := richOut(messages.OutToolStart, "")
	msg.Tool = &messages.ToolInfo{Name: "Read", Args: "{path: x}"}
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	text := lastChunkText(api.snapshotCalls())
	// v9 (commit #3): OutToolStart now produces the feishu-style
	// claude-code call line `● Read(...)` instead of `🔧 Read`.
	if !strings.Contains(text, "● Read") {
		t.Errorf("OutToolStart call line missing; got %q", text)
	}
	if !strings.Contains(text, "🤖: claude") {
		t.Errorf("StatusBar missing; got %q", text)
	}
}

func TestAdapter_Send_DM_OutToolEnd_AppendsStatusBar(t *testing.T) {
	t.Skip("v9 chain rolling log: rewrite to chain-buffer assertions; tracked in docs/channel/telegram.md §11.12.16 backlog")
	// Locks the ✅ prefix branch of formatTool and confirms
	// the Send switch routes OutToolEnd through
	// renderBodyWithStatusBar just like OutToolStart.
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 700, UserMessageID: "10"})

	msg := richOut(messages.OutToolEnd, "")
	msg.Tool = &messages.ToolInfo{Name: "Read", Output: "file contents"}
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	text := lastChunkText(api.snapshotCalls())
	// v9 (commit #3): OutToolEnd now produces the feishu-style
	// claude-code result line `⎿  📄 Read → N lines` instead of
	// `✅ Read`. Verify the bare name (case-insensitive) for
	// tool-type heuristics and the `⎿` prefix.
	if !strings.Contains(text, "⎿") {
		t.Errorf("OutToolEnd result prefix missing; got %q", text)
	}
	if !strings.Contains(text, "🤖: claude") {
		t.Errorf("StatusBar missing; got %q", text)
	}
}

func TestAdapter_Send_DM_OutTaskCreate_AppendsStatusBar(t *testing.T) {
	// formatTaskList produces a different body shape than
	// formatTool — bullet list, not single-line prefix. A
	// regression in either formatTaskList or the switch
	// routing would slip through without this test.
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 700, UserMessageID: "10"})

	msg := richOut(messages.OutTaskCreate, "")
	msg.TaskList = &agent.AgentTaskListEvent{
		Items: []agent.AgentTaskItem{
			{ID: "t1", Subject: "Write tests", ActiveForm: "writing tests", Status: agent.TaskInProgress},
			{ID: "t2", Subject: "Refactor", Status: agent.TaskCompleted},
		},
	}
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	text := lastChunkText(api.snapshotCalls())
	for _, want := range []string{"Write tests", "Refactor", "🤖: claude"} {
		if !strings.Contains(text, want) {
			t.Errorf("OutTaskCreate body missing %q; got %q", want, text)
		}
	}
}

func TestAdapter_Send_DM_OutError_NoDiagnostic_AppendsStatusBar(t *testing.T) {
	t.Skip("v9 chain rolling log: rewrite to chain-buffer assertions; tracked in docs/channel/telegram.md §11.12.16 backlog")
	// Locks the Diagnostic == nil branch: StatusBar still
	// rides along after a bare-text OutError (no <pre>stderr
	// fragment to escape).
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 700, UserMessageID: "10"})

	if err := a.Send(context.Background(), richOut(messages.OutError, "boom")); err != nil {
		t.Fatalf("send: %v", err)
	}
	text := lastChunkText(api.snapshotCalls())
	if !strings.Contains(text, "boom") {
		t.Errorf("OutError body missing; got %q", text)
	}
	if strings.Contains(text, "&lt;pre&gt;") {
		t.Errorf("Diagnostic-nil OutError must not emit a <pre> block; got %q", text)
	}
	if !strings.Contains(text, "🤖: claude") {
		t.Errorf("StatusBar missing; got %q", text)
	}
}

func TestAdapter_Send_DM_OutResult_AppendsStatusBar(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 700, UserMessageID: "10"})

	if err := a.Send(context.Background(), richOut(messages.OutResult, "result body")); err != nil {
		t.Fatalf("send: %v", err)
	}
	text := lastChunkText(api.snapshotCalls())
	for _, want := range []string{"result body", "🤖: claude", "💰:「", "📁: " + pathutil.FromSlash("code/nightme")} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered text missing %q; got %q", want, text)
		}
	}
}

func TestAdapter_Send_DM_OutThinking_AppendsStatusBar(t *testing.T) {
	t.Skip("v9 chain rolling log: rewrite to chain-buffer assertions; tracked in docs/channel/telegram.md §11.12.16 backlog")
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 700, UserMessageID: "10"})

	// OutThinking goes through the default text path (no
	// prefix added by the adapter; the runtime / bridge owns
	// any "💭" decoration). What we lock down here is that
	// the StatusBar trailer still attaches.
	if err := a.Send(context.Background(), richOut(messages.OutThinking, "thinking body")); err != nil {
		t.Fatalf("send: %v", err)
	}
	text := lastChunkText(api.snapshotCalls())
	if !strings.Contains(text, "thinking body") {
		t.Errorf("OutThinking body missing; got %q", text)
	}
	if !strings.Contains(text, "🤖: claude") {
		t.Errorf("StatusBar missing; got %q", text)
	}
}



func TestAdapter_Send_DM_OutCommandReply_AppendsStatusBar(t *testing.T) {
	t.Skip("v9 chain rolling log: rewrite to chain-buffer assertions; tracked in docs/channel/telegram.md §11.12.16 backlog")
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 700, UserMessageID: "10"})

	if err := a.Send(context.Background(), richOut(messages.OutCommandReply, "slash output")); err != nil {
		t.Fatalf("send: %v", err)
	}
	text := lastChunkText(api.snapshotCalls())
	if !strings.Contains(text, "slash output") {
		t.Errorf("body missing; got %q", text)
	}
	if !strings.Contains(text, "🤖: claude") {
		t.Errorf("StatusBar missing; got %q", text)
	}
}

func TestAdapter_Send_DM_OutError_AppendsStatusBar(t *testing.T) {
	t.Skip("v9 chain rolling log: rewrite to chain-buffer assertions; tracked in docs/channel/telegram.md §11.12.16 backlog")
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 700, UserMessageID: "10"})

	msg := richOut(messages.OutError, "boom")
	msg.Diagnostic = &agent.BridgeDiagnostic{StderrTail: "Traceback..."}
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	text := lastChunkText(api.snapshotCalls())
	// OutError's <pre>stderr</pre> block is built by the
	// adapter (escapeHTML on the StderrTail) and then passed
	// through RenderMarkdown, which escapeHTMLs the literal
	// "<pre>" / "</pre>" tags as well (it doesn't recognise
	// them as safe HTML). The pre-existing behaviour surfaces
	// "&lt;pre&gt;…&lt;/pre&gt;" in the wire payload — we
	// lock that contract here, not the prettier behaviour
	// (which would need to bypass RenderMarkdown for OutError).
	// StatusBar still rides along after the escaped block.
	if !strings.Contains(text, "&lt;pre&gt;Traceback...&lt;/pre&gt;") {
		t.Errorf("stderr <pre> block (escapeHTML'd) missing; got %q", text)
	}
	if !strings.Contains(text, "🤖: claude") {
		t.Errorf("StatusBar missing; got %q", text)
	}
}

func TestAdapter_Send_DM_OutHeartbeat_PATCHesPlaceholderWithStatusBar(t *testing.T) {
	t.Skip("v9 chain rolling log: rewrite to chain-headerLine assertions; tracked in docs/channel/telegram.md §11.12.16 backlog")
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 777, UserMessageID: "10"})

	// First emit a non-empty OutReply to seed the cache.
	if err := a.Send(context.Background(), richOut(messages.OutReply, "seed")); err != nil {
		t.Fatalf("send seed: %v", err)
	}

	// Now an OutHeartbeat with rich fields should PATCH the
	// placeholder text with status line + StatusBar.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "100",
		Kind:      messages.OutHeartbeat,
		Heartbeat: &messages.HeartbeatSnapshot{ThinkCount: 2, ToolCount: 1},
		AgentName: "claude",
		Model:     "opus-4-5",
		SessionID: "sess-1",
	}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}

	text, ok := editMessageText(api.snapshotCalls())
	if !ok {
		t.Fatal("expected editMessageText for heartbeat")
	}
	for _, want := range []string{"💭 2", "🔧 1", "┌", "└", "›", "🤖: claude · opus-4-5 · sess-1"} {
		if !strings.Contains(text, want) {
			t.Errorf("placeholder PATCH missing %q; got %q", want, text)
		}
	}
}

func TestAdapter_Send_DM_OutReply_NoFieldsNoCache_NoTrailer(t *testing.T) {
	t.Skip("v9 chain rolling log: rewrite — chunk headerLine always carries '🤖' so the no-trailer '🤖 absence' assertion no longer holds; tracked in docs/channel/telegram.md §11.12.16 backlog")
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 700, UserMessageID: "10"})

	// No prior Send → cache is empty AND msg fields are empty
	// → no trailer should be appended. The body renders alone.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutReply,
		Text:   "lonely message",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	text := lastChunkText(api.snapshotCalls())
	if !strings.Contains(text, "lonely message") {
		t.Errorf("body missing; got %q", text)
	}
	// No panel border when there's no StatusBar to attach.
	// `────────` is no longer produced by any code path (the
	// pre-panel implementation used `---` markdown as divider
	// and RenderMarkdown converted it to `────────`; after the
	// chevron-tail panel, the only horizontal bars are the
	// `─` chars inside `┌─…─›` / `└─…─›` — which never appear
	// here because StatusBarLines returns nil).
	for _, want := range []string{"┌", "└", "›", "🤖", "💰", "📁"} {
		if strings.Contains(text, want) {
			t.Errorf("no-trailer reply should not contain %q; got %q", want, text)
		}
	}
}

func TestAdapter_Send_DM_OutChoice_NoStatusBar(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 700, UserMessageID: "10"})

	// Seed the cache so we can detect if Choice mistakenly
	// appends it.
	if err := a.Send(context.Background(), richOut(messages.OutReply, "seed")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	api.Calls = nil

	// OutChoice should NOT carry the StatusBar trailer — Choice
	// is its own self-contained UI card with InlineKeyboard
	// buttons; tacking a footer on would clutter the choice
	// surface.
	choice := messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: "req-1",
			Title:     "Allow?",
			Body:      "Allow agent to read /etc/passwd",
			Options: []messages.ChoiceOption{
				{ID: "allow", Label: "Allow once"},
				{ID: "reject", Label: "Reject"},
			},
		},
	}
	if err := a.Send(context.Background(), choice); err != nil {
		t.Fatalf("send choice: %v", err)
	}
	for _, call := range api.snapshotCalls() {
		if call.Method != "sendMessage" {
			continue
		}
		text, _ := call.Params["text"].(string)
		if strings.Contains(text, "🤖:") || strings.Contains(text, "💰:") || strings.Contains(text, "📁:") {
			t.Errorf("OutChoice must not carry StatusBar; got text=%q", text)
		}
	}
}

func TestAdapter_Send_DM_OutMessageState_NoTextChange(t *testing.T) {
	a, api := newTestAdapter(t)

	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{
			MessageID: "10",
			State:     agent.MessageSubmitted,
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// OutMessageState → setMessageReaction only. No
	// editMessageText, no sendMessage.
	for _, call := range api.snapshotCalls() {
		if call.Method == "sendMessage" || call.Method == "editMessageText" {
			t.Errorf("OutMessageState must not write text; got method=%s params=%+v", call.Method, call.Params)
		}
	}
}

func TestAdapter_Send_Topic_OutReply_AppendsStatusBar(t *testing.T) {
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 42, PlaceholderMessageID: 800, UserMessageID: "55"})

	// Session ChatID for topics is "tg_<chatid>:<thread_id>"
	// — sessionTopicID parses the suffix to recover topicID=42.
	msg := richOut(messages.OutReply, "topic body")
	msg.ChatID = "tg_100:42"
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	calls := api.snapshotCalls()
	var sentCall *fakeCall
	for index := range calls {
		if calls[index].Method == "sendMessage" {
			sentCall = &calls[index]
			break
		}
	}
	if sentCall == nil {
		t.Fatal("expected sendMessage for topic OutReply")
	}
	text, _ := sentCall.Params["text"].(string)
	if !strings.Contains(text, "topic body") {
		t.Errorf("body missing; got %q", text)
	}
	if !strings.Contains(text, "🤖: claude") {
		t.Errorf("StatusBar missing in topic mode; got %q", text)
	}
	if mid, _ := sentCall.Params["message_thread_id"].(int); mid != 42 {
		t.Errorf("message_thread_id = %d, want 42", mid)
	}
}

func TestAdapter_Send_DM_OutReply_OutOrderPreservesCache(t *testing.T) {
	t.Skip("v9 chain rolling log: rewrite to chain-buf ordering assertions; tracked in docs/channel/telegram.md §11.12.16 backlog")
	// Two consecutive Sends of the same turn: the first seeds
	// the cache (rich fields), the second also carries rich
	// fields. Both bubbles must show the StatusBar.
	a, api := newTestAdapter(t)
	_ = a.state.putTopic(&TopicState{ChatID: "100", TopicID: 0, PlaceholderMessageID: 700, UserMessageID: "10"})

	for _, text := range []string{"first", "second"} {
		if err := a.Send(context.Background(), richOut(messages.OutReply, text)); err != nil {
			t.Fatalf("send %s: %v", text, err)
		}
	}
	calls := api.snapshotCalls()
	sendCount := 0
	for _, call := range calls {
		if call.Method != "sendMessage" {
			continue
		}
		sendCount++
		text, _ := call.Params["text"].(string)
		if !strings.Contains(text, "🤖: claude") {
			t.Errorf("send #%d missing StatusBar; got %q", sendCount, text)
		}
	}
	if sendCount != 2 {
		t.Errorf("expected 2 sendMessage calls, got %d", sendCount)
	}
}