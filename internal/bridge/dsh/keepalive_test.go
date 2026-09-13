// keepalive_test.go — pins the per-driver d.cli keepalive contract.
//
// Context (per docs/bridge/dsh-shared-host.md §7.6 and the fix
// tracked in this PR): dsh.driver.Keepalive used to read
// host.GetGlobal() to check whether the dsh backend was alive.
// After the foreign-dsh-attached fallback path replaces the
// process-global *Client via host.ReplaceGlobal, that lookup
// would always see the NEW cli's Done() — even when the OLD
// cli's Done() is the one that just fired. Drivers whose
// d.cli still pointed at the dead old client would never
// observe the failure and never call onRecover → the runtime
// would hang on stale d.cli forever.
//
// The fix: dsh.driver.Keepalive uses d.cli (per-driver, set at
// handshake time) instead of host.GetGlobal(). The fallback
// path's oldCli.Close() fires d.cli.Done() on every pre-fallback
// driver, the next keepalive tick observes it, onRecover runs,
// and the existing spawner.Spawn path rebuilds the AgentSession
// handle on the new cli — with dsh's idempotent
// session.create({sessionId, cwd}) preserving the sessionId.

package dsh

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

// TestKeepalive_OpenCLI_DoesNotCallOnRecover pins the happy
// path: d.cli is alive (Done not closed), Keepalive returns
// nil without calling onRecover.
func TestKeepalive_OpenCLI_DoesNotCallOnRecover(t *testing.T) {
	cli := host.New("http://127.0.0.1:1", slog.Default())
	t.Cleanup(cli.Close)

	d := &driver{cli: cli}

	called := make(chan struct{}, 1)
	onRecover := func(ctx context.Context) error {
		called <- struct{}{}
		return nil
	}

	if err := d.Keepalive(context.Background(), onRecover); err != nil {
		t.Fatalf("Keepalive on open cli: %v", err)
	}
	select {
	case <-called:
		t.Fatal("onRecover called while d.cli is alive")
	default:
		// expected
	}
}

// TestKeepalive_ClosedCLI_CallsOnRecover is the post-fallback
// invariant: after oldCli.Close() (the fallback's last step),
// d.cli.Done() is closed, the next Keepalive tick fires
// onRecover. This is the path that lets the runtime's existing
// spawner.Spawn rebuild a fresh *dsh.driver on the new cli.
func TestKeepalive_ClosedCLI_CallsOnRecover(t *testing.T) {
	cli := host.New("http://127.0.0.1:1", slog.Default())

	d := &driver{cli: cli}

	called := make(chan struct{}, 1)
	onRecover := func(ctx context.Context) error {
		called <- struct{}{}
		return nil
	}

	// Close the cli. Done() is now closed.
	cli.Close()

	// Keepalive should call onRecover.
	if err := d.Keepalive(context.Background(), onRecover); err != nil {
		t.Fatalf("Keepalive on closed cli: %v", err)
	}
	select {
	case <-called:
		// expected
	case <-time.After(time.Second):
		t.Fatal("onRecover not called on closed cli (regression: Keepalive probably still reads host.GetGlobal)")
	}
}

// TestKeepalive_NilOnRecover_ReturnsError pins the contract
// that a nil onRecover callback is a programmer error, not a
// silent no-op.
func TestKeepalive_NilOnRecover_ReturnsError(t *testing.T) {
	cli := host.New("http://127.0.0.1:1", slog.Default())
	t.Cleanup(cli.Close)

	d := &driver{cli: cli}

	if err := d.Keepalive(context.Background(), nil); err == nil {
		t.Fatal("Keepalive with nil onRecover should return an error")
	}
}

// TestKeepalive_NilCLI_CallsOnRecover pins the no-cli path: a
// driver with d.cli == nil (handshake not yet started) treats
// the state as "no backend" and fires onRecover so the
// chat layer can re-run EnsureSharedHost. Mirrors the pre-fix
// behavior of the global-lookup path.
func TestKeepalive_NilCLI_CallsOnRecover(t *testing.T) {
	d := &driver{cli: nil}

	called := make(chan struct{}, 1)
	onRecover := func(ctx context.Context) error {
		called <- struct{}{}
		return nil
	}

	if err := d.Keepalive(context.Background(), onRecover); err != nil {
		t.Fatalf("Keepalive on nil cli: %v", err)
	}
	select {
	case <-called:
		// expected
	case <-time.After(time.Second):
		t.Fatal("onRecover not called on nil cli")
	}
}

// TestKeepalive_OnRecoverError_Propagates ensures errors
// returned by onRecover (e.g. EnsureSharedHost failed) bubble
// up so the chat layer can log/handle them rather than silently
// dropping the failure.
func TestKeepalive_OnRecoverError_Propagates(t *testing.T) {
	cli := host.New("http://127.0.0.1:1", slog.Default())
	cli.Close()
	d := &driver{cli: cli}

	wantErr := errors.New("recover boom")
	onRecover := func(ctx context.Context) error {
		return wantErr
	}

	got := d.Keepalive(context.Background(), onRecover)
	if got == nil {
		t.Fatal("expected error to propagate, got nil")
	}
	if got.Error() != wantErr.Error() {
		t.Errorf("Keepalive error = %v, want %v", got, wantErr)
	}
}
