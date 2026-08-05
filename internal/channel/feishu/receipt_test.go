// Package feishu — F-44 task-only receipt tests.
//
// F-44 collapsed the receipt card to a single section (the
// **📋 Tasks** checklist). OutReply / OutResult / OutInit / OutUsage
// no longer flow through the receipt; their tests moved to
// adapter_test.go (OutReply independent reply, OutResult independent
// reply, OutInit/OutUsage silent drop). This file covers the
// surviving receipt surface: SetTaskList / buildReceiptCard.
package feishu

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// mockReceiptBot captures SendCard / PatchMessage calls so the
// task-only card can be exercised without a live Feishu connection.
//
// F-44: AddReaction / UpdateMessage / SendMessageText are still on
// the receiptBot interface for backward compatibility but no longer
// called from production paths. The mock implements them as no-ops
// to satisfy the interface contract.
type mockReceiptBot struct {
	cards        []cardCall
	patches      []cardCall
	reactions    []reactionCall
	messages     []string
	sendCardErr  error
	patchErr     error
	nextCardID   int
}

type reactionCall struct {
	MessageID string
	Emoji     string
}

type cardCall struct {
	MessageID string
	Body      string
}

func (m *mockReceiptBot) AddReaction(_ context.Context, msgID, emoji string) (string, error) {
	m.reactions = append(m.reactions, reactionCall{msgID, emoji})
	return "mock-reaction-" + emoji, nil
}

func (m *mockReceiptBot) UpdateMessage(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockReceiptBot) SendMessageText(_ context.Context, _, text, _ string, _ bool) (string, error) {
	m.messages = append(m.messages, text)
	return "mock-text-msg", nil
}

func (m *mockReceiptBot) SendCard(_ context.Context, _, body, _ string, _ bool) (string, error) {
	if m.sendCardErr != nil {
		return "", m.sendCardErr
	}
	if m.nextCardID == 0 {
		m.nextCardID = 1
	}
	id := fmt.Sprintf("om_card_%d", m.nextCardID)
	m.nextCardID++
	m.cards = append(m.cards, cardCall{MessageID: id, Body: body})
	return id, nil
}

func (m *mockReceiptBot) PatchMessage(_ context.Context, messageID, body string) error {
	if m.patchErr != nil {
		return m.patchErr
	}
	m.patches = append(m.patches, cardCall{MessageID: messageID, Body: body})
	return nil
}

// TestSetTaskList_FirstCallPromotesPendingToRunning verifies that
// the first SetTaskList transitions the receipt from PromptPending to
// PromptRunning (so the footer PR can later recover the prompt
// state header without re-plumbing state transitions).
func TestSetTaskList_FirstCallPromotesPendingToRunning(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card", bot)

	if got := r.PromptState(); got != agent.PromptPending {
		t.Fatalf("initial promptState = %v, want agent.PromptPending", got)
	}

	list := &agent.TaskListEvent{
		Items: []agent.TaskItem{
			{ID: "t1", Subject: "task one", Status: agent.TaskPending},
			{ID: "t2", Subject: "task two", Status: agent.TaskCompleted},
		},
	}
	if err := r.SetTaskList(context.Background(), list); err != nil {
		t.Fatalf("SetTaskList: %v", err)
	}
	if got := r.PromptState(); got != agent.PromptRunning {
		t.Errorf("after first SetTaskList: promptState = %v, want agent.PromptRunning", got)
	}
}

// TestSetTaskList_EmptyClearsTasks verifies that a snapshot with zero
// items clears the existing checklist (F-38 §1.2 contract).
func TestSetTaskList_EmptyClearsTasks(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card", bot)

	// First snapshot: 2 items.
	if err := r.SetTaskList(context.Background(), &agent.TaskListEvent{
		Items: []agent.TaskItem{
			{ID: "t1", Subject: "task one", Status: agent.TaskPending},
		},
	}); err != nil {
		t.Fatalf("first SetTaskList: %v", err)
	}

	// Second snapshot: empty — should clear.
	if err := r.SetTaskList(context.Background(), &agent.TaskListEvent{
		Items: []agent.TaskItem{},
	}); err != nil {
		t.Fatalf("empty SetTaskList: %v", err)
	}

	r.mu.Lock()
	tasksLen := len(r.tasks)
	r.mu.Unlock()
	if tasksLen != 0 {
		t.Errorf("after empty snapshot, r.tasks len = %d, want 0", tasksLen)
	}
}

// TestSetTaskList_ReplacesSnapshot verifies that successive SetTaskList
// calls fully replace the prior snapshot (not append). The bridge
// always sends the full snapshot (F-38), so the receipt must mirror
// the latest one verbatim.
func TestSetTaskList_ReplacesSnapshot(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card", bot)

	first := &agent.TaskListEvent{
		Items: []agent.TaskItem{
			{ID: "t1", Subject: "first", Status: agent.TaskPending},
		},
	}
	second := &agent.TaskListEvent{
		Items: []agent.TaskItem{
			{ID: "s1", Subject: "second-a", Status: agent.TaskPending},
			{ID: "s2", Subject: "second-b", Status: agent.TaskCompleted},
		},
	}

	if err := r.SetTaskList(context.Background(), first); err != nil {
		t.Fatalf("first SetTaskList: %v", err)
	}
	if err := r.SetTaskList(context.Background(), second); err != nil {
		t.Fatalf("second SetTaskList: %v", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) != 2 {
		t.Fatalf("after replacement, r.tasks len = %d, want 2", len(r.tasks))
	}
	if r.tasks[0].ID != "s1" || r.tasks[1].ID != "s2" {
		t.Errorf("snapshot not replaced: got IDs %q/%q, want s1/s2",
			r.tasks[0].ID, r.tasks[1].ID)
	}
}

// TestSetTaskList_PATCHesOnSubsequentCall verifies that the
// pre-seeded cardMsgID (from NewMessageReceiptForReply) is used as
// the PATCH target across successive SetTaskList calls. The receipt
// never calls SendCard itself — the caller (ensureReceiptForTask
// in adapter.go) is responsible for the initial card post; the
// receipt just maintains the rolling-log PATCH cycle.
func TestSetTaskList_PATCHesOnSubsequentCall(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card_initial", bot)

	list1 := &agent.TaskListEvent{
		Items: []agent.TaskItem{
			{ID: "t1", Subject: "first", Status: agent.TaskPending},
		},
	}
	list2 := &agent.TaskListEvent{
		Items: []agent.TaskItem{
			{ID: "t1", Subject: "first", Status: agent.TaskCompleted},
			{ID: "t2", Subject: "second", Status: agent.TaskPending},
		},
	}

	if err := r.SetTaskList(context.Background(), list1); err != nil {
		t.Fatalf("first SetTaskList: %v", err)
	}
	if err := r.SetTaskList(context.Background(), list2); err != nil {
		t.Fatalf("second SetTaskList: %v", err)
	}

	if len(bot.cards) != 0 {
		t.Errorf("SendCard count = %d, want 0 (receipt never sends itself — caller did the initial card)", len(bot.cards))
	}
	if len(bot.patches) != 2 {
		t.Errorf("PatchMessage count = %d, want 2 (one per SetTaskList)", len(bot.patches))
	}
	for i, p := range bot.patches {
		if p.MessageID != "om_card_initial" {
			t.Errorf("patches[%d].MessageID = %q, want %q", i, p.MessageID, "om_card_initial")
		}
	}
}

// TestSetTaskList_NilListReturnsError verifies the contract from
// receipt.go: SetTaskList(nil) is a programmer error and returns
// an explicit error rather than silently clearing the list.
func TestSetTaskList_NilListReturnsError(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card", bot)

	if err := r.SetTaskList(context.Background(), nil); err == nil {
		t.Errorf("SetTaskList(nil) = nil, want error")
	}
}

// TestBuildReceiptCard_TaskOnly verifies that the F-44 simplified
// buildReceiptCard emits ONLY task markdown elements — no header /
// hr / footer sections (which were tied to OutReply / OutInit /
// OutUsage writers that are now gone).
func TestBuildReceiptCard_TaskOnly(t *testing.T) {
	r := &MessageReceipt{
		chatID:    "oc_chat",
		userMsgID: "om_user",
		tasks: []agent.TaskItem{
			{ID: "t1", Subject: "first task", Status: agent.TaskPending},
			{ID: "t2", Subject: "second task", Status: agent.TaskCompleted},
		},
	}
	body, err := buildReceiptCard(r.entries, r.tasks, r.footer)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}

	// Card 2.0 envelope markers present.
	for _, marker := range []string{
		`"schema":"2.0"`,
		`"width_mode":"fill"`,
		`"body":{`,
		`"elements":[`,
		`"tag":"markdown"`,
		`**📋 Tasks**`,
		`- [ ] first task`,
		`- [x] second task`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("body missing marker %q\nbody: %s", marker, body)
		}
	}

	// F-44 negative: no header / hr / footer markers.
	for _, absent := range []string{
		`⏳`,        // PromptPending header
		`🔄`,        // heartbeat (already retired pre-F-44)
		`✅ 已完成`, // PromptSucceeded header
		`❌`,        // PromptFailed header
		`Agent:`,   // footer (deleted with OutInit silent drop)
		`Workspace:`, // footer (deleted with OutInit silent drop)
		`Tokens:`,   // footer (deleted with OutUsage silent drop)
		`"tag":"hr"`, // hr divider (no longer needed)
	} {
		if strings.Contains(body, absent) {
			t.Errorf("body should NOT contain %q after F-44 simplification\nbody: %s", absent, body)
		}
	}
}

// TestBuildReceiptCard_NoTasksEmptyBody verifies that an empty
// task list produces an empty card body (no checklist element).
// Feishu's API accepts an empty `elements` array.
func TestBuildReceiptCard_NoTasksEmptyBody(t *testing.T) {
	r := &MessageReceipt{
		chatID:    "oc_chat",
		userMsgID: "om_user",
	}
	body, err := buildReceiptCard(r.entries, r.tasks, r.footer)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	// Empty elements array; no checklist; no error.
	if !strings.Contains(body, `"elements":[`) {
		t.Errorf("body missing elements marker: %s", body)
	}
	if strings.Contains(body, "**📋 Tasks**") {
		t.Errorf("body should NOT contain Tasks header for empty receipt: %s", body)
	}
}
// TestAppendEntry_FoldsIntoReceipt — F-44 revert: each OutReply
// chunk is appended to the rolling-log entries, PATCHing the card
// in place. The mockReceiptBot records each PATCH and the receipt
// keeps append-only order.
func TestAppendEntry_FoldsIntoReceipt(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card_initial", bot)
	// Seed the first entry (the cold-start path installs the first
	// entry before AppendEntry is called; the test mirrors that
	// pattern by directly mutating r.entries).
	r.mu.Lock()
	r.entries = []LogEntry{{Icon: "💬", Text: "first"}}
	r.mu.Unlock()

	// Append second entry.
	if err := r.AppendEntry(context.Background(), LogEntry{Icon: "💬", Text: "second"}); err != nil {
		t.Fatalf("AppendEntry second: %v", err)
	}
	r.mu.Lock()
	if len(r.entries) != 2 {
		t.Errorf("after AppendEntry: entries len = %d, want 2", len(r.entries))
	}
	if r.entries[0].Text != "first" || r.entries[1].Text != "second" {
		t.Errorf("entries out of order: %q / %q", r.entries[0].Text, r.entries[1].Text)
	}
	r.mu.Unlock()

	// 1 PATCH (the second entry triggered a re-render).
	if len(bot.patches) != 1 {
		t.Errorf("patch count = %d, want 1 (one per AppendEntry)", len(bot.patches))
	}
}

// TestAppendEntry_OverflowBailsOut — F-40 + F-44 revert: when
// appending an entry would push the card past 50 elements /
// 30 KB envelope, AppendEntry returns ErrReceiptOverflow WITHOUT
// issuing a PATCH. The caller is expected to catch this and send
// the entry as a fresh top-level Create (ReplyInChat). The
// receipt's existing entries stay intact.
func TestAppendEntry_OverflowBailsOut(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card_initial", bot)

	// Pre-fill entries so the next append would push the card over
	// the element cap. 50 entries × 1 element each = at the cap.
	// The 51st would overflow.
	entries := make([]LogEntry, receiptMaxElements)
	for i := range entries {
		entries[i] = LogEntry{Icon: "💬", Text: fmt.Sprintf("entry %d", i)}
	}
	r.mu.Lock()
	r.entries = entries
	r.mu.Unlock()

	// Snapshot the PATCH count before the overflow attempt.
	beforePatches := len(bot.patches)

	// This append should overflow → return ErrReceiptOverflow, no PATCH.
	err := r.AppendEntry(context.Background(), LogEntry{Icon: "💬", Text: "overflow"})
	if !errors.Is(err, ErrReceiptOverflow) {
		t.Fatalf("AppendEntry returned %v, want ErrReceiptOverflow", err)
	}
	if len(bot.patches) != beforePatches {
		t.Errorf("PATCH issued on overflow path: before=%d, after=%d (should be unchanged)",
			beforePatches, len(bot.patches))
	}
	// The receipt's existing entries stay intact.
	r.mu.Lock()
	if len(r.entries) != receiptMaxElements {
		t.Errorf("entries len after overflow = %d, want %d (overflow entry must NOT be committed)",
			len(r.entries), receiptMaxElements)
	}
	r.mu.Unlock()
}
