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
// `/me continue` hands the control back.
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
	r.requestPause("you took over — `/me continue` hands the control back")
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
