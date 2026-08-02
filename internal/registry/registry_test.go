package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sampleEntry returns a populated Entry with a given session ID. Only
// sessionID needs to vary across tests; the rest is fixed.
func sampleEntry(sessionID string) Entry {
	now := time.Now().UTC().Truncate(time.Second)
	code := 0
	return Entry{
		SessionID: sessionID,
		ChatID:    "chat-" + sessionID,
		Workspace: "/tmp/work-" + sessionID,
		Agent:     "claude",
		Args:      []string{"--foo"},
		PID:       12345,
		PPID:      1,
		StartedAt: now,
		LastRunAt: now,
		Status:    StatusRunning,
		ExitCode:  &code,
	}
}

func TestUpsertGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	e := sampleEntry("sess-1")
	if err := f.Upsert(e); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, ok := f.Get("sess-1")
	if !ok {
		t.Fatalf("Get(sess-1) returned not-ok")
	}
	if got.SessionID != e.SessionID || got.ChatID != e.ChatID || got.PID != e.PID {
		t.Errorf("Get returned %+v, want %+v", got, e)
	}

	// Upsert replaces on duplicate.
	e2 := e
	e2.PID = 99999
	if err := f.Upsert(e2); err != nil {
		t.Fatalf("Upsert (replace): %v", err)
	}
	got, _ = f.Get("sess-1")
	if got.PID != 99999 {
		t.Errorf("after replace PID = %d, want 99999", got.PID)
	}
}

func TestDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	_ = f.Upsert(sampleEntry("sess-1"))
	_ = f.Upsert(sampleEntry("sess-2"))

	if err := f.Delete("sess-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, ok := f.Get("sess-1"); ok {
		t.Errorf("sess-1 should be gone")
	}
	if _, ok := f.Get("sess-2"); !ok {
		t.Errorf("sess-2 should remain")
	}

	// Delete of unknown id is a no-op (no error).
	if err := f.Delete("never-existed"); err != nil {
		t.Errorf("Delete(unknown) error: %v", err)
	}

	// Persisted: re-open and verify sess-1 is still gone.
	f2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f2.Get("sess-1"); ok {
		t.Errorf("sess-1 still on disk after re-open")
	}
}

func TestList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := f.List(); len(got) != 0 {
		t.Errorf("List() on empty = %d, want 0", len(got))
	}

	want := []string{"sess-1", "sess-2", "sess-3"}
	for _, s := range want {
		_ = f.Upsert(sampleEntry(s))
	}

	got := f.List()
	if len(got) != len(want) {
		t.Fatalf("List() = %d, want %d", len(got), len(want))
	}
	seen := make(map[string]bool, len(got))
	for _, e := range got {
		seen[e.SessionID] = true
	}
	for _, s := range want {
		if !seen[s] {
			t.Errorf("List() missing %q", s)
		}
	}
}

func TestGetByChat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	_ = f.Upsert(sampleEntry("sess-1"))
	_ = f.Upsert(sampleEntry("sess-2"))

	got, ok := f.GetByChat("chat-sess-1")
	if !ok {
		t.Fatalf("GetByChat(chat-sess-1) returned not-ok")
	}
	if got.SessionID != "sess-1" {
		t.Errorf("GetByChat returned %q, want sess-1", got.SessionID)
	}

	if _, ok := f.GetByChat("chat-unknown"); ok {
		t.Errorf("GetByChat(unknown) should return not-ok")
	}
}

func TestMarkDetachedAndExited(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	_ = f.Upsert(sampleEntry("sess-1"))

	if err := f.MarkDetached("sess-1"); err != nil {
		t.Fatalf("MarkDetached: %v", err)
	}
	got, _ := f.Get("sess-1")
	if got.Status != StatusDetached {
		t.Errorf("Status = %s, want detached", got.Status)
	}

	if err := f.MarkExited("sess-1", 7); err != nil {
		t.Fatalf("MarkExited: %v", err)
	}
	got, _ = f.Get("sess-1")
	if got.Status != StatusExited {
		t.Errorf("Status = %s, want exited", got.Status)
	}
	if got.ExitCode == nil || *got.ExitCode != 7 {
		t.Errorf("ExitCode = %v, want 7", got.ExitCode)
	}

	// Marking an unknown session returns ErrNotFound.
	if err := f.MarkExited("nope", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkExited(unknown) error = %v, want ErrNotFound", err)
	}
}

// TestConcurrentUpsert races many goroutines writing distinct session
// IDs through the same registry. Run with `go test -race` to validate.
func TestConcurrentUpsert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	const writers = 16
	const perWriter = 25

	var wg sync.WaitGroup
	var failures int64

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				id := sessionID(w, i)
				if err := f.Upsert(sampleEntry(id)); err != nil {
					atomic.AddInt64(&failures, 1)
					t.Errorf("Upsert(%s): %v", id, err)
				}
			}
		}(w)
	}

	// Concurrent readers.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = f.List()
			}
		}()
	}

	wg.Wait()

	if failures > 0 {
		t.Fatalf("%d concurrent writes failed", failures)
	}

	// Verify all entries landed on disk.
	f2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(f2.List()); got != writers*perWriter {
		t.Errorf("after concurrent writes, entry count = %d, want %d", got, writers*perWriter)
	}
}

func TestFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.Upsert(sampleEntry("sess-1")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Errorf("file mode = %o, want 0600", perm)
	}
}

func TestCorruptedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open(corrupted) error: %v", err)
	}

	// Registry should be reset to empty.
	if got := f.List(); len(got) != 0 {
		t.Errorf("after Open(corrupt), List() = %d, want 0", len(got))
	}

	// Backup file should exist and contain the original bytes.
	bak := path + ".bak"
	data, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("backup file missing: %v", err)
	}
	if string(data) != "{not-json" {
		t.Errorf("backup contents = %q, want %q", string(data), "{not-json")
	}

	// The original file should be gone (or empty until we write).
	// Upsert should now write a fresh, valid JSON file at v3.
	if err := f.Upsert(sampleEntry("sess-1")); err != nil {
		t.Fatalf("Upsert after corruption: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip onDisk
	if err := json.Unmarshal(raw, &roundTrip); err != nil {
		t.Errorf("post-upsert JSON invalid: %v", err)
	}
	if roundTrip.Version != Version {
		t.Errorf("post-upsert version = %d, want %d", roundTrip.Version, Version)
	}
	if _, ok := roundTrip.Sessions["sess-1"]; !ok {
		t.Errorf("post-upsert entry missing from on-disk JSON")
	}
}

func TestOpenMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open(missing) error: %v", err)
	}
	if got := f.List(); len(got) != 0 {
		t.Errorf("fresh registry List() = %d, want 0", len(got))
	}
	// Path should be preserved.
	if f.Path() != path {
		t.Errorf("Path() = %q, want %q", f.Path(), path)
	}
}

func sessionID(w, i int) string {
	return "w" + itoa(w) + "_i" + itoa(i)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

// --- v0.2.x → v1.1 migration tests (commit 5) ---

// TestMigrateLegacyToV3 verifies the on-Open migration:
// v0.2.x files (no "version" key) are loaded, every SessionEntry's
// ChatID is extracted into a BindingEntry, and the entries'
// ChatID fields are blanked.
func TestMigrateLegacyToV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")

	// Hand-craft a v0.2.x file: flat map[string]Entry keyed by
	// session_id, with ChatID populated on each entry.
	legacy := map[string]Entry{
		"s_alpha": {
			SessionID: "s_alpha",
			ChatID:    "oc_chat_42",
			Workspace: "/tmp/a",
			Agent:     "claude",
			Status:    StatusRunning,
		},
		"s_beta": {
			SessionID: "s_beta",
			ChatID:    "oc_chat_99",
			Workspace: "/tmp/b",
			Agent:     "codex",
			Status:    StatusDetached,
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy: %v", err)
	}

	// SessionEntry ChatID fields must be blank after migration.
	bind, _ := f.Get("s_alpha")
	if bind.ChatID != "" {
		t.Errorf("after migration: s_alpha.ChatID = %q, want \"\"", bind.ChatID)
	}
	bind, _ = f.Get("s_beta")
	if bind.ChatID != "" {
		t.Errorf("after migration: s_beta.ChatID = %q, want \"\"", bind.ChatID)
	}

	// Binding table has both entries.
	if got, ok := f.GetBinding("oc_chat_42"); !ok || got.SessionID != "s_alpha" || got.Workspace != "/tmp/a" {
		t.Errorf("binding oc_chat_42 = %+v (ok=%v), want sessionID=s_alpha", got, ok)
	}
	if got, ok := f.GetBinding("oc_chat_99"); !ok || got.SessionID != "s_beta" {
		t.Errorf("binding oc_chat_99 = %+v (ok=%v), want sessionID=s_beta", got, ok)
	}

	// ListBindings returns both.
	if got := f.ListBindings(); len(got) != 2 {
		t.Errorf("ListBindings = %d, want 2", len(got))
	}

	// GetByChat still works after migration (used by legacy code).
	if e, ok := f.GetByChat("oc_chat_42"); !ok || e.SessionID != "s_alpha" {
		t.Errorf("GetByChat after migration: ok=%v e=%+v", ok, e)
	}

	// Dirty flag set; next write rewrites in v3.
	if !f.dirty {
		t.Error("expected dirty flag after legacy migration")
	}
}

// TestMigratePersistsOnWrite verifies that after Open migrates a
// legacy file in-memory, the next Upsert writes it out at v3.
func TestMigratePersistsOnWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")

	legacy := map[string]Entry{
		"s_x": {
			SessionID: "s_x",
			ChatID:    "oc_chat_x",
			Workspace: "/x",
			Agent:     "claude",
			Status:    StatusRunning,
		},
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Trigger a write via UpsertBinding.
	if err := f.UpsertBinding(BindingEntry{
		ChatID:    "oc_chat_x",
		ChatType:  "p2p",
		SessionID: "s_x",
		Workspace: "/x",
		Agent:     "claude",
	}); err != nil {
		t.Fatalf("UpsertBinding: %v", err)
	}

	// File is now v3.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v3 onDisk
	if err := json.Unmarshal(raw, &v3); err != nil {
		t.Fatalf("post-write parse: %v", err)
	}
	if v3.Version != Version {
		t.Errorf("post-write version = %d, want %d", v3.Version, Version)
	}
	if len(v3.Sessions) != 1 || len(v3.Bindings) != 1 {
		t.Errorf("post-write shape: sessions=%d bindings=%d, want 1/1", len(v3.Sessions), len(v3.Bindings))
	}
	if v3.Sessions["s_x"].ChatID != "" {
		t.Errorf("post-write: s_x.ChatID = %q, want \"\"", v3.Sessions["s_x"].ChatID)
	}
}

// TestBindingTableRoundTrip covers the binding upsert/get/list/
// delete flow without any session involvement.
func TestBindingTableRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	b1 := BindingEntry{ChatID: "oc_chat_1", ChatType: "p2p", SessionID: "s_1", Workspace: "/a", Agent: "claude"}
	b2 := BindingEntry{ChatID: "oc_chat_2", ChatType: "group", SessionID: "s_2", Workspace: "/b", Agent: "codex"}
	if err := f.UpsertBinding(b1); err != nil {
		t.Fatalf("UpsertBinding b1: %v", err)
	}
	if err := f.UpsertBinding(b2); err != nil {
		t.Fatalf("UpsertBinding b2: %v", err)
	}

	got, ok := f.GetBinding("oc_chat_1")
	if !ok || got.Workspace != "/a" {
		t.Errorf("GetBinding: %+v (ok=%v)", got, ok)
	}

	list := f.ListBindings()
	if len(list) != 2 {
		t.Errorf("ListBindings = %d, want 2", len(list))
	}

	if err := f.DeleteBinding("oc_chat_1"); err != nil {
		t.Fatalf("DeleteBinding: %v", err)
	}
	if _, ok := f.GetBinding("oc_chat_1"); ok {
		t.Error("after Delete: still present")
	}
	if got := f.ListBindings(); len(got) != 1 {
		t.Errorf("after Delete: ListBindings = %d, want 1", len(got))
	}
}

// TestDeleteSessionCascadesToBinding verifies that deleting a
// session entry also removes any binding pointing at it (orphan
// bindings are useless).
func TestDeleteSessionCascadesToBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	f, _ := Open(path)
	if err := f.Upsert(sampleEntry("s_x")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := f.UpsertBinding(BindingEntry{ChatID: "oc_chat_x", SessionID: "s_x"}); err != nil {
		t.Fatalf("UpsertBinding: %v", err)
	}

	if err := f.Delete("s_x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := f.GetBinding("oc_chat_x"); ok {
		t.Error("binding survived session delete (orphan)")
	}
}
