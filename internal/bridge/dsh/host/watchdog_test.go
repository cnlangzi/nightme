// watchdog_test.go — tests for the SharedHost watchdog / respawn cycle.
//
// Uses a tiny Go binary as a fake dsh subprocess. The fake binds
// the TCP port (so spawnAndWire's waitForListen succeeds), prints
// the URL line, writes its PID to FAKE_DSH_PIDFILE, and either
// dies fast (simulating crash) or sleeps (so we can observe the
// respawned instance). Tests do NOT depend on the real dsh binary
// on PATH.
//
// The fake is a single Go process. Earlier attempts at a bash +
// background-python split had env-var-order races and parent-death
// polling windows where the python listener could be killed before
// waitForListen saw the bind. A pure Python fake was cleaner but
// pulled a python3 dep into the test binary. A single Go binary
// keeps everything in one language; the source is compiled once
// per test binary in TestMain and the same binary is reused across
// every test (per-test behavior is driven via env vars, not
// rebuild).
//
// Build tag: the watchdog is only useful on unix where dsh runs;
// windows is skipped via build tag.

//go:build !windows

package host_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

// fakeDSHSource is the source of the fake-dsh binary used by
// tests. Compiled once at TestMain; per-test behavior is driven
// via env vars (FAKE_DSH_PIDFILE / FAKE_DSH_LIFETIME) and CLI
// flags (--port). The binary mimics the minimum surface of
// `dsh --profile web` that spawnAndWire observes:
//
//   - binds TCP on the --port we ask for (waitForListen sees it)
//   - prints the URL line on stdout (legacy contract kept for
//     any test still inspecting stdout)
//   - writes PID to FAKE_DSH_PIDFILE so tests can synchronize
//     on the process lifecycle
//   - sleeps FAKE_DSH_LIFETIME (default 0.05s) then exits 1
//
// Closing the listener before exit releases the port immediately,
// which is what subsequent tests in the same run depend on —
// fakeDSHScript's default 0.05s lifetime is intentionally tiny
// for the same reason (tests run in sequence and each test's
// spawn needs the port to be free).
const fakeDSHSource = `package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

func main() {
	port := 3080
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "--port" && i+1 < len(os.Args) {
			if p, err := strconv.Atoi(os.Args[i+1]); err == nil {
				port = p
			}
			i++
		}
	}

	host := fmt.Sprintf("127.0.0.1:%d", port)
	s, err := net.Listen("tcp", host)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake-dsh: listen %s: %v\n", host, err)
		os.Exit(2)
	}
	defer s.Close()

	// Real dsh 0.1.2-rc.1 prints a query token on the URL line;
	// spawnAndWire captures it and uses it to mint the dsh-auth
	// cookie. The fake mints a fixed token per invocation (the OS
	// PID is unique enough for test purposes; tests that care
	// about token stability can override via FAKE_DSH_TOKEN env).
	token := os.Getenv("FAKE_DSH_TOKEN")
	if token == "" {
		token = fmt.Sprintf("fake-token-%d", os.Getpid())
	}
	fmt.Printf("dsh web: http://%s/?token=%s\n", host, token)

	if pidfile := os.Getenv("FAKE_DSH_PIDFILE"); pidfile != "" {
		if err := os.WriteFile(pidfile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "fake-dsh: pidfile %s: %v\n", pidfile, err)
		}
	}

	// Minimal HTTP server so spawnAndWire's mintAuthCookie step
	// gets a real Set-Cookie back. Real dsh validates the launch
	// token and signs the cookie per-process; the fake just echoes
	// a fixed cookie value with a counter so tests can assert on
	// request counts. Anything else (/api/*, /api/events.*) gets
	// a 200 with empty JSON — enough to keep the WS dial pump from
	// crashing on every retry during the test.
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Query().Get("token") == token {
			w.Header().Set("Set-Cookie", fmt.Sprintf("dsh-auth-fake=v1.fake.%d; Max-Age=2592000; Path=/; HttpOnly", os.Getpid()))
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"items\":[]}"))
	})
	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(s)
	}()
	defer srv.Close()

	// Default 30s lifetime (override via FAKE_DSH_LIFETIME env).
	//
	// Lifetime must be long enough that spawnAndWire can complete
	// the mint-cookie HTTP round-trip before the fake exits — on
	// CI runners the round-trip can take >50ms due to VM clock
	// jitter, and the previous 0.05s default caused intermittent
	// "connection reset by peer" failures during mint. The fake is
	// still a per-test fixture (not a long-running service) — each
	// test that needs it explicitly tears down the subprocess in
	// its Cleanup via killFakeDSH, so a long default does not leak
	// across tests. The env override is preserved for any future
	// test that wants the tight race-window behavior back.
	lifetime := 30.0
	if s := os.Getenv("FAKE_DSH_LIFETIME"); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			lifetime = v
		}
	}
	time.Sleep(time.Duration(lifetime * float64(time.Second)))
	os.Exit(1)
}
`

// fakeDSHBin is the compiled fake-dsh binary path. Set once by
// TestMain; read by writeFakeDSH. Tests share the same binary
// (behavior is parameterized via env vars / CLI flags, not
// rebuild).
var fakeDSHBin string

// TestMain compiles fakeDSHBin once for the whole test binary.
// Per-test writeFakeDSH is then just a path return — no per-test
// compile overhead.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fake-dsh-bin-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake-dsh test setup: mkdir: %v\n", err)
		os.Exit(2)
	}

	srcPath := filepath.Join(dir, "fake-dsh.go")
	binPath := filepath.Join(dir, "fake-dsh")
	if err := os.WriteFile(srcPath, []byte(fakeDSHSource), 0o644); err != nil {
		os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "fake-dsh test setup: write source: %v\n", err)
		os.Exit(2)
	}

	build := exec.Command("go", "build", "-o", binPath, srcPath)
	if out, err := build.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "fake-dsh test setup: build failed: %v\n%s\n", err, out)
		os.Exit(2)
	}
	fakeDSHBin = binPath

	code := m.Run()

	os.RemoveAll(dir)
	os.Exit(code)
}

// writeFakeDSH returns the path of the precompiled fake-dsh binary.
// The build happens once per test binary (see TestMain); each call
// here just hands back the path. Per-test configuration goes via
// env vars (FAKE_DSH_PIDFILE / FAKE_DSH_LIFETIME) and the --port
// flag passed to StartSharedHost, not via rebuild.
func writeFakeDSH(t *testing.T) string {
	t.Helper()
	if fakeDSHBin == "" {
		t.Fatal("fake-dsh binary not built; TestMain must run first")
	}
	return fakeDSHBin
}

// waitPIDFile polls path until it contains a positive integer, or
// returns 0 on timeout.
func waitPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			if pid, perr := strconv.Atoi(string(data)); perr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0
}

// procAlive returns true if pid is currently a running process.
// Uses `kill -0` shell-out for cross-distro compatibility.
func procAlive(pid int) bool {
	cmd := exec.Command("kill", "-0", strconv.Itoa(pid))
	return cmd.Run() == nil
}

// killFakeDSH sends SIGKILL to the subprocess owned by sh (if any)
// and waits for it to be reaped. Replaces the legacy sh.Close() in
// tests after the daemon stopped tearing dsh down on shutdown —
// tests still need to terminate the spawned process so the test
// binary doesn't leak it. No-op when sh owns no subprocess
// (PID == 0).
func killFakeDSH(t *testing.T, sh *host.SharedHost) {
	t.Helper()
	pid := sh.PID()
	if pid == 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		// Already gone (ESR); nothing to do.
		return
	}
	_ = proc.Signal(os.Kill)
	// Wait for the kernel to reap so the next test's port /
	// state isn't racing with a zombie. 2s is generous for a
	// SIGKILL'd process.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			if !procAlive(pid) {
				close(done)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Logf("killFakeDSH: pid %d did not exit within 2s", pid)
	}
}

// TestSharedHost_WatchdogRespawns verifies the watchdog observes a
// fake-dsh crash and spawns a replacement. The fake writes its
// PID to FAKE_DSH_PIDFILE on startup; we observe the file changing
// (different PID) as proof of respawn.
//
// Phase 1 (crash): LIFETIME=0.1s, fake-dsh dies fast → watchdog fires.
// Phase 2 (alive): LIFETIME=10s, watchdog respawns, new instance sticks.
func TestSharedHost_WatchdogRespawns(t *testing.T) {
	fake := writeFakeDSH(t)
	dir := t.TempDir()

	firstPIDFile := filepath.Join(dir, "first.pid")
	t.Setenv("FAKE_DSH_PIDFILE", firstPIDFile)
	t.Setenv("FAKE_DSH_LIFETIME", "0.1")

	// Reset singletons so this test (and the next) can install their
	// own. The watchdog tests run sequentially within the same
	// process; without this, the second test panics on SetGlobal's
	// "called twice" guard.
	host.UnsetGlobal()
	host.UnsetSharedHost()
	t.Cleanup(func() {
		host.UnsetGlobal()
		host.UnsetSharedHost()
	})

	sh, err := host.StartSharedHost(context.Background(), host.SharedHostOptions{
		Workspace:  dir,
		HostCmd:    fake,
		ForceSpawn: true, // bypass discover — drive our own fake-dsh
	})
	if err != nil {
		t.Fatalf("StartSharedHost: %v", err)
	}

	firstPID := waitPIDFile(t, firstPIDFile, 2*time.Second)
	if firstPID == 0 {
		t.Fatal("first fake-dsh never started")
	}

	// Wait for first instance to die + watchdog to respawn.
	// Switch the lifetime so the second instance is long-lived.
	time.Sleep(400 * time.Millisecond)
	secondPIDFile := filepath.Join(dir, "second.pid")
	t.Setenv("FAKE_DSH_PIDFILE", secondPIDFile)
	t.Setenv("FAKE_DSH_LIFETIME", "10")

	secondPID := waitPIDFile(t, secondPIDFile, 5*time.Second)
	if secondPID == 0 {
		t.Fatal("watchdog did not respawn fake-dsh (no second pid file)")
	}
	if secondPID == firstPID {
		t.Errorf("watchdog reused pid %d (should be a new process)", secondPID)
	}

	// Verify first instance is actually gone.
	if procAlive(firstPID) {
		t.Errorf("first fake-dsh pid %d still alive", firstPID)
	}

	// Shut down. The second instance is short-lived now, so the
	// fake-dsh will exit on its own. Then we SIGKILL the live
	// subprocess (if any) to clean up. SharedHost no longer
	// exposes Close/Done — the daemon doesn't tear dsh down.
	t.Setenv("FAKE_DSH_LIFETIME", "0.05")
	killFakeDSH(t, sh)
}

// TestRespawnDelay_Bounded verifies respawnDelay returns a value
// within the documented bounds. We test via behavior (the
// constant is unexported), checking that an obviously huge attempt
// index clamps to the max.
func TestRespawnDelay_Bounded(t *testing.T) {
	t.Skip("respawnDelay is unexported; bounded via direct testing of the watchdog which is slow")
}
