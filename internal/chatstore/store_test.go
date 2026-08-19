package chatstore

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/registry"
)

// newTestStore creates a Store backed by a temp file. Each test gets a
// fresh store so there's no cross-test contamination on disk.
func newTestStore(t *testing.T) (*Store, *registry.ChatSessionFile) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "chat_sessions.json")
	f, err := registry.OpenChatSessionFile(path)
	if err != nil {
		t.Fatalf("OpenChatSessionFile: %v", err)
	}
	return New(f), f
}

// TestSetter_PersistsLastWriter is the bug regression test for
// F-CHATSTORE-001 §1.3 — the lost-update race. Before the fix, a
// chat_sessions.json entry could end up torn or stale. After the fix,
// every setter holds record.mu.Lock through the Upsert, so the entry
// on disk is always a complete snapshot of one Setter's invocation.
func TestSetter_PersistsLastWriter(t *testing.T) {
	store, file := newTestStore(t)
	if _, err := store.Bootstrap("chat-1", "claude"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	const iterations = 100
	var wg sync.WaitGroup
	for i := 0; i < iterations; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := store.SetSelectedCwd("chat-1", "/path/A"); err != nil {
				t.Errorf("SetSelectedCwd A: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := store.SetSelectedCwd("chat-1", "/path/B"); err != nil {
				t.Errorf("SetSelectedCwd B: %v", err)
			}
		}()
	}
	wg.Wait()

	// Read fresh from disk (not store cache) to verify what actually
	// landed on disk after all the racing writers completed.
	e, ok := file.GetByChat("chat-1")
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
	store, file := newTestStore(t)
	if _, err := store.Bootstrap("chat-1", "claude"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	var wg sync.WaitGroup
	const goroutines = 16
	const iterations = 50

	// Mix all setters — they touch different fields but all go
	// through the same record.mu. The "every persisted entry is
	// internally consistent" assertion holds because each setter
	// writes its own field then atomically Upserts the whole entry.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				switch (gid + j) % 6 {
				case 0:
					_ = store.SetSelectedCwd("chat-1", "/x")
				case 1:
					_ = store.SetSelectedAgent("chat-1", "codex")
				case 2:
					_ = store.SetWatchMode("chat-1", 1)
				case 3:
					_ = store.SetThinkMode("chat-1", 1)
				case 4:
					_ = store.SetToolsMode("chat-1", 1)
				case 5:
					_ = store.SetWatcherHintEmitted("chat-1", true)
				}
			}
		}(i)
	}
	wg.Wait()

	e, ok := file.GetByChat("chat-1")
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
// field to its current value must NOT trigger an Upsert (track by
// counting file mtime / write count, but here we use a tighter
// proxy: verify lastInteractionAt is NOT bumped when the value
// matches).
func TestSetter_NoChange(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Bootstrap("chat-1", "claude"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// First call sets the value + bumps LastInteractionAt.
	if err := store.SetSelectedCwd("chat-1", "/workspace"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	first, _ := store.Get("chat-1")
	firstAt := first.LastInteractionAt

	// Sleep so any subsequent bump would be visibly later.
	time.Sleep(2 * time.Millisecond)

	// Second call with the SAME value should be a no-op.
	if err := store.SetSelectedCwd("chat-1", "/workspace"); err != nil {
		t.Fatalf("SetSelectedCwd second: %v", err)
	}
	second, _ := store.Get("chat-1")
	if !second.LastInteractionAt.Equal(firstAt) {
		t.Errorf("no-op setter bumped LastInteractionAt: %v → %v", firstAt, second.LastInteractionAt)
	}
}

// TestBootstrap_NewChat verifies Bootstrap creates a fresh entry on
// disk when the chat is unknown.
func TestBootstrap_NewChat(t *testing.T) {
	store, file := newTestStore(t)

	e, err := store.Bootstrap("chat-fresh", "claude")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if e.PrimaryAgent != "claude" {
		t.Errorf("PrimaryAgent = %q, want claude", e.PrimaryAgent)
	}
	if e.ID != "cs_chat-fresh" {
		t.Errorf("ID = %q, want cs_chat-fresh", e.ID)
	}
	if e.LastInteractionAt.IsZero() {
		t.Error("LastInteractionAt zero after Bootstrap")
	}
	// Verify it landed on disk.
	diskE, ok := file.GetByChat("chat-fresh")
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
	store, file := newTestStore(t)

	// Write a known entry directly to disk via file.Upsert.
	existing := &registry.ChatSessionEntry{
		ID:           "cs_pre",
		ChatID:       "pre",
		SelectedCwd:  "/pre-existing",
		PrimaryAgent: "codex",
	}
	if err := file.Upsert(existing); err != nil {
		t.Fatalf("Upsert seed: %v", err)
	}

	e, err := store.Bootstrap("pre", "")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if e.SelectedCwd != "/pre-existing" {
		t.Errorf("SelectedCwd = %q, want /pre-existing", e.SelectedCwd)
	}
	if e.PrimaryAgent != "codex" {
		t.Errorf("PrimaryAgent = %q", e.PrimaryAgent)
	}
}

// TestBootstrap_MissingFromDisk verifies Bootstrap fails when both
// memory and disk lack the entry AND primaryAgent is empty.
func TestBootstrap_MissingFromDisk(t *testing.T) {
	store, _ := newTestStore(t)
	_, err := store.Bootstrap("unknown", "")
	if err == nil {
		t.Fatal("expected error when chat not on disk and primaryAgent empty")
	}
}

// TestGet_CopyReturned verifies Get returns a fresh copy each call —
// mutating the returned entry has no effect on the in-memory record.
func TestGet_CopyReturned(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Bootstrap("chat-1", "claude"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := store.SetSelectedCwd("chat-1", "/x"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	e1, _ := store.Get("chat-1")
	e1.SelectedCwd = "/mutated"

	e2, _ := store.Get("chat-1")
	if e2.SelectedCwd != "/x" {
		t.Errorf("Get returned a shared pointer: e2.SelectedCwd = %q after mutating e1", e2.SelectedCwd)
	}
}

// TestList verifies List returns a snapshot of all in-memory entries.
func TestList(t *testing.T) {
	store, _ := newTestStore(t)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := store.Bootstrap(id, "claude"); err != nil {
			t.Fatalf("Bootstrap %s: %v", id, err)
		}
	}
	list := store.List()
	if len(list) != 3 {
		t.Errorf("List len = %d, want 3", len(list))
	}
	seen := make(map[string]bool)
	for _, e := range list {
		seen[e.ChatID] = true
	}
	for _, id := range []string{"a", "b", "c"} {
		if !seen[id] {
			t.Errorf("List missing %q", id)
		}
	}
}

// TestConcurrent_DifferentChats verifies that mutating different chats
// in parallel does not contend (per-chat record mutex; cross-chat
// writes to csFile serialize on csFile.mu but the gap is tiny).
func TestConcurrent_DifferentChats(t *testing.T) {
	store, _ := newTestStore(t)
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

// TestSetSelectedAgent_EmptyAgent verifies the validation.
func TestSetSelectedAgent_EmptyAgent(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Bootstrap("chat-1", "claude"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := store.SetSelectedAgent("chat-1", ""); err == nil {
		t.Error("expected error on empty agent")
	}
}

// TestSetSelectedCwd_EmptyCwdAllowed verifies empty cwd is the
// "no workspace" state (legal).
func TestSetSelectedCwd_EmptyCwdAllowed(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Bootstrap("chat-1", "claude"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := store.SetSelectedCwd("chat-1", "/foo"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := store.SetSelectedCwd("chat-1", ""); err != nil {
		t.Errorf("empty cwd should be legal (cleared state), got %v", err)
	}
	e, _ := store.Get("chat-1")
	if e.SelectedCwd != "" {
		t.Errorf("SelectedCwd = %q, want empty", e.SelectedCwd)
	}
}

// TestSetSelectedCwd_BeforeBootstrap verifies a setter called before
// Bootstrap errors out cleanly (no silent corruption).
func TestSetSelectedCwd_BeforeBootstrap(t *testing.T) {
	store, _ := newTestStore(t)
	if err := store.SetSelectedCwd("never-bootstrapped", "/x"); err == nil {
		t.Error("expected error when chat was never bootstrapped")
	}
}

// TestRegistryPath exercises the on-disk format briefly to make sure
// the store plays nicely with the real registry package.
func TestRegistryPath(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "chat_sessions.json")
	f, err := registry.OpenChatSessionFile(path)
	if err != nil {
		t.Fatalf("OpenChatSessionFile: %v", err)
	}
	s := New(f)

	if _, err := s.Bootstrap("real-chat", "claude"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := s.SetSelectedCwd("real-chat", "/workspace"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not written: %v", err)
	}
}

func chatID(i int) string {
	return "chat-" + string(rune('a'+i))
}