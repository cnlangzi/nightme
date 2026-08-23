// print_exec_review_translate_test.go — unit tests for the
// NDJSON → AgentEvent translation layer introduced when
// runCodexReviewPlain switched from `codex review` (plain-text
// stdout, no streaming) to `codex exec review --json -o <tmpfile>`
// (full NDJSON event stream + tempfile for final answer).
//
// The translator is split into two functions on purpose:
// translateItemStarted (item.started events) and
// translateItemCompleted (item.completed + item.updated events).
// Both are called from the runNDJSON callback in runCodexReviewPlain
// (see print.go).
//
// We test them in isolation here — no codex binary, no subprocess,
// no NDJSON parsing. The fake-script path in sink_test.go covers
// the end-to-end integration; these tests lock the per-shape
// translation contract.

//go:build !windows

package codex

import (
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestTranslateItemStarted_CommandExecution — `codex exec` emits
// `item.started` when a shell command begins. The translator
// must produce an EventAgentToolStart with Name="bash" and
// Args=command so the chat channel can render
// "🔧 bash -lc <cmd>" in the receipt rolling log.
func TestTranslateItemStarted_CommandExecution(t *testing.T) {
	var captured []agent.AgentEvent
	sink := func(ev agent.AgentEvent) { captured = append(captured, ev) }

	translateItemStarted(sink, &codexExecItem{
		ID:      "item_42",
		Type:    "command_execution",
		Command: "/bin/bash -lc 'git diff main'",
		Status:  "in_progress",
	})

	if len(captured) != 1 {
		t.Fatalf("sink observed %d events, want 1", len(captured))
	}
	got := captured[0]
	if got.Kind != agent.EventAgentToolStart {
		t.Errorf("Kind = %s, want ToolStart", got.Kind)
	}
	if got.ToolStart == nil {
		t.Fatalf("ToolStart is nil")
	}
	if got.ToolStart.ID != "item_42" {
		t.Errorf("ToolStart.ID = %q, want item_42", got.ToolStart.ID)
	}
	if got.ToolStart.Name != "bash" {
		t.Errorf("ToolStart.Name = %q, want bash", got.ToolStart.Name)
	}
	if got.ToolStart.Args != "/bin/bash -lc 'git diff main'" {
		t.Errorf("ToolStart.Args = %q, want /bin/bash -lc 'git diff main'",
			got.ToolStart.Args)
	}
}

// TestTranslateItemStarted_McpToolCall — `codex exec` emits
// `item.started` for MCP tool invocations too. The Name is the
// server.tool composite so the chat can render e.g.
// "🔧 filesystem.read_file".
func TestTranslateItemStarted_McpToolCall(t *testing.T) {
	var captured []agent.AgentEvent
	sink := func(ev agent.AgentEvent) { captured = append(captured, ev) }

	translateItemStarted(sink, &codexExecItem{
		ID:     "item_99",
		Type:   "mcp_tool_call",
		Server: "filesystem",
		Tool:   "read_file",
	})

	if len(captured) != 1 {
		t.Fatalf("sink observed %d events, want 1", len(captured))
	}
	got := captured[0]
	if got.Kind != agent.EventAgentToolStart ||
		got.ToolStart.Name != "filesystem.read_file" {
		t.Errorf("got %+v, want ToolStart with name filesystem.read_file", got)
	}
}

// TestTranslateItemStarted_IgnoresOtherTypes — only
// command_execution and mcp_tool_call produce started events worth
// translating. reasoning / agent_message / file_change / error
// get a started event from codex but the translator must skip them
// (they emit real content only on completed).
func TestTranslateItemStarted_IgnoresOtherTypes(t *testing.T) {
	cases := []string{
		"reasoning", "agent_message", "file_change", "error",
		"web_search", "plan_update",
	}
	for _, typ := range cases {
		t.Run(typ, func(t *testing.T) {
			var captured []agent.AgentEvent
			sink := func(ev agent.AgentEvent) {
				captured = append(captured, ev)
			}
			translateItemStarted(sink, &codexExecItem{ID: "x", Type: typ})
			if len(captured) != 0 {
				t.Errorf("type=%q observed %d events, want 0 (start event for %q is a no-op)",
					typ, len(captured), typ)
			}
		})
	}
}

// TestTranslateItemStarted_NilSafe — nil sink and nil item are
// both no-ops, matching runNDJSON's tolerance for malformed
// lines. Bridges never crash on a single bad event.
func TestTranslateItemStarted_NilSafe(t *testing.T) {
	translateItemStarted(nil, nil)                 // both nil
	translateItemStarted(nil, &codexExecItem{})     // nil sink
	called := false
	translateItemStarted(
		func(agent.AgentEvent) { called = true },
		nil,
	) // nil item
	if called {
		t.Errorf("sink called with nil item")
	}
}

// TestTranslateItemCompleted_CommandExecution — `item.completed`
// for a successful shell command emits AgentToolEnd with the
// captured output. Exit code 0 → no "[exit N]" prefix in Output.
func TestTranslateItemCompleted_CommandExecution(t *testing.T) {
	var captured []agent.AgentEvent
	sink := func(ev agent.AgentEvent) { captured = append(captured, ev) }

	exit := 0
	translateItemCompleted(sink, &codexExecItem{
		ID:               "item_42",
		Type:             "command_execution",
		Command:          "/bin/bash -lc 'git diff main'",
		AggregatedOutput: "diff --git a/foo b/foo\n",
		ExitCode:         &exit,
		Status:           "completed",
	})

	if len(captured) != 1 {
		t.Fatalf("sink observed %d events, want 1", len(captured))
	}
	got := captured[0]
	if got.Kind != agent.EventAgentToolEnd {
		t.Errorf("Kind = %s, want ToolEnd", got.Kind)
	}
	if got.ToolEnd.ID != "item_42" {
		t.Errorf("ToolEnd.ID = %q, want item_42", got.ToolEnd.ID)
	}
	if got.ToolEnd.Output != "diff --git a/foo b/foo\n" {
		t.Errorf("ToolEnd.Output = %q, want raw stdout (no exit-code prefix on success)",
			got.ToolEnd.Output)
	}
}

// TestTranslateItemCompleted_CommandExecution_NonZeroExit —
// when exit code != 0 the bridge folds "[exit N]" into Output
// so the receipt card's "⎿ output" line shows the failure marker
// (AgentToolEndEvent has no Err field; this is the workaround).
func TestTranslateItemCompleted_CommandExecution_NonZeroExit(t *testing.T) {
	var captured []agent.AgentEvent
	sink := func(ev agent.AgentEvent) { captured = append(captured, ev) }

	exit := 2
	translateItemCompleted(sink, &codexExecItem{
		ID:               "item_42",
		Type:             "command_execution",
		Command:          "/bin/bash -lc 'false'",
		AggregatedOutput: "boom",
		ExitCode:         &exit,
		Status:           "failed",
	})

	if len(captured) != 1 || captured[0].ToolEnd == nil {
		t.Fatalf("captured = %+v, want one ToolEnd", captured)
	}
	got := captured[0].ToolEnd.Output
	if !strings.HasPrefix(got, "[exit 2] ") || !strings.Contains(got, "boom") {
		t.Errorf("ToolEnd.Output = %q, want prefix [exit 2] + stderr tail", got)
	}
}

// TestTranslateItemCompleted_Reasoning — reasoning items
// surface as EventAgentText with the ThinkingPrefix sentinel so
// the gateway.Translate maps it to OutOutMessage.Kind =
// OutThinking, which the Feishu adapter renders as a 💭 side line
// instead of an OutReply bubble.
func TestTranslateItemCompleted_Reasoning(t *testing.T) {
	var captured []agent.AgentEvent
	sink := func(ev agent.AgentEvent) { captured = append(captured, ev) }

	translateItemCompleted(sink, &codexExecItem{
		ID:   "item_5",
		Type: "reasoning",
		Text: "Scanning diff for security issues",
	})

	if len(captured) != 1 {
		t.Fatalf("captured = %+v, want one Text", captured)
	}
	got := captured[0]
	if got.Kind != agent.EventAgentText {
		t.Errorf("Kind = %s, want Text", got.Kind)
	}
	if !strings.HasPrefix(got.Text, "[思考] ") {
		t.Errorf("Text = %q, want [思考] -prefixed", got.Text)
	}
	if !strings.Contains(got.Text, "Scanning diff for security issues") {
		t.Errorf("Text missing reasoning body: %q", got.Text)
	}
}

// TestTranslateItemCompleted_FileChange — file_change items
// collapse to a single EventAgentText summarizing how many files
// changed and which paths (truncated to 8 when there are many).
// Folds into the receipt card rolling log as a 📝 line.
func TestTranslateItemCompleted_FileChange(t *testing.T) {
	cases := []struct {
		name       string
		changes    []codexExecItemChange
		wantSubstr []string
	}{
		{
			name: "single file",
			changes: []codexExecItemChange{
				{Path: "internal/foo.go", Kind: "add"},
			},
			wantSubstr: []string{"📝", "1 file", "internal/foo.go"},
		},
		{
			name: "many files truncated",
			changes: []codexExecItemChange{
				{Path: "a.go", Kind: "add"},
				{Path: "b.go", Kind: "update"},
				{Path: "c.go", Kind: "delete"},
				{Path: "d.go", Kind: "add"},
				{Path: "e.go", Kind: "update"},
				{Path: "f.go", Kind: "delete"},
				{Path: "g.go", Kind: "add"},
				{Path: "h.go", Kind: "update"},
				{Path: "i.go", Kind: "delete"},
				{Path: "j.go", Kind: "add"},
			},
			// Truncation rule: first 8 paths listed, 9th and
			// later suppressed. h.go (the 8th) MUST appear; i.go
			// and j.go MUST NOT.
			wantSubstr: []string{"📝", "10 file", "first 8", "h.go"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured []agent.AgentEvent
			sink := func(ev agent.AgentEvent) {
				captured = append(captured, ev)
			}

			translateItemCompleted(sink, &codexExecItem{
				ID:      "item_x",
				Type:    "file_change",
				Changes: tc.changes,
				Status:  "completed",
			})

			if len(captured) != 1 {
				t.Fatalf("captured = %+v, want one Text", captured)
			}
			got := captured[0]
			if got.Kind != agent.EventAgentText {
				t.Errorf("Kind = %s, want Text", got.Kind)
			}
			for _, s := range tc.wantSubstr {
				if !strings.Contains(got.Text, s) {
					t.Errorf("Text %q missing %q", got.Text, s)
				}
			}
		})
	}
}

// TestTranslateItemCompleted_McpToolCall — mcp_tool_call
// completed events produce ToolEnd with the server.tool composite
// Name.
func TestTranslateItemCompleted_McpToolCall(t *testing.T) {
	var captured []agent.AgentEvent
	sink := func(ev agent.AgentEvent) { captured = append(captured, ev) }

	translateItemCompleted(sink, &codexExecItem{
		ID:               "item_99",
		Type:             "mcp_tool_call",
		Server:           "filesystem",
		Tool:             "read_file",
		AggregatedOutput: `{"contents":"hello"}`,
		Status:           "completed",
	})

	if len(captured) != 1 {
		t.Fatalf("captured = %+v, want one ToolEnd", captured)
	}
	got := captured[0]
	if got.Kind != agent.EventAgentToolEnd {
		t.Errorf("Kind = %s, want ToolEnd", got.Kind)
	}
	if got.ToolEnd.Name != "filesystem.read_file" {
		t.Errorf("ToolEnd.Name = %q, want filesystem.read_file", got.ToolEnd.Name)
	}
	if got.ToolEnd.Output != `{"contents":"hello"}` {
		t.Errorf("ToolEnd.Output = %q", got.ToolEnd.Output)
	}
}

// TestTranslateItemCompleted_AgentMessage_Suppressed — P1 fix.
// agent_message items are DELIBERATELY DROPPED from the sink.
// The final prose surfaces exactly once via EventAgentResult
// after turn.completed (read from the -o tempfile). Emitting both
// AgentEventText for agent_message AND EventAgentResult would
// produce two visible copies via outbound.Translate (OutReply
// from Text + OutResult from Result).
func TestTranslateItemCompleted_AgentMessage_Suppressed(t *testing.T) {
	var captured []agent.AgentEvent
	sink := func(ev agent.AgentEvent) { captured = append(captured, ev) }

	translateItemCompleted(sink, &codexExecItem{
		ID:   "item_final",
		Type: "agent_message",
		Text: "This is the final review answer.",
	})

	if len(captured) != 0 {
		t.Fatalf("captured = %+v, want [] (agent_message is suppressed — see P1 fix doc)", captured)
	}
}

// TestTranslateItemCompleted_Error — error items are silently
// dropped from the sink here. The actual error case surfaces
// from a separate Error emit on the failure path
// (runCodexReviewPlin's formatCodexExitError branch). The model
// name extraction lives in the runNDJSON callback
// (extractModelFromError), not here.
func TestTranslateItemCompleted_Error(t *testing.T) {
	var captured []agent.AgentEvent
	sink := func(ev agent.AgentEvent) { captured = append(captured, ev) }

	translateItemCompleted(sink, &codexExecItem{
		ID:      "item_err",
		Type:    "error",
		Message: "Model metadata for `MiniMax-M3` not found. Defaulting to fallback...",
	})

	if len(captured) != 0 {
		t.Errorf("captured = %+v, want [] (error items are no-op'd here)", captured)
	}
}

// TestTranslateItemCompleted_NilSafe — nil sink and nil item are
// no-ops, matching runNDJSON's tolerance.
func TestTranslateItemCompleted_NilSafe(t *testing.T) {
	translateItemCompleted(nil, nil)
	translateItemCompleted(nil, &codexExecItem{Type: "command_execution"})
	called := false
	translateItemCompleted(
		func(agent.AgentEvent) { called = true },
		nil,
	)
	if called {
		t.Errorf("sink called with nil item")
	}
}