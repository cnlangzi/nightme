package me

import (
	"context"
	"fmt"

	"github.com/cnlangzi/nightme/internal/wfe"
)

// stepVerdict is what a step produced.
type stepVerdict struct {
	// Outcome is the verdict of the step.
	Outcome wfe.Outcome
	// Settled is false while the step is still running: it has no
	// verdict yet, so the run state must not record it.
	Settled bool
}

// executeStep runs one step of the workflow.
//
// The three kinds answer to three places: a message goes to the work
// chat, a shell command runs in the workspace, an action runs through
// the action registry.
func (s *Service) executeStep(ctx context.Context, r *run, job string, step wfe.Step) (stepVerdict, error) {
	switch step.Kind() {
	case wfe.StepKindMessage:
		return s.executeMessage(ctx, r, job, step)
	case wfe.StepKindShell:
		return s.executeShell(ctx, r, job, step)
	case wfe.StepKindUse:
		return s.executeUse(ctx, r, job, step)
	}
	return stepVerdict{}, fmt.Errorf("the step %q names no kind", step.ID)
}

// executeMessage sends the step's text to the work chat and waits for
// the outcome.
func (s *Service) executeMessage(ctx context.Context, r *run, job string, step wfe.Step) (stepVerdict, error) {
	text := s.evalText(r, step.Message)
	res, err := s.runCommand(ctx, r, text, false)
	if err != nil {
		return stepVerdict{}, err
	}
	if res.Status == statusWaiting {
		// The command is still running: it has no verdict yet, so the
		// run state records the step as pending and the agent decides
		// what to do about it.
		r.recordPending(job, step)
		return stepVerdict{Outcome: wfe.Outcome{Status: wfe.OutcomeUnknown, Summary: res.Summary}}, nil
	}
	out := wfe.Outcome{
		Status:  outcomeStatus(res.Status),
		Summary: res.Summary,
		Data:    res.Data,
	}
	r.record(wfe.ActionKindRun, text, step.ID, out)
	return stepVerdict{Outcome: out, Settled: true}, nil
}

// executeShell runs the step's shell command in the workspace.
//
// The command is bound to the run: `/me stop` and the daemon's
// shutdown stop the child instead of leaving it behind.
func (s *Service) executeShell(ctx context.Context, r *run, job string, step wfe.Step) (stepVerdict, error) {
	if s.cfg.Shell == nil {
		return stepVerdict{}, fmt.Errorf("the step %q needs a shell runner, and the daemon has none", step.ID)
	}
	command := s.evalText(r, step.Shell)
	s.notify(r, "▶ "+truncate(command, 80))

	execCtx, cancel := r.runContext(ctx)
	defer cancel()
	res, err := s.cfg.Shell.RunShell(execCtx, wfe.ShellSpec{
		Cwd:     r.cwd,
		Command: command,
		Env:     mergeEnv(r.env(), step.Env),
	})
	out := wfe.Outcome{Status: wfe.OutcomeOK, Summary: "the shell command ran"}
	if res != nil {
		out.Data = res.Outputs
		out.Summary = fmt.Sprintf("the shell command exited with %d", res.ExitCode)
	}
	if err != nil {
		out.Status = wfe.OutcomeFailed
		out.Summary = err.Error()
		if res != nil && res.Stderr != "" {
			out.Summary = truncate(res.Stderr, 400)
		}
	}
	r.record(wfe.ActionKindRun, command, step.ID, out)
	s.notifyStep(r, command, out)
	return stepVerdict{Outcome: out, Settled: true}, nil
}

// executeUse runs the action the step names.
//
// The action is bound to the run: `/me stop` and the daemon's shutdown
// stop the script instead of leaving it behind.
func (s *Service) executeUse(ctx context.Context, r *run, job string, step wfe.Step) (stepVerdict, error) {
	name := s.evalText(r, step.Use)
	with := wfe.EvalMap(step.With, r.exprCtx())
	s.notify(r, "▶ "+name)

	execCtx, cancel := r.runContext(ctx)
	defer cancel()
	res, err := s.actions.Run(execCtx, wfe.ActionSpec{
		Name: name,
		With: with,
		Env:  mergeEnv(r.env(), step.Env),
	})
	out := wfe.Outcome{Status: wfe.OutcomeOK, Summary: "the action " + name + " ran"}
	if res != nil {
		out.Data = stringifyAnyMap(res.Outputs)
	}
	if err != nil {
		out.Status = wfe.OutcomeFailed
		out.Summary = err.Error()
	}
	r.record(wfe.ActionKindRun, name, step.ID, out)
	s.notifyStep(r, name, out)
	return stepVerdict{Outcome: out, Settled: true}, nil
}

// notifyStep posts the progress of a step that is not a message.
func (s *Service) notifyStep(r *run, label string, out wfe.Outcome) {
	switch out.Status {
	case wfe.OutcomeOK, wfe.OutcomeNoop:
		s.notify(r, "✅ "+label)
	case wfe.OutcomeFailed:
		s.notify(r, "❌ "+label+" failed")
	default:
		s.notify(r, "❓ "+label+" finished with no verdict")
	}
}

// evalText resolves the expressions of a step's text.
//
// The context is the run state: the steps that ran and what they
// produced.
func (s *Service) evalText(r *run, text string) string {
	return wfe.EvalString(text, r.exprCtx())
}

// stringifyAnyMap renders the outputs of an action as the facts an
// expression reads.
func stringifyAnyMap(m map[string]any) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch x := v.(type) {
		case string:
			out[k] = x
		case map[string]any:
			for kk, vv := range x {
				if s, ok := vv.(string); ok {
					out[k+"."+kk] = s
					continue
				}
				out[k+"."+kk] = fmt.Sprintf("%v", vv)
			}
		default:
			out[k] = fmt.Sprintf("%v", v)
		}
	}
	return out
}

// mergeEnv overlays override onto base.
func mergeEnv(base, override map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}
