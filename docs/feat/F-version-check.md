# F-VERSION-CHECK: REPL startup version-check + `nightme update`

> **Status**: shipped

> **Scope**: version-check is wired into `runREPL` (bare `nightme`
> invocation). Subcommands and the daemon child do NOT trigger
> the prompt. The companion `nightme update` subcommand is the
> always-live path for power users.

---

## 1. Description

Two entry points give operators a way to know they are on
an out-of-date `nightme`:

1. **REPL startup prompt.** When the user runs bare `nightme`,
   the REPL does one throttled lookup via
   `internal/updater.LookupLatestTag` (nightme.dev release
   feed first, GitHub `/releases/latest` fallback).
   If a newer release is available it prints a single `y/N`
   prompt; any answer other than `y` (including EOF / Ctrl-C /
   read error) ends the prompt immediately and falls through
   to the interactive loop. The prompt runs at most once per
   startup.
2. **`nightme update` subcommand.** One verb that walks the
   whole self-update flow: check → download → verify → install →
   restart the daemon. It runs the stages unconditionally and
   prints what each of them did.

Both paths share `internal/version.Checker`, so the cache
file format, error policy, and semver comparison are defined
in exactly one place.

---

## 2. Why this design

The design constraints, in priority order, are spelled out in
the package comments of `internal/version/check.go` and
`cmd/nightme/repl_update_prompt.go`. Short version:

1. **Never block the REPL on a slow nightme.dev.** Every network
   path is wrapped in a soft-failure envelope: HTTP 403/429/5xx,
   JSON decode error, and disk write error are all swallowed
   and logged, never returned to the caller. The caller (the
   REPL prompt) treats all of these as "we don't know" and
   skips the prompt silently.
2. **Throttle.** The cache file lives at
   `<DataDir>/version-check.json` with a 24h TTL. Repeated
   `nightme` invocations in the dev workflow therefore hit
   the version API at most once per day. (Note: the endpoint
   itself serves `cache-control: max-age=60` from its CDN;
   that controls the CDN's caching, not ours. Our 24h is a
   deliberate over-estimate so dev iteration does not hammer
   the API.)
3. **Single y/N read.** A stray `?` or empty line ends the
   prompt immediately. Looping forever would be worse than
   asking once.
4. **Subcommands don't trigger the prompt.** `nightme status`
   etc. would feel noisy if every invocation paused for a
   network round-trip. Only the bare-`nightme` REPL path
   prompts.
5. **`nightme update` shares the cache.** The CLI builds the
   same `version.Checker` over the same `<DataDir>/version-check.json`,
   so a run right after a REPL start does not hit the release
   feed a second time.

---

## 3. Surface

### 3.1 `internal/version` additions

```go
type LatestTagLookup func(ctx context.Context) (tag, source string, err error)

type Checker struct {
    Lookup      LatestTagLookup  // required: nil → Check returns empty
    HTTPTimeout time.Duration    // 0 → 5s
    CacheTTL    time.Duration    // 0 → 24h
    CachePath   string           // "" disables caching
    Now         func() time.Time // nil → time.Now
}

type CheckResult struct {
    Current   string
    Latest    string
    Outdated  bool
    Source    string
    FromCache bool
    CheckedAt time.Time
}

func NewChecker(dataDir string, lookup LatestTagLookup) (*Checker, string)
func (c *Checker) Check(ctx context.Context, currentVersion string, logf func(string, ...any)) CheckResult
```

The compare path uses `golang.org/x/mod/semver`. Unparseable
inputs (e.g. `dev` builds) fall back to a string compare so
the prompt still has something to say. The Lookup seam
isolates `internal/version` from any specific HTTP endpoint;
production wires `updater.LookupLatestTag`, tests wire a stub.

### 3.2 REPL wiring

`runREPL` picks one of three paths per host:

| Path                | When                                         | Version check |
|---------------------|----------------------------------------------|----------------|
| `runREPLInteractive` | tty + VT (Windows Terminal / ConPTY / POSIX) | yes, before `readline.NewShell()` |
| `runREPLScanner`    | tty but no VT (classic cmd.exe)              | yes, before the first `nightme> ` |
| `runREPLWith`       | non-tty (piped / redirected stdin)           | no — a piped first line must not be eaten as a y/N answer |

The two "yes" paths share `runStartupUpdateCheck(out, logger, reader)`,
which runs `checkWithCountdown` (5s, writes a countdown line to
`out`) then `promptForUpdateIfOutdated` (reads one cooked-stdin
line for the y/N answer). It must run on a COOKED terminal —
before `readline.NewShell()` on the readline path, and stdin is
always cooked on the scanner path.

```go
// shared helper — called by runREPLInteractive and runREPLScanner:
runStartupUpdateCheck(os.Stdout, logger, newStdinLineReader())

// runREPLWith (test / non-tty) deliberately skips it, so unit-test
// transcripts and `echo "exit" | nightme` invocations stay clean.
```

`readlineUsable()` (see `repl_console_windows.go` /
`repl_console_unix.go`) gates the readline path: it returns true
only when stdin is a tty AND stdout's console already has
`ENABLE_VIRTUAL_TERMINAL_PROCESSING`. We never call
`SetConsoleMode` to force the bit on — enabling VT on a classic
cmd.exe hangs `reeflective/readline` at startup (the "Plan C"
regression, commit 6d29c03), and with VT off readline floods
literal CSI (`nightme> [1 q[?25l[120D...`). So cmd.exe falls back
to `runREPLScanner`, which keeps the update flow but loses inline
editing / ↑/↓ history on that one host. Restoring those needs a
Win32-native editor (ReadConsoleInputW +
SetConsoleCursorPosition, no ANSI), tracked as follow-up work.

### 3.3 `nightme update`

```
nightme update [--quiet] [--no-install] [--no-restart] [--yes]
```

| Flag | Effect |
|---|---|
| `--quiet` / `-q` | Suppress the download progress bar (SHA256 is still verified). |
| `--no-install` | Stop after the download; do not swap the binary. |
| `--no-restart` | Skip the daemon restart after the install. |
| `--yes` / `-y` | Skip the y/N confirmations (CI / scripted runs). |
| `--repl` | Skip the post-install exec: the process stays alive and prints a hint to re-enter `nightme`. |

The command walks three stages:

1. **check** — `version.Checker` resolves the latest tag. Up to
   date → it prints the current version and exits.
2. **download** — `updater.DownloadTag` fetches
   `SHA256SUMS.txt` and the release asset (GitHub first,
   nightme.dev mirror as fallback), verifies the asset's SHA256
   against the sums file and extracts the binary into
   `<DataDir>/updates/<ver>/`.
3. **install** — `updater.Install` renames the running binary
   aside (`<target>.old`), copies the new one into place, and the
   daemon is restarted. The command then re-execs the new binary
   with the user's original argv. The REPL startup prompt does the
   same: it runs before readline takes the TTY, so the exec cannot
   lose the shell's state. `--repl` suppresses the exec and prints
   a hint instead.

Output:

```
  ▲  Update available
     0.1.0 → 0.2.0
  ✓  Staged nightme_0.2.0_linux_amd64.tar.gz  sha256=1f0c…
  ✓  installed /usr/local/bin/nightme
     backup /usr/local/bin/nightme.old
  →  restarting daemon…
  ✓  daemon restarted

  ✓  Installed 0.2.0 — restarting into the new binary.
```

When the build is already current the command prints:

```
  ✓  Already up to date
     0.2.0
```

When the version API is unreachable the "no version info
available" line goes to stderr and the command fails; the
install hint of the REPL prompt is a different surface and is
still printed there — a user running the startup prompt
mid-debug should not be blocked by an offline environment.

---

## 4. Failure modes (must hold)

| Condition                      | Behavior                                  |
| ------------------------------ | ----------------------------------------- |
| API 200, version older        | Prompt + y/N, full flow                   |
| API 200, version equal/newer   | Silent (no prompt)                        |
| API 200, malformed JSON        | Silent (decode error logged)              |
| API 200, no usable field       | Silent (empty `tag_name` treated as an error) |
| API 403 / 429 (rate-limit)     | Silent (rate-limit error logged)          |
| API 5xx                        | Silent (status error logged)              |
| Network unreachable            | Silent (transport error logged)           |
| Cache hit, fresh (<24h)        | Silent (no network call)                  |
| Cache hit, stale, no network   | Use stale cache; prompt if it says outdated|
| Reader returns "" (EOF)        | Silent + "Run \`nightme update\` whenever you're ready." |
| Reader returns read error      | Silent + "(read error: …)" line           |
| User types "?" / empty / "n" at "Update now?" | Silent (no install hint printed) |
| User types "y" / "yes" at "Update now?" | Download starts without a second prompt |
| User types "n" at "Install now?" | The staged archive is kept; "Run \`nightme update\` later to install." is printed |
| User types "y" / "yes" at "Install now?" | Install + daemon restart + re-exec |
| Non-tty path (`runREPLWith`)  | Silent (version check skipped)             |

The release feed is probed twice — nightme.dev first, GitHub as
the fallback. A failure of the first source is not fatal as long
as the second answers.

---

## 5. Cache file

`<DataDir>/version-check.json` (the default data dir is
`~/.nightme`):

```json
{
  "latest": "v0.2.0",
  "source": "nightme.dev",
  "checked_at": "2026-08-17T00:12:34Z"
}
```

The file is rewritten on every successful live fetch. A
corrupt file is treated as missing; the next write
overwrites it. No file lock — concurrent REPL startups on
the same machine may race to write, but the worst case is
"one write wins, the older one is dropped", which is fine
for a cache.

---

## 6. Follow-up work

- **Signature verification**: the asset is verified against the
  release's `SHA256SUMS.txt`, and a missing entry for our asset
  is a hard error. A minisign signature check against a pinned
  key is not implemented.
- **Background polling**: there is no polling goroutine in the
  runtime. The system tray checks the version in the background
  when its menu is built and shows the "Download Update" row
  only when a newer release exists; it shares the same 24h cache
  as the REPL prompt.
- **The daemon's own binary**: `nightme update` replaces the
  binary of the process that runs it and restarts the daemon. A
  daemon that was started from a different binary than the one
  the user upgrades is out of scope.

---

## 7. Test coverage

- `internal/version/check_test.go` (15 cases) — semver,
  cache hit/miss, network failure, rate limit, fetch decode,
  `NewChecker` wiring.
- `cmd/nightme/repl_update_test.go` (17 cases) — every
  row of the failure-mode table above plus the
  "no re-prompt on garbage input" guard.
- Existing `TestREPL_*` suite (9 cases) — unchanged; `runREPLWith`
  (the test / non-tty path) skips the version check entirely, so the
  banner transcript stays free of countdown + y-N chatter.
