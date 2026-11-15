package me

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/cnlangzi/nightme/internal/wfe"
)

// renderWorkflow renders the workflow as the model reads it.
//
// The rendering holds the facts the model needs to decide: the
// objective, the steps in the order they run, why a step is there, the
// commands a step allows and how often a step may try. The machinery
// of the workflow — its conditions, its environment, its workspaces and
// its triggers — is none of the model's business.
func (s *Service) renderWorkflow(r *run) string {
	r.mu.Lock()
	wf := r.workflow
	r.mu.Unlock()
	if wf == nil {
		return ""
	}
	var b strings.Builder
	if wf.Title != "" {
		fmt.Fprintf(&b, "objective: %s\n", wf.Title)
	}
	for _, name := range wf.JobOrder() {
		job := wf.Jobs[name]
		fmt.Fprintf(&b, "\njob %s — %s\n", name, roundsText(job))
		for _, step := range job.Steps {
			fmt.Fprintf(&b, "  %s (%s): %s\n", step.ID, stepKind(step), s.evalText(r, stepText(step)))
			if note := strings.TrimSpace(step.Note); note != "" {
				fmt.Fprintf(&b, "    note: %s\n", note)
			}
			if len(step.Allow) > 0 {
				fmt.Fprintf(&b, "    allowed commands: %s\n", strings.Join(step.Allow, ", "))
			}
			if step.Attempts > 0 {
				fmt.Fprintf(&b, "    limit: %d attempts in this round\n", step.Attempts)
			}
		}
	}
	return b.String()
}

// roundsText renders how many rounds a job runs.
func roundsText(job wfe.Job) string {
	switch rounds := job.Rounds(); {
	case rounds == 0:
		return "repeats until the run is stopped"
	case rounds == 1:
		return "runs once"
	default:
		return fmt.Sprintf("runs %d rounds", rounds)
	}
}

// stepKind renders the kind of a step.
func stepKind(step wfe.Step) string {
	switch step.Kind() {
	case wfe.StepKindMessage:
		return "message"
	case wfe.StepKindShell:
		return "shell"
	case wfe.StepKindUse:
		return "action"
	case wfe.StepKindWait:
		return "wait"
	}
	return "none"
}

// stepText renders what a step does.
func stepText(step wfe.Step) string {
	switch step.Kind() {
	case wfe.StepKindMessage:
		return step.Message
	case wfe.StepKindShell:
		return step.Shell
	case wfe.StepKindUse:
		return step.Use
	case wfe.StepKindWait:
		return step.Wait
	}
	return ""
}

// renderSituation renders where the run stands and why it needs the
// model.
//
// It is the report the model reads when it is called: the reason, the
// facts of the round, the outcome of the last action and what the
// transcript said about it.
func (s *Service) renderSituation(r *run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The run needs you: %s.\n", s.situationReason(r))

	if args := argLine(r.argNames, r.argVals); args != "" {
		fmt.Fprintf(&b, "args: %s\n", args)
	}
	job, round := r.brainIteration()
	if job != "" {
		fmt.Fprintf(&b, "round: %s, round %d\n", job, round)
	}
	if cwd := s.workDir(r); cwd != "" {
		fmt.Fprintf(&b, "directory: %s\n", cwd)
	}

	if steps := s.renderRoundSteps(r); steps != "" {
		b.WriteString(steps)
	}

	if action, ok := r.lastAction(); ok {
		out := action.Outcome
		fmt.Fprintf(&b, "\nlast outcome:\n  status: %s\n  summary: %s\n", out.Status, out.Summary)
		for k, v := range out.Data {
			fmt.Fprintf(&b, "  %s: %s\n", k, v)
		}
		if hint := reasonHint(out.Data["reason"]); hint != "" {
			fmt.Fprintf(&b, "  hint: %s\n", hint)
		}
	}
	if transcript := r.transcriptTail(); transcript != "" {
		b.WriteString("\ntranscript (tail):\n")
		b.WriteString(tailText(transcript, transcriptBytes))
		b.WriteString("\n")
	}
	return b.String()
}

// renderRoundSteps renders the steps of the round the run is on: where
// each of them stands and which one is ready.
func (s *Service) renderRoundSteps(r *run) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	wf := r.workflow
	if wf == nil {
		return ""
	}
	states := map[string]wfe.StepState{}
	for _, st := range r.state.Steps {
		states[st.ID] = st
	}
	remaining := r.policy.Remaining()
	ready := map[string]bool{}
	if steps, err := wfe.ReadySet(r.state, r.workflow); err == nil {
		for _, step := range steps {
			ready[step.ID] = true
		}
	}
	job, _ := r.brainIterationLocked()
	var b strings.Builder
	for _, name := range wf.JobOrder() {
		if job != "" && name != job {
			continue
		}
		for _, step := range wf.Jobs[name].Steps {
			fmt.Fprintf(&b, "  %s: %s", step.ID, stepStateText(states[step.ID]))
			if idx := r.stepIndex(step.ID); idx >= 0 && idx < len(remaining.Steps) && remaining.Steps[idx] >= 0 {
				fmt.Fprintf(&b, " (%d attempts left)", remaining.Steps[idx])
			}
			if ready[step.ID] {
				b.WriteString(" — ready")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// stepStateText renders where a step stands.
func stepStateText(st wfe.StepState) string {
	switch {
	case st.ID == "":
		return "not run yet"
	case st.Pending:
		return "still running"
	case st.Skipped:
		return "skipped"
	default:
		return string(st.Outcome.Status)
	}
}

// situationReason says why the run needs the model, in the words the
// model reads.
func (s *Service) situationReason(r *run) string {
	reason := strings.TrimSpace(r.judgmentReason())
	if reason == "" {
		return "the work cannot go on in the normal order"
	}
	switch {
	case prevStepReason.MatchString(reason):
		m := prevStepReason.FindStringSubmatch(reason)
		return fmt.Sprintf("step %s ended as %s", m[1], m[2])
	case lastStepReason.MatchString(reason):
		m := lastStepReason.FindStringSubmatch(reason)
		return fmt.Sprintf("the plan is over and step %s ended as %s", m[1], m[2])
	case readyStepsReason.MatchString(reason):
		m := readyStepsReason.FindStringSubmatch(reason)
		return fmt.Sprintf("%s steps are ready and the plan cannot pick one", m[1])
	case stillRunningReason.MatchString(reason):
		m := stillRunningReason.FindStringSubmatch(reason)
		return fmt.Sprintf("step %s is still running", m[1])
	case attemptsReason.MatchString(reason):
		m := attemptsReason.FindStringSubmatch(reason)
		return fmt.Sprintf("step %s used up its attempts", m[1])
	case jobsReason.MatchString(reason):
		m := jobsReason.FindStringSubmatch(reason)
		return fmt.Sprintf("%s jobs are ready to start another round", m[1])
	case strings.HasPrefix(reason, "the plan is exhausted"):
		return "every step of the plan has run"
	}
	return reason
}

// The reasons the engine reports, and the words they are said in.
var (
	prevStepReason     = regexp.MustCompile(`^the previous step "([^"]+)" ended as "([^"]+)"$`)
	lastStepReason     = regexp.MustCompile(`^the plan is exhausted and the last step "([^"]+)" ended as "([^"]+)"$`)
	readyStepsReason   = regexp.MustCompile(`^(\d+) steps are ready`)
	stillRunningReason = regexp.MustCompile(`^the step "([^"]+)" is still running$`)
	attemptsReason     = regexp.MustCompile(`^the step "([^"]+)" already ran \d+ times, its limit is \d+$`)
	jobsReason         = regexp.MustCompile(`^(\d+) jobs are ready to start another round$`)
)

// reasonHints explain the reason codes a command reports.
//
// The codes are stable (see docs/me.md); the hint says what the code
// means in the words of the work. Only the code in play is shown.
var reasonHints = map[string]string{
	"agent_error":           "the agent did not run, or it produced no usable output.",
	"not_committed":         "the agent finished, but nothing was committed.",
	"conflict":              "the working tree has unresolved conflicts.",
	"dirty":                 "the working tree has changes that were not committed.",
	"detached":              "HEAD is detached: the branch has to be checked out first.",
	"no_commits":            "the branch has no commit beyond its base.",
	"provider_error":        "the git platform could not be reached.",
	"dispatch_failed":       "the worktree was made, but the work never reached the agent.",
	"not_supported":         "the agent cannot do this.",
	"unknown_agent":         "the agent's name is not registered.",
	"no_agent":              "no agent is selected.",
	"no_remote":             "the repository has no origin remote.",
	"already_active":        "another fix is already running in this chat.",
	"workspace_unreachable": "the workspace directory is gone.",
}

// reasonHint explains a reason code, or returns an empty string when
// the code is not one the run knows.
func reasonHint(code string) string {
	if code == "" {
		return ""
	}
	return reasonHints[code]
}

// transcriptTail returns the tail of the last action's transcript.
func (r *run) transcriptTail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastTranscript
}
