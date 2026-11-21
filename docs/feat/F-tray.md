# F-tray: System Tray Icon for the NightMe Daemon

> **Status**: implemented (single branch, no separate PR per direction)
> **Target branch**: `feat-statustask`
> **Library**: [`github.com/getlantern/systray`](https://github.com/getlantern/systray) v1.2.2

---

## 1. What this is

When the user runs `nightme start`, the forked `_daemon` process
installs a system-tray (notification-area / menu-bar) icon on the
user's desktop.

> **Platform availability.** The tray is built in on macOS and
> Windows. On **Linux it is opt-in** via `-tags gui` and absent
> from the default build — see §7 for why. The `darwin/amd64`
> release artifact is built with `-tags notray` (CGo is disabled on
> that cross-compile), so it carries no tray either. Everything
> below describes the tray-enabled build.

The menu is hand-grouped in `cmd/nightme/tray_gui.go` — the
sub-command groups are not a mirror of the cobra tree:

```
NightMe (icon)
├─ 🟢  NightMe[<name>] v<version> is running  [disabled info row]
├─ ──────────────
├─ >_  Console                (spawn a terminal running `nightme`)
├─ ──────────────
├─ Logs >       logs / clean
├─ Sessions >   list / kill
├─ Config…
├─ About >      agents / doctor
├─ ──────────────
├─ ⓘ  Download Update        (hidden unless a newer release exists)
├─ ↻  Restart
└─ ⏻  Quit
```

The status row carries the daemon's configured instance name
(`config.EffectiveName`) and the compiled-in version, so a user with
two daemons can tell them apart at a glance.

Every item in the `Logs` / `Sessions` / `About` submenus, plus
`Console` and `Config…`, opens a **real terminal window** running
the matching subcommand (`proc.OpenTerminal(ctx, "nightme",
"<cliTitle>")`). A terminal is used rather than an in-process
dispatch because these commands are interactive — they tail a log,
they read a table, they open a menu.

`cmdRegistry.addNoTray` keeps the TTY-bound commands (`test`,
`login`, `logs`, `me`, `workflow`) and the lifecycle commands
(`start`, `status`, `stop`, `restart`) out of the REPL banner's
tray twin, so the tray never carries a click that would either
block the event loop (PTY, log tail, interactive prompts) or
duplicate a primary item. `config` and `update` are ordinary
`reg.add()` commands and are reachable from the tray.

---

## 2. Threading model flip

`runtime.Runner.Run` is blocking. `systray.Run` is blocking
on a per-OS native event loop (and on macOS must run on the
main thread — Cocoa's NSApp). The two cannot share a
goroutine.

`runTrayOwning` (cmd/nightme/tray_gui.go) flips the model:

- The daemon runtime runs in a background goroutine.
- `systray.Run` occupies the calling thread (main thread
  on macOS where cobra dispatches the `_daemon` subcommand).
- The two exit paths are coupled by a single shared error
  channel and `systray.Quit()`. The watcher goroutine that
  sees the runtime return calls `systray.Quit`; the
  `systray.Run` block returns, the function reads the
  runtime's exit code, and `runDaemonChild` returns it as
  its own error.

`runDaemonChild` in `daemon_lifecycle_{unix,windows}.go`
invokes `runTrayOwning` in place of `runRunWith`. On
Linux/Windows the flip is cosmetic (systray can run on any
thread); on macOS it is mandatory (Cocoa refuses otherwise).

`runTrayOwning` short-circuits to `runRunWith` when
`isHeadless()` reports no GUI session (`tray_headless_linux.go`
on Linux, `tray_headless_other.go` elsewhere) — GTK cannot open a
display, so the tray is skipped instead of crashing the daemon.
A `recover()` around `systray.Run` catches the cases the
detection misses (X server dies mid-run, partial init panic) and
leaves the daemon running without the tray UI.

---

## 3. Click dispatch and debounce

Each click handler runs in systray's event goroutine, NOT
the main thread. Handlers must not block the event loop:

- **Console** / **Config…** / the sub-menu items call
  `proc.OpenTerminal` in a goroutine and return immediately.
- **Restart** / **Quit** invoke a callback passed in via
  `trayOptions`. The Unix default for Quit is `onStopRequestDefault`
  (SIGTERM to the current process); the Unix default for Restart
  is `onRestartRequestDefault` (see §6). The runtime's signal
  channel picks the stop up; graceful shutdown runs; the runtime
  returns; the watcher goroutine calls `systray.Quit`;
  `systray.Run` returns.
- **Download Update** opens a terminal running `nightme update`.

A 500ms debounce window (`trayDebounce` in `cmd/nightme/tray.go`)
drops the second click on the same menu item. macOS touchpads in
particular can register 3-4 clicks from a single user gesture;
without the debouncer, a single user action would spawn 3-4
terminal windows or send 3-4 SIGTERMs. Each item carries its own
`clickTracker`, so a click on one item never suppresses a click on
another.

---

## 4. Opening a terminal (`internal/proc`)

`proc.OpenTerminal(ctx, name, args...)` spawns a fresh terminal
window that runs the nightme binary with the given arguments.
The binary is resolved through `os.Executable()`, not
`exec.LookPath`, so a `go install`-style install outside `$PATH`
works.

| OS | Recipe |
|----|--------|
| macOS | AppleScript-driven Terminal.app (`do script`), then maximise the window (`set zoomed of front window to true`), activate Terminal and bring it to the front via System Events. Terminal.app only — iTerm2 is not tried: its `create window with default profile command` does not hand the string to a shell but tokenizes it on whitespace, so a command like `nightme kill` arrives as the argument `kill;`. The spawned shell command is suffixed with `; echo ; printf 'press enter to close\n' ; read dummy` so the window survives the command's exit. |
| Linux | Probe `gnome-terminal / konsole / alacritty / kitty / xfce4-terminal / lxterminal / mate-terminal / xterm / foot / wezterm` in preference order (`proc.LinuxTerminalProbes`); the first one on `$PATH` wins. Each probe receives `sh -c "<command>"`, and the keep-open suffix is appended. |
| Windows | `cmd /c start "<title>" cmd /k <nightme.exe> <args…>`. Routed through `proc.NewWith` with `HideWindow: false` — the user is asking for a visible terminal and `CREATE_NO_WINDOW` would defeat that. |

The per-OS recipes live in `internal/proc/exec_unix.go`
(`openTerminalMac` / `openTerminalLinux`) and
`internal/proc/exec_windows.go`, so they are unit-testable without
dragging in `cmd/nightme` or the systray library. The launcher
process is reaped in a goroutine and the call returns as soon as
the launch succeeded.

---

## 5. Resources

All tray assets live under `cmd/nightme/assets/`, consistent
with the existing `logo.ico` / `manifest.xml` / `winres.json`
layout established by commit `cbb30ca`. `cmd/nightme/tray_assets.go`
embeds them via `//go:embed` and exposes per-OS accessors:

| Platform | Setter | Source asset | Notes |
|----------|--------|--------------|-------|
| macOS    | `systray.SetTemplateIcon(template, regular)` | `trayTemplate.png` (template) + `tray.png` (regular) | Alpha-only black mask; Cocoa auto-inverts for dark mode. The API takes no multi-resolution input, so the `@2x` / `@3x` PNGs are kept as regenerable sources rather than fed to systray. |
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
(1391 / 3578 / 6091 bytes) so the build does not require a
macOS dev box. They are ImageMagick-generated alpha
masks — a macOS dev who wants sharper anti-aliasing runs
`make tray-assets` (with `GOOS=darwin`) on macOS to
regenerate via `sips`+`iconutil`.

---

## 6. Restart semantics

**Unix** (`cmd/nightme/daemon_lifecycle_tray_unix.go`):

- `onStopRequestDefault` sends `SIGTERM` to the current process.
  The runtime's signal channel picks it up and runs the graceful
  shutdown path. If the signal itself fails, the handler exits the
  process directly so the user is never left with an icon they
  cannot stop.
- `onRestartRequestDefault` spawns a **detached `nightme restart`
  child** and returns. It does not send the stop signal itself:
  the spawned process performs the whole stop+start protocol over
  the daemoncontrol socket, and signalling here as well would race
  the spawn. A bare `_daemon` child would exit immediately — the
  daemon child requires the `NIGHTME_DAEMON_LOCK_FD` /
  `NIGHTME_READY_FD` plumbing that only `nightme start` sets up —
  so the tray restart would silently degrade to a plain stop.

**Windows** (`cmd/nightme/daemon_lifecycle_tray_windows.go`):

- Quit exits the daemon with `os.Exit(0)`.
- Restart launches a separate `nightme restart` process without a
  console window. That process owns the stop/start handoff and waits
  for the replacement daemon to become ready.
- If resolving the executable or launching the restart command fails,
  the daemon logs the error and stays running. Once launched, the
  command writes errors to `<DataDir>/daemon-stderr.log` or the path
  set by `NIGHTME_STDERR_FILE`, if capture is available.

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
`DT_NEEDED` entries. When `cmd/nightme/tray_gui.go` did that
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
| `tray_assets.go` | *(none)* | `//go:embed` of the icon payloads + `IconWindows` / `IconDarwin` / `IconLinux` |
| `tray_gui.go` | `(darwin && !notray) \|\| windows \|\| (linux && gui)` | the real implementation: `runTrayOwning`, `trayOnReady`, `handleClick`, `addTerminalSubItem`, `decorateUpdateItem` |
| `tray_stub.go` | `linux && !gui` | `runTrayOwning` → straight to `runRunWith` |
| `tray_stub_darwin.go` | `darwin && notray` | `runTrayOwning` → straight to `runRunWith` |
| `tray_icon_darwin.go` | `darwin && !notray` | `applyIcon` (imports systray, so it must match `tray_gui.go`) |
| `tray_icon_linux.go` | `linux && gui` | `applyIcon` (imports systray, so it must match `tray_gui.go`) |
| `tray_icon_windows.go` | `windows` | `applyIcon` |
| `tray_headless_linux.go` | `linux` | `isHeadless` — X / Wayland session detection |
| `tray_headless_other.go` | `!linux` | `isHeadless` — always false |

The click callbacks live next to the daemon lifecycle that owns
them: `cmd/nightme/daemon_lifecycle_tray_unix.go` (`!windows`) and
`cmd/nightme/daemon_lifecycle_tray_windows.go` (`windows`).

`runTrayOwning` is the only cross-file seam, so
`daemon_lifecycle_{unix,windows}.go` need no build-tag awareness.
`trayOptions` lives in the untagged file because both
implementations share the signature.

The `notray` tag is a **darwin-only** opt-out. It exists because
macos-latest (Apple Silicon) cannot cross-compile CGo to
darwin/amd64: Go disables CGo on cross-compile and systray has no
non-CGo fallback. Setting `-tags notray` on Windows or Linux is a
no-op — the tray stays compiled in there, and the Windows
cross-compile problem is solved by the native `windows-11-arm`
runner instead.

Resulting defaults:

| Command | Linux | macOS / Windows |
| --- | --- | --- |
| `go build` / `make build` | no tray, no CGo | tray |
| `go build -tags gui` / `make build-gui` | tray | *(n/a — already on)* |
| `go build -tags notray` | *(n/a)* | no tray (darwin only) |

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
requires no extra install. The `darwin/amd64` release matrix entry
carries `go_tags: notray` and `cgo_enabled: "0"`, so that
cross-compiled artifact links cleanly without the tray.

---

## 9. macOS .app bundle (optional)

A bare `nightme` binary on macOS works, but the menu-bar
icon falls back to the generic executable glyph. For the
proper icon, build the .app bundle:

```bash
make tray-assets      # cmd/nightme/assets/trayTemplate.icns
make build            # bin/nightme
make app-bundle       # dist/NightMe.app
cp -R dist/NightMe.app /Applications/
open /Applications/NightMe.app
```

`scripts/NightMe/Info.plist` (committed) declares:

- `CFBundleExecutable = nightme`
- `CFBundleIconFile = AppIcon` (resolves to
  `trayTemplate.icns` after `make app-bundle`)
- `LSUIElement = true` (no Dock entry)
- `NSHighResolutionCapable = true` (Retina-sharp)
- `NSAppleScriptEnabled = true` (so the Console click
  can drive Terminal.app via osascript)

The .app is **not** shipped in release artifacts. The
release tar.gz carries the bare `nightme` binary, which
is the right payload for `brew install` / `npm install -g`
packaging. The .app is a GUI-only convenience built
locally.

---

## 10. Known limitations / future work

- **Linux default has no tray**: the shipped Linux binary is built
  without `-tags gui`, so desktop Linux users only get the icon if
  they install the `-gui` release archive. Making the tray a
  runtime-optional feature instead (dlopen the AppIndicator
  library, fall back silently when absent) would give one universal
  Linux binary, but needs either a CGo `dlopen` shim or a pure-Go
  StatusNotifierItem D-Bus client in place of
  `getlantern/systray`. Out of scope for the #295 fix.
- **darwin/amd64 has no tray**: the release artifact for that
  platform is built `-tags notray`, because the cross-compile
  cannot produce a CGo systray binary.
- **Windows Restart is a stop**: `onRestartRequestDefault` on
  Windows is the stop path. See §6.
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
  via the instance name and the version.
