package stt

import (
	"context"
	"errors"
	"testing"
	"time"
)

// stubRecognizer is the minimal Recognizer the probe tests
// need — same shape as cmd/nightme-stt/internal/sherpa.stubRecognizer,
// duplicated here because the sherpa package lives under cmd/
// and would create an internal-package import cycle if
// internal/stt tests pulled it in.
type stubRecognizer struct{}

func (stubRecognizer) Recognize(_ context.Context, _ RecognizeRequest) (RecognizeResult, error) {
	return RecognizeResult{}, nil
}
func (stubRecognizer) Close() error { return nil }

// TestRPCClient_WorkerStatus_HappyPath wires an in-process
// Server over the LoopbackTransport, then dials via the loopback
// and exercises RPCClient.WorkerStatus directly. This is the
// half of ProbeWorkerStatus that depends on the protocol; the
// other half (DefaultTransport().Dial with the real OS endpoint)
// is exercised in CI by running the worker under a real socket.
//
// Catches wire-shape drift (omitted fields, version mismatches)
// before it surfaces as a "nightme stt status shows nothing"
// bug in production.
func TestRPCClient_WorkerStatus_HappyPath(t *testing.T) {
	_, ln := Loopback()

	srv := NewServer(ln, nil, stubRecognizer{}, nil)
	go func() { _ = srv.Serve(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// LoopbackTransport.Dial takes the endpoint as a sentinel
	// only — the actual connection is in-process. We need a
	// transport instance here; Loopback() already created one
	// but only the listener was captured. Build a fresh one
	// bound to the same listener so Dial succeeds.
	tr := &LoopbackTransport{listener: ln}
	conn, err := tr.Dial(ctx, Endpoint("loopback"))
	if err != nil {
		t.Fatalf("loopback dial: %v", err)
	}
	defer conn.Close()

	client := NewRPCClient(conn)
	ws, err := client.WorkerStatus(ctx)
	if err != nil {
		t.Fatalf("WorkerStatus: %v", err)
	}
	if ws.Version != ProtocolVersion {
		t.Errorf("Version = %d, want %d", ws.Version, ProtocolVersion)
	}
	// LoopbackListener.Endpoint() returns "" by design — the
	// in-process test transport has no OS-level address to
	// report. The real Unix-socket transport fills in the
	// conventional <dataDir>/stt/stt.sock path. The wire-
	// shape contract (PID / StartedAt / Version / BuildVer)
	// is what this test guards; the Endpoint string is a
	// transport detail.
	_ = ws.Endpoint
	if ws.StartedAt.IsZero() {
		t.Errorf("StartedAt is zero")
	}
	if ws.PID <= 0 {
		// PID in the test process is the test runner PID; the
		// exact value varies but must be positive.
		t.Errorf("PID = %d, want > 0", ws.PID)
	}
}

// TestProbeWorkerStatus_NoListener confirms that probing an
// endpoint with no listener surfaces ErrWorkerNotReachable —
// the CLI's runtime section renders "not running" on this error
// path, so the error sentinel is part of the CLI contract.
//
// Uses the real DefaultTransport (not loopback) so the test
// fails if a future refactor accidentally drops the sentinel
// from the production code path.
func TestProbeWorkerStatus_NoListener(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := ProbeWorkerStatus(ctx, Endpoint("/tmp/nightme-stt-definitely-not-listening-test.sock"))
	if err == nil {
		t.Fatalf("ProbeWorkerStatus on unbound endpoint: want error, got nil")
	}
	if !errors.Is(err, ErrWorkerNotReachable) {
		t.Errorf("error chain missing ErrWorkerNotReachable: %v", err)
	}
}
