//go:build !windows

// Real-machine Start integration test for the copilot bridge.
//
// Exercises the FULL bridge chain against the real copilot
// binary:
//
//   Starter.Start (PTY + ACP handshake: initialize +
//                  session/new)  →  *agent.Agent
//   SendBlocks                   →  driver translates
//                                   to ACP session/prompt
//   Events() <-chan AgentEvent   ←  driver translates
//                                   agent_message_chunk /
//                                   session.status / etc.
//
// Skipped when no `copilot` binary on PATH. End-to-end takes
// 30-60s due to the model call.
package copilot

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestStart_RealBinary_FullPromptFlow drives the bridge end-to-end:
//
//  1. Starter.Start → ACP handshake (initialize + session/new)
//  2. Expect EventAgentReady with non-empty SessionID
//  3. SendBlocks("Reply with just: PONG")
//  4. Drain Events() until EventAgentResult or EventAgentError
//  5. Expect Result.Text contains "PONG"
//  6. Close()
//
// This is the canary test for the bridge — if it breaks, the
// IM channel can't drive a long-lived copilot session.
func TestStart_RealBinary_FullPromptFlow(t *testing.T) {
	if _, err := exec.LookPath("copilot"); err != nil {
		t.Skipf("copilot binary not on PATH: %v", err)
	}

	s := NewStarter("copilot", "copilot", DefaultACPArgs)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	workspace := t.TempDir()

	a, err := s.Start(ctx, agent.StartConfig{Workspace: workspace})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Close()

	// 1. Wait for EventAgentReady (handshake completed, session ID known).
	//    Bridge contract: EventAgentReady fires before any user-facing events.
	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	defer readyCancel()
	var readySessionID string
	for readySessionID == "" {
		select {
		case ev, ok := <-a.Events():
			if !ok {
				t.Fatal("events channel closed before Ready")
			}
			switch ev.Kind {
			case agent.EventAgentReady:
				readySessionID = ev.SessionID
				t.Logf("EventAgentReady sessionID=%q", readySessionID)
			case agent.EventAgentError:
				t.Fatalf("got EventAgentError before Ready: %+v", ev.Err)
			default:
				t.Logf("pre-Ready event: kind=%v", ev.Kind)
			}
		case <-readyCtx.Done():
			t.Fatalf("timed out waiting for EventAgentReady: %v", readyCtx.Err())
		}
	}

	if readySessionID == "" {
		t.Fatal("EventAgentReady arrived but SessionID was empty")
	}

	// 2. Send the prompt.
	if err := a.SendBlocks(ctx, []agent.ContentBlock{
		{Type: agent.ContentText, Text: "Reply with just the word: PONG"},
	}); err != nil {
		t.Fatalf("SendBlocks: %v", err)
	}

	// 3. Drain until EventAgentDone or EventAgentError. For ACP
	//    bridges the answer arrives as one or more EventAgentText
	//    events (the answer is streamed as `agent_message_chunk`),
	//    and turn-end is signalled by EventAgentDone (emitted from
	//    the `session.status:idle` handler) — EventAgentResult is
	//    NOT used by the ACP bridge per docs/bridge/acp.md §4.
	//
	//    We accumulate every EventAgentText's Text and assert the
	//    answer "PONG" appears somewhere in the streamed chunks
	//    (it may be split across multiple chunks or preceded by
	//    `[思考] ...` thought-prefix chunks; both are valid).
	type result struct {
		streamedText strings.Builder
		err          error
		hasError     bool
	}
	ch := make(chan result, 1)
	go func() {
		var r result
		for ev := range a.Events() {
			t.Logf("event: kind=%v text=%q err=%v", ev.Kind, ev.Text, ev.Err)
			switch ev.Kind {
			case agent.EventAgentText:
				r.streamedText.WriteString(ev.Text)
			case agent.EventAgentError:
				r.err = ev.Err
				r.hasError = true
				ch <- r
				return
			case agent.EventAgentDone:
				ch <- r
				return
			}
		}
		ch <- r // events closed without terminal
	}()

	var finalText string
	select {
	case r := <-ch:
		if r.hasError {
			t.Fatalf("terminal EventAgentError: %v", r.err)
		}
		finalText = r.streamedText.String()
	case <-ctx.Done():
		t.Fatalf("timed out waiting for turn result: %v", ctx.Err())
	}

	// The model may emit reasoning ("[思考] ...") before the
	// answer, then "PONG". Both get prepended to the same
	// streamed-text accumulator. Assert "PONG" appears
	// anywhere in the accumulated stream — this catches
	// "empty response" regressions while tolerating the normal
	// thought-prefix noise.
	if !strings.Contains(finalText, "PONG") {
		t.Errorf("streamed text does not contain PONG: %q", finalText)
	}
	t.Logf("streamed text = %q (sessionId=%s)", finalText, readySessionID)
}