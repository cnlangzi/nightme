package gtw

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// runCmd is the canonical exec.CommandContext wrapper used by every
// CLI runner in this package (git, gh, glab, hook scripts). It
// exists so the CWD / capture / context contract lives in exactly
// one place — every runner below delegates to it, and any future
// runner (e.g. `tea`, `gh ext`, custom wrappers) should too.
//
// Contract:
//
//   - If dir is non-empty, cmd.Dir is set explicitly. This is the
//     safety property: callers MUST pass a known-valid working
//     directory (a worktree path, a repo root, …) so the spawned
//     process does not inherit a daemon CWD that may have been
//     stale'd since startup (moved/deleted worktree, NFS gone away,
//     …). The previous ExecCLIRunner omitted cmd.Dir entirely,
//     which manifested as `gh pr create` failing with `git: fatal:
//     Unable to read current working directory` whenever the
//     daemon's CWD no longer existed. Centralising the wrap makes
//     it impossible to forget again.
//
//   - If dir is empty, cmd.Dir is left unset and the child inherits
//     the parent CWD. This matches the historical behavior of the
//     CLI runner and hooks so callers that don't yet know their
//     workspace (rare; usually only during early discovery) keep
//     working.
//
//   - stdout / stderr are captured to buffers; trailing newlines
//     are stripped (strings.TrimRight(s, "\n"), i.e. all of them,
//     not just one). Every runner in this package has historically
//     returned output without a trailing newline, and downstream
//     string-matching logic (branch-name comparisons, remote-URL
//     parsing, …) relies on that contract — preserving it here
//     keeps callers from re-Trimming per call site.
//
//   - The provided ctx is honored for cancellation and timeout.
func runCmd(ctx context.Context, dir, name string, args ...string) (stdout, stderr string, err error) {
	if name == "" {
		return "", "", errors.New("gtw: cli: empty command name")
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// MSYS path translation off: when the child is spawned from
	// a Git-for-Windows bash environment (e.g. GitHub Actions
	// windows-latest runner, or a user running nightme from
	// Git Bash), `cmd.Dir = "D:\a\foo"` would be translated to
	// `/d/a/foo` by MSYS before the child sees it, making the
	// child's `pwd` output disagree with what we passed in.
	//
	// We set BOTH the v1 and v2 knobs to cover all MSYS variants
	// in one shot:
	//
	//   - MSYS_NO_PATHCONV=1     — the original MSYS / Git for
	//     Windows v1 switch. Disables path conversion in the
	//     argv the bash layer passes to CreateProcess.
	//   - MSYS2_ARG_CONV_EXCL=*  — the MSYS2 / Git for Windows
	//     v2 switch (added in 2024). Disables the newer
	//     pattern-based converter that v1's MSYS_NO_PATHCONV
	//     didn't fully cover. `*` excludes every arg from
	//     conversion.
	//
	// On non-MSYS hosts (Linux, macOS, native Windows, WSL bash)
	// both env vars are harmless no-ops. Setting them
	// unconditionally is the simplest policy: nightme sets
	// cmd.Dir to an exact Windows path, and we want the child
	// to see that exact path. Users who run nightme from a
	// MSYS-aware shell expect this behavior.
	cmd.Env = append(os.Environ(),
		"MSYS_NO_PATHCONV=1",
		"MSYS2_ARG_CONV_EXCL=*",
	)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	if err := cmd.Run(); err != nil {
		return strings.TrimRight(so.String(), "\n"),
			strings.TrimRight(se.String(), "\n"), err
	}
	return strings.TrimRight(so.String(), "\n"),
		strings.TrimRight(se.String(), "\n"), nil
}