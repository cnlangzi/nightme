package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// renderedMock records every Feishu side-effect the receipt / renderer
// performs. It implements the receiptBot interface so a real
// MessageReceipt can drive it.
type renderedMock struct {
	mu sync.Mutex

	added     []receiptSwapCall       // (msgID, emoji) AddReaction calls
	deleted   []string                // reaction IDs deleted
	updated   []receiptSwapUpdateCall // (msgID, text) UpdateMessage calls
	sentText  []receiptSwapSendCall   // SendMessageText calls
	addErr    error                   // returned by AddReaction
	updateErr error                   // returned by UpdateMessage
	sendErr   error                   // returned by SendMessageText

	// nextReactionID auto-increments so the returned IDs are
	// unique and stable across calls within one test.
	nextReactionID int
}

func (m *renderedMock) AddReaction(_ context.Context, msgID, emoji string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.added = append(m.added, receiptSwapCall{msgID, emoji})
	if m.addErr != nil {
		return "", m.addErr
	}
	m.nextReactionID++
	return "rid_" + emoji + "_" + itoa(m.nextReactionID), nil
}

func (m *renderedMock) DeleteReaction(_ context.Context, msgID, rid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, msgID+":"+rid)
	return nil
}

func (m *renderedMock) UpdateMessage(_ context.Context, msgID, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updated = append(m.updated, receiptSwapUpdateCall{msgID, text})
	if m.updateErr != nil {
		return m.updateErr
	}
	return nil
}

func (m *renderedMock) SendMessageText(_ context.Context, chatID, text string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sentText = append(m.sentText, receiptSwapSendCall{chatID, text})
	if m.sendErr != nil {
		return "", m.sendErr
	}
	return "first_reply_id", nil
}

// updatedTexts returns just the text bodies of every UpdateMessage
// call in order — convenient for asserting on the rolling log.
func (m *renderedMock) updatedTexts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.updated))
	for i, u := range m.updated {
		out[i] = u.Text
	}
	return out
}

// lastUpdatedText returns the text of the most recent UpdateMessage
// call, or "" if none have happened yet.
func (m *renderedMock) lastUpdatedText() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.updated) == 0 {
		return ""
	}
	return m.updated[len(m.updated)-1].Text
}

// newRendererWithMock builds a fresh Renderer backed by a fresh
// renderedMock and a pre-existing receipt for "oc_chat". The
// returned mock can be inspected for the Feishu side-effects.
func newRendererWithMock() (*Renderer, *renderedMock) {
	mock := &renderedMock{}
	r := &Renderer{
		adapter:  nil, // render_test doesn't need the adapter
		receipts: make(map[string]*MessageReceipt),
	}
	// Seed the renderer with one active receipt so events have a
	// target. The receipt uses the mock via its bot field; we
	// hand-roll it (rather than NewMessageReceipt) because that
	// would call SendMessageText and addReaction, which we want
	// the test to control.
	r.receipts["oc_chat"] = &MessageReceipt{
		chatID:     "oc_chat",
		userMsgID:  "user_msg_1",
		replyMsgID: "first_reply_id",
		bot:        mock,
		logger:     discardLogger(),
		state:      StateExecuting,
	}
	return r, mock
}

// discardLogger returns a slog.Logger that drops all output — tests
// don't pollute test logs with the receipt's normal info/warn
// messages.
func discardLogger() *slog.Logger {
	return slog.New(&slogDiscard{})
}

type slogDiscard struct{ mu sync.Mutex }

func (s *slogDiscard) Enabled(_ context.Context, _ slog.Level) bool {
	return false
}
func (s *slogDiscard) Handle(_ context.Context, _ slog.Record) error {
	return nil
}
func (s *slogDiscard) WithAttrs(_ []slog.Attr) slog.Handler { return s }
func (s *slogDiscard) WithGroup(_ string) slog.Handler      { return s }

// TestRender_Text_AppendsToReceipt verifies that EventText no longer
// sends a separate Feishu message. Instead it appends to the active
// receipt's log and the receipt re-renders via UpdateMessage.
func TestRender_Text_AppendsToReceipt(t *testing.T) {
	r, mock := newRendererWithMock()

	if err := r.RenderEvent(context.Background(), "oc_chat", agent.AgentEvent{
		Kind: agent.EventText,
		Text: "hello",
	}); err != nil {
		t.Fatalf("RenderEvent: %v", err)
	}

	if got := len(mock.sentText); got != 0 {
		t.Errorf("RenderEvent for EventText must NOT send a new message; sent %d", got)
	}
	texts := mock.updatedTexts()
	if len(texts) != 1 {
		t.Fatalf("UpdateMessage calls = %d, want 1", len(texts))
	}
	if !strings.Contains(texts[0], "💬") || !strings.Contains(texts[0], "hello") {
		t.Errorf("log = %q, want 💬 hello", texts[0])
	}
}

// TestRender_Text_Thinking verifies that the claudecode bridge's
// "[思考] " prefix is honoured — thinking blocks become 💭 entries
// in the log, not 💬 reply entries.
func TestRender_Text_Thinking(t *testing.T) {
	r, mock := newRendererWithMock()

	if err := r.RenderEvent(context.Background(), "oc_chat", agent.AgentEvent{
		Kind: agent.EventText,
		Text: "[思考] exploring the workspace structure first",
	}); err != nil {
		t.Fatalf("RenderEvent: %v", err)
	}

	texts := mock.updatedTexts()
	if len(texts) != 1 {
		t.Fatalf("UpdateMessage calls = %d, want 1", len(texts))
	}
	if !strings.Contains(texts[0], "💭") {
		t.Errorf("thinking log = %q, want 💭 prefix", texts[0])
	}
	if strings.Contains(texts[0], "💬") {
		t.Errorf("thinking log must not contain 💬: %q", texts[0])
	}
	if !strings.Contains(texts[0], "exploring the workspace") {
		t.Errorf("thinking text missing: %q", texts[0])
	}
}

// TestRender_NoReceipt verifies orphan events (no active receipt for
// the chat) are silently dropped rather than sent as standalone
// messages. Common during boot or after a chat was force-closed.
func TestRender_NoReceipt(t *testing.T) {
	mock := &renderedMock{}
	r := &Renderer{
		adapter:  nil,
		receipts: make(map[string]*MessageReceipt),
	}

	if err := r.RenderEvent(context.Background(), "oc_unknown", agent.AgentEvent{
		Kind: agent.EventText,
		Text: "hello",
	}); err != nil {
		t.Fatalf("RenderEvent: %v", err)
	}
	if got := len(mock.sentText); got != 0 {
		t.Errorf("sent %d messages, want 0 (no receipt)", got)
	}
	if got := len(mock.updated); got != 0 {
		t.Errorf("UpdateMessage calls = %d, want 0", got)
	}
}

// TestRender_ToolStart verifies tool invocations append a 🔧 entry to
// the receipt's log.
func TestRender_ToolStart(t *testing.T) {
	r, mock := newRendererWithMock()

	if err := r.RenderEvent(context.Background(), "oc_chat", agent.AgentEvent{
		Kind:      agent.EventToolStart,
		ToolStart: &agent.ToolStartEvent{Name: "Read", Args: "/tmp/foo.go"},
	}); err != nil {
		t.Fatalf("RenderEvent: %v", err)
	}

	texts := mock.updatedTexts()
	if len(texts) != 1 {
		t.Fatalf("UpdateMessage calls = %d, want 1", len(texts))
	}
	if !strings.Contains(texts[0], "🔧") || !strings.Contains(texts[0], "Read") {
		t.Errorf("tool-start log = %q, want 🔧 Read(...)", texts[0])
	}
}

// TestRender_ToolEnd_Success verifies a successful tool result
// appends a ✅ entry that includes the tool's output so the user
// can tell what the agent actually did.
func TestRender_ToolEnd_Success(t *testing.T) {
	r, mock := newRendererWithMock()

	if err := r.RenderEvent(context.Background(), "oc_chat", agent.AgentEvent{
		Kind: agent.EventToolEnd,
		ToolEnd: &agent.ToolEndEvent{
			Name:   "Read",
			Output: "47 lines, handler at L42",
		},
	}); err != nil {
		t.Fatalf("RenderEvent: %v", err)
	}

	texts := mock.updatedTexts()
	if len(texts) != 1 {
		t.Fatalf("UpdateMessage calls = %d, want 1", len(texts))
	}
	got := texts[0]
	if !strings.Contains(got, "✅") {
		t.Errorf("tool-end log missing ✅: %q", got)
	}
	if !strings.Contains(got, "Read") {
		t.Errorf("tool-end log missing tool name: %q", got)
	}
	if !strings.Contains(got, "47 lines") {
		t.Errorf("tool-end log missing the output summary — user sees only a useless template: %q", got)
	}
}

// TestRender_ToolEnd_NoOutput verifies the legacy fallback when the
// bridge forgets to populate Output. We still render a useful
// "Read done" line — just not as informative as the Output path.
func TestRender_ToolEnd_NoOutput(t *testing.T) {
	r, mock := newRendererWithMock()

	if err := r.RenderEvent(context.Background(), "oc_chat", agent.AgentEvent{
		Kind:    agent.EventToolEnd,
		ToolEnd: &agent.ToolEndEvent{Name: "Read"},
	}); err != nil {
		t.Fatalf("RenderEvent: %v", err)
	}

	texts := mock.updatedTexts()
	if len(texts) != 1 {
		t.Fatalf("UpdateMessage calls = %d, want 1", len(texts))
	}
	if !strings.Contains(texts[0], "Read done") {
		t.Errorf("tool-end fallback = %q, want 'Read done'", texts[0])
	}
}

// TestRender_ToolEnd_Failure verifies a failed tool result appends
// a ❌ entry with the error text.
func TestRender_ToolEnd_Failure(t *testing.T) {
	r, mock := newRendererWithMock()

	if err := r.RenderEvent(context.Background(), "oc_chat", agent.AgentEvent{
		Kind:    agent.EventToolEnd,
		ToolEnd: &agent.ToolEndEvent{Name: "Bash", Err: errors.New("exit status 2")},
	}); err != nil {
		t.Fatalf("RenderEvent: %v", err)
	}

	texts := mock.updatedTexts()
	if len(texts) != 1 {
		t.Fatalf("UpdateMessage calls = %d, want 1", len(texts))
	}
	if !strings.Contains(texts[0], "❌") || !strings.Contains(texts[0], "exit status 2") {
		t.Errorf("tool-fail log = %q, want ❌ + error text", texts[0])
	}
}

// TestRender_Error verifies a fatal session error appends a ❌ entry
// AND transitions the receipt to Completed (terminal).
func TestRender_Error(t *testing.T) {
	r, mock := newRendererWithMock()

	if err := r.RenderEvent(context.Background(), "oc_chat", agent.AgentEvent{
		Kind:  agent.EventError,
		Error: &agent.ErrorEvent{Err: errors.New("agent died")},
	}); err != nil {
		t.Fatalf("RenderEvent: %v", err)
	}

	texts := mock.updatedTexts()
	if len(texts) != 1 {
		t.Fatalf("UpdateMessage calls = %d, want 1", len(texts))
	}
	if !strings.Contains(texts[0], "❌") || !strings.Contains(texts[0], "agent died") {
		t.Errorf("error log = %q, want ❌ agent died", texts[0])
	}
	// After an error, the receipt must be Completed — late
	// events would be dropped.
	if got := r.receipts["oc_chat"].State(); got != StateCompleted {
		t.Errorf("state after error = %s, want Completed", got)
	}
}

// TestRender_Done verifies EventDone transitions the receipt to
// Completed (terminal state) and re-renders the header with the
// completion timestamp. No new entry is added — the work log stays
// as-is.
func TestRender_Done(t *testing.T) {
	r, mock := newRendererWithMock()

	if err := r.RenderEvent(context.Background(), "oc_chat", agent.AgentEvent{
		Kind: agent.EventDone,
		Done: &agent.DoneEvent{ExitCode: 0},
	}); err != nil {
		t.Fatalf("RenderEvent: %v", err)
	}

	if got := r.receipts["oc_chat"].State(); got != StateCompleted {
		t.Errorf("state after done = %s, want Completed", got)
	}
	text := mock.lastUpdatedText()
	if !strings.Contains(text, "✅") {
		t.Errorf("done header = %q, want ✅ completed indicator", text)
	}
	if !strings.Contains(text, "已") {
		t.Errorf("done header missing 中文: %q", text)
	}
}

// TestRender_FIFOEviction verifies that when the rolling log grows
// past replyMaxBytes, the oldest entries are dropped and the
// rendered log shows the "…(前 N 条已省略)" marker.
func TestRender_FIFOEviction(t *testing.T) {
	r, mock := newRendererWithMock()

	// Push enough events to exceed replyMaxBytes. Each entry
	// has ~80 bytes; we need >replyMaxBytes/80 ≈ 44 entries to
	// trigger eviction. Send 60 to be safe.
	big := strings.Repeat("x", 200) // long text triggers per-entry truncation
	for i := 0; i < 60; i++ {
		ev := agent.AgentEvent{
			Kind: agent.EventText,
			Text: big,
		}
		if err := r.RenderEvent(context.Background(), "oc_chat", ev); err != nil {
			t.Fatalf("RenderEvent #%d: %v", i, err)
		}
	}

	text := mock.lastUpdatedText()
	if len(text) > replyMaxBytes+50 { // small slack for marker
		t.Errorf("final log size = %d, want ≤ %d", len(text), replyMaxBytes+50)
	}
	if !strings.Contains(text, "前") || !strings.Contains(text, "已省略") {
		t.Errorf("eviction marker missing from log: %q", text[:min(120, len(text))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestRender_Permission verifies permission requests still render as
// a separate interactive card (bypassing the rolling log).
func TestRender_Permission(t *testing.T) {
	a := testAdapter(t)
	// Capture every send so we can assert the card landed.
	sent := make([]string, 0, 1)
	a.sendFunc = func(_ context.Context, chatID, msgType, content string) (string, error) {
		sent = append(sent, content)
		return "card_id", nil
	}
	r := NewRenderer(a)

	req := &agent.PermissionRequest{
		Tool:    "Bash",
		Action:  "Run go test ./...",
		Options: []string{"once", "reject"},
	}
	if err := r.RenderEvent(context.Background(), "oc_chat", agent.AgentEvent{
		Kind:        agent.EventPermission,
		Permission: req,
	}); err != nil {
		t.Fatalf("RenderEvent: %v", err)
	}

	if got := len(sent); got != 1 {
		t.Fatalf("permission should send exactly 1 card; sent %d", got)
	}
	cardJSON := sent[0]
	var envelope struct {
		Card json.RawMessage `json:"card"`
	}
	if err := json.Unmarshal([]byte(cardJSON), &envelope); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, cardJSON)
	}
	if envelope.Card == nil {
		t.Fatalf("envelope has no card payload: %s", cardJSON)
	}
	var card struct {
		Header struct {
			Title struct {
				Content string `json:"content"`
			} `json:"title"`
		} `json:"header"`
		Elements []struct {
			Tag     string           `json:"tag"`
			Actions []map[string]any `json:"actions"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(envelope.Card, &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if !strings.Contains(card.Header.Title.Content, "Permission") {
		t.Errorf("card title = %q, want 'Permission needed'", card.Header.Title.Content)
	}
	// Last element is the action row carrying the buttons.
	actions := card.Elements[len(card.Elements)-1].Actions
	if len(actions) != 2 {
		t.Fatalf("card buttons = %d, want 2", len(actions))
	}
}

// msgTypeCheck is a tiny helper for tests that need the msg_type
// out of the recorded call. sentText records the JSON-encoded text
// body, so this parses it back out.
func (c receiptSwapSendCall) msgTypeCheck() string {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(c.Text), &payload); err != nil {
		return ""
	}
	// The bot's sendFunc records the rendered JSON content; the
	// adapter wraps it in {"text": "..."} for text messages and
	// passes the raw card JSON for interactive ones. We detect by
	// presence of a "card" key.
	if _, ok := payload["card"]; ok {
		return interactiveMessageType
	}
	return "text"
}