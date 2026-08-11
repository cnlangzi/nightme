package feishu

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestPushPopToolStart_FIFO locks the FIFO ordering invariant:
// the first pushed entry is the first popped. Stream-json's
// tool_use → tool_result pairs are strictly ordered; the merge
// path relies on each End editing the correct Start's message_id
// even when several pairs are in flight (parallel tool_use).
func TestPushPopToolStart_FIFO(t *testing.T) {
	a := testAdapter(t)

	a.pushToolStart("om_user_1", "om_msg_a", "● A")
	a.pushToolStart("om_user_1", "om_msg_b", "● B")
	a.pushToolStart("om_user_1", "om_msg_c", "● C")

	first, ok := a.popToolStart("om_user_1")
	if !ok {
		t.Fatal("first pop returned not-ok; buffer should be non-empty")
	}
	if first.startMsgID != "om_msg_a" || first.startBody != "● A" {
		t.Errorf("first pop = %+v, want om_msg_a / ● A", first)
	}
	second, ok := a.popToolStart("om_user_1")
	if !ok || second.startMsgID != "om_msg_b" || second.startBody != "● B" {
		t.Errorf("second pop = %+v, want om_msg_b / ● B", second)
	}
	third, ok := a.popToolStart("om_user_1")
	if !ok || third.startMsgID != "om_msg_c" || third.startBody != "● C" {
		t.Errorf("third pop = %+v, want om_msg_c / ● C", third)
	}
	// Buffer should be drained.
	if _, ok := a.popToolStart("om_user_1"); ok {
		t.Error("fourth pop returned ok; buffer should be drained after 3 pushes")
	}
}

// TestPushPopToolStart_EmptyBufferMiss covers the orphan-End path:
// an OutToolEnd arrives without a matching OutToolStart in the
// buffer (e.g. tool stream truncation). popToolStart returns
// (_, false) so the caller falls back to posting the result as a
// fresh thread reply.
func TestPushPopToolStart_EmptyBufferMiss(t *testing.T) {
	a := testAdapter(t)

	// No pushes.
	if _, ok := a.popToolStart("om_user_1"); ok {
		t.Error("pop on empty buffer returned ok; want (_, false)")
	}

	// Pop on a different userMsgID than what was pushed.
	a.pushToolStart("om_user_2", "om_msg_x", "● X")
	if _, ok := a.popToolStart("om_user_1"); ok {
		t.Error("pop on different userMsgID returned ok; want (_, false)")
	}
	// The original entry is still in om_user_2's queue.
	entry, ok := a.popToolStart("om_user_2")
	if !ok || entry.startMsgID != "om_msg_x" {
		t.Errorf("om_user_2 pop = %+v, want om_msg_x", entry)
	}
}

// TestPushToolStart_EmptyMsgIDIsNoOp covers the orphan-Start
// path: pushToolStart refuses to record an entry when the
// freshly-posted Start returned no message_id (rootID was ""
// → sendRawOutText fallback). The matching End will then fall
// through to the fresh-thread-reply fallback path in
// Adapter.Send.
func TestPushToolStart_EmptyMsgIDIsNoOp(t *testing.T) {
	a := testAdapter(t)

	a.pushToolStart("om_user_1", "", "● A")

	if _, ok := a.popToolStart("om_user_1"); ok {
		t.Error("pop after push with empty msgID returned ok; push should be a no-op")
	}
}

// TestClearToolEvents covers per-userMsgID cleanup. After clear,
// the buffer for that userMsgID is empty; entries for other
// userMsgIDs are untouched. Mirrors the turn-end cleanup path
// (clearToolEvents is exposed but not yet auto-called — it's
// available for future turn-end hooks).
func TestClearToolEvents(t *testing.T) {
	a := testAdapter(t)

	a.pushToolStart("om_user_1", "om_msg_a", "● A")
	a.pushToolStart("om_user_2", "om_msg_b", "● B")

	a.clearToolEvents("om_user_1")

	if _, ok := a.popToolStart("om_user_1"); ok {
		t.Error("om_user_1 pop after clear returned ok; want (_, false)")
	}
	// om_user_2 should be untouched.
	entry, ok := a.popToolStart("om_user_2")
	if !ok || entry.startMsgID != "om_msg_b" {
		t.Errorf("om_user_2 pop after sibling clear = %+v, want om_msg_b", entry)
	}
}

// TestClearAllToolEvents covers full-buffer release (Adapter.Stop
// path). All entries across all userMsgIDs are dropped.
func TestClearAllToolEvents(t *testing.T) {
	a := testAdapter(t)

	a.pushToolStart("om_user_1", "om_msg_a", "● A")
	a.pushToolStart("om_user_2", "om_msg_b", "● B")

	a.clearAllToolEvents()

	if _, ok := a.popToolStart("om_user_1"); ok {
		t.Error("om_user_1 pop after clearAll returned ok; want (_, false)")
	}
	if _, ok := a.popToolStart("om_user_2"); ok {
		t.Error("om_user_2 pop after clearAll returned ok; want (_, false)")
	}
}

// TestMergeToolReply_PatchesSameMessage is the happy-path merge:
// an OutToolStart post followed by an OutToolEnd results in
// exactly ONE Create (the start) and ONE PATCH (the merge). The
// user sees a single thread reply containing both call and
// result lines.
//
// Note: OutToolStart / OutToolEnd also touch the receipt card
// (cold-start card → PATCH in-place via updateFunc). That side
// effect is unrelated to the merge path; we filter the
// sendFunc counts to msg_type=text so the receipt's interactive
// card Create doesn't pollute the count.
func TestMergeToolReply_PatchesSameMessage(t *testing.T) {
	a := testAdapter(t)

	var textCreateCount, updateCount int
	a.sendFunc = func(_ context.Context, _ string, msgType, _ string, _ string, _ bool) (string, error) {
		if msgType == "text" {
			textCreateCount++
		}
		return "om_start_msg_id", nil
	}
	a.mergeTextFunc = func(_ context.Context, messageID, merged string) error {
		updateCount++
		if messageID != "om_start_msg_id" {
			t.Errorf("PATCH target = %q, want om_start_msg_id", messageID)
		}
		// The merged body must contain BOTH the start line and
		// the result line.
		if !strings.Contains(merged, "● Bash(ls)") {
			t.Errorf("merged body missing start line: %q", merged)
		}
		if !strings.Contains(merged, "⎿") {
			t.Errorf("merged body missing result line: %q", merged)
		}
		return nil
	}

	// OutToolStart
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:    messages.OutToolStart,
		ChatID:  "oc_test",
		ReplyTo: "om_user_1",
		Tool:    &messages.ToolInfo{Name: "Bash", Args: "ls"},
	}); err != nil {
		t.Fatalf("Send(OutToolStart): %v", err)
	}

	// OutToolEnd — should PATCH the start, not post a new reply.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:    messages.OutToolEnd,
		ChatID:  "oc_test",
		ReplyTo: "om_user_1",
		Tool:    &messages.ToolInfo{Name: "Bash", Output: "file1\nfile2"},
	}); err != nil {
		t.Fatalf("Send(OutToolEnd): %v", err)
	}

	if textCreateCount != 1 {
		t.Errorf("text Create count = %d, want 1 (only the start, no fresh result reply)", textCreateCount)
	}
	if updateCount != 1 {
		t.Errorf("PATCH count = %d, want 1 (the merge)", updateCount)
	}
}

// TestMergeToolReply_PATCHFailureFallsBackToFreshReply covers the
// resilience contract: if the merge PATCH fails (retry exhausted
// or non-transient error), the result body is still posted as a
// fresh thread reply so the user sees the tool result. No silent
// data loss.
//
// Note: filter to msg_type=text to exclude the receipt card's
// interactive Create.
func TestMergeToolReply_PATCHFailureFallsBackToFreshReply(t *testing.T) {
	a := testAdapter(t)

	var textCreateCount, updateCount int
	a.sendFunc = func(_ context.Context, _ string, msgType, _ string, _ string, _ bool) (string, error) {
		if msgType == "text" {
			textCreateCount++
		}
		return "om_start_msg_id", nil
	}
	a.mergeTextFunc = func(_ context.Context, _ string, _ string) error {
		updateCount++
		return errors.New("simulated PATCH failure")
	}

	// OutToolStart
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:    messages.OutToolStart,
		ChatID:  "oc_test",
		ReplyTo: "om_user_1",
		Tool:    &messages.ToolInfo{Name: "Bash", Args: "ls"},
	}); err != nil {
		t.Fatalf("Send(OutToolStart): %v", err)
	}

	// OutToolEnd — PATCH fails; fallback posts a fresh thread
	// reply with the result body. Send should NOT return the
	// PATCH error to the caller (the data was preserved via the
	// fallback).
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:    messages.OutToolEnd,
		ChatID:  "oc_test",
		ReplyTo: "om_user_1",
		Tool:    &messages.ToolInfo{Name: "Bash", Output: "file1\nfile2"},
	}); err != nil {
		t.Fatalf("Send(OutToolEnd) returned error after PATCH failure: %v", err)
	}

	if textCreateCount != 2 {
		t.Errorf("text Create count = %d, want 2 (start + fallback result after PATCH failure)", textCreateCount)
	}
	if updateCount != 1 {
		t.Errorf("PATCH count = %d, want 1 (the failed merge attempt)", updateCount)
	}
}

// TestMergeToolReply_OrphanEndFallsBackToFreshReply covers the
// orphan-End path: an OutToolEnd arrives without a matching
// OutToolStart in the buffer. The result body is posted as a
// fresh thread reply (pre-F-38 behaviour preserved for the
// unhappy path).
func TestMergeToolReply_OrphanEndFallsBackToFreshReply(t *testing.T) {
	a := testAdapter(t)

	var textCreateCount, updateCount int
	a.sendFunc = func(_ context.Context, _ string, msgType, _ string, _ string, _ bool) (string, error) {
		if msgType == "text" {
			textCreateCount++
		}
		return "om_fresh_msg_id", nil
	}
	a.mergeTextFunc = func(_ context.Context, _ string, _ string) error {
		updateCount++
		return nil
	}

	// OutToolEnd with no preceding OutToolStart — buffer is
	// empty, popToolStart returns miss, fallback posts fresh.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:    messages.OutToolEnd,
		ChatID:  "oc_test",
		ReplyTo: "om_user_1",
		Tool:    &messages.ToolInfo{Name: "Bash", Output: "file1"},
	}); err != nil {
		t.Fatalf("Send(OutToolEnd) orphan: %v", err)
	}

	if textCreateCount != 1 {
		t.Errorf("text Create count = %d, want 1 (the orphan fallback)", textCreateCount)
	}
	if updateCount != 0 {
		t.Errorf("PATCH count = %d, want 0 (orphan End should NOT PATCH)", updateCount)
	}
}

// TestMergeToolReply_ParallelToolUse covers the parallel-tool_use
// case where multiple OutToolStart events arrive before any
// matching End. Each End must edit the correct (FIFO-positioned)
// Start's message, leaving the others unpaired.
//
// Note: sendFunc is called for both thread replies AND receipt
// cards. We track only the text-type calls so the receipt card
// Create doesn't increment the per-tool counter.
func TestMergeToolReply_ParallelToolUse(t *testing.T) {
	a := testAdapter(t)

	var textSendIdx int
	patchTargets := []string{}
	a.sendFunc = func(_ context.Context, _ string, msgType, _ string, _ string, _ bool) (string, error) {
		if msgType != "text" {
			// Receipt card or other interactive content — return a
			// dummy msg_id without incrementing textSendIdx.
			return "om_card", nil
		}
		// Text-type (thread reply) — return a unique msg_id per
		// call so we can verify the FIFO pairing.
		id := "om_msg_" + string(rune('A'+textSendIdx))
		textSendIdx++
		return id, nil
	}
	a.mergeTextFunc = func(_ context.Context, messageID, _ string) error {
		patchTargets = append(patchTargets, messageID)
		return nil
	}

	// Two parallel tool_use blocks: Start A, Start B, End A, End B.
	for _, name := range []string{"Read", "Bash"} {
		if err := a.Send(context.Background(), messages.OutboundMessage{
			Kind:    messages.OutToolStart,
			ChatID:  "oc_test",
			ReplyTo: "om_user_1",
			Tool:    &messages.ToolInfo{Name: name},
		}); err != nil {
			t.Fatalf("Send(OutToolStart %s): %v", name, err)
		}
	}
	// End for first tool (Read). Should PATCH om_msg_A.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:    messages.OutToolEnd,
		ChatID:  "oc_test",
		ReplyTo: "om_user_1",
		Tool:    &messages.ToolInfo{Name: "Read", Output: "x"},
	}); err != nil {
		t.Fatalf("Send(OutToolEnd Read): %v", err)
	}
	// End for second tool (Bash). Should PATCH om_msg_B.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:    messages.OutToolEnd,
		ChatID:  "oc_test",
		ReplyTo: "om_user_1",
		Tool:    &messages.ToolInfo{Name: "Bash", Output: "y"},
	}); err != nil {
		t.Fatalf("Send(OutToolEnd Bash): %v", err)
	}

	wantTargets := []string{"om_msg_A", "om_msg_B"}
	if len(patchTargets) != len(wantTargets) {
		t.Fatalf("PATCH sequence = %v, want %v", patchTargets, wantTargets)
	}
	for i := range wantTargets {
		if patchTargets[i] != wantTargets[i] {
			t.Errorf("PATCH[%d] target = %q, want %q (FIFO order)",
				i, patchTargets[i], wantTargets[i])
		}
	}
}

// TestMergeToolReply_DifferentUserMsgIDsAreIndependent covers the
// 1-turn-1-userMsgID invariant (SPEC §2.2): pending entries for
// one userMsgID must not be matched by an End from another turn.
func TestMergeToolReply_DifferentUserMsgIDsAreIndependent(t *testing.T) {
	a := testAdapter(t)

	var textCreateCount, updateCount int
	a.sendFunc = func(_ context.Context, _ string, msgType, _ string, _ string, _ bool) (string, error) {
		if msgType == "text" {
			textCreateCount++
		}
		return "om_start", nil
	}
	a.mergeTextFunc = func(_ context.Context, _ string, _ string) error {
		updateCount++
		return nil
	}

	// Start on turn 1 (userMsgID = om_turn_1).
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:    messages.OutToolStart,
		ChatID:  "oc_test",
		ReplyTo: "om_turn_1",
		Tool:    &messages.ToolInfo{Name: "Read"},
	}); err != nil {
		t.Fatalf("Send start turn1: %v", err)
	}

	// End on turn 2 (userMsgID = om_turn_2) — orphan for this
	// buffer, falls back to fresh post. Must NOT PATCH the turn-1
	// start's message_id.
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:    messages.OutToolEnd,
		ChatID:  "oc_test",
		ReplyTo: "om_turn_2",
		Tool:    &messages.ToolInfo{Name: "Read", Output: "x"},
	}); err != nil {
		t.Fatalf("Send end turn2: %v", err)
	}

	if textCreateCount != 2 {
		t.Errorf("text Create count = %d, want 2 (turn1 start + turn2 orphan-end fallback)", textCreateCount)
	}
	if updateCount != 0 {
		t.Errorf("PATCH count = %d, want 0 (orphan End across turns must NOT cross-match)", updateCount)
	}
}

// TestMergeToolReply_EmptyMsgIDRefusesPatch covers the defensive
// guard: mergeToolReply returns a clear error when called with
// an empty startMsgID, rather than silently issuing a PATCH that
// Feishu would reject.
func TestMergeToolReply_EmptyMsgIDRefusesPatch(t *testing.T) {
	a := testAdapter(t)

	err := a.mergeToolReply(context.Background(), "", "anything")
	if err == nil {
		t.Fatal("mergeToolReply with empty msgID returned nil; want clear error")
	}
	if !strings.Contains(err.Error(), "empty startMsgID") {
		t.Errorf("error message = %q, want it to mention empty startMsgID", err.Error())
	}
}
