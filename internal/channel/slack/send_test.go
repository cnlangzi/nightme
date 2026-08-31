package slack

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

const testChatID = "sl_T1:C1"

func outbound(kind messages.OutboundKind, text string) messages.OutboundMessage {
	return messages.OutboundMessage{
		ChatID:  testChatID,
		Kind:    kind,
		Text:    text,
		ReplyTo: "1000.1",
	}
}

// The rolling placeholder: reply chunks fold into one stream, not
// one message each.
func TestSend_OutReplyFoldsIntoOneStream(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	ctx := context.Background()

	for _, chunk := range []string{"one", "two", "three"} {
		if err := a.Send(ctx, outbound(messages.OutReply, chunk)); err != nil {
			t.Fatalf("send %q: %v", chunk, err)
		}
	}

	if n := api.countOf("StartStream"); n != 1 {
		t.Fatalf("StartStream count = %d, want exactly 1 for the turn", n)
	}
	if n := api.countOf("AppendStream"); n != 2 {
		t.Fatalf("AppendStream count = %d, want 2", n)
	}
	if n := api.countOf("PostMessage"); n != 0 {
		t.Fatalf("reply chunks must not create standalone messages, got %d", n)
	}
}

func TestSend_ThinkingIsPrefixed(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())

	if err := a.Send(context.Background(), outbound(messages.OutThinking, "pondering")); err != nil {
		t.Fatalf("send: %v", err)
	}
	texts := chunkTexts(api.snapshot()[0].Chunks)
	if len(texts) != 1 || !strings.HasPrefix(texts[0], "💭 ") {
		t.Fatalf("thinking chunk = %v, want a 💭 prefix", texts)
	}
}

// A tool's start and end must land on ONE card id, otherwise the
// user sees two half-cards per call.
func TestSend_ToolStartAndEndShareCardID(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	ctx := context.Background()

	start := outbound(messages.OutToolStart, "")
	start.Tool = toolInfo("Bash", "git status")
	if err := a.Send(ctx, start); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	end := outbound(messages.OutToolEnd, "")
	end.Tool = &messages.ToolInfo{Name: "Bash", Output: "clean"}
	if err := a.Send(ctx, end); err != nil {
		t.Fatalf("tool end: %v", err)
	}

	var tasks []string
	var statuses []string
	for _, c := range api.snapshot() {
		for _, tc := range taskChunks(c.Chunks) {
			tasks = append(tasks, tc.ID)
			statuses = append(statuses, string(tc.Status))
		}
	}
	if len(tasks) != 2 {
		t.Fatalf("expected two task chunks, got %d", len(tasks))
	}
	if tasks[0] != tasks[1] {
		t.Fatalf("start id %q != end id %q — Slack would render two cards", tasks[0], tasks[1])
	}
	if statuses[0] != "in_progress" || statuses[1] != "complete" {
		t.Fatalf("statuses = %v, want [in_progress complete]", statuses)
	}
}

func TestSend_FailedToolEndsAsError(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	ctx := context.Background()

	start := outbound(messages.OutToolStart, "")
	start.Tool = toolInfo("Bash", "false")
	_ = a.Send(ctx, start)

	end := outbound(messages.OutToolEnd, "")
	end.Tool = &messages.ToolInfo{Name: "Bash"}
	end.Err = errBoom
	if err := a.Send(ctx, end); err != nil {
		t.Fatalf("tool end: %v", err)
	}

	var last string
	for _, c := range api.snapshot() {
		for _, tc := range taskChunks(c.Chunks) {
			last = string(tc.Status)
		}
	}
	if last != "error" {
		t.Fatalf("failed tool status = %q, want error", last)
	}
}

// An end with no matching start still has to render something.
func TestSend_UnpairedToolEndStillRenders(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())

	end := outbound(messages.OutToolEnd, "")
	end.Tool = &messages.ToolInfo{Name: "Bash", Output: "done"}
	if err := a.Send(context.Background(), end); err != nil {
		t.Fatalf("tool end: %v", err)
	}

	found := false
	for _, c := range api.snapshot() {
		if len(taskChunks(c.Chunks)) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("an unpaired tool end must still produce a card")
	}
}

func TestSend_TaskListMapsStatuses(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())

	msg := outbound(messages.OutTaskCreate, "")
	msg.TaskList = &agent.AgentTaskListEvent{Items: []agent.AgentTaskItem{
		{ID: "a", Subject: "write tests", Status: agent.TaskInProgress, ActiveForm: "writing tests"},
		{ID: "b", Subject: "ship it", Status: agent.TaskPending},
		{ID: "c", Subject: "done thing", Status: agent.TaskCompleted},
	}}
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	var got []string
	for _, c := range api.snapshot() {
		for _, tc := range taskChunks(c.Chunks) {
			got = append(got, string(tc.Status))
		}
	}
	want := []string{"in_progress", "pending", "complete"}
	if len(got) != len(want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("status %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The heartbeat rides the assistant-status endpoint, which has a far
// larger budget than the stream's.
func TestSend_HeartbeatUsesAssistantStatus(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())

	msg := outbound(messages.OutHeartbeat, "")
	hb := hbSnapshot(2, 3)
	msg.Heartbeat = &hb
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	calls := api.snapshot()
	if len(calls) != 1 || calls[0].Method != "SetAssistantStatus" {
		t.Fatalf("calls = %v, want a single SetAssistantStatus", api.methods())
	}
	if calls[0].Status != "💭 2 · 🔧 3" {
		t.Fatalf("status = %q", calls[0].Status)
	}
	if n := api.countOf("AppendStream"); n != 0 {
		t.Fatal("the heartbeat must not spend the stream's quota")
	}
}

// Workspaces without AI features reject the status call; the plan
// chunk is the fallback so the user still sees progress.
func TestSend_HeartbeatFallsBackToPlanChunk(t *testing.T) {
	api := newFakeAPI()
	api.failAlways("SetAssistantStatus", errBoom)
	a := newTestAdapter(t, api, newFakeSocket())

	msg := outbound(messages.OutHeartbeat, "")
	hb := hbSnapshot(1, 0)
	msg.Heartbeat = &hb
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	if api.countOf("StartStream") != 1 {
		t.Fatalf("expected the plan chunk to open a stream, got %v", api.methods())
	}
}

func TestSend_EmptyHeartbeatIsDropped(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())

	msg := outbound(messages.OutHeartbeat, "")
	hb := hbSnapshot(0, 0)
	msg.Heartbeat = &hb
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(api.methods()) != 0 {
		t.Fatalf("a zero-valued heartbeat should be dropped, got %v", api.methods())
	}
}

// §4.2 of docs/channel/slack.md: a blocking prompt must not sit in
// the throttle buffer, and it must be independently clickable, so it
// is a standalone message rather than a stream chunk.
func TestSend_ChoiceIsStandaloneAndImmediate(t *testing.T) {
	api := newFakeAPI()
	a := withThrottle(newTestAdapter(t, api, newFakeSocket()), time.Hour)

	msg := outbound(messages.OutChoice, "")
	msg.Choice = &messages.Choice{
		RequestID: "req-1",
		Title:     "Allow Bash?",
		Body:      "git status",
		Options: []messages.ChoiceOption{
			{ID: "allow", Label: "Allow"},
			{ID: "deny", Label: "Deny"},
		},
	}
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	calls := api.snapshot()
	if len(calls) != 1 || calls[0].Method != "PostMessage" {
		t.Fatalf("calls = %v, want a single PostMessage", api.methods())
	}
	if len(calls[0].Blocks) != 2 {
		t.Fatalf("expected a section + actions block, got %d", len(calls[0].Blocks))
	}
	if !strings.Contains(calls[0].Text, "Allow Bash?") {
		t.Fatalf("notification fallback text = %q", calls[0].Text)
	}
}

func TestSend_ChoicePatchUpdatesInPlace(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	ctx := context.Background()

	choice := &messages.Choice{
		RequestID: "req-1",
		Title:     "Allow Bash?",
		Options:   []messages.ChoiceOption{{ID: "allow", Label: "Allow"}},
	}
	msg := outbound(messages.OutChoice, "")
	msg.Choice = choice
	if err := a.Send(ctx, msg); err != nil {
		t.Fatalf("send choice: %v", err)
	}
	postTS := api.snapshot()[0].TS

	patch := outbound(messages.OutChoicePatch, "")
	patch.Choice = &messages.Choice{
		RequestID: "req-1", Title: "Allow Bash?",
		Settled: true, SelectedID: "allow",
		Options: choice.Options,
	}
	if err := a.Send(ctx, patch); err != nil {
		t.Fatalf("send patch: %v", err)
	}

	calls := api.snapshot()
	last := calls[len(calls)-1]
	if last.Method != "UpdateMessage" {
		t.Fatalf("last call = %q, want UpdateMessage", last.Method)
	}
	if last.TS != postTS {
		t.Fatalf("patch targeted %q, want the original %q", last.TS, postTS)
	}
	if !strings.Contains(last.Text, "Allow") {
		t.Fatalf("settled text should name the pick, got %q", last.Text)
	}
}

func TestSend_CommandReplyIsStandaloneWithMarker(t *testing.T) {
	api := newFakeAPI()
	a := withThrottle(newTestAdapter(t, api, newFakeSocket()), time.Hour)

	if err := a.Send(context.Background(), outbound(messages.OutCommandReply, "cwd set")); err != nil {
		t.Fatalf("send: %v", err)
	}
	calls := api.snapshot()
	if len(calls) != 1 || calls[0].Method != "PostMessage" {
		t.Fatalf("calls = %v, want one PostMessage", api.methods())
	}
	if !strings.HasPrefix(calls[0].Text, "❯ ") {
		t.Fatalf("command reply = %q, want the ❯ marker", calls[0].Text)
	}
}

func TestSend_ErrorCarriesStderrTail(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())

	msg := outbound(messages.OutError, "bridge died")
	msg.Diagnostic = &agent.BridgeDiagnostic{StderrTail: "panic: nil map"}
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	text := api.snapshot()[0].Text
	if !strings.Contains(text, "bridge died") || !strings.Contains(text, "panic: nil map") {
		t.Fatalf("error message dropped context: %q", text)
	}
}

// Slack lets a bot remove its own reaction, so the state track is a
// real replacement rather than the stack Feishu ends up with.
func TestSend_MessageStateReplacesPreviousReaction(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	ctx := context.Background()

	send := func(state agent.MessageState) {
		msg := messages.OutboundMessage{
			ChatID:       testChatID,
			Kind:         messages.OutMessageState,
			MessageState: &messages.MessageStatePayload{State: state, MessageID: "1000.1"},
		}
		if err := a.Send(ctx, msg); err != nil {
			t.Fatalf("send %v: %v", state, err)
		}
	}

	send(agent.MessageQueued)
	send(agent.MessageSubmitted)

	calls := api.snapshot()
	if len(calls) != 3 {
		t.Fatalf("calls = %v, want add / remove / add", api.methods())
	}
	if calls[0].Method != "AddReaction" || calls[0].Reaction != reactionQueued {
		t.Fatalf("first call = %+v", calls[0])
	}
	if calls[1].Method != "RemoveReaction" || calls[1].Reaction != reactionQueued {
		t.Fatalf("second call should retract the old emoji, got %+v", calls[1])
	}
	if calls[2].Method != "AddReaction" || calls[2].Reaction != reactionSubmitted {
		t.Fatalf("third call = %+v", calls[2])
	}
}

func TestSend_RepeatedMessageStateIsDeduped(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	ctx := context.Background()

	msg := messages.OutboundMessage{
		ChatID:       testChatID,
		Kind:         messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{State: agent.MessageQueued, MessageID: "1000.1"},
	}
	_ = a.Send(ctx, msg)
	_ = a.Send(ctx, msg)

	if n := api.countOf("AddReaction"); n != 1 {
		t.Fatalf("repeated identical state should short-circuit, got %d AddReaction calls", n)
	}
}

func TestSend_UnparseableChatIDIsRejectedNotSilent(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())

	msg := messages.OutboundMessage{ChatID: "tg_-100", Kind: messages.OutCommandReply, Text: "hi"}
	if err := a.Send(context.Background(), msg); err == nil {
		t.Fatal("a foreign chat id should surface an error, not be silently dropped")
	}
}

// OnPromptEnded is the only place a stream gets closed. If it did
// not, the message would render as in-progress forever.
func TestOnPromptEnded_ClosesStreamAndMarksDone(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	ctx := context.Background()

	if err := a.Send(ctx, outbound(messages.OutReply, "answer")); err != nil {
		t.Fatalf("send: %v", err)
	}
	a.OnPromptEnded(ctx, testChatID, "1000.1")

	if n := api.countOf("StopStream"); n != 1 {
		t.Fatalf("StopStream count = %d, want 1", n)
	}
	found := false
	for _, c := range api.snapshot() {
		if c.Method == "AddReaction" && c.Reaction == reactionDone {
			found = true
		}
	}
	if !found {
		t.Fatal("the user's message should be marked done")
	}
	if _, ok := a.streams.lookup(testChatID, "1000.1"); ok {
		t.Fatal("the finished turn should be purged from the index")
	}
}

func TestOnPromptEnded_ClearsAssistantStatus(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	ctx := context.Background()

	_ = a.Send(ctx, outbound(messages.OutReply, "answer"))
	a.OnPromptEnded(ctx, testChatID, "1000.1")

	cleared := false
	for _, c := range api.snapshot() {
		if c.Method == "SetAssistantStatus" && c.Status == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("the thinking indicator must be cleared when the turn ends")
	}
}

// A late event for a turn that already ended must still reach the
// user rather than vanish.
func TestSend_AfterPromptEndedFallsBackToStandalone(t *testing.T) {
	api := newFakeAPI()
	a := newTestAdapter(t, api, newFakeSocket())
	ctx := context.Background()

	_ = a.Send(ctx, outbound(messages.OutReply, "answer"))
	stream, _ := a.streams.lookup(testChatID, "1000.1")
	if err := stream.finish(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if err := a.Send(ctx, outbound(messages.OutReply, "late")); err != nil {
		t.Fatalf("late send: %v", err)
	}
	posted := false
	for _, c := range api.snapshot() {
		if c.Method == "PostMessage" && strings.Contains(c.Text, "late") {
			posted = true
		}
	}
	if !posted {
		t.Fatal("a late chunk should be delivered standalone, not dropped")
	}
}
