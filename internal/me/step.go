package me

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/pkg/wfe"
)

// stepVerdict is what a step produced.
type stepVerdict struct {
	// Outcome is the verdict of the step.
	Outcome wfe.Outcome
	// Settled is false while the step is still running: it has no
	// verdict yet, so the run state must not record it.
	Settled bool
	// Transcript is what the coding agent said, when the step talked
	// to it.
	Transcript string
	// Failure is why the attempt could not run at all: the chat could
	// not take the command, or the attempt had no agent to work with.
	// It is empty when the command ran.
	Failure error
}

// executeStep runs one step of the workflow.
//
// The four kinds answer to four places: a message goes to the the
// chat, a shell command runs in the chat's directory, an action
// runs through the action registry, and a wait runs out the clock.
func (s *Service) executeStep(ctx context.Context, r *run, job string, step wfe.Step) (stepVerdict, error) {
	// The run knows which step is running: the operator's commands act
	// on the step the run is on.
	r.setRunningStep(step.ID)
	defer r.clearRunningStep()
	// The step runs where the chat stands: the run's declared context
	// follows the chat before the step begins.
	s.syncWorkDir(r)
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

// executeMessage sends the step's text to the chat and waits for
// the outcome.
//
// The step may name a chain of agents: the attempts run in order, and
// an attempt that breaks hands over to the next one. The first
// attempt that does not break is the step's outcome.
//
// A chain that ran out of attempts ends the step as failed: the plan
// cannot go on by itself, so the agent decides what to do next.
func (s *Service) executeMessage(ctx context.Context, r *run, job string, step wfe.Step) (stepVerdict, error) {
	base := s.evalText(r, step.Message)
	verdict, _, err := s.runMessage(ctx, r, job, step, base)
	if err != nil {
		return stepVerdict{}, err
	}
	return verdict, nil
}

// runMessage runs a step's message and the chain of agents the step
// names.
//
// The base is the text the attempts are built from: the step's own
// message, or the command the decision model issued for it.
func (s *Service) runMessage(ctx context.Context, r *run, job string, step wfe.Step, base string) (stepVerdict, stepResult, error) {
	if err := checkArgsMessage(step, base); err != nil {
		out := failedOutcome(err.Error(), nil)
		r.record(wfe.ActionKindRun, base, step.ID, out)
		s.refreshRun(r)
		return stepVerdict{Outcome: out, Settled: true, Failure: err}, stepResult{}, nil
	}
	attempts := s.composeAttempts(r, step, base)
	return s.runAttempts(ctx, r, job, step, attempts)
}

// attempt is one run of a step's message with one agent.
type attempt struct {
	// mode is how the agent is applied: "use" or "args".
	mode string
	// agent is the agent of the attempt.
	agent string
	// command is what the run speaks into the chat.
	command string
	// recorded is what the action history keeps: the command, plus the
	// agent when the command itself does not name it.
	recorded string
}

// composeAttempts composes the attempts of a step's message.
//
// The entries run in the order they are declared, and the names of an
// entry in the order they are declared. A step that names no agent has
// one attempt: its message, unchanged.
func (s *Service) composeAttempts(r *run, step wfe.Step, base string) []attempt {
	if len(step.Agents) == 0 {
		return []attempt{{command: base, recorded: base}}
	}
	out := make([]attempt, 0, len(step.Agents))
	for _, entry := range step.Agents {
		for _, name := range entry.Names {
			agent := strings.TrimSpace(s.evalText(r, name))
			if entry.Mode == wfe.AgentModeArgs {
				command := base + " -a " + agent
				out = append(out, attempt{
					mode:     entry.Mode,
					agent:    agent,
					command:  command,
					recorded: command,
				})
				continue
			}
			out = append(out, attempt{
				mode:     entry.Mode,
				agent:    agent,
				command:  base,
				recorded: base + " [agent: " + agent + "]",
			})
		}
	}
	return out
}

// runAttempts runs the attempts of a step's message in order.
//
// The attempts run in the order they are composed. An attempt that
// broke hands over to the next one; a cancelled attempt ends the
// chain. The first attempt that did not break is the step's verdict.
//
// Every attempt is recorded against the step. The error is why the
// run cannot go on: the run was cancelled.
func (s *Service) runAttempts(ctx context.Context, r *run, job string, step wfe.Step, attempts []attempt) (stepVerdict, stepResult, error) {
	var (
		verdict stepVerdict
		res     stepResult
	)
	for _, a := range attempts {
		v, raw, err := s.runAttempt(ctx, r, job, step, a)
		if err != nil {
			return stepVerdict{}, stepResult{}, err
		}
		verdict, res = v, raw
		if !v.Settled {
			// The command is still running: it has no verdict yet.
			return verdict, res, nil
		}
		if !fallsThrough(v.Outcome) {
			return verdict, res, nil
		}
	}
	return verdict, res, nil
}

// runAttempt runs one attempt of a step's message.
//
// The chat's agent is switched for the attempt when the attempt names
// one, and the chat's own agent is put back when the attempt returns.
func (s *Service) runAttempt(ctx context.Context, r *run, job string, step wfe.Step, a attempt) (stepVerdict, stepResult, error) {
	if a.mode != "" && a.agent == "" {
		// The name of the agent resolved to nothing: the attempt has
		// no agent to work with, so it breaks.
		err := fmt.Errorf("the step %q names an agent that resolved to nothing", step.ID)
		return s.breakAttempt(r, step, a, err), stepResult{}, nil
	}
	restore, err := s.switchAgent(r, step, a)
	if err != nil {
		// The chat cannot work with that agent: the attempt breaks and
		// the chain moves on.
		return s.breakAttempt(r, step, a, err), stepResult{}, nil
	}
	if restore != nil {
		defer restore()
	}
	res, err := s.runCommand(ctx, r, a.command, false)
	// The decision about this step hangs under the message its command
	// became: the decision reads directly after the error it answers.
	r.rememberCommandMsg(step.ID, r.lastCommandMsgIDValue())
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return stepVerdict{}, stepResult{}, err
		}
		out := failedOutcome(err.Error(), nil)
		r.record(wfe.ActionKindRun, a.recorded, step.ID, out)
		s.refreshRun(r)
		return stepVerdict{Outcome: out, Settled: true, Failure: err}, stepResult{}, nil
	}
	if res.Status == statusWaiting {
		// The command is still running: it has no verdict yet, so the
		// run state records the step as pending and the agent decides
		// what to do about it.
		r.recordPending(job, step)
		return stepVerdict{Outcome: wfe.Outcome{Status: wfe.OutcomeUnknown, Summary: res.Summary}}, res, nil
	}
	out := wfe.Outcome{
		Status:  outcomeStatus(res.Status),
		Summary: res.Summary,
		Data:    res.Data,
	}
	r.record(wfe.ActionKindRun, a.recorded, step.ID, out)
	return stepVerdict{Outcome: out, Settled: true, Transcript: res.Transcript}, res, nil
}

// breakAttempt records an attempt that could not run and returns its
// verdict.
//
// The attempt breaks: the chain moves on to the next agent, and the
// last attempt that broke is the step's outcome.
func (s *Service) breakAttempt(r *run, step wfe.Step, a attempt, err error) stepVerdict {
	out := failedOutcome(err.Error(), nil)
	r.record(wfe.ActionKindRun, a.recorded, step.ID, out)
	s.refreshRun(r)
	return stepVerdict{Outcome: out, Settled: true, Failure: err}
}

// switchAgent switches the chat's agent for the attempt.
//
// The switch is process-local: the chat store keeps the agent the
// operator picked. The function it returns puts the chat's own agent
// back.
func (s *Service) switchAgent(r *run, step wfe.Step, a attempt) (func(), error) {
	if a.mode != wfe.AgentModeUse {
		return nil, nil
	}
	if s.cfg.AgentSwitcher == nil {
		return nil, fmt.Errorf("the step %q needs an agent switcher, and the daemon has none", step.ID)
	}
	return s.cfg.AgentSwitcher.SwitchAgent(r.chatID, a.agent)
}

// fallsThrough reports whether the outcome hands over to the next
// attempt of the chain.
//
// The exception axis is what tells a normal result from a breakage. A
// cancelled outcome ends the chain instead: the run is stopping, and
// another agent would run work nobody asked for.
func fallsThrough(out wfe.Outcome) bool {
	if out.Status == wfe.OutcomeCancelled {
		return false
	}
	return out.Exception()
}

// checkArgsMessage refuses a message the args mode cannot carry.
//
// The args mode puts the agent on the command line, and only a
// command takes one. The message is checked again when the step runs:
// the load-time check sees the expression, the command dispatcher sees
// what it became.
func checkArgsMessage(step wfe.Step, message string) error {
	for _, entry := range step.Agents {
		if entry.Mode != wfe.AgentModeArgs {
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(message), "/") {
			return fmt.Errorf("the args mode needs a command: %q is not a slash command", message)
		}
	}
	return nil
}

// executeShell runs the step's shell command in the chat's
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
		out := failedOutcome(fmt.Sprintf("the step %q needs a shell runner, and the daemon has none", step.ID), nil)
		r.record(wfe.ActionKindRun, command, step.ID, out)
		s.refreshRun(r)
		return stepVerdict{Outcome: out, Settled: true}, nil
	}

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
		summary := err.Error()
		if res != nil && res.Stderr != "" {
			summary = truncate(res.Stderr, 400)
		}
		out = failedOutcome(summary, out.Data)
	}
	r.record(wfe.ActionKindRun, command, step.ID, out)
	s.refreshRun(r)
	return stepVerdict{Outcome: out, Settled: true}, nil
}

// workDir returns the directory the run acts in: the chat's current
// directory, or the directory the run last knew when the daemon
// cannot tell.
//
// The chat moves when a command moves it — `/gtw fix` moves the chat
// into the worktree — so the directory is read when it is used, not
// when the run started.
func (s *Service) workDir(r *run) string {
	if dir := s.liveDir(r); dir != "" {
		return dir
	}
	return r.workCwd()
}

// liveDir returns the directory the chat currently works in, or an
// empty string when the daemon cannot tell.
func (s *Service) liveDir(r *run) string {
	if s.cfg.Cwd == nil {
		return ""
	}
	return s.cfg.Cwd.Cwd(r.chatID)
}

// syncWorkDir re-initializes the run's declared context to the
// directory the chat works in.
//
// The run's own record of where it works — its directory, the
// workspace of its state and the environment its steps run with —
// has to follow the chat: a run that works inside a worktree and
// describes the repository it started in decides on false facts.
//
// A directory the daemon cannot tell leaves the run as it is.
func (s *Service) syncWorkDir(r *run) {
	if !r.setWorkCwd(s.liveDir(r)) {
		return
	}
	// The report the model reads names the directory and the
	// repository facts of where the run was: a chat that moved makes
	// the report stale.
	r.markSituation()
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
// get to report an exit code it never had. The error description is
// reserved too: it is the axis the plan reads to tell a normal result
// from a breakage, so a command that prints `error=...` does not get
// to claim it.
func shellOutputs(res *wfe.ShellResult) map[string]string {
	out := make(map[string]string, len(res.Outputs)+3)
	for k, v := range res.Outputs {
		out[k] = v
	}
	delete(out, wfe.OutcomeKeyError)
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
	if err := waitFor(ctx, r, d); err != nil {
		return stepVerdict{}, err
	}
	out := wfe.Outcome{Status: wfe.OutcomeOK, Summary: "waited " + d.String()}
	r.record(wfe.ActionKindRun, "wait "+d.String(), step.ID, out)
	s.refreshRun(r)
	return stepVerdict{Outcome: out, Settled: true}, nil
}

// executeUse runs the action the step names.
//
// The action is bound to the run: `/me stop` and the daemon's shutdown
// stop the script instead of leaving it behind.
func (s *Service) executeUse(ctx context.Context, r *run, job string, step wfe.Step) (stepVerdict, error) {
	name := s.evalText(r, step.Use)
	with := wfe.EvalMap(step.With, r.exprCtx())

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
	var data map[string]string
	if res != nil {
		data = stringifyAnyMap(res.Outputs)
	}
	out := wfe.Outcome{Status: wfe.OutcomeOK, Summary: "the action " + name + " ran", Data: data}
	if err != nil {
		out = failedOutcome(err.Error(), data)
	}
	r.record(wfe.ActionKindRun, name, step.ID, out)
	s.refreshRun(r)
	return stepVerdict{Outcome: out, Settled: true}, nil
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
//
// The error description is reserved: it is the axis the plan reads to
// tell a normal result from a breakage, so an action that reports an
// `error` output does not get to claim it.
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
	delete(out, wfe.OutcomeKeyError)
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
