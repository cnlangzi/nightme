# nightme

> **Sleep tight, code all night.**
>
> A single-process daemon that turns your AI coding CLI into a remote-controlled teammate you can drive from chat.

![Status](https://img.shields.io/badge/status-development-blue)
![Go](https://img.shields.io/badge/go-1.22%2B-00ADD8)
![License](https://img.shields.io/badge/license-MIT-green)
![Platforms](https://img.shields.io/badge/platforms-macOS%20%7C%20Linux%20%7C%20Windows-lightgrey)
![Single Binary](https://img.shields.io/badge/distribution-single%20binary-success)

nightme sits between **AI coding CLIs** (Claude Code / Codex / OpenCode / Pi)
and **IM channels** (Feishu / WhatsApp / Web UI), so you can drop
*"refactor the auth module, push a PR, then sleep"* into a chat at
night and collect the diff in the morning — without ever opening
your laptop.

```
           you (phone, IM, web)                     your laptop
 ┌──────────────────────────┐                  ┌──────────────────────────┐
 │  Feishu DM / WhatsApp /  │ ── text/voice ─► │  nightme daemon (one Go  │
 │  Web UI                  │ ◄─ cards/files ─ │  process, <100MB RAM)    │
 └──────────────────────────┘                  │                          │
                                              │  ┌────────────────────┐  │
                                              │  │  AgentSession pool │  │
                                              │  │  (claude, codex,   │  │
                                              │  │   opencode, pi)    │  │
                                              │  └────────────────────┘  │
                                              └──────────────────────────┘
```

---

## Why nightme?

Most "AI agent in chat" tools fall into one of two camps:

1. **Heavy agent runtimes** — they own the LLM, the prompt template, the
   tool registry, the memory store, the multi-agent orchestrator. You're
   using *their* agent, just routed through chat.
2. **Thin one-CLI bridges** — they wrap one vendor's CLI (usually
   Claude Code) with one IM protocol. Useful, but switching CLIs means
   re-installing.

nightme takes the boring middle path and runs with it: **a transparent
byte-pipe daemon that drives the existing CLI you already paid for,
keeps its conversation context across switches, and lets chat control
your whole dev workflow — not just one prompt.**

| Principle | What it means in practice |
|---|---|
| **Transparent byte pipe** | nightme never rewrites prompts, never filters output, never invents a "memory". What you see in chat is exactly what the CLI saw on its TTY. |
| **PTY-backed** | Every agent runs in a real pseudo-terminal. Progress bars, colors, interactive prompts, ANSI escapes — they all just work. |
| **Pool, don't restart** | Switch between agents (`/use codex`) or workspaces (`/cwd ~/code/b`) and the old CLI process is *kept alive*. Switching back reuses the same conversation. |
| **Local-first, single-binary** | One Go binary on your laptop. No cloud relay, no SaaS, no telemetry. State lives in `~/.nightme/` with `0600` perms and atomic writes. |
| **Per-chat persistence** | Every IM chat → 1 ChatSession → N AgentSessions. Close the laptop, reopen a week later, the same DM resumes the same process. |
| **Channel is a dumb renderer** | Channels (Feishu/WhatsApp/Web) only do protocol encoding and visual rendering. All routing / state machines live in the gateway. Adding a new channel never touches session logic. |

---

## Highlights

### 🤖 Multiple agents, one pool

A single ChatSession owns a **pool of AgentSessions** — one CLI process
per `(agent, cwd)` pair. While you're chatting with `claude` on
`~/code/bailing`, you can `/use codex` to spawn `codex` in the same
workspace, or `/cwd ~/code/side` to bind a second agent in a second
workspace. All processes stay alive. `/gtw` workflows can spawn a
**one-shot** `claude` (via `Starter.RunOnce`) while the long-lived
chat agent keeps running — the one-shot talks, the chat agent
stays quiet, and the bus surfaces whichever one has something to say.

### ⚡ Switch any time, restart nothing

```
/use claude       ─►  spawn (or reuse) AgentSession{agent=claude, cwd=…}
/use codex        ─►  spawn (or reuse) AgentSession{agent=codex, cwd=…}
/use claude       ─►  hit the pool → reuse the SAME process + conversation
/cwd ~/code/b     ─►  bind workspace; old AgentSession stays in the pool
/cwd ~/code/a     ─►  switch back; the previous AgentSession is reattached
```

Pool lookup is `O(1)` by `(agent, cwd)`. The old CLI never dies — it
just stops being routed to. When you `/kill`, every AgentSession in
the chat is torn down gracefully (`bridge.Close`, 5s outer timeout).

### 📂 Survive restart, resume seamlessly

- **ChatSession** state (`chat_sessions.json`, `0600`, atomic rename)
  — selectedAgent, selectedCwd, primaryAgent, watch/think/tools mode.
- **AgentSession** state (`agent_sessions.json`, `0600`, atomic rename)
  — per-AS ID, PID, status, args, lastRunAt.
- On daemon restart, every ChatSession is reattached; every
  AgentSession in `StatusDetached` is auto-respawned with its saved
  args (`--resume <session-id>` for Claude / Pi, equivalent for
  others). Conversation history survives.
- `nightme list` shows the persisted state so you can confirm before
  opening your laptop.

### ⌨️ Slash commands that actually do work

Every action lives behind one slash command. The full catalog:

| Command | What it does |
|---|---|
| `/cwd <path>` | Bind this chat to a workspace. Validates the path; lazy-spawns on the next message. |
| `/use <agent>` | Switch the active agent (`claude` / `codex` / `opencode` / `pi`). Reuses or spawns. |
| `/kill` | Graceful shutdown of all AgentSessions in this chat. |
| `/new [agent]` | Reset the agent's conversation context without restarting the process. |
| `/watch on\|off` | Per-chat message-watch mode (default: only `@bot` or `@_all` in groups). |
| `/think on\|off` | Show / hide the agent's thinking blocks in the receipt card. |
| `/tools on\|off` | Show / hide per-tool thread replies (default off to keep the card quiet). |
| `/gtw fix [-a <agent>]` | Spawn a one-shot agent in a `git worktree`, propose branch name + work. |
| `/gtw push [-a <agent>]` | Commit + push; reply card shows branch / base / url. |
| `/gtw pr  [-a <agent>]` | One-shot agent generates Conventional Commits title+body, opens the PR via `gh` / `glab`. |
| `/gtw close` | Drop the worktree, return to main, delete the branch. |
| `/gtw sync` | Pull `origin/main` into the worktree, fast-forward. |
| `/help` | List every slash command (in-chat). |

All slash commands are dispatched through the `command.Commander` /
`Registry` / `Factory` triple in `internal/command/` — adding a new
command is a single `Factory` registration; nothing in the gateway
or channel layer needs to change.

---

## Features

| Feature | Doc | Summary |
|---|---|---|
| **Multi-agent pool** | [F-29](./docs/feat/F-29-agent-session-pool.md) | `(agent, cwd)` 1:1 AgentSession pool. `/use` and `/cwd` never kill a process. |
| **`/use <agent>`** | [F-28](./docs/feat/F-28-use-command.md) | Lazy switch: reuse existing AgentSession if present, spawn if not. |
| **ChatSession model** | [F-27](./docs/feat/F-27-chatsession.md) | Per-chat persistent context. Survives daemon restart. |
| **`/cwd <path>`** | [F-01](./docs/feat/F-01-session-create.md) | Bind the chat to a workspace. Pool lookup by `(agent, cwd)`. |
| **`/kill`** | [F-43](./docs/feat/F-43-kill-new-graceful-and-reset.md) | Graceful shutdown of all AgentSessions in this chat. |
| **`/new`** | [F-34](./docs/feat/F-34-new-slash-command.md) | Reset the agent's conversation context without restarting the process. |
| **Feishu channel** | [F-08](./docs/feat/F-08-channel-abstraction.md) | WebSocket adapter; receipt cards; thread replies; tool merge; interactive cards. |
| **Interactive cards** | [F-46](./docs/feat/F-46-interactive-cards.md) | `/gtw` decision cards with inline buttons that round-trip via IM reactions. |
| **Rolling-log receipt UX** | [F-25](./docs/feat/F-25-rolling-log.md) | One card per turn, PATCHed in place — no card spam. |
| **Task checklist card** | [F-38](./docs/feat/F-38-task-checklist.md) | Claude's `TaskCreate/TaskUpdate` renders as a Feishu task list in the same receipt. |
| **Usage footer** | [F-45](./docs/feat/F-45-session-footer.md) | Per-turn model + token + cost footer stamped on main-chat messages. |
| **PTY bridge** | [F-19](./docs/feat/F-19-cli-bridge.md) | PTY-backed byte pipe for any CLI. |
| **ACP / SDK / JSON-IO / PTY** | [F-21](./docs/feat/F-21-agent-modes.md) | 4 backend modes — use the most structured one the agent supports. |
| **Pi RPC bridge** | [F-32](./docs/feat/F-32-pi-rpc-bridge.md) | Direct `pi --mode rpc` over stdio JSONL (no ACP layer). |
| **Active WS reconnect** | [F-41](./docs/feat/F-41-active-reconnect.md) | 30s reconnect cadence on Feishu WS drop (vs SDK default 2 min). |
| **`/gtw fix / push / pr / close / sync`** | [`wip/gtw-*.md`](./wip/) | Worktree-isolated git workflow as one-line slash commands. |
| **`nightme config`** | [F-30](./docs/feat/F-30-interactive-config.md) | Interactive menu for picking the primary agent. |
| **`nightme doctor`** | [cmd/doctor.go](./cmd/nightme/doctor.go) | First-stop diagnostic: WS state, last inbound, reconnect count. |
| **`nightme test`** | [F-19](./docs/feat/F-19-cli-bridge.md) | Local smoke test — spawn a CLI in PTY, send bytes, read output. |

---

## Quick start

### Install

Requires Go 1.22+ (1.26+ recommended) and a configured `GOPROXY`.

```bash
go install github.com/cnlangzi/nightme/cmd/nightme@latest
```

Or build from source:

```bash
git clone https://github.com/cnlangzi/nightme.git
cd nightme
make build          # ./bin/nightme
```

### Pick your primary agent

```bash
./bin/nightme config
```

The interactive menu merges built-in agents (claude / codex / opencode / pi)
with anything you've declared in `config.yaml` and lets you set the primary.

### Local smoke test

```bash
./bin/nightme test --workspace /tmp --agent /bin/echo --args hello
```

`test` forwards stdin to the agent in a PTY and prints its stdout back.
Ctrl-C detaches (the child survives); `--cleanup` kills it instead.

### Feishu — the real workflow

```bash
cp configs/nightme.example.yaml ~/.nightme/config.yaml

# One-click registration: run, scan QR, done.
./bin/nightme login feishu

# Start the daemon (detaches into the background by default).
./bin/nightme start

# Tail the log; Ctrl-C exits follow.
./bin/nightme logs --lines 50
```

Open a 1:1 Feishu DM with the bot and try:

```
/cwd /Users/you/code/bailing        # bind this chat to a workspace
/use claude                          # spawn (or reuse) claude in /Users/you/code/bailing
hello, refactor auth.go              # plain text flows to the agent
/use codex                           # switch agent — the claude process is kept in the pool
/use claude                          # switch back — same process, conversation preserved
/cwd /Users/you/code/side-project    # new workspace, new pool entry
/cwd /Users/you/code/bailing         # old workspace still cached
/kill                                # tear down everything in this chat
/help                                # list every slash command
```

### `/gtw` — worktree-isolated git workflow

```
/gtw fix                              # agent proposes worktree name + branch, you approve
/gtw push [-a <agent>]                # commit + push, get a card with branch / base / url
/gtw pr  [-a <agent>]                 # one-shot agent generates Conventional Commits title+body, opens PR
/gtw close                            # drop the worktree, return to main
/gtw sync                             # pull origin/main into the worktree, fast-forward
```

Every step replies with a single, structured card (branch / base / url /
worktree) instead of dumping raw `git` output into chat.

### Daemon lifecycle

```bash
./bin/nightme start          # start daemon in the background
./bin/nightme status         # running? pid? uptime?
./bin/nightme logs -f        # follow the log
./bin/nightme doctor         # WS state + last inbound + reconnect count
./bin/nightme restart        # graceful replace
./bin/nightme stop           # graceful stop (children detached by default)
./bin/nightme list           # list persisted chat sessions
./bin/nightme agents         # list registered agents
./bin/nightme name           # print this instance's name
./bin/nightme name macbook   # set instance name (multi-machine setups)
```

Bare `nightme` (no subcommand) drops you into an interactive REPL with
`↑/↓` history navigation — useful for poking at the daemon over SSH.

---

## Architecture

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

- **Channel** owns transport. v1.3 trimmed to 5 methods; receipt
  lifecycle moved into the adapter (`adapter.go`).
- **Gateway** routes inbound: slash commands dispatched via the
  `command.Commander`, everything else forwarded to the ChatSession's
  active AgentSession.
- **ChatSession** is the per-chat context. Owns the AgentSession pool
  and the InputBuffer FSM. Persists across daemon restarts.
- **AgentSession** is the per-CLI-process handle. One per
  `(agent, cwd)` pair, kept alive across `/use` and `/cwd` switches.
- **Bridge** is the per-agent transport. ModeACP / ModeSDK / ModeJSONIO
  / ModePTY / ModeRPC, picked by what the CLI supports.

See [`docs/SPEC.md`](./docs/SPEC.md) §1 for the full responsibility
table and [`docs/SPEC.md`](./docs/SPEC.md) §0.1 for the v1.3
"Channel is a dumb renderer" rewrite.

---

## Configuration

nightme reads YAML from `~/.nightme/config.yaml` (or `$NIGHTME_CONFIG`
if set). Env-var overrides: `NIGHTME_<SECTION>_<KEY>` (e.g.
`NIGHTME_PRIMARY`).

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

See [`configs/nightme.example.yaml`](./configs/nightme.example.yaml)
for the full schema and per-bridge notes.

Logs go to `~/.nightme/nightme.log` (mode `0600`) as JSON. Attribute
keys containing `secret`, `token`, or `password` are auto-redacted to
`***REDACTED***`.

---

## How nightme compares to the alternatives

nightme targets a specific niche: developers who already use multiple
AI coding CLIs locally and want to drive them from chat. If that's
you, here's how the alternatives stack up.

| Project | Language | Channel scope | Agent scope | Conversation reuse across agent switch | Process keep-alive on `/cwd` switch | Local-first / single binary | Worktree-as-slash-command |
|---|---|---|---|---|---|---|---|
| **nightme** | Go | Feishu (WS + QR onboarding), Web TUI, echo (smoke test) — `Channel` interface ready for more | claude / codex / opencode / pi — pick at runtime via `/use` | ✅ Process kept; context preserved | ✅ Pool-based, no kill | ✅ Single 30 MB static binary | ✅ `/gtw fix / push / pr / close / sync` |
| **openclaw** (Node.js) | TypeScript | 15+ IM (WhatsApp / Telegram / Discord / Slack / Feishu / WeChat / Signal / QQ / …) | Bundles its own Pi Agent runtime + multi-LLM orchestration; not a bridge to *your* CLI | ❌ Tied to its own agent | ❌ Restart on workspace change | ❌ Node + plugin host + bundled LLM stack | ❌ |
| **cc-connect** | Go | 9+ platforms (Feishu / 钉钉 / 企业微信 / 个人微信 / Slack / Telegram / Discord / LINE / QQ) | claude / codex / cursor / gemini / qoder / opencode / iFlow — 7 bundled | ⚠️ Multi-agent orchestration (bots talking in groups), but each agent restart is heavy | ⚠️ Per-project process; not aggressively pooled | ✅ Go binary | ⚠️ Generic `cron` + memory, not worktree-aware |
| **happycoder** | TypeScript (server) + iOS/Android native apps | Mobile-first — WebSocket bridge to native mobile UI; no IM integration | Claude Code only | ❌ Single-agent | ❌ | ⚠️ Server + companion app required | ❌ |
| **hermes** (NousResearch) | Python | None — CLI only | Bundled self-improving agent (`hermes_cli`) | — | — | ✅ Python install script | ❌ |

### What you actually get from nightme that the others don't

- **Genuine multi-agent pool, not multi-agent orchestration.** nightme
  is the only one here that keeps *the actual CLI process* (and its
  in-memory conversation history) alive across `/cwd` and `/use`
  switches. `openclaw` and `cc-connect` happily run multiple agents
  in parallel — but switching back to a previous one pays a respawn
  cost.
- **PTY-first.** `happycoder` ships its own UI to sidestep TTY
  rendering issues; `openclaw` does its own agent runtime. nightme
  just hands the CLI a real PTY and stops pretending it knows better.
- **`/gtw` as a first-class workflow.** `fix → push → pr → close →
  sync`, every step as a one-line IM reply card, with `git worktree`
  isolation so you can keep chatting in main while a PR is open.
  `cc-connect` exposes generic `cron` + memory; `openclaw` has
  nothing like it.
- **Single static Go binary.** `go install` produces a ~30 MB
  binary. No Node + plugin host + LLM stack. No Python venv. No
  companion mobile app to keep updated.
- **Feishu one-click onboarding.** Run `./bin/nightme login feishu`,
  scan the QR, you're done. WebSocket connection — no public IP
  required, no ngrok tunnel.

---

## Documentation

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

---

## Development

```bash
make build     # ./bin/nightme with version metadata
make test      # go test -race ./...   (~20 packages, race-tested)
make lint      # go vet ./...          (0 warnings required by CI)
make install   # go install to $GOBIN
make dev       # go run ./cmd/nightme  (uses example config)
```

CI runs on GitHub Actions (`.github/workflows/ci.yml`) for every push
and pull request: `go vet`, `go test -race`, and `go build` must all
pass.

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
    cwd/ kill/ newcmd/ use/ think/ tools/ watch/ stop/ services/
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

1. **Read the 3-layer doc model** ([`docs/README.md`](./docs/README.md))
   before opening a design PR. PRD → SPEC → FEATURES → feat/F-XX.
2. **Tests must include race coverage.** `make test` runs
   `go test -race ./...`; CI enforces it.
3. **Run `make lint`** before pushing — `go vet` warnings are a CI gate.
4. **Channels are dumb renderers.** If you're tempted to add session
   logic to an adapter, route it through the gateway instead.
5. **Don't merge test files mechanically across renames.** Prefer to
   rewrite them when the underlying API changes.

---

## License

[MIT](./LICENSE) — see [`LICENSE`](./LICENSE) for the full text.
