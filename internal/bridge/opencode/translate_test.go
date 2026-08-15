// Tests for the opencode bridge's SSE → AgentEvent translator,
// focused on the buffering + inline-think-strip path introduced
// by the opencode-stream-buffer fix.
//
// Coverage:
//
//   - text.started → many .delta → .ended:
//
//       (a) the chat surface sees ONE EventAgentText after
//           flush, not N (one per token); guard against the
//           regression that produced the per-word refresh
//           symptom on production.
//
//   - inline <think>...</think> inside text.delta:
//
//       (b) tags do not leak into OutReply (covered by
//           think_tags_test.go splitThinking; this file adds
//           the end-to-end translator check via the [思考]
//           prefix landed in handleTextStreamDelta).
//
//   - thinking-only turn:
//
//       (c) the reply EventAgentText at terminal is empty
//           when only reasoning occurred. The reasoning surfaces
//           on its own [思考] surface and never bleeds into the
//           OutReply.
//
//   - tool boundary:
//
//       (d) pendingText flushes on handleToolPart "pending"
//           transition — pre-tool reply text reaches the chat
//           client before the tool receipt.
//
//   - ghost-Done suppression:
//
//       (e) a terminal event with neither content nor step on
//           this turn does NOT deliver EventAgentDone (the
//           "spawn window replay" symptom that produced the
//           "Done 立即触发" complaint).
package opencode

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── test harness ─────────────────────────────────────────────────

// capturingDeliver returns every event the translator delivers so
// tests can assert on the produced slice. The deliver callback
// contract preserves the original ordering because all calls land
// here on the same goroutine — the SSE reader goroutine — and
// this is the only path that fires during a test.
func capturingDeliver() (func(agent.AgentEvent) agent.AgentEvent, *[]agent.AgentEvent, *sync.Mutex) {
	var (
		mu  sync.Mutex
		out []agent.AgentEvent
	)
	deliver := func(ev agent.AgentEvent) agent.AgentEvent {
		mu.Lock()
		out = append(out, ev)
		mu.Unlock()
		return ev
	}
	return deliver, &out, &mu
}

// newTestTranslator returns a translator wired to a capturing
// deliver. The sessionID / model / agentName / workspace fields
// are populated with sentinel values so tests can assert on them
// when relevant.
func newTestTranslator() (*translator, func() []agent.AgentEvent) {
	deliver, sink, mu := capturingDeliver()
	tr := newTranslator(deliver, "opencode-test", "/tmp", "main", "ses_test", "test-model")
	return tr, func() []agent.AgentEvent {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]agent.AgentEvent, len(*sink))
		copy(cp, *sink)
		return cp
	}
}

// sseTextEvent builds a `session.next.text.*` SessionEvent with
// the opencode 1.18 wire shape: `type` is the full event name
// the bridge dispatches on (translator.handleEvent keys the
// switch on the full string — see translate.go's case statement),
// properties carries partID + the appropriate payload field for
// the sub-type.
func sseTextEvent(sub, partID, payload string) SessionEvent {
	var raw string
	switch sub {
	case "started":
		raw = `{"type":"session.next.text.` + sub + `","properties":{` +
			`"partID":` + jsonString(partID) +
			`}}`
	case "delta":
		raw = `{"type":"session.next.text.` + sub + `","properties":{` +
			`"partID":` + jsonString(partID) +
			`,"delta":` + jsonString(payload) +
			`}}`
	case "ended":
		raw = `{"type":"session.next.text.` + sub + `","properties":{` +
			`"partID":` + jsonString(partID) +
			`,"text":` + jsonString(payload) +
			`}}`
	}
	var ev SessionEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		panic("test sseTextEvent bad json: " + err.Error())
	}
	return ev
}

func sseTerminalEvent(fullType string) SessionEvent {
	raw := `{"type":` + jsonString(fullType) + `,"properties":{}}`
	var ev SessionEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		panic("test sseTerminalEvent bad json: " + err.Error())
	}
	return ev
}

// jsonString quotes s with the escapes required for an ASCII
// test payload. Mirrors pi's testJSONString.
func jsonString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// textEvents returns the subset of events whose Kind is
// EventAgentText. Filters out the [思考] entries so reply-only
// assertions can be written without a manual loop.
func textEvents(events []agent.AgentEvent) []agent.AgentEvent {
	var out []agent.AgentEvent
	for _, ev := range events {
		if ev.Kind != agent.EventAgentText {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// textTexts returns the .Text of every EventAgentText, in the
// order they were delivered.
func textTexts(events []agent.AgentEvent) []string {
	var out []string
	for _, ev := range events {
		if ev.Kind == agent.EventAgentText {
			out = append(out, ev.Text)
		}
	}
	return out
}

// ─── (a) one text block emits one EventAgentText ──────────────────

// TestTranslate_TextStreamBuffersUntilTerminal is the regression
// test for the per-word refresh symptom: many deltas + one
// ended + a terminal event must surface ONE EventAgentText with
// the joined text, not one per delta.
func TestTranslate_TextStreamBuffersUntilTerminal(t *testing.T) {
	tr, snap := newTestTranslator()

	deltas := []string{"Now ", "let me check ", "the print.go ", "files to see ", "how each ", "bridge sets cmd.Dir :"}
	for _, d := range deltas {
		if err := tr.handleEvent(sseTextEvent("delta", "p1", d)); err != nil {
			t.Fatalf("handleEvent(delta): %v", err)
		}
	}
	if err := tr.handleEvent(sseTextEvent("ended", "p1", "")); err != nil {
		t.Fatalf("handleEvent(ended): %v", err)
	}
	// Before the terminal event, NOTHING should have surfaced on
	// the reply surface — the buffer holds the text.
	mid := textEvents(snap())
	if len(mid) != 0 {
		t.Fatalf("mid-stream reply events = %d, want 0; got %v", len(mid), textTexts(mid))
	}
	// Now fire the terminal event. The buffered text should
	// land on the chat surface as ONE EventAgentText.
	if err := tr.handleEvent(sseTerminalEvent("session.next.step.ended")); err != nil {
		t.Fatalf("handleEvent(terminal): %v", err)
	}
	got := textEvents(snap())
	if len(got) != 1 {
		t.Fatalf("after terminal reply events = %d, want 1; got %v", len(got), textTexts(got))
	}
	want := strings.Join(deltas, "")
	if got[0].Text != want {
		t.Errorf("reply text = %q, want %q", got[0].Text, want)
	}
	// And the chat surface never sees partial prefixes that
	// would correspond to one-render-per-token.
	for _, ev := range textEvents(snap()) {
		if ev.Text != want {
			t.Errorf("unexpected duplicate or partial reply render: %q", ev.Text)
		}
	}
}

// ─── (b) inline think block stripped into [思考] ───────────────────

// TestTranslate_TextDeltaInlineThinkStripped exercises the
// end-to-end path through the splitter: a delta whose payload
// has <think>...</think> around "secret plan" must surface the
// reasoning as its own [思考] EventAgentText and leave the
// reply buffer holding only the surrounding plain text.
func TestTranslate_TextDeltaInlineThinkStripped(t *testing.T) {
	tr, snap := newTestTranslator()

	_ = tr.handleEvent(sseTextEvent("started", "p1", ""))
	_ = tr.handleEvent(sseTextEvent("delta", "p1", "Hello "))
	if err := tr.handleEvent(sseTextEvent("delta", "p1", "<think>secret plan</think> world")); err != nil {
		t.Fatalf("handleEvent(delta): %v", err)
	}
	_ = tr.handleEvent(sseTextEvent("ended", "p1", ""))

	// Terminal event flushes the buffered reply text.
	_ = tr.handleEvent(sseTerminalEvent("session.next.step.ended"))

	var replyTexts, thinkTexts []string
	for _, ev := range textEvents(snap()) {
		if strings.HasPrefix(ev.Text, "[思考] ") {
			thinkTexts = append(thinkTexts, strings.TrimPrefix(ev.Text, "[思考] "))
		} else {
			replyTexts = append(replyTexts, ev.Text)
		}
	}

	if len(replyTexts) != 1 {
		t.Fatalf("reply = %v, want exactly 1", replyTexts)
	}
	if replyTexts[0] != "Hello  world" {
		t.Errorf("reply = %q, want %q (tags must not leak)", replyTexts[0], "Hello  world")
	}
	if strings.Contains(replyTexts[0], "<think>") || strings.Contains(replyTexts[0], "</think>") {
		t.Errorf("reply %q contains inline think tags", replyTexts[0])
	}
	if len(thinkTexts) != 1 || thinkTexts[0] != "secret plan" {
		t.Errorf("think = %v, want exactly [%q]", thinkTexts, "secret plan")
	}
}

// ─── (c) thinking-only turn does not leak into OutReply ───────────

// TestTranslate_ThinkingOnlyTurnNoReply mirrors pi's
// TestTranslate_TextDeltaOnlyThinkingLockedAway invariant for
// the opencode streaming path. A turn whose only content is
// inline <think> must NOT produce a reply render that contains
// the reasoning text.
func TestTranslate_ThinkingOnlyTurnNoReply(t *testing.T) {
	tr, snap := newTestTranslator()

	_ = tr.handleEvent(sseTextEvent("started", "p1", ""))
	if err := tr.handleEvent(sseTextEvent("delta", "p1", "<think>secret plan</think>")); err != nil {
		t.Fatalf("handleEvent(delta): %v", err)
	}
	_ = tr.handleEvent(sseTextEvent("ended", "p1", ""))
	_ = tr.handleEvent(sseTerminalEvent("session.next.step.ended"))

	for _, ev := range textEvents(snap()) {
		if ev.Kind != agent.EventAgentText {
			continue
		}
		if strings.Contains(ev.Text, "secret plan") && !strings.HasPrefix(ev.Text, "[思考] ") {
			t.Errorf("reply %q leaked reasoning text", ev.Text)
		}
	}
}

// ─── (d) tool-boundary flush ──────────────────────────────────────

// TestTranslate_ToolBoundaryFlushesBufferedReply confirms the
// pre-tool reply segment becomes a reply render before the tool
// start receipt. The test emits a message.part.updated for the
// tool (the conventional 1.17 path) right after the text.ended
// — the 1.18 streaming path handles the same boundary via
// session.next.tool.* but the buffering logic is identical.
func TestTranslate_ToolBoundaryFlushesBufferedReply(t *testing.T) {
	tr, snap := newTestTranslator()

	// Closed text block.
	_ = tr.handleEvent(sseTextEvent("started", "p1", ""))
	if err := tr.handleEvent(sseTextEvent("delta", "p1", "Let me check the files.")); err != nil {
		t.Fatalf("handleEvent(delta): %v", err)
	}
	_ = tr.handleEvent(sseTextEvent("ended", "p1", ""))

	// Mid-turn tool: the bridge must flush pendingText BEFORE
	// emitting EventAgentToolStart, so the chat client renders
	// the pre-tool text before the tool receipt.
	toolPart := `{"type":"tool","callID":"c1","tool":"bash","state":{"status":"pending","input":{"command":"ls"}}}`
	var ev SessionEvent
	raw := `{"type":"message.part.updated","properties":{"part":` + toolPart + `}}`
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("bad part json: %v", err)
	}
	if err := tr.handleEvent(ev); err != nil {
		t.Fatalf("handleEvent(tool pending): %v", err)
	}

	// Reply render must have come first; the tool start must
	// follow it in order.
	events := snap()
	var foundReply, foundTool bool
	for _, ev := range events {
		switch ev.Kind {
		case agent.EventAgentText:
			if ev.Text == "Let me check the files." {
				foundReply = true
				if foundTool {
					t.Errorf("tool start surfaced before reply render; ordering wrong")
				}
			}
		case agent.EventAgentToolStart:
			foundTool = true
		}
	}
	if !foundReply {
		t.Errorf("reply render not found in events; got %v", textTexts(events))
	}
	if !foundTool {
		t.Errorf("tool start event not found; got %v", textTexts(events))
	}
}

// ─── (e) terminal-event Done semantics ────────────────────────────

// TestTranslate_TerminalWithoutSignalEmitsEmptyDone verifies
// that the translator still emits EventAgentDone on a terminal
// event (session.idle) even when no content / step / buffered
// reply preceded it — the empty turn detector tags it
// Reason:"empty" so the runtime can surface a "(empty response)"
// hint, but the Done itself must always fire (otherwise the AS
// readpump's busy guard never clears on idle-replay paths).
//
// The "ghost Done" complaint in the opencode-stream-buffer
// investigation was about per-word refresh on the reply surface,
// NOT about Done prematurely clearing the busy guard — Done
// suppression is therefore deliberately NOT applied here. The
// opencode wires session.idle / session.next.step.ended as the
// authoritative turn-end signal, and the runtime treats them as
// such. The buffering layer (P0) and inline-think strip (P1)
// handle the visible symptoms; Done is left untouched.
func TestTranslate_TerminalWithoutSignalEmitsEmptyDone(t *testing.T) {
	tr, snap := newTestTranslator()

	if err := tr.handleEvent(sseTerminalEvent("session.idle")); err != nil {
		t.Fatalf("handleEvent(idle): %v", err)
	}

	var done *agent.AgentDoneEvent
	for _, ev := range snap() {
		if ev.Done != nil {
			done = ev.Done
		}
	}
	if done == nil {
		t.Fatalf("EventAgentDone not delivered on idle with no signal")
	}
	if done.Reason != "empty" {
		t.Errorf("Done.Reason = %q, want %q", done.Reason, "empty")
	}
}

// TestTranslate_TerminalWithSignalEmitsDone is the inverse — a
// step.started before the terminal still counts as a real turn
// and earns a Done.Reason = "settled" rather than "empty". This
// is the regression guard for the empty-vs-settled distinction.
func TestTranslate_TerminalWithSignalEmitsDone(t *testing.T) {
	tr, snap := newTestTranslator()

	if err := tr.handleEvent(sseTerminalEvent("session.next.step.started")); err != nil {
		t.Fatalf("handleEvent(step.started): %v", err)
	}
	if err := tr.handleEvent(sseTerminalEvent("session.next.step.ended")); err != nil {
		t.Fatalf("handleEvent(step.ended): %v", err)
	}

	var done *agent.AgentDoneEvent
	for _, ev := range snap() {
		if ev.Done != nil {
			done = ev.Done
		}
	}
	if done == nil {
		t.Fatalf("EventAgentDone not delivered despite step.started signal; events = %v", textTexts(snap()))
	}
	if done.Reason != "settled" {
		t.Errorf("Done.Reason = %q, want %q", done.Reason, "settled")
	}
}

// ─── bridge-state tests ───────────────────────────────────────────

// TestTranslate_ResetTurnClearsBuffer checks the per-turn reset
// boundary: after ResetTurn, a new turn starts with a fresh
// textBuf, an empty pendingText, and no thinkHoldings. If the
// reset is partial (a future refactor flips Reset to in-place
// clear instead of newTurnState), this test catches it without
// needing a wire-log inspection.
func TestTranslate_ResetTurnClearsBuffer(t *testing.T) {
	tr, _ := newTestTranslator()

	// Seed turn 1 with a partially-open think block: the
	// subsequent ResetTurn must wipe it. The bridge does NOT
	// need to fire the terminal event for the reset semantics
	// to apply — ResetTurn is unconditional.
	tr.turnMu.Lock()
	tr.turn.thinkHoldings["p_seed"] = "<think>leftover"
	tr.turn.textBuf["p_seed"] = &strings.Builder{}
	tr.turn.textBuf["p_seed"].WriteString("leftover")
	tr.turn.pendingText.WriteString("leftover")
	tr.turn.activeTextBlock = "p_seed"
	tr.turnMu.Unlock()

	tr.ResetTurn()

	tr.turnMu.Lock()
	defer tr.turnMu.Unlock()
	if len(tr.turn.thinkHoldings) != 0 {
		t.Errorf("post-ResetTurn thinkHoldings=%d entries, want 0", len(tr.turn.thinkHoldings))
	}
	if len(tr.turn.textBuf) != 0 {
		t.Errorf("post-ResetTurn textBuf=%d entries, want 0", len(tr.turn.textBuf))
	}
	if tr.turn.pendingText.Len() != 0 {
		t.Errorf("post-ResetTurn pendingText=%d bytes, want 0", tr.turn.pendingText.Len())
	}
	if tr.turn.activeTextBlock != "" {
		t.Errorf("post-ResetTurn activeTextBlock=%q, want empty", tr.turn.activeTextBlock)
	}
}
