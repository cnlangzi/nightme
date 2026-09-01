package chatstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	// Side-effect import: telegram's init() registers the
	// channel with channel.Register("telegram", "tg_", …),
	// which populates the registry the chatstore reads via
	// channel.ChatIDPrefixes() at load time. Without this,
	// New() would refuse every entry as "no channels are
	// registered" because the chatstore test package does not
	// import the runtime.
	//
	// feishu is NOT imported here: feishu transitively depends
	// on chatsession, which depends on chatstore — pulling it
	// in would create an import cycle (chatstore_test → feishu
	// → chatsession → chatstore). The tests use "tg_*" chat
	// ids so telegram is sufficient.
	"github.com/cnlangzi/nightme/internal/channel"
	_ "github.com/cnlangzi/nightme/internal/channel/telegram"
	"github.com/cnlangzi/nightme/internal/registry"
)

// newTestStore creates a Store backed by a temp file. Each test gets a
// fresh store so there's no cross-test contamination on disk.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "chat_sessions.json")
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestSetter_PersistsLastWriter is the bug regression test for
// F-CHATSTORE-001 §1.3 — the lost-update race. Before the fix, a
// chat_sessions.json entry could end up torn or stale. After the fix,
// every setter holds the Store mutex through save, so the entry on
// disk is always a complete snapshot of one Setter's invocation.
func TestSetter_PersistsLastWriter(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Bootstrap("tg_chat-1", "claude"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	const iterations = 100
	var wg sync.WaitGroup
	for i := 0; i < iterations; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := store.SetSelectedCwd("tg_chat-1", "/path/A"); err != nil {
				t.Errorf("SetSelectedCwd A: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := store.SetSelectedCwd("tg_chat-1", "/path/B"); err != nil {
				t.Errorf("SetSelectedCwd B: %v", err)
			}
		}()
	}
	wg.Wait()

	// Re-load from a fresh Store on the same path to verify what
	// actually landed on disk after all the racing writers completed.
	dir := t.TempDir()
	freshPath := filepath.Join(dir, "fresh.json")
	if err := copyFile(store.Path(), freshPath); err != nil {
		t.Fatalf("copy: %v", err)
	}
	fresh, err := New(freshPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	e, ok := fresh.Get("tg_chat-1")
	if !ok {
		t.Fatal("entry missing from disk after writes")
	}
	if e.SelectedCwd != "/path/A" && e.SelectedCwd != "/path/B" {
		t.Fatalf("disk entry has torn cwd %q (must be exactly one of A/B)", e.SelectedCwd)
	}
}

// TestSetter_NoLostUpdate verifies that under concurrent multi-setter
// load the on-disk entry is always one of the values the test
// produced — no torn snapshots, no stale snapshots.
func TestSetter_NoLostUpdate(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Bootstrap("tg_chat-1", "claude"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	var wg sync.WaitGroup
	const goroutines = 16
	const iterations = 50

	// Mix all setters — they touch different fields but all go
	// through the same Store mutex. The "every persisted entry is
	// internally consistent" assertion holds because each setter
	// writes its own field then atomically saves the whole entry.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				switch (gid + j) % 6 {
				case 0:
					_ = store.SetSelectedCwd("tg_chat-1", "/x")
				case 1:
					_ = store.SetSelectedAgent("tg_chat-1", "codex")
				case 2:
					_ = store.SetWatchMode("tg_chat-1", 1)
				case 3:
					_ = store.SetThinkMode("tg_chat-1", 1)
				case 4:
					_ = store.SetToolsMode("tg_chat-1", 1)
				case 5:
					_ = store.SetWatcherHintEmitted("tg_chat-1", true)
				}
			}
		}(i)
	}
	wg.Wait()

	e, ok := store.Get("tg_chat-1")
	if !ok {
		t.Fatal("entry missing")
	}
	// LastInteractionAt was bumped by every setter; must be > 0.
	if e.LastInteractionAt.IsZero() {
		t.Fatal("LastInteractionAt not bumped")
	}
	// All values must be in the valid range (never torn).
	if e.WatchMode != 0 && e.WatchMode != 1 {
		t.Errorf("WatchMode = %d (must be 0 or 1)", e.WatchMode)
	}
	if e.ThinkMode != 0 && e.ThinkMode != 1 {
		t.Errorf("ThinkMode = %d (must be 0 or 1)", e.ThinkMode)
	}
	if e.ToolsMode != 0 && e.ToolsMode != 1 {
		t.Errorf("ToolsMode = %d (must be 0 or 1)", e.ToolsMode)
	}
	if e.SelectedCwd != "/x" && e.SelectedCwd != "" {
		t.Errorf("SelectedCwd = %q (must be empty or /x)", e.SelectedCwd)
	}
	if e.SelectedAgent != "codex" && e.SelectedAgent != "claude" {
		t.Errorf("SelectedAgent = %q", e.SelectedAgent)
	}
}

// TestSetter_NoChange verifies the no-op short-circuit. Setting a
// field to its current value must NOT trigger a save (so the
// LastInteractionAt is not bumped).
func TestSetter_NoChange(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Bootstrap("tg_chat-1", "claude"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// First call sets the value + bumps LastInteractionAt.
	if err := store.SetSelectedCwd("tg_chat-1", "/workspace"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	first, _ := store.Get("tg_chat-1")
	firstAt := first.LastInteractionAt

	// Sleep so any subsequent bump would be visibly later.
	time.Sleep(2 * time.Millisecond)

	// Second call with the SAME value should be a no-op.
	if err := store.SetSelectedCwd("tg_chat-1", "/workspace"); err != nil {
		t.Fatalf("SetSelectedCwd second: %v", err)
	}
	second, _ := store.Get("tg_chat-1")
	if !second.LastInteractionAt.Equal(firstAt) {
		t.Errorf("no-op setter bumped LastInteractionAt: %v → %v", firstAt, second.LastInteractionAt)
	}
}

// TestBootstrap_NewChat verifies Bootstrap creates a fresh entry on
// disk when the chat is unknown.
func TestBootstrap_NewChat(t *testing.T) {
	store := newTestStore(t)

	e, err := store.Bootstrap("tg_chat-fresh", "claude")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if e.PrimaryAgent != "claude" {
		t.Errorf("PrimaryAgent = %q, want claude", e.PrimaryAgent)
	}
	if e.ID != "cs_tg_chat-fresh" {
		t.Errorf("ID = %q, want cs_chat-fresh", e.ID)
	}
	if e.LastInteractionAt.IsZero() {
		t.Error("LastInteractionAt zero after Bootstrap")
	}
	// Verify it landed on disk by reopening a fresh store from the same path.
	fresh, err := New(store.Path())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	diskE, ok := fresh.Get("tg_chat-fresh")
	if !ok {
		t.Fatal("entry missing on disk after Bootstrap")
	}
	if diskE.PrimaryAgent != "claude" {
		t.Errorf("disk.PrimaryAgent = %q", diskE.PrimaryAgent)
	}
}

// TestBootstrap_ExistingChat verifies Bootstrap loads from disk when
// the entry is not in memory but exists on disk.
func TestBootstrap_ExistingChat(t *testing.T) {
	store := newTestStore(t)

	// Simulate a pre-existing on-disk entry by bootstrapping
	// first, then re-opening a fresh store.
	if _, err := store.Bootstrap("tg_pre", "codex"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store.SetSelectedCwd("tg_pre", "/pre-existing")

	fresh, err := New(store.Path())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := fresh.SetSelectedCwd("tg_pre", "/reopen-overwrite"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	e, err := fresh.Bootstrap("tg_pre", "")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	// Bootstrap should NOT have reset /reopen-overwrite (it would
	// only set fields on a fresh entry). Since the entry already
	// existed in memory, the existing path is taken.
	if e.SelectedCwd != "/reopen-overwrite" {
		t.Errorf("SelectedCwd = %q, want /reopen-overwrite", e.SelectedCwd)
	}
	if e.PrimaryAgent != "codex" {
		t.Errorf("PrimaryAgent = %q", e.PrimaryAgent)
	}
}

// TestBootstrap_MissingFromDisk verifies Bootstrap fails when both
// memory and disk lack the entry AND primaryAgent is empty.
func TestBootstrap_MissingFromDisk(t *testing.T) {
	store := newTestStore(t)
	_, err := store.Bootstrap("tg_unknown", "")
	if err == nil {
		t.Fatal("expected error when chat not on disk and primaryAgent empty")
	}
}

// TestGet_CopyReturned verifies Get returns a fresh copy each call —
// mutating the returned entry has no effect on the in-memory record.
func TestGet_CopyReturned(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Bootstrap("tg_chat-1", "claude"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := store.SetSelectedCwd("tg_chat-1", "/x"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	e1, _ := store.Get("tg_chat-1")
	e1.SelectedCwd = "/mutated"

	e2, _ := store.Get("tg_chat-1")
	if e2.SelectedCwd != "/x" {
		t.Errorf("Get returned a shared pointer: e2.SelectedCwd = %q after mutating e1", e2.SelectedCwd)
	}
}

// TestGet_DeepCopyPointerField verifies the deep-copy contract for
// the only pointer field on ChatSessionEntry (SelectedAgentSessionID).
// A shallow copy would let the caller overwrite the in-memory record
// via the shared pointer; this guards against regressions.
func TestGet_DeepCopyPointerField(t *testing.T) {
	store := newTestStore(t)
	id := "as_target"
	if err := store.Save(&registry.ChatSessionEntry{
		ID:                     "cs_tg_chat-1",
		ChatID:                 "tg_chat-1",
		PrimaryAgent:           "claude",
		SelectedAgentSessionID: &id,
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// First read captures a pointer to the field-target.
	before, _ := store.Get("tg_chat-1")
	if before.SelectedAgentSessionID == nil || *before.SelectedAgentSessionID != "as_target" {
		t.Fatalf("first Get: SelectedAgentSessionID = %v, want as_target", before.SelectedAgentSessionID)
	}

	// Mutate the field on the returned copy. If Get shallow-copied
	// the *string, this would leak into the in-memory record.
	*before.SelectedAgentSessionID = "as_overwritten"

	after, _ := store.Get("tg_chat-1")
	if after.SelectedAgentSessionID == nil || *after.SelectedAgentSessionID != "as_target" {
		t.Fatalf("second Get: SelectedAgentSessionID = %v, want as_target (deep-copy broken)", after.SelectedAgentSessionID)
	}
}

// TestList verifies the snapshot semantics.
func TestList(t *testing.T) {
	store := newTestStore(t)
	a, _ := store.Bootstrap("a", "claude")
	b, _ := store.Bootstrap("b", "claude")
	c, _ := store.Bootstrap("c", "claude")

	list := store.List()
	if len(list) != 3 {
		t.Fatalf("List len = %d, want 3", len(list))
	}
	seen := map[string]bool{}
	for _, e := range list {
		seen[e.ChatID] = true
	}
	for _, id := range []string{"a", "b", "c"} {
		if !seen[id] {
			t.Errorf("List missing %q", id)
		}
	}
	// Verify the returned entries are not the same pointers (snapshot).
	if list[0] == a || list[0] == b || list[0] == c {
		t.Errorf("List returned same pointer as Bootstrap result (should be a fresh copy)")
	}
}

// TestSetSelectedAgent_EmptyAgent verifies the validation.
func TestSetSelectedAgent_EmptyAgent(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Bootstrap("tg_chat-1", "claude"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := store.SetSelectedAgent("tg_chat-1", ""); err == nil {
		t.Errorf("expected error for empty agent")
	}
}

// TestSetSelectedCwd_EmptyCwdAllowed verifies that empty cwd (the
// "no workspace" state) is legal.
func TestSetSelectedCwd_EmptyCwdAllowed(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Bootstrap("tg_chat-1", "claude"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := store.SetSelectedCwd("tg_chat-1", "/foo"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := store.SetSelectedCwd("tg_chat-1", ""); err != nil {
		t.Errorf("empty cwd should be legal (cleared state), got %v", err)
	}
	e, _ := store.Get("tg_chat-1")
	if e.SelectedCwd != "" {
		t.Errorf("SelectedCwd = %q, want empty", e.SelectedCwd)
	}
}

// TestSetSelectedCwd_BeforeBootstrap verifies a setter errors when the
// chat was never bootstrapped.
func TestSetSelectedCwd_BeforeBootstrap(t *testing.T) {
	store := newTestStore(t)
	if err := store.SetSelectedCwd("never-bootstrapped", "/x"); err == nil {
		t.Errorf("expected error when chat was never bootstrapped")
	}
}

// TestConcurrent_DifferentChats verifies that mutating different chats
// in parallel does not contend (single Store mutex; per-chat
// mutations serialize on it, but the gap is tiny).
func TestConcurrent_DifferentChats(t *testing.T) {
	store := newTestStore(t)
	const chats = 8
	for i := 0; i < chats; i++ {
		if _, err := store.Bootstrap(chatID(i), "claude"); err != nil {
			t.Fatalf("Bootstrap %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	var done int64
	for i := 0; i < chats; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = store.SetSelectedCwd(chatID(idx), "/y")
				atomic.AddInt64(&done, 1)
			}
		}(i)
	}
	wg.Wait()

	if done != chats*50 {
		t.Errorf("done = %d, want %d", done, chats*50)
	}

	// All chats should have the final /y value.
	for i := 0; i < chats; i++ {
		e, _ := store.Get(chatID(i))
		if e.SelectedCwd != "/y" {
			t.Errorf("chat %d SelectedCwd = %q, want /y", i, e.SelectedCwd)
		}
	}
}

func chatID(i int) string {
	return "chat-" + string(rune('a'+i))
}

// copyFile reads src and writes its contents to dst. Used in tests
// that need to verify on-disk state without sharing the same
// in-memory entries as the store under test.
func copyFile(src, dst string) error {
	data, err := osReadFile(src)
	if err != nil {
		return err
	}
	return osWriteFile(dst, data)
}

func osReadFile(path string) ([]byte, error)     { return os.ReadFile(path) }
func osWriteFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }

// TestNew_MigratesLegacyIDKeyedFormat is the regression test for the
// cross-version format drift found during review of F-CHATSTORE-001.
// Legacy chat_sessions.json files (written by the old
// registry.ChatSessionFile) keyed the chatSessions map by entry.ID
// ("cs_<chatID>"). The new Store keys by entry.ChatID. Without
// migration, the old entry is invisible to chatID-indexed lookups,
// Bootstrap re-creates a fresh empty entry, and the stale "cs_*"
// key orphans alongside the new key on every save.
//
// Fix (Plan B): New() re-keys by e.ChatID on load; the first save()
// rewrites the whole file with normalized keys. No version bump —
// reading is bidirectionally compatible.
// TestNew_RejectsLegacyIDKeyedFormat verifies that the loader
// refuses to silently rewrite cs_<chatID>-keyed legacy files.
// Operators must migrate by hand (the file format is simple
// enough — re-key by entry.ChatID, then prefix with the
// appropriate channel tag) instead of having the daemon silently
// mutate the on-disk format.
func TestNew_RejectsLegacyIDKeyedFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat_sessions.json")
	// Legacy shape: map key == entry.ID ("cs_42"), ChatID == "42".
	legacy := `{"version":1,"chatSessions":{"cs_42":{"id":"cs_42","chatId":"42","primaryAgent":"codex","selectedCwd":"/old"}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	if _, err := New(path); err == nil {
		t.Fatal("expected New to reject legacy ID-keyed format, got no error")
	}
}

// TestNew_SkipsUnknownPrefix is the lenient-loader regression
// test for the fix-chatsession-fails refactor. Before the fix,
// an unknown chat-id prefix caused the daemon to refuse to start
// (a single stale entry from an old / hand-edited file would
// hide every other chat). Now New() logs a warning identifying
// the offending entry and continues — only a key/ChatID mismatch
// (a structural corruption) is still a hard error.
//
// This file mixes one bare-digit key ("42", pre-stable-chatID
// legacy format) with two properly prefixed ones; the loader
// must skip the bare digit and keep the others.
func TestNew_SkipsUnknownPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat_sessions.json")
	// Mix: bare digit (no known prefix), plus two valid tg_ entries.
	mixed := `{"version":1,"chatSessions":{"42":{"id":"cs_42","chatId":"42","primaryAgent":"codex"},"tg_keep_a":{"id":"cs_keep_a","chatId":"tg_keep_a","primaryAgent":"claude"},"tg_keep_b":{"id":"cs_keep_b","chatId":"tg_keep_b","primaryAgent":"codex"}}}`
	if err := os.WriteFile(path, []byte(mixed), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := New(path)
	if err != nil {
		t.Fatalf("New should succeed despite the unknown-prefix entry; got %v", err)
	}

	if _, ok := s.Get("42"); ok {
		t.Error("bare-digit entry was loaded instead of being skipped")
	}
	if _, ok := s.Get("tg_keep_a"); !ok {
		t.Error("tg_keep_a should have been loaded alongside the skipped entry")
	}
	if _, ok := s.Get("tg_keep_b"); !ok {
		t.Error("tg_keep_b should have been loaded alongside the skipped entry")
	}
}

// TestNew_TelegramFormat_LoadsCleanly verifies the happy path:
// a properly-prefixed chat_sessions.json (the format the
// stable-chatID refactor mandates) loads without any rewriting.
// Both "tg_<chatid>" DM and "tg_<chatid>:<thread_id>" topic
// keys round-trip through Bootstrap → setter → reload.
func TestNew_TelegramFormat_LoadsCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat_sessions.json")
	// Initial state: two Telegram chats, one DM and one topic.
	initial := `{"version":1,"chatSessions":{"tg_8684538097":{"id":"cs_8684538097","chatId":"tg_8684538097","primaryAgent":"claude","selectedCwd":"/Users/..."},"tg_-10012345:42":{"id":"cs_-10012345:42","chatId":"tg_-10012345:42","primaryAgent":"claude","selectedCwd":"/work"}}}`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, id := range []string{"tg_8684538097", "tg_-10012345:42"} {
		e, ok := s.Get(id)
		if !ok {
			t.Errorf("entry %q not loaded", id)
			continue
		}
		if e.ChatID != id {
			t.Errorf("entry %q has ChatID %q", id, e.ChatID)
		}
	}
	if len(s.entries) != 2 {
		t.Errorf("entries len = %d, want 2", len(s.entries))
	}
}

// TestNew_AcceptsRegistryAddedPrefix is the regression guard for
// the fix-chatsession-fails refactor. Before the fix,
// chat_sessions.json validation hard-coded two prefixes ("tg_"
// and "oc_"), so any on-disk entry from a third channel
// (Slack "sl_", bot "bt_", …) caused the daemon to refuse to
// start with
//
//	chat_sessions: … entry "sl_…" is not channel-prefixed
//
// The fix routes validation through channel.ChatIDPrefixes(),
// which returns whatever prefixes each channel adapter
// registered via channel.Register. This test registers a
// prefix into the registry at runtime and verifies that the
// chatstore recognises the matching chat_sessions.json entry
// without any edits to chatstore.
//
// (Slack's own adapter is part of the user's build but is not
// checked into this worktree's tree, so the test registers it
// synthetically. The mechanism under test is identical to the
// real Slack init() flow.)
func TestNew_AcceptsRegistryAddedPrefix(t *testing.T) {
	// Snapshot the registry state so we can restore it after
	// the test. channel.Reset() is too aggressive — it would
	// also wipe the prefixes the other tests in this file rely
	// on.
	original := snapshotPrefixesForTest()
	channel.Reset()
	// Restore the "tg_" prefix that channel/telegram's init()
	// registered — chatstore_test imports it as a side effect.
	channel.Register("telegram", "tg_", nil)
	// Now add the Slack prefix that drives this test.
	channel.Register("slack-test", "sl_", nil)
	t.Cleanup(func() {
		channel.Reset()
		for name, prefix := range original {
			channel.Register(name, prefix, nil)
		}
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "chat_sessions.json")
	// A Slack chat ID of the same shape as the user's failing
	// daemon: "sl_<workspace>:<channel>:<ts>".
	slackEntry := `{"version":1,"chatSessions":{"sl_T0BTP773FAS:D0BTPL0K3UN:1788174480.038419":{"id":"cs_T0BTP773FAS:D0BTPL0K3UN:1788174480.038419","chatId":"sl_T0BTP773FAS:D0BTPL0K3UN:1788174480.038419","primaryAgent":"claude","selectedCwd":"/home/yaitoo/work/repo"}}}`
	if err := os.WriteFile(path, []byte(slackEntry), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, ok := s.Get("sl_T0BTP773FAS:D0BTPL0K3UN:1788174480.038419")
	if !ok {
		t.Fatal("sl_ entry not loaded — registry prefix not consulted?")
	}
	if got.PrimaryAgent != "claude" {
		t.Errorf("PrimaryAgent = %q, want claude", got.PrimaryAgent)
	}
	if got.SelectedCwd != "/home/yaitoo/work/repo" {
		t.Errorf("SelectedCwd = %q", got.SelectedCwd)
	}
}

// snapshotPrefixesForTest returns the currently-registered
// (name, prefix) pairs so a test can restore them after
// channel.Reset(). Builders are not preserved — chatstore
// tests never call BuildAll, so a nil Builder is harmless for
// the duration of the test.
func snapshotPrefixesForTest() map[string]string {
	// channel.ChatIDPrefixes() returns only the prefix set,
	// not the names. We need names too, so walk Available()
	// and look up each name's prefix via ChatIDPrefix.
	out := map[string]string{}
	for _, name := range channel.Available() {
		out[name] = channel.ChatIDPrefix(name)
	}
	return out
}

// TestNew_DroppedEntriesSurviveSave is the data-loss regression
// guard. Before this fix, an unknown-prefix entry was logged
// and silently skipped from the in-memory map; the next
// legitimate save() (triggered by any setter on a known-prefix
// entry) would rewrite chat_sessions.json without the skipped
// entries, permanently destroying the on-disk data. This test
// loads a file with a stray "sl_*" entry, mutates a "tg_*"
// entry, and verifies the "sl_*" entry survives the round-trip
// with all its fields intact.
func TestNew_DroppedEntriesSurviveSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat_sessions.json")
	const droppedJSON = `{"id":"cs_sl_42","chatId":"sl_42","primaryAgent":"claude","selectedCwd":"/legacy","watchMode":1,"thinkMode":0,"toolsMode":1,"watcherHintEmitted":true}`
	initial := `{"version":1,"chatSessions":{"sl_42":` + droppedJSON + `,"tg_keep":{"id":"cs_tg_keep","chatId":"tg_keep","primaryAgent":"claude","selectedCwd":"/old"}}}`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Verify the dropped entry is reported.
	keys, count := s.Dropped()
	if count != 1 {
		t.Fatalf("Dropped count = %d, want 1 (keys: %v)", count, keys)
	}
	if len(keys) != 1 || keys[0] != "sl_42" {
		t.Errorf("Dropped keys = %v, want [sl_42]", keys)
	}

	// Mutate the known entry through a setter. This must trigger
	// a save() that rewrites the file — and that rewrite MUST
	// preserve the dropped "sl_42" entry's fields.
	if err := s.SetSelectedCwd("tg_keep", "/new"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	// Parse the rewritten file and verify sl_42 still has its
	// fields. (We compare field-by-field rather than byte-for-byte
	// because MarshalIndent re-indents the raw bytes for
	// readability — the data is preserved, the formatting is
	// just refreshed.)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	var reloaded struct {
		ChatSessions map[string]json.RawMessage `json:"chatSessions"`
	}
	if err := json.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("unmarshal rewritten file: %v", err)
	}
	raw, ok := reloaded.ChatSessions["sl_42"]
	if !ok {
		t.Fatalf("dropped entry overwritten by save() — data loss!\nfile contents:\n%s", string(data))
	}
	var entry registry.ChatSessionEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("unmarshal dropped entry: %v", err)
	}
	if entry.SelectedCwd != "/legacy" || entry.PrimaryAgent != "claude" {
		t.Errorf("dropped entry fields mutated: %+v", entry)
	}

	// Reload from disk to confirm the dropped entry is still
	// preserved across a fresh loader invocation.
	s2, err := New(path)
	if err != nil {
		t.Fatalf("reload New: %v", err)
	}
	if _, count := s2.Dropped(); count != 1 {
		t.Errorf("after reload: Dropped count = %d, want 1", count)
	}
	if _, ok := s2.Get("tg_keep"); !ok {
		t.Error("after reload: tg_keep entry missing")
	}
}
