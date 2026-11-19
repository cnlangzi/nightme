package me

import (
	"context"
	"strings"

	"github.com/cnlangzi/nightme/internal/messages"
)

// meCommandPrefix is the command that steers a run.
const meCommandPrefix = "/me "

// Seen implements the gateway's inbound observer.
//
// A message a person sent while the run is going means the person
// wants the chat back: the run stops after the step it is on, and
// `/me resume` hands the control back. The person took the chat back,
// so the state of the run is published at once: it hangs under the
// card of the round, where the operator is looking.
//
// A reply to the run's own question is an answer, not a takeover. A
// command that steers the run is not a takeover either.
func (s *Service) Seen(msg *messages.InboundMessage) {
	if msg == nil || msg.Origin != messages.OriginHuman {
		return
	}
	s.mu.Lock()
	r, ok := s.active[msg.ChatID]
	s.mu.Unlock()
	if !ok {
		return
	}
	if r.isAnswerTo(msg) {
		return
	}
	if isMeCommand(msg.Text) {
		return
	}
	if r.pausePending() {
		// The run already waits for the operator: the takeover is not
		// new, and no second state is published.
		return
	}
	r.requestPause("you took over — `/me resume` hands the control back")
	s.postState(r)
}

// postState publishes the state of the run on a goroutine of its own.
//
// The inbound pump must stay free for the operator's next message: a
// `/me resume` may not queue behind a slow chat API call.
//
// The goroutine is tracked by the service's wait group, so a shutdown
// waits for the post to finish. The service's own lock guards the
// bookkeeping: a post that starts after the service went down is
// dropped rather than added to a wait group that is already waiting.
func (s *Service) postState(r *run) {
	s.mu.Lock()
	if !s.started || s.active[r.chatID] != r {
		// The daemon is going down, or the run is over: there is no
		// state left to show.
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		// The run may have ended while the state was on its way: a
		// state that says the run is working while it is over would
		// lie.
		if r.ended() {
			return
		}
		s.showState(context.WithoutCancel(r.ctx), r, r.stateAnchor(""))
	}()
}

// Claim implements the inbound router's reply claimer.
//
// A message that answers the run's question belongs to the run, not to
// the agent: it is the operator's answer, and the agent never sees
// it.
func (s *Service) Claim(ctx context.Context, msg *messages.InboundMessage) bool {
	if msg == nil || msg.Origin != messages.OriginHuman {
		return false
	}
	s.mu.Lock()
	r, ok := s.active[msg.ChatID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	if !r.isAnswerTo(msg) {
		return false
	}
	return r.deliver(msg.Text) == nil
}

// isMeCommand reports whether the text is a command that steers a run.
func isMeCommand(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), meCommandPrefix)
}
