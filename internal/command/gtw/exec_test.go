package gtw

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRunCmd_DirBinding is the regression test for the original
// bug: when gh pr create was invoked, gh forked git which then
// failed with `fatal: Unable to read current working directory`
// because the daemon's CWD no longer existed. The fix is the
// dir-binds-cmd.Dir contract in runCmd; this test pins it.
//
// Strategy: spawn a process that prints its CWD via `pwd`, with
// dir explicitly set to a temp dir. Assert the printed CWD matches
// the temp dir, NOT whatever the test process inherited.
//
// Unix-only: on Windows, `pwd` resolves to an MSYS-shipped
// binary whose getcwd() returns MSYS-translated paths
// (`/c/Users/...` instead of `C:\Users\...`). The MSYS path
// translation is built into MSYS's libc interception layer and
// is not affected by `MSYS_NO_PATHCONV=1` (which only controls
// argv path conversion, not getcwd conversion). Since this test
// asserts a string-level equality between the cmd.Dir we set and
// the child-reported CWD, it cannot run on a Windows host that
// ships MSYS — there's no nightme-side fix that would satisfy
// the assertion. The dir-binds-cmd.Dir contract itself is
// already covered by `runCmd` unit tests on Linux; the Windows
// behaviour is "the kernel reports the same CWD we set,
// translated through MSYS's libc layer".
func TestRunCmd_DirBinding(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("MSYS getcwd() reports translated paths; the equality assertion " +
			"cannot hold even with a correct cmd.Dir. The dir-binds-cmd.Dir " +
			"contract is verified on Unix by this test; on Windows the " +
			"nightme-side cmd.Dir is set correctly, the child just reports " +
			"it in MSYS format.")
	}
	dir := t.TempDir()
	// t.TempDir() may be a symlink on macOS (/var → /private/var);
	// resolve so the comparison below is path-stable.
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	stdout, stderr, err := runCmd(context.Background(), real, "pwd")
	if err != nil {
		t.Fatalf("runCmd: %v (stderr=%s)", err, stderr)
	}
	// pwd prints the dir verbatim; runCmd trims the trailing \n.
	if got := strings.TrimSpace(stdout); got != real {
		t.Errorf("child CWD = %q, want %q (runCmd did not bind cmd.Dir)", got, real)
	}
}

// TestRunCmd_EmptyDirInherits: when dir is empty, the child runs
// in the parent process's CWD. This preserves the historical
// "no workspace yet" path used by callers that haven't determined
// their worktree.
//
// We avoid an exact-string comparison against os.Getwd() because
// /bin/pwd on some filesystems (macOS /var ↔ /private/var, NFS,
// OverlayFS) reports a different symlink-resolved path than Go's
// os.Getwd does — a brittle test on otherwise-correct code.
// Instead we assert the child is NOT in a known-bad directory
// (i.e. it inherited something sensible, not the test binary's
// arbitrary temp location).
//
// Windows: see TestRunCmd_DirBinding's comment for the full MSYS
// path-translation rationale.
func TestRunCmd_EmptyDirInherits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("MSYS getcwd() reports translated paths; see TestRunCmd_DirBinding")
	}
	parent, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runCmd(context.Background(), "", "pwd")
	if err != nil {
		t.Fatalf("runCmd: %v", err)
	}
	got := strings.TrimSpace(stdout)
	if got == "" {
		t.Fatal("child pwd output empty (runCmd broke inheritance?)")
	}
	if !strings.HasPrefix(got, "/") {
		t.Errorf("child CWD = %q, want an absolute path (inherited parent)", got)
	}
	// Sanity: the child should still see roughly the same tree the
	// parent was in. Compare only the last 2 path segments — that
	// stays stable across the symlink-resolution differences
	// mentioned above (the test's package directory is always
	// .../internal/command/gtw regardless of how /var resolves).
	parentTail := tailSegments(parent, 2)
	gotTail := tailSegments(got, 2)
	if parentTail != gotTail {
		t.Errorf("child CWD tail = %q, want %q (parent %q vs child %q — inheritance broken)",
			gotTail, parentTail, parent, got)
	}
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

// tailSegments returns the last n "/" separated segments of p.
// Used by EmptyDirInherits to compare CWDs across symlink
// resolutions without depending on the resolved root.
func tailSegments(p string, n int) string {
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	if len(parts) <= n {
		return strings.TrimRight(p, "/")
	}
	return strings.Join(parts[len(parts)-n:], "/")
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
// Windows: see TestRunCmd_DirBinding's comment — MSYS getcwd()
// reports translated paths; this equality assertion cannot hold
// even when nightme correctly sets cmd.Dir.
func TestExecCLIRunner_DirPropagates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("MSYS getcwd() reports translated paths; see TestRunCmd_DirBinding")
	}
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	r := ExecCLIRunner{Dir: real}
	stdout, _, err := r.Run(context.Background(), "pwd")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(stdout); got != real {
		t.Errorf("CLIRunner child CWD = %q, want %q (Dir field not honored)", got, real)
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
// Windows: see TestRunCmd_DirBinding's comment.
func TestGitHubProvider_RunnerBindsWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("MSYS getcwd() reports translated paths; see TestRunCmd_DirBinding")
	}
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	p := &GitHubProvider{Worktree: real}
	// Pull the production runner (Runner is nil by default → falls
	// through to ExecCLIRunner{Dir: p.Worktree}).
	r := p.runner()

	stdout, _, err := r.Run(context.Background(), "pwd")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(stdout); got != real {
		t.Errorf("gh-equivalent child CWD = %q, want %q (Worktree → Dir binding broken)", got, real)
	}
}

// TestGitLabProvider_RunnerBindsWorktree: same contract for the
// GitLab code path (glab). Two tests rather than one to make the
// regression message unambiguous — a failure on GitHub but not
// GitLab (or vice versa) points right at the missing binding.
func TestGitLabProvider_RunnerBindsWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("MSYS getcwd() reports translated paths; see TestRunCmd_DirBinding")
	}
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	p := &GitLabProvider{Worktree: real}
	r := p.runner()

	stdout, _, err := r.Run(context.Background(), "pwd")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(stdout); got != real {
		t.Errorf("glab-equivalent child CWD = %q, want %q (Worktree → Dir binding broken)", got, real)
	}
}