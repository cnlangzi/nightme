# Changelog

All notable changes to nightme are documented here. nightme
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
as closely as a pre-1.0 project can.

## [Unreleased]

### Added
- GitHub Actions CI workflow (`.github/workflows/ci.yml`)

## [0.1.0] - 2026-07-31

First public release. Closes out milestones M0 / M1 / M2 and
ships the M3 hardening pass (structured logging, panic recovery,
unified exit codes, `--cleanup`, CI).

### Added
- F-22 Feishu one-click app registration (QR 扫码授权)
- F-08 Feishu channel adapter (WebSocket + IM rendering)
- F-20 Gateway (slash command router with `/cwd /run /kill /help`)
- F-21 Agent Communication Modes (ACP / SDK / PTY stubs)
- F-19 PTY Mode byte pipe (`aymanbagabas/go-pty`)
- Session lifecycle (`/cwd` + `/run` two-step)
- Process Registry (JSON persistence, mode 0600, atomic rename)
- Local Bridge test mode (`nightme test`, `--cleanup` to kill on shutdown)
- Session list command (`nightme list`, `--json`)
- Structured logging (`internal/logging`, slog + JSON + secret redaction)
- Panic recovery middleware (`Recover()` → `CodeGenericError`)
- Unified error codes (`internal/errors`, `ExitCode()`)
- GitHub Actions CI (`go build` + `go vet` + `go test -race`)
- Release workflow (build + publish on `v*` tags)

### Configuration
- YAML config at `~/.config/nightme/config.yaml`
- `NIGHTME_<SECTION>_<KEY>` env-var overrides
- Logging defaults to `~/.local/share/nightme/nightme.log` (mode 0600)

## [0.0.0] - 2026-07-25 (M0)

Initial documentation-only milestone: PRD, SPEC, FEATURES index,
PLAN, and per-feature design docs (`docs/feat/F-01..F-21`).
