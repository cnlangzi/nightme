package me

import (
	"context"
	"errors"
	"strings"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// The directives the operator can give a run.
//
// A directive is the operator's decision for the run's next turn. The
// run carries it and its loop applies it: running a step takes as long
// as the work takes, so the decision cannot be carried out in the
// command's own goroutine.
const (
	// directiveResume runs the step the run is on again.
	directiveResume = "resume"
	// directiveNext skips the step the run is on.
	directiveNext = "next"
)

// operatorCallID is the id the operator's action is recorded under.
//
// The operator's action is a synthetic assistant tool call: the chat
// model reads it as one more step of the run.
const operatorCallID = "operator"

// The reason codes the operator's commands report.
//
// A program branches on the code; the message beside it is for the
// operator.
const (
	// ReasonNoRun says the chat holds no run.
	ReasonNoRun = "no_run"
	// ReasonNotPaused says the run waits for an answer: it asked the
	// operator a question, and `/me reply` is the way to answer it.
	ReasonNotPaused = "not_paused"
)

// reasonError is the generic reason code: the failure has no code of
// its own.
const reasonError = "error"

// Refusal is a command the operator's run refused.
//
// The reason code is stable: a program branches on it and never reads
// the message.
type Refusal struct {
	// Reason is the code of the refusal.
	Reason string
	// Message says what went wrong, in words.
	Message string
}

// Error implements the error interface.
func (e *Refusal) Error() string { return e.Message }

// RefusalReason returns the reason code of a refusal, or an empty
// string when the error is not one.
func RefusalReason(err error) string {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ""
}

// directive is the operator's decision for the run's next turn.
//
// The step is part of the decision: the operator was told which step
// the command acts on, and the run acts on that one.
type directive struct {
	// Kind is the decision: "resume" or "next".
	Kind string `json:"kind"`
	// Step is the id of the step the decision acts on.
	Step string `json:"step,omitempty"`
}

// OperatorDecision is what an operator's command did with the step the
// run is on.
//
// The command decides at command time: it acts on the step only when
// there is something to do. A command that acts on nothing holds no
// decision, and says why.
type OperatorDecision struct {
	// Step is the id of the step the command acted on — or the step it
	// looked at when it acted on none.
	Step string
	// Act reports whether the command acted on the step: it runs again,
	// or it is skipped.
	Act bool
	// Why says why the command acted on nothing. The reply of the
	// command names the step and says what did not happen.
	Why string
	// skippedNow reports that the step was skipped at once: its work was
	// in flight, and the command the run waits for was abandoned. The
	// run holds no decision for it: the step is skipped already.
	skippedNow bool
}

// The reasons a command acts on nothing.
//
// A reason names the state of the step the command looked at. The
// reply of the command reads it: the operator cannot tell that their
// command did nothing from the state of the run alone.
const (
	// reasonSucceeded says the step did its job.
	reasonSucceeded = "already succeeded"
	// reasonRunning says the work of the step is in flight.
	reasonRunning = "is still running"
	// reasonSkipped says the step was passed over already.
	reasonSkipped = "is already skipped"
	// reasonCannotRun says the step cannot run again.
	reasonCannotRun = "cannot run again"
	// reasonNotRun says the step has not run yet.
	reasonNotRun = "has not run"
	// reasonWorking says the run is working on the step on its own.
	reasonWorking = "is being worked on"
)

// prepareDecision records the operator's decision for the run's next
// turn and hands the control back to the run.
//
// It reports what the command did with the step the run is on, and
// whether the run was waiting at the gate: a run that waits for the
// operator has to be woken, a run that is working goes on by itself.
//
// The command hands the control back either way: a run that waits for
// the operator goes on. A command that acts on nothing records no
// decision: the plan runs the step it was going to run anyway. A
// decision the run still holds — one it has not taken yet — is the
// run's own: this command does not touch it.
//
// The decision is refused when the run waits for an answer: that
// state has its own command, `/me reply`.
func (r *run) prepareDecision(kind, note string) (OperatorDecision, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	d, err := r.decideDirectiveLocked(kind)
	if err != nil {
		return OperatorDecision{}, false, err
	}

	// A pause that has not taken effect yet is taken back: the run
	// goes on.
	paused := r.paused
	r.pauseRequested = false
	r.paused = false
	r.pauseReason = ""
	r.phase = PhaseRunning
	if d.Act && !d.skippedNow {
		r.directive = &directive{Kind: kind, Step: d.Step}
	}
	// The note is the operator's instruction: it joins the
	// conversation the chat model reads.
	if strings.TrimSpace(note) != "" {
		r.messages = append(r.messages, llm.Message{
			Role:  llm.RoleUser,
			Parts: []llm.Part{{Type: llm.PartText, Text: note}},
		})
	}
	return d, paused, nil
}

// decideDirectiveLocked says what the operator's command will do with
// the step the run is on. The caller holds the lock.
//
// The command acts only on a step that ran and did not succeed, and
// only when that step may run again: that is the step the run's own
// execution layer acts on, so the promise the command makes and the
// action the run performs agree. A step that succeeded, that was
// passed over already, or that the plan runs anyway has nothing to
// decide: the command holds no decision, and says why.
//
// A step whose work is in flight is the exception: `/me next` passes
// it over. A step that is stuck in a retry loop is exactly the step
// the operator has to be able to pass over.
//
// A run that is working decides for itself: there is nothing for the
// operator to decide there either.
//
// The run refuses a decision while it waits for an answer: that state
// has its own command, `/me reply`.
func (r *run) decideDirectiveLocked(kind string) (OperatorDecision, error) {
	if r.ask != nil {
		return OperatorDecision{}, &Refusal{
			Reason:  ReasonNotPaused,
			Message: "this run is waiting for an answer: use /me reply <text>",
		}
	}
	stepID := r.targetStepLocked()
	if stepID == "" || r.stepIndex(stepID) < 0 {
		return OperatorDecision{}, &Refusal{
			Reason:  reasonError,
			Message: "there is no step to " + directiveVerb(kind),
		}
	}

	d := OperatorDecision{Step: stepID}
	// The work of the step is in flight: its command was sent to the
	// chat and has not come back. The step cannot run again, and
	// `/me next` passes it over anyway — when the command can be
	// abandoned.
	if r.stepInFlightLocked(stepID) {
		if kind == directiveNext && r.canAbandonCommandLocked(stepID) {
			d.Act = true
			d.skippedNow = true
			return d, nil
		}
		d.Why = reasonRunning
		return d, nil
	}
	st, ran := r.stepStateLocked(stepID)
	if ran && r.stepSkippedLocked(stepID) {
		// The step was passed over already: there is nothing to skip.
		d.Why = reasonSkipped
		return d, nil
	}
	if ran && st.Outcome.Succeeded() {
		// The step did its job: there is nothing to repair, and
		// nothing to skip.
		d.Why = reasonSucceeded
		return d, nil
	}
	// The run is working: it decides for itself.
	if !r.pausePendingLocked() {
		d.Why = reasonWorking
		return d, nil
	}
	if ran {
		// The step ran and did not succeed: it is the step a decision
		// acts on. `/me resume` needs a step that may run again: a
		// step of an earlier round cannot, the plan has moved on
		// without it.
		if kind == directiveNext {
			d.Act = true
			return d, nil
		}
		if !r.runnableStepLocked(stepID) {
			d.Why = reasonCannotRun
			return d, nil
		}
		d.Act = true
		return d, nil
	}
	// The step has not run: the plan runs it. `/me next` passes it
	// over anyway — the operator asked for it.
	if kind == directiveNext {
		d.Act = true
		return d, nil
	}
	d.Why = reasonNotRun
	return d, nil
}

// directiveVerb renders what the directive does, for the messages the
// operator reads.
func directiveVerb(kind string) string {
	if kind == directiveNext {
		return "skip"
	}
	return "resume"
}

// takeDirective takes the operator's decision and forgets it.
//
// A decision is taken once: the loop applies it on its next turn.
func (r *run) takeDirective() *directive {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.directive
	r.directive = nil
	return d
}

// targetStep returns the step the run is on: the subject of the last
// judgment, or the step the run worked on when it has no judgment.
func (r *run) targetStep() (wfe.Step, bool) {
	r.mu.Lock()
	stepID := r.targetStepLocked()
	r.mu.Unlock()
	if stepID == "" {
		return wfe.Step{}, false
	}
	return r.stepByID(stepID)
}

// targetStepLocked returns the id of the step the run is on. The
// caller holds the lock.
//
// The judgment names the step it is about. A run that has no judgment
// — the operator took the chat back, the agent was throttled — is on
// the step it ran last, when that step did not succeed: the operator
// took the chat back over that step. A run whose last step succeeded
// is on the first step of the plan's ready set.
//
// It is not the step whose budget bounds the run: that one names the
// step that ran last once that step succeeded, and the operator would
// act on the wrong step.
func (r *run) targetStepLocked() string {
	if r.judgmentStep != "" {
		return r.judgmentStep
	}
	if id := r.policy.CurrentStepID(); id != "" && !r.stepSucceededLocked(id) {
		return id
	}
	if ids := r.readyStepIDsLocked(); len(ids) > 0 {
		return ids[0]
	}
	return ""
}

// stepSucceededLocked reports whether the step ended ok. The caller
// holds the lock.
func (r *run) stepSucceededLocked(stepID string) bool {
	for _, st := range r.state.Steps {
		if st.ID == stepID {
			return st.Outcome.Succeeded()
		}
	}
	return false
}

// stepInFlightLocked reports whether the work of the step is in
// flight: its command was sent to the chat and has not come back.
//
// The caller holds the lock.
func (r *run) stepInFlightLocked(stepID string) bool {
	if r.runningStep == stepID {
		// The run is executing the step: its command is on its way.
		return true
	}
	// The step started and has not settled: its command waits for an
	// answer.
	st, ok := r.stepStateLocked(stepID)
	return ok && st.Pending
}

// canAbandonCommandLocked reports whether the run can be told that the
// command of the step was abandoned: the run waits for that command.
//
// Only a chat command can be abandoned. A shell, a wait or an action
// belongs to the run itself, and the run cannot go on while it runs.
//
// The caller holds the lock.
func (r *run) canAbandonCommandLocked(stepID string) bool {
	if r.inflight == nil {
		// The run waits for no command: there is nothing to abandon.
		return false
	}
	if r.runningStep == stepID {
		// The run is executing the step: its command is on its way.
		return true
	}
	// The step started and has not settled: its command waits for an
	// answer.
	st, ok := r.stepStateLocked(stepID)
	return ok && st.Pending
}

// stepSkipped reports whether the step was passed over: the operator
// or the agent skipped it.
//
// A step that was passed over keeps its record: the verdict that
// arrives late does not put it back into the plan.
func (r *run) stepSkipped(stepID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stepSkippedLocked(stepID)
}

// stepSkippedLocked reports whether the step was passed over. The
// caller holds the lock.
func (r *run) stepSkippedLocked(stepID string) bool {
	st, ok := r.stepStateLocked(stepID)
	return ok && isSkippedOutcome(st.Outcome)
}

// isSkippedOutcome reports whether the outcome says the step was passed
// over.
func isSkippedOutcome(out wfe.Outcome) bool {
	if out.Status != wfe.OutcomeNoop {
		return false
	}
	switch out.Summary {
	case operatorSkippedSummary, agentSkippedSummary:
		return true
	}
	return false
}

// setRunningStep records the step whose work is in flight.
func (r *run) setRunningStep(stepID string) {
	r.mu.Lock()
	r.runningStep = stepID
	r.mu.Unlock()
}

// clearRunningStep forgets the step whose work is in flight.
func (r *run) clearRunningStep() {
	r.mu.Lock()
	r.runningStep = ""
	r.mu.Unlock()
}

// wake tells the run that the operator is waiting for it.
//
// The signal is buffered: the run may not have reached the gate yet.
func (r *run) wake() {
	select {
	case r.unpause <- struct{}{}:
	default:
	}
}

// drainUnpause forgets a wake that came before the run waited.
//
// The operator's decision takes the pause back and wakes the run. A
// wake that arrives before the run reaches the gate is stale: the
// pause it belonged to is gone, and the next pause must not be
// swallowed by it.
func (r *run) drainUnpause() {
	select {
	case <-r.unpause:
	default:
	}
}

// applyDirective carries out the operator's decision.
//
// The decision is applied at the start of a turn, before the plan runs:
// the plan then sees the state the decision left behind.
func (s *Service) applyDirective(ctx context.Context, r *run) error {
	d := r.takeDirective()
	if d == nil {
		return nil
	}
	// The decision is taken once: the state no longer carries it, so a
	// daemon that dies in the middle does not apply it twice.
	s.persist(r)
	switch d.Kind {
	case directiveResume:
		return s.applyResume(ctx, r, d.Step)
	case directiveNext:
		return s.applyNext(ctx, r, d.Step)
	}
	return nil
}

// applyResume runs the step the decision names again.
//
// The step's budget for the round starts over: the operator asked for
// the step to run again, and the chances the agent spent are spent no
// more.
//
// A step that has not run yet, that is still running or that succeeded
// has nothing to retry: the decision is dropped and the plan goes on.
func (s *Service) applyResume(ctx context.Context, r *run, stepID string) error {
	if stepID == "" {
		return nil
	}
	step, ok := r.stepByID(stepID)
	if !ok {
		return nil
	}
	st, ok := r.stepState(step.ID)
	if !ok || st.Pending || st.Outcome.Succeeded() {
		return nil
	}
	if !r.runnableStep(step.ID) {
		return nil
	}
	res, err := s.runStepAgain(ctx, r, step, true)
	if err != nil {
		if isToolError(err) {
			// The step may not run now: the plan goes on and the model
			// decides what happens next.
			return nil
		}
		return err
	}
	s.recordOperatorAction(r, toolResume, res)
	s.persist(r)
	return nil
}

// applyNext skips the step the decision names.
//
// The step is recorded as skipped: the facts it produced are kept, the
// error goes, and the plan goes on with the workflow.
//
// A step that is still running is left alone: the work is in flight.
// A step that did its job is left alone too: there is nothing to skip.
func (s *Service) applyNext(ctx context.Context, r *run, stepID string) error {
	if stepID == "" {
		return nil
	}
	step, ok := r.stepByID(stepID)
	if !ok {
		return nil
	}
	st, ran := r.stepState(step.ID)
	if ran {
		if st.Pending {
			return nil
		}
		if st.Outcome.Succeeded() {
			return nil
		}
	}
	res := s.skipStep(ctx, r, step, operatorSkippedSummary)
	s.recordOperatorAction(r, toolNext, res)
	s.persist(r)
	return nil
}

// skipRunningStep skips the step whose work is in flight.
//
// The step is recorded as skipped: the facts it produced are kept, the
// plan goes on. The command the run waits for is abandoned: the wait
// ends at once, and the run goes on to its next step.
//
// The command itself is not cancelled: the chat owns it, and what it
// produces lands in the chat when it finishes.
func (s *Service) skipRunningStep(ctx context.Context, r *run, stepID string) {
	step, ok := r.stepByID(stepID)
	if !ok {
		return
	}
	res := s.skipStep(ctx, r, step, operatorSkippedSummary)
	s.recordOperatorAction(r, toolNext, res)
	r.abandonCommand()
}

// recordOperatorAction records what the operator did in the
// conversation the chat model reads.
//
// The action is a synthetic assistant tool call with its result: the
// model sees what the operator did when it takes over.
func (s *Service) recordOperatorAction(r *run, tool string, res stepResult) {
	recordSyntheticCall(r, operatorCallID, tool, map[string]any{})
	s.appendToolResult(r, pendingCall{ID: operatorCallID, Name: tool}, res, false)
}
