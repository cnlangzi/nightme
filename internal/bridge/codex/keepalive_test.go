package codex

import (
	"context"
	"io"
	"os"
	"testing"
)

// nopRW stands in for the codex app-server connection. The
// Keepalive tests never put bytes on the wire — they only need an
// rpcClient whose closed flag they can flip.
type nopRW struct{}

func (nopRW) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopRW) Write(p []byte) (int, error) { return len(p), nil }

// TestKeepalive_ClosedRPCSessionRecovers covers the "process
// lingered but the RPC session tore down" case. The PID probe
// alone reports the bridge as healthy; the closed RPC client is
// what makes it unusable.
func TestKeepalive_ClosedRPCSessionRecovers(t *testing.T) {
	d := &driver{
		session: &session{
			pid: os.Getpid(),
			rpc: newRPCClient(nopRW{}, nil, nil),
		},
	}
	d.session.rpc.failPending(ErrSessionClosed)

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
		session: &session{
			pid: os.Getpid(),
			rpc: newRPCClient(nopRW{}, nil, nil),
		},
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
		session: &session{
			pid: os.Getpid(),
			rpc: newRPCClient(nopRW{}, nil, nil),
		},
	}
	d.session.rpc.failPending(ErrSessionClosed)
	if err := d.Keepalive(context.Background(), nil); err == nil {
		t.Fatal("Keepalive() = nil, want an error for a dead bridge with no callback")
	}
}

// TestKeepalive_NoSessionDoesNotPanic pins the not-yet-started
// case: a driver without a session has no PID to probe, so it
// counts as dead — but the probe must not panic on the nil
// session.
func TestKeepalive_NoSessionDoesNotPanic(t *testing.T) {
	d := &driver{}
	if err := d.Keepalive(context.Background(), nil); err == nil {
		t.Fatal("Keepalive() = nil, want an error for a driver with no session")
	}
}
