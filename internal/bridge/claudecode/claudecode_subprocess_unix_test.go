//go:build !windows

package claudecode

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// claudeMockScript is the path to a sh wrapper that absorbs the
// bridge's args (--print, --input-format stream-json, ...) and
// runs the Python mock. The bridge's DefaultArgs are flagged
// for the real claude binary; Python would reject them as
// unknown options, so we round-trip through sh.
//
// Unix-only: the script is `#!/bin/sh` + `exec python3`, both of
// which are platform-dependent. On Windows the bridge should be
// tested against a real `claude.exe` or a Windows-aware mock
// (not in this file).
const claudeMockScript = "../../testdata/claude_mock.sh"

// claudeMockCommand returns the argv that spawns the mock. The
// bridge passes DefaultArgs (--print, --input-format stream-json,
// ...) which the underlying Python interpreter rejects as
// unknown options. The sh wrapper around the Python mock absorbs
// those args via shebang semantics so the bridge can pass them
// unchanged.
func claudeMockCommand(t *testing.T) (string, []string) {
	t.Helper()
	// Resolve the mock script to an absolute path so the
	// bridge's exec.LookPath doesn't fail when starting
	// from the test's working directory.
	abs, err := filepath.Abs(claudeMockScript)
	if err != nil {
		t.Fatalf("resolve mock script %q: %v", claudeMockScript, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Skipf("claude mock script not present at %s: %v (skipping integration test)", abs, err)
	}
	return abs, nil
}

// TestClaudeCodeBridge_RealSubprocess drives the claudecode bridge
// through a real subprocess (a Python mock that mimics the Claude
// Code CLI's stream-json surface) and verifies that three
// back-to-back SendBlocks calls produce the expected event trio
// per message. With the pre-fix eviction code the third SendBlocks
// would deadlock; with the post-fix code all three messages
// flow through the bridge's read path and reach the Events
// channel.
//
// The mock is a Python script (not sh -c) so the test exercises
// a real OS subprocess with all the pipe / fd / buffering
// semantics that the production Claude Code session uses.
// Failure modes the test catches:
//   - The bridge writes the wrong stream-json envelope shape
//     (the mock would fail to extract text and emit "empty").
//   - The bridge fails to multiplex concurrent SendBlocks calls
//     onto the child's stdin pipe (one of the writes would block).
//   - The bridge's pumpStream drops frames between messages
//     (the per-message event count would be wrong).
func TestClaudeCodeBridge_RealSubprocess(t *testing.T) {
	cmd, args := claudeMockCommand(t)

	// Bump the default slog level so the bridge's drainStderr
	// surfaces the mock's stderr (the mock logs each step at
	// its own stderr; the bridge forwards them via slog.Debug).
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a := NewStarter("mock-claude", cmd, args)
	sess, err := a.Start(context.Background(), agent.StartConfig{
		Workspace:      t.TempDir(),
		PermissionMode: PermissionBypass,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	if pid := sess.PID(); pid <= 0 {
		t.Fatalf("PID = %d, want > 0 (mock child should be running)", pid)
	}

	// Three messages back-to-back. The bridge's writeLine
	// serializes the writes via stdinMu so each envelope lands
	// atomically on the child's stdin.
	const messages = 3
	for i := 0; i < messages; i++ {
		text := fmt.Sprintf("hello-%d", i)
		if err := sess.SendBlocks(context.Background(), []agent.ContentBlock{
			{Type: agent.ContentText, Text: text},
		}); err != nil {
			t.Fatalf("SendBlocks[%d] (%q): %v", i, text, err)
		}
	}

	// Collect events. Per message we expect (post F-25 v1.1 rolling-
	// log fix; --replay-user-messages was removed from DefaultArgs):
	//   - 1 "got: hello-N"     EventAgentText (assistant response)
	//   - 1 EventAgentResult (final assistant text)
	//   - 1 EventAgentDone  (terminal)
	// Total: 3 * messages.
	//
	// The loop drains every event from the channel until either
	// the channel closes (pumpStream hit EOF) or the deadline
	// trips. Counting only the categories we care about avoids
	// the trap of `assistants == messages && dones < messages` —
	// the && would short-circuit as soon as assistants reached 3
	// even when EventAgentDone for the third message was still in
	// the channel buffer.
	var replays, assistants, results, dones int
	deadline := time.After(5 * time.Second)
drain:
	for {
		// Fast-path exit: once we've seen the expected number of
		// every event kind on the closed channel, the bridge's
		// pumpStream hasn't hit EOF yet (the mock is a long-
		// lived process waiting for stdin) so we must break out
		// ourselves. The explicit check here also proves the
		// counts add up before the loop's deadline-fallback
		// ever fires.
		if assistants >= messages &&
			results >= messages && dones >= messages {
			break drain
		}
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				break drain
			}
			switch ev.Kind {
			case agent.EventAgentText:
				switch {
				case strings.HasPrefix(ev.Text, "[你] [replay] "):
					replays++
				case strings.HasPrefix(ev.Text, "got: "):
					assistants++
				}
			case agent.EventAgentResult:
				results++
			case agent.EventAgentDone:
				dones++
			}
		case <-deadline:
			t.Fatalf("deadline reached with replays=%d assistants=%d results=%d dones=%d (want assistants/results/dones == %d, replays == 0)",
				replays, assistants, results, dones, messages)
		}
	}

	if replays != 0 {
		t.Errorf("replay events = %d, want 0 (--replay-user-messages removed from DefaultArgs)", replays)
	}
	if assistants != messages {
		t.Errorf("assistant events = %d, want %d", assistants, messages)
	}
	if results != messages {
		t.Errorf("EventAgentResult count = %d, want %d", results, messages)
	}
	if dones != messages {
		t.Errorf("EventAgentDone count = %d, want %d", dones, messages)
	}
}

// TestSession_New_SendsClearUserMessage verifies F-34 §3.2.1 final
// (live binary test 2026-08-04): claudecode.New writes a properly-
// structured user-typed JSON envelope whose content is literally
// "/clear". The mock recognizes this content and replies with a
// fresh system/init event carrying a new session_id; the test
// asserts the bridge surfaces that contract end-to-end.
//
// We don't mock the stdin pipe directly — the bridge runs against
// the full mock CLI binary so writeLine → JSON-line → parser is
// exercised end-to-end (F-34 §3.2.1 final).
func TestSession_New_SendsClearUserMessage(t *testing.T) {
	cmd, args := claudeMockCommand(t)

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a := NewStarter("mock-claude", cmd, args)
	sess, err := a.Start(context.Background(), agent.StartConfig{
		Workspace:      t.TempDir(),
		PermissionMode: PermissionBypass,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	// Drive sess.New(); the mock will respond with system/init +
	// a terminal result.
	if err := sess.New(context.Background()); err != nil {
		t.Fatalf("New: %v", err)
	}

	// Drain events. Expect at least:
	//   - 1 EventAgentReady (session_id == "sess-after-clear-mock")
	//   - 1 EventAgentDone (terminal)
	deadline := time.After(5 * time.Second)
	var sawInit bool
	var initSessionID string
	var sawDone bool
loop:
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				break loop
			}
			switch ev.Kind {
			case agent.EventAgentReady:
				sawInit = true
				if true {
					initSessionID = ev.SessionID
				}
			case agent.EventAgentDone:
				sawDone = true
			}
			if sawInit && sawDone {
				break loop
			}
		case <-deadline:
			break loop
		}
	}

	if !sawInit {
		t.Fatalf("expected EventAgentReady from mock's /clear handling")
	}
	if initSessionID != "sess-after-clear-mock" {
		t.Fatalf("Init.SessionID = %q, want %q (mock's post-clear session id)",
			initSessionID, "sess-after-clear-mock")
	}
}
