package me

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

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
// The four kinds answer to four places: a message goes to the work
// chat, a shell command runs in the work chat's directory, an action
// runs through the action registry, and a wait runs out the clock.
func (s *Service) executeStep(ctx context.Context, r *run, job string, step wfe.Step) (stepVerdict, error) {
	// A step may run for a long time: the state is written first, so a
	// daemon that goes away in the middle of it comes back and runs the
	// step again.
	s.persist(r)
	switch step.Kind() {
	case wfe.StepKindMessage:
		return s.executeMessage(ctx, r, job, step)
	case wfe.StepKindShell:
		return s.executeShell(ctx, r, job, step)
	case wfe.StepKindUse:
		return s.executeUse(ctx, r, job, step)
	case wfe.StepKindWait:
		return s.executeWait(ctx, r, job, step)
	}
	return stepVerdict{}, fmt.Errorf("the step %q names no kind", step.ID)
}

// executeMessage sends the step's text to the work chat and waits for
// the outcome.
//
// A command that could not run at all ends the step as failed: the
// plan cannot go on by itself, so the agent decides what to do next.
func (s *Service) executeMessage(ctx context.Context, r *run, job string, step wfe.Step) (stepVerdict, error) {
	text := s.evalText(r, step.Message)
	res, err := s.runCommand(ctx, r, text, false)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return stepVerdict{}, err
		}
		out := wfe.Outcome{Status: wfe.OutcomeFailed, Summary: err.Error()}
		r.record(wfe.ActionKindRun, text, step.ID, out)
		s.notifyStep(r, text, out)
		return stepVerdict{Outcome: out, Settled: true}, nil
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

// executeShell runs the step's shell command in the work chat's
// current directory.
//
// The directory moves when a command moves it — `/gtw fix` moves the
// chat into the worktree — so a step that follows it runs inside that
// worktree.
//
// The command is bound to the run: `/me stop` and the daemon's
// shutdown stop the child instead of leaving it behind.
func (s *Service) executeShell(ctx context.Context, r *run, job string, step wfe.Step) (stepVerdict, error) {
	command := s.evalText(r, step.Shell)
	if s.cfg.Shell == nil {
		out := wfe.Outcome{
			Status:  wfe.OutcomeFailed,
			Summary: fmt.Sprintf("the step %q needs a shell runner, and the daemon has none", step.ID),
		}
		r.record(wfe.ActionKindRun, command, step.ID, out)
		s.notifyStep(r, command, out)
		return stepVerdict{Outcome: out, Settled: true}, nil
	}
	s.notify(r, "▶ "+truncate(command, 80))

	execCtx, cancel := r.runContext(ctx)
	defer cancel()
	res, err := s.cfg.Shell.RunShell(execCtx, wfe.ShellSpec{
		Cwd:     s.workDir(r),
		Command: command,
		Env:     mergeEnv(r.env(), step.Env),
	})
	if err != nil && execCtx.Err() != nil {
		// The run was cancelled: the step has no verdict of its own.
		return stepVerdict{}, execCtx.Err()
	}
	out := wfe.Outcome{Status: wfe.OutcomeOK, Summary: "the shell command ran"}
	if res != nil {
		out.Data = shellOutputs(res)
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

// workDir returns the directory the shell steps run in: the work
// chat's current directory, or the workspace the run started in when
// the daemon cannot tell.
func (s *Service) workDir(r *run) string {
	if s.cfg.Cwd != nil {
		if dir := s.cfg.Cwd.Cwd(r.workChatID); dir != "" {
			return dir
		}
	}
	return r.cwd
}

// maxShellOutput is how much of a shell command's output a step keeps.
//
// A build log runs to megabytes. The step's outputs go into the run
// state and into the decision model's request, and neither wants the
// whole log.
const maxShellOutput = 4 << 10

// shellOutputs renders what a shell command produced as the facts an
// expression reads: the exit code, the output, the error output and
// the key=value lines of the output.
//
// The reserved facts win: a command that prints `exit_code=0` does not
// get to report an exit code it never had.
func shellOutputs(res *wfe.ShellResult) map[string]string {
	out := make(map[string]string, len(res.Outputs)+3)
	for k, v := range res.Outputs {
		out[k] = v
	}
	out["exit_code"] = strconv.Itoa(res.ExitCode)
	out["stdout"] = truncate(res.Stdout, maxShellOutput)
	out["stderr"] = truncate(res.Stderr, maxShellOutput)
	return out
}

// executeWait waits out the step's duration and reports ok.
//
// The wait belongs to the run: `/me stop` and the daemon's shutdown
// end it at once.
func (s *Service) executeWait(ctx context.Context, r *run, job string, step wfe.Step) (stepVerdict, error) {
	d, err := time.ParseDuration(step.Wait)
	if err != nil {
		return stepVerdict{}, fmt.Errorf("the step %q waits for %q, which is not a duration", step.ID, step.Wait)
	}
	s.notify(r, "⏳ waiting "+d.String())

	execCtx, cancel := r.runContext(ctx)
	defer cancel()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-execCtx.Done():
		return stepVerdict{}, execCtx.Err()
	}
	out := wfe.Outcome{Status: wfe.OutcomeOK, Summary: "waited " + d.String()}
	r.record(wfe.ActionKindRun, "wait "+d.String(), step.ID, out)
	s.notifyStep(r, "wait "+d.String(), out)
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
	if err != nil && execCtx.Err() != nil {
		// The run was cancelled: the step has no verdict of its own.
		return stepVerdict{}, execCtx.Err()
	}
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
