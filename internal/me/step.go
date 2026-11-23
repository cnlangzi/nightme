package me

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/pkg/outcome"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// stepVerdict is what a step produced.
type stepVerdict struct {
	// Outcome is the verdict of the step.
	Outcome outcome.Outcome
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
	return s.runAttempts(ctx, r, job, step, base)
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

// chainAgents returns the chain of a step's message: the agents of
// every entry, in the order the entries and their names are declared.
//
// The command of an attempt is not filled in yet: what the attempt
// runs depends on the attempt before it.
//
// A step that names no agent has no chain: its message runs once,
// unchanged.
func (s *Service) chainAgents(r *run, step wfe.Step) []attempt {
	if len(step.Agents) == 0 {
		return nil
	}
	out := make([]attempt, 0, len(step.Agents))
	for _, entry := range step.Agents {
		for _, name := range entry.Names {
			out = append(out, attempt{
				mode:  entry.Mode,
				agent: strings.TrimSpace(s.evalText(r, name)),
			})
		}
	}
	return out
}

// runAttempts runs the chain of agents of a step's message.
//
// The attempts are composed one at a time: the command of an attempt
// depends on what the attempt before it left behind. An attempt that
// broke hands over to the next one; a cancelled attempt ends the
// chain. The first attempt that did not break is the step's verdict.
//
// A handover carries the work that is left: an attempt that reported
// how much of its work it did not finish hands that much over, and
// the next agent finishes it instead of running the whole thing
// again. An attempt that reported no count leaves nothing behind —
// the next agent runs the whole thing again.
//
// One execution of the step is one run of it: it costs one chance of
// the step's budget and it is recorded once, with the command of the
// attempt the chain ended on. An attempt the chain swallowed leaves
// nothing behind but a line in the daemon's log — its error is not the
// step's error.
func (s *Service) runAttempts(ctx context.Context, r *run, job string, step wfe.Step, base string) (stepVerdict, stepResult, error) {
	agents := s.chainAgents(r, step)
	if len(agents) == 0 {
		// The step names no agent: its message runs once, unchanged.
		agents = []attempt{{}}
	}
	var (
		verdict stepVerdict
		res     stepResult
		// last is the attempt the chain ended on: its command is the
		// command the chain records.
		last attempt
	)
	// unfinished is what the attempt before left behind: the number of
	// items it did not finish. Zero means there is nothing to hand
	// over.
	unfinished := 0
	for i := range agents {
		a := agents[i]
		switch a.mode {
		case wfe.AgentModeArgs:
			a.command = base + " -a " + a.agent
			if unfinished > 0 {
				// The work that is left is handed over: the next
				// agent finishes it instead of running it again.
				a.command = base + " --continue -a " + a.agent
			}
			a.recorded = a.command
		case wfe.AgentModeUse:
			// The use mode sends the message as it is: there is no
			// place to put the handover in it.
			a.command = base
			a.recorded = base + " [agent: " + a.agent + "]"
		default:
			// The step names no agent: its message runs once,
			// unchanged.
			a.command = base
			a.recorded = base
		}
		last = a
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
			break
		}
		if i+1 < len(agents) {
			// The attempt broke and another agent is left: the chain
			// swallows the attempt and tries the next one. The
			// attempt's error is not the step's error, so it leaves
			// no trace in the run but this line. The summary says why
			// it broke: the attempt that could not run at all carries
			// its reason there too.
			s.logger.Info("me: an attempt of the chain broke; the chain tries the next agent",
				"run_id", r.id,
				"step_id", step.ID,
				"command", a.command,
				"agent", a.agent,
				"next_agent", agents[i+1].agent,
				"status", v.Outcome.Status,
				"summary", v.Outcome.StatusText)
		}
		unfinished = unfinishedCount(v.Outcome)
	}
	r.record(wfe.ActionKindRun, last.recorded, step.ID, verdict.Outcome)
	s.refreshRun(r)
	// The chain's result decides whether the run pauses: an attempt the
	// chain swallowed does not hold the run up, and an attempt that is
	// still in flight has no verdict to act on yet.
	s.pauseForRateLimit(ctx, r, res)
	return verdict, res, nil
}

// unfinishedCount returns how many items an outcome left unfinished.
//
// The count is a fact the command reports: a command that finished
// part of its work says how much of it is left. An outcome without
// the count leaves nothing behind — the next agent runs the whole
// thing again.
func unfinishedCount(out outcome.Outcome) int {
	n, err := strconv.Atoi(out.Data[KeyUnfinished])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// runAttempt runs one attempt of a step's message.
//
// The chat's agent is switched for the attempt when the attempt names
// one, and the chat's own agent is put back when the attempt returns.
//
// The attempt records nothing: the chain records the run of the step,
// and an attempt the chain swallows leaves no trace in the run.
func (s *Service) runAttempt(ctx context.Context, r *run, job string, step wfe.Step, a attempt) (stepVerdict, stepResult, error) {
	if a.mode != "" && a.agent == "" {
		// The name of the agent resolved to nothing: the attempt has
		// no agent to work with, so it breaks.
		err := fmt.Errorf("the step %q names an agent that resolved to nothing", step.ID)
		return breakAttempt(err), stepResult{}, nil
	}
	restore, err := s.switchAgent(r, step, a)
	if err != nil {
		// The chat cannot work with that agent: the attempt breaks and
		// the chain moves on.
		return breakAttempt(err), stepResult{}, nil
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
		// The command could not run at all: the attempt breaks and the
		// chain moves on.
		return breakAttempt(err), stepResult{}, nil
	}
	if res.phase == phaseAbandoned || r.stepSkipped(step.ID) {
		// The step was passed over while its command was in flight: the
		// verdict that comes late does not put it back into the plan.
		// The step has no verdict of its own, and the run goes on to its
		// next step.
		return stepVerdict{Outcome: noVerdict(res.StatusText)}, res, nil
	}
	if res.phase == phaseWaiting {
		// The command is still running: it has no verdict yet, so the
		// run state records the step as pending and the agent decides
		// what to do about it.
		r.recordPending(job, step)
		return stepVerdict{Outcome: noVerdict(res.StatusText)}, res, nil
	}
	return stepVerdict{Outcome: res.Outcome, Settled: true, Transcript: res.Transcript}, res, nil
}

// breakAttempt returns the verdict of an attempt that could not run.
//
// The attempt breaks: the chain moves on to the next agent, and the
// last attempt that broke is the step's outcome.
func breakAttempt(err error) stepVerdict {
	out := failedOutcome(err.Error(), nil)
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
func fallsThrough(out outcome.Outcome) bool {
	if out.Status == outcome.StatusClientClosedRequest {
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
	out := outcome.Outcome{Success: true, Status: http.StatusOK, StatusText: "the shell command ran"}
	if res != nil {
		out.Data = shellOutputs(res)
		out.StatusText = fmt.Sprintf("the shell command exited with %d", res.ExitCode)
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
	out[KeyExitCode] = strconv.Itoa(res.ExitCode)
	out[KeyStdout] = truncate(res.Stdout, maxShellOutput)
	out[KeyStderr] = truncate(res.Stderr, maxShellOutput)
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
	out := outcome.Outcome{Success: true, Status: http.StatusOK, StatusText: "waited " + d.String()}
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
	out := outcome.Outcome{Success: true, Status: http.StatusOK, StatusText: "the action " + name + " ran", Data: data}
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
