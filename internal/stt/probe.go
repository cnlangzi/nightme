package stt

import (
	"context"
	"errors"
	"fmt"
)

// ErrWorkerNotReachable is returned by ProbeWorkerStatus when the
// IPC endpoint has no listener. The CLI surfaces it as
// "nightme-stt: not running" rather than a transport-level error
// — a missing listener is the expected steady state when no Voice
// message has been processed in this session and is NOT a fault.
//
// Distinguished from transport errors (refused connection on a
// stale socket, protocol mismatch on an old worker, etc.) so the
// CLI can render a clean status row for "runtime: not running"
// without burying the user in dial-stack output.
var ErrWorkerNotReachable = errors.New("stt: worker not reachable on endpoint")

// ProbeWorkerStatus dials the worker's IPC endpoint directly
// (no Manager, no Spawner, no nightme daemon) and queries
// OpStatus. Used by `nightme stt status` so the user can
// diagnose a broken install without having to start the
// daemon first — same diagnostic surface whether or not
// nightme itself is running.
//
// Endpoint is what stt.DefaultEndpoint(dataDir) returns. On
// Unix this is `<dataDir>/stt/stt.sock`; on Windows it is
// `\\.\pipe\nightme-stt`. The default 2s timeout mirrors
// `nightme status`'s ping window (daemon_lifecycle.go:178)
// so neither CLI holds the user hostage on a stuck worker.
func ProbeWorkerStatus(ctx context.Context, endpoint Endpoint) (WorkerStatus, error) {
	if endpoint == "" {
		return WorkerStatus{}, fmt.Errorf("stt: empty endpoint")
	}
	conn, err := DefaultTransport().Dial(ctx, endpoint)
	if err != nil {
		return WorkerStatus{}, fmt.Errorf("%w: %v", ErrWorkerNotReachable, err)
	}
	defer conn.Close()
	client := NewRPCClient(conn)
	ws, err := client.WorkerStatus(ctx)
	if err != nil {
		return WorkerStatus{}, fmt.Errorf("%w: %v", ErrWorkerNotReachable, err)
	}
	return ws, nil
}
