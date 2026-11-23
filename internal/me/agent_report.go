package me

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"

	"github.com/cnlangzi/nightme/pkg/wfe"
)

// renderWorkflow renders the workflow as the model reads it.
//
// The rendering holds the facts the model needs to decide: the
// objective, the steps in the order they run, why a step is there and
// how often a step may try. The machinery of the workflow — its
// conditions, its environment, its workspaces and its triggers — is
// none of the model's business.
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
			if description := strings.TrimSpace(step.Description); description != "" {
				fmt.Fprintf(&b, "    description: %s\n", description)
			}
			if step.Final {
				b.WriteString("    final: it runs when the round ends, however the round ends\n")
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
	case wfe.StepKindCmd:
		return "cmd"
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
	case wfe.StepKindCmd:
		if step.Message != "" {
			return step.Message
		}
		// A cmd step that says nothing names the command it runs.
		return step.Cmd.Name
	}
	return ""
}

// renderSituation renders where the run stands and why it needs the
// model.
//
// It is the report the model reads when it is called: the reason, the
// facts of the round, what every step of the round produced, what the
// agents behind the last two actions said about it, the state of the
// repository and what the model may do now.
func (s *Service) renderSituation(ctx context.Context, r *run) string {
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
	if facts := s.renderRepository(ctx, r); facts != "" {
		b.WriteString(facts)
	}

	steps, rendered := s.renderRoundSteps(r)
	if steps != "" {
		b.WriteString(steps)
	}
	if repair := s.renderRepair(r); repair != "" {
		b.WriteString(repair)
	}
	if directives := s.renderDirectives(r); directives != "" {
		b.WriteString(directives)
	}

	// The last action's outcome: the step list above carries it when
	// the action was a step's own run. A repair the run issued leaves
	// its verdict on no step, so it is rendered here.
	if action, ok := r.lastVerdict(); ok && !carriesOutcome(rendered, action) {
		out := action.Outcome
		fmt.Fprintf(&b, "\nlast outcome:\n  status: %d\n  summary: %s\n", out.Status, out.StatusText)
		for _, k := range sortedNames(out.Data) {
			fmt.Fprintf(&b, "  %s: %s\n", k, out.Data[k])
		}
	}
	if transcripts := r.actionTranscripts(); len(transcripts) > 0 {
		b.WriteString("\ntranscripts (tail):\n")
		for _, t := range transcripts {
			fmt.Fprintf(&b, "\n--- %s ---\n", t.Command)
			b.WriteString(tailText(t.Text, transcriptBytes))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// renderRepository renders the state of the repository the run works
// in.
//
// The facts are the run's own: the agent should not spend a tool call
// on what the run already knows.
func (s *Service) renderRepository(ctx context.Context, r *run) string {
	if s.cfg.Git == nil {
		return ""
	}
	dir := s.workDir(r)
	if dir == "" {
		return ""
	}
	facts, err := s.repoFactsWithBase(ctx, dir, r.baseBranchOf(ctx, s, dir))
	if err != nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("repository:\n")
	if facts.Branch != "" {
		fmt.Fprintf(&b, "  branch: %s\n", facts.Branch)
	}
	fmt.Fprintf(&b, "  dirty: %t\n", facts.Dirty)
	if facts.Base != "" {
		fmt.Fprintf(&b, "  base: %s\n", facts.Base)
		fmt.Fprintf(&b, "  ahead of base: %d\n", facts.AheadOfBase)
		fmt.Fprintf(&b, "  behind base: %d\n", facts.BehindBase)
	}
	return b.String()
}

// renderRepair renders what the model has to repair and what it has
// left to do it with.
//
// The step the judgment is about, its description, the commands the
// policy accepts now, the chances left for it and the turns the agent
// spent without acting.
func (s *Service) renderRepair(r *run) string {
	stepID := r.budgetStep()
	if stepID == "" {
		return ""
	}
	step, ok := r.stepByID(stepID)
	if !ok {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nrepair:\n")
	fmt.Fprintf(&b, "  step: %s\n", step.ID)
	if description := strings.TrimSpace(step.Description); description != "" {
		fmt.Fprintf(&b, "  description: %s\n", description)
	}
	if left, limit := r.stepUsage(step.ID); limit > 0 {
		fmt.Fprintf(&b, "  chances left: %d of %d\n", limit-left, limit)
	}
	fmt.Fprintf(&b, "  turns without an action: %d\n", r.consecutiveStalls())
	if cmds := r.stepCommands(); len(cmds) > 0 {
		fmt.Fprintf(&b, "  step commands now: %s\n", strings.Join(cmds, ", "))
	}
	return b.String()
}

// renderDirectives renders the directives the model may take now.
//
// The list is the whole catalogue of the run: a report that named
// only some of the tools would read as the set of permitted moves.
func (s *Service) renderDirectives(r *run) string {
	var b strings.Builder
	b.WriteString("\ndirectives now:\n")
	for _, tool := range s.toolCatalogue(r) {
		fmt.Fprintf(&b, "  %s\n", tool.Name)
	}
	return b.String()
}

// renderRoundSteps renders the steps of the round the run is on: where
// each of them stands, what each of them produced and which one is
// ready.
//
// It also returns the state of every step it rendered, so the caller
// can tell whether the step list already carries a verdict.
func (s *Service) renderRoundSteps(r *run) (string, map[string]wfe.StepState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	wf := r.workflow
	if wf == nil {
		return "", nil
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
	rendered := map[string]wfe.StepState{}
	var b strings.Builder
	for _, name := range wf.JobOrder() {
		if job != "" && name != job {
			continue
		}
		for _, step := range wf.Jobs[name].Steps {
			st := states[step.ID]
			rendered[step.ID] = st
			fmt.Fprintf(&b, "  %s: %s", step.ID, stepStateText(st))
			if idx := r.stepIndex(step.ID); idx >= 0 && idx < len(remaining.Steps) && remaining.Steps[idx] >= 0 {
				fmt.Fprintf(&b, " (%d chances left)", remaining.Steps[idx])
			}
			if ready[step.ID] {
				b.WriteString(" — ready")
			}
			b.WriteString("\n")
			writeOutcomeFacts(&b, st)
		}
	}
	return b.String(), rendered
}

// writeOutcomeFacts renders what a step produced: its words, the
// facts it reported and the status that says which branch it took.
//
// The facts are the evidence the deciding agent needs: why a step
// ended the way it did is rarely in the status alone.
func writeOutcomeFacts(b *strings.Builder, st wfe.StepState) {
	if st.ID == "" || st.Pending {
		return
	}
	out := st.Outcome
	if out.StatusText != "" {
		fmt.Fprintf(b, "    %s\n", out.StatusText)
	}
	for _, k := range sortedNames(out.Data) {
		fmt.Fprintf(b, "    %s: %s\n", k, out.Data[k])
	}
}

// carriesOutcome reports whether the rendered steps already carry the
// action's verdict.
//
// A step's own run leaves the verdict on the step: the step list
// carries it. A repair the run issued for a step does not: its verdict
// is nowhere else.
func carriesOutcome(rendered map[string]wfe.StepState, action wfe.ActionRecord) bool {
	st, ok := rendered[action.StepID]
	if !ok {
		return false
	}
	return st.Outcome.Status == action.Outcome.Status &&
		st.Outcome.StatusText == action.Outcome.StatusText &&
		maps.Equal(st.Outcome.Data, action.Outcome.Data)
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
		return strconv.Itoa(st.Outcome.Status)
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
		return withDetail(fmt.Sprintf("step %s ended with status %s", m[1], m[2]), m[3])
	case lastStepReason.MatchString(reason):
		m := lastStepReason.FindStringSubmatch(reason)
		return withDetail(fmt.Sprintf("the plan is over and step %s ended with status %s", m[1], m[2]), m[3])
	case readyStepsReason.MatchString(reason):
		m := readyStepsReason.FindStringSubmatch(reason)
		return fmt.Sprintf("%s steps are ready and the plan cannot pick one", m[1])
	case stillRunningReason.MatchString(reason):
		m := stillRunningReason.FindStringSubmatch(reason)
		return fmt.Sprintf("step %s is still running", m[1])
	case budgetReason.MatchString(reason):
		m := budgetReason.FindStringSubmatch(reason)
		return fmt.Sprintf("step %s used up its budget", m[1])
	case jobsReason.MatchString(reason):
		m := jobsReason.FindStringSubmatch(reason)
		return fmt.Sprintf("%s jobs are ready to start another round", m[1])
	case strings.HasPrefix(reason, "the plan is exhausted"):
		return "every step of the plan has run"
	}
	return reason
}

// The reasons the engine reports, and the words they are said in.
//
// The third group of the step reasons is the error description the
// engine rides along: it says what broke, and it is empty when the
// reason carries none.
var (
	prevStepReason     = regexp.MustCompile(`^the previous step "([^"]+)" ended with status (\d+)(?:: (.*))?$`)
	lastStepReason     = regexp.MustCompile(`^the plan is exhausted and the last step "([^"]+)" ended with status (\d+)(?:: (.*))?$`)
	readyStepsReason   = regexp.MustCompile(`^(\d+) steps are ready`)
	stillRunningReason = regexp.MustCompile(`^the step "([^"]+)" is still running$`)
	budgetReason       = regexp.MustCompile(`^the step "([^"]+)" already ran \d+ times, its limit is \d+$`)
	jobsReason         = regexp.MustCompile(`^(\d+) jobs are ready to start another round$`)
)

// withDetail appends the error description a reason carries.
func withDetail(sentence, detail string) string {
	if detail == "" {
		return sentence
	}
	return sentence + ": " + detail
}
