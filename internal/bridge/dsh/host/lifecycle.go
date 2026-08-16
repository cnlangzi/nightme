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
	"os"
	"os/exec"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// webURLParseTimeout bounds waiting for dsh web to print its bound
// URL on stdout. Real-machine cold start is ~1.5s; 10s is generous.
const webURLParseTimeout = 10 * time.Second

// dshURLPattern matches the first line of `dsh --profile web` stdout:
// "dsh web: http://127.0.0.1:3080". Captures host + port.
var dshURLPattern = regexp.MustCompile(`dsh web:\s+http://([^:\s]+):(\d+)`)

// sigintGrace is how long we wait after SIGINT for dsh to exit
// gracefully before escalating to SIGKILL. dsh's own SIGINT handler
// closes WS, persists final sessions, and exits 0 — usually <1s.
const sigintGrace = 5 * time.Second

// sigkillGrace is the final wait budget after SIGKILL. If dsh is
// still alive after this we error out (caller decides what to do).
const sigkillGrace = 5 * time.Second

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
// pointing at it. Use StartSharedHost to construct; call Close to
// tear down.
//
// Two ownership modes:
//
//   - ownsProcess=true:  SharedHost spawned the dsh subprocess;
//                        watchdog respawns on crash; Close SIGINTs it.
//   - ownsProcess=false: SharedHost reused a pre-existing dsh the user
//                        already had running (e.g. browser dashboard);
//                        watchdog is a no-op; Close only disconnects.
//
// Watchdog (ownsProcess only): a background goroutine watches cmd.Wait
// and respawns the dsh subprocess if it exits unexpectedly. After a
// successful respawn, the watchdog re-attaches every Router
// subscription on the new dsh via Client.RecoverSubscriptions.
type SharedHost struct {
	cmd    *exec.Cmd
	cli    *Client
	logger *slog.Logger
	opts   SharedHostOptions // captured at Start for respawn parity

	// ownsProcess distinguishes "we spawned this dsh" from "this is
	// the user's dsh we attached to". Close() and the watchdog
	// consult this to decide whether to signal / respawn.
	ownsProcess bool

	mu     sync.RWMutex // guards cmd + cli swap during respawn
	once   sync.Once    // guards Close idempotency + closed chan
	closed chan struct{}

	// watchdogDone is closed when the watchdog goroutine has fully
	// exited (or immediately if no watchdog was started). Close()
	// blocks on it so a respawn in progress at shutdown time can't
	// outlive the daemon.
	watchdogDone chan struct{}
}

// closedChan is a pre-closed channel used as the watchdogDone value
// when SharedHost doesn't run a watchdog (ownsProcess=false). This
// way Close()'s <-h.watchdogDone returns immediately without
// special-casing.
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

// Done returns a channel that's closed when Close() completes (or
// has already completed). Tests wait on this for clean shutdown.
func (h *SharedHost) Done() <-chan struct{} { return h.closed }

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
		h := &SharedHost{
			cli:          cli,
			logger:       logger,
			opts:         opts,
			ownsProcess:  false,
			closed:       make(chan struct{}),
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
	cmd := agent.NewCmd(ctx, opts.HostCmd, "--profile", "web")
	cmd.Dir = opts.Workspace
	cmd.Env = append(os.Environ(),
		"DSH_PERMISSION_MODE="+opts.PermissionMode,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("dsh.host: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("dsh.host: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("dsh.host: spawn: %w", err)
	}

	logger.Info("dsh.host: web spawned",
		"pid", cmd.Process.Pid,
		"argv", cmd.Args,
		"workspace", opts.Workspace,
		"permission_mode", opts.PermissionMode,
	)

	// Drain stderr in the background. We don't parse it (URL was
	// captured from stdout) but we MUST keep the pipe flowing or
	// dsh blocks once its 64 KiB stderr pipe buffer fills. We do
	// log stderr lines at debug level for `/diagnose` triage.
	host := &SharedHost{
		cmd:    cmd,
		logger: logger,
		closed: make(chan struct{}),
	}
	go host.drainStderr(stderr)

	// Read stdout until we see the URL or ctx fires.
	urlCtx, urlCancel := context.WithTimeout(ctx, webURLParseTimeout)
	baseURL, err := parseWebURL(urlCtx, stdout)
	urlCancel()
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("dsh.host: parse web url: %w", err)
	}
	logger.Info("dsh.host: web url parsed", "url", baseURL, "pid", cmd.Process.Pid)

	// Construct the Client (RPC + Hub + Router) and wire it.
	// Note: cli is already declared above the discover-or-spawn
	// switch — we just reassign in the spawn branch.
	cli = New(baseURL, logger)
	host.cli = cli
	if err := cli.Start(ctx); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("dsh.host: client start: %w", err)
	}

	// Install as the process-wide singleton. dsh.newDriver will look
	// this up via GetGlobal() at every ChatSession Start.
	SetGlobal(cli)

	// Capture opts so a watchdog respawn can rebuild the subprocess
	// with the same workspace / hostCmd / permission mode.
	host.opts = opts

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

// Close shuts the shared host down. Idempotent.
//
// Order:
//
//  1. Close h.closed FIRST. The watchdog observes this and exits
//     on its next inner-loop tick — important because the watchdog
//     can otherwise race us: bash dies from SIGINT (cmdDone fires),
//     watchdog enters tryRespawn before we manage to defer-close
//     h.closed, then keeps respawning new cmds indefinitely.
//  2. Close Client (stops mux/host pumps; in-flight RPC returns errors).
//  3. SIGINT dsh (graceful shutdown; dsh closes WS, persists sessions).
//  4. Wait up to sigintGrace; if still alive, SIGKILL.
//  5. Wait up to sigkillGrace; if still alive, return error.
//  6. Wait for watchdog to fully exit (so a respawn-in-progress can't outlive us).
//
// We do NOT cancel individual sessions — the runtime's own shutdown
// sequence (ShutdownRun) calls session.Close() for every ChatSession
// before this Close runs.
func (h *SharedHost) Close() error {
	var err error
	h.once.Do(func() {
		close(h.closed)

		if h.cli != nil {
			h.cli.Close()
		}

		h.mu.RLock()
		cmd := h.cmd
		h.mu.RUnlock()

		if cmd == nil || cmd.Process == nil {
			<-h.watchdogDone
			return
		}

		// SIGINT — dsh web handles SIGINT gracefully.
		_ = cmd.Process.Signal(syscall.SIGINT)

		select {
		case <-waitCmd(cmd):
			h.logger.Info("dsh.host: exited after SIGINT", "pid", cmd.Process.Pid)
			<-h.watchdogDone
			return
		case <-time.After(sigintGrace):
		}

		// SIGKILL fallback.
		h.logger.Warn("dsh.host: SIGINT did not free; escalating to SIGKILL",
			"pid", cmd.Process.Pid)
		_ = cmd.Process.Kill()

		select {
		case <-waitCmd(cmd):
			<-h.watchdogDone
			return
		case <-time.After(sigkillGrace):
			err = errors.New("dsh.host: child did not exit within SIGKILL grace")
		}

		// Wait for the watchdog to fully exit even on error path so
		// we don't leave an orphaned goroutine past the daemon.
		<-h.watchdogDone
	})
	return err
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

// runWatchdog loops until the SharedHost is closed. Each iteration:
//
//  1. Wait for the current cmd to exit (graceful or otherwise).
//  2. If Close was called (h.closed is closed), exit cleanly.
//  3. Otherwise the dsh died unexpectedly — respawn with backoff.
//  4. After respawn, re-attach every Router subscription on the
//     new dsh via Client.RecoverSubscriptions so ChatSessions keep
//     receiving mux frames.
//
// The respawn cycle has bounded total attempts (maxRespawnAttempts);
// if every attempt fails the watchdog gives up and exits. The
// daemon stays alive (SharedHost.Close still works), but dsh will
// not be re-spawned until the next manual restart.
func (h *SharedHost) runWatchdog() {
	defer close(h.watchdogDone)

	for {
		h.mu.RLock()
		cmd := h.cmd
		h.mu.RUnlock()

		if cmd == nil {
			return
		}

		cmdDone := waitCmd(cmd)

		select {
		case <-h.closed:
			return
		case <-cmdDone:
			// dsh exited. Determine whether this was graceful.
			h.mu.RLock()
			graceful := h.isCloseFlagSetLocked()
			h.mu.RUnlock()
			if graceful {
				return
			}
		}

		h.logger.Error("dsh.host: subprocess exited unexpectedly; respawning")

		if err := h.tryRespawn(); err != nil {
			h.logger.Error("dsh.host: respawn cycle failed; watchdog giving up",
				"err", err)
			return
		}

		// Re-attach subscriptions on the new dsh. Best-effort —
		// orphaned sessions (cwd mismatch / server-side reap)
		// are logged but don't block the watchdog from
		// continuing to watch the new cmd.
		ctx, cancel := context.WithTimeout(context.Background(), respawnRecoverTO)
		result := h.cli.RecoverSubscriptions(ctx)
		cancel()
		h.logger.Info("dsh.host: post-respawn recovery complete",
			"reattached", result.Reattached,
			"orphaned", len(result.Orphaned))
	}
}

// isCloseFlagSetLocked reports whether Close() has been initiated.
// Caller MUST hold h.mu.
func (h *SharedHost) isCloseFlagSetLocked() bool {
	select {
	case <-h.closed:
		return true
	default:
		return false
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
		select {
		case <-h.closed:
			return errors.New("dsh.host: closed during respawn")
		case <-time.After(respawnDelay(attempt)):
		}

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

// spawnOnce runs one dsh spawn cycle. Returns the live cmd + Client
// pair (URL parsed from stdout), or an error. Mirrors the spawn
// block inside StartSharedHost but doesn't touch any SharedHost
// fields — caller is responsible for swapping.
//
// No --port flag: dsh's own default for --profile web is 3080, and
// the reuse-or-spawn contract in StartSharedHost assumes "3080 or
// fail loud". Falling back to --port 0 here would split sessions
// across instances if the user's dsh is on a different port.
func (h *SharedHost) spawnOnce() (*exec.Cmd, *Client, error) {
	cmd := agent.NewCmd(context.Background(), h.opts.HostCmd,
		"--profile", "web")
	cmd.Dir = h.opts.Workspace
	cmd.Env = append(os.Environ(),
		"DSH_PERMISSION_MODE="+h.opts.PermissionMode,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("dsh.host: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("dsh.host: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, nil, fmt.Errorf("dsh.host: spawn: %w", err)
	}

	go func(r io.ReadCloser) {
		scnr := bufio.NewScanner(r)
		scnr.Buffer(make([]byte, 0, 4096), 16*1024)
		for scnr.Scan() {
			h.logger.Debug("dsh.host: stderr", "line", scnr.Text())
		}
	}(stderr)

	urlCtx, urlCancel := context.WithTimeout(context.Background(), webURLParseTimeout)
	baseURL, err := parseWebURL(urlCtx, stdout)
	urlCancel()
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("dsh.host: parse web url: %w", err)
	}

	cli := New(baseURL, h.logger)
	if err := cli.Start(context.Background()); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, nil, fmt.Errorf("dsh.host: client start: %w", err)
	}
	return cmd, cli, nil
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