// lifecycle.go — process management for the shared dsh web daemon.
//
// In the shared-host architecture (F-dsh-shared-host), exactly ONE
// `dsh --profile web` subprocess is owned by the nightme daemon —
// started once at boot, kept alive for the daemon's lifetime,
// gracefully shut down on exit. This file owns the subprocess;
// the rest of the package (client.go, stream.go, router.go) talks
// to it over HTTP + WebSocket.
//
// Lifecycle model:
//
//   StartSharedHost(ctx, opts)
//     1. exec `dsh --profile web --port 0` (or fallback to --port 0
//        if the default 3080 is taken — see fallbackPort for the
//        actual probe order)
//     2. read stdout until "dsh web: http://127.0.0.1:<port>" appears
//     3. construct *host.Client rooted at that URL
//     4. Client.Start pumps → mux/host WS connects
//     5. install client via host.SetGlobal so dsh.newDriver can find it
//
//   ShutdownSharedHost(ctx, client)
//     1. Client.Close (stops mux/host pumps)
//     2. session.cancel best-effort for any subscribed sessions (none
//        at daemon shutdown — sessions were already Closed by the
//        runtime's own shutdown sequence)
//     3. SIGINT dsh, wait 5s, SIGKILL, wait 5s
//
// Phase 1 only implements start + graceful shutdown. Watchdog +
// auto-restart on crash lands in Phase 2 (F-dsh-shared-host §4.2).
package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
)

// webURLParseTimeout bounds waiting for dsh web to print its bound
// URL on stdout. Real-machine cold start is ~1.5s; 10s is generous.
const webURLParseTimeout = 10 * time.Second

// dshURLPattern matches the first line of `dsh --profile web` stdout:
// "dsh web: http://127.0.0.1:3080". Captures host + port.
var dshURLPattern = regexp.MustCompile(`dsh web:\s+http://([^:\s]+):(\d+)`)

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
	ForceSpawn bool

	// Logger is the slog handle for lifecycle messages. nil → slog.Default().
	Logger *slog.Logger
}

// SharedHost wraps the running dsh subprocess + the host.Client
// pointing at it. Use StartSharedHost to construct; the daemon
// never tears it down (dsh is a persistent service — see
// internal/bridge/dsh/host/ensure.go for the lazy-start model).
//
// Two ownership modes:
//
//   - ownsProcess=true:  SharedHost spawned the dsh subprocess;
//                        watchdog respawns on crash.
//   - ownsProcess=false: SharedHost reused a pre-existing dsh the user
//                        already had running (e.g. browser dashboard);
//                        watchdog is a no-op.
//
// Watchdog (ownsProcess only): a background goroutine watches cmd.Wait
// and respawns the dsh subprocess if it exits unexpectedly. After a
// successful respawn, the watchdog re-attaches every Router
// subscription on the new dsh via Client.RecoverSubscriptions. The
// watchdog is killed by the Go runtime when the daemon process exits
// — there is no graceful-shutdown handshake since the daemon never
// signals dsh on shutdown.
type SharedHost struct {
	cmd    *exec.Cmd
	cli    *Client
	logger *slog.Logger
	opts   SharedHostOptions // captured at Start for respawn parity

	// ownsProcess distinguishes "we spawned this dsh" from "this is
	// the user's dsh we attached to". The watchdog consults this to
	// decide whether to respawn.
	ownsProcess bool

	mu sync.RWMutex // guards cmd + cli swap during respawn

	// watchdogDone is closed when the watchdog goroutine has fully
	// exited (or immediately if no watchdog was started).
	watchdogDone chan struct{}
}

// closedChan is a pre-closed channel used as the watchdogDone value
// when SharedHost doesn't run a watchdog (ownsProcess=false). It's
// a stand-in for the "watchdog already done" sentinel so callers
// that (defensively) range on h.watchdogDone don't need to
// special-case the no-watchdog path.
var closedChan = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

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

// StartSharedHost attaches to (or spawns) the shared dsh web daemon
// and installs the resulting *Client as the process-wide singleton
// via SetGlobal.
//
// Reuse-or-spawn:
//
//  1. Probe 127.0.0.1:3080 for an existing dsh. If found, attach to
//     it (the user might have `dsh web` open in their browser) —
//     the SharedHost ownsProcess=false; no subprocess lifecycle,
//     no watchdog; Close just disconnects.
//  2. If nothing's on 3080, spawn `dsh --profile web` (no --port
//     flag → dsh defaults to 3080) and own it. Watchdog respawns
//     on crash; Close SIGINTs.
//  3. If something IS on 3080 but it's NOT dsh, surface the error
//     (spawning on top of a foreign web service is a footgun).
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

	// Step 1: try to reuse an existing dsh on the default port,
	// unless the caller opted out via ForceSpawn.
	cli, err := func() (*Client, error) {
		if opts.ForceSpawn {
			return nil, ErrNotRunning
		}
		return DiscoverExisting(ctx, defaultDSHPort)
	}()
	switch {
	case err == nil:
		// Reused — build a SharedHost that owns no subprocess.
		// watchdogDone is the pre-closed closedChan so Close's
		// <-h.watchdogDone returns immediately without a
		// special-case branch.
		//
		// CRITICAL: DiscoverExisting returns the Client fully
		// wired but its Hub hasn't started pumping yet — call
		// cli.Start(ctx) to bring up the mux+host WS pumps.
		// Without this, ChatSessions subscribing via Router
		// would never receive frames (the mux stream is open
		// lazily inside Hub.Start).
		if err := cli.Start(ctx); err != nil {
			return nil, fmt.Errorf("dsh.host: client start (reuse): %w", err)
		}
		h := &SharedHost{
			cli:          cli,
			logger:       logger,
			opts:         opts,
			ownsProcess:  false,
			watchdogDone: closedChan,
		}
		SetGlobal(cli)
		logger.Info("dsh.host: attached to existing dsh web",
			"base_url", cli.BaseURL(),
			"workspace", opts.Workspace)
		return h, nil
	case errors.Is(err, ErrNotRunning):
		// Fall through to spawn path below.
	case errors.Is(err, ErrNotDSH):
		return nil, fmt.Errorf("dsh.host: port %d responds but doesn't look like dsh: %w",
			defaultDSHPort, err)
	default:
		return nil, fmt.Errorf("dsh.host: discover: %w", err)
	}

	// Step 2: spawn a fresh dsh. No --port flag → dsh uses its
	// default (3080). If 3080 is now bound by something else the
	// spawn will fail and we'll surface the error loudly (we no
	// longer fall back to --port 0 — that hid the user's existing
	// dsh from us and split sessions across instances).
	cmd, cli, err := spawnAndWire(ctx, opts, logger)
	if err != nil {
		return nil, err
	}

	// Sanity: the spawned dsh must bind the canonical port. dsh
	// defaults to 3080 for `--profile web`, but a misconfigured
	// setup (DSH_PORT env, a CLI flag, a dsh config file) can
	// silently steer it to a different port — and we couldn't
	// re-discover it on next start because DiscoverExisting is
	// hard-coded to 3080. Catch the mismatch early and fail loud:
	// kill the spawn, return a clear error pointing at the fix.
	port, portErr := portFromBaseURL(cli.BaseURL())
	if portErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("dsh.host: parse spawned baseURL %q: %w",
			cli.BaseURL(), portErr)
	}
	if port != defaultDSHPort {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf(
			"dsh.host: spawned dsh bound to port %d, expected %d — "+
				"unset any dsh port override (DSH_PORT env, --port flag, "+
				"config) and free up 3080, then retry",
			port, defaultDSHPort)
	}

	logger.Info("dsh.host: web spawned",
		"pid", cmd.Process.Pid,
		"argv", cmd.Args,
		"workspace", opts.Workspace,
		"permission_mode", opts.PermissionMode,
		"port", port,
	)

	host := &SharedHost{
		cmd:    cmd,
		cli:    cli,
		logger: logger,
		opts:   opts,
	}

	// Install as the process-wide singleton. dsh.newDriver will look
	// this up via GetGlobal() at every ChatSession Start.
	SetGlobal(cli)

	// Start the watchdog. It watches cmd.Wait and respawns the dsh
	// subprocess if it exits unexpectedly (ungraceful death during
	// the daemon's lifetime). See runWatchdog for the contract.
	host.watchdogDone = make(chan struct{})
	go host.runWatchdog()

	return host, nil
}

// parseWebURL reads stdout until it sees the `dsh web: http://…` line
// or ctx fires. We use a goroutine + channel instead of bufio.Scanner
// because we want timeout cancellation mid-read.
//
// This used to live in internal/bridge/dsh/session.go (the old
// per-driver path). It moved here when the per-driver driver was
// removed in Phase 1; behaviour is unchanged.
func parseWebURL(ctx context.Context, stdout io.Reader) (string, error) {
	type result struct {
		url string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 4096), 16*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if m := dshURLPattern.FindStringSubmatch(line); m != nil {
				ch <- result{url: fmt.Sprintf("http://%s:%s", m[1], m[2])}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			ch <- result{err: fmt.Errorf("scan stdout: %w", err)}
			return
		}
		ch <- result{err: errors.New("dsh web: stdout closed before URL line appeared")}
	}()
	select {
	case r := <-ch:
		return r.url, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("timeout after %s waiting for dsh web url", webURLParseTimeout)
	}
}

// portFromBaseURL extracts the explicit port from a fully-qualified
// dsh base URL (e.g. "http://127.0.0.1:3080" → 3080). Returns an
// error if the URL is malformed or has no port (we refuse to
// accept implicit-default ports — the canonical port must be in
// the URL string so the operator can see what dsh actually bound).
func portFromBaseURL(rawURL string) (int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, fmt.Errorf("dsh.host: parse %q: %w", rawURL, err)
	}
	portStr := u.Port()
	if portStr == "" {
		return 0, fmt.Errorf("dsh.host: %q has no explicit port", rawURL)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("dsh.host: %q port %q: %w", rawURL, portStr, err)
	}
	return port, nil
}

// drainStderr keeps dsh's stderr pipe flowing. Without this, dsh
// blocks once its 64 KiB stderr pipe buffer fills. We log lines at
// debug level for post-mortem.
func (h *SharedHost) drainStderr(stderr io.ReadCloser) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 4096), 16*1024)
	for scanner.Scan() {
		h.logger.Debug("dsh.host: stderr", "line", scanner.Text())
	}
}

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

// ─── Watchdog: auto-restart dsh on unexpected death ─────────────

// runWatchdog loops forever (until the daemon process exits) and
// respawns dsh whenever it dies unexpectedly. Each iteration:
//
//  1. Wait for the current cmd to exit.
//  2. dsh died — respawn with backoff.
//  3. After respawn, re-attach every Router subscription on the
//     new dsh via Client.RecoverSubscriptions so ChatSessions keep
//     receiving mux frames.
//
// The respawn cycle has bounded total attempts (maxRespawnAttempts);
// if every attempt fails the watchdog gives up and exits. The
// daemon stays alive but dsh will not be re-spawned until the next
// daemon restart (which triggers a fresh lazy start).
//
// The daemon never calls any shutdown hook on this goroutine —
// when the daemon exits, Go's runtime takes the goroutine down.
// That is the only reason the loop doesn't otherwise need a break
// condition.
func (h *SharedHost) runWatchdog() {
	defer close(h.watchdogDone)

	// Start the health probe in parallel with the main watchdog
	// loop. The probe fires onFailure → h.forceKillCmd() when
	// strikes accumulate, which causes the cmd.Wait channel to
	// fire and the main loop's respawn path takes over. We only
	// run the probe when we own the process — for a reused
	// user-owned dsh, killing it would be hostile.
	var probe *HealthProbe
	if h.ownsProcess {
		probe = NewHealthProbe(
			func() *Client { return h.Client() },
			h.forceKillCmd,
			h.logger,
		)
		probe.Start()
		defer probe.Stop()
	}

	for {
		h.mu.RLock()
		cmd := h.cmd
		h.mu.RUnlock()

		if cmd == nil {
			return
		}

		// Block until cmd exits. The Go runtime tears this
		// goroutine down when the daemon process exits, so there
		// is no graceful-shutdown branch.
		<-waitCmd(cmd)

		h.logger.Error("dsh.host: subprocess exited unexpectedly; respawning")

		if err := h.tryRespawn(); err != nil {
			h.logger.Error("dsh.host: respawn cycle failed; watchdog giving up",
				"err", err)
			return
		}

		// Re-attach subscriptions on the new dsh. Best-effort —
		// orphaned sessions (cwd mismatch / server-side reap)
		// are logged per-session at Warn level (see
		// Client.RecoverSubscriptions) but don't block the
		// watchdog from continuing to watch the new cmd.
		ctx, cancel := context.WithTimeout(context.Background(), respawnRecoverTO)
		result := h.cli.RecoverSubscriptions(ctx, h.logger)
		cancel()
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
}

// forceKillCmd sends SIGKILL to the current dsh subprocess. Called
// by HealthProbe when strikesMax consecutive /health probes fail
// (signals "dsh is alive but wedged" — recoverable only by hard
// restart). SIGKILL triggers cmd.Wait() to return, which the
// main watchdog loop sees as an unexpected exit and respawns.
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

// tryRespawn attempts to bring up a fresh dsh subprocess. Up to
// maxRespawnAttempts tries with exponential backoff (respawnBackoffBase
// → respawnBackoffMax). Returns nil on success.
//
// On success, h.cmd and h.cli are swapped atomically (under h.mu)
// and the global Client pointer is replaced via ReplaceGlobal. The
// previous Client is closed after the swap so any in-flight RPC
// gets a clean error rather than a hung transport.
func (h *SharedHost) tryRespawn() error {
	for attempt := 0; attempt < maxRespawnAttempts; attempt++ {
		// No close-watch here: the daemon process is the only
		// thing that can interrupt this loop, and when it does
		// the watchdog goroutine is killed before tryRespawn
		// returns. Letting the backoff tick fully is fine.
		time.Sleep(respawnDelay(attempt))

		cmd, cli, err := h.spawnOnce()
		if err != nil {
			h.logger.Warn("dsh.host: respawn attempt failed",
				"attempt", attempt, "err", err)
			continue
		}

		h.mu.Lock()
		oldCli := h.cli
		h.cmd = cmd
		h.cli = cli
		h.mu.Unlock()
		ReplaceGlobal(cli)

		if oldCli != nil {
			// Old Client's mux/host pumps already died with the
			// old dsh (close on conn); just close the in-process
			// state to free the goroutines cleanly.
			oldCli.Close()
		}
		h.logger.Info("dsh.host: respawn success",
			"pid", cmd.Process.Pid,
			"attempt", attempt)
		return nil
	}
	return errors.New("dsh.host: max respawn attempts exceeded")
}

// spawnAndWire spawns a fresh dsh subprocess, parses its bound URL
// from stdout, and constructs + starts a *Client rooted at that URL.
// Returns the live cmd (caller takes ownership of lifecycle) and the
// started Client (RPC + Hub + Router). On any error after Start() the
// cmd is killed + wait'd before returning so callers don't have to
// clean up a half-built subprocess.
//
// Shared by StartSharedHost's initial spawn and the watchdog's
// spawnOnce path — same mechanics, different ownership semantics.
//
// No --port flag: dsh's own default for --profile web is 3080, and
// the reuse-or-spawn contract assumes "3080 or fail loud". Falling
// back to --port 0 would split sessions across instances if the
// user's dsh is on a different port.
func spawnAndWire(ctx context.Context, opts SharedHostOptions, logger *slog.Logger) (*exec.Cmd, *Client, error) {
	child := proc.New(ctx, opts.HostCmd, "--profile", "web")
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
	// subprocess. Logs at debug level for /diagnose triage.
	go func(r io.ReadCloser) {
		scnr := bufio.NewScanner(r)
		scnr.Buffer(make([]byte, 0, 4096), 16*1024)
		for scnr.Scan() {
			logger.Debug("dsh.host: stderr", "line", scnr.Text())
		}
	}(stderr)

	urlCtx, urlCancel := context.WithTimeout(ctx, webURLParseTimeout)
	defer urlCancel()
	baseURL, err := parseWebURL(urlCtx, stdout)
	if err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("dsh.host: parse web url: %w", err)
	}

	cli := New(baseURL, logger)
	if err := cli.Start(ctx); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		return nil, nil, fmt.Errorf("dsh.host: client start: %w", err)
	}
	return child, cli, nil
}

// spawnOnce is the watchdog's per-attempt spawn wrapper around
// spawnAndWire. Passes through the host's captured opts.
func (h *SharedHost) spawnOnce() (*exec.Cmd, *Client, error) {
	return spawnAndWire(context.Background(), h.opts, h.logger)
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