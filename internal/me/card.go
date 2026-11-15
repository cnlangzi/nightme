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
// maxCardCommand is how long the line of the command in flight is:
// the full text is in the chat's tool line.
const (
	maxCardError   = 300
	maxCardCommand = 300
)

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

// iteration is one pass through a job's step list: the job and the
// round of that job.
type iteration struct {
	job   string
	round int
}

// postCard publishes a card of the run's overview in the chat and
// returns the id of the message it became.
//
// The card records the given iteration: a card of its own belongs to
// the iteration the run enters.
func (s *Service) postCard(ctx context.Context, r *run, it iteration) (string, error) {
	if s.cfg.Operator == nil {
		return "", errors.New("me: no operator: the chat cannot be driven")
	}
	id, err := s.cfg.Operator.PostCard(ctx, r.chatID, s.cardView(r, it))
	if err != nil {
		return "", fmt.Errorf("me: the run card could not be posted: %w", err)
	}
	return id, nil
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
	if err := s.cfg.Operator.UpdateCard(ctx, r.chatID, id, s.cardView(r, r.cardIteration())); err != nil {
		s.logger.Warn("me: the run card could not be updated", "run_id", r.id, "err", err)
	}
}

// switchCard moves the run to the card of the iteration it is in.
//
// Every iteration gets a card of its own. The card of the iteration
// that ended stays in the chat as the record of that iteration: the
// engine clears the round's step records when the next round starts,
// so the finished card cannot be rebuilt afterwards. A run that comes
// back to an iteration goes on with the card that iteration has.
//
// The switch happens before the first command of the iteration is sent
// to the chat, so the card of an iteration stands in front of the
// commands of that iteration.
func (s *Service) switchCard(ctx context.Context, r *run, it iteration) {
	if it.job == "" {
		return
	}
	id, known := r.cardOf(it)
	r.mu.Lock()
	current := r.cardMsgID
	currentIt := iteration{job: r.cardJob, round: r.cardRound}
	r.mu.Unlock()

	switch {
	case known && id != "":
		// The iteration has a card of its own: the run goes back to it.
		if current == id && currentIt == it {
			// The run is already on that card.
			return
		}
		r.setCard(id, it)
		s.updateCard(r)
	case current == "":
		// The run has no card at all: the iteration gets one.
		s.publishCard(ctx, r, it, current)
	case currentIt.job == "":
		// The card the run started with has no iteration of its own:
		// it adopts the one the run enters.
		r.setCard(current, it)
		r.rememberCard(it, current)
		s.updateCard(r)
	default:
		// Another iteration: it gets a card of its own, and the card
		// of the iteration that ended keeps its content.
		s.publishCard(ctx, r, it, current)
	}
}

// publishCard publishes the card of an iteration.
//
// The card of the iteration that ended keeps its content: the run
// does not write to it any more.
func (s *Service) publishCard(ctx context.Context, r *run, it iteration, current string) {
	newID, err := s.postCard(ctx, r, it)
	if err != nil {
		// The card is for people: the work goes on on the card the run
		// has. The chat refused the card, so the iteration that ended
		// has no card of its own — the run says where it is on the
		// card it has.
		s.logger.Warn("me: the run card could not be published", "run_id", r.id, "err", err)
		r.setCard(current, it)
		return
	}
	r.setCard(newID, it)
	r.rememberCard(it, newID)
}

// cardView renders the run's overview for the iteration the card
// records.
func (s *Service) cardView(r *run, it iteration) channel.CardSpec {
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
	r.mu.Unlock()

	lines := make([]string, 0, len(steps)+9)
	// The iteration the card records. A card that has none yet — the
	// one the run started with — does not name one.
	if it.job != "" {
		lines = append(lines, fmt.Sprintf("job: %s · round: %d", it.job, it.round))
	}
	if args != "" {
		lines = append(lines, "args: "+args)
	}
	for i, step := range steps {
		lines = append(lines, fmt.Sprintf("%d. %s %s", i+1, stepIcon(states[step.ID]), stepLabel(step)))
	}

	fields := make([]string, 0, 6)
	if inflight != nil {
		fields = append(fields, clipLine("current: "+inflight.Command, maxCardCommand))
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
