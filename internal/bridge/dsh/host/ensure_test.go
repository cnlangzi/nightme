//go:build !windows

// ensure_test.go — tests for the lazy-start helper.
//
// Covers the three behaviors the runtime depends on:
//
//   - First call materializes the host (sync.Once fires exactly once).
//   - Subsequent calls return the cached client (no second spawn).
//   - A missing / failing binary surfaces a clear error.
//
// The tests use the same fake-dsh harness as watchdog_test.go so we
// can drive our own subprocess path via ForceSpawn=true and avoid
// poking at port 3080.

package host_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

// resetEnsureState clears the lazy-start state plus the two
// underlying singletons. Used by every test in this file so they
// can run in any order without leaking state.
func resetEnsureState() {
	host.UnsetGlobal()
	host.UnsetSharedHost()
	host.ResetEnsureForTest()
}

// TestEnsureSharedHost_FirstCallStarts — first call materializes the
// host and returns a non-nil client. The package's globalClient
// pointer is also populated, so any subsequent dsh.bridge call
// would see it.
func TestEnsureSharedHost_FirstCallStarts(t *testing.T) {
	resetEnsureState()
	t.Cleanup(resetEnsureState)

	fake := writeFakeDSH(t)
	dir := t.TempDir()

	cli, err := host.EnsureSharedHost(context.Background(), host.SharedHostOptions{
		Workspace:  dir,
		HostCmd:    fake,
		ForceSpawn: true,
	})
	if err != nil {
		t.Fatalf("EnsureSharedHost: %v", err)
	}
	if cli == nil {
		t.Fatal("EnsureSharedHost returned nil client")
	}
	if host.GetGlobal() == nil {
		t.Error("GetGlobal() still nil after EnsureSharedHost")
	}
	if host.GetSharedHost() == nil {
		t.Error("GetSharedHost() still nil after EnsureSharedHost")
	}
}

// TestEnsureSharedHost_SecondCallReturnsSame — sync.Once guarantees
// no second spawn. Calling EnsureSharedHost twice with the same
// (or different) opts returns the same *Client both times.
func TestEnsureSharedHost_SecondCallReturnsSame(t *testing.T) {
	resetEnsureState()
	t.Cleanup(resetEnsureState)

	fake := writeFakeDSH(t)
	dir := t.TempDir()

	first, err := host.EnsureSharedHost(context.Background(), host.SharedHostOptions{
		Workspace:  dir,
		HostCmd:    fake,
		ForceSpawn: true,
	})
	if err != nil {
		t.Fatalf("first EnsureSharedHost: %v", err)
	}

	// Second call with deliberately different opts — the Once
	// short-circuits, so opts are ignored.
	second, err := host.EnsureSharedHost(context.Background(), host.SharedHostOptions{
		Workspace:  "/tmp/unused",
		HostCmd:    "/nonexistent/path/__will_never_run__",
		ForceSpawn: true,
	})
	if err != nil {
		t.Fatalf("second EnsureSharedHost: %v", err)
	}
	if first != second {
		t.Errorf("second call returned a different *Client: first=%p second=%p",
			first, second)
	}
}

// TestEnsureSharedHost_ConcurrentFirstTouch — N goroutines race
// to the first call. Only one should actually invoke StartSharedHost;
// the rest must observe the cached client. The cheapest observable
// invariant is that all N callers receive the same *Client
// pointer.
func TestEnsureSharedHost_ConcurrentFirstTouch(t *testing.T) {
	resetEnsureState()
	t.Cleanup(resetEnsureState)

	fake := writeFakeDSH(t)
	dir := t.TempDir()

	const N = 8
	var wg sync.WaitGroup
	clis := make([]*host.Client, N)
	errs := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			cli, err := host.EnsureSharedHost(context.Background(), host.SharedHostOptions{
				Workspace:  dir,
				HostCmd:    fake,
				ForceSpawn: true,
			})
			clis[i] = cli
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
		if clis[i] == nil {
			t.Errorf("goroutine %d: nil client", i)
		}
	}
	// All clis must be the same pointer — proves the Once fired
	// once and StartSharedHost ran once.
	for i := 1; i < N; i++ {
		if clis[i] != clis[0] {
			t.Errorf("clis[%d] != clis[0]: %p vs %p", i, clis[i], clis[0])
		}
	}
}

// TestEnsureSharedHost_MissingBinary — a HostCmd that doesn't exist
// (e.g. dsh not installed on this machine) must surface an error
// to the caller, NOT a panic. The dsh bridge wraps this in its own
// message; the test confirms the underlying exec.LookPath error
// path is reachable.
func TestEnsureSharedHost_MissingBinary(t *testing.T) {
	resetEnsureState()
	t.Cleanup(resetEnsureState)

	// A path that can't exist on any reasonable system. Use a
	// nested path under t.TempDir() so we don't depend on
	// /nonexistent being unmapped by the test sandbox.
	binary := filepath.Join(t.TempDir(), "definitely-not-installed-dsh-binary")

	_, err := host.EnsureSharedHost(context.Background(), host.SharedHostOptions{
		Workspace:  t.TempDir(),
		HostCmd:    binary,
		ForceSpawn: true,
	})
	if err == nil {
		t.Fatal("EnsureSharedHost returned nil error for missing binary")
	}
	// Soft check: the error should mention the binary name (so
	// the user can debug "is dsh installed?"). Not a hard
	// assertion — exec.LookPath error format is OS-dependent.
	if !strings.Contains(err.Error(), "dsh") &&
		!strings.Contains(err.Error(), filepath.Base(binary)) {
		t.Logf("error message lacks binary name; err=%v", err)
	}
}
