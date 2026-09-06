# Primary-Agent Resolution, Built-in Whitelist, and First-Run Prompt

> **Status**: implemented
> **Scope**: `cfg.Primary` resolution chain, `internal/agent/registry.go` insertion order, `internal/bridge/pty` exclusion from `agent.Builtins`, `cfg.Agents` path-override semantics, `cmd/nightme/firstrun.go` interactive prompt
> **Related docs**:
> - [`SPEC.md`](./SPEC.md) §1.1 Primary Agent、§1.3 不变式
> - [`feat/F-09-agent-abstraction.md`](./feat/F-09-agent-abstraction.md) 三层抽象(AgentSpec / Starter / Agent)
> - [`feat/F-chat-session.md`](./feat/F-chat-session.md) §3 ChatSession.PrimaryAgent 只读语义
> - [`CHATSTORE.md`](./CHATSTORE.md) §3 ChatSession bootstrap 失败兜底

---

## 1. Resolution chain (`cfg.Primary`)

`LoadDefault` (`internal/config/config.go`) layers four sources, later wins:

| # | Source | Mechanism |
|---|--------|-----------|
| 1 | `cfg.Primary` persisted in YAML | `Load(path)` → `yaml.Unmarshal` |
| 2 | `NIGHTME_PRIMARY` env | `applyEnvOverrides` |
| 3 | First built-in whose `Detect()` passes | `detectPrimaryFromBuiltins` |
| 4 | Interactive first-run prompt | `cmd/nightme/firstrun.go::EnsureAgentAvailable` (when none of the above yields a working agent) |

Steps 1–3 run inside `LoadDefault`. Step 4 runs at the entry point of `nightme run` and `nightme test` after the config has been loaded; it never re-reads disk on its own.

```text
$ NIGHTME_PRIMARY=codex nightme run
    ├─ yaml primary: codex     ← step 1, no prompt
    └─ Env overrides same value
```

```text
$ nightme run            # fresh install, only opencode on PATH
    ├─ yaml: empty
    ├─ env: unset
    ├─ Builtins.List() probe:
    │     claude   → error  ✗
    │     codex    → error  ✗
    │     dsh      → error  ✗
    │     opencode → nil    ✓
    ├─ cfg.Primary = "opencode"
    └─ SaveDefault:         ~/.nightme/config.yaml (primary: opencode)
```

```text
$ nightme run            # nothing on PATH, TTY attached
    ├─ yaml: empty
    ├─ env: unset
    ├─ Builtins.List() probe: all fail
    ├─ First-run prompt:
    │     nightme: no AI coding agent found in PATH or cfg.Agents.
    │     Supported built-ins:
    │       [1] claude       not found
    │       [2] codex        not found
    │       ...
    │     Enter a number to configure that agent's path, or q to abort:
    │     > 1
    │     Absolute path to claude binary (q to abort):
    │     > /Users/me/.local/bin/claude
    │     ✓ wrote primary=claude, cfg.Agents[claude]=/Users/me/.local/bin/claude
    └─ cfg.Agents and cfg.Primary persisted
```

---

## 2. `cfg.Agents` is a path-override table, not an agent list

Each entry is `{name, command}` where `name` MUST be a built-in (`claude / codex / dsh / opencode / cursor / pi / copilot`) and `command` is an absolute path to that binary. The bridge, mode, args, env, and protocol wiring stay fixed by nightme; the user only gets to point the binary at a non-PATH location.

`agentregistry.Build` (`internal/agentregistry/agentregistry.go`) applies the overrides built-in-driven rather than config-driven:

```go
overrides := cfgPathMap(cfg.Agents)
for _, s := range reg.List() {           // every registered built-in
    name := s.Info().Name
    if path, ok := overrides[name]; ok { // consult config as a side lookup
        agent.SetBuiltinCommand(reg, name, path)
    }
}
```

Iteration is over the registered built-ins, not over cfg.Agents. Names outside the whitelist are never read — no warn log, no PTY fallback, no silently aliased shell agent. The bridge surface (AskUserQuestion, tool events, ACP handshake) is too valuable to lose; users who want a shell wrapper go through `nightme test --agent /path/to/bin` (bare-path auto-register), which is explicitly a one-shot escape hatch rather than a primary.

`cfgPathMap` stores the path verbatim (trimmed, not whitespace-split) so Windows paths with spaces (`C:\Program Files\claude\claude.exe`) round-trip unchanged. Args / env / mode / protocol flags are not user-configurable here — they're fixed by the bridge.

---

## 3. Built-in whitelist & registration order

`cmd/nightme/agents.go::init` registers the seven built-ins in a fixed order. The order is the auto-detection priority chain when more than one resolves; new built-ins append to the end.

```
claude / codex / dsh / opencode / cursor / pi / copilot
```

Append-only: `cmd/nightme/agents.go:7-11` doc explains why inserting a new entry earlier permanently shifts every existing user's auto-detection outcome.

`cursor` uses binary name `cursor-agent`, not `cursor`, because the official installers (bash and PowerShell variants) both create `cursor-agent` as the real entry-point and only optionally alias it.

---

## 4. `Detect()` now resolves absolute paths too

Every built-in starter's `Detect()` was rewritten as:

```go
return agent.ResolveCommand(s.command)
```

`ResolveCommand` (`internal/agent/commandpath.go`) prefers `os.Stat` for absolute paths and falls back to `exec.LookPath` for relative names. This is what makes the cfg.Agents override actually work: a non-PATH absolute path is stat-checked, not searched.

`SetBuiltinCommand` (`internal/agent/commandpath.go`) is the seam cfg.Agents overrides flow through. It mutates the registered `*Starter` (a singleton held in `agent.Builtins`), so subsequent `Detect()` and `Info()` reflect the new path.

---

## 5. First-run prompt (`cmd/nightme/firstrun.go`)

`EnsureAgentAvailable(cfg, in, out)` runs at the top of `nightme run` and `nightme test`. Decision tree:

```
load cfg
    │
    ▼
probe every built-in (cfg.Agents overrides applied)
    │
    ├─ cfg.Primary resolves ─────────► return nil
    │
    ├─ at least one built-in resolves ► auto-pick first, save, return nil
    │
    ├─ no TTY / NIGHTME_NO_PROMPT=1 ──► return errNoAgentConfigured
    │
    └─ otherwise ─────────────────────► firstrunPrompt(in, out):
                                           list built-ins with status
                                           read agent choice
                                           promptAgentPath loop:
                                               absolute, file exists, ResolveCommand OK
                                           applyAgentOverride + SaveDefault
                                           cfg.Primary = picked
```

`firstrunPrompt` validates the user-supplied path before saving: `os.Stat` (must be a regular file) plus `agent.ResolveCommand` (must resolve to an invokable binary). On success it writes a single `cfg.Agents` entry and sets `cfg.Primary` in one `SaveDefault`.

`canPrompt` returns false when stdin is not a TTY OR `NIGHTME_NO_PROMPT=1` is set. CI / container invocations that hit the all-fail branch surface `errNoAgentConfigured` verbatim rather than hanging on a read.

---

## 6. Why PTY is not in Builtins

`internal/bridge/pty` is shared infrastructure for the bridges that need a PTY transport, and for the one-shot bare-path agent case (`nightme test --agent /path/to/bin`). It is NOT a user-facing agent. Putting `"bash"` into `agent.Builtins` would auto-resolve on every Unix box and silently claim the `bash` primary slot, which is wrong on every level:

- The primary agent should be an AI coding CLI, not a shell.
- `nightme agents` would list `bash` as a usable agent.
- Auto-detection would never get past the first probe.

The whitelist keeps pty out. Bare-path auto-register still works for one-off testing.

---

## 7. Non-interactive bail-out contract

`errNoAgentConfigured` is the canonical "no agent available" error. Its message embeds the remediation hint:

```
no AI coding agent available; install one of the built-ins
(claude / codex / dsh / opencode / cursor / pi / copilot) or set
cfg.Agents to override its path, then retry
```

`nightme run` and `nightme test` propagate this verbatim to stderr and exit non-zero. CI smoke tests assert on the string and the exit code.

---

## 8. Code map

| File | Role |
|------|------|
| `internal/agent/commandpath.go` | `ResolveCommand` (absolute vs PATH), `CommandSetter` interface, `SetBuiltinCommand` |
| `internal/agent/registry.go` | `Builtins` package var, registration helpers |
| `internal/agentregistry/agentregistry.go` | `Build(cfg, requested)` — whitelist enforcement + path overrides + bare-path escape hatch |
| `internal/config/config.go` | `LoadDefault` (steps 1–3), `detectPrimaryFromBuiltins` |
| `cmd/nightme/firstrun.go` | `EnsureAgentAvailable`, `firstrunPrompt`, `promptAgentPath`, `applyAgentOverride`, `removeAgentOverride` |
| `cmd/nightme/run.go` | Pre-flight `EnsureAgentAvailable` before `runtime.Runner.Run` |
| `cmd/nightme/test.go` | Same pre-flight before `agentregistry.Build` |
| `cmd/nightme/config.go` | `[2] Agents` interactive menu — built-in list with status, sub-menu for set-primary / change-path / remove-override |
| `cmd/nightme/agents.go` | Built-in registration (`init`) |
