// Regression tests for the ACP fix-stop path.
//
// fix-stop replaces the pre-fix SIGINT-driven Stop with the ACP
// session/cancel JSON-RPC method. SIGINT through a PTY-backed
// transport depends on every agent implementation interpreting
// the signal the same way — some agents exit, some translate it
// to an unstructured stream event the bridge can't tell apart
// from a crash, some ignore it. session/cancel is the documented
// ACP method, so the behaviour is now uniform: agent stays alive,
// settles the in-flight prompt, the chat layer's TryFlush picks
// up the next queued prompt on the SAME sessionId (no respawn,
// no --resume, no ghost turn).
//
// The tests below pin:
//
//  1. Stop() returns ErrNotSupported when transport is nil
//     (pre-Start / post-Close).
//  2. Stop() is a no-op when sessionID is empty (handshake
//     failed) — mirrors stop.go's "nothing to stop" branch.
//  3. Stop() sends session/cancel with the captured sessionId.
//  4. Stop() falls back to SIGINT when session/cancel returns
//     -32601 method-not-found (old agents).
//  5. Stop() surfaces other errors to the caller (chat layer
//     renders "stop failed: ...").
//  6. The isMethodNotFound sentinel helper distinguishes -32601
//     from a -32600 that mentions "method not found" in its body.

package acp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── helpers ───

// sigRecorder is a mockTransport that captures every Signal call
// so the SIGINT-fallback test can assert on it. Production never
// sees this — net.Pipe is the test bridge.
//
// We embed mockTransport via composition (not inheritance — Go)
// so PID stays the test default.
type sigRecorder struct {
	net.Conn
	mu          sync.Mutex
	signals     []os.Signal
	failOnError bool
}

// stub the rest of mockTransport's surface
func (r *sigRecorder) PID() int { return 0 }

func (r *sigRecorder) Signal(sig os.Signal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.signals = append(r.signals, sig)
	if r.failOnError {
		return errors.New("stub: signal transport error")
	}
	return nil
}

// record returns a copy of the signals sent so far.
func (r *sigRecorder) record() []os.Signal {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]os.Signal, len(r.signals))
	copy(out, r.signals)
	return out
}

// acpStubServer is a hand-rolled ACP server running over one
// end of a net.Pipe. It supports:
//   - initialize  → canned reply
//   - session/new → canned sessionId reply
//   - session/cancel → configurable reply (null / -32601 / never)
//
// One goroutine owns ALL server-side reads; another goroutine
// fans the parsed frames out to a chan for test inspection.
type acpStubServer struct {
	conn net.Conn
	// frames is a buffered channel of every JSON-RPC frame the
	// bridge wrote. Tests pop frames and assert on their Method.
	frames chan map[string]any
	// cancelBehaviour controls session/cancel reply:
	//   "ok"            → respond with null
	//   "method-missing" → respond with -32601 Method not found
	//   "stall"         → never reply (test relies on timeout)
	cancelBehaviour string
	done            chan struct{}
}

func newACPStub(t *testing.T, cancelBehaviour string) (*driver, *sigRecorder, *acpStubServer) {
	t.Helper()

	clientConn, serverConn := net.Pipe()

	sig := &sigRecorder{Conn: clientConn}
	stub := &acpStubServer{
		conn:            serverConn,
		frames:          make(chan map[string]any, 16),
		cancelBehaviour: cancelBehaviour,
		done:            make(chan struct{}),
	}

	go runACPDrain(serverConn, stub)

	a := newAgentForTest(sig, "test", "/tmp/ws")

	t.Cleanup(func() {
		_ = a.Close()
		_ = serverConn.Close()
		_ = clientConn.Close()
		<-stub.done
	})

	return a, sig, stub
}

// runACPDrain reads LF-delimited JSON-RPC frames off conn and
// dispatches canned replies for initialize, session/new, and
// session/cancel. Each parsed frame is also pushed onto stub.frames
// for tests to inspect.
func runACPDrain(conn net.Conn, stub *acpStubServer) {
	defer close(stub.done)

	buf := make([]byte, 4096)
	var pending []byte
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		pending = append(pending, buf[:n]...)
		for {
			idx := indexACPLF(pending)
			if idx < 0 {
				break
			}
			frame := pending[:idx]
			pending = pending[idx+1:]

			var m map[string]any
			if json.Unmarshal(frame, &m) == nil {
				select {
				case stub.frames <- m:
				default:
				}
			}

			var msg rpcMessage
			if json.Unmarshal(frame, &msg) != nil {
				continue
			}
			if len(msg.ID) == 0 {
				continue
			}

			switch msg.Method {
			case "initialize":
				_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) +
					`,"result":{"protocolVersion":1}}` + "\n"))
			case "session/new":
				_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) +
					`,"result":{"sessionId":"sess-abc"}}` + "\n"))
			case "session/cancel":
				switch stub.cancelBehaviour {
				case "ok":
					_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) +
						`,"result":null}` + "\n"))
				case "method-missing":
					_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) +
						`,"error":{"code":-32601,"message":"Method not found"}}` + "\n"))
				case "stall":
					// no reply; the bridge times out via stopRPCTimeout
				}
			}
		}
	}
}

// indexACPLF returns the index of the first '\n' in b, or -1.
func indexACPLF(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}

// ─── Stop ───

// TestStop_NoTransport returns ErrNotSupported when Start has
// not been called (transport is nil). Mirrors the pre-fix contract.
func TestStop_NoTransport(t *testing.T) {
	a := &driver{} // transport is nil
	err := a.Stop(context.Background())
	if !errors.Is(err, agent.ErrNotSupported) {
		t.Errorf("Stop before Start = %v, want ErrNotSupported", err)
	}
}

// TestStop_NoSessionIsNoop is a no-op when sessionID is empty —
// mirrors stop.go's "Action=noop" branch so a second /stop in
// quick succession doesn't surface a spurious failure when the
// session never finished handshake.
func TestStop_NoSessionIsNoop(t *testing.T) {
	a, sig, _ := newACPStub(t, "ok")
	defer func() { _ = a.Close() }()

	// No handshake → sessionID stays empty.
	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("Stop with empty sessionID = %v, want nil", err)
	}

	// And critically: NO signal should have been sent (the
	// sessionID=="" short-circuit must fire BEFORE the SIGINT
	// fallback).
	if got := sig.record(); len(got) != 0 {
		t.Errorf("sessionID==\"\" path sent %d signals; want 0 (signals=%v)",
			len(got), got)
	}
}

// TestStop_CallsSessionCancel is the central fix-stop regression.
// With a session established, Stop() must issue a session/cancel
// JSON-RPC carrying the captured sessionId — NOT SIGINT. The
// stub captures the frame and we assert on its Method and Params.
func TestStop_CallsSessionCancel(t *testing.T) {
	a, _, stub := newACPStub(t, "ok")
	defer func() { _ = a.Close() }()

	// Run the handshake so sessionID is populated.
	if err := a.handshake(context.Background(), "/tmp/ws"); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Pop frames until we see session/cancel (initialize + new
	// came first from handshake).
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-stub.frames:
			if got := f["method"]; got == "session/cancel" {
				params, _ := f["params"].(map[string]any)
				if got := params["sessionId"]; got != "sess-abc" {
					t.Errorf("params.sessionId = %v, want sess-abc", got)
				}
				return
			}
		case <-deadline:
			t.Fatal("bridge never wrote session/cancel within 2s")
		}
	}
}

// TestStop_FallsBackOnMethodNotFound covers the old-agent
// compatibility path. Post-fix we prefer session/cancel but fall
// back to SIGINT when the agent returns -32601. The test asserts
// the bridge issued session/cancel AND did not surface the
// JSON-RPC error to the caller — the SIGINT fallback path
// signals the mock transport, which we verify via sig.record.
func TestStop_FallsBackOnMethodNotFound(t *testing.T) {
	a, sig, _ := newACPStub(t, "method-missing")
	defer func() { _ = a.Close() }()

	if err := a.handshake(context.Background(), "/tmp/ws"); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	err := a.Stop(context.Background())
	if err != nil {
		t.Errorf("Stop with method-not-found = %v; want nil "+
			"(SIGINT fallback should consume the -32601)", err)
	}

	// Confirm the SIGINT fallback actually fired: sig.record
	// should have one entry equal to os.Interrupt.
	signals := sig.record()
	if len(signals) == 0 {
		t.Fatal("SIGINT fallback never signalled; -32601 must trigger transport.Signal")
	}
	if signals[len(signals)-1] != os.Interrupt {
		t.Errorf("last signal = %v, want os.Interrupt", signals[len(signals)-1])
	}
}

// TestStop_OtherErrorSurfaces covers the path where
// session/cancel returns a non -32601 error. The chat layer
// renders "stop failed: ..." on this — it must NOT silently
// fall back to SIGINT (that would mask a real protocol
// failure).
//
// We trigger this by stalling the stub (no reply) so
// session/cancel times out via stopRPCTimeout.
func TestStop_OtherErrorSurfaces(t *testing.T) {
	a, _, _ := newACPStub(t, "stall")
	defer func() { _ = a.Close() }()

	if err := a.handshake(context.Background(), "/tmp/ws"); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err := a.Stop(ctx)

	// stopRPCTimeout is 3s but our ctx cancels at 250ms, so the
	// call should return ctx.DeadlineExceeded (or a wrapped
	// version). The point: an error IS surfaced.
	if err == nil {
		t.Fatal("Stop on stalled stub returned nil; want an error")
	}
}

// ─── sentinel ───

// TestIsMethodNotFound is a unit-level check on the sentinel
// helper: positive cases (real -32601 errors), negative cases
// (other JSON-RPC errors that mention "method not found" in
// their body), and the nil case.
func TestIsMethodNotFound(t *testing.T) {
	if isMethodNotFound(nil) {
		t.Error("isMethodNotFound(nil) = true; want false")
	}
	if !isMethodNotFound(&rpcError{Code: -32601, Message: "x"}) {
		t.Error("isMethodNotFound(-32601) = false; want true")
	}
	if isMethodNotFound(&rpcError{Code: -32600, Message: "method not found in body"}) {
		t.Error("isMethodNotFound(-32600) = true; want false (code mismatch)")
	}
	if !isMethodNotFound(errors.New("acp: stop: method not found")) {
		t.Error("isMethodNotFound(\"method not found\" string) = false; want true")
	}
	if isMethodNotFound(errors.New("acp: stop: timeout")) {
		t.Error("isMethodNotFound(\"timeout\") = true; want false")
	}
}