package runtime

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/registry"
)

// reconcileFixture opens a fresh agent_sessions.json and returns it
// alongside the path so individual tests can seed entries directly.
func reconcileFixture(t *testing.T) (*registry.AgentSessionFile, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent_sessions.json")
	asFile, err := registry.OpenAgentSessionFile(path)
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}
	return asFile, path
}

// seedRunning writes a StatusRunning entry with the given PID.
func seedRunning(t *testing.T, asFile *registry.AgentSessionFile, id string, pid int, withSessionID bool) {
	t.Helper()
	now := time.Now()
	entry := &registry.AgentSessionEntry{
		ID:            id,
		ChatSessionID: "cs_test",
		Agent:         "claude",
		Cwd:           "/code/test",
		PID:           pid,
		Status:        registry.StatusRunning,
		CreatedAt:     now,
		LastRunAt:     now,
	}
	if withSessionID {
		entry.SessionID = "sess-preserve-me"
	}
	if err := asFile.Upsert(entry); err != nil {
		t.Fatalf("Upsert running: %v", err)
	}
}

// seedDetached writes a StatusDetached entry with the given PID.
func seedDetached(t *testing.T, asFile *registry.AgentSessionFile, id string, pid int) {
	t.Helper()
	now := time.Now()
	entry := &registry.AgentSessionEntry{
		ID:            id,
		ChatSessionID: "cs_test",
		Agent:         "claude",
		Cwd:           "/code/test",
		PID:           pid,
		Status:        registry.StatusDetached,
		CreatedAt:     now,
		LastRunAt:     now,
	}
	if err := asFile.Upsert(entry); err != nil {
		t.Fatalf("Upsert detached: %v", err)
	}
}

// seedExited writes a StatusExited entry; reconcile must NOT touch it.
func seedExited(t *testing.T, asFile *registry.AgentSessionFile, id string, code int) {
	t.Helper()
	now := time.Now()
	entry := &registry.AgentSessionEntry{
		ID:            id,
		ChatSessionID: "cs_test",
		Agent:         "claude",
		Cwd:           "/code/test",
		Status:        registry.StatusExited,
		ExitCode:      &code,
		CreatedAt:     now,
		LastRunAt:     now,
	}
	if err := asFile.Upsert(entry); err != nil {
		t.Fatalf("Upsert exited: %v", err)
	}
}

// silentLogger returns a logger that drops every record. Tests
// only care about side effects (the ReconcileResult); the log
// output is noise.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestReconcile_ReapsDeadRunningPID spins up a real child, waits for
// it to exit, then runs reconcile with the production probe.
// Asserts: entry is flipped to StatusExited, PID cleared, ExitCode
// is the reconcile sentinel.
func TestReconcile_ReapsDeadRunningPID(t *testing.T) {
	asFile, _ := reconcileFixture(t)

	// Spawn a real short-lived child, capture its PID, Wait to reap.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	seedRunning(t, asFile, "as_dead_run", pid, true /* withSessionID */)

	res, err := ReconcileAgentSessions(asFile, PidAlive, silentLogger(), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ReconcileAgentSessions: %v", err)
	}
	if res.Probed != 1 {
		t.Errorf("Probed = %d, want 1", res.Probed)
	}
	if res.Reaped != 1 {
		t.Errorf("Reaped = %d, want 1", res.Reaped)
	}

	e, ok := asFile.Get("as_dead_run")
	if !ok {
		t.Fatalf("entry missing after reconcile")
	}
	if e.Status != registry.StatusExited {
		t.Errorf("Status = %q, want %q", e.Status, registry.StatusExited)
	}
	if e.PID != 0 {
		t.Errorf("PID = %d, want 0", e.PID)
	}
	if e.ExitCode == nil || *e.ExitCode != -3 {
		var got *int = e.ExitCode
		t.Errorf("ExitCode = %v, want pointer to -3", got)
	}
	if e.SessionID != "sess-preserve-me" {
		t.Errorf("SessionID = %q, want %q (resume id must survive reconcile)", e.SessionID, "sess-preserve-me")
	}
}

// TestReconcile_ClearsSuspectState pins the F-61 contract: a dead
// AS has nothing left to probe, so SuspectReason / SuspectSince
// must be cleared on the terminal transition (mirrors the
// in-memory AgentSession.SetExited path).
func TestReconcile_ClearsSuspectState(t *testing.T) {
	asFile, _ := reconcileFixture(t)

	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	deadPID := cmd.Process.Pid
	_ = cmd.Wait()

	now := time.Now()
	if err := asFile.Upsert(&registry.AgentSessionEntry{
		ID:            "as_suspect",
		ChatSessionID: "cs_test",
		Agent:         "claude",
		Cwd:           "/code/test",
		PID:           deadPID,
		Status:        registry.StatusRunning,
		SuspectReason: "hung_prompt",
		SuspectSince:  &now,
		CreatedAt:     now,
		LastRunAt:     now,
	}); err != nil {
		t.Fatalf("Upsert suspect: %v", err)
	}

	res, err := ReconcileAgentSessions(asFile, PidAlive, silentLogger(), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ReconcileAgentSessions: %v", err)
	}
	if res.Reaped != 1 {
		t.Errorf("Reaped = %d, want 1", res.Reaped)
	}

	e, _ := asFile.Get("as_suspect")
	if e.SuspectReason != "" {
		t.Errorf("SuspectReason = %q, want \"\" (must clear on terminal transition)", e.SuspectReason)
	}
	if e.SuspectSince != nil {
		t.Errorf("SuspectSince = %v, want nil", e.SuspectSince)
	}
}

// TestReconcile_LeavesLivePIDAlone uses a controlled probe that
// reports the recorded PID as alive, and asserts the entry is
// untouched. We can't use the production probe here because the
// fixture's PID is fake and dead.
func TestReconcile_LeavesLivePIDAlone(t *testing.T) {
	asFile, _ := reconcileFixture(t)

	seedRunning(t, asFile, "as_live", 12345, true)

	live := func(int) bool { return true }
	res, err := ReconcileAgentSessions(asFile, live, silentLogger(), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ReconcileAgentSessions: %v", err)
	}
	if res.Probed != 1 {
		t.Errorf("Probed = %d, want 1", res.Probed)
	}
	if res.Reaped != 0 {
		t.Errorf("Reaped = %d, want 0", res.Reaped)
	}

	e, ok := asFile.Get("as_live")
	if !ok {
		t.Fatalf("entry missing")
	}
	if e.Status != registry.StatusRunning {
		t.Errorf("Status = %q, want %q", e.Status, registry.StatusRunning)
	}
	if e.PID != 12345 {
		t.Errorf("PID = %d, want 12345", e.PID)
	}
}

// TestReconcile_HandlesMixedBatch exercises the typical post-crash
// shape: a real dead PID, a fake "live" PID, a detached entry, and
// an already-exited entry. Only the dead ones should be reaped.
func TestReconcile_HandlesMixedBatch(t *testing.T) {
	asFile, _ := reconcileFixture(t)

	// A real dead PID.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	deadPID := cmd.Process.Pid
	_ = cmd.Wait()

	seedRunning(t, asFile, "as_dead", deadPID, false)
	seedRunning(t, asFile, "as_fake_live", 12345, false)
	seedDetached(t, asFile, "as_det", 12346)
	seedExited(t, asFile, "as_already_exited", 0)

	// fake_live + det are "alive" per the probe; deadPID is dead.
	probe := func(pid int) bool {
		return pid == 12345 || pid == 12346
	}
	res, err := ReconcileAgentSessions(asFile, probe, silentLogger(), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ReconcileAgentSessions: %v", err)
	}
	if res.Probed != 3 {
		t.Errorf("Probed = %d, want 3 (running*2 + detached, skipped exited)", res.Probed)
	}
	if res.Reaped != 1 {
		t.Errorf("Reaped = %d, want 1", res.Reaped)
	}

	if e, _ := asFile.Get("as_dead"); e.Status != registry.StatusExited {
		t.Errorf("as_dead Status = %q, want %q", e.Status, registry.StatusExited)
	}
	if e, _ := asFile.Get("as_fake_live"); e.Status != registry.StatusRunning {
		t.Errorf("as_fake_live Status = %q, want %q (must not be touched)", e.Status, registry.StatusRunning)
	}
	if e, _ := asFile.Get("as_det"); e.Status != registry.StatusDetached {
		t.Errorf("as_det Status = %q, want %q (must not be touched)", e.Status, registry.StatusDetached)
	}
	if e, _ := asFile.Get("as_already_exited"); e.Status != registry.StatusExited {
		t.Errorf("as_already_exited Status = %q, want %q", e.Status, registry.StatusExited)
	}
}

// TestReconcile_SkipsPIDZero confirms we never probe a sentinel
// "not running" PID. PID=0 entries are already semantically
// non-running; an OS probe would be meaningless.
func TestReconcile_SkipsPIDZero(t *testing.T) {
	asFile, _ := reconcileFixture(t)

	seedRunning(t, asFile, "as_zero_pid", 0, false)

	called := 0
	probe := func(int) bool { called++; return false }

	res, err := ReconcileAgentSessions(asFile, probe, silentLogger(), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ReconcileAgentSessions: %v", err)
	}
	if res.Probed != 0 {
		t.Errorf("Probed = %d, want 0 (PID 0 must not be probed)", res.Probed)
	}
	if called != 0 {
		t.Errorf("probe called %d times for PID 0; must be 0", called)
	}
	if e, _ := asFile.Get("as_zero_pid"); e.Status != registry.StatusRunning {
		t.Errorf("Status = %q, want %q (untouched)", e.Status, registry.StatusRunning)
	}
}

// TestReconcile_NilStoreIsNoOp guards the bootstrap path: a caller
// that has not yet opened the agent_sessions.json must not crash.
func TestReconcile_NilStoreIsNoOp(t *testing.T) {
	res, err := ReconcileAgentSessions(nil, PidAlive, silentLogger(), &bytes.Buffer{})
	if err != nil {
		t.Errorf("nil store returned error: %v", err)
	}
	if res.Probed != 0 || res.Reaped != 0 {
		t.Errorf("res = %+v, want zero", res)
	}
}

// TestPidAlive_CurrentProcess pins the production probe's
// behaviour for a live process: it must return true for ourselves.
func TestPidAlive_CurrentProcess(t *testing.T) {
	if !PidAlive(os.Getpid()) {
		t.Errorf("PidAlive(self) = false, want true")
	}
}

// TestPidAlive_FakeDeadPID confirms a PID nobody is using returns
// false. Picked well above the Linux default pid_max (4M) to make
// a collision with a real process extremely unlikely; the test
// skips rather than fails if a host happens to have one.
func TestPidAlive_FakeDeadPID(t *testing.T) {
	const definitelyDead = 1 << 30
	if PidAlive(definitelyDead) {
		t.Skipf("pid %d unexpectedly reports alive on this kernel", definitelyDead)
	}
}

// TestPidAlive_RejectsZero pins the contract: PID 0 is the
// "not running" sentinel and must never be probed.
func TestPidAlive_RejectsZero(t *testing.T) {
	if PidAlive(0) {
		t.Errorf("PidAlive(0) = true, want false")
	}
	if PidAlive(-1) {
		t.Errorf("PidAlive(-1) = true, want false")
	}
}
