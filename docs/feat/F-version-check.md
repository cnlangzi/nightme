# F-VERSION-CHECK: REPL startup version-check + `nightme update`

> **Status**: shipped (download/replace is still a stub)

> **Scope**: version-check is wired into `runREPL` (bare `nightme`
> invocation). Subcommands and the daemon child do NOT trigger
> the prompt. The companion `nightme update` subcommand is the
> always-live path for power users.

---

## 1. Description

Two new entry points give operators a way to know they are on
an out-of-date `nightme`:

1. **REPL startup prompt.** When the user runs bare `nightme`,
   the REPL does one throttled lookup against
   `https://nightme.dev/api/version`. If a newer release is
   available it prints a single `y/N` prompt; any answer other
   than `y` (including EOF / Ctrl-C / read error) ends the
   prompt immediately and falls through to the interactive
   loop. The prompt runs at most once per startup.
2. **`nightme update` subcommand.** An always-live check with
   no caching. Same comparison logic; prints current vs
   latest plus manual install instructions. Replaces
   nothing in this round — it is the placeholder for the
   future download/replace flow.

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
5. **`nightme update` is cache-bypass.** Explicit "update me
   now" should always be a live check.

---

## 3. Surface

### 3.1 `internal/version` additions

```go
type Checker struct {
    VersionURL string         // nightme.dev endpoint; "" → default
    HTTPClient *http.Client   // nil → &http.Client{Timeout: 5s}
    Now        func() time.Time
    CachePath  string         // "" disables caching
}

type CheckResult struct {
    Current   string
    Latest    string
    Outdated  bool
    FromCache bool
    CheckedAt time.Time
}

func DefaultChecker(dataDir string) (*Checker, string)
func (c *Checker) Check(ctx context.Context, currentVersion string, logf func(string, ...any)) CheckResult
```

The compare path uses `golang.org/x/mod/semver`. Unparseable
inputs (e.g. `dev` builds) fall back to a string compare so
the prompt still has something to say.

### 3.2 REPL wiring

```go
// in runREPLInteractive (after banner):
_ = promptForUpdateIfOutdated(context.Background(), &PromptDeps{
    Out:    rl.Stdout(),
    Reader: rl.Readline,
    Logger: logger,
})

// in runREPLWith (test-driven scanner path):
_ = promptForUpdateIfOutdated(context.Background(), &PromptDeps{
    Out:    out,
    Logger: logger,           // Reader deliberately nil → silent
})
```

### 3.3 New subcommand

```
nightme update [--yes]
```

Prints:

```
Current version: 0.1.0
Latest release:  0.2.0
Status: a newer release is available.

Automatic self-update is not implemented yet. To upgrade:
  go install github.com/cnlangzi/nightme/cmd/nightme@latest
Or download a binary release from:
  https://github.com/cnlangzi/nightme/releases/latest
```

When `Latest` is empty (version API unreachable), the
"Latest release" line reads `(could not reach the version
API)` and the warning goes to stderr. The install hint
still prints — a user running `nightme update` mid-debug
should not be blocked by an offline environment.

---

## 4. Failure modes (must hold)

| Condition                      | Behavior                                  |
| ------------------------------ | ----------------------------------------- |
| API 200, version older        | Prompt + y/N, full flow                   |
| API 200, version equal/newer   | Silent (no prompt)                        |
| API 200, malformed JSON        | Silent (decode error logged)              |
| API 200, no usable field       | Silent (no version key error logged)      |
| API 403 / 429 (rate-limit)     | Silent (rate-limit error logged)          |
| API 5xx                        | Silent (status error logged)              |
| Network unreachable            | Silent (transport error logged)           |
| Cache hit, fresh (<24h)        | Silent (no network call)                  |
| Cache hit, stale, no network   | Use stale cache; prompt if it says outdated|
| Reader returns "" (EOF)        | Silent + "Run \`nightme update\`…" hint   |
| Reader returns read error      | Silent + "read error: …" line             |
| User types "?" / empty / "n"   | Silent (no install hint printed)          |
| User types "y" / "yes"         | Echo "y", print install instructions      |
| Nil Reader (runREPLWith only)  | Silent (test-only branch)                 |

---

## 5. Cache file

`~/.nightme/version-check.json`:

```json
{
  "latest_version": "0.2.0",
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

## 6. Follow-up work (not in this round)

The actual download / verify / replace path for
`nightme update` is the next commit. Open questions:

- Asset selection: per-OS / per-arch matching against the
  GitHub release assets (the install instructions still
  point at github.com/cnlangzi/nightme/releases/latest as
  the source of truth), or fall back to `go install`.
- Verification: SHA256 of the asset, optional minisign
  signature check against a pinned key.
- Replace: signal the running daemon to exit, swap the
  binary on disk, exec the new one. The daemon child
  pattern (`_daemon` subcommand) means we do NOT have to
  coordinate a graceful shutdown of a live process — the
  update can simply exit and let the user restart. But
  that's a UX call worth a follow-up doc.
- Background polling: a goroutine in `runtime.Runner` that
  re-checks every N hours and writes to a status struct the
  REPL can surface. Out of scope until the manual path
  proves itself.

---

## 7. Test coverage

- `internal/version/check_test.go` (8 cases) — semver,
  cache hit/miss, network failure, rate limit, fetch decode,
  `DefaultChecker` wiring.
- `cmd/nightme/repl_update_test.go` (10 cases) — every
  row of the failure-mode table above plus the
  "no re-prompt on garbage input" guard.
- Existing `TestREPL_*` suite (9 cases) — unchanged; the
  `runREPLWith` path's nil-Reader branch keeps the banner
  transcript free of prompt chatter.