package me

import (
	"context"
	"errors"
	"strings"

	"github.com/cnlangzi/nightme/internal/wfe"
)

// brainBytes is how much of a session's answer the model reads.
const brainBytes = 8 << 10

// roundBrain is the session that thinks for the owner.
//
// It belongs to one job in one round, in one directory: when the run
// moves on, the session goes with it, so the next round starts with a
// clean mind.
type roundBrain struct {
	// session is the open session.
	session AgentSession
	// job is the job the session belongs to.
	job string
	// round is the round of that job the session belongs to.
	round int
	// cwd is the directory the session works in.
	cwd string
	// greeted reports whether the session got its opening turn yet.
	greeted bool
}

// agentName returns the agent that thinks for the owner: the one the
// daemon names, or the one of the chat that started the run.
func (s *Service) agentName(chatAgent string) string {
	if s.cfg.Agent != "" {
		return s.cfg.Agent
	}
	return chatAgent
}

// hasBrain reports whether the run can ask anyone: there is an agent
// to ask, and a way to open it.
func (s *Service) hasBrain(r *run) bool {
	return s.cfg.AgentOpener != nil && r.agentName != ""
}

// brainState returns the session that thinks for the owner.
func (r *run) brainState() *roundBrain {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.brain
}

// setBrain records the session that thinks for the owner.
func (r *run) setBrain(b *roundBrain) {
	r.mu.Lock()
	r.brain = b
	r.mu.Unlock()
}

// brainGreeted reports whether the session got its opening turn.
func (r *run) brainGreeted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.brain != nil && r.brain.greeted
}

// markBrainGreeted records that the session got its opening turn.
func (r *run) markBrainGreeted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.brain != nil {
		r.brain.greeted = true
	}
}

// brainIteration returns the job and the round the run is on.
func (r *run) brainIteration() (string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.brainIterationLocked()
}

// brainIterationLocked returns the job and the round the run is on.
// The caller holds the lock.
//
// A step that was skipped does not count: the chain continues past it,
// so the run is not in that job. The job of the last step that ran is
// the one the run is working on — and the one a judgment belongs to.
func (r *run) brainIterationLocked() (string, int) {
	job := ""
	for i := len(r.state.Steps) - 1; i >= 0; i-- {
		if r.state.Steps[i].Job == "" || r.state.Steps[i].Skipped {
			continue
		}
		job = r.state.Steps[i].Job
		break
	}
	if job == "" {
		return "", 0
	}
	return job, r.state.RoundOf(job)
}

// rounds returns the round every job is in.
func (r *run) rounds() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return roundsSnapshot(r.state.Rounds)
}

// brain opens the session that thinks for the owner, and opens a new
// one when the run left the iteration the last session belonged to.
//
// The session follows the run: another job, another round or another
// directory means another session, so no answer of the last round
// leaks into this one.
func (s *Service) brain(r *run) (AgentSession, error) {
	if s.cfg.AgentOpener == nil {
		return nil, errors.New("no agent to think with")
	}
	if !s.hasBrain(r) {
		return nil, errors.New("no agent to think with")
	}
	job, round := r.brainIteration()
	cwd := s.workDir(r)
	if b := r.brainState(); b != nil && b.job == job && b.round == round && b.cwd == cwd {
		return b.session, nil
	}
	s.closeBrain(r)
	session, err := s.cfg.AgentOpener.Open(r.ctx, r.agentName, cwd)
	if err != nil {
		return nil, err
	}
	r.setBrain(&roundBrain{session: session, job: job, round: round, cwd: cwd})
	return session, nil
}

// closeBrain ends the session that thinks for the owner.
func (s *Service) closeBrain(r *run) {
	b := r.brainState()
	if b == nil {
		return
	}
	r.setBrain(nil)
	if err := b.session.Close(); err != nil {
		s.logger.Warn("me: the thinking session did not close cleanly", "run_id", r.id, "err", err)
	}
}

// closeBrainOnNewRound ends the session that thinks for the owner when
// the run left the iteration it belonged to.
//
// The engine advances a round inside its own decision, so the rounds
// are compared around it: a round that moved means the session of the
// old round has nothing left to say.
func (s *Service) closeBrainOnNewRound(r *run, before map[string]int, decision wfe.Decision) {
	if r.brainState() == nil {
		return
	}
	for job, round := range r.rounds() {
		if round != before[job] {
			s.closeBrain(r)
			return
		}
	}
	b := r.brainState()
	if b == nil {
		return
	}
	if decision.Job != "" && decision.Job != b.job {
		s.closeBrain(r)
		return
	}
	for _, step := range decision.Ready {
		if job := wfe.JobOf(r.workflow, step.ID); job != "" && job != b.job {
			s.closeBrain(r)
			return
		}
	}
}

// askBrain asks the session that thinks for the owner one question.
//
// A session that fails to answer is closed and opened again: a stale
// session is the usual reason a turn dies. Only the second failure
// ends the question.
func (s *Service) askBrain(ctx context.Context, r *run, task string) (AgentReply, error) {
	// The question belongs to the run: it does not outlive it.
	ctx, cancel := r.runContext(ctx)
	defer cancel()
	session, err := s.brain(r)
	if err != nil {
		return AgentReply{}, err
	}
	reply, err := s.askSession(ctx, r, session, task)
	if err == nil {
		return reply, nil
	}
	s.closeBrain(r)
	session, err = s.brain(r)
	if err != nil {
		return AgentReply{}, err
	}
	return s.askSession(ctx, r, session, task)
}

// askSession asks one session a question.
func (s *Service) askSession(ctx context.Context, r *run, session AgentSession, task string) (AgentReply, error) {
	prompt := task
	if !r.brainGreeted() {
		prompt = s.brainOpening(r, task)
	}
	reply, err := session.Ask(ctx, prompt)
	if err != nil {
		return AgentReply{}, err
	}
	r.markBrainGreeted()
	return reply, nil
}

// brainOpening renders the opening turn of the session that thinks for
// the owner.
//
// The first turn says what the session is for; every turn after it
// carries the question alone, so the session is never reminded of the
// workflow's machinery.
func (s *Service) brainOpening(r *run, task string) string {
	title := r.workflowTitle()
	cwd := s.workDir(r)
	var b strings.Builder
	b.WriteString("You are the thinking of the owner of this work. The objective: ")
	b.WriteString(title)
	b.WriteString(". You are in ")
	b.WriteString(cwd)
	b.WriteString(".\n")
	b.WriteString("For each question, investigate — read code, run tests and read-only commands, check git and the issue tracker — find the cause, and recommend what to do next, with your reasons. Do not modify files, commit or push: the owner's avatar carries out every change.\n")
	b.WriteString(task)
	return b.String()
}

// brainSummary renders what a session answered: the first line of the
// answer, and the answer itself as the transcript.
func brainSummary(reply AgentReply) (summary, transcript string) {
	text := strings.TrimSpace(reply.Text)
	if text == "" {
		return "", ""
	}
	summary = text
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		summary = strings.TrimSpace(text[:idx])
	}
	if len(text) > brainBytes {
		text = text[:brainBytes] + "…"
	}
	return summary, text
}
