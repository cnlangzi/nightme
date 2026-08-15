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
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
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

func (m *mockReceiptBot) SendCardForReceipt(_ context.Context, _, body, _ string, _ bool) (string, error) {
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

// TestSetTaskList_ReceiptBornRunning (F-53 rename) verifies that
// a fresh receipt starts in chatsession.PromptRunning (was PromptPending in
// v1.3 — F-53 deletes the Pending value entirely). SetTaskList
// no longer promotes any state — receipts are born running and
// stay running for their lifetime (chatsession.PromptDone is reserved but
// never assigned in Phase 0).
//
// See docs/feat/message_lifecycle.md §7 (chatsession.PromptState shrink to
// Running/Done).
func TestSetTaskList_ReceiptBornRunning(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card", bot)

	if got := r.PromptState(); got != chatsession.PromptRunning {
		t.Fatalf("initial promptState = %v, want chatsession.PromptRunning", got)
	}

	list := &agent.AgentTaskListEvent{
		Items: []agent.AgentTaskItem{
			{ID: "t1", Subject: "task one", Status: agent.TaskPending},
			{ID: "t2", Subject: "task two", Status: agent.TaskCompleted},
		},
	}
	if err := r.SetTaskList(context.Background(), list); err != nil {
		t.Fatalf("SetTaskList: %v", err)
	}
	// F-53: SetTaskList is a no-op on promptState — receipt was
	// already chatsession.PromptRunning at construction. After SetTaskList
	// it should still be chatsession.PromptRunning (not promoted to anything
	// new).
	if got := r.PromptState(); got != chatsession.PromptRunning {
		t.Errorf("after SetTaskList: promptState = %v, want chatsession.PromptRunning", got)
	}
}

// TestSetTaskList_EmptyClearsTasks verifies that a snapshot with zero
// items clears the existing checklist (F-38 §1.2 contract).
func TestSetTaskList_EmptyClearsTasks(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card", bot)

	// First snapshot: 2 items.
	if err := r.SetTaskList(context.Background(), &agent.AgentTaskListEvent{
		Items: []agent.AgentTaskItem{
			{ID: "t1", Subject: "task one", Status: agent.TaskPending},
		},
	}); err != nil {
		t.Fatalf("first SetTaskList: %v", err)
	}

	// Second snapshot: empty — should clear.
	if err := r.SetTaskList(context.Background(), &agent.AgentTaskListEvent{
		Items: []agent.AgentTaskItem{},
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

	first := &agent.AgentTaskListEvent{
		Items: []agent.AgentTaskItem{
			{ID: "t1", Subject: "first", Status: agent.TaskPending},
		},
	}
	second := &agent.AgentTaskListEvent{
		Items: []agent.AgentTaskItem{
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

	list1 := &agent.AgentTaskListEvent{
		Items: []agent.AgentTaskItem{
			{ID: "t1", Subject: "first", Status: agent.TaskPending},
		},
	}
	list2 := &agent.AgentTaskListEvent{
		Items: []agent.AgentTaskItem{
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
		tasks: []agent.AgentTaskItem{
			{ID: "t1", Subject: "first task", Status: agent.TaskPending},
			{ID: "t2", Subject: "second task", Status: agent.TaskCompleted},
		},
	}
	body, _, err := buildReceiptCard(r.entries, r.tasks, r.footerLines, &r.heartbeat)
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
	body, _, err := buildReceiptCard(r.entries, r.tasks, r.footerLines, &r.heartbeat)
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

// TestAppendEntry_OverflowRolloverPatchesNewCard (fix-reply-placehold-card)
// verifies the new overflow → rollover → continue-PATCHing flow:
//
//  1. Pre-fill the receipt to its 50-element cap so the next append
//     would overflow.
//  2. Append one more entry → ErrReceiptOverflow, no PATCH, the
//     overflow entry is NOT committed (matches TestAppendEntry_OverflowBailsOut).
//  3. Simulate the adapter's overflow handler: build a new card
//     body for a fresh placeholder, send it via SendCardForReceipt
//     to get the new msgID, then call receipt.RolloverTo to switch
//     the receipt's tracking to the new card.
//  4. Append a follow-up entry on the rolled-over receipt →
//     should PATCH the NEW msgID, NOT the original cardMsgID.
//
// Assertions:
//
//   - bot.cards has exactly 1 entry (the new placeholder), with a
//     different MessageID from the original.
//   - bot.patches has exactly 1 entry, addressed to the NEW
//     MessageID. The original cardMsgID never receives another
//     PATCH after the rollover.
func TestAppendEntry_OverflowRolloverPatchesNewCard(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card_initial", bot)

	// (1) Pre-fill to the element cap.
	entries := make([]LogEntry, receiptMaxElements)
	for i := range entries {
		entries[i] = LogEntry{Icon: "💬", Text: fmt.Sprintf("entry %d", i)}
	}
	r.mu.Lock()
	r.entries = entries
	r.mu.Unlock()

	// (2) Next append overflows; receipt stays untouched.
	overflowEntry := LogEntry{Icon: "💬", Text: "overflow"}
	if err := r.AppendEntry(context.Background(), overflowEntry); !errors.Is(err, ErrReceiptOverflow) {
		t.Fatalf("AppendEntry = %v, want ErrReceiptOverflow", err)
	}
	r.mu.Lock()
	if got := len(r.entries); got != receiptMaxElements {
		t.Fatalf("pre-rollover entries len = %d, want %d (overflow entry must NOT commit)", got, receiptMaxElements)
	}
	preRolloverCard := r.cardMsgID
	r.mu.Unlock()

	// (3) Simulate adapter's OutReply overflow branch: build body,
	// send a new top-level card, call RolloverTo.
	r.mu.Lock()
	currentTasks := r.tasks
	r.mu.Unlock()
	body, _, err := buildReceiptCard([]LogEntry{overflowEntry}, currentTasks, nil, nil)
	if err != nil {
		t.Fatalf("buildReceiptCard for overflow placeholder: %v", err)
	}
	newMsgID, err := bot.SendCardForReceipt(context.Background(), "oc_chat", body, "", false)
	if err != nil {
		t.Fatalf("SendCardForReceipt for overflow placeholder: %v", err)
	}
	if newMsgID == preRolloverCard {
		t.Fatalf("new placeholder msgID == original cardMsgID (%q); rollover would not actually move", newMsgID)
	}
	r.RolloverTo(newMsgID, overflowEntry, nil)

	// (4) Append a follow-up entry → PATCHes the new card.
	if err := r.AppendEntry(context.Background(), LogEntry{Icon: "💬", Text: "after-rollover"}); err != nil {
		t.Fatalf("AppendEntry after rollover: %v", err)
	}

	// Exactly one new placeholder was created.
	if len(bot.cards) != 1 {
		t.Fatalf("SendCard count = %d, want 1 (the overflow placeholder)", len(bot.cards))
	}
	if got := bot.cards[0].MessageID; got != newMsgID {
		t.Errorf("placeholder MessageID = %q, want %q", got, newMsgID)
	}

	// Exactly one PATCH was issued, and it targets the new card —
	// the original cardMsgID receives zero post-rollover PATCHes.
	if len(bot.patches) != 1 {
		t.Fatalf("PatchMessage count = %d, want 1", len(bot.patches))
	}
	if got := bot.patches[0].MessageID; got != newMsgID {
		t.Errorf("PATCH MessageID = %q, want %q (must target the new placeholder)", got, newMsgID)
	}
	if got := bot.patches[0].MessageID; got == preRolloverCard {
		t.Errorf("PATCH targeted the ORIGINAL card %q after rollover; receipt should have migrated", got)
	}

	// Receipt state reflects the rollover: 2 entries on the new
	// card, cardMsgID switched.
	r.mu.Lock()
	defer r.mu.Unlock()
	if got := len(r.entries); got != 2 {
		t.Errorf("post-rollover entries len = %d, want 2 (overflow + follow-up)", got)
	}
	if r.entries[0].Text != "overflow" || r.entries[1].Text != "after-rollover" {
		t.Errorf("entries = [%q, %q], want [overflow, after-rollover]", r.entries[0].Text, r.entries[1].Text)
	}
	if r.cardMsgID != newMsgID {
		t.Errorf("cardMsgID = %q, want %q", r.cardMsgID, newMsgID)
	}
}

// TestRolloverTo_ResetsEntriesAndFooter verifies the receipt's
// internal state after a RolloverTo call:
//
//   - cardMsgID / replyMsgID switch to the new card.
//   - entries is reset to exactly [firstEntry] (NOT appended to the
//     pre-rollover slice — otherwise the new card would re-overflow
//     immediately).
//   - footerLines is replaced (or cleared if nil was passed).
//   - tasks is PRESERVED (the checklist is a global view across the
//     turn; freezing it on the old card would orphan in-flight
//     tasks).
//   - lastBody + lastBodyPatch are reset so the first PATCH on the
//     new card isn't suppressed by the duplicate-body short-circuit
//     or delayed by the 300ms rate limiter measured against the old
//     card's last PATCH time.
func TestRolloverTo_ResetsEntriesAndFooter(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card_initial", bot)

	// Seed: 3 entries, a footer, and 2 tasks. Also set lastBody +
	// lastBodyPatch to non-zero values so we can verify the reset.
	r.mu.Lock()
	r.entries = []LogEntry{
		{Icon: "💬", Text: "old-1"},
		{Icon: "💬", Text: "old-2"},
		{Icon: "💬", Text: "old-3"},
	}
	r.footerLines = []string{"old-footer-1", "old-footer-2"}
	r.tasks = []agent.AgentTaskItem{
		{ID: "t1", Subject: "task one", Status: agent.TaskPending},
		{ID: "t2", Subject: "task two", Status: agent.TaskCompleted},
	}
	r.lastBody = "previous render body"
	r.lastBodyPatch = time.Now()
	r.mu.Unlock()

	// Rollover to a new card.
	first := LogEntry{Icon: "💬", Text: "new-first"}
	r.RolloverTo("om_card_new", first, []string{"new-footer"})

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cardMsgID != "om_card_new" {
		t.Errorf("cardMsgID = %q, want %q", r.cardMsgID, "om_card_new")
	}
	if r.replyMsgID != "om_card_new" {
		t.Errorf("replyMsgID = %q, want %q", r.replyMsgID, "om_card_new")
	}
	if got := len(r.entries); got != 1 {
		t.Fatalf("entries len = %d, want 1 (reset to firstEntry only)", got)
	}
	if r.entries[0].Text != "new-first" {
		t.Errorf("entries[0].Text = %q, want %q", r.entries[0].Text, "new-first")
	}
	if got := len(r.footerLines); got != 1 || r.footerLines[0] != "new-footer" {
		t.Errorf("footerLines = %v, want [new-footer]", r.footerLines)
	}
	// tasks preserved.
	if got := len(r.tasks); got != 2 {
		t.Errorf("tasks len = %d, want 2 (preserved across rollover)", got)
	}
	if r.tasks[0].ID != "t1" || r.tasks[1].ID != "t2" {
		t.Errorf("tasks = %v, want t1/t2 preserved", r.tasks)
	}
	// Render-state cache reset.
	if r.lastBody != "" {
		t.Errorf("lastBody = %q, want empty (so first PATCH on new card is not skipped)", r.lastBody)
	}
	if !r.lastBodyPatch.IsZero() {
		t.Errorf("lastBodyPatch = %v, want zero (so first PATCH on new card is not rate-limited)", r.lastBodyPatch)
	}
}

// TestRolloverTo_NilReceiptIsNoop verifies the nil-receiver contract
// parallel to AppendEntryWithFooter / SetPromptState.
func TestRolloverTo_NilReceiptIsNoop(t *testing.T) {
	var r *MessageReceipt
	r.RolloverTo("om_card_x", LogEntry{Icon: "💬", Text: "x"}, nil)
	// No panic = pass.
}

// TestRolloverTo_EmptyMsgIDIsNoop guards against an adapter bug
// where SendCardForReceipt returns "" on failure — calling RolloverTo
// with an empty msgID would silently migrate the receipt to an
// untracked card, breaking the next PATCH.
func TestRolloverTo_EmptyMsgIDIsNoop(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card_initial", bot)
	r.mu.Lock()
	r.entries = []LogEntry{{Icon: "💬", Text: "keep-me"}}
	r.mu.Unlock()

	r.RolloverTo("", LogEntry{Icon: "💬", Text: "drop-me"}, nil)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cardMsgID != "om_card_initial" {
		t.Errorf("cardMsgID = %q, want unchanged (%q) on empty-msgID rollover", r.cardMsgID, "om_card_initial")
	}
	if got := len(r.entries); got != 1 || r.entries[0].Text != "keep-me" {
		t.Errorf("entries = %v, want unchanged on empty-msgID rollover", r.entries)
	}
}

// TestSetPromptState_AfterRollover_ReactionGoesToLastCard (fix-reply-placehold-card)
// verifies that OnPromptEnded → SetPromptState(PromptDone) lands
// the ✅ reaction on the LAST active placeholder card, not the
// original receipt card. This is the user-visible payoff of the
// rollover: the "done" badge sticks to the surface the user is
// reading, even after the receipt has overflowed once or more.
func TestSetPromptState_AfterRollover_ReactionGoesToLastCard(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card_initial", bot)

	// Rollover: receipt now tracks a new placeholder.
	r.RolloverTo("om_card_overflow_1", LogEntry{Icon: "💬", Text: "x"}, nil)
	// Rollover again: simulate a second overflow within the same turn.
	r.RolloverTo("om_card_overflow_2", LogEntry{Icon: "💬", Text: "y"}, nil)

	// Adapter fires OnPromptEnded → SetPromptState(PromptDone).
	r.SetPromptState(context.Background(), chatsession.PromptDone)

	// The reaction should be on the LAST overflow card.
	if len(bot.reactions) != 1 {
		t.Fatalf("reaction count = %d, want 1", len(bot.reactions))
	}
	if got := bot.reactions[0].MessageID; got != "om_card_overflow_2" {
		t.Errorf("reaction MessageID = %q, want %q (the last active placeholder)", got, "om_card_overflow_2")
	}
	// The original cardMsgID must NOT have received any reaction.
	for i, rxn := range bot.reactions {
		if rxn.MessageID == "om_card_initial" {
			t.Errorf("reaction[%d] targeted the ORIGINAL card %q after rollover", i, rxn.MessageID)
		}
	}
}

// TestTasks_ReturnsSnapshotUnderLock verifies the new Tasks()
// getter that the adapter's overflow handler uses to build the
// rollover placeholder body. Specifically:
//
//   - Tasks() returns the current r.tasks slice verbatim.
//   - Tasks() on a fresh receipt returns nil (no list stamped yet).
//   - Tasks() reads under r.mu, so a concurrent SetTaskList
//     cannot produce a torn slice header. The race detector
//     (go test -race) is the actual assertion here; the body of
//     this test just exercises the contended path with enough
//     iterations to make a data race observable if the lock were
//     missing.
func TestTasks_ReturnsSnapshotUnderLock(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_card_initial", bot)

	// Empty case: fresh receipt, no SetTaskList call yet.
	if got := r.Tasks(); got != nil {
		t.Errorf("Tasks() on fresh receipt = %v, want nil", got)
	}

	// Seed: install a 2-item task list.
	if err := r.SetTaskList(context.Background(), &agent.AgentTaskListEvent{
		Items: []agent.AgentTaskItem{
			{ID: "t1", Subject: "task one", Status: agent.TaskPending},
			{ID: "t2", Subject: "task two", Status: agent.TaskCompleted},
		},
	}); err != nil {
		t.Fatalf("seed SetTaskList: %v", err)
	}
	got := r.Tasks()
	if len(got) != 2 {
		t.Fatalf("Tasks() len = %d, want 2", len(got))
	}
	if got[0].ID != "t1" || got[1].ID != "t2" {
		t.Errorf("Tasks() = %v, want t1/t2", got)
	}

	// Race-detector exercise: hammer Tasks() / SetTaskList from two
	// goroutines. Each SetTaskList installs a fresh slice via the
	// copy in SetTaskListWithFooter; Tasks() must read the slice
	// header under r.mu so it never observes a torn pointer.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = r.SetTaskList(context.Background(), &agent.AgentTaskListEvent{
				Items: []agent.AgentTaskItem{
					{ID: fmt.Sprintf("writer-%d", i), Subject: "x", Status: agent.TaskPending},
				},
			})
			i++
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = r.Tasks()
		}
	}()
	// Let the race detector observe both goroutines for a while.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestTasks_NilReceiptReturnsNil mirrors AppendEntry's nil-receiver
// guard: Tasks() on a nil *MessageReceipt must not panic.
func TestTasks_NilReceiptReturnsNil(t *testing.T) {
	var r *MessageReceipt
	if got := r.Tasks(); got != nil {
		t.Errorf("nil.Tasks() = %v, want nil", got)
	}
}
