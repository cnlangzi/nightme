package me

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/wfe"
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

// drive runs one playbook to its end and reports how it ended.
func (s *Service) drive(ctx context.Context, r *run) {
	defer close(r.done)
	err := s.loop(ctx, r)
	if r.isSuspended() {
		// The daemon is going away, not the run: the state is kept and
		// the run resumes when the daemon comes back.
		return
	}
	switch {
	case r.isFinished():
		// The model ended the run itself; the phase it ended with is
		// kept — a run that failed is not turned into a finished one.
		s.finish(r, r.currentPhase(), "")
		s.removeState(r.id)
	case err == nil, errors.Is(err, errFinished):
		s.finish(r, PhaseDone, "")
		s.removeState(r.id)
	case errors.Is(err, context.Canceled):
		s.finish(r, PhaseStopped, "")
		s.removeState(r.id)
	default:
		s.finish(r, PhaseFailed, err.Error())
		s.notify(r, "❌ the run failed: "+err.Error())
		s.removeState(r.id)
	}
}

// loop is the run's main cycle.
//
// Every turn does one of three things: it finishes the tool calls
// that are still open, it asks the decision model what comes next, or
// it lets the chat model take a turn.
func (s *Service) loop(ctx context.Context, r *run) error {
	if s.cfg.Provider == nil {
		return s.loopDeciderOnly(ctx, r)
	}
	maxIter := s.maxIterations(r)
	for i := 0; i < maxIter; i++ {
		if err := s.checkAlive(ctx, r); err != nil {
			return err
		}
		if len(r.pendingCallList()) > 0 {
			if err := s.runPendingCalls(ctx, r); err != nil {
				return err
			}
			continue
		}
		action, err := s.system1(ctx, r)
		if err != nil {
			return err
		}
		if action == sys1Act {
			continue
		}
		done, err := s.system2(ctx, r)
		if err != nil {
			return err
		}
		if done {
			return errFinished
		}
	}
	return fmt.Errorf("the run reached its limit of %d turns", maxIter)
}

// loopDeciderOnly runs the run with the decision model alone.
//
// Without a chat model there is nobody to escalate to, so a decision
// the model is not sure about becomes a question for the user.
func (s *Service) loopDeciderOnly(ctx context.Context, r *run) error {
	maxIter := s.maxIterations(r)
	for i := 0; i < maxIter; i++ {
		if err := s.checkAlive(ctx, r); err != nil {
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
				done, err := s.actOnChoice(ctx, r, choice, "the user")
				if err != nil {
					return err
				}
				if done {
					return errFinished
				}
			}
			continue
		}
		dec, err := s.decide(ctx, r)
		if err != nil {
			return err
		}
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
		done, err := s.actOnChoice(ctx, r, choice, "the user")
		if err != nil {
			return err
		}
		if done {
			return errFinished
		}
	}
	return fmt.Errorf("the run reached its limit of %d turns", maxIter)
}

// checkAlive reports whether the run may go on.
func (s *Service) checkAlive(ctx context.Context, r *run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.ctx.Err()
}

// maxIterations returns the loop cap of a run: three times the
// playbook's action budget, unless the configuration says otherwise.
func (s *Service) maxIterations(r *run) int {
	if s.cfg.MaxIterations > 0 {
		return s.cfg.MaxIterations
	}
	actions := r.playbook.MaxActions
	if actions <= 0 {
		actions = wfe.DefaultMaxActions
	}
	return actions * DefaultMaxIterationsFactor
}

// notify posts a line to the observer chat.
//
// A run without a sender — a test, or a daemon that is still wiring
// up — simply has nobody to tell.
func (s *Service) notify(r *run, text string) {
	if s.cfg.Sender == nil || strings.TrimSpace(text) == "" {
		return
	}
	// The run's own context is cancelled when the run ends, and the
	// last line — the summary — is sent after that. The context
	// outlives the run here on purpose.
	ctx := context.WithoutCancel(r.ctx)
	if err := s.cfg.Sender.Send(ctx, r.observerChatID, text); err != nil {
		s.logger.Warn("me: the observer could not be told", "run_id", r.id, "err", err)
	}
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
	if !s.canAct(dec) {
		// The decision is not sure enough to act on: the chat model
		// takes over, with the decision attached as a hint.
		s.appendHint(r, dec)
		return sys1Escalate, nil
	}
	return s.actOnDecision(ctx, r, dec)
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
		s.recordSynthetic(r, "finish", map[string]any{"status": "done", "summary": "the playbook is complete"}, dec)
		r.markFinished()
		s.finish(r, PhaseDone, "")
		s.notify(r, finishLine(r, "done", r.playbook.Name+" is complete"))
		s.persist(r)
		return sys1Act, nil
	default:
		if _, err := r.checkRun(dec.Next); err != nil {
			return s.escalateDecision(r, dec, err)
		}
		s.recordSynthetic(r, "run_step", map[string]any{"command": dec.Next}, dec)
		outcome, err := s.runPlaybookStep(ctx, r, dec.Next)
		if err != nil {
			return sys1None, err
		}
		s.appendToolResult(r, pendingCall{ID: syntheticCallID, Name: "run_step"}, outcome, outcome.Status != string(wfe.OutcomeOK))
		s.persist(r)
		return sys1Act, nil
	}
}

// escalateDecision hands a policy-rejected decision over.
//
// A decision the policy does not allow is not an error: the model
// simply picked something the playbook forbids, and somebody else
// has to decide.
func (s *Service) escalateDecision(r *run, dec decision, cause error) (sys1Action, error) {
	s.logger.Info("me: the decision was not allowed", "run_id", r.id, "choice", dec.Next, "err", cause)
	s.appendHint(r, decision{Next: optionEscalate, Options: dec.Options, Model: dec.Model,
		NextConfidence: dec.NextConfidence, NeedsHuman: dec.NeedsHuman})
	return sys1Escalate, nil
}

// actOnChoice carries out a command the user picked.
//
// It reports whether the run is over. A choice the policy refuses is
// reported to the observer and asked again — the user picked something
// the playbook does not allow, not a reason to end the run.
func (s *Service) actOnChoice(ctx context.Context, r *run, command, by string) (bool, error) {
	if command == optionEscalate {
		return false, nil
	}
	if command == optionDone {
		if err := r.checkFinish("done"); err != nil {
			s.notify(r, "⚠️ "+err.Error())
			return false, nil
		}
		r.markFinished()
		s.finish(r, PhaseDone, "")
		s.notify(r, finishLine(r, "done", r.playbook.Name+" is complete"))
		s.persist(r)
		return true, nil
	}
	if _, err := r.checkRun(command); err != nil {
		s.notify(r, "⚠️ "+err.Error())
		return false, nil
	}
	s.recordSynthetic(r, "run_step", map[string]any{"command": command},
		decision{Next: command, Model: by, NextConfidence: 1})
	outcome, err := s.runPlaybookStep(ctx, r, command)
	if err != nil {
		return false, err
	}
	s.appendToolResult(r, pendingCall{ID: syntheticCallID, Name: "run_step"}, outcome, outcome.Status != string(wfe.OutcomeOK))
	s.persist(r)
	return false, nil
}

// appendHint attaches a decision to the conversation, so the chat
// model knows what the decision model thought.
func (s *Service) appendHint(r *run, dec decision) {
	if dec.Model == "" && dec.Next == "" {
		return
	}
	var b strings.Builder
	b.WriteString("system-1 hints: ")
	if dec.Next != "" {
		fmt.Fprintf(&b, "next=%s (p=%.2f)", dec.Next, dec.NextConfidence)
	}
	if dec.NeedsHuman > 0 {
		fmt.Fprintf(&b, " needs_human=%.2f", dec.NeedsHuman)
	}
	if dec.StepSucceeded != nil {
		fmt.Fprintf(&b, " step_succeeded=%t", *dec.StepSucceeded)
	}
	r.appendMessage(llm.Message{
		Role:  llm.RoleUser,
		Parts: []llm.Part{{Type: llm.PartText, Text: b.String()}},
	})
}
