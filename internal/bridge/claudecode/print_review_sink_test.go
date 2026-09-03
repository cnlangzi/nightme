// print_review_sink_test.go — unit tests for parsePrintStream's
// intermediate-event forwarding to the per-call sink. v10's bug
// (chat sees nothing during a long /code-review) was caused by
// assistant / tool_use / tool_result events being parsed but
// never sent to the sink; this test pins the forward path so
// future refactors don't regress it.
//
//go:build !windows

package claudecode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// sinkRecorder is a minimal agent.AgentEvent sink for tests.
// Synchronous + thread-safe so we can call from the parser
// goroutine and inspect after parsePrintStream returns.
type sinkRecorder struct {
	mu     sync.Mutex
	events []agent.AgentEvent
}

func (r *sinkRecorder) record(ev agent.AgentEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *sinkRecorder) byKind(k agent.EventKind) []agent.AgentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []agent.AgentEvent
	for _, ev := range r.events {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}
	return out
}

// TestParsePrintStream_ForwardsAssistantTextToSink pins the
// assistant-text forward path. Pre-v10 the chat sat on "Working…"
// for the full review duration; the fix forwards every
// assistant text block to the sink so the chat channel's
// StatusBar / receipt shows the plugin's intermediate progress.
func TestParsePrintStream_ForwardsAssistantTextToSink(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-4-5"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"I'll invoke the code-review skill."}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Reviewer 1 finished."}]}}`,
		`{"type":"result","subtype":"success","session_id":"s1","duration_ms":12345,"result":"final review text"}`,
		"",
	}, "\n")

	rec := &sinkRecorder{}
	got, err := parsePrintStream(context.Background(), strings.NewReader(stream), true, rec.record)
	if err != nil {
		t.Fatalf("parsePrintStream: %v", err)
	}

	// Result event's text wins for Result; sink captures the
	// intermediate text separately.
	if got.Text != "final review text" {
		t.Errorf("Result.Text = %q, want %q", got.Text, "final review text")
	}

	textEvents := rec.byKind(agent.EventAgentText)
	if len(textEvents) != 2 {
		t.Fatalf("sink received %d EventAgentText events, want 2", len(textEvents))
	}
	wantTexts := []string{
		"I'll invoke the code-review skill.",
		"Reviewer 1 finished.",
	}
	for i, want := range wantTexts {
		if textEvents[i].Text != want {
			t.Errorf("textEvents[%d].Text = %q, want %q", i, textEvents[i].Text, want)
		}
	}
}

// TestParsePrintStream_ForwardsToolUseAndToolResult pins the
// tool lifecycle forward path. Tool calls give the chat
// renderer structured progress markers ("Skill: code-review",
// "Bash: git diff main...HEAD", …) without flooding it with
// raw model text. tool_use → ToolStart, tool_result → ToolEnd
// pair by ID so the renderer can match them.
//
// v12.1 also asserts Args correlation (the toolUseArgs map
// populated on tool_use surfaces the same input on the
// matching tool_result's ToolEnd.Args) and that content
// payloads go through stringifyToolResult (so the chat
// renderer sees "review finished" instead of the raw
// 16-char JSON string `"review finished"` with literal
// quotes).
func TestParsePrintStream_ForwardsToolUseAndToolResult(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-4-5"}`,
		// assistant → tool_use (Skill)
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Skill","input":{"skill":"code-review"}}]}}`,
		// user → tool_result (Skill's output as JSON string)
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"review finished"}]}}`,
		// assistant → another tool_use (Bash)
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool-2","name":"Bash","input":{"command":"git diff main...HEAD"}}]}}`,
		// user → tool_result (Bash's output as JSON string)
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-2","content":"(Bash completed with no output)"}]}}`,
		`{"type":"result","subtype":"success","session_id":"s1","duration_ms":12345,"result":"final"}`,
		"",
	}, "\n")

	rec := &sinkRecorder{}
	if _, err := parsePrintStream(context.Background(), strings.NewReader(stream), true, rec.record); err != nil {
		t.Fatalf("parsePrintStream: %v", err)
	}

	starts := rec.byKind(agent.EventAgentToolStart)
	if len(starts) != 2 {
		t.Fatalf("sink received %d EventAgentToolStart events, want 2", len(starts))
	}
	wantStartNames := []string{"Skill", "Bash"}
	wantStartIDs := []string{"tool-1", "tool-2"}
	wantStartArgs := []string{
		`{"skill":"code-review"}`,
		`{"command":"git diff main...HEAD"}`,
	}
	for i, want := range wantStartNames {
		if starts[i].ToolStart == nil {
			t.Fatalf("starts[%d].ToolStart nil", i)
		}
		if starts[i].ToolStart.Name != want {
			t.Errorf("starts[%d].ToolStart.Name = %q, want %q", i, starts[i].ToolStart.Name, want)
		}
		if starts[i].ToolStart.ID != wantStartIDs[i] {
			t.Errorf("starts[%d].ToolStart.ID = %q, want %q", i, starts[i].ToolStart.ID, wantStartIDs[i])
		}
		if starts[i].ToolStart.Args != wantStartArgs[i] {
			t.Errorf("starts[%d].ToolStart.Args = %q, want %q", i, starts[i].ToolStart.Args, wantStartArgs[i])
		}
	}

	ends := rec.byKind(agent.EventAgentToolEnd)
	if len(ends) != 2 {
		t.Fatalf("sink received %d EventAgentToolEnd events, want 2", len(ends))
	}
	// ToolEnd.Output must go through stringifyToolResult:
	// the chat renderer should see the unquoted content
	// ("review finished"), NOT the raw JSON literal
	// `"review finished"` with quotes (v12.1 fix).
	wantEndOutput := []string{"review finished", "(Bash completed with no output)"}
	wantEndArgs := wantStartArgs // correlation: same args surface on ToolEnd
	for i, ev := range ends {
		if ev.ToolEnd == nil {
			t.Fatalf("ends[%d].ToolEnd nil", i)
		}
		if ev.ToolEnd.ID != wantStartIDs[i] {
			t.Errorf("ends[%d].ToolEnd.ID = %q, want %q", i, ev.ToolEnd.ID, wantStartIDs[i])
		}
		if ev.ToolEnd.Output != wantEndOutput[i] {
			t.Errorf("ends[%d].ToolEnd.Output = %q, want %q (stringifyToolResult must unwrap the JSON string content)",
				i, ev.ToolEnd.Output, wantEndOutput[i])
		}
		if ev.ToolEnd.Args != wantEndArgs[i] {
			t.Errorf("ends[%d].ToolEnd.Args = %q, want %q (toolUseArgs correlation)",
				i, ev.ToolEnd.Args, wantEndArgs[i])
		}
		if ev.Err != nil {
			t.Errorf("ends[%d].Err = %v, want nil (tool_result was not error)", i, ev.Err)
		}
	}
}

// TestParsePrintStream_ToolResultArrayContentFlattens pins the
// multi-modal tool_result shape: when content is an array of
// content blocks (e.g. Read tool returning both a text
// description and an image annotation), stringifyToolResult
// must flatten text blocks into a single-line summary rather
// than emitting the raw JSON array.
func TestParsePrintStream_ToolResultArrayContentFlattens(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-4-5"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Read","input":{"file_path":"/tmp/x.go"}}]}}`,
		// content is a JSON array of two text blocks (multi-modal
		// shape); stringifyToolResult must flatten to "alpha | beta".
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":[{"type":"text","text":"alpha"},{"type":"text","text":"beta"}]}]}}`,
		`{"type":"result","subtype":"success","session_id":"s1","duration_ms":100,"result":"final"}`,
		"",
	}, "\n")

	rec := &sinkRecorder{}
	if _, err := parsePrintStream(context.Background(), strings.NewReader(stream), true, rec.record); err != nil {
		t.Fatalf("parsePrintStream: %v", err)
	}
	ends := rec.byKind(agent.EventAgentToolEnd)
	if len(ends) != 1 {
		t.Fatalf("sink received %d ToolEnd events, want 1", len(ends))
	}
	want := "alpha | beta"
	if ends[0].ToolEnd.Output != want {
		t.Errorf("ToolEnd.Output = %q, want %q (array content must flatten via stringifyToolResult)",
			ends[0].ToolEnd.Output, want)
	}
}

// TestParsePrintStream_ToolErrorResultStampsErr pins the
// failure-mapping on tool_result: a tool_result whose
// is_error=true must surface as a non-nil Err on the
// ToolEnd event so the chat renderer's rolling log can
// mark the failure without parsing the raw content itself.
func TestParsePrintStream_ToolErrorResultStampsErr(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-4-5"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"false"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","is_error":true,"content":"exit 1"}]}}`,
		`{"type":"result","subtype":"success","session_id":"s1","duration_ms":100,"result":"done"}`,
		"",
	}, "\n")

	rec := &sinkRecorder{}
	if _, err := parsePrintStream(context.Background(), strings.NewReader(stream), true, rec.record); err != nil {
		t.Fatalf("parsePrintStream: %v", err)
	}

	ends := rec.byKind(agent.EventAgentToolEnd)
	if len(ends) != 1 {
		t.Fatalf("sink received %d ToolEnd events, want 1", len(ends))
	}
	if ends[0].Err == nil {
		t.Errorf("ToolEnd.Err = nil, want non-nil (is_error=true on tool_result)")
	}
}

// TestParsePrintStream_NilSinkStaysBackwardCompatible pins
// the "sink == nil" path: when no sink is wired (e.g. legacy
// callers or unit tests that only want the RunResult),
// parsePrintStream must still parse cleanly and not panic on
// the nil sink dereference inside the assistant / user
// branches. v10 added the sink parameter; v10.1 (this test)
// pins the nil tolerance so the change doesn't break
// unrelated call sites.
func TestParsePrintStream_NilSinkStaysBackwardCompatible(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-4-5"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"some progress"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"ok"}]}}`,
		`{"type":"result","subtype":"success","session_id":"s1","duration_ms":100,"result":"final"}`,
		"",
	}, "\n")

	got, err := parsePrintStream(context.Background(), strings.NewReader(stream), true, nil)
	if err != nil {
		t.Fatalf("parsePrintStream with nil sink: %v", err)
	}
	if got.Text != "final" {
		t.Errorf("Result.Text = %q, want %q", got.Text, "final")
	}
}

// TestReview_SinkSeesBranchField pins Fix #4: the chat
// StatusBar's "⎇ <branch>" footer segment renders from the
// Branch field on EventAgentReady / EventAgentResult. v12
// computed branch only for the argv positional and never
// stamped it onto the envelope, so the segment rendered empty
// for every /review on claude. v12.1 threads the computed
// branch through both events.
//
// Asserted via a sh mock that emits a clean result so we can
// drive runCodeReviewPrintMode end-to-end without a real
// `claude` binary.
func TestReview_SinkSeesBranchField(t *testing.T) {
	repoDir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-q", "-m", "root"},
		{"checkout", "-q", "-b", "feat-branch-field"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	mockDir := t.TempDir()
	mockPath := filepath.Join(mockDir, "fake-claude")
	if err := os.WriteFile(mockPath, []byte(`#!/bin/sh
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"ok","duration_ms":10}'
exit 0
`), 0o755); err != nil {
		t.Fatalf("write mock: %v", err)
	}

	var events []agent.AgentEvent
	sink := func(ev agent.AgentEvent) { events = append(events, ev) }

	a := NewStarter("mock-claude", mockPath, nil)
	if _, err := a.Review(context.Background(), agent.StartConfig{
		Workspace: repoDir,
	}, agent.WithEventSink(sink)); err != nil {
		t.Fatalf("Review: %v", err)
	}

	var sawReady, sawResult bool
	for _, ev := range events {
		switch ev.Kind {
		case agent.EventAgentReady:
			sawReady = true
			if ev.Branch != "feat-branch-field" {
				t.Errorf("Ready.Branch = %q, want %q", ev.Branch, "feat-branch-field")
			}
		case agent.EventAgentResult:
			sawResult = true
			if ev.Branch != "feat-branch-field" {
				t.Errorf("Result.Branch = %q, want %q", ev.Branch, "feat-branch-field")
			}
		}
	}
	if !sawReady {
		t.Errorf("sink missed EventAgentReady")
	}
	if !sawResult {
		t.Errorf("sink missed EventAgentResult")
	}
}

// TestReview_SinkErrorEventCarriesDiagnostic pins Fix #2:
// outbound.translate.go:200 silently drops EventAgentError
// events whose Diagnostic is nil, leaving the chat receipt
// card stuck on 🔄 on /review failure. v12 emitted the error
// without Diagnostic; v12.1 always populates it via
// claudeDiagnostic. We trigger the error path by pointing
// Starter at a non-existent binary (runPrintModeWithPrompt's
// proc.New fails → returns error → runCodeReviewPrintMode
// emits EventAgentError with Diagnostic).
func TestReview_SinkErrorEventCarriesDiagnostic(t *testing.T) {
	var events []agent.AgentEvent
	sink := func(ev agent.AgentEvent) { events = append(events, ev) }

	// Throwaway repo so the workspace-pre-flight check passes;
	// the failure we want to test comes from proc.New failing
	// on the non-existent mock binary.
	repoDir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-q", "-m", "root"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	a := NewStarter("mock-claude", "/this/binary/does/not/exist", nil)
	_, err := a.Review(context.Background(), agent.StartConfig{
		Workspace: repoDir,
	}, agent.WithEventSink(sink))
	if err == nil {
		t.Fatalf("expected spawn error, got nil")
	}

	var sawError bool
	for _, ev := range events {
		if ev.Kind != agent.EventAgentError {
			continue
		}
		sawError = true
		if ev.Diagnostic == nil {
			t.Errorf("EventAgentError.Diagnostic = nil (translate.go silently drops these)")
		}
		if ev.Diagnostic.AgentName != "claudecode" {
			t.Errorf("Diagnostic.AgentName = %q, want %q", ev.Diagnostic.AgentName, "claudecode")
		}
	}
	if !sawError {
		t.Errorf("sink missed EventAgentError; got events: %+v", events)
	}
}
