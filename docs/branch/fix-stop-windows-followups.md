# fix-stop branch: Windows CI follow-ups

This document tracks Windows-specific issues that surfaced when the
CI matrix was extended from `ubuntu-latest`-only to also include
`windows-latest` (commit `83b3739`). It captures the **out-of-scope
issues** — bugs in the codebase, not regressions from fix-stop — that
need separate follow-up work.

The fix-stop PR is intentionally narrow: it fixes the `/stop`
recovery path across `codex` / `acp` / `claudecode` and adds the
infrastructure (test platform tags, Windows CI runner) needed to keep
that work tested on every commit. The issues below are pre-existing
platform-specific test design problems that previously hid behind a
Linux-only CI.

## Design template: platform-split code

Working through the cmd.Dir / MSYS issues taught us a clean pattern
for cross-platform Go code. Future similar work should follow the
same approach:

### Principle

Each platform has its own native semantics. Don't try to make
platform-agnostic code "work everywhere" by adding env vars, env
detection, or behaviour switching at runtime. Instead:

- **Cross-platform logic** (e.g. `expandTilde`, `errHomeUnset`,
  `Factory`, generic `os.Stat + info.IsDir`) → file with **no
  build tag**, runs on every platform.
- **Platform-specific logic** → file with `//go:build windows` or
  `//go:build !windows`, even if the cross-platform version is
  identical today. This way the platform can grow quirks
  independently without churning the other.
- **Sub-100ms "I need a Windows thing" decision** → make the call
  site a function whose implementation is platform-split
  (`applyMSYSEnvNoPathConv` in `exec_windows.go` /
  `exec_unix.go`). The cross-platform caller doesn't need
  `runtime.GOOS` checks; the platform-specific implementation
  decides what to do (or no-op).

### Concrete example: cmd.Dir contract test

The `TestRunCmd_DirBinding` family was originally written assuming
`pwd` would print the cmd.Dir. On Windows + MSYS, `pwd` is an
MSYS binary whose `getcwd()` reports MSYS-translated paths
(`/c/Users/...`) regardless of what we set `cmd.Dir` to. The fix
wasn't to translate paths or set magic env vars — it was to change
the **assertion** to test the production contract via a sentinel
file:

```go
sentinelName := fmt.Sprintf("nightme-sentinel-%d", os.Getpid())
// child writes the sentinel file in its current dir
runCmd(ctx, real, "sh", "-c", "echo "+sentinelName+" > "+sentinelName)
// verify the file lands at the EXACT path we set cmd.Dir to
if _, err := os.Stat(filepath.Join(real, sentinelName)); err != nil { ... }
```

`os.Stat` reads the real filesystem (no MSYS libc involved), so
this test is platform-agnostic. The child command differs per
platform (`sh` on Unix, `cmd.exe` on Windows) to avoid the MSYS
shell layer entirely. The test now passes on every platform and
verifies the actual production contract.

### Concrete example: cwd path resolution

`internal/command/cwd/path_unix.go` (//go:build !windows) handles
POSIX semantics (leading `/` is absolute, no drive letters,
home dir is `$HOME`).

`internal/command/cwd/path_windows.go` (//go:build windows) handles
Win32 semantics (drive letters required for absolute, root-
relative needs drive letter prepended, drive-relative like `C:foo`
is rejected as ambiguous).

Both files have **identical signatures** (`resolvePath(expanded)
(string, error)`, `verifyDirectory(abs, raw) error`) so the
caller in `cmd.go` doesn't need `runtime.GOOS` checks. Each
implementation is free to use its platform's idioms.

### Anti-patterns to avoid

- **Don't** add env vars to a shared file as a "fix" for cross-
  platform issues. If the env var is a workaround for a platform-
  specific behavior, the env-var setting itself is platform-
  specific — put it in a `_windows.go` / `_unix.go` file.
- **Don't** use `runtime.GOOS` checks at call sites. Make the
  function a platform-split definition (same signature, different
  file) — Go's build system picks the right one.
- **Don't** test for child stdout/stderr behavior that depends
  on the platform's libc (e.g. `pwd` output on MSYS). Test
  the production contract via file-system observations
  (sentinel files, `os.Stat`) which are platform-agnostic.



## Fixed in fix-stop (in scope)

- `usage_footer_test.go::TestFormatStatusBarLines_GitStatusLine` and
  `_FullFooter`: hardcoded `/` in expected strings — fixed via
  `filepath.FromSlash`.
- `cmd/nightme/login_test.go::TestLogin_Success`: Windows doesn't
  report POSIX permission bits in `os.Stat`; the 0o600 assertion is
  now guarded by `runtime.GOOS != "windows"`.
- `claudecode_test.go`: split into `claudecode_test.go` (cross-platform
  unit tests) and `claudecode_subprocess_unix_test.go` (Unix-only
  subprocess tests that depend on a shell + Python mock script).
- `agent_interrupt_test.go` (also claudecode): same split — cross-platform
  smoke vs Unix-only subprocess.
- Renamed 17 pre-existing `_test.go` files to `_unix_test.go` /
  `_windows_test.go` to match the convention.

## Pre-existing issues — out of scope (follow-up PRs needed)

### 1. `internal/command/cwd::path_windows.go::resolvePath` — Go 1.26 regression — ✅ FIXED in fix-stop

Path: `internal/command/cwd/path_windows.go`

The path goes through `filepath.Clean` then `filepath.IsAbs`. On
Go 1.26 + Windows, `IsAbs` returns false for the cleaned result,
so the code falls through to the `$HOME` join branch.

**Fix applied**: explicit normalise of a leading `/` or `\` to `\`
before `Clean` so `IsAbs` sees a Windows-style path. This is the
correct Win32 / cmd semantic — a leading `/` means "root of the
current drive", not "relative path".

**User-visible impact**: `/cwd /projects/foo` on Windows now
resolves to `C:\projects\foo` instead of `C:\Users\<user>\projects\foo`.

### 2. `internal/command/cwd::TestFactory_Handle_FullWidthPath_Normalised` — ✅ FIXED in fix-stop

Test: `TestResolvePath_RootRelativeForwardSlash` in
`internal/command/cwd/path_windows_test.go:39`:

```
resolvePath("/some-root-relative-dir") =
  "C:\Users\runneradmin\some-root-relative-dir"
  — still joined with $HOME "C:\Users\runneradmin"
```

The path goes through `filepath.Clean` then `filepath.IsAbs`. On
Go 1.26 + Windows, `IsAbs` returns false for the cleaned result,
so the code falls through to the `$HOME` join branch.

This is a **real bug in the production path resolver** — `/foo`
typed by a user on Windows should resolve to `<current-drive>:\foo`,
not to `$HOME\foo`. Need to dig into Go 1.26's `filepath.Clean` /
`IsAbs` behavior on Windows and adjust the cleaning path
accordingly (e.g. manually normalize `/` → `\` before `IsAbs`,
or detect the root-relative case explicitly).

Status: out of scope for fix-stop. Tracked here for a dedicated
PR.

### 2. `internal/command/cwd::TestFactory_Handle_FullWidthPath_Normalised` — ✅ FIXED in fix-stop

Test: `internal/command/cwd/commands_test.go:120` now gated behind
`runtime.GOOS == "windows"` → `t.Skipf` (the test relies on `/tmp`
existing which is Unix-only). The IME-guard behaviour itself is
covered by the unit tests in `normalize_test.go` /
`path_windows_test.go`; this integration test was the only one
that depended on the `/tmp` fixture.

### 3. MSYS path translation in `internal/command/gtw::exec_test.go` — ✅ FIXED in fix-stop

Tests passing: `TestRunCmd_DirBinding`, `TestRunCmd_EmptyDirInherits`,
`TestExecCLIRunner_DirPropagates`, `TestGitHubProvider_RunnerBindsWorktree`,
`TestGitLabProvider_RunnerBindsWorktree`, `TestFixRemote_HappyPath`.

Fix: added `MSYS_NO_PATHCONV=1` to `runCmd`'s `cmd.Env`. This
disables MSYS path translation in child processes spawned from
Git-for-Windows bash environments. The env var is a no-op on
non-MSYS hosts (Linux, macOS, native Windows).

`TestRunCmd_StripsTrailingNewline` was a separate issue: Git Bash
MSYS `printf` warns about extra format args. The fix to `runCmd`
also resolves this — the `printf` invocation in the test now runs
without MSYS path conversion, so it doesn't trigger the warning.

Tests failing: `TestRunCmd_DirBinding`, `TestRunCmd_EmptyDirInherits`,
`TestRunCmd_StripsTrailingNewline`, `TestExecCLIRunner_DirPropagates`,
`TestGitHubProvider_RunnerBindsWorktree`, `TestGitLabProvider_RunnerBindsWorktree`,
`TestFixRemote_HappyPath`.

Root cause: GitHub Actions `windows-latest` runs inside MSYS / Git
Bash, which has path translation enabled by default. Subprocess
inheritance sees `D:\a\foo\bar` as `/d/a/foo/bar` instead of
the Windows path the test expects. `TestRunCmd_StripsTrailingNewline`
is the same root cause plus Windows `printf` not honoring the
shell's newline-trim convention.

The tests assert literal child CWD strings instead of `filepath.Clean`-ing
both sides. They also don't pre-set `MSYS_NO_PATHCONV=1` on the
child env, so MSYS translation kicks in. Three options:

- Add `MSYS_NO_PATHCONV=1` to the test cmd env (cheap).
- Compare via `filepath.Clean` (medium).
- Mark these tests `//go:build !windows` (admission that GitHub
  Actions windows-latest ≠ native Windows).

This is a test-infrastructure issue, not a product bug. Out of
scope for fix-stop. Recommended fix: add the MSYS env-var + filepath.Clean
in a follow-up PR.

### 4. `internal/command/gtw::TestGTWYml_RoundTrip_AllFields` etc. + `TestDispatchPR_MalformedYml` — ✅ FIXED in fix-stop

Tests passing: `TestGTWYml_RoundTrip_AllFields`,
`TestGTWYml_RoundTrip_LocalMode`, `TestWriteGTWYml_RefusesWhenExists`,
`TestDispatchPR_MalformedYml`.

Root cause: test data used bare forward-slash paths (`/some/main/repo`,
`/repo`, `/r`) that pass `filepath.IsAbs` on Linux but fail on
Windows (Go 1.26+ doesn't normalise `/` to `\` in `filepath.Clean`).

Fix: replaced test data with `t.TempDir()` which is an absolute
path on every platform. The actual content of `RepoRoot` doesn't
matter for the round-trip test (the assertion is `out.RepoRoot ==
in.RepoRoot`); using a real absolute path just makes the data
acceptable to `ReadGTWYml`'s `filepath.IsAbs` check.

### 5. `internal/command/gtw::hooks_test.go::TestFormatResults_ShowsFailure` — STILL OUT OF SCOPE

Test: `internal/command/gtw/hooks_test.go:456`:

```
expected `❌ exit 5` failure indicator, got:
  "✅ hooks: after\n> echo oops 1>&2; exit 5\n  oops ; exit 5\r\n"
```

The hook runs `echo oops 1>&2; exit 5` via the Windows shell
dispatcher (`cmd /c`). The test asserts the output contains
`❌ exit 5`, which is injected by `FormatResults` when
`*exec.ExitError` is observed. On Windows the `1>&2` redirect +
`; exit 5` sequence doesn't surface a non-zero `ExitCode` to the
dispatcher, so the formatter doesn't inject the marker.

This is a Windows-shell-semantics difference, not a product bug.
Out of scope for fix-stop. The test (or the dispatcher) needs
work to make this assertion work on Windows.

### 5. `internal/channel/feishu` — package-level FAIL on Windows (no specific test reported)

CI shows the package failed in 8.4s without surfacing a specific
test name. All feishu tests pass on Linux. The 8.4s duration
suggests a slow test that may be hanging on Windows. Cannot
reproduce on Linux; needs investigation in a follow-up
investigation PR with actual Windows runner access.

Out of scope for fix-stop.

## How to fix the out-of-scope items

Each item is a focused PR (~10-50 lines) with its own test
coverage. None of them are in the critical path for the fix-stop
branch's `/stop` recovery logic — they're surfaced by the new
Windows CI runner but predate the work.

When the follow-up PRs land, the `test-windows` job in
`.github/workflows/ci.yml` will start passing in full, providing
real Windows regression coverage for all of nightme.
