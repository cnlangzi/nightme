// Process lifecycle for the opencode bridge.
//
// serverProc owns the spawned `opencode serve` child. Spawning is
// slightly different from the other bridges because opencode picks its
// own port at start time and writes the bound URL to stdout:
//
//	opencode server listening on http://127.0.0.1:4096
//
// We pass `--port 0` so the server chooses a free port; that way we
// never collide with other instances on the same machine. The same
// trick is used by the opencode JS SDK (see packages/sdk/js/src/server.ts
// `createOpencodeServer`).
//
// Lifecycle:
//
//   startServer(ctx, cfg)
//     ├─ exec.CommandContext("opencode", "serve", "--hostname=127.0.0.1", "--port=0")
//     ├─ start child
//     ├─ read stdout line by line until match serverURLRegex
//     ├─ deadline = serverStartTimeout (10s)
//     └─ return *serverProc{cmd, baseURL, pid}
//
// Failure semantics:
//   - Spawn / pipe failure: returns the error immediately, no cleanup
//     needed (process never started).
//   - Banner timeout: kill the child, return ErrServerStartTimeout.
//   - Banner parse failure: kill the child, return a single error
//     containing the partial stdout for debugging.
package opencode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// serverURLRegex matches the line opencode emits on stdout when the
// HTTP server is ready:
//
//	opencode server listening on http://127.0.0.1:4096
//
// Capturing group 1 is the full URL. We deliberately stay loose on
// hostname / port — the server may pick either 127.0.0.1, localhost,
// or an IPv6 address depending on the platform.
var serverURLRegex = regexp.MustCompile(`opencode server listening on (https?://\S+)`)

// serverProc is the spawned `opencode serve` child. It owns the
// underlying *exec.Cmd and the baseURL the HTTP client should target.
type serverProc struct {
	cmd     *exec.Cmd
	baseURL string
	pid     int
}

// serverConfig is what the caller passes to startServer.
type serverConfig struct {
	workspace string   // cwd for the child process
	env       []string // extra env (e.g. OPENCODE_SERVER_PASSWORD)
	args      []string // extra argv appended after the canonical serve flags
}

// startServer spawns `opencode serve` and waits up to serverStartTimeout
// for the "opencode server listening on http://..." banner. On success
// it returns a *serverProc whose baseURL is the captured URL.
//
// The caller is responsible for calling proc.Close() (or proc.Kill())
// when the session ends. The cmd's stdout pipe is drained by the
// lifecycle goroutine after the banner is captured so the child cannot
// block on a full stderr buffer.
func startServer(ctx context.Context, cfg serverConfig) (*serverProc, error) {
	if cfg.workspace == "" {
		return nil, fmt.Errorf("opencode: workspace is required")
	}

	// Canonical argv. We always bind to localhost with --port 0 so the
	// server picks a free port. The bridge never needs to know the
	// port in advance — we parse it from the banner.
	args := []string{
		"serve",
		"--hostname=127.0.0.1",
		"--port=0",
	}
	args = append(args, cfg.args...)

	cmd := exec.CommandContext(ctx, "opencode", args...)
	// The server's cwd determines which instance context it picks
	// up — the InstanceStore maps (directory → provider/auth). The
	// user's opencode config lives at ~/.config/opencode (or
	// $XDG_CONFIG_HOME/opencode) and is read by the server boot
	// path, but the InstanceContext for /api/session/{id}/prompt
	// must be wired from a directory the user has set up.
	//
	// We spawn from the user's HOME rather than the workspace
	// because the workspace is typically a fresh git checkout
	// without any opencode auth. The workspace is then attached
	// per-session via the `x-opencode-directory` header and the
	// Session.directory field, so the server routes the request
	// to the right project.
	//
	// Operators who want a different default can set
	// NIGHTME_OPENCODE_HOME in the env. Empty HOME falls back to
	// the workspace (legacy behaviour).
	cmd.Dir = opencodeHomeDir(cfg.workspace)
	cmd.Env = append([]string(nil), cfg.env...)

	// Capture stdout so we can parse the banner. We close the stdout
	// reader after the banner is captured; the lifecycle goroutine
	// then re-opens it (or shares the same fd) only as a stderr tail.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("opencode: stdout pipe: %w", err)
	}
	// Open a separate stderr pipe so we can include stderr in the
	// error envelope if startup fails — useful for "port already in
	// use" / "no provider configured" diagnostic.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("opencode: stderr pipe: %w", err)
	}

	startErr := cmd.Start()
	if startErr != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("opencode: start: %w", startErr)
	}

	proc := &serverProc{
		cmd: cmd,
		pid: cmd.Process.Pid,
	}

	// Banner parsing + stderr tail run concurrently. We don't want a
	// noisy stderr to deadlock the child before the banner appears.
	var (
		baseURL    string
		bannerErr  error
		stderrTail strings.Builder
		wg         sync.WaitGroup
	)
	startTimeout := serverStartTimeout
	// NIGHTME_OPENCODE_INITIAL_DELAY overrides the start timeout for
	// local-regulator tests where the bridge should wait longer
	// (e.g. opencode pulls a model on first start). Values ≤ 0
	// disable the override (use the default).
	if v := os.Getenv("NIGHTME_OPENCODE_INITIAL_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			startTimeout = d
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		baseURL, bannerErr = scanBanner(stdout, startTimeout)
	}()

	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		// Drain stderr into stderrTail. We don't fail startup on
		// stderr noise — opencode writes info-level diagnostics freely.
		// Only the first 8 KiB is kept; the rest is dropped to bound
		// memory in the failure path.
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 && stderrTail.Len() < 8192 {
				stderrTail.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for the banner goroutine to finish. It exits either on
	// match, on EOF, or on timeout — whichever comes first.
	wg.Wait()

	if bannerErr == nil && baseURL == "" {
		// We hit the deadline without ever parsing the banner.
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		<-stderrDone
		stderr := stderrTail.String()
		if stderr != "" {
			return nil, fmt.Errorf("%w (after %s)\n--- stderr ---\n%s", ErrServerStartTimeout, startTimeout, stderr)
		}
		return nil, fmt.Errorf("%w (after %s)", ErrServerStartTimeout, startTimeout)
	}
	if bannerErr != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		<-stderrDone
		stderr := stderrTail.String()
		if !errors.Is(bannerErr, io.EOF) {
			if stderr != "" {
				return nil, fmt.Errorf("opencode: banner parse: %w\n--- stderr ---\n%s", bannerErr, stderr)
			}
			return nil, fmt.Errorf("opencode: banner parse: %w", bannerErr)
		}
		if stderr != "" {
			return nil, fmt.Errorf("opencode: server exited before banner\n--- stderr ---\n%s", stderr)
		}
		return nil, fmt.Errorf("opencode: server exited before banner")
	}

	proc.baseURL = baseURL
	oLog("server started", "pid", proc.pid, "base_url", baseURL, "workspace", cfg.workspace)
	return proc, nil
}

// scanBanner reads one line at a time from r, returning the URL captured
// by serverURLRegex on the first matching line. If the scan reaches
// timeout or EOF before a match, it returns the relevant error.
func scanBanner(r io.Reader, timeout time.Duration) (string, error) {
	scanner := bufio.NewScanner(r)
	// Generous buffer so a single banner line with a long URL does
	// not trip the default 64 KiB cap.
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			return "", nil
		default:
		}
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", err
			}
			return "", io.EOF
		}
		line := scanner.Text()
		if m := serverURLRegex.FindStringSubmatch(line); m != nil {
			return m[1], nil
		}
	}
}

// Close terminates the underlying *exec.Cmd. It first tries SIGINT
// (graceful) then SIGKILL after shutdownGrace. Idempotent. Safe to call
// after the process has already exited.
func (p *serverProc) Close() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	timer := time.AfterFunc(shutdownGrace, func() {
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
	defer timer.Stop()
	_, err := p.cmd.Process.Wait()
	return err
}

// opencodeHomeDir returns the directory the opencode server should
// be spawned from. The server's cwd drives the InstanceStore
// lookup, which is where the per-project provider/auth lives.
//
// Resolution order:
//
//  1. NIGHTME_OPENCODE_HOME env var (explicit override, useful for
//     CI / test fixtures).
//  2. cfg.workspace — the agent workspace passed in by nightme.
//     In practice the user also runs `opencode run ...` from
//     this directory, so it has opencode initialized (auth,
//     MCP config, providers). Spawning the server from here
//     guarantees the InstanceContext for the workspace is
//     reachable via the cwd-scoped InstanceStore.
//
// opencode 1.18's HTTP layer returns 500 ServeError on
// instance-scoped endpoints (/api/session/{id}/prompt) when the
// server's cwd doesn't have a valid InstanceContext. Working
// from the agent workspace (instead of HOME) matches the CLI's
// session model where the user's cwd is the runtime context.
func opencodeHomeDir(workspace string) string {
	if v := os.Getenv("NIGHTME_OPENCODE_HOME"); v != "" {
		return v
	}
	return workspace
}
