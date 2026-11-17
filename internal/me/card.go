package me

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
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
// The question is a reply to the run's card, so the answer comes back
// the way a person's reply comes back.
//
// The question is not dispatched: it belongs to the run, not to the
// agent. The agent never sees what the run asked.
func (s *Service) speakQuestion(ctx context.Context, r *run, question string) (string, error) {
	if s.cfg.Operator == nil {
		return "", errors.New("me: no operator: the chat cannot be spoken in")
	}
	replyTo := ""
	if sess := r.runSessionRef(); sess != nil {
		replyTo = sess.cardID
	}
	return s.cfg.Operator.Ask(ctx, r.chatID, "❓ "+question, channel.SpeakOptions{
		ReplyTo: replyTo,
		Display: BotMark + "❓ " + question,
		Owner:   r.owner,
		RunID:   r.id,
	})
}

// askQuestion asks the operator something in the chat.
//
// The question is a message of the chat, and the choices are the
// chat's own choice card: a click on an option is the answer.
//
// The answer is claimed by the run before the agent sees it, and the
// agent never sees the question at all.
func (s *Service) askQuestion(ctx context.Context, r *run, question string, options []llm.Option) (string, error) {
	msgID, err := s.speakQuestion(ctx, r, question)
	if err != nil {
		return "", err
	}
	requestID := r.nextQuestionRequestID()
	r.mu.Lock()
	if r.ask != nil {
		r.ask.RequestID = requestID
	}
	r.mu.Unlock()
	s.emit(ctx, r, r.runSessionRef(), messages.OutboundMessage{
		Kind: messages.OutChoice,
		Choice: &messages.Choice{
			RequestID: requestID,
			Kind:      messages.ChoiceKindQuestion,
			Title:     "The run needs you",
			Body:      question,
			Options:   choiceOptions(options),
		},
	})
	return msgID, nil
}

// askOptions returns the choices the run is waiting for.
//
// "escalate" is a word for the decision model, not for the operator:
// it is left out of the choices the operator may pick. A choice
// without a name is no choice either.
func askOptions(ask *askState) []llm.Option {
	if ask == nil {
		return nil
	}
	return pickableOptions(ask.Options)
}

// pickableOptions returns the choices the operator may pick.
func pickableOptions(options []llm.Option) []llm.Option {
	out := make([]llm.Option, 0, len(options))
	for _, option := range options {
		if option.Value == optionEscalate || strings.TrimSpace(option.Value) == "" {
			continue
		}
		out = append(out, option)
	}
	return out
}

// argLine renders the workflow's arguments.
func argLine(names []string, values map[string]string) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+values[name])
	}
	return strings.Join(parts, " ")
}

// choiceOptions renders the choices the operator may pick.
//
// A choice without a name is not a choice: the operator would not
// know what to pick. "escalate" is a word for the decision model, not
// for the operator: it is left out of the choices.
func choiceOptions(options []llm.Option) []messages.ChoiceOption {
	pickable := pickableOptions(options)
	out := make([]messages.ChoiceOption, 0, len(pickable))
	for i, option := range pickable {
		out = append(out, messages.ChoiceOption{
			ID:    fmt.Sprintf("%d", i),
			Label: strings.TrimSpace(option.Value),
		})
	}
	return out
}

// iteration is one pass through a job's step list: the job and the
// round of that job.
type iteration struct {
	job   string
	round int
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
