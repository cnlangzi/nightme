package chatsession

import (
	"context"
	"log/slog"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Work tracks the structured outcome of one inbound message.
//
// A message's handling can span several participants: the command
// that ran, and the agent turn it queued. Each participant takes a
// hold and releases it when its part is done. The settled message
// goes out once, when the last hold is released — so the outcome a
// consumer sees is the merge of everything that reported.
//
// A message nobody reported on settles with OutcomeUnknown.
type Work struct {
	cs    *ChatSession
	msgID string

	mu       sync.Mutex
	holds    int
	released bool
	outcome  messages.Outcome
	unsubs   []*unsubOnce
	// anchor is the userMsgID the prompt-end event carries for
	// this message. It equals msgID until the message is merged
	// into a multi-message prompt batch: the prompt anchors on
	// the batch's LAST message, so the earlier messages' anchors
	// move there. Empty means "anchor on msgID".
	anchor string
}

// Hold takes a hold on the outcome of msgID and returns the shared
// Work for it. Repeated calls for the same msgID return the same
// Work and each add a hold.
//
// An empty msgID yields nil — there is nothing to key a settled
// message on.
func (cs *ChatSession) Hold(msgID string) *Work {
	if cs == nil || msgID == "" {
		return nil
	}
	cs.workMu.Lock()
	defer cs.workMu.Unlock()
	if cs.works == nil {
		cs.works = make(map[string]*Work)
	}
	w, ok := cs.works[msgID]
	if !ok {
		w = &Work{cs: cs, msgID: msgID}
		cs.works[msgID] = w
	}
	w.mu.Lock()
	w.holds++
	w.mu.Unlock()
	return w
}

// Report merges an outcome into the work. Reporting after the work
// was released is a no-op — the settled message already went out.
func (w *Work) Report(o messages.Outcome) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.released {
		return
	}
	w.outcome = messages.Merge(w.outcome, o)
}

// Outcome returns the merged outcome reported so far.
func (w *Work) Outcome() messages.Outcome {
	if w == nil {
		return messages.Outcome{Status: messages.OutcomeUnknown}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.outcome
}

// anchorID returns the userMsgID whose prompt-end event ends this
// work. It is the message's own id until the message is merged into a
// multi-message prompt batch, where the prompt anchors on the batch's
// last message.
func (w *Work) anchorID() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.anchor != "" {
		return w.anchor
	}
	return w.msgID
}

// reanchor moves the work's prompt-end trigger to anchorID. A work
// that already settled keeps its trigger — the settled message went
// out, nothing is waiting for it anymore.
func (w *Work) reanchor(anchorID string) {
	if w == nil || anchorID == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.released {
		return
	}
	w.anchor = anchorID
}

// Release drops one hold. When the last hold goes away the settled
// message is emitted once. Releasing more times than held is a
// no-op.
func (w *Work) Release() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.released {
		w.mu.Unlock()
		return
	}
	if w.holds > 0 {
		w.holds--
	}
	if w.holds > 0 {
		w.mu.Unlock()
		return
	}
	w.released = true
	outcome := w.outcome
	unsubs := w.unsubs
	w.unsubs = nil
	w.mu.Unlock()

	for _, u := range unsubs {
		u.call()
	}
	w.cs.settle(w.msgID, outcome)
}

// settle emits the settled message for msgID and drops the work.
func (cs *ChatSession) settle(msgID string, outcome messages.Outcome) {
	cs.workMu.Lock()
	delete(cs.works, msgID)
	cs.workMu.Unlock()

	if outcome.Status == "" {
		outcome.Status = messages.OutcomeUnknown
	}
	if outcome.Summary == "" {
		outcome.Summary = string(outcome.Status)
	}
	// Every IM channel drops the settled message — it exists for
	// programs that drive nightme, not for humans. The log line is
	// the only place a person can see WHEN a message really
	// finished, which is what makes "the command said done but the
	// agent was still working" visible in a local run.
	slog.Info("chatsession: message settled",
		"chat_id", cs.ChatID,
		"message_id", msgID,
		"status", string(outcome.Status),
		"summary", outcome.Summary,
	)

	em := cs.Emitter()
	if em == nil {
		return
	}
	_ = em.Send(context.Background(), messages.OutboundMessage{
		ChatID:  cs.ChatID,
		Kind:    messages.OutMessageSettled,
		ReplyTo: msgID,
		Outcome: &outcome,
	})
}

// HoldUntilPromptEnd takes a hold on msgID and settles it when the
// agent turn carrying that msgID ends. The PromptEnd reason maps
// onto the outcome: a clean end is ok, a user stop or kill is
// cancelled, anything else is failed.
//
// The hold is released when the turn ends. A turn that never ends
// leaves the hold open — the caller waiting on it supplies its own
// timeout.
//
// The trigger is the work's anchor, not msgID itself: a message
// merged into a multi-message prompt batch ends with the batch, and
// the prompt-end event carries the batch's anchor id.
func (cs *ChatSession) HoldUntilPromptEnd(msgID string) *Work {
	w := cs.Hold(msgID)
	if w == nil {
		return nil
	}
	bus := cs.PromptEndBus
	if bus == nil {
		return w
	}
	u := &unsubOnce{}
	w.mu.Lock()
	w.unsubs = append(w.unsubs, u)
	w.mu.Unlock()

	u.set(bus.Subscribe(func(e PromptEndedEvent) bool {
		if e.UserMsgID != w.anchorID() {
			return false
		}
		u.call()
		w.Report(outcomeForPromptEnd(e.Reason))
		w.Release()
		return false
	}))
	return w
}

// reanchorWork moves the work for msgID onto a different prompt-end
// trigger. Used when a message is merged into a multi-message prompt
// batch: the prompt anchors on the batch's last message, so the
// earlier messages must end with it.
func (cs *ChatSession) reanchorWork(msgID, anchorID string) {
	if cs == nil || msgID == "" || anchorID == "" || msgID == anchorID {
		return
	}
	cs.workMu.Lock()
	w := cs.works[msgID]
	cs.workMu.Unlock()
	w.reanchor(anchorID)
}

// outcomeForPromptEnd maps a prompt-end reason onto an outcome.
//
// The summary is empty on purpose: it lets the merge keep the
// summary a command reported (the prompt end is the generic
// fallback, the command's is the specific one).
func outcomeForPromptEnd(reason agent.PromptEndReason) messages.Outcome {
	out := messages.Outcome{Status: messages.OutcomeFailed}
	switch reason {
	case agent.PromptEndClean:
		out.Status = messages.OutcomeOK
	case agent.PromptEndUserStopped, agent.PromptEndUserKilled:
		out.Status = messages.OutcomeCancelled
	}
	out.Data = map[string]string{"end_reason": reason.String()}
	return out
}

// unsubOnce runs an unsubscribe function exactly once, and copes
// with the race between subscribing and the handler firing.
type unsubOnce struct {
	mu   sync.Mutex
	fn   func()
	done bool
}

func (u *unsubOnce) set(fn func()) {
	u.mu.Lock()
	if u.done {
		u.mu.Unlock()
		if fn != nil {
			fn()
		}
		return
	}
	u.fn = fn
	u.mu.Unlock()
}

func (u *unsubOnce) call() {
	u.mu.Lock()
	fn := u.fn
	u.done = true
	u.fn = nil
	u.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// workCtxKey keys the Work on a context.
type workCtxKey struct{}

// WithWork returns a context carrying the Work for the inbound
// message being handled. Commands report through it with
// ReportOutcome.
func WithWork(ctx context.Context, w *Work) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if w == nil {
		return ctx
	}
	return context.WithValue(ctx, workCtxKey{}, w)
}

// ReportOutcome merges an outcome into the Work carried by ctx.
// It is a no-op when ctx carries no Work — a command invoked
// outside the inbound dispatch path has nothing to report to.
func ReportOutcome(ctx context.Context, o messages.Outcome) {
	if ctx == nil {
		return
	}
	w, _ := ctx.Value(workCtxKey{}).(*Work)
	if w == nil {
		return
	}
	w.Report(o)
}
