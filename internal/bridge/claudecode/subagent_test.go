// Background-subagent turn-boundary tests.
//
// Claude Code can start background subagents through the Agent tool.
// A user message that spawned one spans several CLI turns: every
// finished subagent produces a task_notification frame, the CLI opens
// a new turn for it, and that turn ends with a result frame of its own.
// The bridge must not end the user's turn on the first result frame —
// the prompt has to stay installed until the last subagent finished,
// so every turn's events keep their anchor.
package claudecode

import (
	"strconv"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// countKinds returns how many events of the given kind the stream
// produced.
func countKinds(evs []agent.AgentEvent, kind agent.EventKind) int {
	n := 0
	for _, ev := range evs {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// TestPumpStream_Result_SubagentsOutstanding_KeepsTurnOpen covers the
// core guard: a result frame whose counters still report a running
// subagent must not end the turn.
func TestPumpStream_Result_SubagentsOutstanding_KeepsTurnOpen(t *testing.T) {
	evs := streamFromFixture(t, "subagent_result_outstanding.json", nil)
	if n := countKinds(evs, agent.EventAgentResult); n != 0 {
		t.Errorf("EventAgentResult count = %d, want 0 (the turn continues while a subagent runs)", n)
	}
	if n := countKinds(evs, agent.EventAgentDone); n != 0 {
		t.Errorf("EventAgentDone count = %d, want 0 (the turn continues while a subagent runs)", n)
	}
}

// TestPumpStream_Result_SubagentsSettled_EndsTurn is the other half:
// once the last subagent completed, the result frame ends the turn.
func TestPumpStream_Result_SubagentsSettled_EndsTurn(t *testing.T) {
	evs := streamFromFixture(t, "subagent_result_settled.json", nil)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (Result + Done)", len(evs))
	}
	if evs[0].Kind != agent.EventAgentResult {
		t.Errorf("evs[0].Kind = %v, want EventAgentResult", evs[0].Kind)
	}
	if evs[1].Kind != agent.EventAgentDone {
		t.Errorf("evs[1].Kind = %v, want EventAgentDone", evs[1].Kind)
	}
	if evs[1].Done == nil || evs[1].Done.ExitCode != 0 {
		t.Errorf("done = %+v, want ExitCode 0", evs[1].Done)
	}
}

// TestPumpStream_Result_Error_EndsTurnWithSubagentsOutstanding keeps
// the failure visible: the turn failed, so withholding the error would
// hide it and leave the one-shot path waiting for a success that never
// comes.
func TestPumpStream_Result_Error_EndsTurnWithSubagentsOutstanding(t *testing.T) {
	evs := streamFromFixture(t, "subagent_result_error.json", nil)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (Result + Done)", len(evs))
	}
	if evs[0].Kind != agent.EventAgentResult {
		t.Fatalf("evs[0].Kind = %v, want EventAgentResult", evs[0].Kind)
	}
	if evs[0].Err == nil {
		t.Error("EventAgentResult.Err is nil, want the error to be carried")
	}
	if evs[0].Result == nil || evs[0].Result.Text == "" {
		t.Error("EventAgentResult.Text is empty, want the error text")
	}
	if evs[1].Kind != agent.EventAgentDone {
		t.Errorf("evs[1].Kind = %v, want EventAgentDone", evs[1].Kind)
	}
}

// TestPumpStream_Result_ZeroCounters_EndsTurn covers a result frame
// whose counters are all zero — nothing was spawned, so the turn ends.
func TestPumpStream_Result_ZeroCounters_EndsTurn(t *testing.T) {
	input := `{"type":"result","subtype":"success","result":"done","subagent_stats":{"spawned":0,"completed":0,"failed":0,"killed":{"parent":0,"user":0,"system":0}}}` + "\n"
	evs := streamFromString(input)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (Result + Done)", len(evs))
	}
	if evs[0].Kind != agent.EventAgentResult {
		t.Errorf("evs[0].Kind = %v, want EventAgentResult", evs[0].Kind)
	}
	if evs[1].Kind != agent.EventAgentDone {
		t.Errorf("evs[1].Kind = %v, want EventAgentDone", evs[1].Kind)
	}
}

// TestPumpStream_Result_KilledSubagents_EndsTurn: a killed subagent
// reached a terminal state, so nothing is outstanding any more.
func TestPumpStream_Result_KilledSubagents_EndsTurn(t *testing.T) {
	input := `{"type":"result","subtype":"success","result":"done","subagent_stats":{"spawned":2,"completed":0,"failed":0,"killed":{"parent":1,"user":1,"system":0}}}` + "\n"
	evs := streamFromString(input)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (Result + Done)", len(evs))
	}
	if evs[1].Kind != agent.EventAgentDone {
		t.Errorf("evs[1].Kind = %v, want EventAgentDone", evs[1].Kind)
	}
}

// TestPumpStream_Result_CountersExceedSpawned_EndsTurn covers the
// counters running ahead of `spawned`: a /clear zeroes them while a
// background subagent that outlives the /clear still records its
// outcome. The outstanding count must clamp at zero instead of going
// negative and holding the turn open forever.
func TestPumpStream_Result_CountersExceedSpawned_EndsTurn(t *testing.T) {
	input := `{"type":"result","subtype":"success","result":"done","subagent_stats":{"spawned":0,"completed":1,"failed":0,"killed":{"parent":0,"user":0,"system":0}}}` + "\n"
	evs := streamFromString(input)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (Result + Done)", len(evs))
	}
	if evs[1].Kind != agent.EventAgentDone {
		t.Errorf("evs[1].Kind = %v, want EventAgentDone", evs[1].Kind)
	}
}

// TestPumpStream_Result_WithoutSubagentStats_EndsTurn: an older CLI
// reports no subagent_stats at all. The counters read as zero, so the
// turn ends as it always did.
func TestPumpStream_Result_WithoutSubagentStats_EndsTurn(t *testing.T) {
	evs := streamFromFixture(t, "result.json", nil)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (Result + Done)", len(evs))
	}
	if evs[0].Kind != agent.EventAgentResult {
		t.Errorf("evs[0].Kind = %v, want EventAgentResult", evs[0].Kind)
	}
	if evs[1].Kind != agent.EventAgentDone {
		t.Errorf("evs[1].Kind = %v, want EventAgentDone", evs[1].Kind)
	}
}

// TestPumpStream_HeldOpenTurn_KeepsAssistantText checks that the text
// of a held-open turn still reaches the chat: the assistant block
// streams it as EventAgentText, the result frame's own copy is the
// duplicate the success path drops.
func TestPumpStream_HeldOpenTurn_KeepsAssistantText(t *testing.T) {
	input := `{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"我先派两个 subagent"}]},"model":"claude-sonnet-4-5"}` + "\n" +
		`{"type":"result","subtype":"success","result":"我先派两个 subagent","subagent_stats":{"spawned":2,"completed":0,"failed":0,"killed":{"parent":0,"user":0,"system":0}}}` + "\n"
	evs := streamFromString(input)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1 (the assistant text only)", len(evs))
	}
	if evs[0].Kind != agent.EventAgentText {
		t.Fatalf("evs[0].Kind = %v, want EventAgentText", evs[0].Kind)
	}
	if evs[0].Text != "我先派两个 subagent" {
		t.Errorf("Text = %q, want the assistant text", evs[0].Text)
	}
}

// TestPumpStream_MultiTurnWithSubagents_TerminatesOnce replays the
// production shape: a turn that starts two background subagents, then
// one task-notification turn per finished subagent. The turn ends once,
// on the last result frame.
func TestPumpStream_MultiTurnWithSubagents_TerminatesOnce(t *testing.T) {
	assistant := func(text string) string {
		return `{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"` + text + `"}]},"model":"claude-sonnet-4-5"}` + "\n"
	}
	result := func(completed int) string {
		return `{"type":"result","subtype":"success","result":"progress","subagent_stats":{"spawned":2,"completed":` +
			strconv.Itoa(completed) + `,"failed":0,"killed":{"parent":0,"user":0,"system":0}}}` + "\n"
	}
	input := strings.Join([]string{
		assistant("先派出去"),
		result(0),
		assistant("第一个回来了"),
		result(1),
		assistant("第二个回来了"),
		result(2),
	}, "")

	evs := streamFromString(input)
	if n := countKinds(evs, agent.EventAgentText); n != 3 {
		t.Errorf("EventAgentText count = %d, want 3 (one per turn)", n)
	}
	if n := countKinds(evs, agent.EventAgentResult); n != 1 {
		t.Errorf("EventAgentResult count = %d, want 1", n)
	}
	if n := countKinds(evs, agent.EventAgentDone); n != 1 {
		t.Errorf("EventAgentDone count = %d, want 1", n)
	}
	// The terminating pair comes last: the turn ends after the last
	// turn's text, not before it.
	last := evs[len(evs)-1]
	if last.Kind != agent.EventAgentDone {
		t.Errorf("last event = %v, want EventAgentDone", last.Kind)
	}
}

// TestPumpStream_RealBackgroundSubagentTurns_OneTurn replays a real
// capture: a turn that launched a background subagent, then the
// task-notification turn the subagent's completion opened.
//
// The capture is the CLI's own output with stdin held open — the way
// the bridge drives it. The CLI writes the first result while the
// subagent still runs (spawned=1, completed=0); the second turn's
// result carries the completed counter. The bridge must end the turn
// on the second result only.
func TestPumpStream_RealBackgroundSubagentTurns_OneTurn(t *testing.T) {
	evs := streamFromFixture(t, "subagent_turns.jsonl", nil)
	if n := countKinds(evs, agent.EventAgentText); n != 2 {
		t.Errorf("EventAgentText count = %d, want 2 (one per turn)", n)
	}
	if n := countKinds(evs, agent.EventAgentResult); n != 1 {
		t.Errorf("EventAgentResult count = %d, want 1 (the last turn's result only)", n)
	}
	if n := countKinds(evs, agent.EventAgentDone); n != 1 {
		t.Errorf("EventAgentDone count = %d, want 1 (the last turn only)", n)
	}
	// The turn ends after the second turn's text — the first turn's
	// result must not close it.
	last := evs[len(evs)-1]
	if last.Kind != agent.EventAgentDone {
		t.Errorf("last event = %v, want EventAgentDone", last.Kind)
	}
	if evs[0].Kind != agent.EventAgentText || evs[0].Text != "LAUNCHED" {
		t.Errorf("evs[0] = %+v, want the first turn's text", evs[0])
	}
}

// TestPumpStream_TwoBackgroundAgents_OneTurn replays a real capture of
// a turn that launched two background subagents which finished within
// milliseconds of each other.
//
// The CLI wrote the launch turn's result frame after both agents had
// finished, so its counters report spawned=2 / completed=2 — nothing
// outstanding — while the turns the two completions opened were still
// queued. The counters alone would end the user's turn there and
// orphan the two notification turns; the task bookkeeping is what says
// they are still coming.
func TestPumpStream_TwoBackgroundAgents_OneTurn(t *testing.T) {
	evs := streamFromFixture(t, "subagent_two_agents.jsonl", nil)
	if n := countKinds(evs, agent.EventAgentText); n != 2 {
		t.Errorf("EventAgentText count = %d, want 2 (one per turn)", n)
	}
	if n := countKinds(evs, agent.EventAgentResult); n != 1 {
		t.Errorf("EventAgentResult count = %d, want 1 (the last turn's result only)", n)
	}
	if n := countKinds(evs, agent.EventAgentDone); n != 1 {
		t.Errorf("EventAgentDone count = %d, want 1 (the last turn only)", n)
	}
	// Both turns' text lands before the turn ends — the notification
	// turn's text must not arrive after the Done.
	doneAt := -1
	for i, ev := range evs {
		if ev.Kind == agent.EventAgentDone {
			doneAt = i
		}
	}
	if doneAt < 0 {
		t.Fatal("no EventAgentDone")
	}
	for i, ev := range evs {
		if ev.Kind == agent.EventAgentText && i > doneAt {
			t.Errorf("event %d = %+v, want no text after the turn ended", i, ev)
		}
	}
}

// TestPumpStream_StaleBackgroundAgent_DoesNotHoldNextTurn covers the
// cross-turn case: a turn that ends with an error while a background
// agent is still running leaves that agent alive, and the next user
// message must not wait for it. The counters are cumulative for the
// session, so the frame of the next turn still reports the agent.
func TestPumpStream_StaleBackgroundAgent_DoesNotHoldNextTurn(t *testing.T) {
	input := `{"type":"system","subtype":"task_started","task_id":"t1","run_id":"r1","is_backgrounded":true}` + "\n" +
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom","subagent_stats":{"spawned":1,"completed":0,"killed":{"parent":0,"user":0,"system":0}}}` + "\n" +
		`{"type":"result","subtype":"success","result":"second answer","subagent_stats":{"spawned":1,"completed":0,"killed":{"parent":0,"user":0,"system":0}}}` + "\n"
	evs := streamFromString(input)
	if n := countKinds(evs, agent.EventAgentResult); n != 2 {
		t.Errorf("EventAgentResult count = %d, want 2 (the error turn and the next turn)", n)
	}
	if n := countKinds(evs, agent.EventAgentDone); n != 2 {
		t.Errorf("EventAgentDone count = %d, want 2", n)
	}
}

// TestPumpStream_Result_MalformedSubagentStats_StillEndsTurn: a
// subagent_stats shape the bridge does not know must not make the
// frame undecodable — a dropped result frame is a turn that never
// ends. The counters read as "no subagent" and the turn ends.
func TestPumpStream_Result_MalformedSubagentStats_StillEndsTurn(t *testing.T) {
	input := `{"type":"result","subtype":"success","result":"done","subagent_stats":{"spawned":2,"completed":0,"killed":3}}` + "\n"
	evs := streamFromString(input)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (Result + Done)", len(evs))
	}
	if evs[0].Kind != agent.EventAgentResult {
		t.Errorf("evs[0].Kind = %v, want EventAgentResult", evs[0].Kind)
	}
	if evs[1].Kind != agent.EventAgentDone {
		t.Errorf("evs[1].Kind = %v, want EventAgentDone", evs[1].Kind)
	}
}

// TestPumpStream_HeldOpenTurn_SumsUsage: the frames the bridge
// withholds belong to the user's turn, so their tokens are added to
// the frame that ends it — the CLI reports usage per turn.
func TestPumpStream_HeldOpenTurn_SumsUsage(t *testing.T) {
	input := `{"type":"result","subtype":"success","result":"progress","usage":{"input_tokens":100,"output_tokens":10},"subagent_stats":{"spawned":1,"completed":0,"killed":{"parent":0,"user":0,"system":0}}}` + "\n" +
		`{"type":"result","subtype":"success","result":"done","usage":{"input_tokens":20,"output_tokens":5},"subagent_stats":{"spawned":1,"completed":1,"killed":{"parent":0,"user":0,"system":0}}}` + "\n"
	evs := streamFromString(input)
	if n := countKinds(evs, agent.EventAgentResult); n != 1 {
		t.Fatalf("EventAgentResult count = %d, want 1", n)
	}
	usage := evs[0].Result.Usage
	if usage == nil {
		t.Fatal("Usage is nil")
	}
	if usage.InputTokens != 120 {
		t.Errorf("InputTokens = %d, want 120 (100 withheld + 20 reported)", usage.InputTokens)
	}
	if usage.OutputTokens != 15 {
		t.Errorf("OutputTokens = %d, want 15 (10 withheld + 5 reported)", usage.OutputTokens)
	}
}
