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

### 1. `internal/command/cwd::path_windows.go::resolvePath` — Go 1.26 regression

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

### 2. `internal/command/cwd::TestFactory_Handle_FullWidthPath_Normalised`

Test: `internal/command/cwd/commands_test.go:120`:

```
Reply missing set-confirmation:
  "Path does not exist: C:\\Users\\runneradmin\\tmp
   (resolved from \"／tmp\")"
```

Pre-existing test that exercises full-width path normalization
(`／tmp`). The user-facing message uses a Windows-resolved path but
the test expectation was written assuming POSIX path output. Needs
either an expected-string update (similar to the `usage_footer` fix
already in fix-stop) or the resolver needs to be cross-platform
aware. Out of scope for fix-stop.

### 3. MSYS path translation in `internal/command/gtw::exec_test.go`

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

### 4. `internal/command/gtw::hooks_test.go::TestFormatResults_ShowsFailure`

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
