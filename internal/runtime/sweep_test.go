package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// sweepStarter is a minimal agent.Starter. The sweep only needs the
// name → binary mapping its Info() publishes; it never starts an
// agent.
type sweepStarter struct {
	name    string
	command string
}

func (s *sweepStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModePTY, s.command, nil, nil)
}
func (s *sweepStarter) Detect() error { return nil }
func (s *sweepStarter) Init(string)   {}
func (s *sweepStarter) Start(context.Context, agent.StartConfig) (*agent.Agent, error) {
	return nil, errors.New("sweepStarter: Start not implemented")
}
func (s *sweepStarter) RunOnce(context.Context, agent.StartConfig, []agent.ContentBlock, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("sweepStarter: RunOnce not implemented")
}
func (s *sweepStarter) Review(context.Context, agent.StartConfig, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("sweepStarter: Review not implemented")
}

// sweepRegistry builds a registry that maps agent names to binaries.
func sweepRegistry(pairs map[string]string) *agent.Registry {
	reg := agent.New()
	for name, command := range pairs {
		reg.Register(&sweepStarter{name: name, command: command})
	}
	return reg
}

// seedSweepEntry writes one agent_sessions.json row.
func seedSweepEntry(t *testing.T, asFile *registry.AgentSessionFile, id, agentName string, pid int, sessionID string) {
	t.Helper()
	now := time.Now()
	entry := &registry.AgentSessionEntry{
		ID:            id,
		ChatSessionID: "cs_test",
		Agent:         agentName,
		Cwd:           "/code/test",
		PID:           pid,
		Status:        registry.StatusRunning,
		SessionID:     sessionID,
		CreatedAt:     now,
		LastRunAt:     now,
	}
	if err := asFile.Upsert(entry); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}

// TestSweepOrphanAgents_TerminatesLiveOrphan is the crash-recovery
// case: the daemon that spawned an agent CLI was `kill -9`ed, so the
// child is still running and its registry row still claims
// "running". The next daemon has to clean it up — and record the
// exit code that says "nightme terminated this", not "the CLI died".
func TestSweepOrphanAgents_TerminatesLiveOrphan(t *testing.T) {
	asFile, _ := reconcileFixture(t)

	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sleep not available: %v", err)
	}
	pid := cmd.Process.Pid
	waitErr := make(chan error, 1)
	go func() {
		_, err := cmd.Process.Wait()
		waitErr <- err
	}()

	seedSweepEntry(t, asFile, "as_orphan", "sleep-agent", pid, "sess-preserve-me")

	swept, err := SweepOrphanAgents(
		asFile,
		sweepRegistry(map[string]string{"sleep-agent": "/bin/sleep"}),
		PidAlive,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("SweepOrphanAgents: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1", swept)
	}

	select {
	case <-waitErr:
		// Wait returning proves the orphan was terminated and reaped.
	case <-time.After(5 * time.Second):
		t.Fatalf("orphan pid %d survived the sweep", pid)
	}

	e, ok := asFile.Get("as_orphan")
	if !ok {
		t.Fatal("entry deleted, want it preserved as exited")
	}
	if e.Status != registry.StatusExited {
		t.Errorf("Status = %q, want %q", e.Status, registry.StatusExited)
	}
	if e.PID != 0 {
		t.Errorf("PID = %d, want 0", e.PID)
	}
	if e.ExitCode == nil || *e.ExitCode != registry.ExitCodeTerminatedByNightme {
		t.Errorf("ExitCode = %v, want %d", e.ExitCode, registry.ExitCodeTerminatedByNightme)
	}
	if e.SessionID != "sess-preserve-me" {
		t.Errorf("SessionID = %q, want %q (the resume id must survive)", e.SessionID, "sess-preserve-me")
	}
}

// TestSweepOrphanAgents_SkipsForeignPID is the safety net: the OS
// recycles PIDs, and agent_sessions.json outlives the daemon. A row
// whose PID now runs something else must not take that process down
// with it — the mismatch is logged and the process is left alone.
func TestSweepOrphanAgents_SkipsForeignPID(t *testing.T) {
	asFile, _ := reconcileFixture(t)

	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sleep not available: %v", err)
	}
	pid := cmd.Process.Pid
	waitErr := make(chan error, 1)
	go func() {
		_, err := cmd.Process.Wait()
		waitErr <- err
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waitErr
	})

	// The row claims "claude"; the live PID runs /bin/sleep.
	seedSweepEntry(t, asFile, "as_mismatch", "claude", pid, "sess-preserve-me")

	var logs bytes.Buffer
	swept, err := SweepOrphanAgents(
		asFile,
		sweepRegistry(map[string]string{"claude": "claude"}),
		PidAlive,
		slog.New(slog.NewTextHandler(&logs, nil)),
	)
	if err != nil {
		t.Fatalf("SweepOrphanAgents: %v", err)
	}
	if swept != 0 {
		t.Fatalf("swept = %d, want 0 — the PID belongs to another binary", swept)
	}

	select {
	case <-waitErr:
		t.Fatal("the unrelated process was terminated")
	case <-time.After(300 * time.Millisecond):
		// Still running — as it must be.
	}

	e, ok := asFile.Get("as_mismatch")
	if !ok {
		t.Fatal("entry disappeared")
	}
	if e.Status != registry.StatusRunning {
		t.Errorf("Status = %q, want %q — we proved nothing about this process", e.Status, registry.StatusRunning)
	}
	if e.PID != pid {
		t.Errorf("PID = %d, want %d", e.PID, pid)
	}
	if !strings.Contains(logs.String(), "different binary") {
		t.Errorf("the mismatch was not logged:\n%s", logs.String())
	}
}

// TestSweepOrphanAgents_UnknownAgentFailsOpen pins the fail-open
// policy for a row whose agent is no longer configured (renamed or
// removed): there is nothing to compare against, so the process is
// terminated — refusing would strand the row as "running" forever.
func TestSweepOrphanAgents_UnknownAgentFailsOpen(t *testing.T) {
	asFile, _ := reconcileFixture(t)

	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sleep not available: %v", err)
	}
	pid := cmd.Process.Pid
	waitErr := make(chan error, 1)
	go func() {
		_, err := cmd.Process.Wait()
		waitErr <- err
	}()

	seedSweepEntry(t, asFile, "as_unknown", "agent-that-was-removed", pid, "sess-preserve-me")

	swept, err := SweepOrphanAgents(
		asFile,
		sweepRegistry(nil),
		PidAlive,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("SweepOrphanAgents: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1", swept)
	}

	select {
	case <-waitErr:
	case <-time.After(5 * time.Second):
		t.Fatalf("orphan pid %d survived the sweep", pid)
	}
}

// TestSweepOrphanAgents_IgnoresDeadPID asserts the sweep leaves the
// entries ReconcileAgentSessions owns alone: a PID that is already
// dead gets exit code -3 from the reconcile pass, not -4 from the
// sweep.
func TestSweepOrphanAgents_IgnoresDeadPID(t *testing.T) {
	asFile, _ := reconcileFixture(t)

	seedSweepEntry(t, asFile, "as_dead", "sleep-agent", 1<<30, "sess-preserve-me")

	swept, err := SweepOrphanAgents(
		asFile,
		sweepRegistry(map[string]string{"sleep-agent": "/bin/sleep"}),
		PidAlive,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("SweepOrphanAgents: %v", err)
	}
	if swept != 0 {
		t.Fatalf("swept = %d, want 0 — the PID is dead, the reconcile pass owns that row", swept)
	}

	e, _ := asFile.Get("as_dead")
	if e.Status != registry.StatusRunning {
		t.Errorf("Status = %q, want %q — the sweep must not touch a dead PID", e.Status, registry.StatusRunning)
	}
}
