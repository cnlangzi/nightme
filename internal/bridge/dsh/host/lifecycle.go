// lifecycle.go — process management for the shared dsh web daemon.
//
// In the shared-host architecture (F-dsh-shared-host), exactly ONE
// `dsh --profile web` subprocess serves every ChatSession. This
// file owns the lifecycle; the rest of the package (client.go,
// stream.go, router.go) talks to dsh over HTTP + WebSocket.
//
// dsh 0.1.2-rc.1 enforces per-process signed-cookie auth on
// /api/* and /api/events.* (see
// @deepseek-ai/dsh-client-connection/lib/index.js::BrowserAuth).
// The signing secret is persisted to
// `~/.dsh/.credentials.yaml` (record `client-connection/browser-session`,
// payload.secret). Since the secret is per-`.dsh/` directory and
// every dsh started by nightme uses the same directory, nightme
// can MINT the dsh-auth cookie locally using that secret — no
// launch-token exchange required. The cookie validates against any
// dsh process on this host that's loaded the same secret, which
// means nightme can attach to a still-running dsh on restart
// without re-spawning or persisting anything of its own.
//
// Lifecycle model (sign-cookie-then-spawn):
//
//	StartSharedHost(ctx, opts)
//	  1. Mint a dsh-auth cookie using the signing secret loaded
//	     from ~/.dsh/.credentials.yaml (via mintDSHAuthCookie).
//	     No network round-trip to dsh — the algorithm is a pure
//	     HMAC-SHA256 over a base64url-encoded payload.
//	  2. Construct *Client with the minted cookie jar. The jar is
//	     the same shape dsh itself emits, so every /api/* and WS
//	     upgrade carries the cookie.
//	  3. Pick a port (3080 default, fallback sweep [3081, 3099]
//	     if 3080 is held by a non-dsh service).
//	  4. spawnAndWire spawns `dsh --profile web --port <port>`,
//	     dials /api/remote.mux, and the Hub's auth cookie is
//	     already in place.
//	  5. Install via SetGlobal so dsh.newDriver can find it.
//	     Start the watchdog.
//
//	ShutdownSharedHost(ctx, client)
//	  1. Client.Close (stops mux/host pumps)
//	  2. SIGINT dsh, wait 5s, SIGKILL, wait 5s
package host

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cnlangzi/nightme/internal/proc"
)

// webURLParseTimeout bounds waiting for dsh web to print its bound
// URL on stdout. Real-machine cold start is ~1.5s; environments
// that export SOCKS proxy vars (all_proxy / ALL_PROXY) push dsh's
// startup measurably higher because dsh refuses SOCKS and runs a
// fallback path. 30s is generous even on those hosts and gives the
// workspace-init scan enough room without bumping into a false
// timeout that loses the stderr diagnostic on /review failures.
const webURLParseTimeout = 30 * time.Second

// stderrCaptureCap bounds the ring of recent stderr lines the
// parseWebURL failure path attaches to its error chain. 64 lines
// is enough to surface dsh's plugin / profile errors without
// unbounded growth on a runaway process.
const stderrCaptureCap = 64

// stderrFlushGrace gives the stderr drain goroutine a brief
// window to flush whatever dsh was mid-writing when the URL line
// failed to appear, BEFORE we SIGKILL the subprocess. 200ms keeps
// the user-visible error latency negligible while still letting
// one line at typical pipe speeds reach our ring.
const stderrFlushGrace = 200 * time.Millisecond

// Probe tuning for the monitor's attached-mode branch. The
// monitor probes /api/session.list at defaultAttachedProbeInterval
// and on defaultAttachedProbeStrikes consecutive failures
// considers the dsh dead, transitions the host from attached
// to owned by fallback-spawning a fresh dsh, and then runs the
// owned-mode branch. 30s × 3 = 90s detection latency is the
// production default; tests override via h.testHooks.
//
// Attached → owned transition is one-way: once the monitor
// promotes the host, it stays in owned mode for the lifetime
// of the daemon. Attached mode is only the boot-time shape
// (tryAttachExistingDSH succeeded at StartSharedHost); if the
// attached dsh later dies, we take over and never look back.
const (
	defaultAttachedProbeInterval = 30 * time.Second
	defaultAttachedProbeStrikes  = 3
	defaultAttachedProbeTimeout  = 5 * time.Second
)

// dshReadyTimeout / dshReadyAttempts bound the post-cookie
// readiness probe in spawnAndWire. The probe is per-call
// (workspace.list) so per-attempt wall time is small; the
// timeout is the cap. With respawnDelay backoff (0, 1s, 2s, ...)
// 5 attempts sums to ~3s plus per-attempt RPC time — well
// within a 15s budget on a healthy machine. Tuned in 2026-09
// after observing the workspaceController startup race where
// dsh's HTTP server is up but the plugin registry is still
// loading — without this probe, the first workspace.create
// after spawn returns "active Service workspaceController is
// unavailable" and the user sees a startup-race error.
const (
	dshReadyTimeout  = 15 * time.Second
	dshReadyAttempts = 5
)

// SharedHostOptions configures StartSharedHost.
type SharedHostOptions struct {
	// Workspace is the dsh process's working directory. dsh's bash /
	// fs plugins read process.cwd() set via cmd.Dir. Required.
	Workspace string

	// HostCmd is the dsh binary name or absolute path. Defaults to
	// "dsh" (PATH-resolved by exec.LookPath at spawn time).
	HostCmd string

	// PermissionMode is the value of DSH_PERMISSION_MODE env var
	// injected into the dsh subprocess. Per
	// [[agent-no-config-tampering]] the bridge injects only
	// transport + permissions — never model / provider /
	// credentials. Default: "danger-full-access" (matches the
	// pre-shared-host behaviour).
	PermissionMode string

	// ForceSpawn bypasses the reuse-or-spawn discovery: always
	// spawn a fresh dsh subprocess, even if 3080 has one running.
	// Used by tests (which need to drive their own fake dsh
	// subprocess) and by users who explicitly want isolation
	// (e.g. CI, multiple daemons on the same host). Default: false.
	//
	// Deprecated: with cookie-mint the spawn-vs-attach decision
	// goes away. We always spawn our own dsh; the cookie we mint
	// with the ~/.dsh/.credentials.yaml secret will be accepted by
	// any other dsh that shares that .dsh/ directory (i.e. every
	// dsh started from this user's HOME). ForceSpawn is preserved
	// only for tests that need a clean isolated dsh subprocess.
	ForceSpawn bool

	// Port is the TCP port dsh should bind to. Set by
	// StartSharedHost after the discover-or-spawn decision:
	// defaultDSHPort (3080) when 3080 was empty, or a fallback
	// from findFreePort when 3080 was occupied by a non-dsh
	// service. Captured here so the watchdog's respawn path
	// (spawnOnce) reuses the same port.
	Port int

	// Logger is the slog handle for lifecycle messages. nil → slog.Default().
	Logger *slog.Logger
}

// SharedHost wraps the running dsh host + the host.Client
// pointing at it. Use StartSharedHost to construct; the daemon
// never tears it down (dsh is a persistent service — see
// internal/bridge/dsh/host/ensure.go for the lazy-start model).
//
// SharedHost has a single lifecycle: monitor (runMonitor). The
// monitor starts in attached mode when StartSharedHost succeeded
// via tryAttachExistingDSH (cmd = nil), and switches to owned
// mode (cmd != nil) once the attached dsh dies and a fresh
// nightme-owned dsh is fallback-spawned, or immediately when
// StartSharedHost went down the spawn path.
//
// The monitor never voluntarily exits: attached-mode probe
// failures trigger an attached → owned fallback that retries
// indefinitely; owned-mode cmd.Wait exits trigger tryRespawn
// that retries indefinitely; respawn failures back off and
// retry. The only exits are:
//   - the daemon process exits (Go runtime kills the goroutine)
//   - ShutdownSharedHost cancels the host's closed channel
//     (which the monitor listens to before each retry)
//
// After a successful respawn (either fallback or owned respawn),
// the monitor invokes Client.RecoverSubscriptions to re-attach
// every Router subscription on the new dsh so chat sessions
// keep streaming events.
type SharedHost struct {
	cmd    *exec.Cmd // nil while attached; non-nil once nightme owns the subprocess
	cli    *Client
	logger *slog.Logger
	opts   SharedHostOptions // captured at Start for respawn parity

	mu sync.RWMutex // guards cmd + cli swap during fallback / respawn

	// closed is closed by ShutdownSharedHost to ask the monitor to
	// exit cleanly. The monitor selects on it before each backoff
	// sleep and after each spawn attempt. Production paths never
	// close it explicitly; the Go runtime tears the goroutine down
	// when the daemon process exits.
	closed chan struct{}

	// watchdogDone is closed when the monitor goroutine has fully
	// exited. Tests wait on this to assert "monitor observed the
	// event" without sleeping on production-grade timing.
	watchdogDone chan struct{}

	// probe is the wedged-dsh detector (see health.go). It runs
	// session.list via the shared RPCClient; after strikesMax
	// consecutive failures it invokes forceKillCmd which sends
	// SIGKILL to the subprocess, unblocking the monitor's
	// waitCmd. nil until runMonitor starts the probe. Stopped
	// by ShutdownSharedHost.
	probe *HealthProbe

	// testHooks is nil in production; tests inject non-nil values
	// to drive the probe loop faster, replace the spawner, and
	// signal completion via a channel. Documented on each field
	// of testHooks below.
	testHooks *testHooks
}

// respawnBackoffBase / respawnBackoffMax bound the exponential
// backoff between respawn attempts. After a successful respawn the
// backoff resets. Total wait budget across one cycle is bounded
// by maxRespawnAttempts so a persistently-broken dsh doesn't
// stall the watchdog forever.
const (
	respawnBackoffBase = 500 * time.Millisecond
	respawnBackoffMax  = 30 * time.Second
	maxRespawnAttempts = 5
	respawnRecoverTO   = 30 * time.Second // ctx for RecoverSubscriptions
)

// Client returns the *Client the runtime should pass to bridge code.
// The caller may use this directly (RPC, Subscribe, etc.); the
// SharedHost retains ownership of the subprocess.
func (h *SharedHost) Client() *Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cli
}

// PID returns the dsh subprocess PID, or 0 if the subprocess has
// already exited. Used for `/diagnose` output and log lines.
func (h *SharedHost) PID() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.cmd == nil || h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

// StartSharedHost spawns a fresh dsh web daemon and installs the
// resulting *Client as the process-wide singleton via SetGlobal.
//
// Always-spawn contract (replaces the previous reuse-or-spawn):
//
//  1. TCP-dial 127.0.0.1:3080.
//     - dial succeeds → 3080 is occupied by SOMETHING (could be
//     another dsh or a foreign service — we treat them the
//     same). Spawn on findFreePort(3081, 3099).
//     - dial fails (refused/timeout) → 3080 is ours. Spawn there.
//  2. spawnAndWire spawns `dsh --profile web --port <port>`, parses
//     the launch token from stdout, GETs /?token=... to mint the
//     dsh-auth cookie, and constructs a Client that carries the
//     cookie jar through both RPC and WS.
//  3. SetGlobal installs the Client; watchdog respawns on crash.
//
// Why "always spawn" replaces "reuse existing": dsh 0.1.2-rc.1
// enforces per-process signed-cookie auth. The launch token is
// process-internal and never written to a file, so attaching to a
// dsh we didn't spawn gives us no way to mint a valid cookie —
// every /api/* and WS call gets 401. Reusing would only be
// reachable if dsh itself added a token-sharing mechanism.
//
// Errors are fatal-startup semantics: callers should treat them as
// hard-fail boot conditions (no per-session fallback).
func StartSharedHost(ctx context.Context, opts SharedHostOptions) (*SharedHost, error) {
	if opts.Workspace == "" {
		return nil, errors.New("dsh.host: workspace is required")
	}
	if opts.HostCmd == "" {
		opts.HostCmd = "dsh"
	}
	if opts.PermissionMode == "" {
		opts.PermissionMode = "danger-full-access"
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Step 1: try to attach to an existing dsh before spawning.
	// The cookie we mint from ~/.dsh/.credentials.yaml is accepted
	// by ANY dsh on this host that loaded the same secret — and
	// every dsh on this user account does, because they all share
	// ~/.dsh/. So if anything is already listening on 3080 (or any
	// other port dsh uses), we probe with our minted cookie and
	// reuse the running dsh. This eliminates the orphan-dsh-on-port-
	// 3081-3099 pattern that the old "spawn fallback" policy
	// created.
	attachCli, attachedPort, attached := tryAttachExistingDSH(ctx, logger, opts)
	host := &SharedHost{
		cli:          attachCli,
		logger:       logger,
		opts:         opts,
		closed:       make(chan struct{}),
		watchdogDone: make(chan struct{}),
	}
	if attached {
		// Nightme attached to a user-spawned dsh (cmd=nil). The
		// monitor starts in attached mode; on death it fallback-
		// spawns nightme's own dsh and promotes itself to owned.
		logger.Info("dsh.host: attached to existing dsh — no spawn needed",
			"port", attachedPort)
		// Remember the attached port so the monitor logs it on
		// transition events; falls back to opts.Port on fallback.
		if attachedPort != 0 {
			host.opts.Port = attachedPort
		}
	} else {
		// Step 2: cookie validate on the foreign dsh failed (or
		// none is listening on 3080). Spawn our own. nightme dsh
		// service is pinned to port 3080 — no fallback. Operators
		// running both a user dsh and nightme dsh must coordinate
		// on the port (stop the user dsh before launching nightme,
		// or run nightme on a separate machine).
		port := defaultDSHPort
		if dialReachable(defaultDSHPort) {
			return nil, fmt.Errorf(
				"dsh.host: port %d already in use; "+
					"nightme dsh service does not fall back to "+
					"alternate ports — stop the foreign listener "+
					"or move nightme to a separate machine",
				defaultDSHPort)
		}

		cmd, cli, err := spawnAndWire(ctx, opts, port, logger)
		if err != nil {
			return nil, err
		}

		logger.Info("dsh.host: web spawned",
			"pid", cmd.Process.Pid,
			"argv", cmd.Args,
			"workspace", opts.Workspace,
			"permission_mode", opts.PermissionMode,
			"port", port,
		)

		host.cmd = cmd
		host.cli = cli
		host.opts.Port = port
	}

	// Install as the process-wide singleton. dsh.newDriver will look
	// this up via GetGlobal() at every ChatSession Start.
	SetGlobal(host.cli)

	// Start the monitor. It probes the dsh (attached mode) or
	// waits on cmd.Wait (owned mode), and on death either
	// fallback-spawns or respawns — retrying until success. See
	// runMonitor for the contract.
	go host.runMonitor()

	return host, nil
}

// testHooks lets tests drive the monitor faster, replace the
// spawner, and signal completion via a channel. Production never
// sets this (h.testHooks stays nil; the monitor uses the package-
// level constants). Each field documents the production default
// it overrides.
type testHooks struct {
	// ProbeInterval overrides defaultAttachedProbeInterval when > 0.
	ProbeInterval time.Duration
	// ProbeStrikes overrides defaultAttachedProbeStrikes when > 0.
	ProbeStrikes int
	// ProbeTimeout overrides defaultAttachedProbeTimeout when > 0.
	ProbeTimeout time.Duration

	// ProbeFailure, if set, replaces the production /api/session.list
	// probe (h.probeOnce) with a deterministic outcome. Tests use this
	// to drive strike accumulation without spinning up an actual
	// dsh. nil = production probe.
	ProbeFailure func(strikes int) bool

	// Spawner, if non-nil, replaces h.spawnOnce inside the fallback
	// path. Tests use this to inject a mock spawner (returns a fake
	// *exec.Cmd / *Client without forking a real dsh) so they can
	// drive the retry + state-mutation paths without a real
	// subprocess. nil = production spawner.
	Spawner func() (*exec.Cmd, *Client, error)

	// Respawner, if non-nil, replaces h.spawnOnce inside the owned-
	// mode respawn path (tryRespawn). Same rationale as Spawner.
	// nil = production respawner.
	Respawner func() (*exec.Cmd, *Client, error)

	// ExitedProbe, if non-nil, is invoked before each waitCmd return
	// to let tests inject "dsh exited" into the owned-mode loop
	// without spawning an actual subprocess. nil = wait on cmd.Wait
	// for real.
	ExitedProbe func() (cmd *exec.Cmd, exited bool)
}

// effectiveTestHooks returns h.testHooks if non-nil, otherwise a
// zero-value struct (so the production monitor's "override only
// when > 0" pattern stays in one place).
func (h *SharedHost) effectiveTestHooks() testHooks {
	if h.testHooks != nil {
		return *h.testHooks
	}
	return testHooks{}
}

// tryAttachExistingDSH probes 3080 (then 3081-3099 in order) for
// any dsh that's already listening, builds a *Client with our
// minted cookie, and calls /api/session/list to verify the cookie
// validates. On success returns (cli, port, true); on all-probes-
// failed returns (nil, 0, false) so the caller spawns its own.
//
// Cookie mint + probe contract: dsh 0.1.2-rc.1 enforces per-process
// signed-cookie auth; the launch token is process-internal and
// never leaves the dsh process. nightme reads the
// ~/.dsh/.credentials.yaml secret and signs a cookie locally (see
// mintDSHAuthCookieFromCredentials), and any dsh that loaded the
// same ~/.dsh/ directory will accept it.
func tryAttachExistingDSH(ctx context.Context, logger *slog.Logger, opts SharedHostOptions) (*Client, int, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// nightme dsh service is pinned to 3080 — no port fallback.
	// Operators running both a user dsh and nightme dsh must
	// coordinate on the port. If port 3080 isn't reachable,
	// attach fails and StartSharedHost falls through to spawn.
	port := defaultDSHPort
	if !dialReachable(port) {
		return nil, 0, false
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	authority := strings.TrimPrefix(baseURL, "http://")
	jar, err := mintDSHAuthCookieFromCredentials(authority)
	if err != nil {
		logger.Debug("dsh.host: attach probe: mint cookie failed",
			"port", port, "err", err)
		return nil, 0, false
	}
	probe := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	// session.list is a typed POST (args._request); see
	// @deepseek-ai/dsh-api-session-controller/lib/typert.host.js.
	// Use POST + the canonical typert envelope so the gateway
	// routes on namespace = "session" + method = "list".
	body := []byte(`{"type":"client-request","rpcId":"probe","method":"session/list","payload":{"args":{"_request":{}}}}`)
	req, _ := http.NewRequestWithContext(probeCtx, http.MethodPost,
		baseURL+"/api/session/list", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := probe.Do(req)
	if err != nil {
		logger.Debug("dsh.host: attach probe: dial failed",
			"port", port, "err", err)
		return nil, 0, false
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.Debug("dsh.host: attach probe: cookie rejected",
			"port", port, "status", resp.StatusCode)
		return nil, 0, false
	}
	// Cookie validates — build the production *Client and start
	// its WS pump. We didn't spawn this dsh so cmd is nil and
	// the monitor starts in attached mode.
	cli := NewWithJar(baseURL, jar, logger)
	// Install the host waterfall handler BEFORE Start — dsh
	// sends the host $events `ready` frame immediately after
	// the WS upgrade, so the handler must be wired before we
	// dial.
	OnLifecycleInstall(cli)
	// Use the caller's long-lived ctx (StartSharedHost's), NOT
	// probeCtx (10s timeout): the Hub's WS reconnect loop has
	// to outlive the attach probe or it'll die mid-session.
	if err := cli.Start(ctx); err != nil {
		logger.Warn("dsh.host: attach probe: cli.Start failed",
			"port", port, "err", err)
		return nil, 0, false
	}
	// SetGlobal is the caller's job (StartSharedHost does it once
	// after tryAttachExistingDSH returns); we don't install the
	// client here.
	return cli, port, true
}

// runMonitor is the single unified watchdog goroutine. It
// dispatches by h.cmd:
//
//   - h.cmd == nil (attached mode): periodically probe the dsh via
//     /api/session.list. On `attachedProbeStrikes` consecutive
//     failures, run monitorAttachedFallback to spawn a fresh dsh
//     and promote the host to owned.
//
//   - h.cmd != nil (owned mode): wait on waitCmd. On exit, run
//     monitorOwnedRespawn to bring up a fresh dsh. After respawn,
//     re-attach every Router subscription via
//     Client.RecoverSubscriptions.
//
// Both branches retry indefinitely: attached → owned fallback has
// no cap (replaces the old maxAttachedFallbackAttempts=3 cap), and
// owned → owned respawn has no cap (replaces the old
// maxRespawnAttempts=5 cap). The only exits are daemon shutdown
// (ShutdownSharedHost closes h.closed) and the Go runtime killing
// the goroutine when the daemon process exits.
//
// Why retry forever: nightme has taken over the dsh host lifecycle
// end to end. The previous "give up after N attempts; user must
// `make restart`" behaviour left the host wedged on transient
// failures (pnpm updating the dsh shim, port briefly held by
// another service, etc.) and forced a manual restart for no good
// reason. The right behaviour is to keep trying — dsh's own
// failures are not something the user should have to debug at
// runtime.
func (h *SharedHost) runMonitor() {
	defer close(h.watchdogDone)

	// Health probe runs alongside the monitor: it issues
	// session.list every healthProbeInterval; after strikesMax
	// consecutive failures (event-loop frozen, plugin
	// deadlock, WS stuck) it invokes h.forceKillCmd, which
	// sends SIGKILL to the dsh subprocess. That unblocks the
	// monitor's <-waitCmd(cmd) (owned branch) or the fallback
	// probe path (attached branch with cmd==nil → no-op) and
	// lets recovery continue. Without the probe, a wedged dsh
	// blocks forever — there is no other signal that the dsh
	// process is alive-but-stuck.
	//
	// The probe uses h.Client() (which follows the cli
	// swap-on-respawn), so it automatically tracks fallback
	// and respawn transitions. Stop drains the probe
	// goroutine via HealthProbe.Done; ShutdownSharedHost calls
	// Stop as part of teardown.
	h.probe = NewHealthProbe(
		func() *Client { return h.Client() },
		h.forceKillCmd,
		h.logger,
	)
	h.probe.Start()
	defer h.probe.Stop()

	for {
		select {
		case <-h.closed:
			return
		default:
		}

		h.mu.RLock()
		cmd := h.cmd
		h.mu.RUnlock()

		if cmd == nil {
			// Attached mode: probe + (on death) fallback to owned.
			shuttingDown := h.monitorAttachedLoop()
			if shuttingDown {
				return
			}
			// Fallback succeeded. Re-read cmd: if it's nil
			// (Spawner test hook returned no subprocess —
			// nothing to monitor), exit. Otherwise loop into
			// the owned branch.
			h.mu.RLock()
			cmd = h.cmd
			h.mu.RUnlock()
			if cmd == nil {
				return
			}
			continue
		}

		// Owned mode: wait on cmd, then respawn.
		h.monitorOwnedLoop(cmd)
	}
}

// monitorAttachedLoop runs the attached-mode probe + fallback
// path. Returns shuttingDown=true when h.closed fired; returns
// shuttingDown=false after a successful fallback (the host is
// now in owned mode — caller re-reads h.cmd to decide whether
// to dispatch into the owned branch or exit if the fallback
// Spawner returned nil cmd).
//
// monitorAttachedLoop never returns because of a fallback FAILURE —
// it backs off and retries forever (with capped backoff).
func (h *SharedHost) monitorAttachedLoop() (shuttingDown bool) {
	hooks := h.effectiveTestHooks()
	interval := defaultAttachedProbeInterval
	strikes := defaultAttachedProbeStrikes
	probeTimeout := defaultAttachedProbeTimeout
	if hooks.ProbeInterval > 0 {
		interval = hooks.ProbeInterval
	}
	if hooks.ProbeStrikes > 0 {
		strikes = hooks.ProbeStrikes
	}
	if hooks.ProbeTimeout > 0 {
		probeTimeout = hooks.ProbeTimeout
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var consecutiveFailures int
	for {
		select {
		case <-h.closed:
			return true
		case <-ticker.C:
		}

		if h.probeOnce(probeTimeout, hooks.ProbeFailure) {
			consecutiveFailures = 0
			continue
		}
		consecutiveFailures++
		if consecutiveFailures < strikes {
			continue
		}

		h.logger.Warn("dsh.host: attached dsh appears dead after strikes; spawning fresh dsh",
			"strikes", consecutiveFailures,
			"port", h.opts.Port)
		err := h.monitorAttachedFallback()
		if err != nil {
			// Fallback failed; monitorAttachedFallback has
			// already backed off and retried (or h.closed
			// fired during backoff). Reset the strike counter
			// so a flapping attached dsh doesn't spam the log
			// line if it comes back online briefly between
			// failed fallbacks.
			consecutiveFailures = 0
			continue
		}

		// Fallback succeeded. Whether the Spawner returned
		// a live cmd (production path → runMonitor's owned
		// branch takes over) or nil (test hook → tests
		// inspect state and exit), monitorAttachedLoop is
		// done — the host is now in owned mode and there is
		// no more attached-mode work for this loop.
		return false
	}
}

// monitorAttachedFallback is the attached → owned transition. It
// invokes spawnOnce (or the test override) and on success:
//
//  1. swaps h.cmd/h.cli in place (h.mu held briefly around the
//     field mutations);
//  2. replaces the process-global *Client via ReplaceGlobal;
//  3. closes the OLD cli so existing drivers' Keepalive sees it
//     dead and rebuilds via spawner.Spawn (preserving the user's
//     sessionId via dsh's idempotent session.create({sessionId,
//     cwd})).
//
// On spawn failure it backs off (capped) and retries until
// success or h.closed fires.
func (h *SharedHost) monitorAttachedFallback() error {
	for {
		select {
		case <-h.closed:
			return errors.New("dsh.host: monitor closed before fallback succeeded")
		default:
		}

		cmd, cli, port, err := h.spawnAttachedOnce()
		if err != nil {
			h.logger.Error("dsh.host: fallback spawn failed; retrying after backoff",
				"err", err,
				"backoff", respawnBackoffMax)
			select {
			case <-h.closed:
				return errors.New("dsh.host: monitor closed during fallback backoff")
			case <-time.After(respawnBackoffMax):
				continue
			}
		}

		h.mu.Lock()
		oldCli := h.cli
		h.cmd = cmd
		h.cli = cli
		if port > 0 {
			// Track the actual bound port so subsequent
			// respawns (which re-enter spawnOnce) reuse it
			// rather than drifting to a new findFreePort
			// every cycle — see finding 6 in the lifecycle
			// refactor review.
			h.opts.Port = port
		}
		h.mu.Unlock()

		// Swap the process-global Client. Existing drivers still
		// hold references to oldCli; closing it fires oldCli.Done()
		// which their Keepalive loop sees as "subprocess dead" and
		// calls onRecover → spawner.Spawn → fresh *dsh.driver.
		//
		// Close is asynchronous: closing a Client drains its Hub
		// pump goroutines synchronously (pumpWG.Wait inside
		// Client.Close). Doing that on the monitor goroutine
		// would block shutdown callers (ShutdownSharedHost can't
		// drive h.closed through the monitor's backoff selects
		// until oldCli.Close returns). Run it in a background
		// goroutine so the monitor stays responsive. The Client
		// is safe to close concurrently with the new Cli's
		// subscription traffic — its Hub mux loop drains on its
		// own.
		ReplaceGlobal(cli)
		if oldCli != nil {
			go oldCli.Close()
		}

		newPID := -1
		if cmd != nil && cmd.Process != nil {
			newPID = cmd.Process.Pid
		}
		h.logger.Info("dsh.host: fallback spawn complete — entering owned mode",
			"port", h.opts.Port,
			"new_pid", newPID)
		return nil
	}
}

// spawnAttachedOnce is the attached-mode spawner. It honours the
// Spawner test hook when set; production calls h.spawnOnce
// (which honours h.opts.Port and falls back to findFreePort).
// Returns the actual port the new dsh is listening on so the
// caller can update h.opts.Port (otherwise respawn would re-check
// a stale port and drift to a different fallback port every
// cycle — see finding 6 in the lifecycle refactor review).
func (h *SharedHost) spawnAttachedOnce() (*exec.Cmd, *Client, int, error) {
	if hooks := h.effectiveTestHooks(); hooks.Spawner != nil {
		cmd, cli, err := hooks.Spawner()
		// Test hooks don't go through spawnOnce's port
		// selection, so the returned port is whatever
		// matches the mock's URL. Use port=0 (meaning
		// "don't update h.opts.Port").
		return cmd, cli, 0, err
	}
	return h.spawnOnce()
}

// spawnRespawnOnce is the owned-mode respawner. It honours the
// Respawner test hook when set; production calls h.spawnOnce.
// Returns the actual port the new dsh bound to so the caller
// can update h.opts.Port (see finding 6).
func (h *SharedHost) spawnRespawnOnce() (*exec.Cmd, *Client, int, error) {
	if hooks := h.effectiveTestHooks(); hooks.Respawner != nil {
		cmd, cli, err := hooks.Respawner()
		return cmd, cli, 0, err
	}
	return h.spawnOnce()
}

// spawnRespawnOnce is the owned-mode respawner. It honours the
// Respawner test hook when set; production calls h.spawnOnce.
// Returns the actual port the new dsh bound to so the caller
// can update h.opts.Port (see finding 6).
// probeOnce calls /api/session.list on the current client with
// a short timeout. Returns true on success (200 OK), false on any
// error. The test override (hooks.ProbeFailure) lets unit tests
// inject deterministic outcomes.
func (h *SharedHost) probeOnce(timeout time.Duration, probeFailure func(strikes int) bool) bool {
	if probeFailure != nil {
		return probeFailure(0)
	}
	h.mu.RLock()
	cli := h.cli
	h.mu.RUnlock()
	if cli == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := cli.RPC.SessionList(ctx); err != nil {
		h.logger.Debug("dsh.host: attached probe failed", "err", err)
		return false
	}
	return true
}

// monitorOwnedLoop runs the owned-mode cmd.Wait + respawn path.
// Returns when:
//
//   - h.closed fired (shutdown)
//   - the owned dsh exited and tryRespawn succeeded (host is now
//     in owned mode again; runMonitor will iterate)
//
// tryRespawn itself retries forever — see its doc comment for
// the new contract.
func (h *SharedHost) monitorOwnedLoop(cmd *exec.Cmd) {
	hooks := h.effectiveTestHooks()
	var exited bool
	if hooks.ExitedProbe != nil {
		cmd, exited = hooks.ExitedProbe()
	}
	if !exited {
		<-waitCmd(cmd)
	}

	h.logger.Error("dsh.host: subprocess exited unexpectedly; respawning")

	if err := h.tryRespawn(); err == nil {
		h.recoverSubscriptions()
	}
	// tryRespawn backs off and retries internally; if it returned
	// non-nil then h.closed fired (no other failure mode). The for
	// loop in runMonitor will dispatch into owned mode again (cmd
	// is the new one) or exit if h.closed fired.
}

// recoverSubscriptions re-attaches every Router subscription on
// the current cli. Best-effort: orphaned sessions (cwd mismatch /
// server-side reap) are logged per-session at Warn level (see
// Client.RecoverSubscriptions) but don't block the monitor.
func (h *SharedHost) recoverSubscriptions() {
	h.mu.RLock()
	cli := h.cli
	h.mu.RUnlock()
	if cli == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), respawnRecoverTO)
	defer cancel()
	result := cli.RecoverSubscriptions(ctx, h.logger)
	switch {
	case result.Reattached == 0 && len(result.Orphaned) > 0:
		h.logger.Error("dsh.host: post-respawn recovery: NO sessions reattached; all orphaned",
			"orphaned_count", len(result.Orphaned))
	default:
		h.logger.Info("dsh.host: post-respawn recovery complete",
			"reattached", result.Reattached,
			"orphaned", len(result.Orphaned))
	}
}

// ShutdownSharedHost tears down the shared dsh host so a fresh
// nightme daemon can bind the port on next start. It performs
// the same teardown the daemon SIGTERM path used to do via
// SharedHost.Close before the refactor:
//
//  1. close h.closed — signals runMonitor + HealthProbe to
//     exit (monitor returns nil errors from tryRespawn and
//     its backoff selects wake up; probe selects close).
//  2. wait for runMonitor to exit via watchdogDone (5s budget).
//     Without this the HealthProbe below could race with a
//     half-drained monitor.
//  3. stop the HealthProbe (Stop drains its goroutine via the
//     done channel). Belt-and-suspenders: probe also exits on
//     h.closed but Stop makes the order deterministic.
//  4. SIGINT the dsh subprocess, wait 5s for graceful exit.
//     Falls through to SIGKILL if cmd is still alive. Either
//     way releases the TCP port so the next start can bind.
//  5. close the live *Client (drains Hub WS pump goroutines
//     + Router state).
//  6. unset the package-level singletons so a subsequent
//     EnsureSharedHost (lazy start) re-materialises a clean
//     host instead of returning the just-shut-down one.
//
// Safe to call multiple times (idempotent on the closed
// channel); nil h is a no-op.
func ShutdownSharedHost(h *SharedHost) {
	if h == nil {
		return
	}
	select {
	case <-h.closed:
		// already closed
	default:
		close(h.closed)
	}

	// 2. wait for monitor to exit (bounded so a hung dsh
	// doesn't block the daemon shutdown indefinitely).
	if h.watchdogDone != nil {
		select {
		case <-h.watchdogDone:
		case <-time.After(5 * time.Second):
			h.logger.Warn("dsh.host: ShutdownSharedHost: monitor did not exit within 5s; continuing")
		}
	}

	// 3. stop probe (idempotent if probe was never started
	// — the Stop select handles a nil/no-op path).
	if h.probe != nil {
		h.probe.Stop()
	}

	// 4. SIGINT dsh subprocess, then SIGKILL if it ignores
	// SIGINT. We take the cmd under mu; the field may have
	// been nil since start (attached mode with no own proc).
	h.mu.RLock()
	cmd := h.cmd
	oldCli := h.cli
	h.mu.RUnlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
			// graceful exit
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done // wait for reap so we don't leave a zombie
		}
	}

	// 5. close the live Client to drain Hub pumps.
	if oldCli != nil {
		oldCli.Close()
	}

	// 6. unset singletons so EnsureSharedHost rebuilds cleanly
	// on next start (closes SetGlobal/UnsetSharedHost panics if
	// called twice without Unset first).
	UnsetGlobal()
	UnsetSharedHost()
}

// dialReachable reports whether a TCP connection to 127.0.0.1:port
// is accepted (regardless of what's on the other end). Used by
// StartSharedHost to decide whether 3080 is occupied; we don't care
// whether the responder is dsh or a foreign service — the policy
// is "always spawn our own on a fresh port".
func dialReachable(port int) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// dshURLPattern matches the first line of `dsh --profile web` stdout:
//
//	dsh web: http://127.0.0.1:3080/?token=<launchToken>
//
// Captures the full URL (host + port + path + query). spawnAndWire
// uses the query to extract the launch token, then GETs /?token=...
// to mint the dsh-auth cookie (see mintAuthCookie).
var dshURLPattern = regexp.MustCompile(`dsh web:\s+(http://[^\s]+)`)

// defaultDSHPort is the canonical port both `dsh web` and the
// spawned dsh subprocess default to. The fallback sweep in
// StartSharedHost covers [3081, 3099] when this port is occupied.
const defaultDSHPort = 3080

// mintAuthCookie does the dsh 0.1.2-rc.1 launch-token → cookie
// exchange: GET /?token=<launchToken>. dsh 303-redirects to / with
// a Set-Cookie carrying the dsh-auth signed payload. We capture
// that one cookie into a fresh cookiejar so every subsequent
// /api/* and /api/remote.mux call carries it.
//
// baseURL is the dsh root WITHOUT the token query (e.g.
// "http://127.0.0.1:3080"). token is the launch token printed on
// dsh's stdout. Returned jar is populated; never nil unless an
// error is also returned.
//
// Why this is necessary: dsh 0.1.2-rc.1 auth-gates /api/* and the
// /api/remote.mux WS endpoint with per-process signed cookies.
// The launch token only works on the initial GET / — it mints the
// cookie. Without this step, every bridge call gets 401 and every
// WS upgrade closes mid-handshake.
//
// Used by spawnAndWire when nightme owns the subprocess (so the
// launch token is reachable on dsh's stdout). The attach path
// (tryAttachExistingDSH) uses mintDSHAuthCookieFromCredentials
// instead because a foreign dsh's launch token is process-internal
// and never leaves its memory.
func mintAuthCookie(ctx context.Context, baseURL, token string) (http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: cookiejar: %w", err)
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: parse base url %q: %w", baseURL, err)
	}
	// url.Values.Set doesn't propagate back to URL.RawQuery; use
	// Encode() to rebuild the query string with proper percent
	// escaping (raw concatenation would mangle tokens containing
	// & = + / or other reserved characters).
	q := *u
	values := q.Query()
	values.Set("token", token)
	q.RawQuery = values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: build token-exchange req: %w", err)
	}
	// We deliberately do NOT pass the jar — the cookiejar is
	// populated from this single response, not sent on it.
	client := &http.Client{
		Timeout: webURLParseTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// dsh returns 303 → /. We want the cookies from THAT
			// response, not from any further redirect. Stop after
			// the first hop.
			if len(via) >= 1 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: token-exchange GET %s: %w", q.String(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dsh.host: token-exchange: HTTP %d (want 303 or 200)", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		return nil, fmt.Errorf("dsh.host: token-exchange: no Set-Cookie in response (dsh version mismatch?)")
	}
	jar.SetCookies(u, cookies)
	return jar, nil
}

// mintDSHAuthCookieFromCredentials signs a dsh-auth-<sha256>=v1.body.sig
// cookie using the signing secret loaded from ~/.dsh/.credentials.yaml.
// The algorithm is verified against dsh 0.1.2-rc.1 by capturing
// the real POST body and reproducing the HMAC-SHA256 signature
// byte-for-byte (2026-09-11). See
// @deepseek-ai/dsh-client-connection/lib/index.js:encodeCookie.
//
// Returns a fresh cookiejar.Jar populated with one cookie scoped
// to baseURL (cookie name = "dsh-auth-" + base64url(sha256(authority)));
// the cookie lifetime is dsh.host.constants.cookieMaxAgeDays.
//
// Why this works: every dsh subprocess using the same ~/.dsh/
// directory loads the same client-connection/browser-session
// signing secret. We mint locally; dsh accepts. So we don't need
// to spawn a fresh dsh OR persist a cookie OR attach to a running
// dsh — just read ~/.dsh/.credentials.yaml, sign, attach.
//
// Used by tryAttachExistingDSH (foreign dsh on the wire) and by
// spawnAndWire when ~/.dsh/.credentials.yaml is available. When
// the credentials file is missing, spawnAndWire falls back to
// mintAuthCookie (launch-token exchange) — that's the spawn path
// that runs on a CI runner with no prior dsh install.
func mintDSHAuthCookieFromCredentials(authority string) (http.CookieJar, error) {
	secret, err := loadBrowserSessionSecret()
	if err != nil {
		return nil, fmt.Errorf("dsh.host: load secret: %w", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: cookiejar: %w", err)
	}
	u := &url.URL{Scheme: "http", Host: authority}
	cookieName := "dsh-auth-" + base64URL(sha256Sum([]byte(authority)))
	cookieValue := encodeDSHAuthCookie(secret, authority, cookieMaxAgeDays)
	jar.SetCookies(u, []*http.Cookie{{
		Name:     cookieName,
		Value:    cookieValue,
		Path:     "/",
		MaxAge:   cookieMaxAgeDays * 24 * 60 * 60,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}})
	return jar, nil
}

// loadBrowserSessionSecret reads ~/.dsh/.credentials.yaml and
// returns the secret stored under
// records["client-connection"]["browser-session"].payload.secret.
//
// The file is owned by the user (mode 0600) — same security
// profile as the dsh-auth cookie itself. If the file is missing
// or the schema is unexpected, we surface a clear error rather
// than silently falling through to a network exchange (which
// wouldn't work anyway — we have no token).
func loadBrowserSessionSecret() ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("dsh.host: home dir: %w", err)
	}
	path := filepath.Join(home, ".dsh", ".credentials.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: read %s: %w", path, err)
	}
	var rec struct {
		Records map[string]struct {
			Kind    string `json:"kind"`
			Payload struct {
				Secret string `json:"secret"`
			} `json:"payload"`
		} `json:"records"`
	}
	if err := yaml.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("dsh.host: parse %s: %w", path, err)
	}
	browserSession, ok := rec.Records["client-connection/browser-session"]
	if !ok {
		return nil, fmt.Errorf("dsh.host: %s missing client-connection/browser-session record", path)
	}
	if browserSession.Kind != "grant" {
		return nil, fmt.Errorf("dsh.host: %s browser-session record kind=%q, want \"grant\"", path, browserSession.Kind)
	}
	raw, err := base64URLDecode(browserSession.Payload.Secret)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: secret base64url: %w", err)
	}
	return raw, nil
}

// encodeDSHAuthCookie builds the v1.body.sig cookie value per
// dsh 0.1.2-rc.1's encodeCookie (verified 2026-09-11).
func encodeDSHAuthCookie(secret []byte, authority string, maxAgeDays int) string {
	body := encodeBase64URL([]byte(fmt.Sprintf(
		`{"version":1,"authority":%q,"issuedAt":%d,"expiresAt":%d}`,
		authority, time.Now().UnixMilli(),
		time.Now().Add(time.Duration(maxAgeDays)*24*time.Hour).UnixMilli(),
	)))
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	return "v1." + body + "." + encodeBase64URL(mac.Sum(nil))
}

// cookieMaxAgeDays is the dsh-auth cookie's lifetime in days.
// Matches dsh 0.1.2-rc.1's default (30 days, see
// @deepseek-ai/dsh-client-connection/lib/index.js).
const cookieMaxAgeDays = 30

// encodeBase64URL / base64URLDecode / base64URL / sha256Sum are
// thin wrappers around the stdlib encoders with the exact
// padding / char-set semantics dsh 0.1.2-rc.1 uses. dsh strips the
// trailing '=' padding (URL-safe base64) and uses the URL-safe
// alphabet ('-' / '_' for '+' / '/'). We mirror that exactly.
func encodeBase64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func base64URLDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

func base64URL(b []byte) string { return encodeBase64URL(b) }

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// stderrRing is a bounded line buffer for dsh's stderr. The
// waitForListen failure path dumps its snapshot into a Warn-level
// log line so /diagnose can see what dsh actually said before we
// SIGKILL'd it; stderrCaptureCap bounds memory. append shifts left
// by one when full (cap is small, the copy is cheap) so the
// newest stderr lines always win, which is what the operator
// wants when triaging "why didn't dsh bind the port we asked for".
type stderrRing struct {
	mu   sync.Mutex
	ring []string
}

func newStderrRing() *stderrRing {
	return &stderrRing{ring: make([]string, 0, stderrCaptureCap)}
}

func (r *stderrRing) append(line string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ring) >= stderrCaptureCap {
		copy(r.ring, r.ring[1:])
		r.ring = r.ring[:stderrCaptureCap-1]
	}
	r.ring = append(r.ring, line)
}

func (r *stderrRing) snapshot() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.ring))
	copy(out, r.ring)
	return out
}

// portFromBaseURL removed: we now know the port because we passed
// --port to dsh explicitly, so the URL we construct is just
// fmt.Sprintf("http://127.0.0.1:%d", port). Parsing the spawned
// URL to extract a port we already own was redundant.

// waitCmd returns a channel that closes when cmd.Wait() returns.
// cmd.Wait may only be called once; using a helper that runs it in
// a goroutine lets us safely select on it.
func waitCmd(cmd *exec.Cmd) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return done
}

// SharedHost lifecycle singleton — parallel to SetGlobal/GetGlobal
// for the *Client. Set by the daemon boot path (cmd/nightme/main.go
// or runtime/runDaemon); read by the shutdown path so it can Close
// the subprocess. Tests can swap via SetSharedHost + UnsetSharedHost.
//
// Why a separate getter for SharedHost and Client: SharedHost owns
// the subprocess (needs Close), Client owns the in-process RPC +
// stream plumbing (needs no shutdown because SharedHost.Close already
// tears it down). Splitting them keeps each side's responsibility
// clean and lets the daemon boot path control process lifetime
// separately from RPC plumbing.
var (
	sharedHostMu     sync.RWMutex
	sharedHostGlobal *SharedHost
)

// SetSharedHost installs h as the process-wide SharedHost singleton.
// Must be called once during daemon boot AFTER StartSharedHost and
// BEFORE any ChatSession / AgentSession can be created. Calling
// twice panics.
func SetSharedHost(h *SharedHost) {
	sharedHostMu.Lock()
	defer sharedHostMu.Unlock()
	if sharedHostGlobal != nil {
		panic("dsh.host: SetSharedHost called twice; the shared host is a singleton")
	}
	sharedHostGlobal = h
}

// GetSharedHost returns the SharedHost installed by SetSharedHost,
// or nil if SetSharedHost hasn't run yet.
func GetSharedHost() *SharedHost {
	sharedHostMu.RLock()
	defer sharedHostMu.RUnlock()
	return sharedHostGlobal
}

// UnsetSharedHost clears the singleton. Used by tests that install
// a fresh host per case. Not intended for production paths.
func UnsetSharedHost() {
	sharedHostMu.Lock()
	sharedHostGlobal = nil
	sharedHostMu.Unlock()
}

// ─── Owned-mode respawn helpers ──────────────────────────────

// forceKillCmd sends SIGKILL to the current dsh subprocess. Called
// by HealthProbe when strikesMax consecutive /health probes fail
// (signals "dsh is alive but wedged" — recoverable only by hard
// restart). SIGKILL triggers cmd.Wait() to return, which the
// monitor's monitorOwnedLoop sees as an unexpected exit and triggers
// respawn.
//
// Safe to call from any goroutine; takes h.mu briefly to read the
// current cmd. No-op if cmd is nil or already exited.
func (h *SharedHost) forceKillCmd() {
	h.mu.RLock()
	cmd := h.cmd
	h.mu.RUnlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Kill(); err != nil {
		h.logger.Warn("dsh.host: forceKillCmd: kill failed",
			"pid", cmd.Process.Pid, "err", err.Error())
	}
}

// tryRespawn brings up a fresh dsh subprocess and atomically
// swaps it in. Retries forever with exponential backoff (capped
// at respawnBackoffMax) — the only exit is h.closed firing during
// a backoff sleep (returns non-nil error). The previous
// maxRespawnAttempts=5 cap was removed for the same reason as
// monitorAttachedFallback: nightme owns the dsh host lifecycle and
// should keep trying on transient failures, not strand the host.
//
// On success, h.cmd and h.cli are swapped atomically (under h.mu),
// the global Client pointer is replaced via ReplaceGlobal, the old
// cli's Router subscriptions are re-registered on the new Client
// (so existing drivers' muxSubs stay consistent across the swap),
// and the previous Client is closed so any in-flight RPC gets a
// clean error rather than a hung transport.
//
// Caller is expected to invoke Client.RecoverSubscriptions on the
// new cli once tryRespawn returns nil.
func (h *SharedHost) tryRespawn() error {
	for attempt := 0; ; attempt++ {
		select {
		case <-h.closed:
			return errors.New("dsh.host: monitor closed before respawn succeeded")
		default:
		}

		select {
		case <-h.closed:
			return errors.New("dsh.host: monitor closed during respawn backoff")
		case <-time.After(respawnDelay(attempt)):
		}

		cmd, cli, port, err := h.spawnRespawnOnce()
		if err != nil {
			h.logger.Warn("dsh.host: respawn attempt failed; retrying after backoff",
				"attempt", attempt, "err", err)
			continue
		}

		h.mu.Lock()
		oldCli := h.cli
		h.cmd = cmd
		h.cli = cli
		if port > 0 {
			// Track the actual bound port so subsequent
			// respawns reuse it (see finding 6).
			h.opts.Port = port
		}
		h.mu.Unlock()
		ReplaceGlobal(cli)

		if oldCli != nil {
			// Snapshot the old Client's Router subscriptions
			// BEFORE closing it — see comment below.
			subs := oldCli.Router.Snapshot()
			// Re-register each session on the new Client's
			// Router (same SessionID + cwd + handler). This
			// keeps the daemon's Router.muxSubs consistent
			// across respawns so RecoverSubscriptions (called
			// next by monitorOwnedLoop) actually sees the active
			// subscriptions. Without this transfer, each respawn
			// would orphan every active session's mux subscription
			// and the chat session would silently stop receiving
			// events. The StreamHub subscription is reconstructed
			// by StreamHub's generation tracking on the next
			// reconnect (see host/stream.go::toReopen).
			//
			// We deliberately reuse the same handler closure
			// from the old Router entry — it's a *driver
			// method value, so it doesn't hold any transport
			// state that needs resetting.
			for _, sub := range subs {
				if sub.Handler == nil {
					continue
				}
				cli.Router.Subscribe(sub.SessionID, sub.CWD, sub.Handler)
			}
			// Old Client's mux/host pumps already died with the
			// old dsh (close on conn); just close the in-process
			// state to free the goroutines cleanly. The Router
			// snapshot above lets the new Client pick up where
			// the old one left off.
			oldCli.Close()
		}
		h.logger.Info("dsh.host: respawn success",
			"pid", cmd.Process.Pid,
			"attempt", attempt)
		return nil
	}
}

// spawnAndWire spawns a fresh dsh subprocess with --port <port>
// explicit, polls TCP readiness via waitForListen, and constructs
// + starts a *Client rooted at fmt.Sprintf("http://127.0.0.1:%d", port).
// Returns the live cmd (caller takes ownership of lifecycle) and the
// started Client (RPC + Hub + Router). On any error after Start() the
// cmd is killed + wait'd before returning so callers don't have to
// clean up a half-built subprocess.
//
// Shared by StartSharedHost's initial spawn and the watchdog's
// spawnOnce path — same mechanics, different ownership semantics.
//
// --port is always explicit (no reliance on dsh's default) so the
// daemon owns the bind. Host is always 127.0.0.1; nothing in this
// code path supports remote hosts. The fallback-port sweep in
// StartSharedHost picks a port from findFreePort(3081, 3099) when
// 3080 is held by something that isn't dsh; spawnAndWire doesn't
// choose the port itself, the caller does.
func spawnAndWire(ctx context.Context, opts SharedHostOptions, port int, logger *slog.Logger) (*exec.Cmd, *Client, error) {
	if logger == nil {
		// Match StartSharedHost's nil-guard so callers and tests
		// can omit the logger without panicking in the stderr
		// drain goroutine. (Pre-existing fragility surfaced by
		// the diagnostic-capture test.)
		logger = slog.Default()
	}
	child := proc.New(ctx, opts.HostCmd, "--profile", "web",
		"--port", strconv.Itoa(port))
	child.Dir = opts.Workspace
	child.Env = append(os.Environ(),
		"DSH_PERMISSION_MODE="+opts.PermissionMode,
	)

	stdout, err := child.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("dsh.host: stdout pipe: %w", err)
	}
	stderr, err := child.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("dsh.host: stderr pipe: %w", err)
	}
	if err := child.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, nil, fmt.Errorf("dsh.host: spawn: %w", err)
	}

	// Drain stderr so the pipe buffer doesn't fill and deadlock the
	// subprocess. Logs each line at debug level for /diagnose
	// triage; also retains a bounded ring so the waitForListen
	// failure path can attach dsh's actual stderr to the error
	// chain (regression visible in /review failures where the
	// timeout branch used to discard everything).
	stderrBuf := newStderrRing()
	go func(r io.ReadCloser) {
		scnr := bufio.NewScanner(r)
		scnr.Buffer(make([]byte, 0, 4096), 16*1024)
		for scnr.Scan() {
			line := scnr.Text()
			logger.Debug("dsh.host: stderr", "line", line)
			stderrBuf.append(line)
		}
	}(stderr)

	// Drain stdout so dsh's pipe buffer doesn't fill and deadlock
	// the subprocess. The launch-token exchange path needs the
	// first URL line; we capture it into tokenCh while still
	// logging every line at debug level for /diagnose triage.
	// Buffered=1 so the goroutine doesn't block if the receiver
	// already grabbed the token.
	tokenCh := make(chan string, 1)
	tokenErrCh := make(chan error, 1)
	go func(r io.Reader) {
		scnr := bufio.NewScanner(r)
		scnr.Buffer(make([]byte, 0, 4096), 16*1024)
		sent := false
		for scnr.Scan() {
			line := scnr.Text()
			logger.Debug("dsh.host: stdout", "line", line)
			if sent {
				continue
			}
			if m := dshURLPattern.FindStringSubmatch(line); m != nil {
				u, perr := url.Parse(m[1])
				if perr != nil {
					tokenErrCh <- fmt.Errorf("dsh.host: parse dsh url %q: %w", m[1], perr)
					return
				}
				token := u.Query().Get("token")
				if token == "" {
					tokenErrCh <- fmt.Errorf("dsh.host: dsh url %q has no ?token=...", m[1])
					return
				}
				tokenCh <- token
				sent = true
			}
		}
		if !sent {
			tokenErrCh <- errors.New("dsh.host: stdout closed before URL line appeared")
		}
	}(stdout)

	// Wait for dsh to accept TCP on the port we asked for.
	listenCtx, listenCancel := context.WithTimeout(ctx, webURLParseTimeout)
	defer listenCancel()
	if err := waitForListen(listenCtx, port); err != nil {
		// Brief grace so the stderr goroutine can flush any
		// output dsh was mid-writing when bind/listen failed.
		// stderrFlushGrace keeps the user-visible error
		// latency negligible.
		time.Sleep(stderrFlushGrace)

		stderrSnapshot := stderrBuf.snapshot()
		_ = child.Process.Kill()
		_ = child.Wait()
		_ = stdout.Close()

		if len(stderrSnapshot) > 0 {
			logger.Warn("dsh.host: dsh did not listen on time; diagnostic context",
				"port", port,
				"stderr_lines", len(stderrSnapshot),
				"stderr_tail", strings.Join(stderrSnapshot, "\n"),
			)
		}
		return nil, nil, fmt.Errorf("dsh.host: dsh not listening on port %d: %w (stderr=%d lines)",
			port, err, len(stderrSnapshot))
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	// Mint the dsh-auth cookie. Try the local-secret path first —
	// it works without a launch-token round-trip and lets the
	// minted cookie validate against any dsh on this user account
	// (which is what the attach path needs). If ~/.dsh/.credentials.yaml
	// is missing or unreadable, fall back to the launch-token
	// exchange (we own this dsh, so the token is on stdout). The
	// fallback keeps the spawn path working on machines that never
	// ran dsh --profile web (CI runners, fresh nightme installs).
	authority := strings.TrimPrefix(baseURL, "http://")
	jar, err := mintDSHAuthCookieFromCredentials(authority)
	if err != nil {
		logger.Debug("dsh.host: credentials-based mint unavailable; falling back to launch-token exchange",
			"err", err)
		// Wait for the launch token from the stdout drain goroutine.
		tokenCtx, tokenCancel := context.WithTimeout(ctx, webURLParseTimeout)
		token, terr := func() (string, error) {
			select {
			case t := <-tokenCh:
				return t, nil
			case e := <-tokenErrCh:
				return "", e
			case <-tokenCtx.Done():
				return "", fmt.Errorf("dsh.host: timeout waiting for launch token: %w", tokenCtx.Err())
			}
		}()
		tokenCancel()
		if terr != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
			_ = stdout.Close()
			return nil, nil, terr
		}
		jar, err = mintAuthCookie(ctx, baseURL, token)
		if err != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
			_ = stdout.Close()
			return nil, nil, err
		}
	}

	cli := NewWithJar(baseURL, jar, logger)
	// Install the host waterfall handler BEFORE Start — dsh sends
	// the host $events `ready` frame immediately after the WS
	// upgrade, so the handler must be wired before we dial. The
	// install is process-once idempotent (see
	// internal/bridge/dsh/host_waterfall.go::installHostHandler).
	// Wired via a deferred host.OnLifecycleInstall to avoid an
	// import cycle (this package is imported by the dsh package).
	OnLifecycleInstall(cli)
	// Wait for dsh's internal plugins (workspaceController etc.)
	// to finish initializing. Without this probe, the driver's
	// first workspace.create hits "active Service workspaceController
	// is unavailable" — the HTTP server is up but the typert
	// gateway's plugin registry is still loading. WaitForDSHReady
	// polls workspace.create (the same RPC nightme uses to bind
	// the session) and retries on "service-unavailable" with
	// respawnDelay backoff. opts.Workspace is the path argument
	// dsh's workspace.create requires (dsh.md §2.4.2); passing the
	// empty string triggers a terminal "input-invalid" error rather
	// than a transient race, so the caller must supply it.
	// The cookie is required (workspace.create auths the request),
	// so this must come after mintDSHAuthCookie. ctx is bounded
	// by the spawn timeout so we don't hang forever on a
	// genuinely broken dsh binary.
	readyCtx, readyCancel := context.WithTimeout(ctx, dshReadyTimeout)
	defer readyCancel()
	if err := cli.WaitForDSHReady(readyCtx, opts.Workspace, dshReadyAttempts); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		return nil, nil, fmt.Errorf("dsh.host: dsh started but not ready: %w", err)
	}
	if err := cli.Start(ctx); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		return nil, nil, fmt.Errorf("dsh.host: client start: %w", err)
	}
	return child, cli, nil
}

// spawnOnce spawns nightme's own dsh subprocess. It prefers
// h.opts.Port (the port nightme captured at Start) when that
// port is currently free — dsh died and the kernel released
// the TCP socket, so reuse it. If the port is still occupied
// (previous dsh somehow still alive, or a foreign service
// grabbed it during the dead window), fall back to the first
// free port in [defaultPortScanMin, defaultPortScanMax] and
// persist that as the new h.opts.Port via the caller's
// bookkeeping.
//
// Returns the actual port the new dsh bound to so the caller
// can update h.opts.Port. Without this, every respawn would
// re-evaluate the (now stale) captured port and drift to a
// different fallback port — see findings 6-7 in the lifecycle
// refactor review.
//
// Why port == 0 → no fallback (not the other way around):
// h.opts.Port = 0 means "dsh never bound a port here yet"
// (e.g. attached mode with a foreign dsh, or a fresh
// StartSharedHost that went down the spawn path with
// opts.Port still at the zero value). Forcing findFreePort
// in that case would discard the user's deploy-time port
// preference.
func (h *SharedHost) spawnOnce() (*exec.Cmd, *Client, int, error) {
	port := h.opts.Port
	if port > 0 && dialReachable(port) {
		// Captured port is held by something — previous dsh
		// still alive, or foreign service grabbed it
		// during the dead window. nightme dsh service is pinned
		// to 3080 — no port fallback during respawn. Operator
		// must resolve the foreign listener before nightme
		// can recover.
		h.logger.Warn("dsh.host: captured port occupied during respawn",
			"captured_port", h.opts.Port)
	}
	cmd, cli, err := spawnAndWire(context.Background(), h.opts, port, h.logger)
	return cmd, cli, port, err
}

// waitForListen polls 127.0.0.1:port until TCP accepts a connection
// or ctx fires. Replaces the old parseWebURL stdout-parse path:
// host is always 127.0.0.1 (dsh doesn't bind anywhere else) and
// port is whatever we passed via --port, so neither needs to be
// extracted from dsh's output. TCP accept is the actual readiness
// signal we care about — cli.Start's HTTP handshake right after
// catches the small kernel-accept-vs-app-Accept race window.
func waitForListen(ctx context.Context, port int) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	const tick = 50 * time.Millisecond
	for {
		// Bound the dial itself so a firewall blackhole doesn't
		// burn the full budget on a single attempt.
		dialCtx, cancel := context.WithTimeout(ctx, tick)
		d := net.Dialer{}
		conn, err := d.DialContext(dialCtx, "tcp", addr)
		cancel()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("dsh.host: timeout after %s waiting for dsh to listen on %s",
				webURLParseTimeout, addr)
		case <-time.After(tick):
		}
	}
}

// findFreePort scans [start, end] (inclusive) for the first TCP
// port not bound by anything on 127.0.0.1. Used by StartSharedHost
// when 3080 is occupied by a non-dsh service: spawn dsh on the
// first free port in [3081, 3099] instead of failing loudly.
//
// Implementation: try `net.Listen("tcp", "127.0.0.1:N")` for each
// N; EADDRINUSE → next, anything else → error. We close the
// listener immediately — there's a tiny race window where another
// process could grab the port between close and dsh's bind, but
// that's a known property of the OS's port allocator and is the
// same race dsh would face scanning by hand.
func findFreePort(start, end int) (int, error) {
	if start < 1 || end > 65535 || start > end {
		return 0, fmt.Errorf("dsh.host: invalid scan range [%d, %d]", start, end)
	}
	for p := start; p <= end; p++ {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
		l, err := net.Listen("tcp", addr)
		if err == nil {
			_ = l.Close()
			return p, nil
		}
		// EADDRINUSE → try next. Anything else (permission,
		// resolver, etc.) is a real error worth surfacing.
		if !errors.Is(err, syscall.EADDRINUSE) {
			return 0, fmt.Errorf("dsh.host: scan port %d: %w", p, err)
		}
	}
	return 0, fmt.Errorf("dsh.host: no free port in range [%d, %d]", start, end)
}

// respawnDelay returns the backoff for the given attempt index.
// Pure function so tests can pin the policy.
func respawnDelay(attempt int) time.Duration {
	d := respawnBackoffBase
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= respawnBackoffMax {
			return respawnBackoffMax
		}
	}
	return d
}
