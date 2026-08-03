//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestLogsCommandSurface(t *testing.T) {
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"logs"})
	if err != nil || cmd == nil {
		t.Fatalf("logs not registered: err=%v", err)
	}
	if got := cmd.Flags().Lookup("lines"); got == nil {
		t.Fatal(`logs missing --lines flag`)
	}
	if got := cmd.Flags().Lookup("follow"); got == nil {
		t.Fatal(`logs missing --follow flag`)
	}
	if got := cmd.Flags().Lookup("once"); got == nil {
		t.Fatal(`logs missing --once flag`)
	}
}

func TestLogsRunE_OnceDoesNotLeakIntoNextInvocation(t *testing.T) {
	// cobra/pflag do not reset flag values between successive
	// Execute() calls on the same command tree (verified against
	// pflag@cobra's vendored copy). The RunE callback captures
	// logsOnce into a local and resets the package variable so a
	// single `logs --once` does not silently flip a later REPL
	// `logs` invocation into one-shot mode.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	logsOnce = false

	cmd := newLogsCmd()
	cmd.SetArgs([]string{"--once"})
	// With HOME pointing at an empty tempdir, the default log
	// file does not exist; --once flips follow off so runLogs
	// returns nil after the "does not exist yet" hint rather than
	// blocking on the follow loop.
	if err := cmd.Execute(); err != nil {
		t.Fatalf("first Execute (--once): %v", err)
	}
	// RunE must have reset logsOnce for the next caller.
	if logsOnce {
		t.Errorf("logsOnce leaked into the next invocation: got true, want false")
	}
}

func TestTailLines_ReturnsLastN(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nightme.log")
	contents := "alpha\nbeta\ngamma\ndelta\nepsilon\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLines(path, 3)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	want := []string{"gamma", "delta", "epsilon"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d", len(lines), len(want))
	}
	for i, w := range want {
		if string(lines[i]) != w {
			t.Errorf("line %d = %q, want %q", i, string(lines[i]), w)
		}
	}
}

func TestTailLines_LargerThanChunk(t *testing.T) {
	// Build a file with many lines, sized so tailLines chunks
	// from the end and skips the partial leading line.
	dir := t.TempDir()
	path := filepath.Join(dir, "nightme.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		_, _ = f.WriteString("padding-line-" + strings.Repeat("x", 1100) + "\n")
	}
	_ = f.Close()
	lines, err := tailLines(path, 5)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5", len(lines))
	}
	for _, line := range lines {
		if !bytes.HasPrefix(line, []byte("padding-line-")) {
			t.Errorf("unexpected partial line: %q", string(line))
		}
	}
}

func TestTailLines_FileMissing(t *testing.T) {
	if _, err := tailLines(filepath.Join(t.TempDir(), "nope.log"), 10); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestTailLines_ParsesJSON(t *testing.T) {
	// Cheap integration check that tailLines doesn't corrupt
	// the JSON records the slog handler writes.
	dir := t.TempDir()
	path := filepath.Join(dir, "nightme.log")
	rec, _ := json.Marshal(map[string]any{
		"time":  "2025-01-01T00:00:00Z",
		"level": "INFO",
		"msg":   "hello",
	})
	if err := os.WriteFile(path, append(rec, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLines(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	var parsed map[string]any
	if err := json.Unmarshal(lines[0], &parsed); err != nil {
		t.Fatalf("tail produced invalid JSON: %v: %q", err, string(lines[0]))
	}
	if parsed["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", parsed["msg"])
	}
}

func TestFollowLog_ScansNewLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.log")
	if err := os.WriteFile(path, []byte("seed-1\nseed-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_ = followLog(ctx, &buf, path)
		close(done)
	}()

	// Give the loop time to seek past the seed bytes, then write
	// a fresh line and assert it appears. The polling interval is
	// 200ms; allow a generous margin so the test stays stable on
	// loaded CI runners.
	time.Sleep(300 * time.Millisecond)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("appended-1\nappended-2\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "appended-2") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	if !strings.Contains(buf.String(), "appended-1") || !strings.Contains(buf.String(), "appended-2") {
		t.Fatalf("missing appended lines; got %q", buf.String())
	}
}

func TestRunLogs_NoFileIsHelpful(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// No log file at the default path; runLogs should print a
	// hint, not a stack trace, and exit cleanly under
	// --no-follow.
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())
	if err := runLogs(cmd, logsCmdFlags{lines: 5, follow: false}); err != nil {
		t.Fatalf("runLogs with missing file: %v", err)
	}
	if !strings.Contains(out.String(), "does not exist") {
		t.Errorf("expected user-friendly hint, got %q", out.String())
	}
}
