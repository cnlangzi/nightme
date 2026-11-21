// stop_log_test.go — the record of who asked the daemon to stop.
//
// The runtime's own "daemon stopping" line names the cause (a
// context cancellation). Only this record says the control socket
// asked for it — without it, a `nightme stop` and an unexplained
// cancellation look the same in the log.

package daemoncontrol

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer the server goroutine can write into
// while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestStopRequestIsRecorded(t *testing.T) {
	dir := t.TempDir()
	paths, err := ResolvePaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, cancel := context.WithCancel(context.Background())
	server, err := Listen(paths.Socket, Status{PID: 42, StartedAt: time.Now()}, cancel)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	var buf syncBuffer
	server.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	go func() { _ = server.Serve() }()
	// Ready before the stop: the starting-state fast path exits the
	// process, which would take the test runner with it.
	server.SetReady()

	if err := Stop(paths.Socket, 2*time.Second); err != nil {
		t.Fatalf("Stop returned %v", err)
	}
	if !strings.Contains(buf.String(), "daemon stop requested") {
		t.Errorf("log = %q, want the stop request recorded", buf.String())
	}
}
