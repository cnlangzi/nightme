# nightme

> Sleep tight, code all night.

![Status](https://img.shields.io/badge/status-development-blue)
![Go](https://img.shields.io/badge/go-1.22%2B-00ADD8)
![License](https://img.shields.io/badge/license-MIT-green)

nightme is a single-process daemon that bridges AI Coding CLIs
(Claude Code / Codex / OpenCode) to IM channels (Feishu /
WhatsApp / Web UI), so you can drop "write X for me" into a chat
at night and collect the result in the morning.

> **Status**: current development version (locked 2026-08-02). One
> snapshot on `main`; there is no versioned release ladder. What
> is committed is what users build and run. See
> [`docs/SPEC.md`](./docs/SPEC.md) for architecture and
> [`MIGRATION.md`](./MIGRATION.md) for breaking changes from earlier
> snapshots.

## Install

Requires Go 1.22+ and `GOPROXY` configured for the proxy you use
(`https://goproxy.cn,direct` works on mainland China).

```bash
go install github.com/cnlangzi/nightme/cmd/nightme@latest
```

Or build from source:

```bash
git clone https://github.com/cnlangzi/nightme.git
cd nightme
go build -o bin/nightme ./cmd/nightme
```

## Quick Start

### 1. Pick your primary agent

```bash
./bin/nightme config
```

Two-level menu:
- `[1] Agents` — merge built-in agents (claude, codex, opencode)
  with your `cfg.Agents` entries, pick one as primary.

This writes `primary: <name>` back to `config.yaml`.

### 2. Local Bridge smoke test

```bash
./bin/nightme test --workspace /tmp --agent /bin/echo --args hello
```

`test` forwards stdin to the agent and writes agent output to
stdout. Send `SIGINT` (Ctrl+C) to **detach** (the child CLI
survives), or pass `--cleanup` to **kill** it instead:

```bash
./bin/nightme test --cleanup --workspace /tmp --agent /bin/echo --args hello
```

### 3. Feishu channel (the real workflow)

```bash
# (Optional) Copy and edit the example config
cp configs/nightme.example.yaml ~/.nightme/config.yaml

# One-click Feishu registration (scan the QR code)
./bin/nightme auth login feishu

# Start the daemon
./bin/nightme run
```

In a 1:1 Feishu chat with the bot:

```text
/cwd /tmp             # bind this chat to a workspace
/use claude           # pick primary agent (lazy spawn; first run)
/hello                # plain text flows to the agent
/use codex            # switch agent; pool preserved (no restart)
/kill                 # clear pool; next message respawns
/help                 # list every nightme command
```

Key behaviour (current dev):
- **/cwd** only sets workspace — no spawn.
- **/use** is lazy: pool has `(claude, /tmp)` → reuse; else spawn.
- **/kill** clears the AgentSession pool (activeCwd/activeAgent
  survive); the next message respawns.
- **Switching agent via `/use` keeps the old CLI alive** in the
  pool — switching back is instant, no respawn.
- **`/run` is deleted.** Use `/cwd` + `/use` instead.

`nightme run` defaults to detaching session CLIs on shutdown
(keeps state across daemon restarts). Pass `--cleanup` to kill
them all on `SIGINT`/`SIGTERM`:

```bash
./bin/nightme run --cleanup
```

Echo channel (no Feishu credentials needed, smoke-test the
daemon):

```bash
./bin/nightme run --channel=echo
```

Inspect persisted chat sessions:

```bash
./bin/nightme list
```

## Features

| Feature | Doc | Summary |
|---------|-----|---------|
| **F-27 ChatSession** | [docs/feat/F-27-chatsession.md](./docs/feat/F-27-chatsession.md) | Per-chat session context + AgentSession pool |
| **F-28 `/use <agent>`** | [docs/feat/F-28-use-command.md](./docs/feat/F-28-use-command.md) | Lazy agent switch (reuse or spawn) |
| **F-29 AgentSession pool** | [docs/feat/F-29-agent-session-pool.md](./docs/feat/F-29-agent-session-pool.md) | `(agent, cwd)` 1:1 pool, no restart on switch |
| **F-30 `nightme config`** | [docs/feat/F-30-interactive-config.md](./docs/feat/F-30-interactive-config.md) | Interactive menu for setting `primary` |
| **ChatSession → AgentSession** | [docs/feat/F-26-gateway-hub.md](./docs/feat/F-26-gateway-hub.md) | Responsibility isolation across channel/gateway/session |
| **Feishu channel** | [docs/feat/F-08-channel-abstraction.md](./docs/feat/F-08-channel-abstraction.md) | WebSocket adapter + IM rendering |
| **PTY bridge** | [docs/feat/F-19-cli-bridge.md](./docs/feat/F-19-cli-bridge.md) | PTY-backed byte pipe for any CLI |
| **Feishu one-click auth** | [docs/feat/F-22-feishu-onclick-registration.md](./docs/feat/F-22-feishu-onclick-registration.md) | QR-code onboarding |
| **Rolling-log receipt** | [docs/feat/F-25-rolling-log.md](./docs/feat/F-25-rolling-log.md) (v1.3; Channel-autonomous) | Per-turn single receipt card PATCHed in place |
| **`nightme test`** | [docs/feat/F-19-cli-bridge.md](./docs/feat/F-19-cli-bridge.md) | Local smoke test (PTY passthrough) |
| **`nightme list`** | [docs/feat/F-10-session-list-cmd.md](./docs/feat/F-10-session-list-cmd.md) | List persisted chat sessions |

## Architecture

```
┌─────────────┐    ┌─────────────┐    ┌──────────────────────┐
│  Channel    │ →  │  Gateway    │ →  │  ChatSession (per chat) │
│  (Feishu)   │ ←  │  (router)   │ ←  │  ├─ AgentSession pool  │
└─────────────┘    └─────────────┘    │  │  (agent, cwd) 1:1    │
                                     │  ├─ InputBuffer FSM     │
                                     │  ├─ readPump             │
                                     │  └─ EventHandler         │
                                     │           ↓              │
                                     │  AgentSession → Bridge   │
                                     │     (PTY / ACP / SDK)    │
                                     │           ↓              │
                                     │       Agent CLI          │
                                     └──────────────────────┘
```

- **Channel** owns transport (Feishu WebSocket; adapter pattern).
- **Gateway** routes inbound: slash commands handled in-process;
  everything else forwarded to the ChatSession's active AgentSession.
- **ChatSession** is the per-chat context. Owns the AgentSession pool
  (`(agent, cwd)` 1:1 unique) and the InputBuffer FSM.
- **AgentSession** is the per-CLI-process handle; one per `(agent, cwd)`
  pair, kept alive across `/use` switches.
- **Registry** persists `chat_sessions.json` + `agent_sessions.json`
  (mode 0600, atomic rename).

See [`docs/SPEC.md` §1](./docs/SPEC.md) for the full
responsibility table.

## Configuration

nightme reads YAML from `~/.nightme/config.yaml` (or
`$NIGHTME_CONFIG` if set). Env-var override:
`NIGHTME_<SECTION>_<KEY>` (e.g. `NIGHTME_PRIMARY`).

```yaml
primary: cc                              # global default agent
agents:                                  # list (each entry = name/bridge/command)
  - name: cc
    bridge: claude
    command: "claude --dangerously-skip-permissions"
  - name: claude
    bridge: claude
    command: claude
```

See [`configs/nightme.example.yaml`](./configs/nightme.example.yaml)
for the full schema.

Logs go to `~/.nightme/nightme.log` (mode 0600) as
JSON; attributes whose key contains `secret`, `token`, or
`password` are auto-redacted to `***REDACTED***`.

## Development

```bash
go test -race ./...      # race-tested, ~20 packages
go vet ./...             # 0 warnings required by CI
go build ./...           # must succeed for CI
```

CI runs on GitHub Actions (`.github/workflows/ci.yml`) for every
push and pull request.

### Project layout

```
cmd/nightme/                       # cobra CLI (test, list, auth, run, config)
configs/                           # example YAML config
docs/                               # PRD / SPEC / FEATURES / PLAN / feat/*
internal/
  agent/                            # Agent / AgentSession / Event interfaces
  auth/                             # Provider + Feishu one-click
  bridge/                           # Bridge abstraction (PTY / ACP / SDK)
    acp/  pty/  sdk/
  channel/                          # Channel interface + Feishu adapter
    feishu/                         #   WebSocket receive + IM rendering
  chatsession/                      # *** v1.2 (NEW) ***
                                    # ChatSession + AgentSession + Manager
                                    # Spawner + InputBuffer + readPump
  config/                           # YAML loader + env overrides
  errors/                           # CodedError + ExitCode
  gateway/                          # Slash router + binding + receipt FSM
  logging/                          # slog + secret redaction
  registry/                         # JSON-backed chat_sessions.json +
                                    # agent_sessions.json (0600, atomic)
  session/                          # *** legacy v1.x (used by gateway
                                    # BindingEntry shims; pending cleanup) ***
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

## Documentation

| Doc | What |
|-----|------|
| [`docs/PRD.md`](./docs/PRD.md) | Product definition — what / why / for whom |
| [`docs/SPEC.md`](./docs/SPEC.md) | Technical architecture — components, data flow, NFRs |
| [`docs/FEATURES.md`](./docs/FEATURES.md) | Feature index — every F-XX in one table |
| [`docs/feat/`](./docs/feat/) | Per-feature design docs |
| [`docs/E2E_TESTING.md`](./docs/E2E_TESTING.md) | Manual Feishu round-trip + troubleshooting |
| [`CHANGELOG.md`](./CHANGELOG.md) | Current snapshot (single Unreleased section) |
| [`MIGRATION.md`](./MIGRATION.md) | Breaking changes between earlier snapshots |

## License

[MIT](./LICENSE) — see [`LICENSE`](./LICENSE) for the full text.