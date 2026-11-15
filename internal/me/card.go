package me

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// The commands the run card's buttons stand for.
//
// A button carries the text of the command it runs, so a click
// travels the same path a typed command takes.
// maxCardError is how much of an error the run card shows.
const maxCardError = 300

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
//
// The question is not dispatched: it belongs to the run, not to the
// agent. The agent never sees what the run asked.
func (s *Service) speakQuestion(ctx context.Context, r *run, question string) (string, error) {
	if s.cfg.Operator == nil {
		return "", errors.New("me: no operator: the chat cannot be spoken in")
	}
	return s.cfg.Operator.Ask(ctx, r.chatID, "❓ "+question, channel.SpeakOptions{
		ReplyTo: r.cardMessageID(),
		Display: BotMark + "❓ " + question,
		Owner:   r.owner,
		RunID:   r.id,
	})
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

// switchCard moves the run's card to the iteration the run is in.
//
// The card of the iteration that ended stays in the chat as the record
// of that iteration: the engine clears the round's step records when
// the next round starts, so the finished card cannot be rebuilt
// afterwards.
//
// The switch happens before the first command of the iteration is sent
// to the chat, so the card of an iteration stands in front of the
// commands of that iteration.
func (s *Service) switchCard(ctx context.Context, r *run, job string, round int) {
	if job == "" {
		return
	}
	r.mu.Lock()
	same := r.cardJob == job && r.cardRound == round
	adopted := r.cardJob == ""
	r.mu.Unlock()
	if same {
		// The card already records this iteration: it is updated in
		// place.
		return
	}
	r.setCardIteration(job, round)
	if adopted {
		// The card the run started with has no iteration of its own:
		// it adopts the one the run entered.
		s.updateCard(r)
		return
	}
	if err := s.postCard(ctx, r); err != nil {
		// The card is for people: the work goes on without it.
		s.logger.Warn("me: the run card could not be published", "run_id", r.id, "err", err)
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
	errMsg := r.errMsg
	paused := r.paused
	pausePending := r.pauseRequested
	pauseReason := r.pauseReason
	actions := len(r.policy.History)
	s1d, s2d := r.system1Decisions, r.system2Decisions
	s1u, s2u := r.system1Usage.Total(), r.system2Usage.Total()
	args := argLine(r.argNames, r.argVals)
	options := askOptions(r.ask)
	cardJob, cardRound := r.cardJob, r.cardRound
	r.mu.Unlock()

	lines := make([]string, 0, len(steps)+9)
	// The iteration the card records. A card that has none yet — the
	// one the run started with — does not name one.
	if cardJob != "" {
		lines = append(lines, fmt.Sprintf("job: %s · round: %d", cardJob, cardRound))
	}
	if args != "" {
		lines = append(lines, "args: "+args)
	}
	for i, step := range steps {
		lines = append(lines, fmt.Sprintf("%d. %s %s", i+1, stepIcon(states[step.ID]), stepLabel(step)))
	}

	fields := make([]string, 0, 6)
	if inflight != nil {
		fields = append(fields, "current: "+inflight.Command)
	}
	if question != "" {
		fields = append(fields, "question: "+question)
	}
	if decision != "" {
		line := "decision: " + decision
		if reason != "" {
			line += " — " + reason
		}
		fields = append(fields, line)
	}
	if judgment != "" {
		fields = append(fields, "blocked: "+judgment)
	}
	switch {
	case paused:
		line := "paused: the run waits for the operator"
		if pauseReason != "" {
			line = "paused: " + pauseReason
		}
		fields = append(fields, line)
	case pausePending:
		fields = append(fields, "pausing: the run stops after the current step")
	}
	if note != "" {
		fields = append(fields, "note: "+note)
	}
	// The error stands on its own line: it is why the run stopped,
	// and it must not read as another field of the run.
	if errMsg != "" {
		fields = append(fields, "error: "+truncate(errMsg, maxCardError))
	}
	if summary != "" {
		fields = append(fields, "result: "+summary)
	}
	if len(fields) > 0 {
		lines = append(lines, "")
		lines = append(lines, fields...)
	}
	lines = append(lines, fmt.Sprintf("actions: %d · system-1: %d decisions, %d tokens · system-2: %d decisions, %d tokens",
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
		//
		// The button carries the number of the option, not its text:
		// a chat button has room for a few bytes only, and the text
		// of an option can be long.
		buttons = nil
		for i, option := range options {
			buttons = append(buttons, channel.CardButton{
				Label:  option.Value,
				Action: cardActionReply + " " + strconv.Itoa(i),
			})
		}
		buttons = append(buttons, channel.CardButton{Label: "停止", Action: cardActionStop})
	}

	return channel.CardSpec{
		Title:   title,
		Body:    strings.Join(lines, "\n"),
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
