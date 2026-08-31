
// Tests for the AskUserQuestion dual-path interception fix
// (2026-08 fix-claude-ask-your-question). See docs/bridge/claude.md
// §5.5 for the full design rationale.
//
// Coverage:
//
//   - armPendingAsk: the response channel gets stored in pendingAsk
//     AND the interceptor goroutine rewrites the emitted
//     EventAgentPermission's ResponseCh to point at our channel
//     (so the channel layer writes to it).
//
//   - SendPermission with TextFallback=true → writeUserText:
//     a plain user-role text message lands on stdin (no
//     tool_use_id, no tool_result). This is the (b) text-fallback
//     path that pre-fix was silently dropping — every user click
//     returned "no pending AskUserQuestion" because the respCh
//     was orphaned.
//
//   - SendPermission with TextFallback=false → tool_result: the
//     existing (a) tool_use path remains unchanged. Pins the
//     regression so a future refactor can't accidentally route
//     the tool_use path through writeUserText.
//
//   - SendPermission with nil pendingAsk → "no pending" error:
//     pre-fix behavior, still correct.
//
//   - pumpStream text-fallback end-to-end: a fixture whose
//     assistant text contains the ask pattern → driver arms
//     pendingAsk with TextFallback=true → EventAgentPermission
//     is emitted with a real ResponseCh.

package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── helpers ───

// newStdinPipeDriver wires a minimal *driver with a writable stdin
// pipe. The returned writeCloser is what the driver writes to; the
// returned readCloser is what the test reads from to capture the
// JSON line(s) the driver emits.
//
// Mirrors newDriverForStdinTest in agent_interrupt_unix_test.go but
// doesn't need a real *exec.Cmd — only stdin plumbing is exercised
// by these tests.
func newStdinPipeDriver() (d *driver, w *os.File, r *os.File) {
	pr, pw, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	d = &driver{
		closed: make(chan struct{}),
	}
	d.stdin = bufio.NewWriter(pw)
	return d, pw, pr
}

// readStdinLine reads one LF-delimited line from r. Uses a short
// timeout so a missing write doesn't hang the test forever.
func readStdinLine(t *testing.T, r io.Reader) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 4096)
		n, err := r.Read(buf)
		if n > 0 {
			// strip trailing LF if present
			if buf[n-1] == '\n' {
				ch <- result{string(buf[:n-1]), err}
				return
			}
			ch <- result{string(buf[:n]), err}
			return
		}
		ch <- result{"", err}
	}()
	select {
	case res := <-ch:
		if res.err != nil && res.err != io.EOF {
			t.Fatalf("read stdin: %v", res.err)
		}
		return res.line
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for driver to write to stdin")
		return ""
	}
}

// ─── armPendingAsk ───

// TestArmPendingAsk_RewritesResponseCh verifies that the
// interceptor goroutine launched by armPendingAsk swaps the
// EventAgentPermission.Permission.ResponseCh of any event that
// passes through armedEvents, so the channel layer writes to the
// channel we stored in pendingAsk — never to whatever default
// ResponseCh the emitter originally allocated.
//
// We don't have direct access to the response channel armPendingAsk
// stored (it's an implementation detail now — callers don't need it).
// Instead we verify two invariants: (1) the emitted event's
// ResponseCh is NOT the channel the emitter allocated (proving
// rewrite happened) and (2) the emitted ResponseCh IS the same
// channel that pendingAsk holds (proving it points at our channel,
// not some third one).
func TestArmPendingAsk_RewritesResponseCh(t *testing.T) {
	d := &driver{
		closed: make(chan struct{}),
		events: make(chan agent.AgentEvent, 4),
	}

	armedEvents, done := d.armPendingAsk("toolu_test", false, false)
	if armedEvents == nil {
		t.Fatal("armedEvents is nil")
	}
	if done == nil {
		t.Fatal("done is nil")
	}

	// armPendingAsk stored its channel in pendingAsk; capture
	// it for the post-rewrite assertion.
	d.pendingMu.Lock()
	wantCh := d.pendingAsk.ResponseCh
	d.pendingMu.Unlock()
	if wantCh == nil {
		t.Fatal("pendingAsk.ResponseCh is nil — armPendingAsk didn't store it")
	}

	// Emit a fake EventAgentPermission whose ResponseCh is a
	// DIFFERENT channel than ours — pre-fix, this would be the
	// orphan problem (defaultAskHandler creates its own
	// make(chan string, 1) inside ask.go).
	otherCh := make(chan string, 1)
	armedEvents <- agent.AgentEvent{
		Kind: agent.EventAgentPermission,
		Permission: &agent.AgentPermissionRequest{
			Tool:       "AskUserQuestion",
			Action:     "[x] y?",
			Options:    []string{"A", "B"},
			ResponseCh: otherCh,
		},
	}
	close(armedEvents)
	<-done

	// The event should have landed on d.events with ResponseCh
	// rewritten to the channel we stored in pendingAsk.
	var ev agent.AgentEvent
	select {
	case ev = <-d.events:
	case <-time.After(time.Second):
		t.Fatal("no event arrived on d.events")
	}
	if ev.Kind != agent.EventAgentPermission {
		t.Fatalf("event kind = %v, want EventAgentPermission", ev.Kind)
	}
	if ev.Permission.ResponseCh == otherCh {
		t.Errorf("ResponseCh still points at the orphan channel (rewrite did not fire)")
	}
	if ev.Permission.ResponseCh != wantCh {
		t.Errorf("ResponseCh not rewritten to pendingAsk.ResponseCh: got %p, want %p",
			ev.Permission.ResponseCh, wantCh)
	}
}

// TestArmPendingAsk_PassesThroughNonPermissionEvents verifies the
// interceptor doesn't drop events whose Kind != EventAgentPermission
// (e.g. EventAgentText, EventAgentDone). The (b) text-fallback path
// relies on this for any non-permission events the emitter produces.
func TestArmPendingAsk_PassesThroughNonPermissionEvents(t *testing.T) {
	d := &driver{
		closed: make(chan struct{}),
		events: make(chan agent.AgentEvent, 4),
	}

	armedEvents, done := d.armPendingAsk("", false, true)

	armedEvents <- agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: "synthetic text event",
	}
	close(armedEvents)
	<-done

	var ev agent.AgentEvent
	select {
	case ev = <-d.events:
	case <-time.After(time.Second):
		t.Fatal("no event arrived on d.events")
	}
	if ev.Kind != agent.EventAgentText {
		t.Fatalf("event kind = %v, want EventAgentText", ev.Kind)
	}
	if ev.Text != "synthetic text event" {
		t.Errorf("text = %q, want %q", ev.Text, "synthetic text event")
	}
}

// ─── SendPermission: text-fallback path ───

// TestSendPermission_TextFallback_WritesUserText is the regression
// test for the 2026-08 fix: when pendingAsk.TextFallback=true,
// SendPermission writes a plain user-role text message (no
// tool_result, no tool_use_id). Pre-fix this branch didn't exist
// at all — text-fallback path's respCh was orphaned, and every
// user click on the text-fallback card returned "no pending
// AskUserQuestion" without ever writing to stdin.
func TestSendPermission_TextFallback_WritesUserText(t *testing.T) {
	d, w, r := newStdinPipeDriver()
	defer w.Close()
	defer r.Close()

	// Pre-arm pendingAsk with TextFallback=true (what the (b)
	// text-fallback path does via pumpStream's armPendingAskFn).
	d.pendingMu.Lock()
	d.pendingAsk = &pendingAsk{
		ToolUseID:    "",
		Multi:        false,
		ResponseCh:   make(chan string, 1),
		TextFallback: true,
	}
	d.pendingMu.Unlock()

	if err := d.SendPermission("完全同步对齐 (git reset --hard origin/main)"); err != nil {
		t.Fatalf("SendPermission: %v", err)
	}

	line := readStdinLine(t, r)
	if line == "" {
		t.Fatal("driver wrote nothing to stdin")
	}

	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		t.Fatalf("unmarshal: %v\nline: %s", err, line)
	}

	if msg.Type != "user" {
		t.Errorf("type = %q, want 'user'", msg.Type)
	}
	if msg.Message.Role != "user" {
		t.Errorf("role = %q, want 'user'", msg.Message.Role)
	}
	if len(msg.Message.Content) != 1 {
		t.Fatalf("content count = %d, want 1", len(msg.Message.Content))
	}
	if msg.Message.Content[0].Type != "text" {
		t.Errorf("content[0].type = %q, want 'text' (NOT tool_result)", msg.Message.Content[0].Type)
	}
	if msg.Message.Content[0].Text != "完全同步对齐 (git reset --hard origin/main)" {
		t.Errorf("content[0].text = %q, want %q",
			msg.Message.Content[0].Text,
			"完全同步对齐 (git reset --hard origin/main)")
	}
}

// ─── SendPermission: tool_use path (regression) ───

// TestSendPermission_ToolUse_WritesToolResult pins the (a) path:
// TextFallback=false still produces a tool_result content block
// referencing the tool_use_id. Future refactors that accidentally
// route through writeUserText will fail this test.
func TestSendPermission_ToolUse_WritesToolResult(t *testing.T) {
	d, w, r := newStdinPipeDriver()
	defer w.Close()
	defer r.Close()

	d.pendingMu.Lock()
	d.pendingAsk = &pendingAsk{
		ToolUseID:    "toolu_001",
		Multi:        false,
		ResponseCh:   make(chan string, 1),
		TextFallback: false,
	}
	d.pendingMu.Unlock()

	if err := d.SendPermission("PostgreSQL"); err != nil {
		t.Fatalf("SendPermission: %v", err)
	}

	line := readStdinLine(t, r)
	if line == "" {
		t.Fatal("driver wrote nothing to stdin")
	}

	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Content   any  `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		t.Fatalf("unmarshal: %v\nline: %s", err, line)
	}

	if len(msg.Message.Content) != 1 {
		t.Fatalf("content count = %d, want 1", len(msg.Message.Content))
	}
	if msg.Message.Content[0].Type != "tool_result" {
		t.Errorf("content[0].type = %q, want 'tool_result'", msg.Message.Content[0].Type)
	}
	if msg.Message.Content[0].ToolUseID != "toolu_001" {
		t.Errorf("tool_use_id = %q, want 'toolu_001'", msg.Message.Content[0].ToolUseID)
	}
	if msg.Message.Content[0].Content != "PostgreSQL" {
		t.Errorf("content = %v, want 'PostgreSQL'", msg.Message.Content[0].Content)
	}
}

// ─── SendPermission: nil pendingAsk ───

func TestSendPermission_NoPending_ReturnsError(t *testing.T) {
	d := &driver{closed: make(chan struct{})}
	err := d.SendPermission("anything")
	if err == nil {
		t.Fatal("expected error when pendingAsk is nil, got nil")
	}
	if !strings.Contains(err.Error(), "no pending AskUserQuestion") {
		t.Errorf("err = %q, want one containing 'no pending AskUserQuestion'", err)
	}
}

// ─── pumpStream text-fallback end-to-end ───

// TestPumpStream_TextFallback_DetectsAndArms exercises the (b)
// path end-to-end: an assistant text block matching the ask
// pattern (markdown table + "please pick one") should be detected
// by detectAskInText, routed through armPendingAskFn with
// TextFallback=true, and emitted as an EventAgentPermission whose
// ResponseCh points at the armPendingAskFn-allocated channel —
// NOT at an orphan.
func TestPumpStream_TextFallback_DetectsAndArms(t *testing.T) {
	// Build a fixture: one assistant message whose text block is
	// the canonical markdown-table ask pattern. Real-world this
	// is what claude emits when AskUserQuestion isn't exposed
	// as a tool_use in the current environment.
	input := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"AskUserQuestion 不可用, please pick one:\n\n| Option | Description |\n|---------|-------------|\n| **PostgreSQL** | Production-ready |\n| **SQLite**      | Dev / small project |\n\nWhich database?"}]}}` + "\n"

	// armPendingAsk's interceptor goroutine writes to d.events
	// (the driver's main event channel). pumpStream is invoked
	// with the SAME channel in production, so we mirror that here.
	events := make(chan agent.AgentEvent, 4)
	d := &driver{
		closed: make(chan struct{}),
		events: events,
	}

	var armFn armPendingAskFn = d.armPendingAsk

	// askHandler is intentionally nil: the text-fallback path
	// uses armPendingAskFn, not askHandler. (The tool_use path
	// is what requires a non-nil askHandler.) This test pins
	// that armPendingAskFn alone is sufficient.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pumpStream(strings.NewReader(input), events, nil, armFn, "claude", "/tmp", "main", nil)
		close(events)
	}()

	var evs []agent.AgentEvent
	for ev := range events {
		evs = append(evs, ev)
	}
	wg.Wait()

	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Kind != agent.EventAgentPermission {
		t.Fatalf("event kind = %v, want EventAgentPermission", ev.Kind)
	}
	if ev.Permission == nil {
		t.Fatal("permission is nil")
	}
	if ev.Permission.Tool != "AskUserQuestion" {
		t.Errorf("tool = %q, want 'AskUserQuestion'", ev.Permission.Tool)
	}
	if ev.Permission.ResponseCh == nil {
		t.Fatal("ResponseCh is nil — interceptor didn't swap it (orphan!)")
	}

	// The driver.pendingAsk should be populated with
	// TextFallback=true.
	d.pendingMu.Lock()
	ask := d.pendingAsk
	d.pendingMu.Unlock()
	if ask == nil {
		t.Fatal("pendingAsk is nil — armPendingAsk didn't store it")
	}
	if !ask.TextFallback {
		t.Errorf("pendingAsk.TextFallback = false, want true (text-fallback path)")
	}
	if ask.ToolUseID != "" {
		t.Errorf("pendingAsk.ToolUseID = %q, want empty (no tool_use_id on text-fallback path)", ask.ToolUseID)
	}

	// SendPermission must now write a user-role text message
	// (not a tool_result). Wire the driver up with a real stdin
	// pipe to verify.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()
	defer pr.Close()
	d.stdin = bufio.NewWriter(pw)

	if err := d.SendPermission("SQLite"); err != nil {
		t.Fatalf("SendPermission after text-fallback: %v", err)
	}

	line := readStdinLine(t, pr)
	if line == "" {
		t.Fatal("driver wrote nothing to stdin")
	}
	if !bytes.Contains([]byte(line), []byte(`"type":"text"`)) {
		t.Errorf("expected user-role text in stdin payload, got: %s", line)
	}
	if bytes.Contains([]byte(line), []byte(`"tool_result"`)) {
		t.Errorf("text-fallback path leaked a tool_result: %s", line)
	}
	if bytes.Contains([]byte(line), []byte(`"tool_use_id"`)) {
		t.Errorf("text-fallback path leaked a tool_use_id: %s", line)
	}
}
