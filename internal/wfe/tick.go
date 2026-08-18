package wfe

import (
	"context"
	"errors"
	"fmt"
)

// Tick advances the run by exactly one step. The state machine is
// intentionally synchronous: bot calls Tick in a loop, each call
// either advances to the next step or terminates the run. Pure
// (no I/O) except via Runtime.
//
// Termination conditions (state.Status moves out of running):
//   - all jobs done             → StatusSucceeded
//   - step failed                → StatusFailed
//   - ctx cancelled              → StatusCancelled
//   - no runnable step remains   → StatusSucceeded (caller may treat
//                                   as no-op if all jobs were skipped)
func Tick(ctx context.Context, state *RunState, wf *Workflow, rt Runtime) (*RunState, error) {
	if state == nil {
		return nil, errors.New("wfe: nil state")
	}
	if state.Status != StatusRunning {
		// Idempotent: already terminated, return as-is.
		return state, nil
	}
	state.UpdatedAt = rt.Now()

	step, jobName, ok := nextStep(wf, state)
	if !ok {
		// Nothing more to do. Either all done or all blocked.
		state.Status = StatusSucceeded
		return state, nil
	}
	state.CurrentJob = jobName
	state.CurrentStep = step.ID

	// Evaluate the if condition; skip if false.
	if step.If != "" {
		ec := buildExprCtx(state)
		ok, err := EvalCond(step.If, ec, rt)
		if err != nil {
			return state, wrapStepErr(step.ID, err)
		}
		if !ok {
			// Step skipped via if: false — mark as ran (empty
			// outputs) so nextStep doesn't return it again.
			if state.StepOutputs == nil {
				state.StepOutputs = map[string]map[string]string{}
			}
			state.StepOutputs[step.ID] = map[string]string{}
			if _, _, stillHasStep := nextStep(wf, state); !stillHasStep {
				state.Status = StatusSucceeded
			}
			return state, nil
		}
	}

	// Step env: merge workflow/run env with step-level env.
	stepEnv := mergeEnv(state.Env, step.Env)

	// Dispatch on step kind.
	var (
		outputs map[string]string
		err     error
	)
	switch step.Kind() {
	case StepKindRun:
		cmd := EvalString(step.Run, buildExprCtx(state), rt)
		var r *ShellResult
		r, err = rt.RunShell(ctx, ShellSpec{
			Cwd:     state.Workspace,
			Command: cmd,
			Env:     stepEnv,
			Shell:   step.Shell,
		})
		if r != nil {
			outputs = r.Outputs
		}

	case StepKindPrompt:
		prompt := EvalString(step.Prompt, buildExprCtx(state), rt)
		agent := step.Agent
		if agent == "" {
			agent = wf.Agent
		}
		var r *Reply
		r, err = rt.SendPrompt(ctx, PromptSpec{
			ChatID:  state.ChatID,
			Agent:   agent,
			Prompt:  prompt,
			Env:     stepEnv,
			Timeout: defaultPromptTimeout,
		})
		if r != nil {
			outputs = stringifyOutputs(r.Outputs)
			if outputs == nil {
				outputs = map[string]string{"text": r.Text}
			} else {
				outputs["text"] = r.Text
			}
		}

	case StepKindUse:
		with := EvalMap(step.With, buildExprCtx(state), rt)
		var r *ActionResult
		r, err = rt.RunAction(ctx, ActionSpec{
			Name: step.Use,
			With: with,
			Env:  stepEnv,
		})
		if r != nil {
			outputs = stringifyAnyMap(r.Outputs)
		}
	}

	if err != nil {
		if step.ContinueOnError {
			if outputs == nil {
				outputs = map[string]string{}
			}
			outputs["error"] = err.Error()
		} else {
			state.Status = StatusFailed
			if state.StepOutputs == nil {
				state.StepOutputs = map[string]map[string]string{}
			}
			state.StepOutputs[step.ID] = map[string]string{"error": err.Error()}
			return state, fmt.Errorf("%w: step %q: %v", ErrStepFailed, step.ID, err)
		}
	}

	if state.StepOutputs == nil {
		state.StepOutputs = map[string]map[string]string{}
	}
	if outputs != nil {
		state.StepOutputs[step.ID] = outputs
	} else {
		// Step ran successfully but produced no outputs (e.g.,
		// shell with no key=value lines). Mark it ran with an
		// empty map so nextStep doesn't return it again.
		state.StepOutputs[step.ID] = map[string]string{}
	}
	if state.Attempts == nil {
		state.Attempts = map[string]int{}
	}
	state.Attempts[step.ID]++
	markStepRan(state, step.ID)

	// Re-check: is the run now complete? If no more runnable steps
	// remain, mark succeeded.
	if _, _, stillHasStep := nextStep(wf, state); !stillHasStep {
		state.Status = StatusSucceeded
	}
	return state, nil
}

// nextStep finds the next runnable step. A step is runnable when:
//   - its job's needs are all satisfied (all steps in those jobs
//     have produced output)
//   - it has not run yet (no entry in StepOutputs for its ID, or
//     its `if:` was false last time)
//
// Returns (step, jobName, true) or (_, _, false) when no runnable
// step remains.
func nextStep(wf *Workflow, state *RunState) (Step, string, bool) {
	completedJobs := map[string]bool{}
	for jobName, job := range wf.Jobs {
		if jobDone(job, state) {
			completedJobs[jobName] = true
		}
	}
	for _, jobName := range wf.jobOrder {
		job := wf.Jobs[jobName]
		// Skip jobs whose needs aren't all done.
		ready := true
		for _, need := range job.Needs {
			if !completedJobs[need] {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		for _, step := range job.Steps {
			if _, ran := state.StepOutputs[step.ID]; ran {
				continue // already ran
			}
			return step, jobName, true
		}
		// Job done with no step to run? Shouldn't happen (validate
		// requires >= 1 step), but defensive.
	}
	return Step{}, "", false
}

func jobDone(job Job, state *RunState) bool {
	for _, step := range job.Steps {
		if _, ok := state.StepOutputs[step.ID]; !ok {
			return false
		}
	}
	return true
}

func markStepRan(state *RunState, stepID string) {
	// No-op state mutation marker; the caller (Tick) already wrote
	// outputs to StepOutputs[stepID]. This function exists so the
	// control flow is explicit; if we ever add post-step hooks they
	// go here.
	_ = state
	_ = stepID
}

func buildExprCtx(state *RunState) ExprCtx {
	ec := ExprCtx{
		Event: state.Event.Data,
		Steps: state.StepOutputs,
		Needs: map[string]map[string]string{},
		Env:   state.Env,
		Now:   state.UpdatedAt,
	}
	// Populate Needs from StepOutputs: each "completed" job's last
	// step's outputs are exposed as needs.<jobname>.* in expressions.
	// For v0 we surface every completed step's outputs; a more
	// sophisticated resolver (last step, or named outputs) can come later.
	for k, v := range state.StepOutputs {
		ec.Needs[k] = v
	}
	return ec
}

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

func stringifyOutputs(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

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
			// Flatten one level deep: out["k.sub"] = "..."
			for kk, vv := range x {
				if s, ok := vv.(string); ok {
					out[k+"."+kk] = s
				}
			}
		default:
			out[k] = fmt.Sprintf("%v", v)
		}
	}
	return out
}

func wrapStepErr(stepID string, err error) error {
	return fmt.Errorf("wfe: step %q: %w", stepID, err)
}

// defaultPromptTimeout is the v0 default for prompt step timeout.
// Matches the agent session default; can be overridden later.
const defaultPromptTimeout = 30 * 60 * 1_000_000_000 // 30 min in ns
