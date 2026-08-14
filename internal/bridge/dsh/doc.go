// Package dsh is the nightme bridge for DeepSeek Harness (dsh).
//
// The bridge is print-mode only: it spawns `dsh --profile headless
// "<prompt>"` as a child process, captures the final assistant text
// from stdout, and returns it as agent.RunResult. There is no chat
// session path — dsh's `--profile headless` profile is documented as
// "Answer one task, print the final assistant message, and exit"
// (no --resume support), and the only other long-lived transport
// (`dsh-jsonrpc-agent-pkg`) requires pip-installing the packaged
// runtime which is outside the scope of this PR.
//
// The bridge deliberately does NOT modify dsh's local default
// configuration. Per the user's locked-in principle
// (agent-no-config-tampering), nightme only injects:
//
//   - cmd.Dir = cfg.Workspace (runtime context, not configuration)
//   - DSH_PERMISSION_MODE=danger-full-access (permissions — the
//     one knob the user explicitly wants nightme to override)
//
// Everything else (provider / model / API key / system prompt /
// sandbox policy / compaction / etc.) flows from dsh's local
// defaults at `~/.dsh/settings.yaml` + `~/.dsh/.credentials.yaml`.
// See docs/feat/F-dsh-bridge.md for the full rationale.
//
// Scope:
//
//   - RunOnce  — `/gtw commit`, `/gtw pr`, `buildAgentPrompt`
//   - chat session  — NOT implemented; dsh starter.Start returns
//     "chat session not implemented" to surface the limitation
//     instead of silently falling back to PTY noise.
package dsh
