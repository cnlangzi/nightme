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
	// Git Bash), MSYS translates path-like argv entries before
	// they reach CreateProcess. We disable the translation so
	// git/gh/glab see the exact paths nightme set on the
	// command line (e.g. `--git-dir=C:\foo\repo` instead of the
	// MSYS-translated `/c/foo/repo`).
	//
	// Two env vars cover both MSYS generations:
	//
	//   - MSYS_NO_PATHCONV=1     — the MSYS / Git for Windows v1
	//     switch. Disables path conversion in argv passed to
	//     CreateProcess via the bash layer.
	//   - MSYS2_ARG_CONV_EXCL=*  — the MSYS2 / Git for Windows v2
	//     switch (added in 2024). Disables the newer
	//     pattern-based converter that v1's MSYS_NO_PATHCONV
	//     didn't fully cover. `*` excludes every arg from
	//     conversion.
	//
	// On non-MSYS hosts (Linux, macOS, native Windows, WSL bash)
	// both env vars are harmless no-ops. Note: these env vars
	// only affect argv path conversion, NOT MSYS libc's
	// interception of getcwd() — child processes that report
	// their CWD via getcwd() will still see MSYS-translated
	// paths. The tests under runCmd work around this by writing
	// a sentinel file to the cmd.Dir and verifying the file
	// exists at the real (untranslated) path, rather than
	// parsing pwd output.
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