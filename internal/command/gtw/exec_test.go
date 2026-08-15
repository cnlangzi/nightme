package gtw

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRunCmd_DirBinding is the regression test for the original
// bug: when gh pr create was invoked, gh forked git which then
// failed with `fatal: Unable to read current working directory`
// because the daemon's CWD no longer existed. The fix is the
// dir-binds-cmd.Dir contract in runCmd; this test pins it.
//
// The contract: when nightme sets `cmd.Dir = X`, the child
// process MUST be able to operate in directory X. We test the
// CONTRACT, not the child's "pwd" output (which is a flaky
// test artifact — MSYS libc's intercepted getcwd() returns
// translated paths like `/c/Users/...` even when cmd.Dir is the
// real `C:\Users\...`).
//
// To test the contract on every platform, we have the child
// create a sentinel file in its current dir, then verify the
// file is visible at the EXACT path we set cmd.Dir to. If cmd.Dir
// wasn't honored, the child would have created the file in a
// different dir and the sentinel check would fail.
//
// The sentinel name is a unique string so two parallel test
// runs (e.g. t.Parallel with a shared tmpfs) don't collide.
func TestRunCmd_DirBinding(t *testing.T) {
	dir := t.TempDir()
	// t.TempDir() may be a symlink on macOS (/var → /private/var);
	// resolve so the comparison below is path-stable.
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	sentinelName := fmt.Sprintf("nightme-sentinel-%d", os.Getpid())
	// On Unix: use the system shell. On Windows: use a command
	// that's NOT an MSYS-bash tool. `cmd.exe /c` is Windows-native
	// and queries the kernel's CWD directly (no MSYS libc
	// interception), so the file lands at the real cmd.Dir path.
	var cmdName string
	var cmdArgs []string
	if runtime.GOOS == "windows" {
		cmdName = "cmd.exe"
		cmdArgs = []string{"/c", "echo " + sentinelName + " > " + sentinelName}
	} else {
		cmdName = "sh"
		cmdArgs = []string{"-c", "echo " + sentinelName + " > " + sentinelName}
	}

	_, _, err = runCmd(context.Background(), real, cmdName, cmdArgs...)
	if err != nil {
		t.Fatalf("runCmd: %v", err)
	}

	// Sentinel file MUST exist at the EXACT path nightme set
	// cmd.Dir to. If cmd.Dir wasn't honored (e.g., the runCmd
	// implementation regresses), the file would be in a different
	// dir and this assertion would fail. MSYS translation of
	// the path is irrelevant here because we use the *real* path
	// (the same `real` we passed to cmd.Dir) for the stat.
	sentinelPath := filepath.Join(real, sentinelName)
	if _, err := os.Stat(sentinelPath); err != nil {
		t.Fatalf("sentinel file not at %s: %v (runCmd did not honor cmd.Dir=%q)",
			sentinelPath, err, real)
	}
}

// TestRunCmd_EmptyDirInherits: when dir is empty, the child runs
// in the parent process's CWD. This preserves the historical
// "no workspace yet" path used by callers that haven't determined
// their worktree.
//
// We test the contract (child operates in the parent's CWD) by
// asking the child to create a sentinel file and verifying it
// lands in the parent's CWD. On Windows we use cmd.exe to avoid
// the MSYS libc getcwd() translation that pwd exhibits — what
// matters is whether the child's file-system operations honour
// cmd.Dir, not what its shell getcwd reports.
func TestRunCmd_EmptyDirInherits(t *testing.T) {
	parent, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Resolve symlinks so the sentinel check works on macOS
	// (/var → /private/var) without the parent-cwd comparison
	// mismatch that bit the previous pwd-based implementation.
	parentReal, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}

	sentinelName := fmt.Sprintf("nightme-inherit-%d", os.Getpid())
	var cmdName string
	var cmdArgs []string
	if runtime.GOOS == "windows" {
		cmdName = "cmd.exe"
		cmdArgs = []string{"/c", "echo " + sentinelName + " > " + sentinelName}
	} else {
		cmdName = "sh"
		cmdArgs = []string{"-c", "echo " + sentinelName + " > " + sentinelName}
	}

	_, _, err = runCmd(context.Background(), "", cmdName, cmdArgs...)
	if err != nil {
		t.Fatalf("runCmd: %v", err)
	}

	sentinelPath := filepath.Join(parentReal, sentinelName)
	if _, err := os.Stat(sentinelPath); err != nil {
		t.Fatalf("sentinel file not at %s: %v (runCmd with empty dir "+
			"should inherit parent CWD %q)", sentinelPath, err, parentReal)
	}
	// Clean up the sentinel so the test package's tmp dir
	// (which we don't own) doesn't accumulate files.
	_ = os.Remove(sentinelPath)
}

// TestRunCmd_EmptyNameRejected: name == "" is a programmer error,
// not a runtime condition. Surface it eagerly so callers can't
// silently spawn an empty argv.
func TestRunCmd_EmptyNameRejected(t *testing.T) {
	_, _, err := runCmd(context.Background(), "", "")
	if err == nil {
		t.Fatal("runCmd with empty name: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "empty command name") {
		t.Errorf("error %q should mention empty command name", err.Error())
	}
}

// TestRunCmd_StripsTrailingNewline: trims a single trailing \n
// from each captured stream, matching the historical contract
// every runner in this package relied on.
func TestRunCmd_StripsTrailingNewline(t *testing.T) {
	// `printf` writes exactly the bytes we tell it to — no implicit
	// newline the way `echo` would.
	//
	// Windows: skip — Git for Windows ships an MSYS-coreutils
	// `printf` that warns "ignoring excess arguments, starting
	// with 'world'" when given two positional args. This isn't a
	// runCmd behaviour issue (runCmd forwards the args verbatim);
	// it's a quirk of the MSYS printf binary that doesn't apply
	// to real-world nightme use (which uses real git/gh/glab, not
	// MSYS printf). The trim-trailing-newline contract itself
	// is exercised by the other `sh -c "echo ..."` tests below.
	if runtime.GOOS == "windows" {
		t.Skipf("Git-for-Windows MSYS printf has a different positional-arg warning; " +
			"see TestRunCmd_StripsTrailingNewline comment")
	}
	stdout, stderr, err := runCmd(context.Background(), "", "printf", "hello\nworld\n")
	if err != nil {
		t.Fatalf("runCmd: %v (stderr=%s)", err, stderr)
	}
	if stdout != "hello\nworld" {
		t.Errorf("stdout = %q, want %q (one trailing \\n should be trimmed)", stdout, "hello\nworld")
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

// TestRunCmd_StderrCaptured: stderr is captured into the second
// return value, not into stdout. Regression test in case a future
// refactor swaps the buffers.
func TestRunCmd_StderrCaptured(t *testing.T) {
	stdout, stderr, err := runCmd(context.Background(), "", "sh", "-c", "echo out; echo err 1>&2")
	if err != nil {
		t.Fatalf("runCmd: %v", err)
	}
	if stdout != "out" {
		t.Errorf("stdout = %q, want %q", stdout, "out")
	}
	if stderr != "err" {
		t.Errorf("stderr = %q, want %q", stderr, "err")
	}
}

// TestRunCmd_PropagatesExitError: a non-zero exit must surface
// as a non-nil error AND keep the captured stderr, so callers
// like GitHubProvider.CreatePR can `strings.Contains(stderr, "...")`
// to detect "already exists" and similar stderr-keyed branches.
func TestRunCmd_PropagatesExitError(t *testing.T) {
	stdout, stderr, err := runCmd(context.Background(), "", "false")
	if err == nil {
		t.Fatal("runCmd(false): expected non-nil err for exit 1, got nil")
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("err = %v, want message containing \"exit status 1\"", err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty (false produces no output)", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty (false produces no output)", stderr)
	}
}

// TestExecCLIRunner_DirPropagates: ExecCLIRunner.Dir must flow
// into runCmd so the spawned gh/glab runs in that directory. This
// is the single property that, if regressed, re-opens the original
// `gtw pr` ENOENT bug.
//
// We test the contract via a sentinel file in the runner's Dir
// (rather than parsing pwd output, which is MSYS-translated on
// Windows). On Windows the child is cmd.exe — Windows-native,
// queries kernel cwd directly, doesn't go through MSYS libc.
func TestExecCLIRunner_DirPropagates(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	sentinelName := fmt.Sprintf("nightme-clirunner-%d", os.Getpid())
	var cmdName string
	var cmdArgs []string
	if runtime.GOOS == "windows" {
		cmdName = "cmd.exe"
		cmdArgs = []string{"/c", "echo " + sentinelName + " > " + sentinelName}
	} else {
		cmdName = "sh"
		cmdArgs = []string{"-c", "echo " + sentinelName + " > " + sentinelName}
	}

	r := ExecCLIRunner{Dir: real}
	if _, _, err := r.Run(context.Background(), cmdName, cmdArgs...); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, sentinelName)); err != nil {
		t.Fatalf("sentinel not at %s: %v (ExecCLIRunner.Dir not honored)", filepath.Join(real, sentinelName), err)
	}
}

// TestGitHubProvider_RunnerBindsWorktree is the regression test
// for the original `gtw pr` ENOENT bug end-to-end:
//
//   GitHubProvider.Worktree (set by resolveProvider from
//   c.Worktree) → provider.runner() returns ExecCLIRunner{Dir:
//   Worktree} → spawned gh process runs in that directory
//   instead of inheriting the daemon's possibly-stale CWD.
//
// Any future refactor that drops the Worktree → Dir binding
// re-introduces the bug; this test fails immediately.
//
// We verify via sentinel file in the Worktree, not pwd output
// (see TestRunCmd_DirBinding's MSYS rationale).
func TestGitHubProvider_RunnerBindsWorktree(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	sentinelName := fmt.Sprintf("nightme-gh-%d", os.Getpid())
	var cmdName string
	var cmdArgs []string
	if runtime.GOOS == "windows" {
		cmdName = "cmd.exe"
		cmdArgs = []string{"/c", "echo " + sentinelName + " > " + sentinelName}
	} else {
		cmdName = "sh"
		cmdArgs = []string{"-c", "echo " + sentinelName + " > " + sentinelName}
	}

	p := &GitHubProvider{Worktree: real}
	// Pull the production runner (Runner is nil by default → falls
	// through to ExecCLIRunner{Dir: p.Worktree}).
	r := p.runner()

	if _, _, err := r.Run(context.Background(), cmdName, cmdArgs...); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, sentinelName)); err != nil {
		t.Fatalf("gh-equivalent sentinel not at %s: %v (Worktree → Dir binding broken)",
			filepath.Join(real, sentinelName), err)
	}
}

// TestGitLabProvider_RunnerBindsWorktree: same contract for the
// GitLab code path (glab). Two tests rather than one to make the
// regression message unambiguous — a failure on GitHub but not
// GitLab (or vice versa) points right at the missing binding.
func TestGitLabProvider_RunnerBindsWorktree(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	sentinelName := fmt.Sprintf("nightme-gl-%d", os.Getpid())
	var cmdName string
	var cmdArgs []string
	if runtime.GOOS == "windows" {
		cmdName = "cmd.exe"
		cmdArgs = []string{"/c", "echo " + sentinelName + " > " + sentinelName}
	} else {
		cmdName = "sh"
		cmdArgs = []string{"-c", "echo " + sentinelName + " > " + sentinelName}
	}

	p := &GitLabProvider{Worktree: real}
	r := p.runner()

	if _, _, err := r.Run(context.Background(), cmdName, cmdArgs...); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, sentinelName)); err != nil {
		t.Fatalf("glab-equivalent sentinel not at %s: %v (Worktree → Dir binding broken)",
			filepath.Join(real, sentinelName), err)
	}
}
// TestRunCmd_RespectsCallerDeadline pins the runCmd safety net's
// "caller always wins" contract: when the caller passes a ctx
// with a deadline shorter than timeouts.CLI (5 min), runCmd must
// NOT override it. The 100 ms deadline below is two thousand
// times shorter than CLI; if runCmd applied its own wrap, the
// call would block for the full 5 minutes.
//
// The original bug this guards against: someone flipping the
// condition `if hasDeadline` → `if !hasDeadline` (or removing the
// guard entirely) would silently cap every call at 5 min, which
// is fine for fast commands but breaks legitimate longer-running
// calls like /gtw commit's 30-min agent budget.
func TestRunCmd_RespectsCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	// `sleep 5` would block for 5 seconds under timeouts.CLI's
	// fallback wrap; with the caller's 100 ms deadline honored,
	// it should return within ~200 ms with a context error.
	sleepName := "sleep"
	sleepArgs := []string{"5"}
	if runtime.GOOS == "windows" {
		// `timeout` is the Windows equivalent of sleep that
		// responds to cancellation; without it, cmd /c "sleep 5"
		// wouldn't terminate cleanly on a short ctx deadline.
		sleepName = "cmd"
		sleepArgs = []string{"/c", "ping -n 6 127.0.0.1 > nul"}
	}
	_, _, err := runCmd(ctx, "", sleepName, sleepArgs...)
	elapsed := time.Since(start)

	if err == nil {
		t.Errorf("expected timeout error, got nil (runCmd did not honor ctx deadline)")
	}
	if elapsed > 2*time.Second {
		t.Errorf("runCmd ran for %v despite a 100ms ctx deadline; safety net overrode caller", elapsed)
	}
}
