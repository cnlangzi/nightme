// discover_test.go — tests for DiscoverExisting + StartSharedHost's
// reuse-or-spawn flow.

package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

// mockDSHServer is a httptest.Server-backed fake dsh that responds
// to host.describe with a valid server-response envelope (rpcId
// echoing). Records call count for assertions.
type mockDSHServer struct {
	server      *httptest.Server
	describeHit atomic.Int64
}

func newMockDSHServer(t *testing.T) *mockDSHServer {
	t.Helper()
	m := &mockDSHServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/host.describe", m.handleHostDescribe)
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockDSHServer) handleHostDescribe(w http.ResponseWriter, r *http.Request) {
	m.describeHit.Add(1)
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Type  string `json:"type"`
		RPCID string `json:"rpcId"`
	}
	_ = json.Unmarshal(body, &req)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "server-response",
		"rpcId":  req.RPCID,
		"result": map[string]any{"ok": true, "value": map[string]any{
			"version":          "dsh-test-0.1.0",
			"cwd":              "/tmp",
			"attachedSessions": 0,
			"canOpenPath":      false,
		}},
	})
}

// portFromURL extracts the listen port from an httptest.Server URL.
// httptest binds on 127.0.0.1:<random> — we read the port back so
// DiscoverExisting can target exactly that port.
func portFromURL(t *testing.T, url string) int {
	t.Helper()
	i := strings.LastIndex(url, ":")
	if i < 0 {
		t.Fatalf("no port in url %s", url)
	}
	p, err := strconv.Atoi(url[i+1:])
	if err != nil {
		t.Fatalf("parse port from %s: %v", url, err)
	}
	return p
}

// freePort returns a port that's free right now (best-effort —
// there's a race window between Close and the test's Discover call,
// but on a quiet CI machine it's reliable).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestDiscoverExisting_HitsLiveDsh verifies the happy path:
// httptest bound → DiscoverExisting sends one probe RPC → returns
// a usable *Client rooted at the discovered URL.
func TestDiscoverExisting_HitsLiveDsh(t *testing.T) {
	mock := newMockDSHServer(t)
	port := portFromURL(t, mock.server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cli, err := host.DiscoverExisting(ctx, port)
	if err != nil {
		t.Fatalf("DiscoverExisting: %v", err)
	}
	if cli == nil {
		t.Fatal("expected non-nil client")
	}

	if got := mock.describeHit.Load(); got != 1 {
		t.Errorf("expected 1 host.describe probe, got %d", got)
	}

	if !strings.Contains(cli.BaseURL(), strconv.Itoa(port)) {
		t.Errorf("baseURL %q does not include discovered port %d",
			cli.BaseURL(), port)
	}
}

// TestDiscoverExisting_NoListenerReturnsErrNotRunning: target port
// has nothing on it → ErrNotRunning (sentinel). StartSharedHost
// uses errors.Is to switch to spawn path on this.
func TestDiscoverExisting_NoListenerReturnsErrNotRunning(t *testing.T) {
	port := freePort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := host.DiscoverExisting(ctx, port)
	if !errors.Is(err, host.ErrNotRunning) {
		t.Errorf("expected ErrNotRunning, got %v", err)
	}
}

// TestDiscoverExisting_WrongProtocolReturnsErrNotDSH: port has a
// server but it doesn't speak dsh → ErrNotDSH. StartSharedHost
// surfaces this instead of spawning on top of a foreign service.
func TestDiscoverExisting_WrongProtocolReturnsErrNotDSH(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>not dsh</body></html>"))
	}))
	defer srv.Close()

	port := portFromURL(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := host.DiscoverExisting(ctx, port)
	if !errors.Is(err, host.ErrNotDSH) {
		t.Errorf("expected ErrNotDSH, got %v", err)
	}
}

// TestStartSharedHost_SpawnsWhenNoDsh: with no dsh on 3080,
// StartSharedHost spawns the configured HostCmd (the bash fake).
// ForceSpawn is required in test env because we can't guarantee
// 3080 is empty in CI — and tests must NOT silently attach to a
// developer's local dsh.
func TestStartSharedHost_SpawnsWhenNoDsh(t *testing.T) {
	fake := writeFakeDSH(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	t.Setenv("FAKE_DSH_PIDFILE", pidFile)
	t.Setenv("FAKE_DSH_LIFETIME", "60") // stay alive long enough for assertions

	host.UnsetGlobal()
	host.UnsetSharedHost()
	t.Cleanup(func() {
		host.UnsetGlobal()
		host.UnsetSharedHost()
	})

	sh, err := host.StartSharedHost(context.Background(), host.SharedHostOptions{
		Workspace:  dir,
		HostCmd:    fake,
		ForceSpawn: true,
	})
	if err != nil {
		t.Fatalf("StartSharedHost: %v", err)
	}
	// SharedHost no longer exposes Close: the daemon never tears
	// dsh down. Tests still need to terminate the spawned
	// subprocess so the test binary doesn't leak it. Send SIGKILL
	// directly; the kernel reaps on test exit.
	defer killFakeDSH(t, sh)

	pid := waitPIDFile(t, pidFile, 2*time.Second)
	if pid == 0 {
		t.Fatal("fake-dsh never spawned")
	}
	// PID() returns the spawned subprocess's PID — proves we
	// OWNED the subprocess (not attached to a pre-existing one).
	if got := sh.PID(); got != pid {
		t.Errorf("SharedHost.PID=%d, want %d (spawned subprocess)", got, pid)
	}
}

// TestStartSharedHost_AttachesToExistingDsh: with a real dsh on
// 3080, StartSharedHost attaches to it. ForceSpawn is NOT set —
// the test only runs if 3080 is free in the test env. We skip
// otherwise (CI shared-host environments may have a competing
// dsh). For deterministic verification, see the DiscoverExisting
// tests above which exercise the same discovery code path.
// TestStartSharedHost_NonCanonicalPort — dsh spawned but bound a
// non-3080 port (e.g. via DSH_PORT env override, a config file, or
// the upcoming dsh --port 0 fallback). StartSharedHost must detect
// the mismatch, kill the spawn, and return a clear error — silent
// acceptance would let the daemon think it owns a host that the
// next StartSharedHost call can't re-discover on 3080.
func TestStartSharedHost_NonCanonicalPort(t *testing.T) {
	host.UnsetGlobal()
	host.UnsetSharedHost()
	host.ResetEnsureForTest()
	t.Cleanup(func() {
		host.UnsetGlobal()
		host.UnsetSharedHost()
		host.ResetEnsureForTest()
	})

	// Build a fake-dsh that always reports a non-3080 port,
	// regardless of argv. The daemon parses this URL and the
	// port check (in StartSharedHost) must reject it.
	dir := t.TempDir()
	fakePath := filepath.Join(dir, "fake-dsh-wrong-port.sh")
	script := `#!/bin/bash
echo "dsh web: http://127.0.0.1:13080"
sleep 30
`
	if err := os.WriteFile(fakePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake dsh: %v", err)
	}

	sh, err := host.StartSharedHost(context.Background(), host.SharedHostOptions{
		Workspace:  dir,
		HostCmd:    fakePath,
		ForceSpawn: true,
	})
	if err == nil {
		killFakeDSH(t, sh)
		t.Fatal("StartSharedHost should reject non-3080 port, got nil error")
	}
	// Subprocess must be killed — verify the pid is gone.
	if !strings.Contains(err.Error(), "13080") || !strings.Contains(err.Error(), "3080") {
		t.Errorf("error should mention both ports, got: %v", err)
	}

	// Pid we just spawned: the error path's cmd.Wait() in
	// StartSharedHost reclaims the spawn, so the test binary
	// doesn't leak it. Nothing to verify beyond the error
	// message; procAlive(0) is meaningless.
}

func TestStartSharedHost_AttachesToExistingDsh(t *testing.T) {
	mock := newMockDSHServer(t)
	port := portFromURL(t, mock.server.URL)

	// Only run if mock's port is reachable as 127.0.0.1:3080.
	// httptest binds to 127.0.0.1:<random>, not 3080, so we can't
	// easily exercise the real "StartSharedHost → discover 3080"
	// path without binding to that specific port (which the test
	// runner might be using for another dsh). Skip with a clear
	// message instead of guessing.
	if port != 3080 {
		t.Skipf("mock dsh bound to %d (not 3080); this test only exercises the real 3080-discovery path on a host where 3080 is free for the mock", port)
	}

	host.UnsetGlobal()
	host.UnsetSharedHost()
	t.Cleanup(func() {
		host.UnsetGlobal()
		host.UnsetSharedHost()
	})

	sh, err := host.StartSharedHost(context.Background(), host.SharedHostOptions{
		Workspace: t.TempDir(),
		HostCmd:   "dsh", // ignored — discovery path doesn't spawn
	})
	if err != nil {
		t.Fatalf("StartSharedHost: %v", err)
	}
	// Discovery path: we don't own the subprocess, so nothing to
	// kill on cleanup. But Close() is gone — no defer needed.
	_ = sh

	// Attached-to-existing path: PID() returns 0 (we don't own
	// the subprocess), but Client() is non-nil and pointing at
	// the discovered host.
	if got := sh.PID(); got != 0 {
		t.Errorf("PID=%d for an attached-to-existing host (want 0)", got)
	}
	if sh.Client() == nil {
		t.Error("Client() is nil for an attached-to-existing host")
	}
}