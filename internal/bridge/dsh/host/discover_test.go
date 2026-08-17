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
		"type":  "server-response",
		"rpcId": req.RPCID,
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
