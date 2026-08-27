package agent

import (
	"context"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
)

// DetectDefaultBranch finds the repo's default branch name
// (main / master / trunk). Returns the BARE branch name (e.g.
// "main"), not a resolvable ref like "origin/main" — callers
// that pass the result to `git diff` / `git merge-base` and
// need a resolvable form should wrap with "origin/" themselves.
//
// Three-tier fallback (mirrors the previous per-bridge copies
// in codex/print.go and claudecode/print.go, plus the
// origin/-prefix variant in agent/review_with_ocr.go):
//
//  1. `git symbolic-ref refs/remotes/origin/HEAD` (most
//     reliable on cloned repos).
//  2. `git remote show origin` (shallow-clone fallback;
//     network round-trip; only on symbolic-ref failure).
//  3. Return "" so the caller falls back gracefully
//     (codex → --uncommitted; claudecode → bare
//     `code-review`; ocr → drops to workspace mode).
//
// Used by:
//
//   - internal/bridge/codex/print.go::runCodexReview to pass
//     `--base <defaultBranch>` to `codex review`.
//   - internal/bridge/claudecode/print.go::runCodeReviewPrintMode
//     to pass `<defaultBranch>...HEAD` as the positional
//     target to `claude -p code-review`.
//   - internal/agent/review_with_ocr.go::precomputeReviewWithOcr
//     to compute the ocr review's merge-base / diff base.
//     The ocr callers wrap with "origin/" since they pass the
//     value to `git diff` and need a resolvable ref.
//
// Returning "" in tier 3 is critical: a hard-coded "main"
// fallback would shadow the caller's else-branch and try
// --base main on master-only / no-remote repos, failing
// instead of gracefully scanning the working tree.
func DetectDefaultBranch(ctx context.Context, workspace string) string {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := proc.New(c, "git",
		"-C", workspace, "symbolic-ref", "refs/remotes/origin/HEAD").Output()
	if err == nil {
		ref := strings.TrimSpace(strings.TrimPrefix(string(out), "ref:"))
		if strings.HasPrefix(ref, "refs/remotes/origin/") {
			return strings.TrimPrefix(ref, "refs/remotes/origin/")
		}
	}
	c2, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	out, err = proc.New(c2, "git",
		"-C", workspace, "remote", "show", "origin").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "HEAD branch:") {
				branch := strings.TrimSpace(strings.TrimPrefix(line, "HEAD branch:"))
				if branch != "" && branch != "(unknown)" {
					return branch
				}
			}
		}
	}
	return ""
}
