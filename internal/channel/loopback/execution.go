package loopback

import (
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// Phase is the lifecycle stage of one tracked execution.
//
// The stages only ever move forward: a late command reply cannot
// pull a running execution back to "queued", and a late heartbeat
// cannot un-settle it.
type Phase string

const (
	// PhaseSent: the command was handed to the daemon; nothing has
	// come back yet.
	PhaseSent Phase = "sent"

	// PhaseQueued: the daemon accepted the message — it published a
	// queued message state, or it answered with a command reply.
	PhaseQueued Phase = "queued"

	// PhaseRunning: an agent turn is producing output — the message
	// was submitted, a heartbeat arrived, or the agent said
	// something.
	PhaseRunning Phase = "running"

	// PhaseSettled: the daemon reported a structured outcome for
	// the message. The execution is complete.
	PhaseSettled Phase = "settled"
)

// phaseRank orders the phases so a transition can be compared with
// the current one.
func phaseRank(p Phase) int {
	switch p {
	case PhaseSent:
		return 0
	case PhaseQueued:
		return 1
	case PhaseRunning:
		return 2
	case PhaseSettled:
		return 3
	}
	return 0
}

// Progress is the heartbeat view of one execution: how much the agent
// thought, how many tools it ran, and when it last showed a sign of
// life. A driver that sees the beat time stop moving knows the turn
// is stalled.
type Progress struct {
	ThinkCount int
	ToolCount  int
	LastBeatAt time.Time
}

// Entry is one line of an execution's transcript — the text the
// daemon produced for that message, in the order it arrived.
type Entry struct {
	// Kind is the outbound kind the text came from.
	Kind messages.OutboundKind
	// Text is the rendered text of that message.
	Text string
	// At is when the message reached the channel.
	At time.Time
}

// Execution is the tracked state of one command the loopback sent.
//
// It is a snapshot: reading it never observes a half-written state,
// and the slices it carries are copies.
type Execution struct {
	// MsgID is the id of the inbound message this execution
	// tracks. It is the correlation key the daemon's outbound
	// messages carry in ReplyTo.
	MsgID string
	// ChatID is the chat the command was sent to.
	ChatID string
	// Text is the command text that was sent.
	Text string

	// Phase is where the execution currently stands.
	Phase Phase

	// SentAt is when the message was handed to the daemon.
	SentAt time.Time
	// StartedAt is when the agent turn started producing output.
	StartedAt time.Time
	// SettledAt is when the daemon reported the outcome.
	SettledAt time.Time

	// Outcome is the structured verdict. It stays unknown until the
	// execution settles.
	Outcome messages.Outcome
	// Progress is the heartbeat view of the execution.
	Progress Progress
	// Transcript holds the text the daemon produced for this
	// message, in order.
	Transcript []Entry
	// Choices holds the interactive prompts (permission, question)
	// the run still has to answer.
	Choices []messages.Choice
}

// Final returns the text of the last result in the transcript, or the
// last transcript text when the run produced no result.
//
// A command that answered without a result (a slash command, an
// error) still has text to report, so the fallback is the last thing
// the daemon said.
func (e Execution) Final() string {
	for i := len(e.Transcript) - 1; i >= 0; i-- {
		if e.Transcript[i].Kind == messages.OutResult {
			return e.Transcript[i].Text
		}
	}
	if n := len(e.Transcript); n > 0 {
		return e.Transcript[n-1].Text
	}
	return ""
}

// execution is the live state of one tracked command. The public
// Execution is the snapshot of it.
type execution struct {
	msgID  string
	chatID string
	text   string

	phase     Phase
	sentAt    time.Time
	startedAt time.Time
	settledAt time.Time

	outcome    messages.Outcome
	progress   Progress
	transcript []Entry
	choices    []messages.Choice

	// settledCh is closed when the execution settles. Await waits
	// on it, so a settle that lands before anybody waits is never
	// lost.
	settledCh chan struct{}

	// watchers receive a snapshot whenever the execution moves.
	watchers []chan Execution
}

// advance moves the execution to a later phase. Transitions that
// would move it backwards are ignored.
func (e *execution) advance(to Phase, now time.Time) bool {
	if phaseRank(to) <= phaseRank(e.phase) {
		return false
	}
	e.phase = to
	if to == PhaseRunning && e.startedAt.IsZero() {
		e.startedAt = now
	}
	return true
}

// appendText records one transcript line. An empty text is not
// recorded, except for a result — "the run finished and said
// nothing" is a fact worth keeping.
func (e *execution) appendText(kind messages.OutboundKind, text string, at time.Time) bool {
	if text == "" && kind != messages.OutResult {
		return false
	}
	e.transcript = append(e.transcript, Entry{Kind: kind, Text: text, At: at})
	e.transcript = trimTranscript(e.transcript)
	return true
}

// trimTranscript keeps the transcript within MaxTranscriptEntries,
// preserving the head and the tail: the opening lines say what was
// asked, the closing lines say how it ended.
func trimTranscript(t []Entry) []Entry {
	if len(t) <= MaxTranscriptEntries {
		return t
	}
	head := t[:transcriptHead]
	tail := t[len(t)-transcriptTail:]
	merged := make([]Entry, 0, MaxTranscriptEntries)
	merged = append(merged, head...)
	merged = append(merged, tail...)
	return merged
}

// snapshot copies the live state into the public shape.
func (e *execution) snapshot() Execution {
	out := Execution{
		MsgID:     e.msgID,
		ChatID:    e.chatID,
		Text:      e.text,
		Phase:     e.phase,
		SentAt:    e.sentAt,
		StartedAt: e.startedAt,
		SettledAt: e.settledAt,
		Outcome:   e.outcome,
		Progress:  e.progress,
	}
	if e.outcome.Data != nil {
		out.Outcome.Data = make(map[string]string, len(e.outcome.Data))
		for k, v := range e.outcome.Data {
			out.Outcome.Data[k] = v
		}
	}
	if len(e.transcript) > 0 {
		out.Transcript = append([]Entry(nil), e.transcript...)
	}
	if len(e.choices) > 0 {
		out.Choices = append([]messages.Choice(nil), e.choices...)
	}
	return out
}
