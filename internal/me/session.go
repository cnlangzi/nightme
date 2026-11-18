// session.go — the agent sessions the personal agent runs.
//
// A run speaks in three kinds of session, and every session owns one
// card: the placeholder the run posted when the session opened. The
// card's message id is the anchor of every event the session sends,
// so the chat renders the session onto that card and patches it in
// place — the way it renders a coding agent's turn.
//
// The package speaks the standard outbound vocabulary
// (internal/messages), and the chat renders it. The two blocks the
// run publishes itself — the overview of the run and the block that
// names the run a round belongs to — are entries of a card, and the
// chat renders them as such.
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
	// sessionRun is the session of the run itself: its card hangs
	// under the message that started the run.
	sessionRun sessionKind = "run"
	// sessionRound is the session of one round of one job: its card
	// hangs under the run's card.
	sessionRound sessionKind = "round"
	// sessionDecision is the session of one decision: its card hangs
	// under the command message of the step the decision is about.
	sessionDecision sessionKind = "decision"
)

// meAgentName is the name the sessions of the personal agent carry.
const meAgentName = "me"

// session is one agent session the run owns.
//
// The session owns one card: the placeholder the run posted when the
// session opened. Every event the session sends carries that card's
// id as its anchor, and the chat patches the card in place.
type session struct {
	// kind is what the session is.
	kind sessionKind
	// cardID is the id of the message the session's card became. It
	// is the anchor of every event the session sends. Empty when the
	// chat refused the card: the session's events then fall back to
	// the chat's orphan handling.
	cardID string
	// id names the session: the run's id, the round's identity or
	// the decision's number.
	id string
	// job and round name the round the session is about. They are
	// empty for the sessions that are not about a round.
	job   string
	round int
}

// openSession posts the placeholder card of a session and returns it.
//
// The placeholder is the card the session owns: the chat adopts it
// and patches it in place when the session's first event lands.
//
// A chat that refuses the card leaves the session without one: the
// run goes on, and the session's events fall back to the chat's
// orphan handling.
func (s *Service) openSession(ctx context.Context, r *run, kind sessionKind, id, job string, round int, replyTo string) *session {
	return s.openSessionBody(ctx, r, kind, id, job, round, replyTo, placeholderLine(kind, job, round))
}

// openSessionBody posts the card of a session with a body of its own.
//
// The body is the one line the card carries until the session's own
// events fill it.
func (s *Service) openSessionBody(ctx context.Context, r *run, kind sessionKind, id, job string, round int, replyTo, body string) *session {
	sess := &session{kind: kind, id: id, job: job, round: round}
	if s.cfg.Operator == nil {
		return sess
	}
	cardID, err := s.cfg.Operator.PostCard(ctx, r.chatID, channel.CardSpec{
		Body:    body,
		ReplyTo: replyTo,
	})
	if err != nil {
		s.logger.Warn("me: the session card could not be posted",
			"run_id", r.id, "kind", string(kind), "err", err)
		return sess
	}
	sess.cardID = cardID
	return sess
}

// placeholderLine is the one line the placeholder card carries.
//
// The card is a placeholder: the session's own events fill it as soon
// as they land.
func placeholderLine(kind sessionKind, job string, round int) string {
	switch kind {
	case sessionRound:
		return fmt.Sprintf("%s · round %d is starting…", job, round)
	case sessionDecision:
		return "the agent is deciding…"
	default:
		return "the run is starting…"
	}
}

// stateLine is the one line the state card carries.
//
// The card shows the state of the round: it does not start the round,
// and the line says so.
func stateLine(job string, round int) string {
	return fmt.Sprintf("%s · round %d — the state of the run", job, round)
}

// sessionCards returns every session of the run that owns a card.
//
// The cards are the anchors of the sessions: the chat patches them in
// place, so a run that comes back has to tell the chat which of its
// messages are cards.
func (r *run) sessionCards() []*session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*session, 0, 4)
	for _, sess := range []*session{r.runSession, r.roundSession, r.decisionSession} {
		if sess != nil && sess.cardID != "" {
			out = append(out, sess)
		}
	}
	for _, sess := range r.roundSessions {
		if sess == nil || sess.cardID == "" {
			continue
		}
		if sess == r.roundSession {
			continue
		}
		out = append(out, sess)
	}
	return out
}

// adoptSessionCards tells the chat which of the run's messages are its
// cards.
//
// A daemon that comes back has forgotten the cards it posted, and the
// receipt of a session would send a second card instead of patching
// the one the run opened before the restart.
func (s *Service) adoptSessionCards(r *run) {
	if s.cfg.Operator == nil {
		return
	}
	for _, sess := range r.sessionCards() {
		s.cfg.Operator.AdoptCard(r.chatID, sess.cardID)
	}
}

// openRunSession opens the session of the run.
//
// The card hangs under the message that started the run, so the
// operator finds it where the run began. The card carries the plan of
// the run and nothing else: what the run is stays the same from the
// start to the end, and the live state of the work is on the card of
// the round.
func (s *Service) openRunSession(ctx context.Context, r *run, replyTo string) bool {
	sess := s.openSession(ctx, r, sessionRun, r.id, "", 0, replyTo)
	if sess.cardID == "" {
		// The chat refused the card: the run goes on with the card it
		// has, and the session's events fall back to the chat's
		// orphan handling.
		return false
	}
	r.setRunSession(sess)
	r.setGitStatus(s.gitStatus(r))
	s.publishInit(ctx, r, sess)
	s.publishReply(ctx, r, sess, r.runCard())
	return true
}

// openRoundSession opens the session of a job round.
//
// The card hangs under the run's card: the round is the run's work,
// one level below it. The card carries the tree of the rounds of the
// run and the steps of the round as its checklist.
func (s *Service) openRoundSession(ctx context.Context, r *run, it iteration) {
	replyTo := ""
	if runSess := r.runSessionRef(); runSess != nil {
		replyTo = runSess.cardID
	}
	sess := s.openSession(ctx, r, sessionRound, roundIdentity(it), it.job, it.round, replyTo)
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

// openRoundStateCard posts the state card of the round the run is in
// and makes it the round's live card.
//
// The card hangs at the top of the chat: the operator asked for the
// state where they are looking. It carries the phase line, the tree of
// the rounds, the checklist of the round and the heartbeat, and the
// run goes on with it: the round is not started over, so its counters
// and its start time stay what they were.
//
// The run moves between rounds on its own goroutine: the card takes the
// place of the round's card only while the run is still in that round.
// A card posted for a round the run has left stays in the chat as a
// card of its own, and the round keeps the card it had.
//
// It returns nil when the run has no round: there is no round state to
// show.
func (s *Service) openRoundStateCard(ctx context.Context, r *run) *session {
	it, ok := r.roundIteration()
	if !ok {
		return nil
	}
	card := s.openSessionBody(ctx, r, sessionRound, roundIdentity(it), it.job, it.round, "", stateLine(it.job, it.round))
	if card.cardID == "" {
		// The chat refused the card: the run keeps the live card it
		// has, and the run goes on.
		return nil
	}
	if !r.adoptRoundSession(it, card) {
		// The run has left that round while the card was posted: the
		// card says nothing the round does not already say, and the
		// run goes on with the card it has.
		return card
	}
	r.rememberRoundSession(it, card)
	r.setGitStatus(s.gitStatus(r))
	s.publishInit(ctx, r, card)
	s.publishReply(ctx, r, card, r.phaseLine())
	s.publishReply(ctx, r, card, r.roundsTree(it))
	s.publishTasks(ctx, r, card, r.roundTasks(it.job, it.round))
	s.publishHeartbeatTo(ctx, r, card)
	return card
}

// openRunOverview posts the overview of the run as a card of its own.
//
// The card is a snapshot of the plan: the run keeps the session it had,
// so the card the operator started the run from keeps the verdict of
// the run.
func (s *Service) openRunOverview(ctx context.Context, r *run) bool {
	sess := s.openSession(ctx, r, sessionRun, r.id, "", 0, "")
	if sess.cardID == "" {
		// The chat refused the card: the run goes on with the card it
		// has.
		return false
	}
	r.setGitStatus(s.gitStatus(r))
	s.publishInit(ctx, r, sess)
	s.publishReply(ctx, r, sess, r.runCard())
	return true
}

// showState posts the state of the run in the chat.
//
// The state card carries the state of the round the run is in. A run
// that has not entered a round yet has no round state to show: the
// overview of the run is posted instead.
//
// It reports whether a card was posted.
func (s *Service) showState(ctx context.Context, r *run) bool {
	if _, ok := r.roundIteration(); !ok {
		return s.openRunOverview(ctx, r)
	}
	return s.openRoundStateCard(ctx, r) != nil
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
			replyTo = roundSess.cardID
		}
	}
	r.mu.Lock()
	r.decisionCount++
	id := fmt.Sprintf("decision-%d", r.decisionCount)
	r.mu.Unlock()
	r.resetDecisionUsage()

	sess := s.openSession(ctx, r, sessionDecision, id, "", 0, replyTo)
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
// back to a round goes on with the card that round has.
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

// recordStepOutcome publishes the outcome of a step.
//
// The row of the step carries the outcome: the checklist of the round
// is the state of the round, and an entry per step would say the same
// thing a second time.
func (s *Service) recordStepOutcome(ctx context.Context, r *run) {
	sess := r.roundSessionRef()
	if sess == nil {
		return
	}
	s.publishTasks(ctx, r, sess, r.roundTasks(sess.job, sess.round))
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

// publishHeartbeat publishes what the run did so far.
//
// The counters ride in the header of the round's card: how many turns
// the chat model took, how many tool calls the run made and when it
// last did something. The card of the run is static, so it carries
// none.
func (s *Service) publishHeartbeat(ctx context.Context, r *run) {
	s.publishHeartbeatTo(ctx, r, r.roundSessionRef())
}

// publishHeartbeatTo publishes what the run did so far to a card.
//
// The card is named: a card that is not the live card of the round
// carries the counters of the run all the same.
func (s *Service) publishHeartbeatTo(ctx context.Context, r *run, sess *session) {
	if sess == nil {
		return
	}
	r.mu.Lock()
	snap := messages.HeartbeatSnapshot{
		ThinkCount: r.thinkCount,
		ToolCount:  r.toolCount,
		LastBeatAt: r.lastBeatAt,
		Status:     messages.HeartbeatRunning,
	}
	r.mu.Unlock()
	s.emit(ctx, r, sess, messages.OutboundMessage{
		Kind:      messages.OutHeartbeat,
		Heartbeat: &snap,
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
