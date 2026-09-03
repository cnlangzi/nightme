package agent

import (
	"context"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
)

// CurrentBranch returns the workspace's active local branch name
// (e.g. "fix-review-on-claude"), or "" on detached HEAD / non-git
// dirs / git error.
//
// Mirrors DetectDefaultBranch's graceful-empty contract and
// signatures: same `string` return (no error), same 5s timeout,
// same proc.New direct-execution path. Callers that need a richer
// GitRunner-based test seam (e.g. internal/command/gtw) wrap this
// helper or maintain their own — the duplication is intentional
// because gtw's GitRunner interface exists to support stub-based
// tests without a real git invocation, and claudecode's path is
// naturally end-to-end.
//
// Why `symbolic-ref --short HEAD` instead of
// `rev-parse --abbrev-ref HEAD`: the latter prints the literal
// string "HEAD" on detached HEAD, which would mislead downstream
// consumers into treating "HEAD" as a real branch ref. The
// non-zero exit on detached HEAD maps cleanly to "" here, matching
// the rest of the package's "absent value = empty string" idiom.
//
// Used by internal/bridge/claudecode/print.go::runCodeReviewPrintMode
// to pass the local branch as the positional [target] of
// `claude -p code-review <branch>` — see that file for the
// why-this-anchors-the-review rationale.
func CurrentBranch(ctx context.Context, workspace string) string {
	if workspace == "" {
		return ""
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := proc.New(c, "git",
		"-C", workspace, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
