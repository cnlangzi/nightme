//go:build !windows

// lifecycle_internal_test.go — unit + integration tests for
// waitForListen, findFreePort, stderrRing, and the spawnAndWire
// failure-recovery path.
//
// Build tag !windows because the spawnAndWire integration test
// spawns a /bin/bash fake dsh. waitForListen / findFreePort /
// stderrRing use only stdlib and run on windows too, but
// co-locating them keeps the single regression-test target together.
//
// Lives in `package host` (internal) because the SUTs are
// unexported. External tests only exercise the public surface
// (StartSharedHost), which has its own coverage in watchdog_test.go.

package host

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// ─── stderrRing (unchanged from prior diagnostic-capture PR) ───

func TestStderrRing_AppendAndSnapshot(t *testing.T) {
	r := newStderrRing()
	r.append("a")
	r.append("b")
	r.append("c")
	snap := r.snapshot()
	want := []string{"a", "b", "c"}
	if len(snap) != len(want) {
		t.Fatalf("snapshot len = %d, want %d", len(snap), len(want))
	}
	for i := range want {
		if snap[i] != want[i] {
			t.Errorf("snapshot[%d] = %q, want %q", i, snap[i], want[i])
		}
	}
}

func TestStderrRing_OverflowBounds(t *testing.T) {
	r := newStderrRing()
	for i := 0; i < stderrCaptureCap*2; i++ {
		r.append(strconv.Itoa(i))
	}
	snap := r.snapshot()
	if len(snap) != stderrCaptureCap {
		t.Fatalf("snapshot len = %d, want %d", len(snap), stderrCaptureCap)
	}
	if snap[0] != strconv.Itoa(stderrCaptureCap) {
		t.Errorf("oldest = %q, want %d", snap[0], stderrCaptureCap)
	}
	if snap[len(snap)-1] != strconv.Itoa(stderrCaptureCap*2-1) {
		t.Errorf("newest = %q, want %d", snap[len(snap)-1], stderrCaptureCap*2-1)
	}
}

func TestStderrRing_NilSafe(t *testing.T) {
	var r *stderrRing
	r.append("line")
	if got := r.snapshot(); got != nil {
		t.Errorf("nil ring snapshot = %v, want nil", got)
	}
}

// ─── waitForListen (TCP-poll readiness) ───

// TestWaitForListen_HappyPath opens a real TCP listener on an
// ephemeral port, then calls waitForListen against it. The dial
// should succeed immediately; we close the listener after the
// first dial to verify the path returns cleanly.
func TestWaitForListen_HappyPath(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	if err := waitForListen(ctx, port); err != nil {
		t.Fatalf("waitForListen: %v", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("waitForListen took %v, want < 500ms", d)
	}
}

func TestWaitForListen_Timeout(t *testing.T) {
	// 0.0.0.0:1 is reserved and never accepts on a normal box;
	// the dial should fail with connection refused (or timeout if
	// some paranoid firewall blackholes it). Either way waitForListen
	// must surface a timeout error bounded by the ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := waitForListen(ctx, 1)
	if err == nil {
		t.Fatal("expected error from waitForListen on unreachable port")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("waitForListen ran %v, want < 500ms (ctx was 250ms)", d)
	}
}

// ─── findFreePort (fallback-port sweep) ───

func TestFindFreePort_FirstAvailable(t *testing.T) {
	// Bind port A; findFreePort scanning [A, B] should skip A
	// and return B. Both A and B are real, in-range ports.
	a, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen a: %v", err)
	}
	defer a.Close()
	portA := a.Addr().(*net.TCPAddr).Port

	// Pick B = portA+1; assume the +1 slot is free (overwhelmingly
	// likely in a test env — if not, the test will retry below).
	got, err := findFreePort(portA, portA+1)
	if err != nil {
		// extremely unlikely: +1 occupied. retry +2.
		got, err = findFreePort(portA, portA+2)
	}
	if err != nil {
		t.Fatalf("findFreePort: %v", err)
	}
	if got == portA {
		t.Errorf("findFreePort returned %d (occupied); expected to skip it", got)
	}
	if got < portA || got > portA+2 {
		t.Errorf("findFreePort returned %d, out of expected range [%d, %d]", got, portA, portA+2)
	}
}

func TestFindFreePort_AllOccupiedFails(t *testing.T) {
	// Bind a single port; scan the [port, port] range. findFreePort
	// must return an error because the only port in the range is
	// bound. This used to bind two OS-assigned ports and scan from
	// the smaller to the larger, which is flaky: nothing forces the
	// two assigned ports to be consecutive, so any gap between them
	// could be free and the test would falsely pass. Scanning a
	// single port removes the assumption.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	if _, err := findFreePort(port, port); err == nil {
		t.Errorf("findFreePort(%d, %d) succeeded; want error (port bound)", port, port)
	}
}

func TestFindFreePort_InvalidRange(t *testing.T) {
	if _, err := findFreePort(70000, 80000); err == nil {
		t.Error("findFreePort accepted out-of-range ports; want error")
	}
	if _, err := findFreePort(5000, 4000); err == nil {
		t.Error("findFreePort accepted start > end; want error")
	}
}

// ─── spawnAndWire (failure-recovery + stderr capture) ───

// TestSpawnAndWire_TimeoutIncludesDiagnostic verifies that when
// the spawned dsh subprocess never binds the requested port
// (because it sits in `sleep` indefinitely), the resulting error
// carries the captured stderr line count AND the subprocess is
// reaped cleanly. Without the diagnostic capture, the timeout
// branch used to discard everything (regression fixed by the
// diagnostic-capture PR; preserved by the TCP-listen refactor).
func TestSpawnAndWire_TimeoutIncludesDiagnostic(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-dsh-nolisten.sh")
	scriptBody := "#!/bin/bash\n" +
		"echo \"loading plugin foo\" >&2\n" +
		"echo \"loading plugin bar\" >&2\n" +
		"echo \"starting web server\" >&2\n" +
		"sleep 30\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}

	// webURLParseTimeout is a const (10s); use a tighter parent
	// ctx so the waitForListen timeout fires within the test
	// budget.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, _, err := spawnAndWire(ctx, SharedHostOptions{
		Workspace: dir,
		HostCmd:   script,
		Port:      4096, // arbitrary free port
	}, 4096, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	msg := err.Error()
	if !contains(msg, "dsh not listening") {
		t.Errorf("err = %v, want 'dsh not listening'", msg)
	}
	if !contains(msg, "stderr=") {
		t.Errorf("err = %v, want 'stderr=' count suffix", msg)
	}
	if !contains(msg, "4096") {
		t.Errorf("err = %v, want port 4096 in message", msg)
	}
	// Give the kernel a moment to deliver SIGCHLD after kill+wait.
	time.Sleep(100 * time.Millisecond)
}

// contains is a tiny helper because we don't need strings.Contains
// imported across the file just for these checks.
func contains(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// ─── sanitizeBaseURL (defense-in-depth for trailing quotes) ───

// TestSanitizeBaseURL_StripsTrailingQuote covers the defense for
// the `.../api/workspace/create%22` regression where a baseURL
// with a stray trailing `"` (typo from a config file or shell
// quote mishandling) gets URL-encoded into the dial path and the
// connection fails with the trailing quote glued to the endpoint.
//
// In production code baseURL is always constructed in-tree
// (lifecycle.go formats "http://127.0.0.1:%d"), so this is a
// belt-and-suspenders defense — but the regression was observable
// from older binaries in the wild, so we lock the behavior down
// with a unit test here.
func TestSanitizeBaseURL_StripsTrailingQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// The actual regression input that produced %22:
		{`http://127.0.0.1:3080"`, "http://127.0.0.1:3080"},
		// Already-clean URL passes through unchanged.
		{"http://127.0.0.1:3080", "http://127.0.0.1:3080"},
		// Trailing slash is stripped (unchanged behavior).
		{"http://127.0.0.1:3080/", "http://127.0.0.1:3080"},
		// Token-bearing URL keeps the token, strips trailing quote.
		{`http://127.0.0.1:3080/?token=abc"`, "http://127.0.0.1:3080/?token=abc"},
		// Single-quote form (shell quote mishap).
		{"http://127.0.0.1:3080'", "http://127.0.0.1:3080"},
		// Multiple trailing quotes (defensive — shouldn't happen).
		{`http://127.0.0.1:3080""`, "http://127.0.0.1:3080"},
		// Mixed trailing slash + quote.
		{`http://127.0.0.1:3080/"`, "http://127.0.0.1:3080"},
	}
	for _, c := range cases {
		got := sanitizeBaseURL(c.in)
		if got != c.want {
			t.Errorf("sanitizeBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
