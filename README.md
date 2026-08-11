# NightMe

[English](./README.md) · [简体中文](./README.zh-CN.md)

![Status](https://img.shields.io/badge/status-development-blue)
![Go](https://img.shields.io/badge/go-1.22%2B-00ADD8)
![License](https://img.shields.io/badge/license-MIT-green)
![Platforms](https://img.shields.io/badge/platforms-macOS%20%7C%20Linux%20%7C%20Windows-lightgrey)
![Single Binary](https://img.shields.io/badge/distribution-single%20binary-success)

## What is NightMe

## Quickstart

## Prerequisites

## Install

```bash
curl -fsSL https://nightme.dev/install.sh | bash
```

> One-line installer (placeholder URL — replace once the official site is live).

---

## Why NightMe

### Three real scenarios

**Scenario A — many projects at once**

Open three Chat Sessions on one laptop:

- Chat #1 is refactoring your auth service
- Chat #2 is fixing a bug in the billing repo
- Chat #3 is writing a one-off migration script

One machine, three projects running in parallel. Tools like openclaw let you flip between sessions but lose context every time you switch — NightMe keeps every session alive and preserves each one's conversation.

**Scenario B — many tasks inside one repo**

You keep chatting in `main`. For each side task, `/gtw fix` opens a fresh git worktree, drops you into a new Chat Session bound to that branch, and you work there. No branch juggling, no stash mess, no `git switch` and pray.

**Scenario C — different agents for different jobs**

Refactor with Claude, generate boilerplate with Codex, look up docs with Pi. `/use claude` ↔ `/use codex` ↔ `/use pi` in the same Chat — the old agent process is kept alive in the pool and your conversation context comes back when you switch.

### Three core capabilities

| Capability | What it means in practice |
|---|---|
| **Multiple Chat Sessions in parallel** | N sessions on one machine, each running a different project or task. |
| **Project × workspace, switchable any time** | `/cwd` to bind or switch workspace. Old workspaces are cached, not killed. |
| **Multi-agent, in the same Chat** | `/use <agent>` swaps the active agent. The previous one stays in the pool with its context. |

---

## Always-in-the-loop

Most "AI dev in chat" tools feel like a black box — you type something, scroll a bit, hope. NightMe treats visibility as a first-class feature.

### StatusBar — Feishu Kino footer card

Every message from the agent carries a fixed footer card on Kino showing exactly what you need to know without leaving chat:

- **Working directory** — which project / branch is currently active
- **Git status** — branch, dirty / clean, ahead / behind
- **Agent status** — `idle` / `running` / `thinking`
- **Token usage** — used / limit for the current session

Other tools drop you into the dark. NightMe shows you what your agent is doing, where, and at what cost.

### Flexible visibility — you decide what you see

| Toggle | What it controls |
|---|---|
| `/think on\|off` | Show or hide the agent's thinking blocks in the receipt. |
| `/tools on\|off` | Show or hide per-tool thread replies (default off to keep the card quiet). |
| `/watch on\|off` | Whether this chat listens to group messages (default: only `@bot` / `@_all`). |
| Active progress + explicit confirmation | Critical checkpoints surface to you proactively — you don't have to refresh to see if the agent is alive. |

**Why this matters:** when you're driving multi-agent work from your phone at 11pm, the difference between "I can see what's happening" and "I'm guessing what's happening" is the difference between productive and infuriating. NightMe defaults to visible — and gives you the toggles when you want quiet.

---

## Stable & Predictable

Three pain points of tools like openclaw, addressed head-on:

| Pain | NightMe's answer |
|---|---|
| The agent dies mid-task, conversation gone | **Process-level resumption.** Daemon / network / sleep interruption — restart and every ChatSession is auto-reattached, every AgentSession in `StatusDetached` is respawned with `--resume <session-id>` (Claude / Pi equivalent for others). Conversation survives. |
| Silent timeout cuts you off | **Sessions are user-managed.** No "after N minutes you're disconnected" surprise. You pick the level: `/stop` halts the in-flight turn (keep session), `/steer` redirects with a new direction (keep session), `/new` resets context (keep process), `/close` terminates the bridge process (session preserved, respawn resumes). |
| Project memory vanishes mid-task | **Continues until you say otherwise.** Default is keep compressing, never zero. Only explicit `/new` resets the agent's conversation context. |

The principle: NightMe **doesn't run its own memory system**. Context management is delegated to the upstream Claude Code / Codex / Pi that you're already paying for. So behavior is predictable — what you see is what the CLI sees, not what NightMe invents. When the CLI's own compression kicks in, you know. When NightMe does nothing, you know.

---

## Coding-Tuned, not Coding-Locked

The default workflow is coding. The commands, the `/gtw` flow, the agent menu — all oriented toward developer work.

But NightMe is a **transparent byte-pipe daemon that drives an existing CLI process**. It doesn't rewrite prompts, doesn't ship its own agent runtime, doesn't pretend to know better than the CLI you're using. So:

- Coding workflow? Optimized. `gtw fix / push / pr / close / sync` is the team workflow you actually want.
- Non-coding agent work? Still works. If your CLI can run, NightMe can drive it.
- Want to switch CLIs later? Drop in a new `agents:` entry in config. The orchestration stays.

NightMe doesn't try to be a "general Agent framework". It doesn't compete with LangChain / OpenAI Agents on that axis. The product claim is narrower and stronger: **a developer-workflow orchestrator for the CLIs you already use.**

---

## Reference

### Chat input routing

Every inbound message is dispatched by its first character. The three routes are owned by separate packages — no routing knowledge leaks between them:

| Prefix | Routed to | Package |
|---|---|---|
| `!` / `！` (full-width) | Shell dispatcher — runs the body in the chat's CWD via `sh -c` / `cmd /c` | `internal/shell/` |
| `/` | Command dispatcher — slash command (`/cwd`, `/use`, `/gtw fix`, …) | `internal/command/` |
| anything else | Agent prompt — forwarded to the active AgentSession | (no package — default route) |

### Shell mode (`!cmd`)

A leading `!` (or full-width `！`) runs the body as a real shell command in the **chat's current CWD** — no agent in the loop, no `/cwd` / `/use` context needed beyond a bound workspace. The reply is a C-style summary card:

```
✅ $ ls -la
exit 0 · 12ms · /Users/you/projects/foo
stdout:
  drwxr-xr-x  …
  -rw-r--r--  …
```

```
❌ $ go test ./...
exit 1 · 4321ms · /Users/you/projects/foo
stdout:
  ok  	github.com/foo/bar	0.124s
stderr:
  # github.com/foo/baz
  ./baz.go:42:9: undefined: qux
```

**When to use it**: quick recon (`!ls`, `!git status`, `!tail -n 50 app.log`), environment probes (`!go version`, `!which gh`), or anything where spawning an agent round-trip is overkill. The CWD is whatever you last bound with `/cwd` (or the chat's default), so `!cmd` always runs against the workspace you're already looking at.

**Rules** (locked in by `parseShell` + `internal/shell/dispatch_test.go`):

- **Prefix required.** `!cmd` matches, `echo !hi` does not (the `!` must be the first non-whitespace char).
- **Empty body is a no-op.** A lone `!` or `!   ` falls through to the agent prompt — no accidental empty shells.
- **Both bang forms work.** `!cmd` and `！cmd` are equivalent; mobile / full-width IME users don't have to switch keyboards.
- **No CWD → friendly error.** If the chat has no bound workspace, you get the card below and nothing runs:

  ```
  ❌ shell: no CWD configured for this chat
  Try `/use <path>` first.
  ```
- **5-minute cap.** A `!cmd` that runs longer than 5 minutes is killed. Longer jobs go in your own screen / tmux.
- **Async + best-effort reply.** The command runs in a detached goroutine and the result posts as a thread reply; the gateway returns immediately so a slow `!cmd` never blocks the next message. The reply is best-effort — if the daemon is restarting (`!make restart`), the new daemon re-attaches to the chat and you may see the result there.
- **Panic-safe.** A misbehaving command (or a sender bug) is recovered inside the goroutine — the daemon stays up, you lose one reply, nothing else.

**Cross-platform**: macOS / Linux use `sh -c <cmd>`; Windows uses `cmd /c <cmd>`. Build-tag isolated in `internal/shell/dispatch_unix.go` / `dispatch_windows.go`.

**Output cap**: stdout is inlined up to 50 lines; beyond that the card shows `… N more lines truncated` so a runaway `!cat huge.log` can't blow up the IM message size limit. stderr has no line cap but is always shown after stdout.

### Slash commands

| Command | What it does |
|---|---|
| `/cwd <path>` | Bind this chat to a workspace. Validates the path; lazy-spawns on the next message. |
| `/use <agent>` | Switch the active agent (`claude` / `codex` / `opencode` / `pi`). Reuses or spawns. |
| `/stop` | Halt the in-flight turn on the selected agent. Session stays; queued messages still flow. |
| `/steer <msg>` | Stop the in-flight turn and prepend `<msg>` to the queue. The steered message becomes the first thing the agent sees on the next turn. |
| `/close [agent]` | Terminate the bridge process(es) for AgentSession(s) in the current workspace. AgentSession entry is preserved; next user message triggers a respawn that replays `--resume <sessionID>` to continue the conversation. |
| `/new [agent]` | Reset the agent's conversation context (Claude Code's `/clear` equivalent). Process stays alive; queued messages are cleared. |
| `/watch on\|off` | Per-chat message-watch mode (default: only `@bot` / `@_all` in groups). |
| `/think on\|off` | Show / hide the agent's thinking blocks in the receipt card. |
| `/tools on\|off` | Show or hide per-tool thread replies (default off to keep the card quiet). |
| `/gtw fix [-a <agent>]` | Spawn a one-shot agent in a `git worktree`, propose branch name + work. |
| `/gtw push [-a <agent>]` | Commit + push; reply card shows branch / base / url. |
| `/gtw pr  [-a <agent>]` | One-shot agent generates Conventional Commits title+body, opens the PR via `gh` / `glab`. |
| `/gtw close` | Drop the worktree, return to main, delete the branch. |
| `/gtw sync` | Pull `origin/main` into the worktree, fast-forward. |
| `/help` | List every slash command (in-chat). |

All slash commands dispatch through `command.Commander` / `Registry` / `Factory` (`internal/command/`) — adding one is a single `Factory` registration; nothing in the gateway or channel layer needs to change.

---

## Git Team Workflow (`/gtw`)

Built-in git team workflow. Every step is one slash command, replied with a structured card (branch / base / url / worktree), not raw `git` spam.

```
/gtw fix                              # open worktree + propose branch + start agent
/gtw push                             # commit + push (uses configured agent for dirty-push commit msg)
/gtw pr                               # open PR via gh / glab
/gtw close                            # tear down worktree, return to main
/gtw sync                             # pull origin/main into the worktree, fast-forward
```

### Hooks — bring the dev environment with you

AI tool indexes (CodeGraph, language servers, caches) usually live inside the repo. Opening a worktree means they all need rebuilding — currently you have to run `codegraph init` by hand.

Hooks automate that. Edit `~/.nightme/gtw.yml`:

```yaml
# ~/.nightme/gtw.yml
fix:
  hooks:
    after:
      - codegraph init                # bare string = shell hook
      - npm install
      - go mod download
```

Hook sugar:

- `- codegraph init` — short form, treated as a shell hook
- `- type: shell / run: codegraph init` — long form, same semantics (forward-compatible for future `type: agent` / `type: notify`)

Each command exposes `hooks.before` and `hooks.after`:

| Hook | When it fires | Typical use |
|---|---|---|
| `before` | before the main flow | record starting SHA, snapshot state |
| `after` | after the main flow (always runs, even on failure) | `codegraph init`, install deps, warm caches |

Iron rules (from the code):

- v1 supports **shell hooks only** — anything else warns + skips.
- Hook failures **never block the main flow**. Failed hook = `⚠️` note in the reply, main command proceeds.
- All stdout/stderr is echoed back so you can see what actually ran.
- 30s default timeout per hook.

### Lightweight agent for routine work

Routine work in `/gtw` (commit message generation, etc.) doesn't need a heavy coding agent — they inject chat context and burn tokens you don't need. Push can use a lightweight agent (Pi or similar) via `<cmd>.agent`:

```yaml
# ~/.nightme/gtw.yml
push:
  agent: pi                          # lightweight agent for commit-message generation
```

Agent selection follows a 3-tier priority chain:

| Priority | Source | Example |
|---|---|---|
| 1 | CLI flag | `/gtw push -a claude` |
| 2 | yml `<cmd>.agent` | `push: agent: pi` |
| 3 | chat's current `/use` agent | — |
| fallback | nothing set | existing `❌ no agent selected` reply |

**Scope (per code):** `push.agent` is consumed in `pushDirty` (pushClean runs pure `git push -u origin`, no agent). `fix / close / sync` reserve the `agent` field for future use but currently don't consume it.

**Degradation:** if `yml.agent` references a name that isn't registered (e.g. `pi` missing from your `agents:` list), NightMe warns (`⚠️ gtw.yml agent "pi" not found; falling back to session default`) and falls back to priority 3 — never silently swaps your agent.

**What you actually get:** heavy thinking stays in the main Chat's Claude / Codex. `/gtw` runs Pi on commit messages and shell hooks on init scripts. Main session stays clean; tokens drop.

### `gtw` hooks cheatsheet

```yaml
# ~/.nightme/gtw.yml
fix:
  hooks:
    before: [echo "starting fix flow"]
    after:  [codegraph init, npm install, go mod download]

push:
  agent: pi                          # lightweight agent for commit-message work

# close:  # reserved for future
# sync:   # reserved for future
```

---

## For developers

### Architecture (advanced)

```
┌─────────────┐    ┌─────────────┐    ┌──────────────────────────┐
│  Channel    │ →  │  Gateway    │ →  │  ChatSession (per chat)   │
│  (Feishu,   │ ←  │  (router +  │ ←  │  ├─ AgentSession pool     │
│   Web TUI)  │    │   binding)  │    │  │  (agent, cwd) 1:1       │
└─────────────┘    └─────────────┘    │  ├─ InputBuffer FSM       │
                                     │  ├─ readPump              │
                                     │  └─ EventHandler          │
                                     │           ↓               │
                                     │  AgentSession → Bridge    │
                                     │     (PTY / ACP / SDK /    │
                                     │      JSON-IO / RPC)       │
                                     │           ↓               │
                                     │       Agent CLI           │
                                     └──────────────────────────┘
```

- **Channel** owns transport. v1.3 trimmed to 5 methods; receipt lifecycle moved into the adapter (`adapter.go`).
- **Gateway** routes inbound: slash commands dispatched via the `command.Commander`, everything else forwarded to the ChatSession's active AgentSession.
- **ChatSession** is the per-chat context. Owns the AgentSession pool and the InputBuffer FSM. Persists across daemon restarts.
- **AgentSession** is the per-CLI-process handle. One per `(agent, cwd)` pair, kept alive across `/use` and `/cwd` switches.
- **Bridge** is the per-agent transport. ModeACP / ModeSDK / ModeJSONIO / ModePTY / ModeRPC, picked by what the CLI supports.

See [`docs/SPEC.md`](./docs/SPEC.md) §1 for the full responsibility table and [`docs/SPEC.md`](./docs/SPEC.md) §0.1 for the v1.3 "Channel is a dumb renderer" rewrite.

### Configuration

NightMe reads YAML from `~/.nightme/config.yaml` (or `$NIGHTME_CONFIG` if set). Env-var overrides: `NIGHTME_<SECTION>_<KEY>` (e.g. `NIGHTME_PRIMARY`).

```yaml
primary: claude                          # global default agent

agents:                                  # list; each entry = name/bridge/command
  - name: claude
    bridge: claude
    command: "claude --dangerously-skip-permissions"
  - name: codex
    bridge: codex
    command: codex
  - name: opencode
    bridge: opencode
    command: opencode
  - name: pi
    bridge: pi
    command: "pi"

feishu:
  app_id: "cli_xxxxxxxxxxxxxxxx"
  app_secret: "xxxxxx…xxxx"
  verification_token: ""
  encrypt_key: ""

session:
  default_pty_cols: 80
  default_pty_rows: 24
  output_chunk_size: 4096
  output_flush_interval_ms: 200

logging:
  level: "info"          # debug | info | warn | error
  file: ""               # empty = stdout; path = file

paths:
  data_dir: "~/.nightme"
```

The `/gtw` workflow reads a **separate** file: `~/.nightme/gtw.yml` — see the [Git Team Workflow section](#git-team-workflow-gtw) above.

See [`configs/nightme.example.yaml`](./configs/nightme.example.yaml) for the full schema and per-bridge notes.

Logs go to `~/.nightme/nightme.log` (mode `0600`) as JSON. Attribute keys containing `secret`, `token`, or `password` are auto-redacted to `***REDACTED***`.

### Documentation

| Doc | What |
|---|---|
| [`docs/PRD.md`](./docs/PRD.md) | Product definition — what / why / for whom. No tech. |
| [`docs/SPEC.md`](./docs/SPEC.md) | Technical architecture — components, data flow, NFRs. |
| [`docs/FEATURES.md`](./docs/FEATURES.md) | Feature index — every F-XX in one table. |
| [`docs/feat/`](./docs/feat/) | Per-feature design docs. |
| [`docs/bridge/`](./docs/bridge/) | Per-agent bridge design: claude, codex, opencode, pi. |
| [`docs/channel/feishu.md`](./docs/channel/feishu.md) | Feishu adapter reference (rendering rules, card semantics, thread routing). |
| [`docs/E2E_TESTING.md`](./docs/E2E_TESTING.md) | Manual Feishu round-trip + troubleshooting. |
| [`CHANGELOG.md`](./CHANGELOG.md) | Current snapshot (single `[Unreleased]` section). |
| [`MIGRATION.md`](./MIGRATION.md) | Breaking changes between earlier snapshots. |

### Development

```bash
make build     # ./bin/nightme with version metadata
make test      # go test -race ./...   (~20 packages, race-tested)
make lint      # go vet ./...          (0 warnings required by CI)
make install   # go install to $GOBIN
make dev       # go run ./cmd/nightme  (uses example config)
```

CI runs on GitHub Actions (`.github/workflows/ci.yml`) for every push and pull request: `go vet`, `go test -race`, and `go build` must all pass.

### Project layout

```
cmd/nightme/                       # cobra CLI (start / stop / restart / status / logs / doctor / test / config / list / login / agents / name / debug)
configs/                           # example YAML config
docs/
  PRD.md SPEC.md FEATURES.md       # 3-layer doc model
  feat/                            # F-XX per-feature design
  bridge/  channel/  flow/         # per-subsystem design
internal/
  agent/                           # Agent / AgentEvent / Info / Starter interface
  agentsession/                    # AgentSession + Prompt + Spawner (per-CLI-process runtime unit)
  bridge/                          # Bridge abstraction
    acp/  pty/  sdk/
    claudecode/  codex/  opencode/  pi/
  channel/                         # Channel interface
    echo/  feishu/                 # adapters (Feishu is the production one)
  chatsession/                     # ChatSession + pool manager + persistence
  command/                         # Slash-command Commander / Registry / Factory
    cwd/ close/ newcmd/ use/ think/ tools/ watch/ stop/ steer/ services/
    gtw/                           # /gtw fix / push / pr / close / sync (worktree workflow)
  config/                          # YAML loader + env overrides
  daemoncontrol/                   # IPC for `nightme doctor` / `status`
  errors/                          # CodedError + ExitCode
  gateway/                         # Slash router + binding + receipt FSM
  logging/                         # slog + secret redaction
  registry/                        # JSON-backed chat_sessions.json + agent_sessions.json (0600, atomic)
```

### Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Generic / unmapped error |
| 2 | Config error |
| 3 | Auth error |
| 4 | Channel error |
| 5 | Session error |
| 6 | Agent error |
| 7 | Bridge error |
| 8 | Validation error |
| 9 | Not found |

---

## Contributing

Issues and PRs are welcome. A few things that help:

1. **Read the 3-layer doc model** ([`docs/README.md`](./docs/README.md)) before opening a design PR. PRD → SPEC → FEATURES → feat/F-XX.
2. **Tests must include race coverage.** `make test` runs `go test -race ./...`; CI enforces it.
3. **Run `make lint`** before pushing — `go vet` warnings are a CI gate.
4. **Channels are dumb renderers.** If you're tempted to add session logic to an adapter, route it through the gateway instead.
5. **Don't merge test files mechanically across renames.** Prefer to rewrite them when the underlying API changes.

---

## License

[MIT](./LICENSE) — see [`LICENSE`](./LICENSE) for the full text.