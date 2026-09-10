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
//	StartSharedHost(ctx, opts)
//	  1. probe 127.0.0.1:3080 via DiscoverExisting (TCP dial +
//	     GET /manifest.webmanifest fingerprint check). If a dsh is
//	     already there, attach to it (ownsProcess=false, no
//	     watchdog, daemon never tears it down on shutdown).
//	  2. if 3080 is empty (ErrNotRunning), spawn a fresh dsh with
//	     `--profile web --port 3080` explicit. Host is always
//	     127.0.0.1 (dsh doesn't bind anywhere else); port is
//	     whatever we passed via --port. Readiness is waitForListen
//	     (TCP accept on the chosen port), NOT stdout parsing.
//	  3. if 3080 is occupied by something that isn't dsh
//	     (ErrNotDSH), sweep [3081, 3099] for the first free
//	     port via findFreePort and spawn dsh on that. Range
//	     exhausted → fail loud.
//	  4. Client.Start pumps → mux/host WS connects (HTTP
//	     handshake catches the small kernel-accept-queue vs
//	     app-Accept race window).
//	  5. install client via host.SetGlobal so dsh.newDriver can find it.
//
//	ShutdownSharedHost(ctx, client)
//	  1. Client.Close (stops mux/host pumps)
//	  2. session.cancel best-effort for any subscribed sessions (none
//	     at daemon shutdown — sessions were already Closed by the
//	     runtime's own shutdown sequence)
//	  3. SIGINT dsh, wait 5s, SIGKILL, wait 5s
package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
)

// webURLParseTimeout bounds waiting for dsh web to print its bound
// URL on stdout. Real-machine cold start is ~1.5s; 10s is generous.
const webURLParseTimeout = 10 * time.Second

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

// portScanRange bounds the fallback port sweep when 3080 is held
// by a non-dsh service. Default = [3081, 3099] (20 ports). Configurable
// via SharedHostOptions if a deployment needs more headroom.
const (
	defaultPortScanMin = 3081
	defaultPortScanMax = 3099
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

// SharedHost wraps the running dsh subprocess + the host.Client
// pointing at it. Use StartSharedHost to construct; the daemon
// never tears it down (dsh is a persistent service — see
// internal/bridge/dsh/host/ensure.go for the lazy-start model).
//
// Two ownership modes:
//
//   - ownsProcess=true:  SharedHost spawned the dsh subprocess;
//     watchdog respawns on crash.
//   - ownsProcess=false: SharedHost reused a pre-existing dsh the user
//     already had running (e.g. browser dashboard);
//     watchdog is a no-op.
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

	// Step 2: decide which port to spawn on. Canonical is 3080
	// (probe already showed it's empty). If probe said ErrNotDSH
	// — something foreign is squatting on 3080 — sweep the
	// fallback range [3081, 3099] for the first free port and
	// spawn there. The range is small on purpose: if 20 ports
	// are taken the operator has a real port-storm problem and
	// should be told rather than silently drifting further.
	port := defaultDSHPort
	if errors.Is(err, ErrNotDSH) {
		scanMin, scanMax := defaultPortScanMin, defaultPortScanMax
		found, scanErr := findFreePort(scanMin, scanMax)
		if scanErr != nil {
			return nil, fmt.Errorf(
				"dsh.host: port %d occupied by non-dsh and no free port in range %d-%d: %w",
				defaultDSHPort, scanMin, scanMax, scanErr)
		}
		port = found
		logger.Warn("dsh.host: 3080 occupied by non-dsh; falling back",
			"foreign_port", defaultDSHPort,
			"fallback_port", port,
		)
	}

	// Step 3: spawn dsh with --port explicit. We pin the port so
	// the contract doesn't depend on dsh's default-port behavior
	// (which has historically drifted across versions) and so the
	// Client URL we construct matches what we asked for.
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

// parseWebURL removed: replaced by waitForListen (TCP-poll on the
// port we asked dsh to bind via --port). Host is always 127.0.0.1
// and port is whatever we passed to dsh, so we no longer parse
// stdout for the URL line — that path was both fragile (dsh stdout
// format drift caused silent timeouts) and unnecessary now that
// we own the port choice.

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
	// triage; also retains a bounded ring so the parseWebURL
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

	// Drain stdout to prevent pipe deadlock; we no longer parse it.
	go func(r io.ReadCloser) {
		scnr := bufio.NewScanner(r)
		scnr.Buffer(make([]byte, 0, 4096), 16*1024)
		for scnr.Scan() {
			logger.Debug("dsh.host: stdout", "line", scnr.Text())
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

	cli := New(baseURL, logger)
	if err := cli.Start(ctx); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		return nil, nil, fmt.Errorf("dsh.host: client start: %w", err)
	}
	return child, cli, nil
}

// spawnOnce is the watchdog's per-attempt spawn wrapper around
// spawnAndWire. Passes through the host's captured opts (which
// carry the port chosen by StartSharedHost — default 3080 or
// fallback from findFreePort).
func (h *SharedHost) spawnOnce() (*exec.Cmd, *Client, error) {
	return spawnAndWire(context.Background(), h.opts, h.opts.Port, h.logger)
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
