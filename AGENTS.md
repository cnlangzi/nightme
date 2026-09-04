# AGENTS.md


## 1. Documentation Rules

Docs describe the system **as it is now**. History, process, and change narratives belong in PRs / commits / ADRs — not here.

### Don't write

- **Change narrative**: "was X, now Y"; "adjusted because of Z"; "as requested by N"
- **Temporal words**: now / recently / newly / "as of vX"; 中文：新增了 / 现已支持 / 之前 / 原本
- **Process artifacts**: task splits, debug logs, file lists, verification steps, options considered
- **Preambles**: "This document introduces..."; "The following changes..."

### Don't create

No new doc without an explicit request.

### When updating

Rewrite the whole section. Delete what is replaced. Never keep two versions side by side.

### Self-check

Read every sentence as a first-time reader. If removing the timestamp or context makes it false, delete it.

## 2. Comment Rules

- Write code that speaks for itself. Comment only when necessary to explain **WHY**, not WHAT. We do not need comments most of the time.
- ❌ AVOID — **Obvious Comments**(复述代码)、**Redundant Comments**、**Outdated Comments**、**Commented-out Code**、**Changelog Comments**、**Divider Comments**

## 3. Project Layout

- Module: `github.com/cnlangzi/nightme`
- Entry: `cmd/nightme/` (CLI + daemon + tray)
- Per-AI-agent backends: `internal/bridge/{claudecode,codex,copilot,cursor,dsh,opencode,pi,pty,acp}`
- Per-chat-platform channels: `internal/channel/{feishu,slack,telegram,echo,bot}`
- Core runtime: `internal/{chatsession,chatstore,command,config,gateway,messages,wfe,registry,runtime,login}`
- README.md has the full map and concepts (ChatSession, CWD = project, Bridge, Channel, Messages, WFE).

## 4. Verify Before Done

- Build:  `make build`           # bin/nightme[.exe] with version metadata
- Test:   `make test`            # `go test -race ./...`
- Lint:   `make lint`            # `go vet ./...` — matches CI
- Fmt:    `make fmt`             # `go fmt ./...`
- Dev:    `make dev`             # `go run ./cmd/nightme` with example config
- Linux GUI variant (`make build-gui`) needs libgtk-3 + libayatana-appindicator3; default build is tray-less.
- CI runs linux + windows + darwin. Respect existing `//go:build !windows` / `//go:build !unix` tags; don't break cross-platform splits.

## 5. Code Style & Runtime Rules

### Style

- No `type X = Y` aliases. Use the underlying type directly; don't keep aliases for backward-compat names.
- Test merges: when tests conflict in a rebase, prefer rewriting over mechanical 3-way merge.

### Runtime

- Bridge layer is transport + permissions only. Do **not** inject model / provider / credentials / bundled config into downstream AI agents — keep their defaults intact.
- Bot failure handling: silent recovery first (retry / respawn / `--resume`). Only notify the chat when recovery is impossible. Do not invent a new notification path.