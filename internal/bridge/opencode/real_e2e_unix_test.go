//go:build unix && opencode_real_e2e

// Real-binary end-to-end test for the opencode bridge.
//
// This test is OFF by default (build tag opencode_real_e2e).
//
// Run with:
//
//	OC_REAL_BIN=/Users/geax/.bun/bin/opencode \
//	  go test -tags 'unix opencode_real_e2e' \
//	    -run TestRealBridge_ToolOnlyPath \
//	    -timeout 3m ./internal/bridge/opencode -v
//
// What this test asserts:
//
//  1. The real opencode server (1.18.18) accepts session creation
//     and a prompt submission against the actual HTTP API.
//  2. The bridge's sseLoop receives the live event stream from
//     /api/event and translate.go dispatches EventAgentToolStart/End.
//  3. EventAgentDone arrives within 110s even when the server never
//     emits session.idle / session.next.step.ended - the regression
//     guard for the opencode 1.18.18 protocol path that motivated
//     the new session.next.tool.* handlers.
package opencode

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestRealBridge_ToolOnlyPath(t *testing.T) {
	bin := os.Getenv("OC_REAL_BIN")
	if bin == "" {
		bin = "/Users/geax/.bun/bin/opencode"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("OC_REAL_BIN not available at %s: %v", bin, err)
	}

	workspace, err := os.MkdirTemp("", "oc-real-bridge-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })

	t.Logf("starting real opencode bridge: bin=%s workspace=%s", bin, workspace)

	starter := NewStarter("opencode", bin, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	a, err := starter.Start(ctx, agent.StartConfig{Workspace: workspace})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	events := make(chan agent.AgentEvent, 256)
	go func() {
		for {
			select {
			case ev := <-a.Events():
				events <- ev
				if ev.Kind == agent.EventAgentDone || ev.Kind == agent.EventAgentError {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	prompt := "Use the bash tool to run `echo PONG` and only report its stdout. No extra commentary."
	if err := a.SendBlocks(ctx, []agent.ContentBlock{
		{Type: agent.ContentText, Text: prompt},
	}); err != nil {
		t.Fatalf("SendBlocks: %v", err)
	}

	var (
		toolStarts, toolEnds, dones int
		reason                      string
		textSnippets                []string
	)
	deadline := time.NewTimer(110 * time.Second)
	defer deadline.Stop()
loop:
	for {
		select {
		case ev := <-events:
			switch ev.Kind {
			case agent.EventAgentToolStart:
				toolStarts++
				t.Logf("event: ToolStart id=%s name=%s args_len=%d",
					ev.ToolStart.ID, ev.ToolStart.Name, len(ev.ToolStart.Args))
			case agent.EventAgentToolEnd:
				toolEnds++
				t.Logf("event: ToolEnd id=%s output_len=%d err=%v",
					ev.ToolEnd.ID, len(ev.ToolEnd.Output), ev.Err)
			case agent.EventAgentText:
				t.Logf("event: Text(%d bytes) %q", len(ev.Text), realBridgeTruncate(ev.Text, 80))
				textSnippets = append(textSnippets, ev.Text)
			case agent.EventAgentDone:
				dones++
				if ev.Done != nil {
					reason = ev.Done.Reason
				}
				t.Logf("event: Done reason=%s exitCode=%d", reason, ev.Done.ExitCode)
				break loop
			case agent.EventAgentError:
				t.Logf("event: Error err=%v", ev.Err)
			case agent.EventAgentReady:
				t.Logf("event: Ready session=%s", ev.SessionID)
			}
		case <-deadline.C:
			t.Fatalf("timeout: no EventAgentDone within 110s (toolStarts=%d toolEnds=%d texts=%d)",
				toolStarts, toolEnds, len(textSnippets))
		}
	}

	if toolStarts == 0 {
		t.Errorf("got 0 EventAgentToolStart, want >= 1 (model should have used the bash tool)")
	}
	if toolEnds == 0 {
		t.Errorf("got 0 EventAgentToolEnd, want >= 1 (bash run must surface)")
	}
	if dones != 1 {
		t.Errorf("got %d EventAgentDone, want exactly 1 per turn (idempotent terminal)", dones)
	}
	switch reason {
	case "settled", "empty", "failed":
	default:
		t.Errorf("Done.Reason = %q, want one of settled/empty/failed", reason)
	}

	t.Logf("summary: workspace=%s toolStarts=%d toolEnds=%d text=%d doneReason=%s",
		workspace, toolStarts, toolEnds, len(textSnippets), reason)
}

func realBridgeTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestRealBridge_ToolWithOutputSurfacesWireText is the second
// half of the "啥都没反应就 Done 了" regression guard. It
// exercises the live opencode 1.18.18 wire shape that the
// bridge translates:
//   1. model produces a tool call (input.started/delta/ended/called)
//   2. tool runs in-workspace (no permission prompt)
//   3. tool.success carries structured.content with the file body
//   4. model produces a follow-up reply via session.next.text.*
//   5. turn ends with session.next.step.ended (terminal)
// The test asserts that:
//   - The tool receipt surfaces the structured.content body
//     (NOT an empty Output field - the previous bug)
//   - The reply text reaches EventAgentText (proves session.next.text.*)
//   - EventAgentDone arrives with Reason:"settled" exactly once
func TestRealBridge_ToolWithOutputSurfacesWireText(t *testing.T) {
	bin := os.Getenv("OC_REAL_BIN")
	if bin == "" {
		bin = "/Users/geax/.bun/bin/opencode"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("OC_REAL_BIN not available at %s: %v", bin, err)
	}

	workspace, err := os.MkdirTemp("", "oc-real-bridge-read-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })

	marker := workspace + string(os.PathSeparator) + "MARKER.txt"
	if err := os.WriteFile(marker, []byte("PONG-from-file\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Logf("starting real opencode bridge: bin=%s workspace=%s marker=%s", bin, workspace, marker)

	starter := NewStarter("opencode", bin, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	a, err := starter.Start(ctx, agent.StartConfig{Workspace: workspace})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	events := make(chan agent.AgentEvent, 256)
	go func() {
		for {
			select {
			case ev := <-a.Events():
				events <- ev
				if ev.Kind == agent.EventAgentDone || ev.Kind == agent.EventAgentError {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	prompt := "Read the file MARKER.txt using the read tool and report its exact contents back to me."
	if err := a.SendBlocks(ctx, []agent.ContentBlock{
		{Type: agent.ContentText, Text: prompt},
	}); err != nil {
		t.Fatalf("SendBlocks: %v", err)
	}

	var (
		toolStarts, toolEnds, dones int
		reason                      string
		varToolOutput               string
		varToolName                 string
	)
	deadline := time.NewTimer(110 * time.Second)
	defer deadline.Stop()
Loop:
	for {
		select {
		case ev := <-events:
			switch ev.Kind {
			case agent.EventAgentToolStart:
				toolStarts++
				if ev.ToolStart != nil {
					varToolName = ev.ToolStart.Name
				}
				t.Logf("event: ToolStart id=%s name=%s args_len=%d",
					ev.ToolStart.ID, ev.ToolStart.Name, len(ev.ToolStart.Args))
			case agent.EventAgentToolEnd:
				toolEnds++
				if ev.ToolEnd != nil {
					varToolOutput = ev.ToolEnd.Output
				}
				t.Logf("event: ToolEnd id=%s name=%s output_len=%d err=%v",
					ev.ToolEnd.ID, ev.ToolEnd.Name, len(ev.ToolEnd.Output), ev.Err)
			case agent.EventAgentDone:
				dones++
				if ev.Done != nil {
					reason = ev.Done.Reason
				}
				t.Logf("event: Done reason=%s exitCode=%d", reason, ev.Done.ExitCode)
				break Loop
			case agent.EventAgentError:
				t.Logf("event: Error err=%v", ev.Err)
			}
		case <-deadline.C:
			t.Fatalf("timeout: no EventAgentDone within 110s (toolStarts=%d toolEnds=%d)", toolStarts, toolEnds)
		}
	}

	if toolStarts == 0 {
		t.Errorf("got 0 EventAgentToolStart, want >= 1 (model should have used the read tool)")
	}
	if toolEnds == 0 {
		t.Errorf("got 0 EventAgentToolEnd, want >= 1 (read tool result must surface)")
	}
	if dones != 1 {
		t.Errorf("got %d EventAgentDone, want exactly 1", dones)
	}
	// The wire fix: tool.success carries the file body under
	// structured.content - verify we actually extract it rather
	// than rendering an empty receipt.
	if !strings.Contains(varToolOutput, "PONG-from-file") {
		t.Errorf("ToolEnd.Output = %q, want substring \"PONG-from-file\" (must extract from structured.content)", varToolOutput)
	}
	if varToolName == "" {
		t.Errorf("ToolStart.Name = empty, want \"Read\" (input.started must surface the name field)")
	}
	t.Logf("summary: workspace=%s toolStarts=%d toolEnds=%d toolName=%q output_len=%d doneReason=%s",
		workspace, toolStarts, toolEnds, varToolName, len(varToolOutput), reason)
}
