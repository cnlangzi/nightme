package me

import (
	"context"
	"errors"
	"fmt"
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
	// ReasonNotPaused says the run does not wait for the operator:
	// it is working, or it is waiting for an answer.
	ReasonNotPaused = "not_paused"
	// ReasonStepRunning says the step the command acts on is still
	// running.
	ReasonStepRunning = "step_running"
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

// prepareDecision records the operator's decision for the run's next
// turn and hands the control back to the run.
//
// It reports the step the command acts on and whether the run was
// waiting at the gate: a run that was working goes on by itself, and
// there is nobody to wake up.
//
// The decision is refused when the run does not wait for the operator:
// a run that is working has nothing to decide, and a run that is
// asking a question is answered with `/me reply`.
func (r *run) prepareDecision(kind, note string) (wfe.Step, bool, error) {
	r.mu.Lock()
	if r.ask != nil {
		r.mu.Unlock()
		return wfe.Step{}, false, &Refusal{
			Reason:  ReasonNotPaused,
			Message: "this run is waiting for an answer: use /me reply <text>",
		}
	}
	if !r.paused && !r.pauseRequested {
		r.mu.Unlock()
		return wfe.Step{}, false, &Refusal{
			Reason:  ReasonNotPaused,
			Message: "this run is not paused",
		}
	}
	if kind == directiveNext {
		if busy := r.busyStepIDLocked(); busy != "" {
			r.mu.Unlock()
			return wfe.Step{}, false, &Refusal{
				Reason:  ReasonStepRunning,
				Message: fmt.Sprintf("the step %q is still running: wait for it", busy),
			}
		}
	}
	stepID := r.targetStepLocked()
	r.mu.Unlock()

	if stepID == "" {
		return wfe.Step{}, false, &Refusal{
			Reason:  reasonError,
			Message: "there is no step to " + directiveVerb(kind),
		}
	}
	step, ok := r.stepByID(stepID)
	if !ok {
		return wfe.Step{}, false, &Refusal{
			Reason:  reasonError,
			Message: "there is no step to " + directiveVerb(kind),
		}
	}

	r.mu.Lock()
	// A pause that has not taken effect yet is taken back: the run
	// goes on.
	paused := r.paused
	r.pauseRequested = false
	r.paused = false
	r.pauseReason = ""
	r.phase = PhaseRunning
	r.directive = &directive{Kind: kind, Step: step.ID}
	// The note is the operator's instruction: it joins the
	// conversation the chat model reads.
	if strings.TrimSpace(note) != "" {
		r.messages = append(r.messages, llm.Message{
			Role:  llm.RoleUser,
			Parts: []llm.Part{{Type: llm.PartText, Text: note}},
		})
	}
	r.mu.Unlock()
	return step, paused, nil
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

// busyStepIDLocked returns the id of the step that is still running:
// the one whose work is in flight, the one a command is on, or the one
// the chat waits for.
//
// The caller holds the lock.
func (r *run) busyStepIDLocked() string {
	if r.runningStep != "" {
		return r.runningStep
	}
	if r.inflight != nil {
		// The run waits for a command: the step it is on is busy.
		return r.targetStepLocked()
	}
	for _, st := range r.state.Steps {
		if st.Pending {
			return st.ID
		}
	}
	return ""
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

// recordOperatorAction records what the operator did in the
// conversation the chat model reads.
//
// The action is a synthetic assistant tool call with its result: the
// model sees what the operator did when it takes over.
func (s *Service) recordOperatorAction(r *run, tool string, res stepResult) {
	recordSyntheticCall(r, operatorCallID, tool, map[string]any{})
	s.appendToolResult(r, pendingCall{ID: operatorCallID, Name: tool}, res, false)
}
