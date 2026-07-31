# nightme

Sleep tight, code all night.

nightme is a single-process daemon that bridges AI Coding CLIs
(Claude Code / Codex / OpenCode) to IM channels (Feishu / WhatsApp /
Web UI), so you can drop "write X for me" into a chat at night and
collect the result in the morning.

> **Status**: v0.1 — M2 done. The full Feishu round-trip is in
> place: a chat can drive session lifecycle via slash commands
> and exchange text with the live agent. See
> [`docs/PLAN.md`](./docs/PLAN.md) for the roadmap.

## Quickstart

Requires Go 1.22+.

```bash
# 1. Build
go build -o bin/nightme ./cmd/nightme

# 2. (Optional) Copy the example config and edit
cp configs/nightme.example.yaml ~/.config/nightme/config.yaml

# 3. Local Bridge smoke test — spawns /bin/echo in a PTY
./bin/nightme test --workspace /tmp --agent /bin/echo --args hello

# 4. List persisted sessions
./bin/nightme list
```

The `test` command forwards stdin to the agent and writes agent
output to stdout. Send `SIGINT` (Ctrl+C) to detach (the child CLI
survives by default — see SPEC §3) or `SIGTERM` to force-kill.

The `list` command reads `~/.local/share/nightme/registry.json`
and prints every persisted session. Pass `--json` for a
machine-readable view.

### Feishu channel (M2)

After building, register a Feishu app with the QR flow and start the
daemon:

```bash
# 1. One-click Feishu registration (scan the QR code)
./bin/nightme auth login feishu

# 2. Configure agents in ~/.config/nightme/config.yaml
cat >> ~/.config/nightme/config.yaml <<'YAML'
agent:
  agents:
    claude:
      command: claude
    codex:
      command: codex
YAML

# 3. Start the daemon
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

The full round-trip — including expected replies, troubleshooting,
and known limitations — is in [`docs/E2E_TESTING.md`](./docs/E2E_TESTING.md).

## Configuration

nightme reads YAML from `~/.config/nightme/config.yaml` (or
`$NIGHTME_CONFIG` if set). Every field can be overridden by a
`NIGHTME_<SECTION>_<KEY>` environment variable. See
[`configs/nightme.example.yaml`](./configs/nightme.example.yaml) for
the full schema and [`docs/SPEC.md` §6](./docs/SPEC.md) for the
resolution rules.

## Project layout

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
  gateway/                # Slash command router + 4 default handlers
  registry/               # JSON-backed process registry (0600, atomic writes)
  session/                # Session + MemoryManager + Restore / Persist
```

## Documentation

| Doc | What |
|-----|------|
| [`docs/PRD.md`](./docs/PRD.md) | Product definition — what / why / for whom |
| [`docs/SPEC.md`](./docs/SPEC.md) | Technical architecture — components, data flow, NFRs |
| [`docs/FEATURES.md`](./docs/FEATURES.md) | Feature index — every F-XX in one table |
| [`docs/PLAN.md`](./docs/PLAN.md) | Implementation roadmap — M1 → M2 → M3 |
| [`docs/feat/`](./docs/feat/) | Per-feature design docs (F-01, F-04, F-05, F-10, …) |
| [`docs/E2E_TESTING.md`](./docs/E2E_TESTING.md) | Manual Feishu round-trip + troubleshooting |

## M2 status

- **M2 done.** — Feishu Channel adapter, AgentEvent rendering, Gateway
  router with `/cwd /run /kill /help`, session lifecycle
  (`CreateOrUpdate / Run / KillByChat`), Feishu round-trip, and the
  manual E2E harness.

## v0.2 plan

- **ACP backend** — v0.2 swaps the PTY default for an ACP transport
  (Codex / OpenCode), giving structured events and proper permission
  cards instead of raw TTY bytes.
- **ACP server mode** — let an external agent speak ACP to nightme
  over a unix socket, so terminal-only sessions can join the same
  multi-channel mirror.
- **Permission-card routing** — connect the Feishu card click handler
  to `AgentSession.SendPermission` so permission prompts actually
  resolve.
- **Multi-channel mirror** — the same chat fan-out to multiple IMs
  (Feishu + Web terminal) per session.
- **Hardening** — slog logging, `--cleanup` flag, CI, v0.2.0 release.
