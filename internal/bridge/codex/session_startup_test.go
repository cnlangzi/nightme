package codex

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestStart_ChildDiesDuringStartup_ReportsExit spawns the bridge
// against a binary that exits immediately and asserts the bridge
// does not take the exit down with it.
//
// The session's delivery hook is wired when the session is
// constructed, but the lifecycle goroutine — the one that reports
// the child dying — starts with the child. A child that exits
// before the driver is wired used to reach a nil hook and panic
// there: the exit error was lost, and the cleanup after it never
// ran.
func TestStart_ChildDiesDuringStartup_ReportsExit(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	// The panic this guards against is recovered by agent.SafeGo
	// and logged, so the test has to watch the log to see it.
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s := NewStarter("codex", stub, nil)
	// The window between the lifecycle goroutine starting and the
	// driver wiring its delivery hook is short, and which side
	// wins it is a race. Repeat the scenario so the race has more
	// than one chance to go the wrong way.
	for i := 0; i < 10; i++ {
		ag, err := s.Start(ctx, agent.StartConfig{Workspace: dir})
		if err == nil {
			t.Cleanup(func() { _ = ag.Close() })
		}
		// Give the lifecycle goroutine time to notice the dead child.
		time.Sleep(200 * time.Millisecond)
		if strings.Contains(logs.String(), "bridge goroutine panic recovered") {
			t.Fatalf("a bridge goroutine panicked while the child died at startup:\n%s", logs.String())
		}
	}
}
