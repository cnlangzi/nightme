# F-tray: System Tray Icon for the NightMe Daemon

> **Status**: implemented (single branch, no separate PR per direction)
> **Target branch**: `feat-statustask`
> **Library**: [`github.com/getlantern/systray`](https://github.com/getlantern/systray) v1.2.2

---

## 1. What this is

When the user runs `nightme start`, the forked `_daemon` process
installs a system-tray (notification-area / menu-bar) icon on the
user's desktop. The icon carries a menu that mirrors the cobra
subcommand tree and the REPL `Common:` list, with three primary
items that are most useful as click actions:

```
NightMe (icon)
├─ ⓘ Status: running · feishu                  [disabled info row]
├─ ──────────────
├─ ▶  Open REPL         (spawn a new terminal)
├─ ⟳  Restart           (v1 = stop; see §6)
├─ ■  Stop              (SIGTERM → graceful shutdown)
├─ ──────────────
├─ ▸  Commands          (submenu; mirrors REPL banner order)
│     ├─ list
│     ├─ kill
│     ├─ agents
│     ├─ name
│     ├─ clean
│     └─ version
├─ ──────────────
└─ ✕  Quit              (Stop + release tray)
```

Hidden cobra commands (`_daemon`) and TTY-bound commands
(`test`, `login`, `logs`, `config`, `update`, `debug`) are
filtered out by `cmdRegistry.addNoTray` so the menu never
carries a click that would either block the event loop (PTY,
log tail, interactive prompts) or read stdin (`debug`).
Lifecycle commands (`start`, `stop`, `restart`, `status`)
also use `addNoTray` because the menu has its own primary
items for the same verbs; the submenu would invite a
double-click race.

---

## 2. Threading model flip

`runtime.Runner.Run` is blocking. `systray.Run` is blocking
on a per-OS native event loop (and on macOS must run on the
main thread — Cocoa's NSApp). The two cannot share a
goroutine.

`runTrayOwning` (cmd/nightme/tray.go) flips the model:

- The daemon runtime runs in a background goroutine.
- `systray.Run` occupies the calling thread (main thread
  on macOS where cobra dispatches the `_daemon` subcommand).
- The two exit paths are coupled by a single shared error
  variable and `systray.Quit()`. The watcher goroutine that
  sees the runtime return calls `systray.Quit`; the
  `systray.Run` block returns, the function reads the
  runtime's exit code, and `runDaemonChild` returns it as
  its own error.

`runDaemonChild` in `daemon_lifecycle_{unix,windows}.go`
now invokes `runTrayOwning` in place of `runRunWith`. On
Linux/Windows the flip is cosmetic (systray can run on any
thread); on macOS it is mandatory (Cocoa refuses otherwise).

---

## 3. Click dispatch and debounce

Each click handler runs in systray's event goroutine, NOT
the main thread. Handlers must not block the event loop:

- **Open REPL** dispatches to `openrepl.Open()` (see §4)
  in a goroutine and returns immediately.
- **Restart** / **Stop** / **Quit** invoke a callback
  passed in via `trayOptions`. Production default for all
  three is `onStopRequestDefault`, which sends SIGTERM to
  the current process. The runtime's signal channel picks
  it up; graceful shutdown runs; the runtime returns; the
  watcher goroutine calls `systray.Quit`; `systray.Run`
  returns. See §6 for why v1 merges Restart with Stop.
- **Commands** submenu items invoke
  `internal/tray.Invoke(item.Command)` in a goroutine.
  `Invoke` re-dispatches a `*cobra.Command` synchronously
  via `cmd.RunE(cmd, nil)`, redirecting stdout/stderr to
  `io.Discard` and restoring them afterwards.

A 500ms debounce window (`trayDebounce` in
`cmd/nightme/tray.go`) drops the second click on the same
primary item. macOS touchpads in particular can register
3-4 clicks from a single user gesture; without the
debouncer, a single user action would spawn 3-4 REPL
windows or send 3-4 SIGTERMs. `openrepl` has its own
1-second debouncer for the same reason on its own click.

---

## 4. Open REPL (`internal/tray/openrepl`)

Spawns a fresh terminal window that runs `nightme` (bare
invocation → REPL mode).

| OS | Recipe |
|----|--------|
| macOS | AppleScript-driven Terminal.app, or iTerm2 if installed. iTerm2 is checked first because it supports `create window with default profile command`. Last-resort: `open -a Terminal` via Launch Services for machines where AppleScript is locked down. |
| Linux | Probe `gnome-terminal / konsole / alacritty / kitty / xfce4-terminal / lxterminal / mate-terminal / xterm / foot / wezterm` in preference order; first one on `$PATH` wins. |
| Windows | `cmd /c start "NightMe REPL" cmd /k nightme.exe`. Routed through `exec.Command` directly (NOT `proc.New`) because the user is asking for a visible terminal and `CREATE_NO_WINDOW` would defeat that. |

The package is its own module so the per-OS spawn recipe is
unit-testable without dragging in `cmd/nightme` or the
systray library. Three tests cover the cross-platform shape:
the second `Open()` within the 1-second window returns nil
(debouncer drops it); a manual `ResetDebouncer` lets the
next call reach the open path; the debounce window is
exactly 1 second (regression guard for the macOS-touchpad
rationale in the package doc).

---

## 5. Resources

All tray assets live under `cmd/nightme/assets/`, consistent
with the existing `logo.ico` / `manifest.xml` / `winres.json`
layout established by commit `cbb30ca`. `cmd/nightme/tray_assets.go`
embeds them via `//go:embed` and exposes per-OS accessors:

| Platform | Setter | Source asset | Notes |
|----------|--------|--------------|-------|
| macOS    | `systray.SetTemplateIcon(1x, 2x, 3x PNGs)` | `trayTemplate{,_@2x,_@3x}.png` | Alpha-only black mask. Cocoa auto-inverts for dark mode. |
| Linux    | `systray.SetIcon(64x64 RGBA PNG)`          | `tray.png`                          | AppIndicator re-rasterises as needed. |
| Windows  | `systray.SetIcon(32x32 ICO)`                | `logo-32.ico`                       | Matches the PE-embedded icon from `go-winres`. |

`make tray-assets` generates the macOS `.icns` and the
Linux PNG:

- On macOS: `sips` resamples `logo.png` to 22/44/66 and
  `iconutil` packs the three PNGs into `trayTemplate.icns`.
- On Linux: `convert` (ImageMagick) does the same for the
  full-color tray PNG and the alpha-only template PNGs.
- On Windows: no-op. `logo.ico` is already produced by the
  `winres` target.

The three macOS template PNGs are committed to the repo
(539 / 1003 / 1382 bytes) so the build does not require a
macOS dev box. They are ImageMagick-generated alpha
masks — a macOS dev who wants sharper anti-aliasing runs
`make tray-assets` (with `GOOS=darwin`) on macOS to
regenerate via `sips`+`iconutil`.

---

## 6. Restart semantics (v1)

`onRestartRequestDefault` is identical to
`onStopRequestDefault` in v1: both send SIGTERM to self.
The tooltip on the menu item tells the user to re-run
`nightme start` to bring a fresh daemon up.

The "real" restart — where the current daemon process
spawns its own replacement and the daemon lock handoff
is atomic — is deferred. It requires a parent-or-supervisor
contract that nightme does not yet have; the lock handoff
is currently the `nightme start` path's job, not the
running daemon's, and re-implementing it mid-flight is
racy. A follow-up PR can add a `restart` IPC command and a
`launchd` / `systemd` supervisor hook for true zero-touch
restart.

This v1 simplification is **not** documented in the
`OnRestartRequest` function name to avoid surprising the
caller; the menu tooltip is the user-visible contract.

---

## 7. CI

`ci.yml` adds a Linux-only step to the `test` job:

```yaml
- name: Install systray build deps (Linux)
  if: matrix.os == 'ubuntu-latest'
  shell: bash
  run: |
    sudo apt-get update
    sudo apt-get install -y gcc libgtk-3-dev libayatana-appindicator3-dev
```

`release.yml` already runs each target on its native
runner; the same `apt-get` install is added to the
ubuntu-latest `build` job in a follow-up if/when the
release matrix pulls from a base image that lacks the dev
headers (most current `ubuntu-latest` images do not).

macOS and Windows runners ship Cocoa and Win32 by
default; the systray CGo build for those platforms
requires no extra install.

---

## 8. macOS .app bundle (optional)

A bare `nightme` binary on macOS works, but the menu-bar
icon falls back to the generic executable glyph. For the
proper icon, build the .app bundle:

```bash
make build              # bin/nightme + cmd/nightme/assets/trayTemplate.icns
make app-bundle         # dist/NightMe.app
cp -R dist/NightMe.app /Applications/
open /Applications/NightMe.app
```

`scripts/NightMe/Info.plist` (committed) declares:

- `CFBundleExecutable = nightme`
- `CFBundleIconFile = AppIcon` (resolves to
  `trayTemplate.icns` after `make app-bundle`)
- `LSUIElement = true` (no Dock entry)
- `NSHighResolutionCapable = true` (Retina-sharp)
- `NSAppleScriptEnabled = true` (so the Open REPL click
  can drive Terminal.app / iTerm2 via osascript)

The .app is **not** shipped in release artifacts. The
release tar.gz carries the bare `nightme` binary, which is
the right payload for `brew install` / `npm install -g`
packaging. The .app is a GUI-only convenience built
locally.

---

## 9. Known limitations / future work

- **Restart**: v1 merges with Stop. See §6.
- **macOS main thread**: `systray.Run` works correctly
  only when invoked on the main thread on macOS. The
  current threading-model flip puts the daemon's `_daemon`
  subcommand body on the main thread (cobra dispatches
  there from `main()`), so this is satisfied. If a future
  refactor moves the daemon off the main thread, the
  tray init must be marshalled back.
- **Icon hot-reload**: dark/light mode on macOS auto-inverts
  via `setTemplate:`, but Linux distros with custom panel
  themes do not. A future "follow the system theme" feature
  would re-render the alpha mask; not in scope for v1.
- **Multiple daemons**: a single user with two
  `NIGHTME_DATA_DIR` daemons would get two tray icons.
  Click ambiguity (which daemon did the click fire against)
  is left to the OS; the Status info row disambiguates
  via the channel name and PID.
