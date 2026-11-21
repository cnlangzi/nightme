// daemon_lifecycle_log_test.go — what the daemon says about itself
// and about its own death.
//
// The daemon's stdout is /dev/null once it is detached (nightme
// start forks it), so the log is the only place these answers can
// live: which process this is, who asked it to stop, and whether the
// shutdown finished cleanly.

package runtime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentregistry"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/version"
)

// logRecord is one captured log line.
type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// logCapture collects the records the daemon logs so a test can
// assert on the level and the attributes rather than on the
// rendered text.
type logCapture struct {
	mu      sync.Mutex
	records []logRecord
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec)
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

// find returns the first record with the given message.
func (c *logCapture) find(msg string) (logRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rec := range c.records {
		if rec.msg == msg {
			return rec, true
		}
	}
	return logRecord{}, false
}

// daemonHarness is a daemon running in the test process, with its
// log captured.
type daemonHarness struct {
	logs *logCapture
	out  *bytes.Buffer
	done chan error
}

// stopWithSignal sends a signal to the daemon and waits for it to
// exit. It returns the daemon's exit error — nil after a clean
// shutdown.
func (h *daemonHarness) stopWithSignal(t *testing.T, sigCh chan os.Signal, sig os.Signal) error {
	t.Helper()
	sigCh <- sig
	select {
	case err := <-h.done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not shut down within 30s")
		return nil
	}
}

// startDaemon boots the daemon with the given channels and waits for
// it to become ready.
func startDaemon(t *testing.T, chs []channel.Channel) (*daemonHarness, chan os.Signal, context.CancelFunc) {
	t.Helper()
	clearAllMgrs(t)

	cfg := &config.Config{
		Primary: "claude",
		Paths:   config.PathsConfig{DataDir: t.TempDir()},
	}
	defaults := DefaultDeps()
	ready := make(chan struct{})
	deps := Deps{
		LoadConfig:        func() (*config.Config, error) { return cfg, nil },
		OpenChatSessions:  defaults.OpenChatSessions,
		OpenAgentSessions: defaults.OpenAgentSessions,
		BuildAgents:       func(*config.Config) *agent.Registry { return agentregistry.Build(cfg, "") },
		NewChannels:       func(*config.Config) ([]channel.Channel, error) { return chs, nil },
		OnReady:           func() { close(ready) },
	}

	logs := &logCapture{}
	out := &bytes.Buffer{}
	sigCh := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())
	runner := RunWith(deps, RunOptions{Out: out, Logger: slog.New(logs), SigCh: sigCh})
	h := &daemonHarness{logs: logs, out: out, done: make(chan error, 1)}
	go func() { h.done <- runner.Run(ctx) }()

	select {
	case <-ready:
	case err := <-h.done:
		t.Fatalf("daemon exited before becoming ready: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not become ready within 30s")
	}
	return h, sigCh, cancel
}

// TestRunDaemon_LogsItsIdentity pins the daemon's self-description.
// Two daemons can run on one machine — a released build and a local
// `go run` — and they write to the same log file. Without the pid,
// the build and the data dir, the operator can only guess which one
// is running.
func TestRunDaemon_LogsItsIdentity(t *testing.T) {
	h, sigCh, _ := startDaemon(t, nil)
	if err := h.stopWithSignal(t, sigCh, os.Interrupt); err != nil {
		t.Fatalf("daemon returned %v, want nil after a clean shutdown", err)
	}

	rec, ok := h.logs.find("daemon running")
	if !ok {
		t.Fatalf("no %q record in the log", "daemon running")
	}
	if want := strconv.Itoa(os.Getpid()); rec.attrs["pid"] != want {
		t.Errorf("pid = %q, want %q", rec.attrs["pid"], want)
	}
	if want := version.String(); rec.attrs["version"] != want {
		t.Errorf("version = %q, want %q", rec.attrs["version"], want)
	}
	if rec.attrs["data_dir"] == "" {
		t.Error("data_dir is empty; the log does not say which data the daemon manages")
	}
}

// TestRunDaemon_LogsTheSignalThatStoppedIt covers the signal path:
// the daemon's stdout line is invisible once it is detached, so the
// signal name has to reach the log too.
func TestRunDaemon_LogsTheSignalThatStoppedIt(t *testing.T) {
	h, sigCh, _ := startDaemon(t, nil)
	if err := h.stopWithSignal(t, sigCh, os.Interrupt); err != nil {
		t.Fatalf("daemon returned %v, want nil after a clean shutdown", err)
	}

	rec, ok := h.logs.find("daemon stopping")
	if !ok {
		t.Fatalf("no %q record in the log", "daemon stopping")
	}
	if rec.attrs["reason"] != "signal" {
		t.Errorf("reason = %q, want %q", rec.attrs["reason"], "signal")
	}
	if want := os.Interrupt.String(); rec.attrs["signal"] != want {
		t.Errorf("signal = %q, want %q", rec.attrs["signal"], want)
	}
	// The foreground daemon still announces the signal on its
	// terminal — that line is not replaced by the log record.
	if !strings.Contains(h.out.String(), "[nightme] received "+os.Interrupt.String()) {
		t.Errorf("stdout = %q, want the received-signal line", h.out.String())
	}
}

// TestRunDaemon_LogsTheContextCancellation covers the other way a
// daemon stops: its context is cancelled. That is what a `nightme
// stop` does, and the reason the record names the cause — the
// daemoncontrol server logs the request separately.
func TestRunDaemon_LogsTheContextCancellation(t *testing.T) {
	h, _, cancel := startDaemon(t, nil)
	cancel()

	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("daemon returned %v, want nil after a clean shutdown", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not shut down within 30s")
	}

	rec, ok := h.logs.find("daemon stopping")
	if !ok {
		t.Fatalf("no %q record in the log", "daemon stopping")
	}
	if rec.attrs["reason"] != "context cancelled" {
		t.Errorf("reason = %q, want %q", rec.attrs["reason"], "context cancelled")
	}
	if want := context.Canceled.Error(); rec.attrs["err"] != want {
		t.Errorf("err = %q, want %q", rec.attrs["err"], want)
	}
}

// TestRunDaemon_LogsTheShutdownOutcome covers the clean exit: the
// log has to say that the shutdown finished and how long it took, so
// a missing line can be read as "it never got that far".
func TestRunDaemon_LogsTheShutdownOutcome(t *testing.T) {
	h, sigCh, _ := startDaemon(t, nil)
	if err := h.stopWithSignal(t, sigCh, os.Interrupt); err != nil {
		t.Fatalf("daemon returned %v, want nil after a clean shutdown", err)
	}

	rec, ok := h.logs.find("daemon stopped")
	if !ok {
		t.Fatalf("no %q record in the log", "daemon stopped")
	}
	if rec.level != slog.LevelInfo {
		t.Errorf("level = %v, want %v", rec.level, slog.LevelInfo)
	}
	if rec.attrs["duration"] == "" {
		t.Error("the stopped record carries no duration")
	}
	if _, err := time.ParseDuration(rec.attrs["duration"]); err != nil {
		t.Errorf("duration = %q, want a parsable duration", rec.attrs["duration"])
	}
}

// TestRunDaemon_LogsAFailedShutdown covers the dirty exit: a channel
// that refuses to stop must not be reported as a clean shutdown.
func TestRunDaemon_LogsAFailedShutdown(t *testing.T) {
	ch := newStartFuncChannel("broken", nil)
	ch.stop = func(context.Context) error { return errors.New("channel stop failed: boom") }

	h, sigCh, _ := startDaemon(t, []channel.Channel{ch})
	if err := h.stopWithSignal(t, sigCh, os.Interrupt); err == nil {
		t.Fatal("daemon returned nil, want the channel stop failure")
	}

	rec, ok := h.logs.find("daemon stopped")
	if !ok {
		t.Fatalf("no %q record in the log", "daemon stopped")
	}
	if rec.level != slog.LevelError {
		t.Errorf("level = %v, want %v for a failed shutdown", rec.level, slog.LevelError)
	}
	if !strings.Contains(rec.attrs["err"], "channel stop failed") {
		t.Errorf("err = %q, want the channel stop failure", rec.attrs["err"])
	}
}
