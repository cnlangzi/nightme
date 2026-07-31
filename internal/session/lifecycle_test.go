package session

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// TestCreateOrUpdate_NewSession covers the happy path: no prior
// session exists for chatID, so a detached session is created and
// persisted.
func TestCreateOrUpdate_NewSession(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{name: "claude", mode: agent.ModePTY, pid: 100})
	mgr := NewMemoryManager(reg, nil, nil)

	sess, err := mgr.CreateOrUpdate("chat-cu", "/tmp/work", "claude", nil)
	if err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	if sess.Status() != StatusDetached {
		t.Errorf("new session status = %s, want detached", sess.Status())
	}
	if sess.Workspace != "/tmp/work" || sess.Agent != "claude" {
		t.Errorf("session fields = %+v", sess.Snapshot())
	}
	if sess.PID != 0 {
		t.Errorf("PID = %d, want 0 (no agent yet)", sess.PID)
	}

	// GetByChat must now resolve it.
	got, err := mgr.GetByChat("chat-cu")
	if err != nil {
		t.Fatalf("GetByChat: %v", err)
	}
	if got.ID != sess.ID {
		t.Errorf("GetByChat ID = %s, want %s", got.ID, sess.ID)
	}
}

// TestCreateOrUpdate_RebindsExitedWorkspace verifies that calling
// /cwd a second time after a session has exited rebinds the
// workspace in place and keeps the original session ID.
func TestCreateOrUpdate_RebindsExitedWorkspace(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{name: "claude", mode: agent.ModePTY})
	mgr := NewMemoryManager(reg, nil, nil)

	first, err := mgr.CreateOrUpdate("chat-cu", "/tmp/old", "claude", nil)
	if err != nil {
		t.Fatalf("first CreateOrUpdate: %v", err)
	}
	// Mark the session exited manually (no agent, etc).
	first.setLifecycle(StatusExited, nil, 0, nil)

	second, err := mgr.CreateOrUpdate("chat-cu", "/tmp/new", "claude", nil)
	if err != nil {
		t.Fatalf("second CreateOrUpdate: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("rebind changed ID: %s -> %s", first.ID, second.ID)
	}
	if second.Workspace != "/tmp/new" {
		t.Errorf("workspace = %q, want /tmp/new", second.Workspace)
	}
}

// TestCreateOrUpdate_RejectsActiveSession covers the "CLI running,
// /kill first" branch from the spec.
func TestCreateOrUpdate_RejectsActiveSession(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{name: "claude", mode: agent.ModePTY})
	mgr := NewMemoryManager(reg, nil, nil)

	sess, err := mgr.CreateOrUpdate("chat-cu", "/tmp/work", "claude", nil)
	if err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	// Mark the session running with a fake PID.
	sess.setLifecycle(StatusRunning, &fakeAgentSession{}, 12345, nil)

	_, err = mgr.CreateOrUpdate("chat-cu", "/tmp/other", "claude", nil)
	if !errors.Is(err, ErrChatAlreadyBound) {
		t.Errorf("second CreateOrUpdate error = %v, want ErrChatAlreadyBound", err)
	}
}

// TestCreateOrUpdate_ValidationErrors covers the cheap pre-checks.
func TestCreateOrUpdate_ValidationErrors(t *testing.T) {
	mgr := NewMemoryManager(agent.New(), nil, nil)
	cases := []struct {
		name      string
		chatID    string
		workspace string
		agent     string
	}{
		{"no chat", "", "/tmp", "claude"},
		{"no workspace", "c", "", "claude"},
		{"no agent", "c", "/tmp", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := mgr.CreateOrUpdate(tc.chatID, tc.workspace, tc.agent, nil); err == nil {
				t.Errorf("CreateOrUpdate(%s) = nil error, want validation", tc.name)
			}
		})
	}
}

// TestCreateOrUpdate_PersistsToRegistry verifies the new session
// is written to the registry (so daemon restart sees it).
func TestCreateOrUpdate_PersistsToRegistry(t *testing.T) {
	dir := t.TempDir()
	file, err := registry.Open(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	reg := agent.New()
	reg.Register(&fakeAgent{name: "claude", mode: agent.ModePTY})
	mgr := NewMemoryManager(reg, file, nil)

	sess, err := mgr.CreateOrUpdate("chat-cu", "/tmp/work", "claude", nil)
	if err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}

	entry, ok := file.Get(sess.ID)
	if !ok {
		t.Fatalf("registry missing entry for %s", sess.ID)
	}
	if entry.Workspace != "/tmp/work" {
		t.Errorf("entry workspace = %q, want /tmp/work", entry.Workspace)
	}
	if entry.Status != registry.StatusDetached {
		t.Errorf("entry status = %s, want detached", entry.Status)
	}
}

// TestRun_NoWorkspace returns ErrSessionNotFound when the chat has
// never been /cwd'd.
func TestRun_NoWorkspace(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{name: "claude", mode: agent.ModePTY})
	mgr := NewMemoryManager(reg, nil, nil)

	_, err := mgr.Run(context.Background(), "chat-no", "claude", nil)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Run error = %v, want ErrSessionNotFound", err)
	}
}

// TestRun_RespawnsAfterExit verifies the bounce-back path: workspace
// set, agent started, agent exited, /run brings a fresh agent up.
func TestRun_RespawnsAfterExit(t *testing.T) {
	reg := agent.New()
	// Use a PID so we can verify the new spawn updated it.
	reg.Register(&fakeAgent{name: "claude", mode: agent.ModePTY, pid: 9999})
	mgr := NewMemoryManager(reg, nil, nil)

	if _, err := mgr.CreateOrUpdate("chat-r", "/tmp/work", "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}

	// First /run starts a fresh agent.
	sess, err := mgr.Run(context.Background(), "chat-r", "claude", nil)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if sess.Snapshot().Status != StatusRunning {
		t.Errorf("after Run status = %s, want running", sess.Snapshot().Status)
	}
	if sess.Snapshot().PID != 9999 {
		t.Errorf("after Run PID = %d, want 9999", sess.Snapshot().PID)
	}
	firstID := sess.ID

	// Kill marks the session exited.
	if err := mgr.KillByChat("chat-r"); err != nil {
		t.Fatalf("KillByChat: %v", err)
	}
	if sess.Status() != StatusExited {
		t.Errorf("after Kill status = %s, want exited", sess.Status())
	}

	// Second /run respawns on the same session ID (workspace preserved).
	sess2, err := mgr.Run(context.Background(), "chat-r", "claude", nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if sess2.ID != firstID {
		t.Errorf("respawn ID = %s, want %s (history preserved)", sess2.ID, firstID)
	}
	if sess2.Snapshot().Status != StatusRunning {
		t.Errorf("after respawn status = %s, want running", sess2.Snapshot().Status)
	}
}

// TestRun_NoopWhenRunning exercises the F-20 rule: an already-running
// CLI is left alone, just reused.
func TestRun_NoopWhenRunning(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{name: "claude", mode: agent.ModePTY, pid: 7777})
	mgr := NewMemoryManager(reg, nil, nil)

	if _, err := mgr.CreateOrUpdate("chat-r", "/tmp/work", "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	first, err := mgr.Run(context.Background(), "chat-r", "claude", nil)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Second /run while still running must be a no-op.
	second, err := mgr.Run(context.Background(), "chat-r", "claude", nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("noop Run changed ID: %s -> %s", first.ID, second.ID)
	}
	if second.Snapshot().PID != 7777 {
		t.Errorf("noop Run PID = %d, want 7777", second.Snapshot().PID)
	}
}

// TestRun_UnknownAgent returns the agent registry sentinel.
func TestRun_UnknownAgent(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{name: "claude", mode: agent.ModePTY})
	mgr := NewMemoryManager(reg, nil, nil)

	if _, err := mgr.CreateOrUpdate("chat-r", "/tmp/work", "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	_, err := mgr.Run(context.Background(), "chat-r", "mystery", nil)
	if !errors.Is(err, agent.ErrUnknownAgent) {
		t.Errorf("Run unknown agent error = %v, want ErrUnknownAgent", err)
	}
}

// TestRun_ValidationErrors covers the cheap pre-checks.
func TestRun_ValidationErrors(t *testing.T) {
	mgr := NewMemoryManager(agent.New(), nil, nil)
	if _, err := mgr.Run(context.Background(), "", "claude", nil); err == nil {
		t.Error("Run(empty chat) = nil error, want validation")
	}
	if _, err := mgr.Run(context.Background(), "c", "", nil); err == nil {
		t.Error("Run(empty agent) = nil error, want validation")
	}
}

// TestRun_RespawnPersistsWorkspace ensures a daemon restart sees the
// surviving session.
func TestRun_RespawnPersistsWorkspace(t *testing.T) {
	dir := t.TempDir()
	file, err := registry.Open(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	reg := agent.New()
	reg.Register(&fakeAgent{name: "claude", mode: agent.ModePTY, pid: 1234})
	mgr := NewMemoryManager(reg, file, nil)

	if _, err := mgr.CreateOrUpdate("chat-r", "/tmp/work", "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	if _, err := mgr.Run(context.Background(), "chat-r", "claude", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Re-open the registry and verify the entry is running.
	file2, err := registry.Open(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatalf("registry.Open(2): %v", err)
	}
	entries := file2.List()
	var found *registry.Entry
	for i := range entries {
		if entries[i].ChatID == "chat-r" {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("registry lost the chat-r entry")
	}
	if found.Status != registry.StatusRunning {
		t.Errorf("entry status = %s, want running", found.Status)
	}
	if found.Workspace != "/tmp/work" {
		t.Errorf("entry workspace = %q, want /tmp/work", found.Workspace)
	}
}

// TestKillByChat_NoSession confirms ErrSessionNotFound.
func TestKillByChat_NoSession(t *testing.T) {
	mgr := NewMemoryManager(agent.New(), nil, nil)
	if err := mgr.KillByChat("none"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("KillByChat unknown error = %v, want ErrSessionNotFound", err)
	}
}

// TestKillByChat_StopsAgent closes the live agent and marks the
// session exited.
func TestKillByChat_StopsAgent(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{name: "claude", mode: agent.ModePTY, pid: 4242})
	mgr := NewMemoryManager(reg, nil, nil)

	if _, err := mgr.CreateOrUpdate("chat-k", "/tmp/work", "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	sess, err := mgr.Run(context.Background(), "chat-k", "claude", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sess.Snapshot().PID == 0 {
		t.Fatalf("PID is zero after Run")
	}

	if err := mgr.KillByChat("chat-k"); err != nil {
		t.Fatalf("KillByChat: %v", err)
	}
	if sess.Status() != StatusExited {
		t.Errorf("after Kill status = %s, want exited", sess.Status())
	}

	// Idempotent.
	if err := mgr.KillByChat("chat-k"); err != nil {
		t.Errorf("second KillByChat = %v, want nil", err)
	}
}

// TestRun_PreservesExtraArgs confirms the per-/run argv is forwarded.
func TestRun_PreservesExtraArgs(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{name: "claude", mode: agent.ModePTY, pid: 1})
	mgr := NewMemoryManager(reg, nil, nil)

	if _, err := mgr.CreateOrUpdate("chat-r", "/tmp/work", "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	sess, err := mgr.Run(context.Background(), "chat-r", "claude", []string{"--opus", "--fast"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sess.Snapshot().Args; len(got) != 2 || got[0] != "--opus" || got[1] != "--fast" {
		t.Errorf("args = %v, want [--opus --fast]", got)
	}
}

// TestRun_ReadPumpDeliversEvents ensures the readPump goroutine
// started by Run honors the manager's callback.
func TestRun_ReadPumpDeliversEvents(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{
		name: "claude",
		mode: agent.ModePTY,
		pid:  1,
		events: []agent.AgentEvent{
			{Kind: agent.EventText, Text: "ahoy"},
			{Kind: agent.EventDone, Done: &agent.DoneEvent{ExitCode: 0}},
		},
	})
	var seen []agent.AgentEvent
	var mu sync.Mutex
	mgr := NewMemoryManager(reg, nil, func(s *Session, ev agent.AgentEvent) {
		mu.Lock()
		seen = append(seen, ev)
		mu.Unlock()
	})

	if _, err := mgr.CreateOrUpdate("chat-r", "/tmp/work", "claude", nil); err != nil {
		t.Fatalf("CreateOrUpdate: %v", err)
	}
	if _, err := mgr.Run(context.Background(), "chat-r", "claude", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Wait for the readPump to drain both events.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("readPump delivered %d events, want >= 2", len(seen))
	}
	if seen[0].Kind != agent.EventText || seen[0].Text != "ahoy" {
		t.Errorf("first event = %+v", seen[0])
	}
	if seen[1].Kind != agent.EventDone {
		t.Errorf("second event = %+v", seen[1])
	}
}
