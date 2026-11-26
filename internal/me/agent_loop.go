package me

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/cnlangzi/nightme/pkg/wfe"
)

// errFinished ends the loop when the model called the finish tool.
var errFinished = errors.New("me: the run finished")

// sys1Action is what the decision model led to.
type sys1Action int

const (
	// sys1None means there was no decision to make.
	sys1None sys1Action = iota
	// sys1Act means the decision was carried out.
	sys1Act
	// sys1Escalate means the decision was handed over: to the chat
	// model, or to the user when there is no chat model.
	sys1Escalate
)

// Who decided. The names show up in the run's wake log.
const (
	decisionDecider = "the decision model"
	decisionChat    = "the chat model"
)

// drive runs one workflow to its end and reports how it ended.
func (s *Service) drive(ctx context.Context, r *run) {
	defer close(r.done)
	err := s.runLoop(ctx, r)
	// The session that thinks for the owner belongs to the run: it
	// goes with it, whatever ended the run.
	s.closeBrain(r)
	if r.isDaemonGone() {
		// The daemon is going away: the run stops where it stands and
		// publishes nothing.
		return
	}
	switch {
	case r.isFinished():
		// The model ended the run itself; the phase it ended with is
		// kept — a run that failed is not turned into a finished one.
		s.finish(r, r.currentPhase(), "")
	case err == nil, errors.Is(err, errFinished):
		s.finish(r, PhaseDone, "")
	case errors.Is(err, context.Canceled):
		s.finish(r, PhaseStopped, "")
	default:
		if r.wasStoppedByOperator() {
			// The operator stopped the run: that is what ended it.
			// The reason it stopped is the stop itself.
			s.finish(r, PhaseStopped, "")
		} else {
			s.finish(r, PhaseFailed, err.Error())
			r.setSummary("the run failed")
			s.refreshRun(r)
		}
	}
	// The run is over: its sessions end with it.
	s.closeRoundSession(ctx, r)
	s.closeDecisionSession(ctx, r)
	s.publishResult(context.WithoutCancel(r.ctx), r, r.runSessionRef(), r.runVerdict())
	// The session is over: the chat gets the record of it. The record
	// is built here and not in the command, because the outcome of the
	// step that was in flight is only known now that the run has
	// unwound.
	s.postRecord(r)
}

// runLoop runs the run's cycle and keeps a panic of it from taking the
// daemon with it.
//
// A bug in one run is that run's business: the panic is reported with
// its stack and the run ends as failed. The daemon goes on serving the
// other chats.
func (s *Service) runLoop(ctx context.Context, r *run) (err error) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		s.logger.Error("me: a run panicked",
			"run_id", r.id,
			"panic", rec,
			"stack", string(debug.Stack()))
		err = fmt.Errorf("the run crashed: %v", rec)
	}()
	return s.loop(ctx, r)
}

// loop is the run's main cycle.
//
// Every turn does one of four things: it finishes the tool calls that
// are still open, it runs the step the workflow decides on, it asks
// the decision model what comes next, or it lets the chat model take
// a turn.
//
// A run has no turn cap: it ends when the plan is exhausted and the
// agent finishes it, when the operator stops it, or when the daemon
// shuts down.
func (s *Service) loop(ctx context.Context, r *run) error {
	if s.cfg.Provider == nil {
		return s.loopDeciderOnly(ctx, r)
	}
	for {
		if err := s.checkGate(ctx, r); err != nil {
			return err
		}
		if len(r.pendingCallList()) > 0 {
			if err := s.runPendingCalls(ctx, r); err != nil {
				return err
			}
			continue
		}
		done, err := s.turn(ctx, r)
		if err != nil {
			return err
		}
		if done {
			return errFinished
		}
	}
}

// checkGate reports whether the run may go on.
//
// A run that was asked to pause waits here: the step that was
// running is not interrupted, and the run picks up again when the
// operator hands the control back.
func (s *Service) checkGate(ctx context.Context, r *run) error {
	if err := s.checkAlive(ctx, r); err != nil {
		return err
	}
	if r.applyPause() {
		s.refreshRun(r)
	}
	if !r.isPaused() {
		return nil
	}
	// A wake that came before the run waited is stale: the pause it
	// belonged to was taken back. It must not swallow the next pause.
	r.drainUnpause()
	if err := r.awaitUnpause(ctx); err != nil {
		return err
	}
	s.refreshRun(r)
	return nil
}

// turn advances the run by one decision.
//
// The workflow decides when it can: the step runs as it is, and no
// model is asked. Where the workflow cannot decide, the agent does.
func (s *Service) turn(ctx context.Context, r *run) (bool, error) {
	// The operator's decision comes first: the plan then sees the state
	// it left behind.
	if err := s.applyDirective(ctx, r); err != nil {
		return false, err
	}
	decision, err := s.plan(ctx, r)
	if err != nil {
		return false, err
	}
	if decision.Kind == wfe.DecisionRun {
		if err := r.checkStep(decision.Step); err == nil {
			r.setJudgment("")
			r.setJudgmentStep("")
			if err := s.runPlanStep(ctx, r, decision); err != nil {
				return false, err
			}
			return false, nil
		} else {
			// The budget is spent: the workflow cannot go on by
			// itself, so the agent decides what happens next.
			decision = wfe.Decision{
				Kind:    wfe.DecisionJudgment,
				Reason:  err.Error(),
				Subject: decision.Step.ID,
			}
		}
	}
	r.setJudgment(decision.Reason)
	r.setJudgmentStep(decision.Subject)
	if decision.Over {
		// The workflow is over: the rounds of its jobs are spent and
		// nothing broke. The run ends on its own — no model is asked
		// what comes next.
		s.finishSpent(r, decision.RoundsSpent)
		return true, nil
	}
	if decision.Exhausted && s.goalMet(r) {
		// Every step ran and the last one ended ok: the goal is met
		// and no model is asked to say so.
		s.finishDone(r, r.workflow.Name+" is complete")
		return true, nil
	}
	// The budget of the step the run is on bounds what the agent may
	// do at it. When it is spent, the run waits for a human: the run
	// keeps its place, and the operator decides what happens to a
	// node that cannot be repaired.
	if s.budgetSpent(r) {
		s.pauseForBudget(ctx, r)
		return false, nil
	}
	// The run asks a model: the decision session opens and owns the
	// wake-up. Its card is where the decision reads.
	s.openDecisionSession(ctx, r)
	action, err := s.system1(ctx, r)
	if err != nil {
		return false, err
	}
	if action == sys1Act {
		return false, nil
	}
	done, err := s.system2(ctx, r)
	if err != nil {
		return false, err
	}
	return done, nil
}

// budgetSpent reports whether the step the run is on used up its
// budget.
func (s *Service) budgetSpent(r *run) bool {
	stepID := r.budgetStep()
	if stepID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := r.stepIndex(stepID)
	if idx < 0 {
		return false
	}
	remaining := r.policy.Remaining()
	if idx >= len(remaining.Steps) {
		return false
	}
	return remaining.Steps[idx] == 0
}

// pauseForBudget pauses the run because the step it is on used up its
// budget.
//
// The run keeps its state and its place: the operator runs the step
// again with `/me resume`, and the step's budget starts over.
func (s *Service) pauseForBudget(ctx context.Context, r *run) {
	stepID := r.budgetStep()
	used, limit := r.stepUsage(stepID)
	reason := fmt.Sprintf("the step %q used up its budget (%d of %d chances) — the run waits for you",
		stepID, used, limit)
	r.pauseNow(reason)
	s.showStateOnPause(ctx, r)
	s.refreshRun(r)
	s.announcePause(ctx, r, reason)
}

// announcePause tells the operator that the run waits for them.
//
// The state of the run hangs in the card of the round: the chat does
// not push it, so the operator is told that the run stopped and what
// they can do about it.
//
// The notice is not a command of the workflow: it is the run telling
// the operator what it waits for.
func (s *Service) announcePause(ctx context.Context, r *run, reason string) {
	if _, err := s.speak(ctx, r, pauseMark+"The run paused: "+reason+". Send /me resume to run the step again, or /me next to skip it.", ""); err != nil {
		s.logger.Warn("me: the pause could not be spoken", "run_id", r.id, "err", err)
	}
}

// plan asks the engine what the run does next.
//
// The engine owns the state: it records the steps it skips on the way
// and advances the round of a job that repeats. The policy follows the
// rounds, so the per-step budgets count the current round only.
//
// The gap between two rounds is waited out here, before the round
// starts: the engine reports it and the run waits it.
//
// The card follows the iteration the plan enters: a run that moves to
// another job or to the next round of its job gets a card of its own,
// and the switch happens before the command of that iteration is sent
// to the chat.
func (s *Service) plan(ctx context.Context, r *run) (wfe.Decision, error) {
	// The engine reads the run's environment to decide: a step whose
	// condition names the workspace has to be decided against the
	// directory the chat stands in now.
	s.syncWorkDir(r)
	r.mu.Lock()
	r.state.UpdatedAt = s.now()
	before := roundsSnapshot(r.state.Rounds)
	decision, err := wfe.Decide(r.state, r.workflow)
	if err != nil {
		r.mu.Unlock()
		return decision, err
	}
	r.policy.SetRounds(r.state.Rounds)
	r.policy.SetSkippedRounds(r.state.SkippedRounds)
	it := targetIteration(r, decision, before)
	r.mu.Unlock()

	// The engine advances a round inside its own decision, so the
	// rounds are compared around it: the thinking session of the round
	// that ended goes with it.
	s.closeBrainOnNewRound(r, before, decision)
	// The gap is waited before the run enters the next round: the card
	// of the round opens when the round starts.
	if err := waitFor(ctx, r, decision.Gap); err != nil {
		return decision, err
	}
	s.switchRoundSession(ctx, r, it)
	return decision, nil
}

// roundsSnapshot copies the round every job is in.
//
// The engine writes the rounds into the map it holds, so the rounds of
// a plan have to be copied before it runs.
func roundsSnapshot(rounds map[string]int) map[string]int {
	out := make(map[string]int, len(rounds))
	for name, round := range rounds {
		out[name] = round
	}
	return out
}

// targetIteration returns the iteration the plan enters.
//
// The engine counts the rounds; the runner reads them and does not
// change how they are counted.
//
// The decision's job wins when it names one: a run decision on job B
// keeps the card on B even when the engine advanced job A in the same
// call. The advanced job is the target only when the decision names
// no job.
//
// A job the plan looked at for the first time is not an advance: the
// engine sets its round to 1 when it reads it.
//
// The caller holds the run's lock.
func targetIteration(r *run, decision wfe.Decision, before map[string]int) iteration {
	if decision.Job != "" {
		return iteration{job: decision.Job, round: r.state.RoundOf(decision.Job)}
	}
	for _, job := range r.workflow.JobOrder() {
		round := r.state.Rounds[job]
		if round > before[job] && round > 1 {
			return iteration{job: job, round: round}
		}
	}
	return iteration{}
}

// runPlanStep runs the step the workflow decided on.
//
// No model is asked: the step's own text is what runs.
func (s *Service) runPlanStep(ctx context.Context, r *run, decision wfe.Decision) error {
	verdict, err := s.executeStep(ctx, r, decision.Job, decision.Step)
	if err != nil {
		return err
	}
	if !verdict.Settled {
		// The step has no verdict yet, so the run state must not
		// record it. A step whose command is still running is picked up
		// where it left off on the next turn; a step the operator
		// passed over is not: it is skipped already, and the plan goes
		// on with the next step.
		return nil
	}
	if err := r.settleStep(decision.Job, decision.Step, verdict.Outcome); err != nil {
		return err
	}
	s.refreshRun(r)
	return nil
}

// goalMet reports whether the plan is done and every step ended ok.
//
// The title of a workflow is written for a person; the engine can only
// say that the plan ran through.
func (s *Service) goalMet(r *run) bool {
	out, ok := r.lastOutcome()
	return ok && out.Succeeded()
}

// loopDeciderOnly runs the run with the decision model alone.
//
// Without a chat model there is nobody to escalate to, so a decision
// the model is not sure about becomes a question for the user.
func (s *Service) loopDeciderOnly(ctx context.Context, r *run) error {
	for {
		if err := s.checkGate(ctx, r); err != nil {
			return err
		}
		// The operator's decision comes first: the plan then sees the
		// state it left behind.
		if err := s.applyDirective(ctx, r); err != nil {
			return err
		}
		// A question from an earlier turn is still open.
		if st := r.pendingAsk(); st != nil {
			reply, err := r.awaitReply(ctx)
			if err != nil {
				return err
			}
			r.clearAsk()
			if choice := matchOption(st.Options, reply); choice != "" {
				done, err := s.actOnChoice(ctx, r, choice)
				if err != nil {
					return err
				}
				if done {
					return errFinished
				}
			}
			continue
		}
		decision, err := s.plan(ctx, r)
		if err != nil {
			return err
		}
		if decision.Kind == wfe.DecisionRun {
			if err := r.checkStep(decision.Step); err == nil {
				r.setJudgment("")
				r.setJudgmentStep("")
				if err := s.runPlanStep(ctx, r, decision); err != nil {
					return err
				}
				continue
			} else {
				// The budget is spent: the workflow cannot go on by
				// itself, so the agent decides what happens next.
				decision = wfe.Decision{
					Kind:    wfe.DecisionJudgment,
					Reason:  err.Error(),
					Subject: decision.Step.ID,
				}
			}
		}
		r.setJudgment(decision.Reason)
		r.setJudgmentStep(decision.Subject)
		if decision.Over {
			// The workflow is over: the rounds of its jobs are spent
			// and nothing broke. The run ends on its own — no model is
			// asked what comes next.
			s.finishSpent(r, decision.RoundsSpent)
			return errFinished
		}
		if decision.Exhausted && s.goalMet(r) {
			s.finishDone(r, r.workflow.Name+" is complete")
			return errFinished
		}
		if s.budgetSpent(r) {
			// The budget of the step the run is on is spent: the run
			// waits for a human.
			s.pauseForBudget(ctx, r)
			continue
		}
		// The run asks the decision model: the decision session opens
		// and owns the wake-up.
		s.openDecisionSession(ctx, r)
		dec, err := s.decide(ctx, r)
		if err != nil {
			return err
		}
		s.recordWake(ctx, r, wakeRecord{
			Who:  decisionDecider,
			What: decisionWhat(dec),
			Why:  r.judgmentReason(),
			At:   s.now(),
		})
		if s.canAct(dec) {
			action, err := s.actOnDecision(ctx, r, dec)
			if err != nil {
				return err
			}
			if action == sys1Act {
				if r.currentPhase() == PhaseDone {
					return errFinished
				}
				continue
			}
		}
		reply, err := s.askUser(ctx, r, escalateQuestion(dec), dec.Options)
		if err != nil {
			return err
		}
		choice := matchOption(dec.Options, reply)
		if choice == "" {
			continue
		}
		done, err := s.actOnChoice(ctx, r, choice)
		if err != nil {
			return err
		}
		if done {
			return errFinished
		}
	}
}

// checkAlive reports whether the run may go on.
func (s *Service) checkAlive(ctx context.Context, r *run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.ctx.Err()
}

// finishDone ends the run as done and tells the observer.
func (s *Service) finishDone(r *run, summary string) {
	r.markFinished()
	r.setSummary(summary)
	s.finish(r, PhaseDone, "")
	s.refreshRun(r)
}

// finishSpent ends the run because a job spent its rounds.
//
// The run ends on its own: the loop of the job is over, and the record
// says so on a card of its own.
func (s *Service) finishSpent(r *run, job string) {
	r.setSpentLimit()
	r.setEndReason(spentReason(r, job))
	s.finishDone(r, r.workflow.Name+" is over")
}

// spentReason renders why the run ended on the limit of its rounds.
//
// The line names the loop and the rounds it ran: the operator reads
// whether the workflow did its work or ran out of road.
func spentReason(r *run, job string) string {
	if job == "" {
		return "the rounds of the workflow are spent — the run stops here"
	}
	return fmt.Sprintf("the loop of %q is done (%d of %d rounds)",
		job, r.roundsRun(job), r.roundLimit(job))
}

// now returns the current time.
func (s *Service) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// system1 asks the decision model what comes next and acts on the
// answer when the model is sure and the policy allows it.
//
// It reports what the decision led to.
func (s *Service) system1(ctx context.Context, r *run) (sys1Action, error) {
	if s.cfg.Decider == nil {
		return sys1None, nil
	}
	dec, err := s.decide(ctx, r)
	if err != nil {
		return sys1None, err
	}
	s.recordWake(ctx, r, wakeRecord{
		Who:  decisionDecider,
		What: decisionWhat(dec),
		Why:  r.judgmentReason(),
		At:   s.now(),
	})
	if !s.canAct(dec) {
		// The decision is not sure enough to act on: the chat model
		// takes over.
		return sys1Escalate, nil
	}
	return s.actOnDecision(ctx, r, dec)
}

// decisionReason renders why the decision model picked what it picked.
func decisionReason(dec decision) string {
	if dec.Model == "" {
		return fmt.Sprintf("p=%.2f", dec.NextConfidence)
	}
	return fmt.Sprintf("%s, p=%.2f", dec.Model, dec.NextConfidence)
}

// decisionWhat renders what the decision model decided.
//
// A model that picked nothing decided nothing: the wake log says so.
func decisionWhat(dec decision) string {
	what := dec.Next
	if what == "" {
		what = "nothing"
	}
	return fmt.Sprintf("%s (p=%.2f)", what, dec.NextConfidence)
}

// canAct reports whether a decision may be acted on without the chat
// model.
//
// The decision has to be confident, must not ask for a human, and the
// policy has to allow it.
func (s *Service) canAct(dec decision) bool {
	if dec.Next == "" || dec.Next == optionEscalate {
		return false
	}
	if dec.NextConfidence < s.cfg.MinConfidence {
		return false
	}
	if dec.NeedsHuman >= 0.5 {
		return false
	}
	return true
}

// actOnDecision carries out a decision the model was sure about.
//
// The decision is recorded in the conversation as a synthetic tool
// call, so the chat model sees a consistent history when it takes
// over.
func (s *Service) actOnDecision(ctx context.Context, r *run, dec decision) (sys1Action, error) {
	switch dec.Next {
	case optionDone:
		if err := r.checkFinish("done"); err != nil {
			return s.escalateDecision(r, dec, err)
		}
		s.recordSynthetic(r, toolFinish, map[string]any{"status": "done", "summary": "the workflow is complete"})
		s.finishDone(r, r.workflow.Name+" is complete")
		return sys1Act, nil
	default:
		if _, err := r.checkRun(dec.Next); err != nil {
			return s.escalateDecision(r, dec, err)
		}
		tool, args := s.syntheticCall(r, dec.Next)
		s.recordSynthetic(r, tool, args)
		verdict, err := s.runStep(ctx, r, dec.Next)
		if err != nil {
			if isToolError(err) {
				// The command could not run: the chat model takes over
				// and can correct it.
				return s.escalateDecision(r, dec, err)
			}
			return sys1None, err
		}
		s.appendToolResult(r, pendingCall{ID: syntheticCallID, Name: tool}, verdict, !verdict.Succeeded())
		s.refreshRun(r)
		return sys1Act, nil
	}
}

// escalateDecision hands a policy-rejected decision over.
//
// A decision the policy does not allow is not an error: the model
// simply picked something the workflow forbids, and somebody else
// has to decide.
func (s *Service) escalateDecision(r *run, dec decision, cause error) (sys1Action, error) {
	s.logger.Info("me: the decision was not allowed", "run_id", r.id, "choice", dec.Next, "err", cause)
	return sys1Escalate, nil
}

// actOnChoice carries out a command the user picked.
//
// It reports whether the run is over. A choice the policy refuses is
// reported to the observer and asked again — the user picked something
// the workflow does not allow, not a reason to end the run.
func (s *Service) actOnChoice(ctx context.Context, r *run, command string) (bool, error) {
	if command == optionEscalate {
		return false, nil
	}
	if command == optionDone {
		if err := r.checkFinish("done"); err != nil {
			r.setSummary(err.Error())
			s.refreshRun(r)
			return false, nil
		}
		s.finishDone(r, r.workflow.Name+" is complete")
		return true, nil
	}
	if _, err := r.checkRun(command); err != nil {
		r.setSummary(err.Error())
		s.refreshRun(r)
		return false, nil
	}
	tool, args := s.syntheticCall(r, command)
	s.recordSynthetic(r, tool, args)
	verdict, err := s.runStep(ctx, r, command)
	if err != nil {
		if isToolError(err) {
			// The command could not run: the user is told and the run
			// goes on.
			r.setSummary(err.Error())
			s.refreshRun(r)
			return false, nil
		}
		return false, err
	}
	s.appendToolResult(r, pendingCall{ID: syntheticCallID, Name: tool}, verdict, !verdict.Succeeded())
	s.refreshRun(r)
	return false, nil
}
