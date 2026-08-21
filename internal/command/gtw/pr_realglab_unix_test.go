//go:build !windows

// pr_realglab_unix_test.go — real-glab end-to-end smoke for
// GitLabProvider.FindOpenPRForBranch / GetPR.
//
// Shells out to the actual `glab` binary on PATH against the
// real www-gitlab-com repository on gitlab.com to verify the
// argv shape produced by GitLabProvider.FindOpenPRForBranch
// (after the 2026-08-21 `--state` fix) is accepted by glab and
// returns the expected JSON shape.
//
// Why a real-glab smoke at all: the hermetic argv-shape pins
// in pr_test.go (runGH stub) can drift from real glab if a
// future Go refactor typos a flag, drops a "--" separator, or
// re-introduces `--state` in a way the stub doesn't catch.
// The real-glab suite catches those the moment someone runs it.
//
// Default: SKIP. Opt in with:
//
//	NIGHTME_REAL_GLAB=1 go test ./internal/command/gtw -run RealGLAB -v -count=1
//
// Tests are gated on:
//  1. testing.Short()     — `go test -short` always skips
//  2. NIGHTME_REAL_GLAB=1  — explicit opt-in (matches the
//                            NIGHTME_REAL_PI convention for pi
//                            e2e tests in pr_realpi_unix_test.go)
//  3. `glab` on PATH      — Skip, not Fail, when the binary is
//                            missing. CI on a clean container
//                            must not break.
//
// Design choice: branch lifecycle. We use a deliberately
// non-existent source branch (e.g. "__nonexistent_branch_for_
// realglab_test_<rand>__") so the test does NOT depend on any
// specific MR IID staying open. glab returns "[]" for a
// non-existent branch, not an error — this is the exact
// production behaviour for "branch has never had an MR opened"
// documented on FindOpenPRForBranch. If www-gitlab-com were
// ever deleted (it won't be, but the test must not assume
// that), the test fails clearly rather than silently passing.
//
// Counterpart to:
//   - internal/command/gtw/pr_realpi_unix_test.go (real pi e2e)
//   - internal/command/gtw/testhelpers_realpi_unix_test.go
//     (requireRealPi — same guard pattern adapted for glab)
package gtw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// requireRealGLAB skips the calling test when:
//   - testing.Short() is set
//   - NIGHTME_REAL_GLAB is not set to a truthy value
//   - the `glab` binary is not on PATH
//
// CI on a clean container must SKIP, not FAIL. The point of
// these tests is to give a human gating a release a real
// signal — gating the build on a developer's local `glab`
// version would be wrong.
func requireRealGLAB(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-glab test in -short mode (use -run RealGLAB to opt in)")
	}
	switch strings.ToLower(os.Getenv("NIGHTME_REAL_GLAB")) {
	case "", "0", "false", "no", "off":
		t.Skip("set NIGHTME_REAL_GLAB=1 to enable real-glab e2e tests")
	}
	if _, err := exec.LookPath("glab"); err != nil {
		t.Skipf("glab binary not on PATH: %v", err)
	}
}

// randomNonce returns a short hex string suitable for tagging
// a non-existent test branch so two concurrent runs of the
// test suite don't collide. 8 bytes (16 hex chars) is plenty
// for "branch that doesn't exist on the remote".
func randomNonce(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// TestRealGLAB_WWWGitLabCom_MRListArgs is the post-fix
// regression test for the 2026-08-21 production failure:
//
//	check existing PR: glab pr/mr list: exit status 1: ERROR
//	Unknown flag: --state.
//	Try --help for usage.
//
// It drives the production GitLabProvider against the real
// `glab` binary on PATH against the real
// https://gitlab.com/gitlab-com/www-gitlab-com.git repo, using
// a random non-existent source branch so the test does not
// depend on any specific MR's lifecycle (opened/closed/merged/
// deleted): glab returns "[]" for a non-existent source
// branch, which is the exact production contract documented
// on FindOpenPRForBranch.
//
// Assertion: the call must return (nil, nil) — no error, no
// PR. If a refactor re-introduces `--state` (which glab 1.36+
// removed from `mr list`), the real glab binary will return
// exit status 1 with "Unknown flag: --state" and the test
// fails with that verbatim stderr — catching the regression
// before it reaches production.
func TestRealGLAB_WWWGitLabCom_MRListArgs(t *testing.T) {
	requireRealGLAB(t)

	nonce := randomNonce(t)
	branch := "__nonexistent_branch_for_realglab_test_" + nonce + "__"

	p := &GitLabProvider{
		Worktree: "",
		Runner:   &ExecCLIRunner{Dir: ""},
	}
	pr, err := p.FindOpenPRForBranch(context.Background(), "gitlab-com", "www-gitlab-com", branch)
	if err != nil {
		t.Fatalf("real glab mr list failed (likely the --state regression is back): %v", err)
	}
	if pr != nil {
		t.Fatalf("expected nil PR for non-existent branch, got %+v", pr)
	}
}

// TestRealGLAB_WWWGitLabCom_GetPR_NoMatch is the GetPR
// mirror of TestRealGLAB_WWWGitLabCom_MRListArgs. Same
// rationale: drive the production GitLabProvider.GetPR end
// to end against the real glab binary, using a non-existent
// source branch so the test does not depend on any MR's
// lifecycle. GetPR has the same fail-soft contract as
// FindOpenPRForBranch: (nil, nil) on empty list.
func TestRealGLAB_WWWGitLabCom_GetPR_NoMatch(t *testing.T) {
	requireRealGLAB(t)

	nonce := randomNonce(t)
	branch := "__nonexistent_branch_for_realglab_test_" + nonce + "__"

	p := &GitLabProvider{
		Worktree: "",
		Runner:   &ExecCLIRunner{Dir: ""},
	}
	pr, err := p.GetPR(context.Background(), "gitlab-com", "www-gitlab-com", branch)
	if err != nil {
		t.Fatalf("real glab mr list (GetPR) failed (likely the --state regression is back): %v", err)
	}
	if pr != nil {
		t.Fatalf("expected nil PR for non-existent branch, got %+v", pr)
	}
}

// TestRealGLAB_MRListHelpContainsNoStateFlag is a low-cost
// complement to the argv-shape tests: it asserts that the
// `glab mr list` help text on the locally installed `glab`
// does NOT mention `--state` as a flag. If a future glab
// release re-adds `--state`, both this test and the live
// argv tests would have to be revisited — but pinning the
// help text gives a clearer failure when the upstream CLI
// changes shape.
func TestRealGLAB_MRListHelpContainsNoStateFlag(t *testing.T) {
	requireRealGLAB(t)

	out, err := exec.Command("glab", "mr", "list", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("glab mr list --help: %v\n%s", err, out)
	}
	// Normalise: glab prints flag descriptions in the form
	// "    --state   description". We check for the exact
	// two-space-prefixed flag entry so we don't false-positive
	// on the docs / examples that mention --state in prose.
	help := string(out)
	for line := range strings.SplitSeq(help, "\n") {
		trimmed := strings.TrimSpace(line)
		// Flag lines start with `-` after trim. Anything else
		// (prose, headers) is ignored.
		if !strings.HasPrefix(trimmed, "-") {
			continue
		}
		// Tokenise the flag tokens. We accept either `--state`
		// / `--state=foo` or the short form (none today, but
		// glab adds those from time to time).
		for tok := range strings.FieldsSeq(trimmed) {
			if tok == "--state" || strings.HasPrefix(tok, "--state=") {
				t.Fatalf("glab mr list --help still advertises --state flag (line %q). The GitLabProvider's --state regression-test assumption is now stale; revisit the fix and the comment in provider.go.", line)
			}
		}
	}
}
