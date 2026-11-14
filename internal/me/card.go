package me

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// The commands the run card's buttons stand for.
//
// A button carries the text of the command it runs, so a click
// travels the same path a typed command takes.
const (
	cardActionPause    = "pause"
	cardActionContinue = "continue"
	cardActionStop     = "stop"
	// cardActionReply answers the question the run is asking.
	cardActionReply = "reply"
)

// speak says something in the chat the run works in.
//
// The chat shows the bot mark in front of the text; the daemon is
// given the text itself, so the mark never reaches the agent.
func (s *Service) speak(ctx context.Context, r *run, text, replyTo string) (string, error) {
	if s.cfg.Operator == nil {
		return "", errors.New("me: no operator: the chat cannot be spoken in")
	}
	return s.cfg.Operator.Speak(ctx, r.chatID, text, channel.SpeakOptions{
		ReplyTo: replyTo,
		Display: BotMark + text,
		Owner:   r.owner,
		RunID:   r.id,
	})
}

// speakQuestion asks the operator something in the chat.
//
// The question is a reply to the run's overview, so the answer comes
// back the way a person's reply comes back.
func (s *Service) speakQuestion(ctx context.Context, r *run, question string) (string, error) {
	return s.speak(ctx, r, "❓ "+question, r.cardMessageID())
}

// postCard publishes the run's overview in the chat.
func (s *Service) postCard(ctx context.Context, r *run) error {
	if s.cfg.Operator == nil {
		return errors.New("me: no operator: the chat cannot be driven")
	}
	id, err := s.cfg.Operator.PostCard(ctx, r.chatID, s.cardView(r))
	if err != nil {
		return fmt.Errorf("me: the run card could not be posted: %w", err)
	}
	r.setCardMessageID(id)
	return nil
}

// updateCard republishes the run's overview.
//
// A run without a card — a daemon that never posted one — simply has
// no overview to update.
func (s *Service) updateCard(r *run) {
	if s.cfg.Operator == nil {
		return
	}
	id := r.cardMessageID()
	if id == "" {
		return
	}
	// The run's own context is cancelled when the run ends, and the
	// last update — the summary — happens after that. The context
	// outlives the run here on purpose.
	ctx := context.WithoutCancel(r.ctx)
	if err := s.cfg.Operator.UpdateCard(ctx, r.chatID, id, s.cardView(r)); err != nil {
		s.logger.Warn("me: the run card could not be updated", "run_id", r.id, "err", err)
	}
}

// cardView renders the run's overview.
func (s *Service) cardView(r *run) channel.CardSpec {
	r.mu.Lock()
	steps := r.workflow.Steps()
	states := make(map[string]wfe.StepState, len(r.state.Steps))
	for _, st := range r.state.Steps {
		states[st.ID] = st
	}
	workflow := r.workflow.Name
	phase := r.phase
	inflight := r.inflight
	question := r.question
	judgment := r.judgment
	decision := r.lastDecision
	reason := r.lastReason
	note := r.lastNote
	summary := r.summary
	paused := r.paused
	pausePending := r.pauseRequested
	pauseReason := r.pauseReason
	actions := len(r.policy.History)
	s1d, s2d := r.system1Decisions, r.system2Decisions
	s1u, s2u := r.system1Usage.Total(), r.system2Usage.Total()
	args := argLine(r.argNames, r.argVals)
	options := askOptions(r.ask)
	r.mu.Unlock()

	var b strings.Builder
	if args != "" {
		b.WriteString("args: ")
		b.WriteString(args)
		b.WriteString("\n\n")
	}
	for i, step := range steps {
		b.WriteString(fmt.Sprintf("%d. %s %s", i+1, stepIcon(states[step.ID]), stepLabel(step)))
		b.WriteString("\n")
	}
	if inflight != nil {
		b.WriteString("\ncurrent: ")
		b.WriteString(inflight.Command)
		b.WriteString("\n")
	}
	if question != "" {
		b.WriteString("question: ")
		b.WriteString(question)
		b.WriteString("\n")
	}
	if decision != "" {
		b.WriteString("decision: ")
		b.WriteString(decision)
		if reason != "" {
			b.WriteString(" — ")
			b.WriteString(reason)
		}
		b.WriteString("\n")
	}
	if judgment != "" {
		b.WriteString("blocked: ")
		b.WriteString(judgment)
		b.WriteString("\n")
	}
	switch {
	case paused:
		b.WriteString("paused: ")
		if pauseReason != "" {
			b.WriteString(pauseReason)
		} else {
			b.WriteString("the run waits for the operator")
		}
		b.WriteString("\n")
	case pausePending:
		b.WriteString("pausing: the run stops after the current step\n")
	}
	if note != "" {
		b.WriteString("note: ")
		b.WriteString(note)
		b.WriteString("\n")
	}
	if summary != "" {
		b.WriteString("result: ")
		b.WriteString(summary)
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("actions: %d · system-1: %d decisions, %d tokens · system-2: %d decisions, %d tokens",
		actions, s1d, s1u, s2d, s2u))

	title := workflow + " · " + string(phase)
	buttons := []channel.CardButton{
		{Label: "暂停", Action: cardActionPause},
		{Label: "继续", Action: cardActionContinue},
		{Label: "停止", Action: cardActionStop},
	}
	if len(options) > 0 {
		// The run is waiting for an answer: the options become the
		// card's buttons, so a click is the answer.
		buttons = nil
		for _, option := range options {
			buttons = append(buttons, channel.CardButton{
				Label:  option.Value,
				Action: cardActionReply + " " + option.Value,
			})
		}
		buttons = append(buttons, channel.CardButton{Label: "停止", Action: cardActionStop})
	}

	return channel.CardSpec{
		Title:   title,
		Body:    strings.TrimRight(b.String(), "\n"),
		Buttons: buttons,
	}
}

// askOptions returns the choices the run is waiting for.
//
// "escalate" is a word for the decision model, not for the operator:
// it is left out of the buttons.
func askOptions(ask *askState) []llm.Option {
	if ask == nil {
		return nil
	}
	out := make([]llm.Option, 0, len(ask.Options))
	for _, option := range ask.Options {
		if option.Value == optionEscalate {
			continue
		}
		out = append(out, option)
	}
	return out
}

// stepLabel renders what a step does.
func stepLabel(step wfe.Step) string {
	switch step.Kind() {
	case wfe.StepKindMessage:
		return step.Message
	case wfe.StepKindShell:
		return "(shell) " + step.Shell
	case wfe.StepKindUse:
		return "(use) " + step.Use
	case wfe.StepKindWait:
		return "(wait) " + step.Wait
	}
	return step.ID
}

// stepIcon renders the state of a step.
func stepIcon(st wfe.StepState) string {
	if st.Skipped {
		return "⏭"
	}
	if st.Pending {
		return "🔄"
	}
	switch st.Outcome.Status {
	case wfe.OutcomeOK, wfe.OutcomeNoop:
		return "✅"
	case wfe.OutcomeFailed:
		return "❌"
	case wfe.OutcomeCancelled:
		return "⏹"
	case wfe.OutcomeTimeout:
		return "⏱"
	case wfe.OutcomeIncomplete:
		return "⚠️"
	}
	return "⏳"
}

// argLine renders the workflow's arguments.
func argLine(names []string, values map[string]string) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+values[name])
	}
	return strings.Join(parts, " ")
}
