# nightme

Sleep tight, code all night.

nightme is a single-process daemon that bridges AI Coding CLIs
(Claude Code / Codex / OpenCode) to IM channels (Feishu / WhatsApp /
Web UI), so you can drop "write X for me" into a chat at night and
collect the result in the morning.

> **Status**: v0.1 — M1 done (11/11 commits). M2 PR #4 ships the Feishu
> Channel adapter, event renderer, and `nightme run` daemon skeleton. The
> Gateway and Feishu round-trip land in M2 PR #5. See
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

### Feishu daemon (M2 PR #4)

After building, register a Feishu app with the QR flow and start the daemon:

```bash
./bin/nightme auth login feishu
./bin/nightme run
```

Send `hello` to the bot. PR #4 prints `received: hello` in the daemon
terminal; it does not reply in Feishu or create an agent session yet. See the
[`docs/E2E_TESTING.md`](./docs/E2E_TESTING.md) guide for the full manual test
and troubleshooting steps.

## Configuration

nightme reads YAML from `~/.config/nightme/config.yaml` (or
`$NIGHTME_CONFIG` if set). Every field can be overridden by a
`NIGHTME_<SECTION>_<KEY>` environment variable. See
[`configs/nightme.example.yaml`](./configs/nightme.example.yaml) for
the full schema and [`docs/SPEC.md` §6](./docs/SPEC.md) for the
resolution rules.

## Project layout

```
cmd/nightme/              # cobra CLI (test, list, …)
configs/                  # example YAML config
docs/                     # PRD / SPEC / FEATURES / PLAN / feat/*
internal/
  agent/                  # Agent / AgentSession / Event interfaces + registry
    ptyagent/             #   PTY-mode agent (default for v0.1)
  channel/                # Channel interface and Feishu adapter/renderer
    feishu/               #   WebSocket receive + IM message rendering
  bridge/                 # Bridge abstraction (ACP / SDK / PTY)
    acp/  pty/  sdk/      #   three backend implementations
  config/                 # YAML loader + NIGHTME_* env overrides
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
| [`docs/E2E_TESTING.md`](./docs/E2E_TESTING.md) | Manual Feishu channel and daemon test guide |

## M2 status

- **PR #4 (complete)** — Feishu Channel adapter, AgentEvent rendering,
  `nightme run`, session restore, and the manual E2E harness.
- **PR #5 (next)** — Gateway routing and the Feishu-to-agent round-trip.

## v0.1 plan

- **M2** — Feishu channel and Gateway MVP: `/cwd` / `/run` / `/kill`,
  ACP/PTY agent registration, and the Feishu round-trip.
- **M3** — Hardening: error edges, slog logging, `--cleanup` flag,
  CI, v0.1.0 release.