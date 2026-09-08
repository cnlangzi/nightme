package codex

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ─── helpers ───

// serverStub pretends to be the codex app-server over a net.Pipe.
// It exposes:
//
//   - serverConn: tests write reply frames here
//   - readFrame(timeout): pulls one LF-delimited frame the bridge
//     wrote (with a short timeout so missing writes don't wedge)
//   - writeLine(payload): non-blocking send — fails fast if the
//     pipe's read side is congested. Caller can retry.
//
// To avoid the classic net.Pipe deadlock (unbuffered, so a write
// blocks until a read happens), the stub spawns ONE long-running
// drain goroutine that pushes every bridge frame into a buffered
// channel. Tests read from the channel.
type serverStub struct {
	conn   net.Conn
	frames chan string
	done   chan struct{}
	once   sync.Once
}

func newServerStub(t *testing.T) (stub *serverStub, client *rpcClient, cleanup func()) {
	t.Helper()

	clientConn, serverConn := net.Pipe()

	noOpSR := func(string, json.RawMessage, json.RawMessage) {}
	noOpN := func(string, json.RawMessage) {}
	client = newRPCClient(clientConn, noOpSR, noOpN)

	stub = &serverStub{
		conn:   serverConn,
		frames: make(chan string, 64),
		done:   make(chan struct{}),
	}

	// Drain the bridge side (clientConn → serverConn direction).
	// Reads happen on the SERVER side of the pipe (serverConn.Read),
	// because that's where bytes the bridge writes land.
	go func() {
		defer close(stub.done)
		scanner := newPipeScanner(serverConn)
		for scanner.scan() {
			select {
			case stub.frames <- scanner.text():
			default:
				// Test is too slow; drop the frame. Should not
				// happen in well-behaved tests.
			}
		}
	}()

	// Run the bridge's read pump. It reads from clientConn what
	// the stub writes to serverConn, then dispatches responses /
	// notifications / server-request frames.
	go client.readPump(context.Background(), func(error) {
		// Wire errors are exercised in session_test.go.
	})

	cleanup = func() {
		stub.once.Do(func() {
			_ = stub.conn.Close()
		})
		<-stub.done
	}

	return stub, client, cleanup
}

// readFrame pulls one LF-delimited frame from the bridge. Blocks
// until the frame arrives or the deadline expires.
func (s *serverStub) readFrame(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case f := <-s.frames:
		return f
	case <-time.After(timeout):
		return ""
	}
}

// writeLine writes one LF-terminated JSON-RPC frame to the bridge.
// If the bridge's reader is wedged this blocks until the deadline
// (which is fine — the test will fail with a clear timeout).
func (s *serverStub) writeLine(t *testing.T, payload string) {
	t.Helper()
	_ = s.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := s.conn.Write([]byte(payload + "\n"))
	_ = s.conn.SetWriteDeadline(time.Time{})
	if err != nil {
		t.Fatalf("serverStub.writeLine: %v", err)
	}
}

// closeNow closes the server side abruptly (used to test EOF → failPending).
func (s *serverStub) closeNow() {
	s.once.Do(func() { _ = s.conn.Close() })
}

// pipeScanner is a minimal LF-delimited scanner built on top of
// net.Conn to avoid pulling in bufio.Scanner for tests (and to make
// it easy to peek the remainder for the concurrent-requests test).
type pipeScanner struct {
	r    net.Conn
	buf  []byte
	line string
	err  error
}

func newPipeScanner(r net.Conn) *pipeScanner {
	return &pipeScanner{r: r, buf: make([]byte, 0, 64*1024)}
}

func (s *pipeScanner) scan() bool {
	for {
		// Look for a newline already in buf.
		for i, c := range s.buf {
			if c == '\n' {
				s.line = string(s.buf[:i])
				s.buf = s.buf[i+1:]
				return true
			}
		}
		// Need more data.
		chunk := make([]byte, 64*1024)
		_ = s.r.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := s.r.Read(chunk)
		_ = s.r.SetReadDeadline(time.Time{})
		if n > 0 {
			s.buf = append(s.buf, chunk[:n]...)
		}
		if err != nil {
			if err == io.EOF {
				s.err = err
				return false
			}
			// Deadline expiry is normal here — we set 500ms to
			// bound the test runtime. If we got no data, return
			// false and let the caller loop / exit.
			if isTimeoutErr(err) {
				if len(s.buf) == 0 {
					s.err = err
				}
				return false
			}
			s.err = err
			return false
		}
	}
}

func (s *pipeScanner) text() string { return s.line }

func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	type timeout interface{ Timeout() bool }
	if te, ok := err.(timeout); ok && te.Timeout() {
		return true
	}
	return false
}

// ─── tests ───

func TestRPC_RequestResponseCorrelation(t *testing.T) {
	stub, client, cleanup := newServerStub(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	type respShape struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Model string `json:"model"`
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.request(ctx, "thread/start",
			map[string]any{"cwd": "/tmp"},
			&respShape{},
		)
	}()

	frame := stub.readFrame(t, 1*time.Second)
	if frame == "" {
		t.Fatal("client did not write a request frame")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(frame), &probe); err != nil {
		t.Fatalf("client wire frame is not JSON: %v\nframe: %s", err, frame)
	}
	idRaw := string(probe["id"])
	if idRaw == "" {
		t.Fatalf("client frame missing id: %s", frame)
	}

	stub.writeLine(t, `{"jsonrpc":"2.0","id":`+idRaw+`,"result":{"thread":{"id":"thr-1"},"model":"o4-mini"}}`)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("request returned error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("request did not return before ctx deadline")
	}
}

func TestRPC_NotifyHasNoResponse(t *testing.T) {
	stub, client, cleanup := newServerStub(t)
	defer cleanup()

	if err := client.notify("initialized", nil); err != nil {
		t.Fatalf("notify failed: %v", err)
	}
	frame := stub.readFrame(t, 1*time.Second)
	if frame == "" {
		t.Fatal("client did not write a notification frame")
	}
	var probe map[string]json.RawMessage
	_ = json.Unmarshal([]byte(frame), &probe)
	if _, hasID := probe["id"]; hasID {
		t.Errorf("notification has id field: %s", frame)
	}
	if m, _ := probe["method"]; string(m) != `"initialized"` {
		t.Errorf("method = %s, want initialized", m)
	}
}

func TestRPC_RespondWritesServerResponse(t *testing.T) {
	stub, client, cleanup := newServerStub(t)
	defer cleanup()

	if err := client.respond(json.RawMessage(`42`),
		map[string]any{"decision": "accept"}); err != nil {
		t.Fatalf("respond failed: %v", err)
	}
	frame := stub.readFrame(t, 1*time.Second)
	if frame == "" {
		t.Fatal("client did not write a response frame")
	}
	var probe map[string]json.RawMessage
	_ = json.Unmarshal([]byte(frame), &probe)
	if string(probe["id"]) != "42" {
		t.Errorf("response id = %s, want 42", probe["id"])
	}
}

func TestRPC_FailPendingOnEOF(t *testing.T) {
	stub, client, cleanup := newServerStub(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.request(ctx, "thread/start",
			map[string]any{}, nil,
		)
	}()
	// Close the server side abruptly. The client's readPump sees
	// EOF, calls failPending, and the pending request returns
	// ErrSessionClosed.
	stub.closeNow()
	cleanup()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("request returned nil error after EOF; want ErrSessionClosed")
		}
	case <-ctx.Done():
		t.Fatal("request did not return after EOF")
	}
}

func TestRPC_ConcurrentRequestsAreIndependent(t *testing.T) {
	stub, client, cleanup := newServerStub(t)
	defer cleanup()

	const N = 25
	type respShape struct {
		N int `json:"n"`
	}

	// Server: for every frame the bridge writes, reply with the
	// same id and a "n" reflecting the order it was received.
	var requestCount atomic.Int64
	var replyWG sync.WaitGroup
	for i := 0; i < N; i++ {
		replyWG.Add(1)
		go func() {
			defer replyWG.Done()
			for {
				frame := stub.readFrame(t, 200*time.Millisecond)
				if frame == "" {
					return
				}
				var probe map[string]json.RawMessage
				if json.Unmarshal([]byte(frame), &probe) != nil {
					continue
				}
				idRaw := string(probe["id"])
				if idRaw == "" {
					continue
				}
				idx := requestCount.Add(1)
				stub.writeLine(t, `{"jsonrpc":"2.0","id":`+idRaw+
					`,"result":{"n":`+itoa(int(idx))+`}}`)
				if int(idx) >= N {
					return
				}
			}
		}()
	}

	results := make([]int, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var got respShape
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := client.request(ctx, "ping",
				map[string]any{"i": i}, &got); err != nil {
				t.Errorf("request %d failed: %v", i, err)
				return
			}
			results[i] = got.N
		}(i)
	}
	wg.Wait()
	cleanup()
	replyWG.Wait()

	// Every result should be a positive integer ≤ N, and all values
	// across goroutines should be unique (sanity: id correlation works).
	seen := make(map[int]bool)
	for i, n := range results {
		if n <= 0 || n > N {
			t.Errorf("results[%d] = %d, want 1..%d", i, n, N)
		}
		if seen[n] {
			t.Errorf("results[%d] = %d, duplicate value (id correlation broke)", i, n)
		}
		seen[n] = true
	}
}

func TestRPC_RespondErrWritesErrorEnvelope(t *testing.T) {
	stub, client, cleanup := newServerStub(t)
	defer cleanup()

	if err := client.respondErr(json.RawMessage(`7`), -32601, "no such method"); err != nil {
		t.Fatalf("respondErr: %v", err)
	}
	frame := stub.readFrame(t, 1*time.Second)
	if frame == "" {
		t.Fatal("client did not write a frame")
	}
	var env rpcResponseEnvelope
	if err := json.Unmarshal([]byte(frame), &env); err != nil {
		t.Fatalf("frame is not an rpcResponseEnvelope: %v\n%s", err, frame)
	}
	if env.Error == nil {
		t.Fatalf("frame missing error field: %+v", env)
	}
	if env.Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601", env.Error.Code)
	}
	if env.Error.Message != "no such method" {
		t.Errorf("error message = %q, want 'no such method'", env.Error.Message)
	}
}

func TestRPC_RequestCancelledByContext(t *testing.T) {
	stub, client, cleanup := newServerStub(t)
	defer cleanup()

	// Drain the request frame so the write unblocks.
	go func() {
		for {
			if stub.readFrame(t, 200*time.Millisecond) == "" {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := client.request(ctx, "thread/start",
		map[string]any{}, nil)
	if err == nil {
		t.Fatal("request returned nil error; want context.DeadlineExceeded")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
}

func TestRPC_PendingCancelledOnWriteFailure(t *testing.T) {
	// Force a write failure by closing the underlying connection
	// BEFORE issuing a request. The client must remove the pending
	// slot and return an error (not panic).
	clientConn, serverConn := net.Pipe()
	noOpSR := func(string, json.RawMessage, json.RawMessage) {}
	noOpN := func(string, json.RawMessage) {}
	client := newRPCClient(clientConn, noOpSR, noOpN)
	go client.readPump(context.Background(), func(error) {})

	_ = serverConn.Close() // close the server side immediately

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	err := client.request(ctx, "ping", map[string]any{}, nil)
	if err == nil {
		t.Fatal("request returned nil error after server conn closed")
	}
}
