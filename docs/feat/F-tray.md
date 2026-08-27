# F-tray: System Tray Icon for the NightMe Daemon

> **Status**: implemented (single branch, no separate PR per direction)
> **Target branch**: `feat-statustask`
> **Library**: [`github.com/getlantern/systray`](https://github.com/getlantern/systray) v1.2.2

---

## 1. What this is

When the user runs `nightme start`, the forked `_daemon` process
installs a system-tray (notification-area / menu-bar) icon on the
user's desktop.

> **Platform availability.** The tray is always built in on macOS
> and Windows. On **Linux it is opt-in** via `-tags gui` and absent
> from the default build — see §7 for why. Everything below
> describes the tray-enabled build.

The icon carries a menu that mirrors the cobra
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

## 7. Linux: the `gui` build tag

The tray is **opt-in on Linux** and always-on elsewhere. This is
the fix for [#295](https://github.com/cnlangzi/nightme/issues/295).

`getlantern/systray` on Linux resolves to
`systray_linux_ayatana.go`, which carries

```go
#cgo linux pkg-config: ayatana-appindicator3-0.1
```

so *importing the package at all* makes the linker record
`libayatana-appindicator3.so.1`, `libgtk-3.so.0` and ~70 other
`DT_NEEDED` entries. When `cmd/nightme/tray.go` did that
unconditionally, every Linux binary — including `nightme --version`
on a headless server — died before `main()`:

```
error while loading shared libraries: libayatana-appindicator3.so.1:
cannot open shared object file: No such file or directory
```

Two properties of that failure drove the design:

1. **`isHeadless()` cannot help.** It is a runtime check
   (`tray_headless_linux.go`); ld.so fails first, so no Go code
   ever runs. The headless guard solves a different problem — GTK
   "cannot open display" once the libraries *are* present.
2. **`CGO_ENABLED=0` alone cannot help.** systray has no non-CGo
   Linux fallback; the package itself fails to compile
   (`undefined: nativeLoop`, `registerSystray`, …). The import has
   to be excluded at the source level.

### File layout

| File | Build tag | Role |
| --- | --- | --- |
| `tray.go` | *(none)* | `trayDebounce`, `clickTracker`, `trayOptions`, `logClickErr` — nothing that touches systray |
| `tray_gui.go` | `darwin \|\| windows \|\| (linux && gui)` | the real implementation: `runTrayOwning`, `trayOnReady`, click helpers |
| `tray_stub.go` | `linux && !gui` | `runTrayOwning` → straight to `runRunWith` |
| `tray_icon_linux.go` | `linux && gui` | `applyIcon` (imports systray, so it must match `tray_gui.go`) |

`runTrayOwning` is the only cross-file seam, so
`daemon_lifecycle_{unix,windows}.go` need no build-tag awareness.
`trayOptions` lives in the untagged file because both
implementations share the signature.

Resulting defaults:

| Command | Linux | macOS / Windows |
| --- | --- | --- |
| `go build` / `make build` | no tray, no CGo | tray |
| `go build -tags gui` / `make build-gui` | tray | *(n/a — already on)* |

Linux defaults to off because Linux hosts are overwhelmingly
servers. A useful side effect: `go install
github.com/cnlangzi/nightme/cmd/nightme` now produces a working
binary on a bare Linux box, which it previously did not.

### Release artifacts

`release.yml` builds both variants on the Linux runners:

```
nightme_<v>_linux_<arch>.tar.gz       CGO_ENABLED=0 make build → static, no tray
nightme_<v>_linux_<arch>-gui.tar.gz   make build-gui           → tray, needs GTK3
```

The default archive keeps its pre-#295 name so `install.sh` needs
no change and existing users are unaffected. The `-gui` archive's
inner binary is also named `nightme` so it can be dropped straight
over an existing install.

`linux/arm64` builds on the native `ubuntu-24.04-arm` runner rather
than cross-compiling: the `-gui` variant is CGo and would otherwise
need an aarch64 sysroot with arm64 GTK3 dev packages. This also
fixed a latent bug where the arm64 build step ran on an amd64
runner with no `GOOS`/`GOARCH` set, publishing an amd64 binary
under the `linux_arm64` name.

---

## 8. CI

`ci.yml`'s `test` job installs the dev headers (Linux only) so it
can compile the `-gui` variant:

```yaml
- name: Install systray build deps (Linux)
  if: matrix.os == 'ubuntu-latest'
  shell: bash
  run: |
    sudo apt-get update
    sudo apt-get install -y gcc libgtk-3-dev libayatana-appindicator3-dev
```

The `startup` job deliberately does **not** install them — it only
runs `make build`, which no longer needs GTK at all.

`release.yml` installs the same packages on both Linux runners,
conditioned on `matrix.goos == 'linux'` rather than
`matrix.os == 'ubuntu-latest'` — the latter would silently skip
`ubuntu-24.04-arm` and break the arm64 `-gui` link step.

### The #295 regression gate

A Go test cannot catch this class of bug (the failure is in ld.so,
before `main()`), so the gate inspects the linked binary. Four
assertions, because a bare "does not link GTK" check passes
vacuously if the tray silently stops building:

1. default build links no GTK/AppIndicator
2. `CGO_ENABLED=0` default build is fully static
3. `-tags gui` still *compiles* — it is otherwise only built on tag
   push, so it would rot unnoticed until release day
4. `-tags gui` actually *does* link AppIndicator, proving the tag
   is still wired to the tray and that (1) is meaningful

The release workflow repeats assertions 1 and 2 on the real
artifact before packaging, so a bad binary fails the release rather
than shipping.

macOS and Windows runners ship Cocoa and Win32 by
default; the systray CGo build for those platforms
requires no extra install.

---

## 9. macOS .app bundle (optional)

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

## 10. Known limitations / future work

- **Restart**: v1 merges with Stop. See §6.
- **Linux default has no tray**: the shipped Linux binary is built
  without `-tags gui`, so desktop Linux users only get the icon if
  they install the `-gui` release archive. Making the tray a
  runtime-optional feature instead (dlopen the AppIndicator
  library, fall back silently when absent) would give one universal
  Linux binary, but needs either a CGo `dlopen` shim or a pure-Go
  StatusNotifierItem D-Bus client in place of
  `getlantern/systray`. Out of scope for the #295 fix.
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
