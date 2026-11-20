// session.go — the agent sessions the personal agent runs.
//
// A run speaks in four kinds of session. Every session hangs under one
// message: the anchor of every event the session sends. The chat
// renders the events of a session onto the card of that message — the
// way it renders a coding agent's turn.
//
// The anchor of a session the run opens itself is a message the run
// posted: the run puts a placeholder in the chat and the chat renders
// the session onto the card of that placeholder.
//
// The run's own session is the exception: it posts no message. Its
// anchor is the message that started the run, and the chat folds the
// run's entries into the card it already holds for that message.
//
// The state of the run is a card of its own when the run reports it:
// the run posts a placeholder for it and the chat renders the state
// onto the card of that message. The card of the state is the latest
// card of the round: the run goes on with it. The state the operator
// asks for lands on the card of the command's own message instead.
//
// The package speaks the standard outbound vocabulary
// (internal/messages), and the chat renders it. The blocks the run
// publishes itself — the overview of the run and the block that names
// the run a round belongs to — are entries of a card, and the chat
// renders them as such.
package me

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// sessionKind is what a session of the run is.
type sessionKind string

const (
	// sessionRun is the session of the run itself: it hangs under the
	// message that started the run.
	sessionRun sessionKind = "run"
	// sessionRound is the session of one round of one job: its card
	// hangs under the message that started the run.
	sessionRound sessionKind = "round"
	// sessionDecision is the session of one decision: its card hangs
	// under the command message of the step the decision is about.
	sessionDecision sessionKind = "decision"
	// sessionRecord is the session of the record a finished run
	// leaves behind: it rides on the card of the run, or on the card
	// of the stop when the operator stopped the run.
	sessionRecord sessionKind = "record"
)

// meAgentName is the name the sessions of the personal agent carry.
const meAgentName = "me"

// session is one agent session the run owns.
//
// The session hangs under one message: the anchor of every event the
// session sends. The chat renders the events onto the card of that
// message.
type session struct {
	// kind is what the session is.
	kind sessionKind
	// anchor is the message the session's events hang under. It is
	// empty when the chat refused the message: the session's events
	// then fall back to the chat's orphan handling.
	anchor string
	// id names the session: the run's id, the round's identity or
	// the decision's number.
	id string
	// job and round name the round the session is about. They are
	// empty for the sessions that are not about a round.
	job   string
	round int
}

// The lines the placeholders carry.
//
// A placeholder says in one line what the card below it is about.
const (
	// decisionLine is what the placeholder of a decision says.
	decisionLine = "the agent is deciding…"
	// stateLine is what the placeholder of the state of the run says.
	stateLine = "the run is reporting its state…"
)

// roundLine is what the placeholder of a round says.
func roundLine(job string, round int) string {
	return fmt.Sprintf("%s · round %d is starting…", job, round)
}

// placeholder posts the placeholder message of a session and returns
// the session.
//
// The placeholder is a message of the chat: the id it becomes is the
// anchor of the session, and the chat renders the session's events
// onto the card of that message.
//
// A chat that refuses the message leaves the session without an
// anchor: the run goes on, and the session's events fall back to the
// chat's orphan handling.
func (s *Service) placeholder(ctx context.Context, r *run, sess *session, line, replyTo string) *session {
	if s.cfg.Operator == nil {
		return sess
	}
	msgID, err := s.cfg.Operator.Ask(ctx, r.chatID, line, channel.SpeakOptions{
		ReplyTo: replyTo,
		Display: BotMark + line,
		Owner:   r.owner,
		RunID:   r.id,
	})
	if err != nil {
		s.logger.Warn("me: the session placeholder could not be posted",
			"run_id", r.id, "kind", string(sess.kind), "err", err)
		return sess
	}
	sess.anchor = msgID
	return sess
}

// openRunSession opens the session of the run.
//
// The session hangs under the message that started the run: the run
// posts no message of its own, and the chat folds the run's entries
// into the card it already holds for that message.
//
// A run that no message started — a synthetic inbound, a command
// without a message id — has no such card: its entries go to the
// chat's orphan handling, and the chat shows each of them on a card
// of its own.
//
// The session carries the plan of the run and nothing else: what the
// run is stays the same from the start to the end, and the state of the
// work is published on its own.
func (s *Service) openRunSession(ctx context.Context, r *run, anchor string) {
	sess := &session{kind: sessionRun, id: r.id, anchor: anchor}
	r.setRunSession(sess)
	r.setGitStatus(s.gitStatus(r))
	s.publishInit(ctx, r, sess)
	s.publishReply(ctx, r, sess, r.runCard())
}

// openRoundSession opens the session of a job round.
//
// The card hangs under the message that started the run: the round is
// the run's work, one level below it. The card carries the tree of the
// rounds of the run and the steps of the round as its checklist. The
// checklist follows the steps of the round: the card shows what the
// round has done.
func (s *Service) openRoundSession(ctx context.Context, r *run, it iteration) {
	replyTo := ""
	if runSess := r.runSessionRef(); runSess != nil {
		replyTo = runSess.anchor
	}
	sess := s.placeholder(ctx, r,
		&session{kind: sessionRound, id: roundIdentity(it), job: it.job, round: it.round},
		roundLine(it.job, it.round), replyTo)
	// The run goes on with the card of the round: everything the run
	// publishes from now on lands on it, and the card it replaces
	// keeps what it had.
	r.setRoundSession(sess)
	r.resetRound()
	// The round starts when its card opens: the first action of the
	// round follows a moment later.
	r.setRoundStarted(it, s.now())
	r.rememberRoundSession(it, sess)
	s.publishInit(ctx, r, sess)
	s.publishReply(ctx, r, sess, r.roundsTree(it))
	s.publishTasks(ctx, r, sess, r.roundTasks(it.job, it.round))
}

// showState publishes the state of the run on a card of its own.
//
// The run reports its state: it posts a placeholder for the state, and
// the chat renders the state onto the card of that placeholder. The
// placeholder hangs under the card of the round the run is in.
func (s *Service) showState(ctx context.Context, r *run) {
	// No message of the operator's own and no answer: the state opens a
	// card of its own.
	s.answerCommand(ctx, r, "", "")
}

// stateSession builds the session the state of the run is published in.
//
// The state rides in the session of the round the run is in: the card of
// that round is the card the state goes on.
func (r *run) stateSession(it iteration, inRound bool) *session {
	sess := &session{kind: sessionRun, id: r.id}
	if inRound {
		sess.kind = sessionRound
		sess.id = roundIdentity(it)
		sess.job = it.job
		sess.round = it.round
	}
	return sess
}

// adoptState makes the card of the state the card the round goes on
// with.
//
// The card of the state is the latest card of the round: the run goes
// on with it, and the card it replaces keeps the content it had.
//
// The run may have moved on to another round while the state was on its
// way: the card of the round stays the one it had.
//
// A card the run opened for itself is a card of a pause. The operator
// may take the pause away while the state is on its way — the run goes
// on, and the operator is answered on the card of their own command.
// The card of the pause is then a snapshot of a moment that has passed,
// and it does not take the card of the round.
func (s *Service) adoptState(r *run, sess *session, it iteration, inRound, own bool) {
	now, still := r.roundIteration()
	if !inRound || !still || now != it || sess.anchor == "" {
		return
	}
	if own && !r.pausePending() {
		return
	}
	r.setRoundSession(sess)
	r.rememberRoundSession(it, sess)
	// The anchor of the round moved: the state on the disk names the
	// card the run goes on with.
	s.persist(r)
}

// publishState publishes the state of the run in the given session.
//
// The state is a snapshot: the phase line of the run, the tree of the
// rounds and the checklist of the round the run is in. A run that has
// not entered a round yet has no round state to show: only the phase
// line is published.
//
// The session names itself before the state goes on it: see
// answerCommand.
func (s *Service) publishState(ctx context.Context, r *run, sess *session, it iteration, inRound bool) {
	s.publishReply(ctx, r, sess, r.phaseLine())
	if !inRound {
		return
	}
	s.publishReply(ctx, r, sess, r.roundsTree(it))
	s.publishTasks(ctx, r, sess, r.roundTasks(it.job, it.round))
}

// openDecisionSession opens the session of a decision.
//
// The card hangs under the command message of the step the decision
// is about, so the decision reads directly after the error it
// answers. A decision about a step that never sent a command to the
// chat hangs under the round's card instead.
//
// The session's first entry is the reason the decision was taken.
func (s *Service) openDecisionSession(ctx context.Context, r *run) {
	// The decision that was open is over: its verdict rides in its own
	// footer.
	s.closeDecisionSession(ctx, r)
	replyTo := r.decisionAnchor()
	if replyTo == "" {
		if roundSess := r.roundSessionRef(); roundSess != nil {
			replyTo = roundSess.anchor
		}
	}
	r.mu.Lock()
	r.decisionCount++
	id := fmt.Sprintf("decision-%d", r.decisionCount)
	r.mu.Unlock()
	r.resetDecisionUsage()

	sess := s.placeholder(ctx, r, &session{kind: sessionDecision, id: id}, decisionLine, replyTo)
	r.setDecisionSession(sess)
	s.publishInit(ctx, r, sess)
	s.publishReply(ctx, r, sess, r.decisionReasonLine())
}

// switchRoundSession moves the run to the session of the round it is
// in.
//
// Every round gets a session of its own. The session of the round
// that ended stays in the chat as the record of that round: the
// engine clears the round's step records when the next round starts,
// so the finished card cannot be rebuilt afterwards. A run that comes
// back to a round goes on with the message that round hangs under: the
// chat opens a new card for it, and the card that was there before stays
// in the chat as a snapshot.
//
// The switch happens before the first command of the round is sent to
// the chat, so the card of a round stands in front of the commands of
// that round.
func (s *Service) switchRoundSession(ctx context.Context, r *run, it iteration) {
	if it.job == "" {
		return
	}
	current := r.roundSessionRef()
	if current != nil && current.job == it.job && current.round == it.round {
		// The run is already in that round.
		return
	}
	// The round the run was in is over.
	s.closeRoundSession(ctx, r)
	if sess, known := r.roundSessionOf(it); known && sess != nil {
		// The round has a session of its own: the run goes back to it.
		r.setRoundSession(sess)
		r.resetRound()
		// The round starts again when the run enters it again.
		r.setRoundStarted(it, s.now())
		s.publishTasks(ctx, r, sess, r.roundTasks(it.job, it.round))
		return
	}
	s.openRoundSession(ctx, r, it)
}

// closeRoundSession ends the session of the round the run is in.
//
// The round's card says what the round came to, and the verdict of
// the round rides in its footer.
func (s *Service) closeRoundSession(ctx context.Context, r *run) {
	sess := r.roundSessionRef()
	if sess == nil {
		return
	}
	rec := r.closeRound(sess.job, sess.round, s.now())
	ctx = context.WithoutCancel(r.ctx)
	s.publishReply(ctx, r, sess, roundEndedLine(rec))
	s.publishReply(ctx, r, sess, decisionsText(rec))
	s.publishResult(ctx, r, sess, string(rec.Outcome.Status))
	r.setRoundSession(nil)
	// The facts of the round belong to the run: a daemon that comes
	// back renders the tree of the rounds the same way. The state goes
	// to the disk after the session is closed: a state that still named
	// the round that just ended would close it a second time.
	s.persist(r)
}

// closeDecisionSession ends the session of the decision that is open.
//
// The verdict of the decision rides in the session's footer.
func (s *Service) closeDecisionSession(ctx context.Context, r *run) {
	sess := r.decisionSessionRef()
	if sess == nil {
		return
	}
	ctx = context.WithoutCancel(r.ctx)
	s.publishResult(ctx, r, sess, r.decisionVerdict())
	r.setDecisionSession(nil)
}

// runVerdict renders how the run ended.
//
// The summary of the run says what it did; a run that has no summary
// says the phase it ended in.
func (r *run) runVerdict() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.summary != "" {
		return r.summary
	}
	return string(r.phase)
}

// decisionVerdict renders what the decision came to.
//
// The verdict is what the model decided: the command it picked, or
// what it asked for.
func (r *run) decisionVerdict() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.wakes) == 0 {
		return ""
	}
	return r.wakes[len(r.wakes)-1].What
}

// decisionAnchor returns the message the decision's card hangs under.
//
// It is the command message of the step the decision is about. A
// decision that names no step, or a step that never sent a command to
// the chat, has no anchor of its own.
func (r *run) decisionAnchor() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	stepID := r.judgmentStep
	if stepID == "" {
		return ""
	}
	return r.commandMsgIDs[stepID]
}

// decisionReasonLine renders why the decision was taken.
//
// The line is the judgment the workflow could not decide on, and the
// error of the step the judgment is about.
func (r *run) decisionReasonLine() string {
	r.mu.Lock()
	reason := r.judgment
	stepID := r.judgmentStep
	errText := ""
	if stepID != "" {
		errText = r.stepErrorLocked(stepID)
	}
	r.mu.Unlock()
	switch {
	case reason != "" && errText != "":
		return "reason: " + reason + " — " + errText
	case reason != "":
		return "reason: " + reason
	case errText != "":
		return "reason: " + errText
	}
	return ""
}

// stepErrorLocked returns the error of a step. The caller holds the
// lock.
func (r *run) stepErrorLocked(stepID string) string {
	for _, st := range r.state.Steps {
		if st.ID != stepID {
			continue
		}
		if text := st.Outcome.Data[wfe.OutcomeKeyError]; text != "" {
			return text
		}
		if st.Outcome.Summary != "" && !st.Outcome.Succeeded() {
			return st.Outcome.Summary
		}
	}
	return ""
}

// roundOutcome returns what a round of a job came to.
//
// The verdict is the last action the round took: the outcome of the
// step that ended it.
func (r *run) roundOutcome(job string, round int) wfe.Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.policy.History) - 1; i >= 0; i-- {
		rec := r.policy.History[i]
		if rec.StepID == "" {
			continue
		}
		if wfe.JobOf(r.workflow, rec.StepID) != job {
			continue
		}
		if rec.Round != round {
			continue
		}
		return rec.Outcome
	}
	return wfe.Outcome{Status: wfe.OutcomeUnknown}
}

// roundIdentity renders the name of a job round.
func roundIdentity(it iteration) string {
	return fmt.Sprintf("%s · round %d", it.job, it.round)
}

// publishInit announces the session to the chat.
//
// The chat turns the announcement into the session's identity line:
// the agent, the model and the session's own name.
func (s *Service) publishInit(ctx context.Context, r *run, sess *session) {
	if sess == nil {
		return
	}
	s.emit(ctx, r, sess, messages.OutboundMessage{
		Kind:      messages.OutInit,
		SessionID: sess.id,
		Model:     r.modelName(),
		AgentName: meAgentName,
		Workspace: r.workCwd(),
	})
}

// publishTasks publishes the checklist of a session.
//
// The snapshot is the whole list: the chat renders it as the card's
// checklist and replaces the previous one with it.
func (s *Service) publishTasks(ctx context.Context, r *run, sess *session, items []agent.AgentTaskItem) {
	if sess == nil {
		return
	}
	s.emit(ctx, r, sess, messages.OutboundMessage{
		Kind:     messages.OutTaskUpdate,
		TaskList: &agent.AgentTaskListEvent{Items: items},
	})
}

// refreshRoundTasks publishes the checklist of the round the run is in.
//
// The checklist belongs to the latest card of the round: a card that
// was left behind keeps the checklist of the moment it was left. The
// run publishes it once per step, so the card shows what the round has
// done so far.
//
// A run that is in no round has no checklist to publish.
func (s *Service) refreshRoundTasks(r *run) {
	sess := r.roundSessionRef()
	if sess == nil {
		return
	}
	it, inRound := r.roundIteration()
	if !inRound {
		return
	}
	s.publishTasks(context.WithoutCancel(r.ctx), r, sess, r.roundTasks(it.job, it.round))
}

// publishReply publishes one entry of a session.
//
// The entry is what the session has to say: the reason a decision was
// taken, the words a model spoke, the outcome of a step.
func (s *Service) publishReply(ctx context.Context, r *run, sess *session, text string) {
	if sess == nil || strings.TrimSpace(text) == "" {
		return
	}
	s.emit(ctx, r, sess, messages.OutboundMessage{
		Kind: messages.OutReply,
		Text: text,
	})
}

// publishRecord publishes the record of the run.
//
// The record is the last word of the run: it carries the verdict in
// its footer, so the card of the run still says how the run ended
// after the record lands on it.
func (s *Service) publishRecord(ctx context.Context, r *run, sess *session, text string) {
	if sess == nil || strings.TrimSpace(text) == "" {
		return
	}
	s.emit(ctx, r, sess, messages.OutboundMessage{
		Kind:          messages.OutReply,
		Text:          text,
		SessionResult: r.runVerdict(),
	})
}

// publishResult ends a session.
//
// The verdict rides in the session's footer: the chat stamps the
// footer and leaves the card where it is.
func (s *Service) publishResult(ctx context.Context, r *run, sess *session, verdict string) {
	if sess == nil {
		return
	}
	s.emit(ctx, r, sess, messages.OutboundMessage{
		Kind: messages.OutResult,
		// The body is empty: the verdict is the footer of the card,
		// not a message of its own.
		Result:        &agent.AgentResultEvent{},
		SessionResult: verdict,
	})
}

// modelName returns the name of the model the run thinks with.
//
// The decision model reports the name it answered with; the chat
// model does not, so the run shows what it knows.
func (r *run) modelName() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.system1Model
}

// sessionUsage returns what a session spent.
//
// The run session shows what the whole run spent, the round session
// what the round spent and the decision session what the decision
// spent.
func (r *run) sessionUsage(sess *session) llm.Usage {
	if sess == nil {
		return llm.Usage{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch sess.kind {
	case sessionRound:
		return r.roundUsage
	case sessionDecision:
		return r.decisionUsage
	default:
		usage := r.system1Usage
		usage.Add(r.system2Usage)
		return usage
	}
}

// gitStatusTimeout is how long the repository facts may take to read.
const gitStatusTimeout = 5 * time.Second

// gitStatus returns the repository facts of the run's workspace.
//
// The facts ride in the footer's workspace line: where the run works
// and which branch it is on.
func (s *Service) gitStatus(r *run) *messages.GitStatus {
	dir := s.workDir(r)
	if dir == "" {
		return nil
	}
	status := &messages.GitStatus{Workspace: dir}
	if s.cfg.Git == nil {
		return status
	}
	// The git call must not hold the run: a repository that hangs
	// would hang the run with it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), gitStatusTimeout)
	defer cancel()
	out, _, err := s.cfg.Git.Run(ctx, dir, "status", "--porcelain", "--branch")
	if err != nil {
		return status
	}
	branch := gitBranch(out)
	if branch == "" {
		return status
	}
	status.Snapshot = &messages.GitStatusSnapshot{Branch: branch}
	return status
}

// gitBranch reads the branch out of a `git status --porcelain
// --branch` answer.
//
// The answer's first line names the branch: "## main...origin/main"
// or "## main".
func gitBranch(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		name := strings.TrimPrefix(line, "## ")
		if idx := strings.IndexAny(name, "..."); idx >= 0 {
			name = name[:idx]
		}
		return strings.TrimSpace(name)
	}
	return ""
}
