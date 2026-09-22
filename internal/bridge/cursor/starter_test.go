// Package cursor — Starter tests.
//
// Unit-level tests for the Starter. Real-binary e2e lives in
// separate test files (build-tagged if needed).
package cursor

import (
	"errors"
	"os/exec"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
)

// TestStarter_Info verifies the Starter's Info() exposes the
// correct ModeACP + the canonical DefaultACPArgs (full-access
// parent flags then "acp").
func TestStarter_Info(t *testing.T) {
	s := NewStarter("cursor", "cursor-agent", DefaultACPArgs)
	info := s.Info()
	if info.Name != "cursor" {
		t.Errorf("Info().Name = %q, want cursor", info.Name)
	}
	if info.Mode != agent.ModeACP {
		t.Errorf("Info().Mode = %v, want agent.ModeACP", info.Mode)
	}
	if info.Command != "cursor-agent" {
		t.Errorf("Info().Command = %q, want cursor-agent", info.Command)
	}
	if !equalStrings(info.Args, DefaultACPArgs) {
		t.Errorf("Info().Args = %v, want %v", info.Args, DefaultACPArgs)
	}
}

// TestDefaultACPArgs_ForceBeforeACP locks the argv order that
// cursor-agent actually parses: parent flags MUST precede the
// `acp` subcommand. `cursor-agent acp --force` is a silent
// no-op (acp has no --force flag and starts the server anyway).
func TestDefaultACPArgs_ForceBeforeACP(t *testing.T) {
	if len(DefaultACPArgs) < 2 {
		t.Fatalf("DefaultACPArgs too short: %v", DefaultACPArgs)
	}
	if DefaultACPArgs[len(DefaultACPArgs)-1] != "acp" {
		t.Errorf("DefaultACPArgs last = %q, want acp (subcommand last)", DefaultACPArgs[len(DefaultACPArgs)-1])
	}
	if DefaultACPArgs[0] == "acp" {
		t.Errorf("DefaultACPArgs starts with acp; full-access flags would be ignored")
	}
	want := []string{"--force", "--trust", "--sandbox", "disabled", "--approve-mcps", "acp"}
	if !equalStrings(DefaultACPArgs, want) {
		t.Errorf("DefaultACPArgs = %v, want %v", DefaultACPArgs, want)
	}
	if !equalStrings(FullAccessArgs, want[:len(want)-1]) {
		t.Errorf("FullAccessArgs = %v, want %v", FullAccessArgs, want[:len(want)-1])
	}
}

// TestPrintModeArgs_PrependsFullAccess verifies print-mode
// uses the same parent flags, still before `-p`.
func TestPrintModeArgs_PrependsFullAccess(t *testing.T) {
	got := printModeArgs("hello", nil)
	want := withFullAccess("-p", "hello", "--output-format", "text")
	if !equalStrings(got, want) {
		t.Errorf("printModeArgs = %v, want %v", got, want)
	}
	extra := printModeArgs("hello", []string{"--model", "auto"})
	if extra[len(extra)-2] != "--model" || extra[len(extra)-1] != "auto" {
		t.Errorf("printModeArgs extra not appended: %v", extra)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestStarter_Info_DefensiveCopy verifies mutating the caller's
// args slice after NewStarter does not affect the Starter
// (defensive copy invariant shared with all bridges).
func TestStarter_Info_DefensiveCopy(t *testing.T) {
	args := []string{"acp"}
	s := NewStarter("cursor", "cursor-agent", args)
	args[0] = "MUTATED"
	info := s.Info()
	if info.Args[0] != "acp" {
		t.Errorf("Info().Args[0] = %q, want acp (defensive copy failed)", info.Args[0])
	}
}

// TestStarter_Detect_BinaryNotOnPath verifies Detect returns
// an error for a non-existent binary.
func TestStarter_Detect_BinaryNotOnPath(t *testing.T) {
	s := NewStarter("cursor", "cursor-does-not-exist-xyz-12345", nil)
	if err := s.Detect(); err == nil {
		t.Errorf("Detect() error = nil, want non-nil for missing binary")
	}
}

// TestStarter_Detect_PassForRealBinary is skipped unless the
// real `cursor-agent` binary is on PATH. Allows manual
// verification that Detect works against a real Cursor CLI
// install (matches what the official installer drops on PATH
// on every platform — bash installer creates it as a legacy
// symlink alongside the primary `agent` alias, PowerShell
// installer (https://cursor.com/install?win32=true) creates
// cursor-agent.cmd as the real entry).
func TestStarter_Detect_PassForRealBinary(t *testing.T) {
	if _, err := exec.LookPath("cursor-agent"); err != nil {
		t.Skipf("cursor-agent binary not on PATH: %v", err)
	}
	s := NewStarter("cursor", "cursor-agent", nil)
	if err := s.Detect(); err != nil {
		t.Errorf("Detect() error = %v, want nil", err)
	}
}

// TestPickLatestMatchingSession pins the session/load-fallback
// selection: pick the most recently updated session whose CWD
// matches, skip the originally-rejected id, and never pick an
// empty id. Mirrors the wire behaviour verified at
// /tmp/cursor-acp-probe/probe_resume_e2e.py — cursor's session/list
// orders entries newest-first, but we don't rely on that ordering
// and instead use UpdatedAt so other ACP agents without that
// ordering still work.
func TestPickLatestMatchingSession(t *testing.T) {
	workspace := "/Users/geax/code/geax/repo"
	skipID := "32c7da3f-4365-4a09-a3a4-bc69ffaccff8" // the rejected id

	cases := []struct {
		name     string
		sessions []acp.SessionInfo
		want     string
	}{
		{
			name:     "empty list returns empty",
			sessions: nil,
			want:     "",
		},
		{
			name: "single matching session, not the rejected one",
			sessions: []acp.SessionInfo{
				{SessionID: "aaaa", CWD: workspace, UpdatedAt: "2026-09-22T10:00:00Z"},
			},
			want: "aaaa",
		},
		{
			name: "only the rejected id → no candidate",
			sessions: []acp.SessionInfo{
				{SessionID: skipID, CWD: workspace, UpdatedAt: "2026-09-22T09:00:00Z"},
			},
			want: "",
		},
		{
			name: "different cwd → skipped",
			sessions: []acp.SessionInfo{
				{SessionID: "bbbb", CWD: "/Users/other/repo", UpdatedAt: "2026-09-22T10:00:00Z"},
			},
			want: "",
		},
		{
			name: "multiple matches, picks the newest",
			sessions: []acp.SessionInfo{
				{SessionID: "older", CWD: workspace, UpdatedAt: "2026-09-21T10:00:00Z"},
				{SessionID: "newer", CWD: workspace, UpdatedAt: "2026-09-22T10:00:00Z"},
				{SessionID: "middle", CWD: workspace, UpdatedAt: "2026-09-22T05:00:00Z"},
			},
			want: "newer",
		},
		{
			name: "rejected id filtered even when its UpdatedAt is newest",
			sessions: []acp.SessionInfo{
				{SessionID: skipID, CWD: workspace, UpdatedAt: "2026-09-23T10:00:00Z"},
				{SessionID: "fallback", CWD: workspace, UpdatedAt: "2026-09-22T10:00:00Z"},
			},
			want: "fallback",
		},
		{
			name: "empty CWD in list entry treated as match (cursor does this on first call)",
			sessions: []acp.SessionInfo{
				{SessionID: "no-cwd", CWD: "", UpdatedAt: "2026-09-22T10:00:00Z"},
			},
			want: "no-cwd",
		},
		{
			name: "empty UpdatedAt falls back to iteration order — first non-skipped wins",
			sessions: []acp.SessionInfo{
				{SessionID: "first", CWD: workspace, UpdatedAt: ""},
				{SessionID: "second", CWD: workspace, UpdatedAt: ""},
			},
			want: "first",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pickLatestMatchingSession(tc.sessions, workspace, skipID)
			if got != tc.want {
				t.Errorf("pickLatestMatchingSession() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIsResumeUnhealthy verifies the wrapper recognises the bridge
// error class that triggers the list-and-retry fallback. Wrapped
// errors (fmt.Errorf with %w) must still match — that's how the acp
// driver reports session/load / session/resume failures.
func TestIsResumeUnhealthy(t *testing.T) {
	if !isResumeUnhealthy(agent.ErrResumeUnhealthy) {
		t.Error("isResumeUnhealthy(agent.ErrResumeUnhealthy) = false, want true")
	}
	wrapped := errors.Join(errors.New("session/load context"), agent.ErrResumeUnhealthy)
	if !isResumeUnhealthy(wrapped) {
		t.Error("isResumeUnhealthy(wrapped ErrResumeUnhealthy) = false, want true")
	}
	other := errors.New("some other failure")
	if isResumeUnhealthy(other) {
		t.Error("isResumeUnhealthy(other error) = true, want false")
	}
}
