package gtw

// Tests for the provider's error classification: a CLI that is not
// installed and a CLI that cannot start are different failures, and
// the second one must not be reported as the first.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// TestIsExecutableNotFound — the helper must distinguish "binary
// missing" from generic subprocess errors.
//
// Only exec.ErrNotFound counts: that is what os/exec reports when
// the binary is not on PATH. The spawn failure that a gone working
// directory produces is a *fs.PathError, and it must NOT land here —
// it would send the user to install a binary they already have.
func TestIsExecutableNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"exec.LookPath miss", &exec.Error{Name: "gh", Err: exec.ErrNotFound}, true},
		{"PathError ENOENT", &fs.PathError{Op: "fork/exec", Path: "/usr/bin/gh", Err: syscall.ENOENT}, false},
		{"generic error", errors.New("exit 1"), false},
		{"wrapped exec.LookPath miss", fmt.Errorf("outer: %w", &exec.Error{Name: "glab", Err: exec.ErrNotFound}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExecutableNotFound(tc.err); got != tc.want {
				t.Fatalf("isExecutableNotFound(%v): got %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsCLIStartFailure — the spawn failure is the other half of
// the classification: the process never started.
func TestIsCLIStartFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"exec.LookPath miss", &exec.Error{Name: "gh", Err: exec.ErrNotFound}, false},
		{"PathError ENOENT", &fs.PathError{Op: "fork/exec", Path: "/usr/bin/gh", Err: syscall.ENOENT}, true},
		{"PathError other errno", &fs.PathError{Op: "fork/exec", Path: "/usr/bin/gh", Err: syscall.EACCES}, true},
		{"generic error", errors.New("exit 1"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCLIStartFailure(tc.err); got != tc.want {
				t.Fatalf("isCLIStartFailure(%v): got %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestExecCLIRunner_CannotStartNamesTheDirectory pins the fix for
// the misdiagnosis: a CLI bound to a directory that is gone cannot
// start, and the error says so — naming the directory — instead of
// claiming the binary is not installed.
//
// The spawn failure names the BINARY in its own text ("no such
// file or directory"), which is exactly why the old classification
// read it as a missing installation.
func TestExecCLIRunner_CannotStartNamesTheDirectory(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "worktree-was-removed")

	r := ExecCLIRunner{Dir: gone}
	_, _, err := r.Run(context.Background(), cliName(t), "--version")
	if err == nil {
		t.Fatal("Run: want an error, got nil")
	}
	if !errors.Is(err, ErrCLICannotStart) {
		t.Fatalf("Run: err = %v, want ErrCLICannotStart", err)
	}
	if errors.Is(err, ErrCLINotInstalled) {
		t.Fatalf("Run: err = %v, must not be reported as a missing installation", err)
	}
	if !strings.Contains(err.Error(), gone) {
		t.Errorf("Run: err = %v, want the directory %q named", err, gone)
	}
}

// TestExecCLIRunner_MissingBinaryIsNotInstalled is the other half:
// a binary that is genuinely not on PATH stays "not installed", with
// the install hint the user can act on.
func TestExecCLIRunner_MissingBinaryIsNotInstalled(t *testing.T) {
	r := ExecCLIRunner{Dir: t.TempDir()}
	_, _, err := r.Run(context.Background(), "nightme-gh-not-installed", "--version")
	if err == nil {
		t.Fatal("Run: want an error, got nil")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("Run: err = %v, want exec.ErrNotFound", err)
	}
	if errors.Is(err, ErrCLICannotStart) {
		t.Fatalf("Run: err = %v, a missing binary is not a start failure", err)
	}
	if isCLIStartFailure(err) {
		t.Fatalf("Run: err = %v, a missing binary must not read as a spawn failure", err)
	}
}

// TestWrapListPRError_ReportsTheStartFailureNotTheInstallHint
// pins what the user reads: a CLI that cannot start is reported as
// such, and the "brew install gh" hint stays reserved for a binary
// that is really not there.
func TestWrapListPRError_ReportsTheStartFailureNotTheInstallHint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	startErr := fmt.Errorf("%w: gh (in %s): %w", ErrCLICannotStart, dir,
		&fs.PathError{Op: "fork/exec", Path: "/usr/local/bin/gh", Err: syscall.ENOENT})

	err := wrapListPRError(startErr, "", "gh")
	if !errors.Is(err, ErrCLICannotStart) {
		t.Fatalf("wrapListPRError: err = %v, want ErrCLICannotStart", err)
	}
	if errors.Is(err, ErrCLINotInstalled) {
		t.Fatalf("wrapListPRError: err = %v, must not claim the CLI is not installed", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("wrapListPRError: err = %v, want the directory %q named", err, dir)
	}
	if strings.Contains(err.Error(), "brew install") {
		t.Errorf("wrapListPRError: err = %v, must not carry the install hint", err)
	}
}

// cliName returns a binary that exists on the host, so the test
// exercises the working-directory failure rather than a missing
// binary.
func cliName(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	return "sh"
}
