# NightMe

> **Sleep tight. NightMe codes all night.**
>
> A remote-pair developer agent. No more babysitting AI—stay in the loop from your phone while your digital twin takes the wheel.

[English](./README.md) · [简体中文](./README.zh-CN.md)

![Status](https://img.shields.io/badge/status-development-blue)
![Go](https://img.shields.io/badge/go-1.22%2B-00ADD8)
![License](https://img.shields.io/badge/license-MIT-green)
![Platforms](https://img.shields.io/badge/platforms-macOS%20%7C%20Linux%20%7C%20Windows-lightgrey)
![Single Binary](https://img.shields.io/badge/distribution-single%20binary-success)

## What is NightMe

**NightMe** drives your local AI Coding Agents — Claude Code, Codex, Pi, OpenCode, etc. — from chat. Send a message in any connected chat platform; NightMe routes it to the right agent process and returns the reply as a structured card.

Many chats run in parallel — one per project. Many agents work in parallel, each on its own task — switching between them is instant, no cold restart. `git` worktree work is hardened into `Git Team Workflow` (`/gtw`): fix / push / pr / close / sync — each step is one IM reply card, integrated with GitHub, GitLab, and similar platforms. NightMe doesn't replace your agent subscriptions or memory; it sits in front of them and keeps them warm.

## Why NightMe

### One chat, one CWD, one project

You (a single developer) work across many projects at once. Each Feishu chat (group or DM) is a **ChatSession**, and each ChatSession has a **CWD** — its current working directory. The CWD *is* the project: set it with `/cwd <path>`, change it anytime. Multiple chats run in parallel, each bound to its own directory.

```
                            You (Feishu)
                                  │
                                  ▼

    ┌─ ChatSession ─┐  ┌─ ChatSession ─┐  ┌─ ChatSession ─┐
    │ CWD: ~/a      │  │ CWD: ~/b      │  │ CWD: ~/c      │
    │               │  │               │  │               │
    │ AI Agents:    │  │ AI Agents:    │  │ AI Agents:    │
    │   Claude Code │  │   Claude Code │  │   Claude Code │
    │   Codex       │  │   Codex       │  │   Codex       │
    │   Pi          │  │   Pi          │  │   Pi          │
    │   OpenCode    │  │   OpenCode    │  │   OpenCode    │
    └───────────────┘  └───────────────┘  └───────────────┘

   ▲ CWD = project; agents run inside that CWD; all parallel from one NightMe instance ▲
```

![Feishu chats list — multiple parallel ChatSessions across DMs and groups, each pinned to its own project](docs/images/feishu-multi-chats.png)

**Project isolation is by directory.** Each ChatSession's CWD is independent — re-running `/cwd` changes which directory a session operates on, without affecting the others. Multiple projects stay
live simultaneously.

**The differentiator vs traditional tools** (Hermes, openclaw, cc-connect, happycoder): they activate one session at a time. NightMe runs all your projects in parallel from a single instance. Switching chats is instant — same daemon, no cold start, no re-init.

### Three core capabilities

| Capability | What it means in practice |
|---|---|
| **Multiple Chat Sessions in parallel** | N sessions on one machine, each running a different project or task. |
| **CWD = project** | Each ChatSession is bound to one current working directory — that directory *is* the project. Set it with `/cwd <path>`; switch anytime. |
| **Multi-agent, in the same Chat** | `/use <agent>` swaps the active agent. The previous one stays in the pool with its context. |

## Prerequisites

- **macOS, Linux, or Windows** — NightMe ships as a single static Go binary; no runtime dependencies.
- **A Feishu account** — currently the only supported IM. `nightme login feishu` registers your bot via QR scan.
- **At least one local AI Coding Agent** — Claude Code, Pi, OpenCode, or Codex. Install the CLI and have it on your `$PATH`; NightMe spawns it as a subprocess.

## Install

Two ways to get `nightme` on your machine:

1. **Prebuilt binary** (recommended):

   ```bash
   curl -fsSL https://nightme.dev/install.sh | bash
   ```

2. **From source** (for development or to pin a commit):

   ```bash
   git clone https://github.com/cnlangzi/nightme.git
   cd nightme
   make dev
   ```

`make dev` runs nightme directly from source using the example config — no separate build step needed.

---

## Quickstart

```bash
nightme login feishu   # prints a QR code; scan with the Feishu mobile app
nightme start          # daemon runs in the background
```

When `start` returns, NightMe sends a welcome message to your Feishu DM — that's how you know you're live.

---

## Always-in-the-loop

You always know what your agent is doing, where, and at what cost — every reply carries a fixed footer with what you need to know, without leaving chat. Most other "AI dev in chat" tools feel like a black box; NightMe treats visibility as a first-class feature.

### StatusBar — pinned to every Feishu reply

![Feishu footer card — CWD / git / agent / token, pinned to every reply](docs/images/feishu-statusbar.png)

Every reply carries a fixed footer showing exactly what you need to know without leaving chat:

- **CWD** — which ChatSession is active (the "project")
- **Git status** — branch, dirty / clean, ahead / behind
- **Agent status** — `idle` / `running` / `thinking`
- **Token usage** — used / limit for the current session

Other tools drop you into the dark. NightMe shows you what your agent is doing, where, and at what cost.

### Flexible visibility — you decide what to see

| Toggle | What it controls |
|---|---|
| `/think on\|off` | Show or hide the agent's thinking blocks. |
| `/tools on\|off` | Show or hide per-tool thread replies (default off). |
| `/watch on\|off` | Listen to all group messages, not just `@bot` / `@_all`. |

**Why this matters:** NightMe defaults to visible. Toggle things off when you want quiet — your choice, no surprises.

---

## What we do differently


| Feature | Openclaw / Hermes | NightMe |
|---|---|---|
| Sessions survive daemon restart | ❌ | ✅ |
| Real `/stop` and `/steer` | ❌ | ✅ |
| No server-side timeout | 30 min | none |
| Clean prompts, no preamble | ❌ | ✅ |

The four differentiators, in short:

1. **Sessions survive.** Daemon restart, network blip, sleep — your chat picks up where it left off. The upstream CLI's session resumes via `--resume <session-id>`.

2. **You can stop or redirect, mid-task.** `/stop` halts the in-flight turn. `/steer <msg>` redirects. Both keep your session and context intact.

3. **No clock on you.** If Claude runs 30 minutes, NightMe runs 30 minutes. You're in charge of when to stop.

4. **No prompt padding.** No preamble, no brand voice, no injected system message. The CLI sees just your words.

We sit in front of Claude / Codex / Pi / OpenCode. You stay in control. Nothing in a black box.

---

## Shell mode

You don't always need the agent to run a shell command. With
Claude Code / Codex, asking the agent to run something goes through
the agent's tool loop — long chain, eats context, nudges your real
task aside while the agent's busy reading shell output.

`!cmd` skips all that. Type `!make test` and NightMe runs the
command in the chat's CWD directly. The result comes back as a
plain IM card. No agent, no round trip, no context eaten.

For the scripts you already have — `make`, `npm test`, deploy
hooks. Anywhere the agent's reasoning adds nothing but a delay.

```
✅ $ make test
exit 0 · 12ms · ~/work/foo
stdout:
  All tests passed
```

## Git Team Workflow (`/gtw`)

`git worktree` gives you isolated branches per task. `gh pr create` gives you a one-shot PR. AI agents give you on-demand coding help. `/gtw` glues the three together — each `/gtw <cmd>` is a slash command that spins up a **short-lived agent** for the heavy lifting and returns a clean IM card. The agent runs once, does the work, and exits. Your main chat stays clean.

GitHub / GitLab issues are the task flow — each `/gtw fix` pins to an issue, and the work moves through the issue's state as the subcommands fire.

### The local dev loop: fix → hooks → sync → close

Four subcommands chain into a complete **local multi-branch
development workflow** in your main repo. Run 3 of these in
parallel — three issues, three worktrees, three agents, no state
collision.

1. **`/gtw fix -n <branch>`** — opens a fresh worktree named
   `<branch>` on your local main, runs a one-shot agent to do
   the work. Pure local — no GitHub issue needed. You keep
   chatting in your main chat.

   For the GitHub / GitLab flow, use `/gtw fix <issue-id>` to
   pin the worktree to a remote issue.

2. **hooks fire automatically** — the dev environment rebuilds
   itself in the new worktree. CodeGraph re-indexes, `npm install`
   / `go mod download` / `cargo build` — whatever your project
   needs. Edit `~/.nightme/gtw.yml`:

   ```yaml
   # ~/.nightme/gtw.yml
   fix:
     hooks:
       after:
         - codegraph init                # bare string = shell hook
         - npm install
         - go mod download
   ```

3. (You work. Agent on demand. Or just edit files yourself.)

4. **`/gtw sync`** — when main has moved (other issues landed,
   other worktrees merged), pull `origin/main` into your worktree
   to keep your branch current. Avoids drift when you eventually
   push.

5. **`/gtw close`** — when the task is done (or you decide not
   to), `/gtw close` tears down the worktree, returns you to
   main, and the branch is ready to ship (or discard).

### Hooks — bring the dev environment with you

AI tool indexes (CodeGraph, language servers, caches) usually live
inside the repo. Each worktree is a fresh checkout — they all
need rebuilding. Hooks automate that.

The common case is `fix: hooks: after` — fires right after
`/gtw fix` opens a new worktree, rehydrating the dev env
in-place:

```yaml
# ~/.nightme/gtw.yml
fix:
  hooks:
    after:
      - codegraph init                # re-index the new worktree
      - npm install                   # install deps
      - go mod download               # download Go modules
```

Hook sugar (the YAML accepts both):

- `- codegraph init` — short form, treated as a shell hook
- `- type: shell / run: codegraph init` — long form, same
  semantics (forward-compatible for future `type: agent` /
  `type: notify`)

Each command (not just `fix`) exposes `hooks.before` and
`hooks.after`:

| Hook | When it fires | Typical use |
|---|---|---|
| `before` | before the main flow | record starting SHA, snapshot state |
| `after` | after the main flow (always runs, even on failure) | re-index, install deps, warm caches |

Iron rules (from the code):

- v1 supports **shell hooks only** — anything else warns + skips.
- Hook failures **never block the main flow**. Failed hook = `⚠️`
  note in the reply, main command proceeds.
- All stdout/stderr is echoed back so you can see what actually ran.
- 30s default timeout per hook.

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