package main

import (
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/registry"
)

// TestList_FiltersExitedByDefault asserts that an exited AgentSession
// is filtered out by the default list path (and counted as GC'd).
func TestList_FiltersExitedByDefault(t *testing.T) {
	csFile, asFile, _, _ := listFixture(t)
	addExitedToFixture(t, asFile, "as_exited_1", "cs_oc_run", "/tmp/exited")

	rows, gced, err := loadListRows(csFile, asFile, false, false)
	if err != nil {
		t.Fatalf("loadListRows: %v", err)
	}
	if gced != 1 {
		t.Errorf("gced = %d, want 1", gced)
	}
	for _, r := range rows {
		if r.AgentSessionID == "as_exited_1" {
			t.Errorf("exited row leaked into output: %+v", r)
		}
	}
	// On-disk file should also be clean.
	if _, ok := asFile.Get("as_exited_1"); ok {
		t.Errorf("on-disk agent_sessions.json still contains exited row")
	}
}

// TestList_AllFlagShowsExited asserts that --all disables filtering.
func TestList_AllFlagShowsExited(t *testing.T) {
	csFile, asFile, _, _ := listFixture(t)
	addExitedToFixture(t, asFile, "as_exited_1", "cs_oc_run", "/tmp/exited")

	rows, gced, err := loadListRows(csFile, asFile, true, false)
	if err != nil {
		t.Fatalf("loadListRows: %v", err)
	}
	if gced != 0 {
		t.Errorf("gced = %d, want 0 (--all skips GC)", gced)
	}
	var foundExited bool
	for _, r := range rows {
		if r.AgentSessionID == "as_exited_1" {
			foundExited = true
		}
	}
	if !foundExited {
		t.Errorf("exited row missing from --all output")
	}
}

// TestList_KeepExitedFlagSkipsGC asserts that --keep-exited disables
// the GC step even when --all is not set.
func TestList_KeepExitedFlagSkipsGC(t *testing.T) {
	csFile, asFile, _, _ := listFixture(t)
	addExitedToFixture(t, asFile, "as_exited_1", "cs_oc_run", "/tmp/exited")

	rows, gced, err := loadListRows(csFile, asFile, false, true)
	if err != nil {
		t.Fatalf("loadListRows: %v", err)
	}
	if gced != 0 {
		t.Errorf("gced = %d, want 0 (--keep-exited skips GC)", gced)
	}
	// The exited row should be filtered from display (defaults) but
	// kept on disk.
	for _, r := range rows {
		if r.AgentSessionID == "as_exited_1" {
			t.Errorf("exited row should not appear in display when --all is not set")
		}
	}
	if _, ok := asFile.Get("as_exited_1"); !ok {
		t.Errorf("on-disk agent_sessions.json lost the exited row under --keep-exited")
	}
}

// TestList_ShowsResumeID asserts the resume column is populated for
// agents that captured a session id.
func TestList_ShowsResumeID(t *testing.T) {
	csFile, asFile, asRun, _ := listFixture(t)

	rows, _, err := loadListRows(csFile, asFile, false, false)
	if err != nil {
		t.Fatalf("loadListRows: %v", err)
	}

	var found bool
	for _, r := range rows {
		if r.AgentSessionID == asRun.ID {
			found = true
			if r.ResumeID != asRun.ResumeID {
				t.Errorf("ResumeID = %q, want %q", r.ResumeID, asRun.ResumeID)
			}
		}
	}
	if !found {
		t.Errorf("running session row missing from list")
	}
}

// TestList_ResumeIDColumn verifies the column header and the "-" placeholder
// for empty resume ids.
func TestList_ResumeIDColumn(t *testing.T) {
	csFile, asFile, _, _ := listFixture(t)
	rows, _, err := loadListRows(csFile, asFile, false, false)
	if err != nil {
		t.Fatalf("loadListRows: %v", err)
	}

	var buf strings.Builder
	printListTable(&buf, rows)
	out := buf.String()
	if !strings.Contains(out, "RESUME") {
		t.Errorf("table header missing RESUME column\n%s", out)
	}
	// Detached row has no resume id → "-". Should appear at least once.
	if !strings.Contains(out, "-") {
		t.Errorf("table missing '-' placeholder for empty resume id\n%s", out)
	}
}

// TestList_JoinMissingChatSession asserts that an AgentSession whose
// owning ChatSession has been deleted (orphan) still appears in the
// list with ChatID="(orphan)".
func TestList_JoinMissingChatSession(t *testing.T) {
	csFile, asFile, _, _ := listFixture(t)
	// Add an AgentSession whose ChatSessionID we never register.
	now := time.Now()
	if err := asFile.Upsert(&registry.AgentSessionEntry{
		ID:            "as_orphan_1",
		ChatSessionID: "cs_does_not_exist",
		Agent:         "claude",
		Cwd:           "/tmp/orphan",
		PID:           99999,
		Status:        registry.StatusRunning,
		CreatedAt:     now,
		LastRunAt:     now,
	}); err != nil {
		t.Fatalf("Upsert orphan: %v", err)
	}

	rows, _, err := loadListRows(csFile, asFile, false, false)
	if err != nil {
		t.Fatalf("loadListRows: %v", err)
	}

	var found bool
	for _, r := range rows {
		if r.AgentSessionID == "as_orphan_1" {
			found = true
			if r.ChatID != "(orphan)" {
				t.Errorf("orphan ChatID = %q, want %q", r.ChatID, "(orphan)")
			}
		}
	}
	if !found {
		t.Errorf("orphan row missing from list")
	}
}

// TestList_SortedByLastRunAt asserts the rows are sorted by LastRunAt
// descending (most recent first).
func TestList_SortedByLastRunAt(t *testing.T) {
	csFile, asFile, _, _ := listFixture(t)
	rows, _, err := loadListRows(csFile, asFile, false, false)
	if err != nil {
		t.Fatalf("loadListRows: %v", err)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].LastRunAt.After(rows[i-1].LastRunAt) {
			t.Errorf("rows not sorted desc by LastRunAt: row[%d]=%v is newer than row[%d]=%v",
				i, rows[i].LastRunAt, i-1, rows[i-1].LastRunAt)
		}
	}
}

// TestList_PreservesExitedWithResumeID asserts that an exited
// AgentSession carrying a non-empty ResumeID is NOT garbage-collected
// by `nightme list`. The resume id is the only durable handle the
// next respawn of the same (chat, agent, cwd) tuple has onto the
// agent's prior session (e.g. Claude Code's
// `system/init.session_id`); deleting it would force a fresh
// conversation and lose user state. Exited entries without a resume
// id are still GC'd (the agent has no replay surface).
func TestList_PreservesExitedWithResumeID(t *testing.T) {
	csFile, asFile, _, _ := listFixture(t)

	now := time.Now()
	code := 0
	if err := asFile.Upsert(&registry.AgentSessionEntry{
		ID:            "as_exited_resume",
		ChatSessionID: "cs_oc_run",
		Agent:         "claude",
		Cwd:           "/tmp/w",
		Status:        registry.StatusExited,
		ExitCode:      &code,
		ResumeID:      "sess-preserve-me",
		CreatedAt:     now,
		LastRunAt:     now,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	gced, err := 0, error(nil)
	_ = gced
	_ = err
	rows, gced, err := loadListRows(csFile, asFile, false, false)
	if err != nil {
		t.Fatalf("loadListRows: %v", err)
	}

	if gced != 0 {
		t.Errorf("exited entry with ResumeID was GC'd (gced=%d); it must be preserved", gced)
	}
	if _, ok := asFile.Get("as_exited_resume"); !ok {
		t.Errorf("on-disk agent_sessions.json lost the resume-id-bearing exited row")
	}
	// Default list view filters exited from display.
	for _, r := range rows {
		if r.AgentSessionID == "as_exited_resume" {
			t.Errorf("exited row leaked into default list output: %+v", r)
		}
	}
}
