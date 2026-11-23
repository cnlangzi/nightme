// Package observe — the side channel that reports what the daemon
// said about the messages a program sent.
//
// A program that drives the daemon (the personal agent, internal/me)
// needs to know how the message it sent is going: was it picked up,
// is it working, is it waiting for an answer, what did it come back
// with.
//
// The tracker correlates the daemon's outbound messages with the
// inbound message that caused them. The correlation key is ReplyTo —
// the id of the inbound message, which the daemon stamps on every
// message it produces for that turn.
//
// Only the messages a program registered are tracked. A message a
// person sent is not attributed to anything: the tracker holds no
// record of it, so nothing the daemon says about it lands in a
// program's execution.
package observe

import (
	"context"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

const (
	// MaxTranscriptEntries caps the transcript of one execution.
	// The head and the tail are kept; the middle is dropped.
	MaxTranscriptEntries = 200

	// transcriptHead / transcriptTail split the kept part of an
	// over-long transcript.
	transcriptHead = MaxTranscriptEntries / 2
	transcriptTail = MaxTranscriptEntries - transcriptHead

	// executionTTL is how long a settled execution is kept after it
	// settled, for a driver that never read it.
	executionTTL = 10 * time.Minute

	// unsettledTTL is how long an execution that never settled is
	// kept. A message whose settle never arrives (the bridge died,
	// the daemon dropped the message) would otherwise stay in the
	// store for the life of the process.
	unsettledTTL = 24 * time.Hour

	// watchBuffer is the depth of one watcher's snapshot queue. A
	// watcher that stops reading loses snapshots — the daemon's
	// outbound path must never block on a slow reader.
	watchBuffer = 16
)

// Observer is told about every inbound message before it is
// dispatched.
//
// The gateway calls it for every message that arrives, in every
// channel, before the dispatch chain sees it. An observer that does
// not want a message ignores it.
type Observer interface {
	Seen(msg *messages.InboundMessage)
}

// Observers fans one message out to several observers, in order.
//
// A nil observer is skipped, so a caller can pass an optional
// observer without checking it first.
func Observers(list ...Observer) Observer {
	out := make([]Observer, 0, len(list))
	for _, o := range list {
		if o == nil {
			continue
		}
		out = append(out, o)
	}
	return observers(out)
}

type observers []Observer

func (o observers) Seen(msg *messages.InboundMessage) {
	for _, obs := range o {
		obs.Seen(msg)
	}
}

// Tracker holds the executions of the messages a program registered.
//
// A tracker is safe for concurrent use: the daemon observes on its
// own goroutines while the program reads from another one.
type Tracker struct {
	mu sync.Mutex
	// execs holds the live executions, keyed by message id.
	execs map[string]*execution
	// inflight names the execution a message without a ReplyTo
	// belongs to, per chat: the most recent one that has not settled.
	inflight map[string]string
	// now is the clock. Nil means time.Now.
	now func() time.Time
}

// NewTracker constructs a Tracker.
func NewTracker() *Tracker {
	return &Tracker{
		execs:    map[string]*execution{},
		inflight: map[string]string{},
	}
}

// Register starts tracking a message the program sent.
//
// It is idempotent: a message that is already tracked keeps the
// state it has. The daemon may already have answered the message by
// the time the program registers it, and re-registering would throw
// that answer away.
func (t *Tracker) Register(msgID, chatID, text string) {
	if msgID == "" {
		return
	}
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.execs[msgID]; ok {
		return
	}
	t.sweepLocked(now)
	t.execs[msgID] = &execution{
		msgID:  msgID,
		chatID: chatID,
		text:   text,
		phase:  PhaseSent,
		sentAt: now,
	}
	t.inflight[chatID] = msgID
}

// Seen implements Observer.
//
// A message the personal agent sent is registered here: the gateway
// calls this before the message is dispatched, so the tracker knows
// about the message before the daemon says anything about it.
func (t *Tracker) Seen(msg *messages.InboundMessage) {
	if msg == nil || msg.Origin != messages.OriginMe {
		return
	}
	t.Register(msg.MessageID, msg.ChatID, msg.Text)
}

// Get returns the execution of a message, without consuming it.
func (t *Tracker) Get(msgID string) (Execution, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked(t.clock())
	ex, ok := t.execs[msgID]
	if !ok {
		return Execution{}, false
	}
	return ex.snapshot(), true
}

// Watch returns a channel of snapshots of an execution.
//
// The watcher starts from the state the execution is in now, so it
// never has to wait for the next change to learn where the run
// stands. A settled execution has nothing left to report: the
// watcher gets the final snapshot and the channel closes.
func (t *Tracker) Watch(msgID string) <-chan Execution {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked(t.clock())

	ch := make(chan Execution, watchBuffer)
	ex, ok := t.execs[msgID]
	if !ok {
		close(ch)
		return ch
	}
	snap := ex.snapshot()
	if ex.phase == PhaseSettled {
		ch <- snap
		close(ch)
		return ch
	}
	ex.watchers = append(ex.watchers, ch)
	ch <- snap
	return ch
}

// Observe folds one outbound message into the execution it belongs to.
//
// A message that names no execution is dropped: it belongs to a
// message the program did not send — a person's turn, or a message
// from another program.
func (t *Tracker) Observe(msg messages.OutboundMessage) {
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked(now)

	ex := t.targetLocked(msg)
	if ex == nil {
		return
	}
	changed, settled := t.applyLocked(ex, msg, now)
	if changed {
		t.notifyLocked(ex)
	}
	if settled {
		t.closeWatchersLocked(ex)
	}
}

// targetLocked picks the execution an outbound message belongs to.
//
// A message that carries a ReplyTo names its execution outright; a
// message without one belongs to the chat's in-flight execution —
// but only when the program has one there. A ReplyTo that names no
// live execution — or one from another chat — is not the program's
// business and is dropped.
func (t *Tracker) targetLocked(msg messages.OutboundMessage) *execution {
	if msg.ReplyTo != "" {
		ex, ok := t.execs[msg.ReplyTo]
		if !ok || ex.chatID != msg.ChatID {
			return nil
		}
		return ex
	}
	if msg.Kind != messages.OutCommandReply {
		return nil
	}
	id, ok := t.inflight[msg.ChatID]
	if !ok {
		return nil
	}
	ex, ok := t.execs[id]
	if !ok {
		return nil
	}
	return ex
}

// applyLocked folds one outbound message into an execution. It
// reports whether the execution changed and whether it settled.
func (t *Tracker) applyLocked(ex *execution, msg messages.OutboundMessage, now time.Time) (changed, settled bool) {
	switch msg.Kind {
	case messages.OutMessageState:
		if msg.MessageState == nil {
			return false, false
		}
		switch msg.MessageState.State {
		case agent.MessageQueued:
			changed = ex.advance(PhaseQueued, now)
		case agent.MessageSubmitted:
			changed = ex.advance(PhaseRunning, now)
		}

	case messages.OutCommandReply:
		// The command answered. The first answer means the message
		// was picked up; a later one (a command that answers after
		// its agent turn) does not move the phase backwards.
		changed = ex.advance(PhaseQueued, now)
		if ex.appendText(msg.Kind, msg.Text, now) {
			changed = true
		}

	case messages.OutReply, messages.OutResult, messages.OutError:
		changed = ex.advance(PhaseRunning, now)
		if ex.appendText(msg.Kind, msg.Text, now) {
			changed = true
		}

	case messages.OutThinking, messages.OutToolStart, messages.OutToolEnd,
		messages.OutTaskCreate, messages.OutTaskUpdate:
		changed = ex.advance(PhaseRunning, now)

	case messages.OutHeartbeat:
		changed = ex.advance(PhaseRunning, now)
		if msg.Heartbeat != nil {
			ex.progress = Progress{
				ThinkCount: msg.Heartbeat.ThinkCount,
				ToolCount:  msg.Heartbeat.ToolCount,
				LastBeatAt: msg.Heartbeat.LastBeatAt,
			}
			changed = true
		}

	case messages.OutChoice:
		changed = ex.advance(PhaseRunning, now)
		if msg.Choice != nil {
			ex.choices = addChoice(ex.choices, *msg.Choice)
			changed = true
		}

	case messages.OutChoicePatch:
		if msg.Choice != nil {
			var patched bool
			ex.choices, patched = patchChoice(ex.choices, *msg.Choice)
			changed = patched
		}

	case messages.OutMessageSettled:
		return t.settleLocked(ex, msg, now)
	}
	return changed, false
}

// settleLocked completes an execution: the phase moves to settled,
// the outcome is copied over, and the waiters are released.
func (t *Tracker) settleLocked(ex *execution, msg messages.OutboundMessage, now time.Time) (changed, settled bool) {
	if ex.phase == PhaseSettled {
		return false, false
	}
	ex.phase = PhaseSettled
	ex.settledAt = now
	if msg.Outcome != nil {
		ex.outcome = *msg.Outcome
	}
	if t.inflight[ex.chatID] == ex.msgID {
		delete(t.inflight, ex.chatID)
	}
	return true, true
}

// addChoice records a pending interactive prompt. A prompt that is
// already known is replaced, so a re-render does not duplicate it.
func addChoice(choices []messages.Choice, choice messages.Choice) []messages.Choice {
	for i := range choices {
		if choices[i].RequestID != "" && choices[i].RequestID == choice.RequestID {
			choices[i] = choice
			return choices
		}
	}
	return append(choices, choice)
}

// patchChoice folds an update of an interactive prompt into the
// pending list.
//
// A patch that names its prompt updates that prompt. A settled
// prompt is answered — it is no longer pending.
//
// A patch that names none is the daemon settling a gate it resolved
// outside the channel (the dashboard): the run has one outstanding
// prompt, and it is answered, so nothing is pending any more.
func patchChoice(choices []messages.Choice, patch messages.Choice) ([]messages.Choice, bool) {
	if patch.RequestID == "" {
		if !patch.Settled || len(choices) == 0 {
			return choices, false
		}
		return nil, true
	}
	for i := range choices {
		if choices[i].RequestID != patch.RequestID {
			continue
		}
		if patch.Settled {
			return append(choices[:i], choices[i+1:]...), true
		}
		choices[i] = patch
		return choices, true
	}
	if patch.Settled {
		return choices, false
	}
	return append(choices, patch), true
}

// notifyLocked hands a fresh snapshot to every watcher.
func (t *Tracker) notifyLocked(ex *execution) {
	if len(ex.watchers) == 0 {
		return
	}
	snap := ex.snapshot()
	for _, w := range ex.watchers {
		select {
		case w <- snap:
		default:
			// The watcher is not keeping up. Dropping a progress
			// snapshot is better than stalling the daemon's outbound
			// path.
		}
	}
}

// closeWatchersLocked ends every watch of a settled execution.
func (t *Tracker) closeWatchersLocked(ex *execution) {
	for _, w := range ex.watchers {
		close(w)
	}
	ex.watchers = nil
}

// sweepLocked drops the executions nobody needs any more: the ones
// that settled long ago, and the ones that never settled at all.
//
// The sweep runs on every access to the store, so an execution is
// reaped as soon as anything touches the tracker again.
func (t *Tracker) sweepLocked(now time.Time) {
	for id, ex := range t.execs {
		if ex.phase == PhaseSettled {
			if now.Sub(ex.settledAt) < executionTTL {
				continue
			}
		} else if now.Sub(ex.sentAt) < unsettledTTL {
			continue
		}
		delete(t.execs, id)
		if t.inflight[ex.chatID] == ex.msgID {
			delete(t.inflight, ex.chatID)
		}
	}
}

// clock returns the tracker's clock.
func (t *Tracker) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// Tap wraps an emitter so the tracker sees what the daemon says.
//
// The message is forwarded unchanged first; the tracker folds it in
// afterwards. A message the tracker does not know is one map lookup
// and nothing more.
func Tap(em messages.Emitter, t *Tracker) messages.Emitter {
	if em == nil || t == nil {
		return em
	}
	return &tapEmitter{inner: em, tracker: t}
}

type tapEmitter struct {
	inner   messages.Emitter
	tracker *Tracker
}

func (e *tapEmitter) Send(ctx context.Context, msg messages.OutboundMessage) (string, error) {
	id, err := e.inner.Send(ctx, msg)
	e.tracker.Observe(msg)
	return id, err
}
