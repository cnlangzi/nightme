# nightme

> Sleep tight, code all night.

![Status](https://img.shields.io/badge/status-v0.1.0-blue)
![Go](https://img.shields.io/badge/go-1.22%2B-00ADD8)
![License](https://img.shields.io/badge/license-MIT-green)

nightme is a single-process daemon that bridges AI Coding CLIs
(Claude Code / Codex / OpenCode) to IM channels (Feishu / WhatsApp /
Web UI), so you can drop "write X for me" into a chat at night and
collect the result in the morning.

> **Status**: v0.1.0 — M3 done. The Feishu round-trip is in place
> with structured logging, panic recovery, and CI. See
> [`docs/PLAN.md`](./docs/PLAN.md) for the roadmap.

## Screenshot

_Coming in v0.2 — see [#X](https://github.com/cnlangzi/nightme/issues)._

## Install

Requires Go 1.22+ and `GOPROXY` configured for the proxy you use
(`https://goproxy.cn,direct` works on mainland China).

```bash
go install github.com/cnlangzi/nightme/cmd/nightme@v0.1.0
```

Or build from source:

```bash
git clone https://github.com/cnlangzi/nightme.git
cd nightme
go build -o bin/nightme ./cmd/nightme
```

## Quick Start

### 1. Local Bridge smoke test

```bash
./bin/nightme test --workspace /tmp --agent /bin/echo --args hello
```

The `test` command forwards stdin to the agent and writes agent
output to stdout. Send `SIGINT` (Ctrl+C) to **detach** (the child
CLI survives), or pass `--cleanup` to **kill** it instead:

```bash
./bin/nightme test --cleanup --workspace /tmp --agent /bin/echo --args hello
```

Inspect persisted sessions:

```bash
./bin/nightme list           # human-readable table
./bin/nightme list --json    # machine-readable
```

### 2. Feishu channel

```bash
# (Optional) Copy and edit the example config
cp configs/nightme.example.yaml ~/.config/nightme/config.yaml

# One-click Feishu registration (scan the QR code)
./bin/nightme auth login feishu

# Start the daemon
./bin/nightme run
```

In a 1:1 Feishu chat with the bot:

```text
/cwd /tmp             # bind this chat to a workspace
/run claude           # spawn the CLI in that workspace
hello                 # plain text flows to the agent
/kill                 # stop the CLI (session preserved)
/help                 # list every nightme command
```

`nightme run` defaults to detaching session CLIs on shutdown
(default keeps state across daemon restarts). Pass `--cleanup` to
kill them all on `SIGINT`/`SIGTERM` — convenient for CI or
one-shot scripts:

```bash
./bin/nightme run --cleanup
```

## Features (v0.1.0)

| Feature | Status | Description |
|---------|--------|-------------|
| F-19 PTY backend | ✅ | PTY-backed byte pipe for any CLI |
| F-21 Agent modes | 🟡 stubs | ACP / PTY (SDK stub in v0.2) |
| F-22 Feishu auth | ✅ | One-click QR registration |
| F-08 Feishu channel | ✅ | WebSocket adapter + IM rendering |
| F-20 Gateway | ✅ | Slash router (`/cwd /run /kill /help`) |
| F-05 Process registry | ✅ | JSON persistence (mode 0600) |
| F-10 Session list | ✅ | `nightme list` CLI |
| F-23 Panic recovery | ✅ | Recover → CodeGenericError |
| F-24 Structured log | ✅ | slog + secret redaction |
| F-25 `--cleanup` | ✅ | Kill vs detach on shutdown |

## Architecture

nightme is a thin pipeline. Each message walks three layers:

```
┌─────────────┐    ┌─────────────┐    ┌─────────────┐
│  Channel    │ →  │  Gateway    │ →  │  Session    │
│  (Feishu)   │ ←  │  (router)   │ ←  │  + Bridge   │
└─────────────┘    └─────────────┘    └─────────────┘
                                          ↓
                                     Agent CLI (PTY)
```

- **Channel** owns the transport (Feishu WebSocket today; adapter
  pattern so WhatsApp / Web TTY can drop in).
- **Gateway** routes inbound messages: slash commands are handled
  in-process; everything else is forwarded to the live session.
- **Session** binds a chat to a workspace and an AgentSession
  handle. The handle is owned by a Bridge (PTY today; ACP tomorrow).
- **Registry** persists the session table as JSON (mode 0600,
  atomic rename).

See [`docs/SPEC.md`](./docs/SPEC.md) for the long form and
[`docs/PRD.md`](./docs/PRD.md) for the product framing.

## Configuration

nightme reads YAML from `~/.config/nightme/config.yaml` (or
`$NIGHTME_CONFIG` if set). Every field can be overridden by a
`NIGHTME_<SECTION>_<KEY>` environment variable. See
[`configs/nightme.example.yaml`](./configs/nightme.example.yaml)
for the full schema and [`docs/SPEC.md` §6](./docs/SPEC.md) for
the resolution rules.

Logs are written to `~/.local/share/nightme/nightme.log` (mode
0600) as JSON; any attribute whose key contains `secret`,
`token`, or `password` is automatically rewritten to
`***REDACTED***`.

## Development

```bash
go test -race ./...      # race-tested, ~219 tests
go vet ./...             # 0 warnings required by CI
go build ./...           # must succeed for CI
```

CI runs on GitHub Actions (`.github/workflows/ci.yml`) for every
push and pull request. Coverage artifacts are uploaded on pushes
to `main`.

### Project layout

```
cmd/nightme/              # cobra CLI (test, list, auth, run)
configs/                  # example YAML config
docs/                     # PRD / SPEC / FEATURES / PLAN / feat/*
internal/
  agent/                  # Agent / AgentSession / Event interfaces + registry
    ptyagent/             #   PTY-mode agent (default for v0.1)
  auth/                   # Provider interface + Feishu one-click flow
  bridge/                 # Bridge abstraction (ACP / SDK / PTY)
    acp/  pty/  sdk/      #   three backend implementations
  channel/                # Channel interface and Feishu adapter/renderer
    feishu/               #   WebSocket receive + IM message rendering
  config/                 # YAML loader + NIGHTME_* env overrides
  errors/                 # CodedError + ExitCode (M3)
  gateway/                # Slash command router + 4 default handlers
  logging/                # slog + secret redaction (M3)
  registry/               # JSON-backed process registry (0600, atomic writes)
  session/                # Session + MemoryManager + Restore / Persist
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
| [`docs/PLAN.md`](./docs/PLAN.md) | Implementation roadmap — M0 → M1 → M2 → M3 |
| [`docs/feat/`](./docs/feat/) | Per-feature design docs (F-01, F-04, F-05, F-10, …) |
| [`docs/E2E_TESTING.md`](./docs/E2E_TESTING.md) | Manual Feishu round-trip + troubleshooting |
| [`CHANGELOG.md`](./CHANGELOG.md) | Version history |

## License

[MIT](./LICENSE) — see [`LICENSE`](./LICENSE) for the full text.
