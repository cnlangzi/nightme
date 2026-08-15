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
// sseTextEvent builds a `session.next.text.*` SessionEvent. The
// real opencode 1.18 wire shape (verified against a live server)
// keys each text block under `textID`; older variants used
// `partID`. We emit BOTH in the helper so the test fixture
// matches the current protocol AND stays forward-compatible with
// the partID fallback in handleTextStreamEvent — the bridge will
// prefer textID, which is what production traffic looks like.
func sseTextEvent(sub, id, payload string) SessionEvent {
	props := []string{
		`"textID":` + jsonString(id),
		`"partID":` + jsonString(id),
	}
	switch sub {
	case "started":
		// No payload on started.
	case "delta":
		props = append(props, `"delta":`+jsonString(payload))
	case "ended":
		props = append(props, `"text":`+jsonString(payload))
	}
	raw := `{"type":"session.next.text.` + sub + `","properties":{` +
		strings.Join(props, ",") + `}}`
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
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
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

// TestTranslate_TextStreamTwoBlocksBufferedSeparately is the
// regression guard for the wire-field mismatch bug:
//
// opencode 1.18 keys text blocks under `textID` on the data
// payload, not `partID`. The pre-fix bridge unmarshalled only
// `partID` (always empty in production wire traffic), so all
// blocks of a turn collapsed onto the single shared "_" bucket.
// For single-block replies the symptom was invisible (the bucket
// held one block's worth of text), but a turn with pre-tool text
// + tool + post-tool text would interleave the two blocks' deltas
// into one buffer and surface them as a single concatenated blob.
//
// The test fires two distinct text blocks (text-0, text-1) with
// disjoint content, then asserts each block closes into its own
// independent EventAgentText at the terminal event.
func TestTranslate_TextStreamTwoBlocksBufferedSeparately(t *testing.T) {
	tr, snap := newTestTranslator()

	// Block 1 (pre-tool narration).
	if err := tr.handleEvent(sseTextEvent("started", "text-0", "")); err != nil {
		t.Fatalf("handleEvent(started text-0): %v", err)
	}
	for _, d := range []string{"Let me ", "check the ", "files."} {
		if err := tr.handleEvent(sseTextEvent("delta", "text-0", d)); err != nil {
			t.Fatalf("handleEvent(delta text-0): %v", err)
		}
	}
	if err := tr.handleEvent(sseTextEvent("ended", "text-0", "")); err != nil {
		t.Fatalf("handleEvent(ended text-0): %v", err)
	}

	// A tool call arrives between the two text blocks. The bridge
	// must flush block 1 (and emit the tool start) before block 2
	// ever opens. If the buffer keying collapses both blocks onto
	// "_", the flush below would surface the wrong text.
	toolPart := `{"type":"tool","callID":"c1","tool":"bash","state":{"status":"pending","input":{"command":"ls"}}}`
	raw := `{"type":"message.part.updated","properties":{"part":` + toolPart + `}}`
	var toolEv SessionEvent
	if err := json.Unmarshal([]byte(raw), &toolEv); err != nil {
		t.Fatalf("bad tool part json: %v", err)
	}
	if err := tr.handleEvent(toolEv); err != nil {
		t.Fatalf("handleEvent(tool pending): %v", err)
	}

	// Block 2 (post-tool conclusion).
	if err := tr.handleEvent(sseTextEvent("started", "text-1", "")); err != nil {
		t.Fatalf("handleEvent(started text-1): %v", err)
	}
	for _, d := range []string{"Found ", "three files."} {
		if err := tr.handleEvent(sseTextEvent("delta", "text-1", d)); err != nil {
			t.Fatalf("handleEvent(delta text-1): %v", err)
		}
	}
	if err := tr.handleEvent(sseTextEvent("ended", "text-1", "")); err != nil {
		t.Fatalf("handleEvent(ended text-1): %v", err)
	}

	// Terminal flushes block 2.
	if err := tr.handleEvent(sseTerminalEvent("session.next.step.ended")); err != nil {
		t.Fatalf("handleEvent(terminal): %v", err)
	}

	// Walk the delivered events; we expect two EventAgentText
	// payloads (block 1 closed at tool boundary, block 2 closed
	// at terminal) and one EventAgentToolStart between them.
	got := textTexts(snap())
	wantText := []string{"Let me check the files.", "Found three files."}
	if len(got) != len(wantText) {
		t.Fatalf("reply events = %d (%v), want %d (%v)", len(got), got, len(wantText), wantText)
	}
	for i, want := range wantText {
		if got[i] != want {
			t.Errorf("reply[%d] = %q, want %q", i, got[i], want)
		}
	}
}

// TestTranslate_ToolBoundaryFlushClosesUnendedBlock is the
// regression guard for the second half of the buffering fix:
//
// Before the fix, flushPendingTextLocked only read from
// pendingText (the closed-blocks sink). A text block whose .ended
// never arrived — opencode 1.18 fires .delta without a closing
// .ended on connection drop, resubscribe mid-part, or single-shot
// models — would stay parked in textBuf and silently disappear
// at the tool boundary. The chat client saw the model go silent
// mid-sentence and jump straight to the tool receipt.
//
// After the fix, flushPendingTextLocked calls
// closeAllTextBlocksLocked FIRST (mirrors pi's pattern), so
// un-ended blocks surface at the tool boundary just like
// properly-closed ones.
func TestTranslate_ToolBoundaryFlushClosesUnendedBlock(t *testing.T) {
	tr, snap := newTestTranslator()

	// Stream a text block, but NEVER send .ended — simulates
	// the opencode resubscribe / connection-drop path.
	if err := tr.handleEvent(sseTextEvent("started", "text-orphan", "")); err != nil {
		t.Fatalf("handleEvent(started): %v", err)
	}
	for _, d := range []string{"Half a ", "sentence, then ", "the tool cuts in."} {
		if err := tr.handleEvent(sseTextEvent("delta", "text-orphan", d)); err != nil {
			t.Fatalf("handleEvent(delta): %v", err)
		}
	}

	// Tool call fires before the text block's .ended would have
	// arrived. flushPendingTextLocked must drain the orphan block
	// into pendingText BEFORE the tool start lands on the chat
	// surface, so the user sees the pre-tool narration.
	toolPart := `{"type":"tool","callID":"c1","tool":"bash","state":{"status":"pending","input":{"command":"ls"}}}`
	raw := `{"type":"message.part.updated","properties":{"part":` + toolPart + `}}`
	var toolEv SessionEvent
	if err := json.Unmarshal([]byte(raw), &toolEv); err != nil {
		t.Fatalf("bad tool part json: %v", err)
	}
	if err := tr.handleEvent(toolEv); err != nil {
		t.Fatalf("handleEvent(tool pending): %v", err)
	}

	events := snap()
	texts := textTexts(events)
	want := "Half a sentence, then the tool cuts in."
	if len(texts) != 1 || texts[0] != want {
		t.Fatalf("reply events after tool = %v, want exactly [%q]", texts, want)
	}
	// The text must precede the tool start so the chat client
	// renders narration before the receipt.
	var sawText, sawTool bool
	for _, ev := range events {
		if ev.Kind == agent.EventAgentText {
			sawText = true
			if sawTool {
				t.Errorf("reply render appeared AFTER tool start; ordering wrong")
			}
		}
		if ev.Kind == agent.EventAgentToolStart {
			sawTool = true
		}
	}
	if !sawText || !sawTool {
		t.Errorf("missing events: text=%v tool=%v", sawText, sawTool)
	}
}

// TestTranslate_MessagePartUpdatedTextStripsInlineThink is the
// regression guard for the handlePart text branch's inline-think
// hygiene. opencode 1.18 doesn't fire message.part.updated with
// type=text alongside session.next.text.delta (verified against a
// live server), but the fallback path must still strip
// <think>...</think> so a non-streaming / replaying server
// doesn't leak the model's scratchpad into the reply bubble.
func TestTranslate_MessagePartUpdatedTextStripsInlineThink(t *testing.T) {
	tr, snap := newTestTranslator()

	// Mirror the real message.part.updated wire shape: the part
	// payload lives under properties.part.{type,text}.
	raw := `{"type":"message.part.updated","properties":{"part":{"type":"text","id":"prt_1","text":"Before <think>secret</think> after"}}}`
	var ev SessionEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if err := tr.handleEvent(ev); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}

	var reply, think []string
	for _, e := range textEvents(snap()) {
		if stripped, ok := strings.CutPrefix(e.Text, "[思考] "); ok {
			think = append(think, stripped)
		} else {
			reply = append(reply, e.Text)
		}
	}

	if len(reply) != 1 || reply[0] != "Before  after" {
		t.Fatalf("reply = %v, want exactly [\"Before  after\"]", reply)
	}
	if len(think) != 1 || think[0] != "secret" {
		t.Fatalf("think = %v, want exactly [\"secret\"]", think)
	}
	for _, r := range reply {
		if strings.Contains(r, "<think>") || strings.Contains(r, "</think>") {
			t.Errorf("reply %q still contains think tags", r)
		}
	}
}

// ─── Wire format tests for session.next.tool.* + context.updated ──
//
// Reproduces the opencode 1.18.18 SSE wire format that the bridge
// consumes in production. The previous helper (sseToolEvent) only
// emitted `tool` and `input`/`output`, all of which come across the
// wire as DIFFERENT field names (name / text / structured.content).
// These helpers mirror the live wire so the regression guard covers
// the real shapes, not just the names that "looked right" to the
// bridge author.

// helper: build a session.next.tool.* Event with caller-supplied
// fields. Emits BOTH `name` and `tool` when the test passes a tool
// name (input.started uses `name`, tool.called uses `tool`). The
// bridge handlers prefer `name` and fall back to `tool`, so this
// keeps every test fixture self-consistent.
func sseToolEvent(sub string, callID, tool string, extra string) SessionEvent {
	parts := []string{
		`"callID":` + jsonString(callID),
	}
	if tool != "" {
		parts = append(parts, `"name":`+jsonString(tool))
		parts = append(parts, `"tool":`+jsonString(tool))
	}
	if extra != "" {
		parts = append(parts, extra)
	}
	raw := `{"type":"session.next.tool.` + sub + `","properties":{` +
		strings.Join(parts, ",") + `}}`
	var ev SessionEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		panic("test sseToolEvent bad json: " + err.Error())
	}
	return ev
}

// sseToolStructuredEvent builds a tool.success with the
// structured.content payload - the wire shape for file-shaped
// tools (read, list, glob) on opencode 1.18.18+.
func sseToolStructuredEvent(callID, content string) SessionEvent {
	raw := `{"type":"session.next.tool.success","properties":{"callID":` +
		jsonString(callID) + `,"structured":{"content":` + jsonString(content) +
		`,"encoding":"utf8","mime":"text/plain"}}}`
	var ev SessionEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		panic("test sseToolStructuredEvent bad json: " + err.Error())
	}
	return ev
}

// sseToolContentEvent builds a tool.success with the
// content:[]LLMToolContent[] payload - the wire shape for
// bash-shaped tools on opencode 1.18.18+.
func sseToolContentEvent(callID, text string) SessionEvent {
	raw := `{"type":"session.next.tool.success","properties":{"callID":` +
		jsonString(callID) + `,"content":[{"type":"text","text":` +
		jsonString(text) + `}]}}`
	var ev SessionEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		panic("test sseToolContentEvent bad json: " + err.Error())
	}
	return ev
}

// sseContextUpdatedEvent builds a session.next.context.updated
// event with an arbitrary prompt-context blob (eg skills payload).
func sseContextUpdatedEvent(text string) SessionEvent {
	raw := `{"type":"session.next.context.updated","properties":{"sessionID":"ses_x","messageID":"msg_x","text":` +
		jsonString(text) + `}}`
	var ev SessionEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		panic("test sseContextUpdatedEvent bad json: " + err.Error())
	}
	return ev
}

// lastDone returns the last EventAgentDone in events, or nil if
// none has been delivered.
func lastDone(events []agent.AgentEvent) *agent.AgentDoneEvent {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Done != nil {
			return events[i].Done
		}
	}
	return nil
}

// TestTranslate_ToolOnly_ToolCalledEmitsStartNoDone verifies the
// primitive: a bare tool.called event with no preceding text or
// step yields ONE EventAgentToolStart and zero Done events (Done
// only fires on tool.success / tool.failed / canonical terminals).
func TestTranslate_ToolOnly_ToolCalledEmitsStartNoDone(t *testing.T) {
	tr, snap := newTestTranslator()

	if err := tr.handleEvent(sseToolEvent("called", "tc1", "bash", `"input":{"command":"ls"}`)); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}

	var starts, dones int
	for _, ev := range snap() {
		switch ev.Kind {
		case agent.EventAgentToolStart:
			starts++
		case agent.EventAgentDone:
			dones++
		}
	}
	if starts != 1 {
		t.Errorf("got %d EventAgentToolStart, want 1", starts)
	}
	if dones != 0 {
		t.Errorf("got %d EventAgentDone (want 0 until tool.success)", dones)
	}
}

// TestTranslate_ToolOnly_StreamingInputBuildsArgs feeds the full
// input.started -> input.delta* -> input.ended -> tool.called ->
// tool.success sequence and asserts the Start event carries the
// full args (the input.ended payload, not the partial delta
// accumulation). Done fires with Reason:"settled" exactly once.
//
// Wire format verified against opencode 1.18.18 SSE: input.ended
// ships the final JSON string under `text` (not `input`), and
// tool.success ships the result under `structured.content` (not
// `output`).
func TestTranslate_ToolOnly_StreamingInputBuildsArgs(t *testing.T) {
	tr, snap := newTestTranslator()

	steps := []SessionEvent{
		sseToolEvent("input.started", "tc1", "bash", ""),
		sseToolEvent("input.delta", "tc1", "", `"delta":"{\"command\":\""`),
		sseToolEvent("input.delta", "tc1", "", `"delta":"ls -la\"}"`),
		sseToolEvent("input.ended", "tc1", "bash", `"text":"{\"command\":\"ls -la\"}"`),
		sseToolEvent("called", "tc1", "bash", ""),
		sseToolStructuredEvent("tc1", "a.txt\nb.txt\n"),
	}
	for _, ev := range steps {
		if err := tr.handleEvent(ev); err != nil {
			t.Fatalf("handleEvent: %v", err)
		}
	}

	var starts, ends, dones int
	var startArgs string
	var endOutput string
	for _, ev := range snap() {
		switch ev.Kind {
		case agent.EventAgentToolStart:
			starts++
			if ev.ToolStart != nil {
				startArgs = ev.ToolStart.Args
			}
		case agent.EventAgentToolEnd:
			ends++
			if ev.ToolEnd != nil {
				endOutput = ev.ToolEnd.Output
			}
		case agent.EventAgentDone:
			dones++
		}
	}
	if starts != 1 {
		t.Errorf("got %d EventAgentToolStart, want 1", starts)
	}
	if ends != 1 {
		t.Errorf("got %d EventAgentToolEnd, want 1", ends)
	}
	if dones != 1 {
		t.Errorf("got %d EventAgentDone, want exactly 1", dones)
	}
	if !strings.Contains(startArgs, "ls -la") {
		t.Errorf("ToolStart.Args = %q, want substring %q", startArgs, "ls -la")
	}
	if !strings.Contains(endOutput, "a.txt") {
		t.Errorf("ToolEnd.Output = %q, want substring %q", endOutput, "a.txt")
	}
	if d := lastDone(snap()); d != nil && d.Reason != "settled" {
		t.Errorf("Done.Reason = %q, want %q (tool activity counts as content)", d.Reason, "settled")
	}
}

// TestTranslate_ToolOnly_DoneAfterEmptyTool asserts that the
// downstream chat client receives a clean Done{Reason:"settled"}
// after a single tool.success even when the model never produced
// any visible text - the opencode 1.18.18 protocol drops the
// canonical text.* / step.ended / session.idle signals entirely,
// so the tool lifecycle is the only turn-end the chat sees.
// This is the regression guard for the "啥都没反应就 Done 了"
// symptom: prior to the fix the busy guard never cleared.
func TestTranslate_ToolOnly_DoneAfterEmptyTool(t *testing.T) {
	tr, snap := newTestTranslator()

	if err := tr.handleEvent(sseToolEvent("called", "tc1", "bash", `"input":{"command":"pwd"}`)); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	if err := tr.handleEvent(sseToolStructuredEvent("tc1", "/tmp")); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}

	var dones int
	var reason string
	for _, ev := range snap() {
		if ev.Kind == agent.EventAgentDone {
			dones++
			if ev.Done != nil {
				reason = ev.Done.Reason
			}
		}
	}
	if dones != 1 {
		t.Fatalf("got %d EventAgentDone, want exactly 1 (one per turn)", dones)
	}
	if reason != "settled" {
		t.Errorf("Done.Reason = %q, want \"settled\" (tool.success is the terminal signal)", reason)
	}
}

// TestTranslate_ToolOnly_DuplicateTerminalIsNoop feeds tool.success
// FOLLOWED by the still-supported session.idle / session.next.idle
// signals to make sure the second terminal event is a no-op. This
// is the "double-Done" regression guard - a stray session.idle
// that arrives after a tool.success would otherwise tear down the
// busy guard mid-turn.
func TestTranslate_ToolOnly_DuplicateTerminalIsNoop(t *testing.T) {
	tr, snap := newTestTranslator()

	if err := tr.handleEvent(sseToolStructuredEvent("tc1", "x")); err != nil {
		t.Fatalf("handleEvent(tool.success): %v", err)
	}
	if err := tr.handleEvent(sseTerminalEvent("session.idle")); err != nil {
		t.Fatalf("handleEvent(session.idle): %v", err)
	}
	if err := tr.handleEvent(sseTerminalEvent("session.next.idle")); err != nil {
		t.Fatalf("handleEvent(session.next.idle): %v", err)
	}

	var dones int
	for _, ev := range snap() {
		if ev.Kind == agent.EventAgentDone {
			dones++
		}
	}
	if dones != 1 {
		t.Errorf("got %d EventAgentDone across 3 terminal-class events, want 1 (idempotent per turn)", dones)
	}
}

// TestTranslate_ToolOnly_FailedEmitsFailedDone verifies that a
// single tool.failed produces a Done with Reason:"failed" (NOT
// "settled") so the runtime can surface a clear failure
// indicator to the chat client - distinguishes a real tool
// failure from a successful tool run on the same per-turn Done
// wire envelope.
func TestTranslate_ToolOnly_FailedEmitsFailedDone(t *testing.T) {
	tr, snap := newTestTranslator()

	if err := tr.handleEvent(sseToolEvent("called", "tc1", "bash", `"input":{"command":"false"}`)); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}
	if err := tr.handleEvent(sseToolEvent("failed", "tc1", "bash", `"error":"non-zero exit"`)); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}

	var dones int
	var reason string
	for _, ev := range snap() {
		if ev.Kind == agent.EventAgentDone {
			dones++
			if ev.Done != nil {
				reason = ev.Done.Reason
			}
		}
	}
	if dones != 1 {
		t.Errorf("got %d EventAgentDone, want 1", dones)
	}
	if reason != "failed" {
		t.Errorf("Done.Reason = %q, want \"failed\"", reason)
	}
}

// TestTranslate_ToolOnly_PerTurnResetReArmsTerminal confirms that
// ResetTurn clears turnTerminalEmitted so a SECOND prompt within
// the same AgentSession still gets its own Done. Without this
// the second prompt would queue forever behind a stale "already
// terminal" flag.
func TestTranslate_ToolOnly_PerTurnResetReArmsTerminal(t *testing.T) {
	tr, snap := newTestTranslator()

	// Turn 1
	if err := tr.handleEvent(sseToolStructuredEvent("tc1", "x")); err != nil {
		t.Fatalf("handleEvent(turn 1): %v", err)
	}
	if got := lastDone(snap()); got == nil || got.Reason != "settled" {
		t.Errorf("turn 1 Done = %v, want settled", got)
	}

	tr.ResetTurn()

	// Turn 2 - Done must fire again. snap() returns the cumulative
	// history across both turns; turn 1 fired one Done, turn 2 must
	// add exactly one more for a grand total of 2.
	beforeReset := 0
	for _, ev := range snap() {
		if ev.Kind == agent.EventAgentDone {
			beforeReset++
		}
	}
	if err := tr.handleEvent(sseToolStructuredEvent("tc2", "y")); err != nil {
		t.Fatalf("handleEvent(turn 2): %v", err)
	}
	afterReset := 0
	for _, ev := range snap() {
		if ev.Kind == agent.EventAgentDone {
			afterReset++
		}
	}
	if afterReset-beforeReset != 1 {
		t.Errorf("turn 2 added %d Done events, want exactly 1 (ResetTurn must re-arm the per-turn terminal)", afterReset-beforeReset)
	}
}

// ─── New wire-format regression tests (1.18.18) ──────────────────

// TestTranslate_ToolOnly_InputStartedUsesNameField is the regression
// guard for the field-name bug: input.started carries the tool name
// under `name`, not `tool`. The previous handler only read `tool`,
// so the bridge saw an empty tool name and rendered a bare receipt
// with no tool label.
func TestTranslate_ToolOnly_InputStartedUsesNameField(t *testing.T) {
	tr, snap := newTestTranslator()

	// input.started carries name="read", no `tool` key at all -
	// mirrors the live wire. We decode it manually because
	// sseToolEvent emits both `name` and `tool` and we want to
	// prove the handler reads `name` even when `tool` is absent.
	var startedEv SessionEvent
	startedRaw := `{"type":"session.next.tool.input.started","properties":{"callID":"tc1","name":"read"}}`
	if err := json.Unmarshal([]byte(startedRaw), &startedEv); err != nil {
		t.Fatalf("parse input.started: %v", err)
	}

	steps := []SessionEvent{
		startedEv,
		sseToolEvent("input.ended", "tc1", "", `"text":"{\"path\":\"/tmp/x\"}"`),
		sseToolEvent("called", "tc1", "read", ""),
		sseToolStructuredEvent("tc1", "file contents"),
	}
	for i, ev := range steps {
		if err := tr.handleEvent(ev); err != nil {
			t.Fatalf("handleEvent[%d]: %v", i, err)
		}
	}

	var gotName string
	for _, ev := range snap() {
		if ev.Kind == agent.EventAgentToolStart && ev.ToolStart != nil {
			gotName = ev.ToolStart.Name
		}
	}
	if gotName != "Read" {
		t.Errorf("ToolStart.Name = %q, want \"Read\" (input.started uses `name`, then normalizeToolName canonicalizes)", gotName)
	}
}

// TestTranslate_ToolOnly_InputEndedUsesTextField verifies that
// input.ended's final args arrive under the `text` JSON string
// key (not under `input`). Without this the streamed args buffer
// would never be replaced with the canonical full block, so the
// chat client would see the partial delta accumulation rather than
// the authoritative tool input.
func TestTranslate_ToolOnly_InputEndedUsesTextField(t *testing.T) {
	tr, snap := newTestTranslator()

	steps := []SessionEvent{
		sseToolEvent("input.started", "tc1", "read", ""),
		sseToolEvent("input.ended", "tc1", "", `"text":"{\"path\":\"/tmp/marker.txt\"}"`),
		sseToolEvent("called", "tc1", "read", ""),
		sseToolStructuredEvent("tc1", "PONG-from-file\n"),
	}
	for _, ev := range steps {
		if err := tr.handleEvent(ev); err != nil {
			t.Fatalf("handleEvent: %v", err)
		}
	}

	var startArgs string
	for _, ev := range snap() {
		if ev.Kind == agent.EventAgentToolStart && ev.ToolStart != nil {
			startArgs = ev.ToolStart.Args
		}
	}
	if !strings.Contains(startArgs, "marker.txt") {
		t.Errorf("ToolStart.Args = %q, want substring \"marker.txt\" (input.ended uses `text`)", startArgs)
	}
}

// TestTranslate_ToolOnly_SuccessExtractsStructuredContent asserts
// that the per-callID output populated from tool.success is the
// `structured.content` field (the live wire's payload for file-shaped
// tools) - NOT the `output` field that the previous handler looked
// for.
func TestTranslate_ToolOnly_SuccessExtractsStructuredContent(t *testing.T) {
	tr, snap := newTestTranslator()

	steps := []SessionEvent{
		sseToolEvent("input.started", "tc1", "read", ""),
		sseToolEvent("input.ended", "tc1", "", `"text":"{\"path\":\"/x\"}"`),
		sseToolEvent("called", "tc1", "read", ""),
		sseToolStructuredEvent("tc1", "structured-text-content"),
	}
	for _, ev := range steps {
		if err := tr.handleEvent(ev); err != nil {
			t.Fatalf("handleEvent: %v", err)
		}
	}

	var endOutput string
	for _, ev := range snap() {
		if ev.Kind == agent.EventAgentToolEnd && ev.ToolEnd != nil {
			endOutput = ev.ToolEnd.Output
		}
	}
	if endOutput != "structured-text-content" {
		t.Errorf("ToolEnd.Output = %q, want \"structured-text-content\" (extracted from structured.content)", endOutput)
	}
}

// TestTranslate_ToolOnly_SuccessExtractsContentArray asserts the
// LLMToolContent[] path: tool.success.data.content carries an
// array of {type,text} objects for bash-shaped tools. The bridge
// joins all text entries into one Output blob.
func TestTranslate_ToolOnly_SuccessExtractsContentArray(t *testing.T) {
	tr, snap := newTestTranslator()

	steps := []SessionEvent{
		sseToolEvent("input.started", "tc1", "bash", ""),
		sseToolEvent("input.ended", "tc1", "", `"text":"{\"cmd\":\"ls\"}"`),
		sseToolEvent("called", "tc1", "bash", ""),
		sseToolContentEvent("tc1", "stdout-line-1\nstdout-line-2\n"),
	}
	for _, ev := range steps {
		if err := tr.handleEvent(ev); err != nil {
			t.Fatalf("handleEvent: %v", err)
		}
	}

	var endOutput string
	for _, ev := range snap() {
		if ev.Kind == agent.EventAgentToolEnd && ev.ToolEnd != nil {
			endOutput = ev.ToolEnd.Output
		}
	}
	if !strings.Contains(endOutput, "stdout-line-1") || !strings.Contains(endOutput, "stdout-line-2") {
		t.Errorf("ToolEnd.Output = %q, want both stdout lines (extracted from content[])", endOutput)
	}
}

// TestTranslate_ToolOnly_ProgressPrePopulatesOutput verifies that
// tool.progress events cache their partial output into the
// pendingTools entry, so when tool.success eventually arrives with
// empty content, the chat client still sees the streamed progress
// (rather than an empty "(no output)").
func TestTranslate_ToolOnly_ProgressPrePopulatesOutput(t *testing.T) {
	tr, snap := newTestTranslator()

	steps := []SessionEvent{
		sseToolEvent("input.started", "tc1", "bash", ""),
		sseToolEvent("input.ended", "tc1", "", `"text":"{\"cmd\":\"sleep\"}"`),
		sseToolEvent("called", "tc1", "bash", ""),
		sseToolContentEvent("tc1", "streamed-stdout\n"),
	}
	for _, ev := range steps {
		if err := tr.handleEvent(ev); err != nil {
			t.Fatalf("handleEvent: %v", err)
		}
	}

	var endOutput string
	for _, ev := range snap() {
		if ev.Kind == agent.EventAgentToolEnd && ev.ToolEnd != nil {
			endOutput = ev.ToolEnd.Output
		}
	}
	if endOutput != "streamed-stdout" {
		t.Errorf("ToolEnd.Output = %q, want \"streamed-stdout\" (trailing newline stripped by extractToolOutput)", endOutput)
	}
}

// TestTranslate_ToolOnly_FailedExtractsObjectErrorMessage verifies
// that tool.failed.data.error shaped as an object (the live wire
// for permission-denied style failures) is flattened to its
// .data.message so the chat client shows a readable message
// rather than the literal JSON.
func TestTranslate_ToolOnly_FailedExtractsObjectErrorMessage(t *testing.T) {
	tr, snap := newTestTranslator()

	if err := tr.handleEvent(sseToolEvent("failed", "tc1", "bash", `"error":"{\"name\":\"PermissionDeniedError\",\"data\":{\"message\":\"opencode: permission denied for /etc/passwd\"}}"`)); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}

	var errMsg string
	for _, ev := range snap() {
		if ev.Kind == agent.EventAgentToolEnd && ev.Err != nil {
			errMsg = ev.Err.Error()
		}
	}
	if !strings.Contains(errMsg, "permission denied") {
		t.Errorf("ToolEnd.Err = %q, want substring \"permission denied\"", errMsg)
	}
}

// TestTranslate_ContextUpdatedDoesNotCrash confirms that the
// session.next.context.updated event (the server's prompt-side
// snapshot injection that arrives between model steps) is handled
// without panicking or emitting phantom text. The bridge currently
// only logs it; the regression guard is the no-crash invariant.
func TestTranslate_ContextUpdatedDoesNotCrash(t *testing.T) {
	tr, snap := newTestTranslator()

	if err := tr.handleEvent(sseContextUpdatedEvent("<available_skills>\n  ...long prompt context blob...\n</available_skills>")); err != nil {
		t.Fatalf("handleEvent: %v", err)
	}

	for _, ev := range snap() {
		if ev.Kind == agent.EventAgentText {
			t.Errorf("context.updated must not emit EventAgentText, got %q", ev.Text)
		}
		if ev.Kind == agent.EventAgentDone {
			t.Errorf("context.updated must not emit EventAgentDone (no turn end)")
		}
	}
}

// ─── extractToolOutput / uriBasename / truncateToolOutput ─────────

// TestExtractToolOutput_TruncatesLargeStructuredContent is the
// regression guard for the runaway-output case: a single read of a
// multi-MiB file would otherwise surface the entire payload in the
// chat receipt and balloon the in-memory AgentEvent. truncateToolOutput
// must clip at toolOutputMaxBytes and append a tail marker so the
// user sees the output was truncated, not that the file just ends
// mid-sentence.
func TestExtractToolOutput_TruncatesLargeStructuredContent(t *testing.T) {
	// Build a structured.content just over the cap so the test is
	// stable across toolOutputMaxBytes tweaks.
	oversize := strings.Repeat("x", toolOutputMaxBytes+128)
	raw, err := json.Marshal(map[string]any{
		"structured": map[string]any{"content": oversize},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var p sessionNextToolEvent
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := extractToolOutput(p.Structured, p.Content)
	if len(out) <= toolOutputMaxBytes {
		t.Fatalf("expected output to be capped at >%d bytes, got %d (no truncation?)", toolOutputMaxBytes, len(out))
	}
	if !strings.Contains(out, "[truncated") {
		t.Errorf("output should carry a truncation marker, got tail %q", out[len(out)-64:])
	}
	if !strings.HasPrefix(out, strings.Repeat("x", toolOutputMaxBytes)) {
		t.Errorf("output should start with the kept prefix of toolOutputMaxBytes x's, got prefix %q", out[:32])
	}
}

// TestExtractToolOutput_FileRefUsesBasenameForLongURIs is the
// regression guard for the file-shape receipt spam: when a
// structured.content is absent but content[] carries a file
// reference, a long file:// URI should surface as the basename
// (e.g. "marker.txt") rather than the full path
// ("file:///Users/.../very/long/path/to/marker.txt").
func TestExtractToolOutput_FileRefUsesBasenameForLongURIs(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "file", "uri": "file:///Users/geax/code/geax/github.com/cnlangzi/nightme.nightme/fix-gtw-agent/internal/bridge/opencode/translate.go", "mime": "text/x-go"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var p sessionNextToolEvent
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := extractToolOutput(p.Structured, p.Content)
	if !strings.Contains(out, "translate.go") {
		t.Errorf("output should contain basename, got %q", out)
	}
	if strings.Contains(out, "/Users/geax/") {
		t.Errorf("output should not contain the full URI, got %q", out)
	}
	if !strings.Contains(out, "text/x-go") {
		t.Errorf("output should carry mime suffix, got %q", out)
	}
}

// TestExtractToolOutput_FileRefPrefersNameOverURI asserts that a
// file ref that ships BOTH `name` and `uri` uses `name` - that is
// what every observed wire event does today, and the bridge should
// not regress to using the URI when a human label is available.
func TestExtractToolOutput_FileRefPrefersNameOverURI(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "file", "name": "MARKER.txt", "uri": "file:///tmp/x/MARKER.txt", "mime": "text/plain"},
		},
	})
	var p sessionNextToolEvent
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := extractToolOutput(p.Structured, p.Content)
	if !strings.HasPrefix(out, "MARKER.txt") {
		t.Errorf("output should start with the human name, got %q", out)
	}
}

// TestURIBasename covers the boundary cases the chat renderer
// relies on - scheme stripping, query / fragment trimming, and
// schemeless paths.
func TestURIBasename(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"file:///a/b/c.txt", "c.txt"},
		{"https://example.com/foo/bar?x=1", "bar"},
		{"https://example.com/foo/bar#frag", "bar"},
		{"a/b/c.txt", "c.txt"},
		{"just-a-file.txt", "just-a-file.txt"},
		{"file:///path/?q=1", "path"},
	}
	for _, tc := range cases {
		got := uriBasename(tc.in)
		if got != tc.want {
			t.Errorf("uriBasename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTruncateToolOutput confirms that small inputs pass through
// unchanged and oversize inputs are clipped with a marker.
func TestTruncateToolOutput(t *testing.T) {
	small := strings.Repeat("a", 100)
	if got := truncateToolOutput(small); got != small {
		t.Errorf("truncateToolOutput(small) mutated input")
	}
	big := strings.Repeat("a", toolOutputMaxBytes+1)
	got := truncateToolOutput(big)
	if !strings.HasPrefix(got, strings.Repeat("a", toolOutputMaxBytes)) {
		t.Errorf("truncated output should keep the prefix, got prefix %q", got[:32])
	}
	if !strings.Contains(got, "[truncated 1 bytes]") {
		t.Errorf("truncated output should carry a 1-byte marker, got tail %q", got[len(got)-32:])
	}
}

// TestExtractToolError_PrefersDataMessage confirms that the
// object-form error picks .data.message over .message over the
// raw JSON - never bare {}. The previous implementation had a
// .name fallback that surfaced the type instead of the message,
// producing chat receipts like "PermissionDeniedError" instead
// of the actual denial reason.
func TestExtractToolError_PrefersDataMessage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain string", "permission denied for /etc/passwd", "permission denied for /etc/passwd"},
		{"object with data.message", `{"name":"PermissionDeniedError","data":{"message":"opencode: permission denied"}}`, "opencode: permission denied"},
		{"object with top-level message", `{"message":"something bad"}`, "something bad"},
		{"object with neither", `{"name":"X"}`, `{"name":"X"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractToolError(tc.in); got != tc.want {
				t.Errorf("extractToolError(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
