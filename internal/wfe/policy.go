package wfe

import (
	"fmt"
	"strings"
)

// Policy is the constraint policy over a bound workflow and the
// actions taken so far. Every check is pure: it reads the workflow
// and the history and answers with an error the agent can act on.
//
// The steps of the policy are every step of every job, in topological
// order. A step's `attempts` caps the actions taken against it in the
// current round — its own message and the commands in its allow list.
type Policy struct {
	// Workflow is the bound workflow the policy enforces.
	Workflow *Workflow
	// History holds the actions taken so far, oldest first.
	History []ActionRecord

	// stepList is the workflow's step list, computed once: the
	// topological order does not change while the run goes on.
	stepList []Step
	// rounds holds the round every job is in. The per-step budgets
	// count the attempts of the current round only.
	rounds map[string]int
}

// NewPolicy returns a policy over wf with the given action history.
func NewPolicy(wf *Workflow, history []ActionRecord) *Policy {
	p := &Policy{Workflow: wf, rounds: map[string]int{}}
	if len(history) > 0 {
		p.History = append([]ActionRecord(nil), history...)
	}
	return p
}

// Record appends an action to the history. Only a run belongs to a
// step, so any other action is recorded without one.
func (p *Policy) Record(rec ActionRecord) {
	if rec.Kind != ActionKindRun {
		rec.StepID = ""
	}
	p.History = append(p.History, rec)
}

// SetRounds records the round every job is in.
//
// The run state owns the rounds; the policy needs them to count the
// attempts of the current round.
func (p *Policy) SetRounds(rounds map[string]int) {
	p.rounds = make(map[string]int, len(rounds))
	for job, round := range rounds {
		p.rounds[job] = round
	}
}

// Round returns the round the job is in. A job that has not started
// is in its first round.
func (p *Policy) Round(job string) int {
	if r := p.rounds[job]; r > 0 {
		return r
	}
	return 1
}

// CheckRun reports whether the command may run now and which step it
// belongs to. The command matches a step when it equals that step's
// message or one of its `allow` entries, compared with whitespace
// normalised away. The step that owns the command is the first one
// listing it.
func (p *Policy) CheckRun(command string) (int, error) {
	cmd := normalizeCommand(command)
	if cmd == "" {
		return -1, fmt.Errorf("no command given")
	}
	idx := p.stepFor(cmd)
	if idx < 0 {
		return -1, fmt.Errorf("%q is not allowed: allowed commands are %s", command, p.allowedCommands())
	}
	if err := p.CheckStep(idx); err != nil {
		return -1, err
	}
	return idx, nil
}

// CheckStep reports whether the step may take an action now: its own
// limit for the current round.
//
// The workflow's own plan runs through this check too: a step that
// spent its budget is a judgment point, not a silent overrun.
func (p *Policy) CheckStep(idx int) error {
	steps := p.steps()
	if idx < 0 || idx >= len(steps) {
		return fmt.Errorf("there is no step %d in this workflow", idx+1)
	}
	step := steps[idx]
	if step.Attempts <= 0 {
		return nil
	}
	if used := p.stepUsage(idx); used >= step.Attempts {
		return fmt.Errorf("the step %q already ran %d times, its limit is %d",
			step.ID, used, step.Attempts)
	}
	return nil
}

// IsStepMessage reports whether the command is the step's own message.
func (p *Policy) IsStepMessage(idx int, command string) bool {
	steps := p.steps()
	if idx < 0 || idx >= len(steps) {
		return false
	}
	msg := steps[idx].Message
	return msg != "" && normalizeCommand(msg) == normalizeCommand(command)
}

// CheckInstruct reports whether the agent may send a free-form prompt
// to the work agent.
func (p *Policy) CheckInstruct() error {
	if p.Workflow == nil || !p.Workflow.Instruct {
		return fmt.Errorf("this workflow does not allow free-form instructions")
	}
	return nil
}

// CheckFinish reports whether the run may end with the given status.
// "done" needs the last executed step's latest outcome to be ok or
// noop; "failed" is always allowed.
func (p *Policy) CheckFinish(status string) error {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed":
		return nil
	case "done":
		stepID, out, ok := p.lastExecuted()
		if !ok {
			return fmt.Errorf("the last step has not run yet")
		}
		if !out.Succeeded() {
			return fmt.Errorf("the last step %q finished as %q: it must be ok or noop", stepID, out.Status)
		}
		return nil
	default:
		return fmt.Errorf("status must be \"done\" or \"failed\", got %q", status)
	}
}

// Remaining reports the budget left in the policy.
type Remaining struct {
	// Steps holds the actions left per step, index-aligned with the
	// workflow's steps. -1 means the step has no limit of its own.
	Steps []int
}

// Remaining returns the actions left per step.
func (p *Policy) Remaining() Remaining {
	rem := Remaining{Steps: make([]int, len(p.steps()))}
	for i, step := range p.steps() {
		if step.Attempts <= 0 {
			rem.Steps[i] = -1
			continue
		}
		n := step.Attempts - p.stepUsage(i)
		if n < 0 {
			n = 0
		}
		rem.Steps[i] = n
	}
	return rem
}

// Render returns the workflow as a compact text block for the agent's
// system prompt: the title, the steps with their notes, the extra
// commands each step allows and the limits.
func (p *Policy) Render() string {
	var b strings.Builder
	if p.Workflow != nil {
		fmt.Fprintf(&b, "workflow: %s\n", p.Workflow.Name)
		if p.Workflow.Title != "" {
			fmt.Fprintf(&b, "title: %s\n", p.Workflow.Title)
		}
	}
	if p.Workflow != nil && !p.Workflow.Instruct {
		b.WriteString("no free-form instructions\n")
	}
	b.WriteString("steps:\n")
	for i, step := range p.steps() {
		fmt.Fprintf(&b, "%d. %s\n", i+1, stepLabel(step))
		if step.Note != "" {
			fmt.Fprintf(&b, "   note: %s\n", step.Note)
		}
		if len(step.Allow) > 0 {
			fmt.Fprintf(&b, "   also allowed: %s\n", strings.Join(step.Allow, ", "))
		}
		if step.Attempts > 0 {
			fmt.Fprintf(&b, "   limit: %d attempts in this round\n", step.Attempts)
		}
	}
	return b.String()
}

// stepLabel renders a step as the agent reads it.
func stepLabel(step Step) string {
	switch step.Kind() {
	case StepKindMessage:
		return step.Message
	case StepKindShell:
		return "(shell) " + step.Shell
	case StepKindUse:
		return "(use) " + step.Use
	case StepKindWait:
		return "(wait) " + step.Wait
	}
	return "(no kind)"
}

// stepFor returns the index of the first step that permits the
// command, or -1 when no step does.
func (p *Policy) stepFor(cmd string) int {
	for i, step := range p.steps() {
		if step.Message != "" && normalizeCommand(step.Message) == cmd {
			return i
		}
		for _, a := range step.Allow {
			if normalizeCommand(a) == cmd {
				return i
			}
		}
	}
	return -1
}

// AllowedCommands lists the commands the policy accepts now.
//
// A command belongs to the first step listing it, so it is left out
// once that step has spent its limit.
func (p *Policy) AllowedCommands() []string {
	var cmds []string
	seen := map[string]bool{}
	for i, step := range p.steps() {
		spent := step.Attempts > 0 && p.stepUsage(i) >= step.Attempts
		for _, c := range append([]string{step.Message}, step.Allow...) {
			n := normalizeCommand(c)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			if spent {
				continue
			}
			cmds = append(cmds, n)
		}
	}
	return cmds
}

// allowedCommands renders the commands the policy accepts now.
func (p *Policy) allowedCommands() string {
	cmds := p.AllowedCommands()
	if len(cmds) == 0 {
		return "none"
	}
	return strings.Join(cmds, ", ")
}

// stepUsage counts the runs taken for the step in the current round.
func (p *Policy) stepUsage(idx int) int {
	steps := p.steps()
	if idx < 0 || idx >= len(steps) {
		return 0
	}
	id := steps[idx].ID
	round := p.Round(JobOf(p.Workflow, id))
	n := 0
	for _, rec := range p.History {
		if rec.Kind != ActionKindRun || rec.StepID != id {
			continue
		}
		if recordRound(rec) != round {
			continue
		}
		n++
	}
	return n
}

// recordRound returns the round an action was taken in.
func recordRound(rec ActionRecord) int {
	if rec.Round <= 0 {
		return 1
	}
	return rec.Round
}

// lastExecuted returns the step that ran last and its latest
// outcome.
//
// The history is in order, so the last run record carries the latest
// outcome of the last executed step.
func (p *Policy) lastExecuted() (string, Outcome, bool) {
	for i := len(p.History) - 1; i >= 0; i-- {
		rec := p.History[i]
		if rec.Kind != ActionKindRun {
			continue
		}
		return rec.StepID, rec.Outcome, true
	}
	return "", Outcome{}, false
}

// steps returns the workflow's steps, tolerating a missing workflow.
func (p *Policy) steps() []Step {
	if p == nil || p.Workflow == nil {
		return nil
	}
	if p.stepList == nil {
		p.stepList = p.Workflow.Steps()
	}
	return p.stepList
}

// normalizeCommand collapses the whitespace of a command so a command
// typed with extra spaces still matches the workflow.
func normalizeCommand(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
