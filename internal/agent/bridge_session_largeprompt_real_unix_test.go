//go:build !windows

package agent_test

// Real-CLI check that the session path carries a prompt larger than
// the platform's argv limit.
//
// The review path runs on the agent's session path: the prompt
// travels over the bridge's protocol pipe, never as an argv element.
// Linux caps one argv string at 131072 bytes and Windows caps the
// whole command line at 32767 characters, so a review prompt larger
// than that used to fail to spawn ("argument list too long").
//
// Each subtest drives agent.RunSession — the exact helper /review
// uses — with a single text block of at least 131072 bytes and
// asserts a successful turn. The same payload on the print-mode path
// fails with E2BIG; that regression is what this file closes.
//
// Gated by NIGHTME_REAL_LARGE_PROMPT=1 plus the bridge's CLI on
// PATH. The prompt is ~32k tokens, so each subtest is a real model
// call: the env gate keeps `go test ./...` cheap by default.
//
// The generic acp bridge is covered through its opencode backend —
// both speak the same wire to the same CLI.

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
	"github.com/cnlangzi/nightme/internal/bridge/claudecode"
	"github.com/cnlangzi/nightme/internal/bridge/codex"
	"github.com/cnlangzi/nightme/internal/bridge/copilot"
	"github.com/cnlangzi/nightme/internal/bridge/cursor"
	"github.com/cnlangzi/nightme/internal/bridge/dsh"
	"github.com/cnlangzi/nightme/internal/bridge/omp"
	"github.com/cnlangzi/nightme/internal/bridge/opencode"
	"github.com/cnlangzi/nightme/internal/bridge/pi"
)

// minPayloadBytes is the smallest payload that breaks the argv limit
// on Linux (32 × PAGE_SIZE).
const minPayloadBytes = 131072

// largePrompt builds a prompt of at least minPayloadBytes bytes. The
// filler is inert prose; the tail asks for a short answer so the
// model's reply stays cheap to assert on.
func largePrompt() string {
	filler := "The quick brown fox jumps over the lazy dog. "
	body := strings.Repeat(filler, minPayloadBytes/len(filler)+1)
	return body + "\nReply with exactly: PONG\n"
}

// TestSession_LargePromptOverPipe drives every review-capable
// bridge's session path with an oversized prompt.
func TestSession_LargePromptOverPipe(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_LARGE_PROMPT") == "" {
		t.Skip("NIGHTME_REAL_LARGE_PROMPT not set; skipping real-CLI oversized-prompt test")
	}

	bridges := []struct {
		name    string
		command string
		starter agent.Starter
	}{
		{"claudecode", "claude", claudecode.NewStarter("claudecode", "claude", nil)},
		{"codex", "codex", codex.NewStarter("codex", "codex", nil)},
		{"copilot", "copilot", copilot.NewStarter("copilot", "copilot", copilot.DefaultACPArgs)},
		{"cursor", "cursor-agent", cursor.NewStarter("cursor", "cursor-agent", cursor.DefaultACPArgs)},
		{"omp", "omp", omp.NewStarter("omp", "omp", nil)},
		{"opencode", "opencode", opencode.NewStarter("opencode", "opencode", []string{"acp"})},
		{"pi", "pi", pi.NewStarter("pi", "pi", nil)},
		{"acp", "opencode", acp.NewStarter("acp", "opencode", []string{"acp"}, nil, 0, 0)},
		{"dsh", "dsh", dsh.NewStarter("dsh")},
	}

	for _, b := range bridges {
		t.Run(b.name, func(t *testing.T) {
			if _, err := exec.LookPath(b.command); err != nil {
				t.Skipf("%s binary not on PATH: %v", b.command, err)
			}
			workspace := t.TempDir()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			payload := largePrompt()
			if len(payload) < minPayloadBytes {
				t.Fatalf("payload is %d bytes, want at least %d", len(payload), minPayloadBytes)
			}

			res, err := agent.RunSession(ctx, b.starter, agent.StartConfig{Workspace: workspace},
				[]agent.ContentBlock{{Type: agent.ContentText, Text: payload}})
			if err != nil {
				t.Fatalf("RunSession with a %d-byte prompt: %v", len(payload), err)
			}
			if res.Text == "" {
				t.Error("RunResult.Text empty; the oversized prompt did not reach the agent")
			}
			t.Logf("%s: session=%q model=%q textLen=%d", b.name, res.SessionID, res.Model, len(res.Text))
		})
	}
}
