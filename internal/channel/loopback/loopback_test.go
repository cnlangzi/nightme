package loopback

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
)

// chatID is the chat the tests drive.
const chatID = "lb_main"

// newClient returns a loopback channel with a client bound to it.
func newClient(t *testing.T) (*Channel, *Client) {
	t.Helper()
	ch := New()
	return ch, NewClient(ch)
}

// emit scripts one outbound message the way the daemon would send it.
func emit(t *testing.T, ch *Channel, msg messages.OutboundMessage) {
	t.Helper()
	if err := ch.Send(context.Background(), msg); err != nil {
		t.Fatalf("channel Send: %v", err)
	}
}

// stateMsg builds the message-state event for a message id.
func stateMsg(replyTo string, state agent.MessageState) messages.OutboundMessage {
	return messages.OutboundMessage{
		ChatID:  chatID,
		Kind:    messages.OutMessageState,
		ReplyTo: replyTo,
		MessageState: &messages.MessageStatePayload{
			State:     state,
			MessageID: replyTo,
		},
	}
}

// settledMsg builds the settled event for a message id.
func settledMsg(replyTo string, outcome messages.Outcome) messages.OutboundMessage {
	return messages.OutboundMessage{
		ChatID:  chatID,
		Kind:    messages.OutMessageSettled,
		ReplyTo: replyTo,
		Outcome: &outcome,
	}
}

// phaseOf collects the phase of every snapshot, dropping repeats.
func phaseOf(snaps []Execution) []Phase {
	var out []Phase
	for _, s := range snaps {
		if n := len(out); n > 0 && out[n-1] == s.Phase {
			continue
		}
		out = append(out, s.Phase)
	}
	return out
}

// drain reads every snapshot of a watch channel.
func drain(ch <-chan Execution) []Execution {
	var out []Execution
	for s := range ch {
		out = append(out, s)
	}
	return out
}

func TestSend_SetsMessageIDHasMentionAndUserID(t *testing.T) {
	ch, client := newClient(t)

	msgID, err := client.Send(context.Background(), chatID, "hello nightme")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if msgID != "lb-1" {
		t.Errorf("MessageID = %q, want %q", msgID, "lb-1")
	}

	var in messages.InboundMessage
	select {
	case in = <-ch.Incoming():
	case <-time.After(2 * time.Second):
		t.Fatal("no inbound message arrived")
	}
	if in.MessageID != msgID {
		t.Errorf("inbound MessageID = %q, want %q", in.MessageID, msgID)
	}
	if !in.HasMention {
		t.Error("HasMention = false, want true (the WatchMode gate would drop the message)")
	}
	if in.UserID != "loopback" {
		t.Errorf("UserID = %q, want %q", in.UserID, "loopback")
	}
	if in.ChatID != chatID {
		t.Errorf("ChatID = %q, want %q", in.ChatID, chatID)
	}
	if in.Text != "hello nightme" {
		t.Errorf("Text = %q, want %q", in.Text, "hello nightme")
	}
	if in.Time.IsZero() {
		t.Error("Time is zero")
	}
}

func TestSend_GeneratesDistinctMessageIDs(t *testing.T) {
	_, client := newClient(t)
	ctx := context.Background()

	first, err := client.Send(ctx, chatID, "one")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	second, err := client.Send(ctx, chatID, "two")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if first == second {
		t.Fatalf("both messages got id %q", first)
	}
	if second != "lb-2" {
		t.Errorf("second MessageID = %q, want %q", second, "lb-2")
	}
}

// TestPhase_SyncCommand scripts the outbound sequence of a command
// that answers on its own: queued, command reply, settled.
func TestPhase_SyncCommand(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "/cwd /tmp")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	watch := client.Watch(msgID)

	emit(t, ch, stateMsg(msgID, agent.MessageQueued))
	emit(t, ch, messages.OutboundMessage{
		ChatID: chatID, Kind: messages.OutCommandReply, ReplyTo: msgID, Text: "cwd set to /tmp",
	})
	emit(t, ch, settledMsg(msgID, messages.Outcome{Status: messages.OutcomeOK}))

	snaps := drain(watch)
	got := phaseOf(snaps)
	want := []Phase{PhaseSent, PhaseQueued, PhaseSettled}
	if len(got) != len(want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("phases = %v, want %v", got, want)
		}
	}
	if last := snaps[len(snaps)-1]; last.Phase != PhaseSettled {
		t.Errorf("final phase = %q, want %q", last.Phase, PhaseSettled)
	}
	if last := snaps[len(snaps)-1]; last.SettledAt.IsZero() {
		t.Error("SettledAt is zero")
	}
}

// TestPhase_CommandWithAgentTurn scripts the outbound sequence of a
// command that queues an agent turn: queued, command reply,
// submitted, agent output, result, settled.
func TestPhase_CommandWithAgentTurn(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "/gtw commit")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	watch := client.Watch(msgID)

	emit(t, ch, stateMsg(msgID, agent.MessageQueued))
	emit(t, ch, messages.OutboundMessage{
		ChatID: chatID, Kind: messages.OutCommandReply, ReplyTo: msgID, Text: "committing",
	})
	emit(t, ch, stateMsg(msgID, agent.MessageSubmitted))
	emit(t, ch, messages.OutboundMessage{
		ChatID: chatID, Kind: messages.OutReply, ReplyTo: msgID, Text: "running the agent",
	})
	emit(t, ch, messages.OutboundMessage{
		ChatID: chatID, Kind: messages.OutResult, ReplyTo: msgID, Text: "committed 3 files",
	})
	emit(t, ch, settledMsg(msgID, messages.Outcome{Status: messages.OutcomeOK}))

	snaps := drain(watch)
	got := phaseOf(snaps)
	want := []Phase{PhaseSent, PhaseQueued, PhaseRunning, PhaseSettled}
	if len(got) != len(want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("phases = %v, want %v", got, want)
		}
	}

	final := snaps[len(snaps)-1]
	if final.StartedAt.IsZero() {
		t.Error("StartedAt is zero — the running phase never stamped it")
	}
	if final.Final() != "committed 3 files" {
		t.Errorf("Final() = %q, want %q", final.Final(), "committed 3 files")
	}
}

// TestPhase_ReviewWithInjectedTurn scripts the outbound sequence of
// /review: the command answers, then the injected agent turn runs and
// the message settles.
func TestPhase_ReviewWithInjectedTurn(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "/review")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	watch := client.Watch(msgID)

	emit(t, ch, stateMsg(msgID, agent.MessageQueued))
	emit(t, ch, messages.OutboundMessage{
		ChatID: chatID, Kind: messages.OutCommandReply, ReplyTo: msgID, Text: "reviewing 3 groups",
	})
	emit(t, ch, messages.OutboundMessage{
		ChatID: chatID, Kind: messages.OutReply, ReplyTo: msgID, Text: "group 1: 2 findings",
	})
	emit(t, ch, settledMsg(msgID, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "review finished",
		Data:    map[string]string{"groups": "3"},
	}))

	snaps := drain(watch)
	got := phaseOf(snaps)
	want := []Phase{PhaseSent, PhaseQueued, PhaseRunning, PhaseSettled}
	if len(got) != len(want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("phases = %v, want %v", got, want)
		}
	}
	final := snaps[len(snaps)-1]
	if final.Outcome.Data["groups"] != "3" {
		t.Errorf("outcome data = %v, want groups=3", final.Outcome.Data)
	}
}

func TestOutcome_CopiedFromSettled(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "/cwd /tmp")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	emit(t, ch, settledMsg(msgID, messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: "cwd set",
		Data:    map[string]string{"cwd": "/tmp"},
	}))

	exec, err := client.Await(ctx, msgID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if exec.Outcome.Status != messages.OutcomeOK {
		t.Errorf("Outcome.Status = %q, want %q", exec.Outcome.Status, messages.OutcomeOK)
	}
	if exec.Outcome.Summary != "cwd set" {
		t.Errorf("Outcome.Summary = %q, want %q", exec.Outcome.Summary, "cwd set")
	}
	if exec.Outcome.Data["cwd"] != "/tmp" {
		t.Errorf("Outcome.Data = %v, want cwd=/tmp", exec.Outcome.Data)
	}
}

func TestOutcome_UnknownStaysUnknown(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "/close")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	emit(t, ch, settledMsg(msgID, messages.Outcome{Status: messages.OutcomeUnknown}))

	exec, err := client.Await(ctx, msgID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if exec.Outcome.Status != messages.OutcomeUnknown {
		t.Errorf("Outcome.Status = %q, want %q", exec.Outcome.Status, messages.OutcomeUnknown)
	}
	if exec.Outcome.IsReported() {
		t.Error("Outcome reports a verdict, want none")
	}
}

// TestAwait_SettledBeforeAwait pins the "a settle that arrives
// before anybody waits is never lost" contract.
func TestAwait_SettledBeforeAwait(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "/cwd /tmp")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	emit(t, ch, settledMsg(msgID, messages.Outcome{Status: messages.OutcomeOK, Summary: "done"}))

	exec, err := client.Await(ctx, msgID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if exec.Phase != PhaseSettled {
		t.Errorf("Phase = %q, want %q", exec.Phase, PhaseSettled)
	}
	if exec.Outcome.Status != messages.OutcomeOK {
		t.Errorf("Outcome.Status = %q, want %q", exec.Outcome.Status, messages.OutcomeOK)
	}
}

func TestAwait_ContextTimeoutKeepsPartialTranscript(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "/gtw commit")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	emit(t, ch, messages.OutboundMessage{
		ChatID: chatID, Kind: messages.OutCommandReply, ReplyTo: msgID, Text: "committing",
	})

	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	exec, err := client.Await(waitCtx, msgID)
	if err == nil {
		t.Fatal("Await returned no error on a deadline")
	}
	if exec.Outcome.Status != messages.OutcomeTimeout {
		t.Errorf("Outcome.Status = %q, want %q", exec.Outcome.Status, messages.OutcomeTimeout)
	}
	if len(exec.Transcript) != 1 {
		t.Fatalf("Transcript = %v, want 1 entry", exec.Transcript)
	}
	if exec.Transcript[0].Text != "committing" {
		t.Errorf("Transcript[0].Text = %q, want %q", exec.Transcript[0].Text, "committing")
	}
}

func TestSend_RejectsAChatIDOutsideTheLoopbackNamespace(t *testing.T) {
	_, client := newClient(t)
	ctx := context.Background()

	// A chat of another channel belongs to that channel's session.
	if _, err := client.Send(ctx, "oc_existing", "hello"); err == nil {
		t.Error("Send accepted a foreign chat id")
	}
	// A chat id without a known prefix is dropped by the chat store
	// on the next load, so its session would not survive.
	if _, err := client.Send(ctx, "main", "hello"); err == nil {
		t.Error("Send accepted a chat id without the lb_ prefix")
	}
	if _, err := client.Send(ctx, "", "hello"); err == nil {
		t.Error("Send accepted an empty chat id")
	}
}

func TestAwait_UnknownMessageID(t *testing.T) {
	_, client := newClient(t)
	if _, err := client.Await(context.Background(), "lb-404"); err == nil {
		t.Fatal("Await on an unknown message id returned no error")
	}
}

// TestAwait_SettledWinsOverAnExpiredDeadline pins the race between a
// settle and a deadline: the outcome the daemon reported wins.
func TestAwait_SettledWinsOverAnExpiredDeadline(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "/cwd /tmp")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	emit(t, ch, settledMsg(msgID, messages.Outcome{Status: messages.OutcomeOK}))

	expired, cancel := context.WithCancel(ctx)
	cancel()
	exec, err := client.Await(expired, msgID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if exec.Outcome.Status != messages.OutcomeOK {
		t.Errorf("Outcome.Status = %q, want %q", exec.Outcome.Status, messages.OutcomeOK)
	}
}

// TestWatch_ReportsPhaseAndHeartbeat pins the progress feed: phase
// changes and heartbeats both produce a snapshot.
func TestWatch_ReportsPhaseAndHeartbeat(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "build it")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	watch := client.Watch(msgID)

	emit(t, ch, stateMsg(msgID, agent.MessageQueued))
	beat := time.Now()
	emit(t, ch, messages.OutboundMessage{
		ChatID:  chatID,
		Kind:    messages.OutHeartbeat,
		ReplyTo: msgID,
		Heartbeat: &messages.HeartbeatSnapshot{
			ThinkCount: 2,
			ToolCount:  1,
			LastBeatAt: beat,
		},
	})
	emit(t, ch, settledMsg(msgID, messages.Outcome{Status: messages.OutcomeOK}))

	snaps := drain(watch)
	var sawHeartbeat bool
	for _, s := range snaps {
		if s.Progress.ThinkCount == 2 && s.Progress.ToolCount == 1 {
			sawHeartbeat = true
			if !s.Progress.LastBeatAt.Equal(beat) {
				t.Errorf("LastBeatAt = %v, want %v", s.Progress.LastBeatAt, beat)
			}
		}
	}
	if !sawHeartbeat {
		t.Errorf("no snapshot carried the heartbeat progress: %v", snaps)
	}
}

// TestWatch_ShowsPendingChoice pins the pending-prompt view: a
// permission prompt stays visible until it is answered.
func TestWatch_ShowsPendingChoice(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "do it")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	watch := client.Watch(msgID)

	emit(t, ch, messages.OutboundMessage{
		ChatID:  chatID,
		Kind:    messages.OutChoice,
		ReplyTo: msgID,
		Choice: &messages.Choice{
			RequestID: "perm:lb_main:1",
			Kind:      messages.ChoiceKindPermission,
			Title:     "Waiting for approval",
			Options:   []messages.ChoiceOption{{ID: "yes", Label: "Yes"}},
		},
	})

	var sawChoice bool
	deadline := time.After(2 * time.Second)
	for !sawChoice {
		select {
		case s, ok := <-watch:
			if !ok {
				t.Fatal("watch closed before the choice showed up")
			}
			if len(s.Choices) == 1 && s.Choices[0].RequestID == "perm:lb_main:1" {
				sawChoice = true
			}
		case <-deadline:
			t.Fatal("the pending choice never showed up on the watch channel")
		}
	}
}

func TestChoice_AnsweredChoiceLeavesThePendingList(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "do it")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	emit(t, ch, messages.OutboundMessage{
		ChatID:  chatID,
		Kind:    messages.OutChoice,
		ReplyTo: msgID,
		Choice: &messages.Choice{
			RequestID: "perm:lb_main:1",
			Kind:      messages.ChoiceKindPermission,
		},
	})
	exec, ok := client.Get(msgID)
	if !ok {
		t.Fatal("Get: no execution")
	}
	if len(exec.Choices) != 1 {
		t.Fatalf("Choices = %v, want 1 pending prompt", exec.Choices)
	}

	emit(t, ch, messages.OutboundMessage{
		ChatID:  chatID,
		Kind:    messages.OutChoicePatch,
		ReplyTo: msgID,
		Choice: &messages.Choice{
			RequestID: "perm:lb_main:1",
			Kind:      messages.ChoiceKindPermission,
			Settled:   true,
		},
	})
	exec, ok = client.Get(msgID)
	if !ok {
		t.Fatal("Get: no execution")
	}
	if len(exec.Choices) != 0 {
		t.Errorf("Choices = %v, want none after the prompt was answered", exec.Choices)
	}
}

// TestObserve_ForeignEntriesAreNotMixedIn pins the correlation
// contract: a message of another chat, or one naming an unknown
// message id, never lands in an execution.
// TestChoice_DashboardSettleClearsThePendingPrompt pins the patch
// the daemon sends when it settles a gate it resolved outside the
// channel: that patch names no prompt, and the run has nothing left
// to answer.
func TestChoice_DashboardSettleClearsThePendingPrompt(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "do it")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	emit(t, ch, messages.OutboundMessage{
		ChatID:  chatID,
		Kind:    messages.OutChoice,
		ReplyTo: msgID,
		Choice: &messages.Choice{
			RequestID: "perm:lb_main:1",
			Kind:      messages.ChoiceKindPermission,
		},
	})

	// The daemon settles the gate on the dashboard: no RequestID.
	emit(t, ch, messages.OutboundMessage{
		ChatID:  chatID,
		Kind:    messages.OutChoicePatch,
		ReplyTo: msgID,
		Choice: &messages.Choice{
			Kind:    messages.ChoiceKindPermission,
			Settled: true,
		},
	})

	exec, ok := client.Get(msgID)
	if !ok {
		t.Fatal("Get: no execution")
	}
	if len(exec.Choices) != 0 {
		t.Errorf("Choices = %v, want none after the gate was settled", exec.Choices)
	}
}

// TestWatch_SettledExecutionReportsTheFinalSnapshot pins the watch of
// a run that is already over: the watcher gets the final snapshot and
// the channel closes.
func TestWatch_SettledExecutionReportsTheFinalSnapshot(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "/cwd /tmp")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	emit(t, ch, settledMsg(msgID, messages.Outcome{Status: messages.OutcomeOK}))

	snaps := drain(client.Watch(msgID))
	if len(snaps) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snaps))
	}
	if snaps[0].Phase != PhaseSettled {
		t.Errorf("Phase = %q, want %q", snaps[0].Phase, PhaseSettled)
	}
	if snaps[0].Outcome.Status != messages.OutcomeOK {
		t.Errorf("Outcome.Status = %q, want %q", snaps[0].Outcome.Status, messages.OutcomeOK)
	}
}

func TestObserve_ForeignEntriesAreNotMixedIn(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "hello")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Another chat, naming this execution.
	emit(t, ch, messages.OutboundMessage{
		ChatID: "lb_other", Kind: messages.OutCommandReply, ReplyTo: msgID, Text: "not mine",
	})
	// This chat, naming an execution that does not exist.
	emit(t, ch, messages.OutboundMessage{
		ChatID: chatID, Kind: messages.OutCommandReply, ReplyTo: "lb-999", Text: "not mine either",
	})

	exec, ok := client.Get(msgID)
	if !ok {
		t.Fatal("Get: no execution")
	}
	if len(exec.Transcript) != 0 {
		t.Errorf("Transcript = %v, want empty", exec.Transcript)
	}

	// A message of this chat without a ReplyTo belongs to the
	// in-flight execution.
	emit(t, ch, messages.OutboundMessage{
		ChatID: chatID, Kind: messages.OutCommandReply, Text: "mine",
	})
	exec, ok = client.Get(msgID)
	if !ok {
		t.Fatal("Get: no execution")
	}
	if len(exec.Transcript) != 1 || exec.Transcript[0].Text != "mine" {
		t.Errorf("Transcript = %v, want the chat's own reply", exec.Transcript)
	}
}

func TestGet_ReportsTheLiveExecution(t *testing.T) {
	_, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "hello")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	exec, ok := client.Get(msgID)
	if !ok {
		t.Fatal("Get: no execution")
	}
	if exec.MsgID != msgID {
		t.Errorf("MsgID = %q, want %q", exec.MsgID, msgID)
	}
	if exec.ChatID != chatID {
		t.Errorf("ChatID = %q, want %q", exec.ChatID, chatID)
	}
	if exec.Text != "hello" {
		t.Errorf("Text = %q, want %q", exec.Text, "hello")
	}
	if exec.Phase != PhaseSent {
		t.Errorf("Phase = %q, want %q", exec.Phase, PhaseSent)
	}
	if exec.SentAt.IsZero() {
		t.Error("SentAt is zero")
	}
}

// TestTranscript_CapKeepsHeadAndTail pins the transcript cap.
func TestTranscript_CapKeepsHeadAndTail(t *testing.T) {
	ch, client := newClient(t)
	ctx := context.Background()

	msgID, err := client.Send(ctx, chatID, "long run")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	total := MaxTranscriptEntries + 50
	for i := 0; i < total; i++ {
		emit(t, ch, messages.OutboundMessage{
			ChatID:  chatID,
			Kind:    messages.OutReply,
			ReplyTo: msgID,
			Text:    time.Unix(int64(i), 0).UTC().Format(time.RFC3339),
		})
	}

	exec, ok := client.Get(msgID)
	if !ok {
		t.Fatal("Get: no execution")
	}
	if len(exec.Transcript) != MaxTranscriptEntries {
		t.Fatalf("Transcript has %d entries, want %d", len(exec.Transcript), MaxTranscriptEntries)
	}
	if exec.Transcript[0].Text != time.Unix(0, 0).UTC().Format(time.RFC3339) {
		t.Errorf("first entry = %q, want the oldest one", exec.Transcript[0].Text)
	}
	last := time.Unix(int64(total-1), 0).UTC().Format(time.RFC3339)
	if exec.Transcript[len(exec.Transcript)-1].Text != last {
		t.Errorf("last entry = %q, want %q", exec.Transcript[len(exec.Transcript)-1].Text, last)
	}
}

func TestHealthSummary_ConnectedOnceStarted(t *testing.T) {
	ch := New()
	if got := ch.HealthSummary(); got.Connected {
		t.Error("Connected = true before Start")
	}
	if err := ch.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	summary := ch.HealthSummary()
	if !summary.Connected {
		t.Error("Connected = false after Start")
	}
	if summary.Channel != "loopback" {
		t.Errorf("Channel = %q, want %q", summary.Channel, "loopback")
	}
}

// TestRegistry_RegistersTheLoopbackPrefix pins the registry
// contract: the channel registers itself under the "lb_" prefix and
// its builder needs no configuration.
func TestRegistry_RegistersTheLoopbackPrefix(t *testing.T) {
	if got := channel.ChatIDPrefix("loopback"); got != "lb_" {
		t.Errorf("ChatIDPrefix(loopback) = %q, want %q", got, "lb_")
	}
	builder := channel.GetBuilder("loopback")
	if builder == nil {
		t.Fatal("no builder registered for loopback")
	}
	ch, err := builder(&config.Config{})
	if err != nil {
		t.Fatalf("builder: %v", err)
	}
	if ch.Name() != "loopback" {
		t.Errorf("Name = %q, want %q", ch.Name(), "loopback")
	}
}
