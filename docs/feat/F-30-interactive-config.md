# F-30: Interactive Config Command

> **Status**: implemented (v1.2; first shipped with the config schema rename)
> **Milestone**: v1.2
> **Depends on**: F-08 (config loading), F-09 (Agent registry), F-20 (slash command pattern)
> **Used by**: end users (manual setup)
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.2 §5 (config), [`PRD.md`](../PRD.md) v1.2 §4.5 (Minimal)

---

## 1. Description

`nightme config` opens an interactive menu for configuring nightme.
The first (and currently only) submenu is **Agents**, where users
pick which agent to set as the global primary (default).

The interactive mode replaces direct YAML editing for the most
common operation: choosing which agent to spawn when no explicit
`/use <name>` is given.

---

## 2. Invocation

```
nightme config
```

Reads `~/.config/nightme/config.yaml` (or whatever `NIGHTME_CONFIG`
points at), merges built-in agents with user-configured agents, lets
the user pick one, and writes back the new `primary` value.

---

## 3. Menu flow

```
nightme config — interactive
===========================

Main menu:
  [1] Agents
  [q] Quit
> 1

Agents (merged: builtins + your config):
    [1] bash        (pty)     bash
  * [2] claude      (json-io) claude
    [3] cc          (claude)  claude --dangerously-skip-permissions

Current primary: claude
Enter number to set as primary [1-3], q to cancel: 3
✓ Primary set to "cc" (config)
✓ Saved to /home/user/.config/nightme/config.yaml

Main menu:
  [1] Agents
  [q] Quit
> q
Bye.
```

`*` marks the current `primary` choice. `q` cancels without saving
at any prompt.

---

## 4. Merge logic (`MergeAgents`)

```
result = []
seen   = {}                // name → index in result

// 1. Built-ins first (alphabetical order).
for a in sorted(agent.Builtins.List()):
    result.append({Name: a.Name(),
                   Bridge: a.Mode().String(),
                   Command: a.Command() + " " + a.Args().join(),
                   Source: "builtin"})
    seen[a.Name()] = result.lastIndex

// 2. User config overrides on name collision; otherwise appended.
for entry in cfg.Agents:
    choice = {Name, Bridge, Command, Source: "config"}
    if entry.Name in seen:
        result[seen[entry.Name]] = choice     // override builtin
    else:
        result.append(choice)
        seen[entry.Name] = result.lastIndex
```

The result preserves insertion order: builtins alphabetical, then
user config in YAML order. This makes the display index stable for
the duration of one `nightme config` invocation.

`MergeAgents` is a **pure function** (cfg in → choices out) for
testability. The interactive loop wraps it with display + selection
+ save.

---

## 5. Save semantics

Selection triggers `config.SaveDefault(cfg)`, which:
- Uses atomic write (temp file + rename) per NFR N-7
- chmod 0600 on the resulting file
- Creates parent directory if missing (0700)

Cancellation (`q` at the selection prompt) leaves the file
untouched. The in-memory `cfg` may still be mutated, but no disk
write happens.

---

## 6. Persistence interaction

`nightme config` reads and writes the same file as `nightme run`,
`nightme agents`, etc. (`config.DefaultPath()`).

If a user later hand-edits `config.yaml` to add a new `agents:`
entry, the next `nightme config` will show that entry in the
merged list (with Source="config").

---

## 7. Test strategy

### 7.1 Unit

- `MergeAgents` with empty cfg → only builtins, sorted, Source="builtin"
- `MergeAgents` with new name → appended, Source="config"
- `MergeAgents` with collision → builtin overridden, no duplicate, Source="config"

### 7.2 Integration

- Full menu loop: pick index → save → reload → assert primary updated
- Cancel with `q` → no save, cfg unchanged
- Top-level `q` → exits cleanly

### 7.3 Manual

- `nightme config` in real shell with real agents installed
- Verify the menu looks right (column widths, ordering)
- Verify the save persists across `nightme run` invocations

---

## 8. Out of scope (v1.2 / F-30)

- **Other submenus** (feishu / session / logging / paths) — future F-XX
- **Detection of binary presence** — user picks from registered list only;
  binary-not-found surfaces later as a spawn error (existing v1.1 path)
- **Bubbletea TUI** — current implementation is plain stdin/stdout;
  upgrade to a full TUI later if needed
- **Multi-select** (e.g. pick multiple favorites) — pick-one is sufficient
  for "global primary" semantics

---

## 9. Open questions (draft)

- **Q-A**: Yaml schema is `primary` (top-level scalar) + `agents:` (top-level list).
  Currently no nesting. If future versions want to group agents under a
  sub-key, this changes. (Lean: keep flat; matches user-stated example.)
- **Q-B**: Should `nightme config` also show the current Feishu config
  status (configured / not configured) without editing? (Lean: yes, but
  in a future submenu, not Agents.)
- **Q-C**: Should selection remember the last chosen index across
  invocations? (Lean: no — fresh list each time, easy mental model.)

---

## 10. Change log

- **2026-08-02** — v1.2 first implementation: top-level `primary` + `agents:`
  list schema; `MergeAgents` (builtin + cfg, cfg overrides); `nightme config`
  interactive menu with Agents submenu. 7 unit tests, all green.