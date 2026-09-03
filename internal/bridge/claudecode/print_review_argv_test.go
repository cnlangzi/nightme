// print_review_argv_test.go — end-to-end test that captures the
// actual argv `(*Starter).Review` spawns and pins the local-branch
// positional. The v1 implementation passed
// `<defaultBase>...HEAD` (a ref-range) which Claude Code's
// `/code-review` plugin rejected, falling back to "list open PRs"
// in environments with a remote. The fix passes the local branch
// name. This test ensures the argv shape doesn't regress.
//
//go:build !windows

package claudecode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestReview_LocalBranchPositional drives Review through a
// throwaway sh mock that captures argv to a file and emits a
// stream-json "empty" result. We assert the captured argv:
//
//  1. starts with `-p code-review <localBranch>` (the fix's
//     contract) — NOT `defaultBase...HEAD` (the pre-fix bug) and
//     NOT a bare `code-review` with no target (the missing-PR
//     fallback path)
//  2. ends with the standard print-mode tail
//     (--output-format stream-json --verbose --permission-mode
//     bypassPermissions)
//
// Why a one-shot sh mock and not the existing long-lived
// `claude_mock.sh` / `claude_mock.py`: the existing mock ignores
// argv and only responds to stdin JSON envelopes. Review never
// sends stdin to a print-mode `-p` invocation; it just waits for
// the child to exit after one turn. So we need a mock that exits
// cleanly on its own after capturing argv and emitting a result.
func TestReview_LocalBranchPositional(t *testing.T) {
	// 1. Build a throwaway git repo with a feature branch.
	repoDir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-q", "-m", "root"},
		{"checkout", "-q", "-b", "fix-review-on-claude"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// 2. Build a one-shot sh mock in a temp dir. The mock:
	//    - writes every argv element to argvFile (one per line)
	//    - emits a stream-json "empty" result and exits
	mockDir := t.TempDir()
	argvFile := filepath.Join(mockDir, "argv.txt")
	mockPath := filepath.Join(mockDir, "fake-claude")
	mockScript := `#!/bin/sh
# One-shot mock for print-mode ` + "`-p`" + ` review. Captures argv
# to $ARGV_FILE then emits a stream-json result event so the
# bridge's parsePrintStream sees a complete turn.
echo "$0" > "$ARGV_FILE"
i=1
for a in "$@"; do
	echo "arg[$i]=$a" >> "$ARGV_FILE"
	i=$((i+1))
done
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"mock review result","duration_ms":100}'
exit 0
`
	if err := os.WriteFile(mockPath, []byte(mockScript), 0o755); err != nil {
		t.Fatalf("write mock: %v", err)
	}

	// 3. Drive Review against the throwaway repo. The mock
	// captures its argv to $ARGV_FILE (we read it back below)
	// and emits a stream-json result line on stdout (consumed
	// by the bridge via its own StdoutPipe). No additional
	// stdout plumbing is needed here — the mock's stdout is
	// the bridge's input, period.
	a := NewStarter("mock-claude", mockPath, nil)

	mockEnv := []string{"ARGV_FILE=" + argvFile}

	result, err := a.Review(context.Background(), agent.StartConfig{
		Workspace: repoDir,
		Env:       mockEnv,
	})
	if err != nil {
		t.Fatalf("Review: %v\nresult: %+v", err, result)
	}

	// 4. Read captured argv and assert.
	rawArgv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read argv file: %v", err)
	}
	argvLines := strings.Split(strings.TrimRight(string(rawArgv), "\n"), "\n")
	if len(argvLines) < 2 {
		t.Fatalf("argv file too short: %q", rawArgv)
	}
	// First line: path to the script ($0). Skip it.
	var args []string
	for _, line := range argvLines[1:] {
		const prefix = "arg["
		if !strings.HasPrefix(line, prefix) {
			t.Fatalf("unexpected argv line: %q", line)
		}
		// arg[N]=value
		eq := strings.IndexByte(line, '=')
		args = append(args, line[eq+1:])
	}

	// Expected argv: -p code-review <branch> --output-format
	// stream-json --verbose --permission-mode bypassPermissions.
	wantPrefix := []string{"-p", "code-review", "fix-review-on-claude"}
	wantTail := []string{
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
	}
	if len(args) < len(wantPrefix)+len(wantTail) {
		t.Fatalf("argv too short (%d): %v", len(args), args)
	}
	for i, want := range wantPrefix {
		if args[i] != want {
			t.Errorf("argv[%d] = %q, want %q\nfull argv: %v", i, args[i], want, args)
		}
	}
	// Local branch name is the critical fix-point: it must be
	// `fix-review-on-claude` (the active branch), NOT
	// `<default>...HEAD` (the pre-fix ref-range that triggered
	// the gh pr list fallback bug).
	if args[2] != "fix-review-on-claude" {
		t.Errorf("argv[2] = %q, want %q (the active local branch)", args[2], "fix-review-on-claude")
	}
	tailStart := len(args) - len(wantTail)
	for i, want := range wantTail {
		if args[tailStart+i] != want {
			t.Errorf("argv tail[%d] = %q, want %q\nfull argv: %v", i, args[tailStart+i], want, args)
		}
	}

	// The result text should be the mock's "mock review result".
	// parsePrintStream extracts the result event's `result` field
	// into RunResult.Text; verify the round-trip.
	if !strings.Contains(result.Text, "mock review result") {
		t.Errorf("result.Text = %q, want to contain %q", result.Text, "mock review result")
	}

	// 5. Sanity: verify the result event landed in the result
	// struct shape parsePrintStream produces. (The mock's
	// duration_ms = 100 should round-trip into DurationMs.)
	if result.DurationMs <= 0 {
		t.Errorf("DurationMs = %d, want > 0 (mock set 100)", result.DurationMs)
	}
}

// TestReview_SinkReceivesLifecycle pins the per-call event sink
// forwarding that the v10 fix added. Pre-v10 dropped opts at
// (*Starter).Review → runCodeReviewPrintMode, so the chat
// channel's StatusBar sat on "Working…" for the entire review
// run. Post-v10: Ready at start, Result at end.
func TestReview_SinkReceivesLifecycle(t *testing.T) {
	// Same throwaway-repo + sh-mock plumbing as the argv test.
	repoDir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-q", "-m", "root"},
		{"checkout", "-q", "-b", "feat-sink"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	mockDir := t.TempDir()
	stdoutFile := filepath.Join(mockDir, "stdout.jsonl")
	preResult := `{"type":"result","subtype":"success","is_error":false,"result":"sink test","duration_ms":50}` + "\n"
	if err := os.WriteFile(stdoutFile, []byte(preResult), 0o644); err != nil {
		t.Fatalf("write pre-result: %v", err)
	}
	mockPath := filepath.Join(mockDir, "fake-claude")
	if err := os.WriteFile(mockPath, []byte(`#!/bin/sh
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"sink test","duration_ms":50}'
exit 0
`), 0o755); err != nil {
		t.Fatalf("write mock: %v", err)
	}

	// Collect sink events.
	var events []agent.AgentEvent
	sink := func(ev agent.AgentEvent) {
		events = append(events, ev)
	}
	a := NewStarter("mock-claude", mockPath, nil)
	if _, err := a.Review(context.Background(), agent.StartConfig{
		Workspace: repoDir,
		Env:       []string{"STDOUT_FILE=" + stdoutFile},
	}, agent.WithEventSink(sink)); err != nil {
		t.Fatalf("Review: %v", err)
	}

	// Expect at minimum: 1 Ready at start, 1 Result at end.
	// Error events would also be acceptable on this path, but
	// the mock returns success so we should see no errors.
	var sawReady, sawResult bool
	for _, ev := range events {
		switch ev.Kind {
		case agent.EventAgentReady:
			sawReady = true
			if ev.AgentName != "mock-claude" {
				t.Errorf("Ready AgentName = %q, want %q", ev.AgentName, "mock-claude")
			}
			if ev.Workspace != repoDir {
				t.Errorf("Ready Workspace = %q, want %q", ev.Workspace, repoDir)
			}
		case agent.EventAgentResult:
			sawResult = true
			if ev.Result == nil || !strings.Contains(ev.Result.Text, "sink test") {
				t.Errorf("Result.Text = %v, want to contain %q", ev.Result, "sink test")
			}
		case agent.EventAgentError:
			t.Errorf("unexpected Error event: %v", ev)
		}
	}
	if !sawReady {
		t.Errorf("sink missed EventAgentReady; got events: %+v", events)
	}
	if !sawResult {
		t.Errorf("sink missed EventAgentResult; got events: %+v", events)
	}
}
