// Unit tests for the JSONL framing + request/response
// correlation layer. These tests mirror the pi bridge's
// rpc_test.go.

package omp

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

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

type testPipe struct {
	writeFn func([]byte) (int, error)
}

func (t *testPipe) Write(p []byte) (int, error) { return t.writeFn(p) }
func (t *testPipe) Close() error                { return nil }

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
	if strings.Count(got, "{") != strings.Count(got, "}") {
		t.Errorf("brace mismatch in %q", got)
	}
}

func TestRequestResponseRoundTrip(t *testing.T) {
	requestCh := make(chan []byte, 1)
	stdin := &testPipe{
		writeFn: func(p []byte) (int, error) {
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

	select {
	case got := <-requestCh:
		t.Logf("outgoing request: %s", got)
	case <-time.After(time.Second):
		t.Fatal("request not written within 1s")
	}
	const reply = `{"id":"boot","type":"response","command":"get_state","success":true,"data":{"sessionId":"abc","model":{"id":"m1","name":"M1","provider":"p"}}}`
	var resp responseEnvelope
	if err := json.Unmarshal([]byte(reply), &resp); err != nil {
		t.Fatalf("parse reply: %v", err)
	}
	t.Logf("dispatching reply id=%s", resp.ID)
	client.dispatchResponse(resp)

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

	client.pendingMu.Lock()
	defer client.pendingMu.Unlock()
	if _, ok := client.pending["boot"]; ok {
		t.Errorf("pending[boot] still present after delivery")
	}
}

func TestRequestTimeout(t *testing.T) {
	client := newRPCClient(nopCloser{Writer: io.Discard})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.request(ctx, "get_state", map[string]any{}, "boot")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

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

func TestWriteAfterClose(t *testing.T) {
	client := newRPCClient(nopCloser{Writer: io.Discard})
	client.failPending(ErrSessionClosed)

	err := client.writeLine([]byte(`{"type":"prompt"}`))
	if !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("writeLine after close err = %v, want ErrSessionClosed", err)
	}
}

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

func TestRequestResponseRaceFree(t *testing.T) {
	requestCh := make(chan struct{}, 1)
	stdin := &testPipe{
		writeFn: func(p []byte) (int, error) {
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

	select {
	case <-requestCh:
	case <-time.After(time.Second):
		t.Fatal("bridge never wrote the request")
	}
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

// TestMaxFrameSizeAtLeastFourMiB pins the R16 invariant: the
// scanner buffer ceiling is at least 4 MiB (covers omp's
// get_state ≈33 KB and available_commands_update ≈16.7 KB
// frames comfortably).
func TestMaxFrameSizeAtLeastFourMiB(t *testing.T) {
	const want = 4 * 1024 * 1024
	if MaxFrameSize < want {
		t.Fatalf("MaxFrameSize = %d, want >= %d", MaxFrameSize, want)
	}
}
