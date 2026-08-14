// starter_test.go — single-CLI-runnable unit tests for the dsh
// bridge. Covers Info / Detect / blocksToPrompt / runPrintMode
// using the real local `dsh` binary (no mock; dsh is installed via
// npm on the test machine). The e2e ping test in
// print_real_unix_test.go gates the larger end-to-end against
// NIGHTME_REAL_DSH=1.
package dsh

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestNewStarter_Info pins the static metadata contract: name,
// Mode=ModeJSONIO, Command="dsh", Args=["--profile","headless"],
// no env.
func TestNewStarter_Info(t *testing.T) {
	s := NewStarter("dsh")
	info := s.Info()

	if info.Name != "dsh" {
		t.Errorf("Name=%q, want dsh", info.Name)
	}
	if info.Mode != agent.ModeJSONIO {
		t.Errorf("Mode=%v, want ModeJSONIO", info.Mode)
	}
	if info.Command != "dsh" {
		t.Errorf("Command=%q, want dsh", info.Command)
	}
	if got, want := info.Args, []string{"--profile", "headless"}; len(got) != len(want) {
		t.Errorf("Args=%v, want %v", got, want)
	} else {
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("Args[%d]=%q, want %q", i, got[i], want[i])
			}
		}
	}
	if len(info.Env) != 0 {
		t.Errorf("Env=%v, want empty (no env injection at Starter level)", info.Env)
	}
}

// TestNewStarter_ArgsDefensiveCopy verifies that mutating the caller's
// slice after NewStarter does not mutate Starter state. (Defensive
// copy invariant — see codex/starter.go:35 for the precedent.)
func TestNewStarter_ArgsDefensiveCopy(t *testing.T) {
	s := NewStarter("dsh")
	info := s.Info()
	if len(info.Args) > 0 {
		info.Args[0] = "MUTATED"
	}
	info2 := s.Info()
	if info2.Args[0] != "--profile" {
		t.Errorf("Starter.Args mutated through Info(): got %q, want %q",
			info2.Args[0], "--profile")
	}
}

// TestDetect_OK and TestDetect_NoDsh gate the install-hint error.
// On the test machine `dsh` is in PATH (npm-installed via nvm), so
// Detect() should succeed.
func TestDetect_OK(t *testing.T) {
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skipf("dsh not in PATH; skipping Detect_OK test: %v", err)
	}
	s := NewStarter("dsh")
	if err := s.Detect(); err != nil {
		t.Errorf("Detect() = %v, want nil", err)
	}
}

// TestDetect_MissingBinary simulates a missing dsh by pointing the
// Starter at a binary name that cannot exist. We use a name that's
// guaranteed not in PATH (contains a slash which makes LookPath fail
// on non-existent paths, plus a unique suffix).
func TestDetect_MissingBinary(t *testing.T) {
	s := NewStarter("definitely-not-a-real-binary-xyz123")
	err := s.Detect()
	if err == nil {
		t.Errorf("Detect() = nil, want error")
	}
	if !strings.Contains(err.Error(), "not found") &&
		!strings.Contains(err.Error(), "PATH") {
		t.Errorf("Detect() error %q should mention 'not found' or 'PATH'", err.Error())
	}
	if !strings.Contains(err.Error(), "npm install") {
		t.Errorf("Detect() error %q should include install hint", err.Error())
	}
}

// TestStart_NotImplemented pins the chat-session error. This is the
// contract that surfaces the limitation to the user instead of
// silently falling back to PTY noise.
func TestStart_NotImplemented(t *testing.T) {
	s := NewStarter("dsh")
	_, err := s.Start(context.Background(), agent.StartConfig{
		Workspace: "/tmp",
	})
	if err == nil {
		t.Fatal("Start() = nil, want error")
	}
	if !strings.Contains(err.Error(), "chat session not implemented") {
		t.Errorf("Start() error %q should mention 'chat session not implemented'", err.Error())
	}
}

// TestBlocksToPrompt_TextOnly covers the common case: multiple
// ContentText blocks joined with "\n".
func TestBlocksToPrompt_TextOnly(t *testing.T) {
	got := blocksToPrompt([]agent.ContentBlock{
		{Type: agent.ContentText, Text: "hello"},
		{Type: agent.ContentText, Text: "world"},
	})
	want := "hello\nworld"
	if got != want {
		t.Errorf("blocksToPrompt(text) = %q, want %q", got, want)
	}
}

// TestBlocksToPrompt_EmptySkipped covers: empty text blocks are
// skipped (no leading/trailing newlines).
func TestBlocksToPrompt_EmptySkipped(t *testing.T) {
	got := blocksToPrompt([]agent.ContentBlock{
		{Type: agent.ContentText, Text: "hi"},
		{Type: agent.ContentText, Text: ""},
		{Type: agent.ContentText, Text: "there"},
	})
	want := "hi\nthere"
	if got != want {
		t.Errorf("blocksToPrompt() = %q, want %q", got, want)
	}
}

// TestBlocksToPrompt_ImageAndFile covers the fallback annotation
// path: ContentImage → "[image: <path> (<mime>)]", ContentFile →
// "[file: <path>]".
func TestBlocksToPrompt_ImageAndFile(t *testing.T) {
	got := blocksToPrompt([]agent.ContentBlock{
		{Type: agent.ContentText, Text: "look at this"},
		{Type: agent.ContentImage, Path: "/tmp/img.png", MediaType: "image/png"},
		{Type: agent.ContentFile, Path: "/tmp/data.json"},
	})
	want := "look at this\n[image: /tmp/img.png (image/png)]\n[file: /tmp/data.json]"
	if got != want {
		t.Errorf("blocksToPrompt() = %q, want %q", got, want)
	}
}

// TestBlocksToPrompt_EmptyPathSkipped covers: an Image / File with
// Path="" is skipped silently.
func TestBlocksToPrompt_EmptyPathSkipped(t *testing.T) {
	got := blocksToPrompt([]agent.ContentBlock{
		{Type: agent.ContentImage, Path: "", MediaType: "image/png"},
		{Type: agent.ContentText, Text: "hello"},
	})
	want := "hello"
	if got != want {
		t.Errorf("blocksToPrompt() = %q, want %q", got, want)
	}
}

// TestRunOnce_EmptyWorkspaceError verifies the workspace preflight.
// runPrintMode requires cfg.Workspace != "" before spawning.
func TestRunOnce_EmptyWorkspaceError(t *testing.T) {
	s := NewStarter("dsh")
	_, err := s.RunOnce(context.Background(), agent.StartConfig{}, nil)
	if err == nil {
		t.Fatal("RunOnce(empty workspace) = nil err, want error")
	}
	if !strings.Contains(err.Error(), "workspace is required") {
		t.Errorf("RunOnce(empty) err %q should mention 'workspace is required'", err.Error())
	}
}

// TestRunOnce_NoBinaryError covers the case where dsh is not in PATH.
// We construct a Starter whose command points at a guaranteed-missing
// binary and expect exec.LookPath to fail at child start.
func TestRunOnce_NoBinaryError(t *testing.T) {
	s := &Starter{
		name:    "dsh-missing",
		command: "definitely-not-a-real-binary-xyz123",
		args:    []string{"--profile", "headless"},
	}
	_, err := s.RunOnce(context.Background(), agent.StartConfig{
		Workspace: "/tmp",
	}, []agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}})
	if err == nil {
		t.Fatal("RunOnce(missing binary) = nil err, want error")
	}
	if !strings.Contains(err.Error(), "start") {
		t.Errorf("RunOnce(missing) err %q should mention 'start' (spawn failure)", err.Error())
	}
}

// TestRunOnce_ContextCancel covers ctx-cancellation. We cancel the
// context before the spawn returns, which should propagate to the
// child via exec.CommandContext's SIGKILL. The exact error shape is
// OS-dependent (timeout vs signal), but we expect an error and
// reasonable wall-clock duration (not hanging forever).
func TestRunOnce_ContextCancel(t *testing.T) {
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skipf("dsh not in PATH; skipping: %v", err)
	}
	s := NewStarter("dsh")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE RunOnce spawns
	start := time.Now()
	_, err := s.RunOnce(ctx, agent.StartConfig{
		Workspace: "/tmp",
	}, []agent.ContentBlock{{Type: agent.ContentText, Text: "Reply with PONG"}})
	elapsed := time.Since(start)
	if err == nil {
		t.Errorf("RunOnce(cancelled ctx) = nil err, want error")
	}
	if elapsed > 10*time.Second {
		t.Errorf("RunOnce(cancelled ctx) took %v, expected < 10s", elapsed)
	}
}
