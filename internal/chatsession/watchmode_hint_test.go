// F-watch first-drop UX: when a non-mention group message is
// dropped because WatchMode==WatchModeMention (the default), the
// user gets a one-time hint pointing them at `/watch on`. The
// hint fires at most once per chat, persists across daemon
// restarts, and never fires when the message IS @-mentioned (DM
// chats always carry HasMention=true, so they're naturally
// exempt; slash commands never reach HandleInbound at all).
//
// These tests live next to watchmode_gate_test.go because they
// share the F-watch subject and the Manager-with-wired-deps
// setup pattern (csFile + Emitter).
package chatsession

import (
	"context"
	"errors"
	"github.com/cnlangzi/nightme/internal/chatstore"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
)

// makeHintTestManager builds a Manager wired with both the
// in-package recording Emitter (so we can assert what was sent)
// and a real ChatSessionFile at the given path (so hint-flag
// persistence is exercised). Pass empty path to skip persistence
// — useful for tests that don't care about cross-restart
// behaviour.
func makeHintTestManager(t *testing.T, storePath string) (*Manager, *testEmitter, *chatstore.Store) {
	t.Helper()
	em := &testEmitter{}
	mgr := NewManager().
		WithEmitter(em).
		WithPrimaryAgent("claude")
	var csFile *chatstore.Store
	if storePath != "" {
		f, err := chatstore.New(storePath)
		if err != nil {
			t.Fatalf("OpenChatSessionFile(%q): %v", storePath, err)
		}
		mgr = mgr.WithPersistence(f, nil)
		csFile = f
	}
	return mgr, em, csFile
}

// newDropInboundMessage constructs a minimal InboundMessage
// shaped like a group-chat non-mention message — just enough
// for HandleInbound to take the drop branch without panicking.
func newDropInboundMessage(chatID string) *messages.InboundMessage {
	return &messages.InboundMessage{
		ChatID:     chatID,
		UserID:     "u_test",
		Text:       "hi everyone",
		MessageID:  "msg-1",
		Time:       time.Now(),
		HasMention: false, // group message that didn't @ bot
	}
}

// TestManager_FirstDrop_EmitsHint: the very first time the drop
// branch fires for a chat, the Emitter receives the `/watch on`
// hint and the registry records WatcherHintEmitted=true.
func TestManager_FirstDrop_EmitsHint(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "chat_sessions.json")

	mgr, em, csFile := makeHintTestManager(t, storePath)

	// Pre-create the ChatSession so AcceptInbound drops
	// (WatchMode=Mention + HasMention=false + cs exists).
	cs, err := mgr.GetOrCreate("oc_hint", "claude")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if cs.WatchMode() != WatchModeMention {
		t.Fatalf("default WatchMode = %q, want %q", cs.WatchMode(), WatchModeMention)
	}

	if err := mgr.HandleInbound(context.Background(), newDropInboundMessage("oc_hint")); err != nil {
		t.Fatalf("HandleInbound: %v", err)
	}

	if got := len(em.Sent); got != 1 {
		t.Fatalf("emitter Send calls = %d, want 1 (the hint)", got)
	}
	hint := em.Sent[0]
	if hint.ChatID != "oc_hint" {
		t.Errorf("hint ChatID = %q, want %q", hint.ChatID, "oc_hint")
	}
	if hint.Kind != messages.OutReply {
		t.Errorf("hint Kind = %v, want OutReply", hint.Kind)
	}
	if !strings.Contains(hint.Text, "/watch on") {
		t.Errorf("hint text %q does not mention /watch on", hint.Text)
	}

	// Registry tombstone must be persisted. We don't assert on
	// the file format — just that GetByChat reports the flag.
	entry, ok := csFile.Get("oc_hint")
	if !ok {
		t.Fatalf("registry has no entry for chat after hint")
	}
	if !entry.WatcherHintEmitted {
		t.Errorf("WatcherHintEmitted = false, want true after first hint")
	}
}

// TestManager_SubsequentDrop_NoHint: the second (and Nth) drop
// in the same chat must NOT re-emit the hint. Verifies the
// runtime tombstone check (independent of the file-on-disk
// check that the persistence test covers).
func TestManager_SubsequentDrop_NoHint(t *testing.T) {
	dir := t.TempDir()
	mgr, em, _ := makeHintTestManager(t, filepath.Join(dir, "chat_sessions.json"))

	if _, err := mgr.GetOrCreate("oc_repeat", "claude"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	for i := 1; i <= 3; i++ {
		msg := newDropInboundMessage("oc_repeat")
		msg.MessageID = "msg-" + string(rune('0'+i))
		if err := mgr.HandleInbound(context.Background(), msg); err != nil {
			t.Fatalf("HandleInbound #%d: %v", i, err)
		}
	}

	if got := len(em.Sent); got != 1 {
		t.Errorf("emitter Send calls across 3 drops = %d, want 1 (only the first)", got)
	}
}

// TestManager_HintPersistsAcrossRestart: simulate a daemon
// restart by discarding the original Manager + re-opening the
// ChatSessionFile at the same path, wiring it to a fresh
// Manager, and running RestoreFromRegistry (the production
// startup path). The tombstone must hydrate from the persisted
// entry so the first drop on the restarted daemon does NOT
// re-emit.
//
// The deeper regression test for the hydration path itself
// lives in TestManager_HintSurvivesRestore; this one exercises
// the full HandleInbound round-trip on a restored daemon.
func TestManager_HintPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "chat_sessions.json")

	// Phase 1: original daemon. Trigger first drop → hint fires.
	mgr1, em1, _ := makeHintTestManager(t, storePath)
	if _, err := mgr1.GetOrCreate("oc_restart", "claude"); err != nil {
		t.Fatalf("GetOrCreate #1: %v", err)
	}
	if err := mgr1.HandleInbound(context.Background(), newDropInboundMessage("oc_restart")); err != nil {
		t.Fatalf("HandleInbound #1: %v", err)
	}
	if got := countHints(em1.Sent); got != 1 {
		t.Fatalf("phase 1: hint count = %d, want 1", got)
	}

	// Phase 2: simulate restart — new Manager, same file path.
	// RestoreFromRegistry is the production startup sequence
	// and must hydrate the tombstone from disk.
	mgr2, em2, csFile2 := makeHintTestManager(t, storePath)
	if err := mgr2.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}

	// Sanity: the on-disk entry still carries the flag (the
	// hydration just mirrors what's on disk; both must agree).
	entry, ok := csFile2.Get("oc_restart")
	if !ok {
		t.Fatalf("phase 2: reopened file has no entry for chat")
	}
	if !entry.WatcherHintEmitted {
		t.Fatalf("phase 2: persisted WatcherHintEmitted = false, want true")
	}

	// And the hydrated CS in the new Manager must see the
	// tombstone too — that's the runtime check that
	// short-circuits the drop branch.
	cs := mgr2.Get("oc_restart")
	if cs == nil {
		t.Fatalf("phase 2: hydrated CS missing")
	}
	if !cs.WatcherHintEmitted() {
		t.Fatalf("phase 2: hydrated CS WatcherHintEmitted = false, want true")
	}

	if err := mgr2.HandleInbound(context.Background(), newDropInboundMessage("oc_restart")); err != nil {
		t.Fatalf("HandleInbound #2: %v", err)
	}
	if got := countHints(em2.Sent); got != 0 {
		t.Errorf("phase 2: post-restart drop re-emitted hint (%d times); want 0", got)
	}
}

// TestManager_HintOnlyForHasMentionFalse: when HasMention=true,
// the drop branch never fires (AcceptInbound short-circuits to
// accept). So the hint must never be emitted regardless of how
// many @-mentions arrive. We assert by counting messages that
// actually contain "/watch on" — other replies (e.g. the
// "send /cwd first" error) are emitted through the same Emitter
// but are not the hint, so a raw `len(em.Sent)` count would be
// misleading.
func TestManager_HintOnlyForHasMentionFalse(t *testing.T) {
	dir := t.TempDir()
	mgr, em, _ := makeHintTestManager(t, filepath.Join(dir, "chat_sessions.json"))

	if _, err := mgr.GetOrCreate("oc_mention", "claude"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	for i := 1; i <= 3; i++ {
		msg := newDropInboundMessage("oc_mention")
		msg.HasMention = true // DM or explicit @-mention
		msg.MessageID = "m-" + string(rune('0'+i))
		if err := mgr.HandleInbound(context.Background(), msg); err != nil {
			t.Fatalf("HandleInbound #%d: %v", i, err)
		}
	}

	if got := countHints(em.Sent); got != 0 {
		t.Errorf("@-mention messages emitted %d hints; want 0", got)
	}
}

// TestManager_HintOnlyInWatchModeMention: when WatchMode==All,
// AcceptInbound accepts every message — the drop branch never
// fires, so the hint must never be emitted. Same count-by-content
// approach as TestManager_HintOnlyForHasMentionFalse (other
// replies share the Emitter).
func TestManager_HintOnlyInWatchModeMention(t *testing.T) {
	dir := t.TempDir()
	mgr, em, _ := makeHintTestManager(t, filepath.Join(dir, "chat_sessions.json"))

	cs, err := mgr.GetOrCreate("oc_all", "claude")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if err := cs.SetWatchMode(WatchModeAll); err != nil {
		t.Fatalf("SetWatchMode(All): %v", err)
	}

	for i := 1; i <= 3; i++ {
		msg := newDropInboundMessage("oc_all")
		msg.MessageID = "m-" + string(rune('0'+i))
		if err := mgr.HandleInbound(context.Background(), msg); err != nil {
			t.Fatalf("HandleInbound #%d: %v", i, err)
		}
	}

	if got := countHints(em.Sent); got != 0 {
		t.Errorf("WatchMode=All non-mention messages emitted %d hints; want 0", got)
	}
}

// countHints returns how many of the sent messages look like the
// `/watch on` hint (contain the substring "/watch on"). Other
// messages on the same Emitter — error replies, future reply
// kinds — are not counted.
func countHints(sent []messages.OutboundMessage) int {
	n := 0
	for _, m := range sent {
		if strings.Contains(m.Text, "/watch on") {
			n++
		}
	}
	return n
}

// TestManager_HintPersistsWithoutFullChatSession: covers the
// "very first message in a brand-new chat" path — there is no
// pre-existing ChatSession because no @-mention has caused
// GetOrCreate yet. The helper must defensively GetOrCreate so
// the tombstone has a stable ChatSession home. The full entry
// (including the hint flag) must be persisted so a subsequent
// restart doesn't re-emit.
//
// In production this case is never reached via AcceptInbound
// (cs==nil + !HasMention returns true → never drops), but the
// helper contract has to handle nil defensively — passing nil
// is the test signal that says "behave as if the drop branch
// fired for a brand-new chat". Pinning the behaviour guards
// against silent regressions in the GetOrCreate fallback.
func TestManager_HintPersistsWithoutFullChatSession(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "chat_sessions.json")
	mgr, em, csFile := makeHintTestManager(t, storePath)

	// Pass nil cs — simulates the defensive fallback path. The
	// helper must GetOrCreate internally and stamp the flag on
	// the freshly-allocated ChatSession.
	mgr.maybeEmitWatcherHint(context.Background(), newDropInboundMessage("oc_brand_new"), nil)

	if got := len(em.Sent); got != 1 {
		t.Fatalf("emitter Send calls = %d, want 1", got)
	}

	entry, ok := csFile.Get("oc_brand_new")
	if !ok {
		t.Fatalf("registry has no entry for chat after hint")
	}
	if !entry.WatcherHintEmitted {
		t.Errorf("WatcherHintEmitted = false, want true after first hint")
	}
	if entry.CreatedAt.IsZero() {
		t.Errorf("entry CreatedAt is zero; GetOrCreate should stamp it")
	}
}

// TestManager_HintNoPersistence_BestEffort: with no csFile
// wired (test/early-startup context), the hint still fires and
// the in-memory CS tombstone suppresses re-emits WITHIN this
// process lifetime. Across a restart with no persistence, the
// flag would be lost and the next hint would re-emit — that
// best-effort behaviour is what we pin here (and the
// persistent equivalent is covered by
// TestManager_HintSurvivesRestore).
func TestManager_HintNoPersistence_BestEffort(t *testing.T) {
	// Within-process: in-memory tombstone suppresses re-emits.
	mgr, em, _ := makeHintTestManager(t, "" /* no persistence */)
	for i := 1; i <= 3; i++ {
		// Pass nil — the helper GetOrCreates a CS in-memory on
		// the first call; subsequent calls hit the in-memory
		// tombstone check and skip.
		mgr.maybeEmitWatcherHint(context.Background(), newDropInboundMessage("oc_nopersist"), nil)
	}
	if got := countHints(em.Sent); got != 1 {
		t.Errorf("within process: hint count across 3 calls = %d, want 1 (in-memory tombstone suppresses re-emits)", got)
	}

	// Cross-restart: discard the original Manager, create a
	// fresh one with no persistence. The in-memory tombstone
	// is gone, so the next hint must re-emit. Pinning this
	// guards against a future "fix" that would silently start
	// persisting the flag to a side channel that we then can't
	// reason about — the documented contract is that without
	// ChatSessionFile wired, the flag is genuinely ephemeral.
	mgr2, em2, _ := makeHintTestManager(t, "" /* no persistence */)
	mgr2.maybeEmitWatcherHint(context.Background(), newDropInboundMessage("oc_nopersist_restart"), nil)
	if got := countHints(em2.Sent); got != 1 {
		t.Errorf("post-restart: hint count = %d, want 1 (no persistence → flag genuinely lost across Manager boundary)", got)
	}
}

// TestManager_HintDoesNotBumpLastInteractionAt: regression
// test for the "hint emission shouldn't count as user
// interaction" bug. The hint is a system event triggered by a
// dropped user message, not a user interaction itself. If we
// bump lastInteractionAt on hint emission, the field would
// misrepresent when the user actually last interacted, and
// (future) idle-expiry decisions would mask the fact that the
// chat has only ever received dropped non-mention traffic.
//
// Robustness: this test deliberately does NOT depend on
// clock-resolution guarantees. Two consecutive time.Now() calls
// can return the SAME nanosecond value on Windows (default
// 15.6ms tick + QPC throttling on the GitHub Actions runner),
// which broke an earlier version that asserted
// `afterBaseline.After(beforeBaseline)`. Instead we capture
// the baseline from the persisted ChatSessionEntry (the actual
// value SetWatchMode wrote) and sleep long enough — 20ms, well
// past any platform's tick — that any (incorrect) bump in
// MarkWatcherHintEmitted would be measurable as a strict-greater
// timestamp. The final assertion is `Equal`, not `After`, so
// it holds whether the in-memory clock has moved or not.
func TestManager_HintDoesNotBumpLastInteractionAt(t *testing.T) {
	dir := t.TempDir()
	mgr, _, csFile := makeHintTestManager(t, filepath.Join(dir, "chat_sessions.json"))
	cs, err := mgr.GetOrCreate("oc_idle", "claude")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	// Establish a known baseline lastInteractionAt by setting
	// WatchMode (which DOES bump it — user-initiated command).
	if err := cs.SetWatchMode(WatchModeAll); err != nil {
		t.Fatalf("SetWatchMode baseline: %v", err)
	}

	// Capture the baseline from the PERSISTED entry — this is
	// the actual value SetWatchMode wrote, regardless of the
	// caller's in-memory clock resolution. Reading the entry
	// bypasses any race between the SetWatchMode write and
	// subsequent in-memory reads.
	baselineEntry, ok := csFile.Get("oc_idle")
	if !ok {
		t.Fatalf("baseline entry missing from registry")
	}
	baselineTime := baselineEntry.LastInteractionAt

	// Sleep long enough that ANY clock (including coarse
	// Windows ticks) will measurably advance. 20ms is well
	// past the Windows 15.6ms default tick — if
	// MarkWatcherHintEmitted were to bump lastInteractionAt,
	// the new value would be at least 20ms later than
	// baselineTime, which any subsequent read would surface.
	time.Sleep(20 * time.Millisecond)

	// MarkWatcherHintEmitted must NOT bump lastInteractionAt.
	if err := cs.MarkWatcherHintEmitted(); err != nil {
		t.Fatalf("MarkWatcherHintEmitted: %v", err)
	}
	afterHint := cs.LastInteractionAt()
	if !afterHint.Equal(baselineTime) {
		t.Errorf("MarkWatcherHintEmitted bumped lastInteractionAt: %v → %v (must not — hint is a system event, not a user interaction)",
			baselineTime, afterHint)
	}

	// And the on-disk entry must agree — the persisted
	// timestamp must match what was there before the hint.
	entry, ok := csFile.Get("oc_idle")
	if !ok {
		t.Fatalf("registry has no entry for chat after hint")
	}
	if !entry.LastInteractionAt.Equal(baselineTime) {
		t.Errorf("on-disk LastInteractionAt = %v, want %v (hint must not bump)",
			entry.LastInteractionAt, baselineTime)
	}
}

// TestManager_HintSurvivesSetWatchMode: regression test for the
// entryLocked tombstone-clobber bug. Before the fix, the hint
// flag was persisted only via a direct csFile.Save in
// maybeEmitWatcherHint, but every other persist path
// (SetWatchMode / SetThinkMode / SetToolsMode / SetSelectedCwd
// / ClearSelectedCwd / QueueUserMessage) calls
// persistChatEntry → entryLocked(), and entryLocked originally
// did NOT include WatcherHintEmitted. So the very next
// SetWatchMode call after the hint would silently overwrite
// the tombstone back to false, and the next daemon restart
// would re-emit.
//
// After the fix, WatcherHintEmitted is a first-class CS field
// included in entryLocked() and hydrated from the persisted
// entry in RestoreFromRegistry. This test exercises the
// critical SetWatchMode-after-hint sequence and asserts the
// flag survives — both in the in-memory state and on disk.
func TestManager_HintSurvivesSetWatchMode(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "chat_sessions.json")
	mgr, em, csFile := makeHintTestManager(t, storePath)

	cs, err := mgr.GetOrCreate("oc_clobber", "claude")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	// Trigger the hint via the drop branch (cs is non-nil
	// here, matching the production call path).
	if err := mgr.HandleInbound(context.Background(), newDropInboundMessage("oc_clobber")); err != nil {
		t.Fatalf("HandleInbound (first drop): %v", err)
	}
	if !cs.WatcherHintEmitted() {
		t.Fatalf("after first drop: in-memory WatcherHintEmitted = false, want true")
	}
	if countHints(em.Sent) != 1 {
		t.Fatalf("after first drop: hint count = %d, want 1", countHints(em.Sent))
	}

	// The critical mutation: SetWatchMode triggers
	// persistChatEntry → entryLocked(). Pre-fix this would
	// clobber WatcherHintEmitted back to false on disk.
	if err := cs.SetWatchMode(WatchModeAll); err != nil {
		t.Fatalf("SetWatchMode(All): %v", err)
	}

	// In-memory state must still carry the flag (SetWatchMode
	// doesn't touch watcherHintEmitted; the fix means
	// entryLocked now reads it back).
	if !cs.WatcherHintEmitted() {
		t.Errorf("after SetWatchMode: in-memory WatcherHintEmitted = false, want true (clobber bug regression)")
	}

	// On-disk entry must also carry the flag. This is the
	// assertion that pre-fix would have failed.
	entry, ok := csFile.Get("oc_clobber")
	if !ok {
		t.Fatalf("after SetWatchMode: registry lost entry for chat")
	}
	if !entry.WatcherHintEmitted {
		t.Errorf("after SetWatchMode: on-disk WatcherHintEmitted = false, want true (clobber bug regression)")
	}

	// And a subsequent drop must NOT re-emit.
	if err := mgr.HandleInbound(context.Background(), newDropInboundMessage("oc_clobber")); err != nil {
		t.Fatalf("HandleInbound (second drop): %v", err)
	}
	if countHints(em.Sent) != 1 {
		t.Errorf("after SetWatchMode + second drop: hint count = %d, want still 1", countHints(em.Sent))
	}
}

// TestManager_HintSurvivesRestore: regression test that the
// daemon-restart round-trip preserves the tombstone. The hint
// fires, we close the file, reopen at the same path, run
// RestoreFromRegistry on a fresh Manager — the resulting CS
// must have WatcherHintEmitted=true so the very first drop on
// the restored daemon does NOT re-emit.
//
// Pre-fix this would have failed at the GetByChat step (the
// stub entry written by maybeEmitWatcherHint would be clobbered
// by the next SetWatchMode call before the simulated restart,
// or — if no SetWatchMode happened — the flag would still be on
// disk from the stub, but the in-memory cs on the restarted
// daemon would have WatcherHintEmitted=false because
// RestoreFromRegistry didn't hydrate it).
func TestManager_HintSurvivesRestore(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "chat_sessions.json")

	// Phase 1: original daemon. Hint fires, flag is stamped
	// on both the in-memory CS and the on-disk entry.
	mgr1, em1, _ := makeHintTestManager(t, storePath)
	cs1, err := mgr1.GetOrCreate("oc_restore", "claude")
	if err != nil {
		t.Fatalf("GetOrCreate #1: %v", err)
	}
	if err := mgr1.HandleInbound(context.Background(), newDropInboundMessage("oc_restore")); err != nil {
		t.Fatalf("HandleInbound #1: %v", err)
	}
	if countHints(em1.Sent) != 1 {
		t.Fatalf("phase 1: hint count = %d, want 1", countHints(em1.Sent))
	}
	if !cs1.WatcherHintEmitted() {
		t.Fatalf("phase 1: in-memory WatcherHintEmitted = false, want true")
	}

	// Phase 2: simulate restart with a fresh Manager + reopen
	// the ChatSessionFile at the same path.
	mgr2, em2, csFile2 := makeHintTestManager(t, storePath)
	if err := mgr2.RestoreFromRegistry(); err != nil {
		t.Fatalf("RestoreFromRegistry: %v", err)
	}
	cs2 := mgr2.Get("oc_restore")
	if cs2 == nil {
		t.Fatalf("phase 2: restored CS missing")
	}
	if !cs2.WatcherHintEmitted() {
		t.Errorf("phase 2: hydrated WatcherHintEmitted = false, want true (restore hydration regression)")
	}

	// On-disk entry must also carry the flag.
	entry, ok := csFile2.Get("oc_restore")
	if !ok {
		t.Fatalf("phase 2: on-disk entry missing")
	}
	if !entry.WatcherHintEmitted {
		t.Errorf("phase 2: on-disk WatcherHintEmitted = false, want true")
	}

	// Phase 3: a drop on the restored daemon must NOT
	// re-emit — the hydrated tombstone must short-circuit.
	if err := mgr2.HandleInbound(context.Background(), newDropInboundMessage("oc_restore")); err != nil {
		t.Fatalf("HandleInbound #2: %v", err)
	}
	if countHints(em2.Sent) != 0 {
		t.Errorf("phase 3: post-restore drop re-emitted %d hints; want 0", countHints(em2.Sent))
	}
}

// TestManager_HintRetriesOnSendFailure: regression test for the
// "stamp-on-Send-failure-permanently-denies-hint" bug. Pre-fix,
// the Send error was silently discarded AND the tombstone was
// stamped unconditionally afterwards — a transient channel
// failure meant the user never saw the hint for that chat, ever.
// After the fix, a Send error leaves the tombstone false, so
// the very next non-mention drop retries.
func TestManager_HintRetriesOnSendFailure(t *testing.T) {
	dir := t.TempDir()
	mgr, em, csFile := makeHintTestManager(t, filepath.Join(dir, "chat_sessions.json"))
	em.SendErr = errors.New("feishu 500: transient") // simulate channel failure

	cs, err := mgr.GetOrCreate("oc_retry", "claude")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	// First drop: Send fails. Flag must NOT be marked. Hint
	// goes into the Sent slice (testEmitter records even on
	// error) but the user-visible contract is that the
	// tombstone stays false.
	if err := mgr.HandleInbound(context.Background(), newDropInboundMessage("oc_retry")); err != nil {
		t.Fatalf("HandleInbound #1 (Send fails): %v", err)
	}
	if cs.WatcherHintEmitted() {
		t.Errorf("after Send failure: WatcherHintEmitted = true, want false (must not stamp on Send error)")
	}
	if entry, ok := csFile.Get("oc_retry"); ok && entry.WatcherHintEmitted {
		t.Errorf("after Send failure: on-disk WatcherHintEmitted = true, want false")
	}

	// Second drop: still failing. Still no stamp. (The
	// operator sees two Warn logs; user sees no hint yet.)
	if err := mgr.HandleInbound(context.Background(), newDropInboundMessage("oc_retry")); err != nil {
		t.Fatalf("HandleInbound #2 (Send still fails): %v", err)
	}
	if cs.WatcherHintEmitted() {
		t.Errorf("after second Send failure: WatcherHintEmitted = true, want false")
	}

	// Third drop: channel recovers (clear SendErr). Now the
	// hint must fire successfully and the tombstone must be
	// stamped — exactly once across all three drops.
	em.SendErr = nil
	if err := mgr.HandleInbound(context.Background(), newDropInboundMessage("oc_retry")); err != nil {
		t.Fatalf("HandleInbound #3 (Send succeeds): %v", err)
	}
	if !cs.WatcherHintEmitted() {
		t.Errorf("after Send recovery: WatcherHintEmitted = false, want true")
	}
	if got := countHints(em.Sent); got != 3 {
		// testEmitter records every Send call regardless of
		// return value, so all 3 attempts appear in Sent even
		// though the user only sees the third one.
		t.Errorf("Sent records = %d, want 3 (every Send call recorded, but only the third is user-visible)", got)
	}
	if entry, ok := csFile.Get("oc_retry"); !ok || !entry.WatcherHintEmitted {
		t.Errorf("after Send recovery: on-disk WatcherHintEmitted = false, want true")
	}

	// Fourth drop: must short-circuit now that the tombstone
	// is stamped. The check happens BEFORE Send is even
	// attempted, so a Send failure at this point is
	// irrelevant — we never reach the Emitter. This is the
	// "no spam" half of the contract: once stamped, the
	// tombstone is authoritative regardless of channel health.
	em.SendErr = errors.New("feishu 500: post-recovery")
	if err := mgr.HandleInbound(context.Background(), newDropInboundMessage("oc_retry")); err != nil {
		t.Fatalf("HandleInbound #4 (post-recovery Send fails): %v", err)
	}
	if got := countHints(em.Sent); got != 3 {
		// Only the 3 prior Sends appear in Sent. The 4th drop
		// short-circuited at the WatcherHintEmitted() check
		// and never reached em.Send. This is the correct
		// behaviour: tombstone-stamped → no further attempts,
		// success or failure.
		t.Errorf("Sent records = %d, want 3 (4th drop must short-circuit at tombstone check, not call Send)", got)
	}
	if !cs.WatcherHintEmitted() {
		t.Errorf("after post-recovery failure: WatcherHintEmitted = false, want true (tombstone must persist once set)")
	}
}

// TestManager_HintRetriesAcrossRestart_NoPersistence: when the
// daemon restarts AND persistence is absent, the in-memory
// tombstone is gone AND there's no on-disk record. A fresh
// drop must retry the hint from scratch — exactly as if the
// user just added the bot to the chat. Pins the "best-effort
// re-emit across restart when no persistence" contract.
func TestManager_HintRetriesAcrossRestart_NoPersistence(t *testing.T) {
	// Phase 1: original Manager, no persistence. Hint fires,
	// in-memory tombstone set.
	mgr1, em1, _ := makeHintTestManager(t, "" /* no persistence */)
	cs1, err := mgr1.GetOrCreate("oc_ephemeral", "claude")
	if err != nil {
		t.Fatalf("GetOrCreate #1: %v", err)
	}
	if err := mgr1.HandleInbound(context.Background(), newDropInboundMessage("oc_ephemeral")); err != nil {
		t.Fatalf("HandleInbound #1: %v", err)
	}
	if !cs1.WatcherHintEmitted() {
		t.Fatalf("phase 1: in-memory WatcherHintEmitted = false, want true")
	}
	if countHints(em1.Sent) != 1 {
		t.Fatalf("phase 1: hint count = %d, want 1", countHints(em1.Sent))
	}

	// Phase 2: simulate restart — discard mgr1, build mgr2.
	// Without persistence, the in-memory tombstone is lost AND
	// there's no on-disk record to hydrate. The next drop
	// must fire the hint again.
	mgr2, em2, _ := makeHintTestManager(t, "" /* no persistence */)
	cs2, err := mgr2.GetOrCreate("oc_ephemeral", "claude")
	if err != nil {
		t.Fatalf("GetOrCreate #2: %v", err)
	}
	if cs2.WatcherHintEmitted() {
		t.Fatalf("phase 2: fresh CS already has WatcherHintEmitted = true (no persistence to hydrate from)")
	}
	if err := mgr2.HandleInbound(context.Background(), newDropInboundMessage("oc_ephemeral")); err != nil {
		t.Fatalf("HandleInbound #2: %v", err)
	}
	if countHints(em2.Sent) != 1 {
		t.Errorf("phase 2: hint count = %d, want 1 (post-restart drop should re-emit)", countHints(em2.Sent))
	}
	if !cs2.WatcherHintEmitted() {
		t.Errorf("phase 2: WatcherHintEmitted = false after re-emit, want true")
	}
}

// TestManager_HintPerChatLockIndependence: regression test for
// the Manager-wide hintMu contention bug. Pre-fix, a single
// Manager-level mutex held across Emitter.Send meant one slow
// hint in chat A blocked hint attempts in every other chat.
// After the fix (per-chat sync.Map[*sync.Mutex]), drops in
// different chats must run independently — a slow/blocked Send
// in chat A must NOT block chat B's hint attempt.
//
// We simulate slowness via a blocking channel: chat A's Send
// blocks until we release it. While A is blocked, chat B's
// hint must complete and stamp its tombstone. If they were
// sharing a Manager-wide mutex, B would be stuck behind A.
func TestManager_HintPerChatLockIndependence(t *testing.T) {
	dir := t.TempDir()
	mgr, _, _ := makeHintTestManager(t, filepath.Join(dir, "chat_sessions.json"))

	// Custom Emitter that blocks Send for chat A until
	// releaseCh is closed, and returns immediately for all
	// other chats. This simulates a slow Feishu API round-trip
	// for chat A only.
	releaseA := make(chan struct{})
	em := &blockingEmitter{
		releaseA:      releaseA,
		defaultResult: &messages.OutboundMessage{},
	}
	mgr.WithEmitter(em)

	if _, err := mgr.GetOrCreate("oc_A", "claude"); err != nil {
		t.Fatalf("GetOrCreate A: %v", err)
	}
	if _, err := mgr.GetOrCreate("oc_B", "claude"); err != nil {
		t.Fatalf("GetOrCreate B: %v", err)
	}

	// Kick off chat A's hint in a goroutine — it'll block on
	// the Emitter until we close releaseA.
	aDone := make(chan error, 1)
	go func() {
		aDone <- mgr.HandleInbound(context.Background(), newDropInboundMessage("oc_A"))
	}()

	// Give A's hint a moment to enter the lock and start
	// blocking on Emitter.Send. We don't have a hook for
	// "Send was entered", so use a tiny sleep as a poor man's
	// barrier. 20ms is generous for goroutine scheduling.
	time.Sleep(20 * time.Millisecond)

	// Now drive chat B's hint. If the per-chat lock works, B
	// completes immediately. If the bug regresses (Manager-wide
	// lock held across A's Send), B hangs.
	bDone := make(chan error, 1)
	go func() {
		bDone <- mgr.HandleInbound(context.Background(), newDropInboundMessage("oc_B"))
	}()

	// B must complete within a reasonable timeout despite A
	// still being blocked.
	select {
	case err := <-bDone:
		if err != nil {
			t.Errorf("chat B HandleInbound errored: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("chat B was blocked behind chat A — per-chat lock independence regression")
	}

	// Verify B's tombstone was actually stamped despite A
	// still pending.
	csB := mgr.Get("oc_B")
	if csB == nil {
		t.Fatalf("chat B CS missing")
	}
	if !csB.WatcherHintEmitted() {
		t.Errorf("chat B WatcherHintEmitted = false, want true (hint must have completed despite A blocking)")
	}

	// Release A and confirm it completes cleanly.
	close(releaseA)
	select {
	case err := <-aDone:
		if err != nil {
			t.Errorf("chat A HandleInbound errored: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("chat A didn't complete after releaseA was closed")
	}
}

// blockingEmitter is a test-only Emitter that blocks Send for
// a designated chatID until releaseA is closed, and returns
// the defaultResult immediately for every other chat. Used by
// TestManager_HintPerChatLockIndependence to simulate a slow
// channel-adapter call without standing up a real Channel.
type blockingEmitter struct {
	outbound.Emitter // embedded so we satisfy the interface; nil-default methods never called
	releaseA         chan struct{}
	defaultResult    *messages.OutboundMessage
	mu               sync.Mutex
}

func (b *blockingEmitter) Send(ctx context.Context, msg messages.OutboundMessage) error {
	if msg.ChatID == "oc_A" {
		select {
		case <-b.releaseA:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	b.mu.Lock()
	if b.defaultResult != nil {
		*b.defaultResult = msg
	}
	b.mu.Unlock()
	return nil
}

// Compile-time guard: blockingEmitter must satisfy outbound.Emitter.
var _ outbound.Emitter = (*blockingEmitter)(nil)
