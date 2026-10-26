package pi

import (
	"context"
	"io"
	"os"
	"testing"
)

// TestKeepalive_ClosedRPCSessionRecovers covers the "process
// lingered but the RPC session tore down" case. The PID probe
// alone reports the bridge as healthy; the closed RPC client is
// what makes it unusable.
func TestKeepalive_ClosedRPCSessionRecovers(t *testing.T) {
	d := &driver{
		pid: os.Getpid(),
		rpc: newRPCClient(nopCloser{Writer: io.Discard}),
	}
	d.rpc.failPending(io.EOF)

	called := false
	if err := d.Keepalive(context.Background(), func(context.Context) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("Keepalive() = %v, want nil", err)
	}
	if !called {
		t.Fatal("Keepalive did not recover a bridge with a closed RPC session")
	}
}

// TestKeepalive_LiveSessionIsHealthy covers the happy path: a
// bridge whose PID is alive and whose RPC session is open must
// not trigger recovery.
func TestKeepalive_LiveSessionIsHealthy(t *testing.T) {
	d := &driver{
		pid: os.Getpid(),
		rpc: newRPCClient(nopCloser{Writer: io.Discard}),
	}
	called := false
	if err := d.Keepalive(context.Background(), func(context.Context) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("Keepalive() = %v, want nil", err)
	}
	if called {
		t.Fatal("Keepalive invoked recovery on a live bridge")
	}
}

// TestKeepalive_NoRecoveryCallback verifies the error path when
// the caller supplies no recovery callback.
func TestKeepalive_NoRecoveryCallback(t *testing.T) {
	d := &driver{
		pid: os.Getpid(),
		rpc: newRPCClient(nopCloser{Writer: io.Discard}),
	}
	d.rpc.failPending(io.EOF)
	if err := d.Keepalive(context.Background(), nil); err == nil {
		t.Fatal("Keepalive() = nil, want an error for a dead bridge with no callback")
	}
}

// TestKeepalive_NoRPCSessionDoesNotPanic pins the not-yet-started
// case: a driver without an RPC client has no death signal, so
// the PID probe decides — and it must not panic on the nil client.
func TestKeepalive_NoRPCSessionDoesNotPanic(t *testing.T) {
	d := &driver{pid: os.Getpid()}
	called := false
	if err := d.Keepalive(context.Background(), func(context.Context) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("Keepalive() = %v, want nil", err)
	}
	if called {
		t.Fatal("Keepalive invoked recovery on a bridge with no death signal")
	}
}
