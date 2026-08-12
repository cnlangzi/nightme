package main

import (
	"os"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/registry"
)

// TestKill_SkipsSessionWithoutPID asserts that a session with no
// usable PID (detached/ACP entries can have PID=0) is reported as
// skipped and its registry entry is left untouched — we must not
// claim a session is dead when we never signalled anything.
func TestKill_SkipsSessionWithoutPID(t *testing.T) {
	_, asFile, _, asDet := listFixture(t)

	oc := killSession(asFile, listRow{AgentSessionID: asDet.ID, PID: 0}, "", false)
	if oc.err != nil {
		t.Fatalf("killSession err = %v, want nil", oc.err)
	}
	if !strings.Contains(oc.result, "skipped") {
		t.Errorf("result = %q, want a skipped result", oc.result)
	}

	e, ok := asFile.Get(asDet.ID)
	if !ok {
		t.Fatalf("entry %s disappeared", asDet.ID)
	}
	if e.Status == registry.StatusExited {
		t.Errorf("entry marked exited although no process was signalled")
	}
}

// TestKill_SkipsSelf asserts the sweep never signals the process
// running the command, even if the store somehow records our own PID.
func TestKill_SkipsSelf(t *testing.T) {
	_, asFile, asRun, _ := listFixture(t)

	oc := killSession(asFile, listRow{AgentSessionID: asRun.ID, PID: os.Getpid()}, "", false)
	if oc.err != nil {
		t.Fatalf("killSession err = %v, want nil", oc.err)
	}
	if !strings.Contains(oc.result, "self") {
		t.Errorf("result = %q, want the self-skip result", oc.result)
	}
	e, _ := asFile.Get(asRun.ID)
	if e.Status == registry.StatusExited {
		t.Errorf("own session marked exited")
	}
}

// TestKill_MarkSessionExitedPreservesResumeID is the core registry
// contract: killing a session must not destroy the agent's resume id,
// because the next spawn replays `--resume <id>`.
func TestKill_MarkSessionExitedPreservesResumeID(t *testing.T) {
	_, asFile, asRun, _ := listFixture(t)
	if asRun.SessionID == "" {
		t.Fatalf("fixture precondition: running session should carry a resume id")
	}

	if err := markSessionExited(asFile, asRun.ID); err != nil {
		t.Fatalf("markSessionExited: %v", err)
	}

	e, ok := asFile.Get(asRun.ID)
	if !ok {
		t.Fatalf("entry %s deleted, want it preserved as exited", asRun.ID)
	}
	if e.Status != registry.StatusExited {
		t.Errorf("Status = %q, want %q", e.Status, registry.StatusExited)
	}
	if e.PID != 0 {
		t.Errorf("PID = %d, want 0", e.PID)
	}
	if e.ExitCode == nil || *e.ExitCode != killedExitCode {
		t.Errorf("ExitCode = %v, want %d", e.ExitCode, killedExitCode)
	}
	if e.SessionID != asRun.SessionID {
		t.Errorf("SessionID = %q, want %q (resume id must survive kill)", e.SessionID, asRun.SessionID)
	}
}

// TestKill_MarkSessionExitedMissingEntry asserts a race with the
// daemon deleting the entry is not an error: the end state we wanted
// is already on disk.
func TestKill_MarkSessionExitedMissingEntry(t *testing.T) {
	_, asFile, _, _ := listFixture(t)
	if err := markSessionExited(asFile, "as_does_not_exist"); err != nil {
		t.Fatalf("markSessionExited(missing) = %v, want nil", err)
	}
}

// TestKill_TableFormat verifies the report renders one row per
// outcome, including the failure detail.
func TestKill_TableFormat(t *testing.T) {
	var buf strings.Builder
	printKillTable(&buf, []killOutcome{
		{row: listRow{AgentSessionID: "as_run_1", ChatID: "oc_run", Agent: "claude", PID: 12345}, result: "killed"},
		{row: listRow{AgentSessionID: "as_det_1", ChatID: "oc_det", Agent: "codex", PID: 0}, result: "skipped (no pid)"},
	})
	out := buf.String()

	for _, want := range []string{"CHAT", "AGENT", "PID", "RESULT", "SID", "as_run_1", "oc_run", "claude", "12345", "killed", "skipped (no pid)"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\n%s", want, out)
		}
	}
	// PID=0 renders as "-" (same convention as list).
	if !strings.Contains(out, "-") {
		t.Errorf("table missing '-' placeholder for empty pid\n%s", out)
	}
}
