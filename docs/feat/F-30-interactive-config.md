# F-30: Interactive Config Command

> **Status**: locked

> **Depends on**: F-08 (config loading), F-09 (Agent registry), F-20 (slash command pattern)

> **Related docs**: [`SPEC.md`](../SPEC.md)(config), [`PRD.md`](../PRD.md)(Minimal), [`../primary-agent-detection.md`](../primary-agent-detection.md)

---

## 1. Description

`nightme config` opens an interactive menu for configuring nightme.
Two submenus are available:

- **Name** — show / set the instance name (`config.Name`)
- **Agents** — list the registered built-in agents and manage each
  one: set it as the global primary (`config.Primary`) and set or
  remove its binary path override (`config.Agents`)

The interactive mode replaces direct YAML editing for the most
common operations: choosing which agent to spawn when no explicit
`/use <name>` is given, and pointing nightme at an agent binary
that is not on `PATH`.

---

## 2. Invocation

```
nightme config
```

Reads `config.DefaultPath()` — `$HOME/.nightme/config.yaml`, or
whatever `NIGHTME_CONFIG` points at — and writes back to that same
path.

---

## 3. Menu flow

```
nightme config — interactive
===========================

Main menu:
  [1] Name    show/set instance name
  [2] Agents
  [q] Quit
> 2

   #  NAME      BRIDGE   COMMAND                 ARGS
✓  1  claude    json-io  /usr/local/bin/claude
✓  2  codex     json-io  codex
✓  3  dsh       json-io  dsh
✓  4  opencode  acp      opencode               acp
✓  5  cursor    acp      /usr/local/bin/cursor-agent  --force --trust --sandbox disabled --approve-mcps acp
✓  6  pi        json-io  pi                     --mode rpc
✓  7  copilot   acp      copilot                --allow-all-tools --acp --stdio
✓  8  omp       json-io  omp                     --mode rpc --approval-mode yolo

Current primary: claude
Enter number to manage, q to cancel:
> 1

claude — PATH
  [1] Set as primary
  [2] Configure path
  [b] Back
> 1
✓ saved to /home/user/.nightme/config.yaml

Main menu:
  [1] Name    show/set instance name
  [2] Agents
  [q] Quit
> q
Bye.
```

The `✓` column marks the agents whose `Detect()` passes. The
`COMMAND` cell shows the `cfg.Agents` override when one exists,
otherwise the resolved `PATH` lookup.

The per-agent sub-menu depends on the agent's state:

| 状态 | 选项 |
|------|------|
| `Detect()` 通过，无路径覆盖 | `[1] Set as primary` / `[2] Configure path` / `[b] Back` |
| `Detect()` 通过，有路径覆盖 | `[1] Set as primary` / `[2] Change path` / `[3] Remove path override` / `[b] Back` |
| `Detect()` 失败，无路径覆盖 | `[2] Configure path` / `[b] Back`（没有 "Set as primary"）|
| `Detect()` 失败，有路径覆盖 | `[2] Change path` / `[3] Remove path override` / `[b] Back` |

The status line after the agent name says where the binary comes
from: `PATH`, `cfg: <path>`, `cfg (broken): <path>` or `not found`.

`[2] Configure path` prompts for an absolute path and re-runs
`Detect()` until the path resolves:

```
Absolute path to claude binary (q to abort):
> /usr/local/bin/claude
```

The **Name** sub-menu shows the effective instance name (the
configured `name`, or the machine hostname when `name` is unset) and
accepts a new one; empty input keeps the current value.

```
Current name: my-laptop
Enter new name (empty to keep current):
> my-box
✓ Name set to "my-box".
  Saved to: /home/user/.nightme/config.yaml
```

`q` cancels without saving at any prompt.

---

## 4. Registry merge (`agentregistry.Build`)

The agent list is not merged by the menu — it comes from
`agentregistry.Build(cfg, "")`
(`internal/agentregistry/agentregistry.go`), which walks the
**registered built-ins**, not `cfg.Agents`:

```go
reg := agent.New()
for _, a := range agent.Builtins.List() {   // 注册顺序，不是字母序
    a.Init("")                              // 先复位成 bridge 自带的默认命令
    if cfg != nil {
        if path := cfgAgentPath(cfg.Agents, a.Info().Name); path != "" {
            a.Init(path)                   // 再套上 cfg.Agents 里的路径覆盖
        }
    }
    reg.Register(a)
}
```

Consequences:

- The display order is the **registration order** in
  `cmd/nightme/agents.go` (`claude / codex / dsh / opencode /
  cursor / pi / copilot / omp`), which is also the primary-agent
  auto-detection priority chain — see
  [`../primary-agent-detection.md`](../primary-agent-detection.md).
- `cfg.Agents` entries whose `name` is not a built-in are ignored:
  there is no PTY fallback and no shell alias.
- A path override is stored verbatim (trimmed, not whitespace-split),
  so Windows paths with spaces round-trip unchanged.
- `cfg.Agents` only overrides the binary path. Bridge, mode, args
  and env stay fixed by nightme.

---

## 5. Save semantics

Every change goes through `config.Update(path, fn)`
(`internal/config/config.go`), which loads the file at save time,
applies the change, and writes it back under one lock:

- Uses atomic write (temp file + rename) per NFR N-7
- chmod 0600 on the resulting file
- Creates parent directory if missing (0700)

Because the file is re-read at save time, a block another writer
changed while the menu was open — the `me.llm` block `nightme me`
writes, say — survives the save.

After a successful save the menu prints `✓ saved to <path>`. If a
daemon is running it also prints a warning that the daemon has to be
restarted for the change to take effect.

Cancellation (`q` at a prompt) leaves the file untouched.

---

## 6. Persistence interaction

`nightme config` reads and writes the same file as the rest of the
binary (`config.DefaultPath()`).

If a user later hand-edits `config.yaml` to add a new `agents:`
entry, the next `nightme config` shows that entry's path in the
`COMMAND` column of the matching built-in row.

---

## 7. Test strategy

### 7.1 Unit

- `agentregistry.Build` with an empty `cfg.Agents` → every built-in
  keeps its baked-in command
- `agentregistry.Build` with a path override → the built-in's
  `Command()` becomes the override
- `agentregistry.Build` with an unknown `name` → the entry is ignored
- `agentregistry.Build` with a path containing spaces → the path is
  kept verbatim
- Removing an override → the built-in reverts to its default command

### 7.2 Integration

- Full menu loop: pick index → save → reload → assert primary updated
- Cancel with `q` → no save, cfg unchanged
- Top-level `q` → exits cleanly
- Name sub-menu: set a new name → saved to the same path the menu was
  loaded from
- A `me.llm` block another writer changed on disk while the menu was
  open survives the save

### 7.3 Manual

- `nightme config` in real shell with real agents installed
- Verify the menu looks right (column widths, ordering)
- Verify the save persists across daemon restarts

---

## 8. Out of scope

- **Other submenus** (feishu / session / logging / paths) — future F-XX
- **User-defined agents** — the menu only manages the registered
  built-ins; `cfg.Agents` is a path-override table, not an agent list
- **Bubbletea TUI** — current implementation is plain stdin/stdout;
  upgrade to a full TUI later if needed
- **Multi-select** (e.g. pick multiple favorites) — pick-one is sufficient
  for "global primary" semantics

---

## 9. Open questions

- **Q-A**: Yaml schema is `primary` (top-level scalar) + `agents:` (top-level list).
  Currently no nesting. If future versions want to group agents under a
  sub-key, this changes. (Lean: keep flat; matches user-stated example.)
- **Q-B**: Should `nightme config` also show the current Feishu config
  status (configured / not configured) without editing? (Lean: yes, but
  in a future submenu, not Agents.)
- **Q-C**: Should selection remember the last chosen index across
  invocations? (Lean: no — fresh list each time, easy mental model.)
