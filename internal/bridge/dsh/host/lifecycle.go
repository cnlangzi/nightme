// lifecycle.go — process management for the shared dsh web daemon.
//
// In the shared-host architecture (F-dsh-shared-host), exactly ONE
// `dsh --profile web` subprocess is owned by the nightme daemon —
// started once at boot, kept alive for the daemon's lifetime,
// gracefully shut down on exit. This file owns the subprocess;
// the rest of the package (client.go, stream.go, router.go) talks
// to it over HTTP + WebSocket.
//
// Lifecycle model (always-spawn — see F-dsh-shared-host §1.3.1):
//
//	StartSharedHost(ctx, opts)
//	  1. TCP-dial 127.0.0.1:3080.
//	     - dial succeeds → 3080 is occupied by SOMETHING (could be
//	       another dsh, could be a foreign service — we don't
//	       care). Spawn our own on findFreePort(3081, 3099).
//	     - dial fails (refused/timeout) → 3080 is ours. Spawn there.
//	  2. spawnAndWire spawns `dsh --profile web --port <port>` and:
//	     a. parses the `?token=<launchToken>` from dsh's stdout
//	        URL line,
//	     b. GETs /?token=<launchToken> to mint the dsh-auth cookie
//	        (dsh 0.1.2-rc.1 returns 303 with set-cookie; without
//	        this step every /api/* and /api/events.* gets 401),
//	     c. builds an http.CookieJar populated with the cookie,
//	     d. constructs *Client with NewWithJar so both the HTTP
//	        RPC client and the WS dialer carry it,
//	     e. Client.Start kicks off mux/host WS pumps; the cookie
//	        is now attached to every upgrade.
//	  3. install client via host.SetGlobal so dsh.newDriver can
//	     find it. Start the watchdog.
//
// Why we no longer "reuse existing dsh": dsh 0.1.2-rc.1 enforces
// per-process signed-cookie auth on /api/* and /api/events.*. The
// launch token is process-internal and never exposed to a file,
// so we have no way to mint the cookie against someone else's dsh.
// Attaching to an external dsh therefore means RPC and WS fail
// with 401 — useless. The shared-host architecture is "nightme
// owns dsh"; the reuse-existing branch was an optimization that's
// no longer reachable in practice, so it's removed.
//
//	ShutdownSharedHost(ctx, client)
//	  1. Client.Close (stops mux/host pumps)
//	  2. SIGINT dsh, wait 5s, SIGKILL, wait 5s
package host

import (
	"bufio"
	"context"
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
	"regexp"
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

	// Step 1: pick a port. Canonical is 3080; if anything is
	// already listening there (dsh or foreign), sweep
	// [3081, 3099] for the first free port.
	port := defaultDSHPort
	if dialReachable(defaultDSHPort) {
		scanMin, scanMax := defaultPortScanMin, defaultPortScanMax
		found, scanErr := findFreePort(scanMin, scanMax)
		if scanErr != nil {
			return nil, fmt.Errorf(
				"dsh.host: port %d occupied and no free port in range %d-%d: %w",
				defaultDSHPort, scanMin, scanMax, scanErr)
		}
		port = found
		logger.Warn("dsh.host: port 3080 occupied; spawning on fallback",
			"foreign_port", defaultDSHPort,
			"fallback_port", port,
		)
	}

	// Step 2: spawn dsh with --port explicit. spawnAndWire also
	// captures the launch token and mints the dsh-auth cookie
	// before constructing the Client.
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
// /api/* and /api/events.* call carries it.
//
// baseURL is the dsh root WITHOUT the token query (e.g.
// "http://127.0.0.1:3080"). token is the launch token printed on
// dsh's stdout. Returned jar is populated; never nil unless an
// error is also returned.
//
// Why this is necessary: dsh 0.1.2-rc.1 auth-gates /api/* and the
// two /api/events.* WS endpoints with per-process signed cookies.
// The launch token only works on the initial GET / — it mints the
// cookie. Without this step, every bridge call gets 401 and every
// WS upgrade closes mid-handshake.
func mintAuthCookie(ctx context.Context, baseURL, token string) (http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: cookiejar: %w", err)
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: parse base url %q: %w", baseURL, err)
	}
	q := *u
	q.RawQuery = "token=" + token
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("dsh.host: build token-exchange req: %w", err)
	}
	// We deliberately do NOT pass the jar — the cookiejar is
	// populated from this single response, not sent on it.
	client := &http.Client{
		Timeout: httpClientTimeout,
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

	// waitForURLToken reads stdout until the URL line appears and
	// captures the launch token, while continuing to drain the pipe
	// in the background so dsh's stdout never deadlocks.
	//
	// We can't share the pipe between two goroutines — once one
	// reads, the bytes are gone. So one goroutine does both: parse
	// the URL line for the token, log every line for /diagnose
	// triage, keep draining until EOF.
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

	// Mint the dsh-auth cookie from the launch token. Without this
	// step dsh 0.1.2-rc.1 401s every /api/* and every WS upgrade.
	tokenCtx, tokenCancel := context.WithTimeout(ctx, webURLParseTimeout)
	token, err := func() (string, error) {
		select {
		case t := <-tokenCh:
			return t, nil
		case e := <-tokenErrCh:
			return "", e
		case <-tokenCtx.Done():
			return "", fmt.Errorf("dsh.host: timeout waiting for launch token: %w", tokenCtx.Err())
		}
	}()
	if err != nil {
		tokenCancel()
		_ = child.Process.Kill()
		_ = child.Wait()
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("dsh.host: capture launch token: %w", err)
	}

	jar, err := mintAuthCookie(tokenCtx, baseURL, token)
	tokenCancel()
	if err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("dsh.host: mint dsh-auth cookie: %w", err)
	}

	cli := NewWithJar(baseURL, jar, logger)
	if err := cli.Start(ctx); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		return nil, nil, fmt.Errorf("dsh.host: client start: %w", err)
	}
	return child, cli, nil
}

// spawnOnce is the watchdog's per-attempt spawn wrapper around
// spawnAndWire.
//
// Port policy: try the port we captured at StartSharedHost first
// (h.opts.Port). If it's now occupied (e.g. another daemon took
// 3080 while our dsh was down, or the port is wedged), fall back
// to findFreePort(3081, 3099) — same policy StartSharedHost uses
// for cold start. This keeps the watchdog from giving up just
// because the canonical port got stolen during the dead window.
func (h *SharedHost) spawnOnce() (*exec.Cmd, *Client, error) {
	port := h.opts.Port
	if !dialReachable(port) {
		if found, err := findFreePort(defaultPortScanMin, defaultPortScanMax); err == nil {
			port = found
			h.logger.Warn("dsh.host: captured port occupied during respawn; falling back",
				"captured_port", h.opts.Port,
				"respawn_port", port,
			)
		}
	}
	return spawnAndWire(context.Background(), h.opts, port, h.logger)
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
