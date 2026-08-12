//go:build !windows

// Unit tests for the JSONL framing + request/response correlation
// layer. These tests do not spawn a process; they drive the
// rpcClient against in-memory pipe pairs to exercise the same code
// paths the real session uses.

package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// nopCloser adapts an io.Writer to io.WriteCloser with a no-op
// Close. Used by tests that exercise writeLine against an
// in-memory bytes.Buffer without back-pressure.
type nopCloser struct {
	io.Writer
}

func (nopCloser) Close() error { return nil }

// testPipe is a WriteCloser stub used by the correlation tests.
// It captures each Write into a buffer and forwards it to a
// caller-supplied writeFn so the test can observe outgoing bytes
// without back-pressuring on io.Pipe. Reads always return EOF;
// correlation tests drive the response directly via dispatchResponse.
type testPipe struct {
	writeFn func([]byte) (int, error)
}

func (t *testPipe) Write(p []byte) (int, error) { return t.writeFn(p) }
func (t *testPipe) Close() error                { return nil }

// TestWriteLineAtomic verifies that the mutex around writeLine
// serializes concurrent callers so the bytes of two frames never
// interleave. We use an in-memory bytes.Buffer (no back-pressure)
// to capture the wire output and assert both lines are present
// in full, each terminated by exactly one '\n'.
func TestWriteLineAtomic(t *testing.T) {
	var buf bytes.Buffer
	client := newRPCClient(nopCloser{Writer: &buf})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := client.writeLine([]byte(`{"type":"prompt","id":"a","message":"AAA"}`)); err != nil {
			t.Errorf("write a: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := client.writeLine([]byte(`{"type":"prompt","id":"b","message":"BBB"}`)); err != nil {
			t.Errorf("write b: %v", err)
		}
	}()
	wg.Wait()

	got := buf.String()
	want1 := `{"type":"prompt","id":"a","message":"AAA"}`
	want2 := `{"type":"prompt","id":"b","message":"BBB"}`
	if !strings.Contains(got, want1+"\n") {
		t.Errorf("missing line a: %q", got)
	}
	if !strings.Contains(got, want2+"\n") {
		t.Errorf("missing line b: %q", got)
	}
	// Ensure each line is a complete JSON object (no half
	// frames from a torn write). A simple way: count '{' and '}'
	// — they must balance across the whole stream.
	if strings.Count(got, "{") != strings.Count(got, "}") {
		t.Errorf("brace mismatch in %q", got)
	}
}

// TestRequestResponseRoundTrip verifies that a registered response
// is delivered to the caller and that the pending slot is freed
// after delivery. We use a small bridge that wires stdin to a
// controllable channel so the test can inject the response from
// a goroutine without back-pressure races on io.Pipe.
func TestRequestResponseRoundTrip(t *testing.T) {
	requestCh := make(chan []byte, 1)
	responseCh := make(chan []byte, 1)
	stdin := &testPipe{
		writeFn: func(p []byte) (int, error) {
			// Hand the outgoing frame to the test goroutine.
			cp := append([]byte(nil), p...)
			select {
			case requestCh <- cp:
			default:
			}
			return len(p), nil
		},
	}
	client := newRPCClient(stdin)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	type result struct {
		env responseEnvelope
		err error
	}
	done := make(chan result, 1)
	go func() {
		env, err := client.request(ctx, "get_state", map[string]any{}, "boot")
		done <- result{env: env, err: err}
	}()

	// Wait for the request to land on the test side, then push
	// the matching response.
	select {
	case got := <-requestCh:
		t.Logf("outgoing request: %s", got)
	case <-time.After(time.Second):
		t.Fatal("request not written within 1s")
	}
	const reply = `{"id":"boot","type":"response","command":"get_state","success":true,"data":{"sessionId":"abc","model":{"id":"m1","name":"M1","provider":"p"}}}`
	// Manually dispatch the reply through the rpcClient, the
	// same way readPump would for a real child.
	var resp responseEnvelope
	if err := json.Unmarshal([]byte(reply), &resp); err != nil {
		t.Fatalf("parse reply: %v", err)
	}
	t.Logf("dispatching reply id=%s", resp.ID)
	client.dispatchResponse(resp)
	close(responseCh)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("request error: %v", r.err)
		}
		if r.env.Command != "get_state" || !r.env.Success {
			t.Errorf("unexpected response: %+v", r.env)
		}
		if string(r.env.ID) != `"boot"` {
			t.Errorf("response id = %s, want \"boot\"", r.env.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request timed out")
	}

	// After delivery, the pending slot is freed so a second
	// "late" response for the same id is silently dropped.
	client.pendingMu.Lock()
	defer client.pendingMu.Unlock()
	if _, ok := client.pending["boot"]; ok {
		t.Errorf("pending[boot] still present after delivery")
	}
}

// TestRequestTimeout verifies that a pending request that never
// receives a response fails with context.DeadlineExceeded.
func TestRequestTimeout(t *testing.T) {
	client := newRPCClient(nopCloser{Writer: io.Discard})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.request(ctx, "get_state", map[string]any{}, "boot")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// TestFailPendingOnClose verifies that closing the client (via
// failPending) wakes up every pending waiter with ErrSessionClosed
// rather than leaving them blocked forever.
func TestFailPendingOnClose(t *testing.T) {
	client := newRPCClient(nopCloser{Writer: io.Discard})
	ctx := context.Background()
	const N = 3
	results := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			_, err := client.request(ctx, "get_state", map[string]any{}, "")
			results <- err
		}()
	}

	// Give the goroutines a moment to register.
	time.Sleep(10 * time.Millisecond)
	client.failPending(ErrSessionClosed)

	for i := 0; i < N; i++ {
		select {
		case err := <-results:
			if !errors.Is(err, ErrSessionClosed) {
				t.Errorf("request %d err = %v, want ErrSessionClosed", i, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("request %d did not unblock", i)
		}
	}
}

// TestWriteAfterClose verifies that writes are rejected after the
// client is closed.
func TestWriteAfterClose(t *testing.T) {
	client := newRPCClient(nopCloser{Writer: io.Discard})
	client.failPending(ErrSessionClosed)

	err := client.writeLine([]byte(`{"type":"prompt"}`))
	if !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("writeLine after close err = %v, want ErrSessionClosed", err)
	}
}

// TestCancelResponseRemovesEntry verifies the round-trip
// invariant: expectResponse stores a waiter under the JSON-encoded
// form of the id; cancelResponse must remove it under the same
// key, otherwise the pending map leaks. Without the matching key
// normalisation a single-turn session would leak one entry per
// request (the bug found during the F-32 review pass).
func TestCancelResponseRemovesEntry(t *testing.T) {
	client := newRPCClient(nopCloser{Writer: io.Discard})

	_ = client.expectResponse("boot")
	_ = client.expectResponse("req-000042")

	client.cancelResponse("boot")
	client.cancelResponse("req-000042")

	client.pendingMu.Lock()
	defer client.pendingMu.Unlock()
	if n := len(client.pending); n != 0 {
		keys := make([]string, 0, n)
		for k := range client.pending {
			keys = append(keys, k)
		}
		t.Errorf("pending leak: %d entries remain: %v", n, keys)
	}
}

// TestRequestResponseRaceFree verifies the request helper
// registers the pending slot BEFORE writing the command to the
// child. We drive the test against a testPipe that injects the
// response immediately (a "very fast child"): if the order were
// reversed (write-first), the response would arrive before
// expectResponse ran and be silently dropped. The test fails
// with a deadline-exceeded if the race bites.
func TestRequestResponseRaceFree(t *testing.T) {
	requestCh := make(chan struct{}, 1)
	stdin := &testPipe{
		writeFn: func(p []byte) (int, error) {
			// Signal that the bridge has written; the test
			// will then dispatch the matching response.
			select {
			case requestCh <- struct{}{}:
			default:
			}
			return len(p), nil
		},
	}
	client := newRPCClient(stdin)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	type result struct {
		env responseEnvelope
		err error
	}
	done := make(chan result, 1)
	go func() {
		env, err := client.request(ctx, "get_state", map[string]any{}, "fast")
		done <- result{env: env, err: err}
	}()

	// Wait for the bridge to register + write.
	select {
	case <-requestCh:
	case <-time.After(time.Second):
		t.Fatal("bridge never wrote the request")
	}
	// Synthesize a response immediately. With a write-first
	// order the response would be dropped (no pending slot);
	// with a register-first order it is delivered to the
	// goroutine waiting on ch inside request.
	var resp responseEnvelope
	if err := json.Unmarshal(
		[]byte(`{"id":"fast","type":"response","command":"get_state","success":true}`),
		&resp,
	); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	client.dispatchResponse(resp)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("request err: %v", r.err)
		}
		if !r.env.Success || r.env.Command != "get_state" {
			t.Errorf("bad response: %+v", r.env)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request hung; write-before-register race tripped")
	}
}
