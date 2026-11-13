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
// order. A step's `max` caps the actions taken against it — its own
// message and the commands in its allow list.
type Policy struct {
	// Workflow is the bound workflow the policy enforces.
	Workflow *Workflow
	// History holds the actions taken so far, oldest first.
	History []ActionRecord
}

// NewPolicy returns a policy over wf with the given action history.
func NewPolicy(wf *Workflow, history []ActionRecord) *Policy {
	p := &Policy{Workflow: wf}
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
	step := p.steps()[idx]
	if step.Max > 0 {
		if used := p.stepUsage(idx); used >= step.Max {
			return -1, fmt.Errorf("step %d (%s) already ran %d times, its limit is %d", idx+1, step.Message, used, step.Max)
		}
	}
	if used, limit := p.usedActions(), p.maxActions(); used >= limit {
		return -1, fmt.Errorf("the workflow allows %d actions in total, all %d are used", limit, used)
	}
	return idx, nil
}

// CheckStep reports whether the step may take an action now: its own
// limit and the workflow's action budget.
//
// The workflow's own plan runs through this check too: a step that
// spent its budget is a judgment point, not a silent overrun.
func (p *Policy) CheckStep(idx int) error {
	steps := p.steps()
	if idx < 0 || idx >= len(steps) {
		return fmt.Errorf("there is no step %d in this workflow", idx+1)
	}
	step := steps[idx]
	if step.Max > 0 {
		if used := p.stepUsage(idx); used >= step.Max {
			return fmt.Errorf("step %d (%s) already ran %d times, its limit is %d", idx+1, step.Message, used, step.Max)
		}
	}
	if used, limit := p.usedActions(), p.maxActions(); used >= limit {
		return fmt.Errorf("the workflow allows %d actions in total, all %d are used", limit, used)
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
// to the work agent. An instruction is an action: it counts against
// the workflow's action budget.
func (p *Policy) CheckInstruct() error {
	if p.Workflow == nil || !p.Workflow.Instruct {
		return fmt.Errorf("this workflow does not allow free-form instructions")
	}
	if used, limit := p.usedActions(), p.maxActions(); used >= limit {
		return fmt.Errorf("the workflow allows %d actions in total, all %d are used", limit, used)
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
	// Actions is the number of actions left overall.
	Actions int
	// Steps holds the actions left per step, index-aligned with the
	// workflow's steps. -1 means the step has no limit of its own.
	Steps []int
}

// Remaining returns the budget left: actions left overall and actions
// left per step.
func (p *Policy) Remaining() Remaining {
	left := p.maxActions() - p.usedActions()
	if left < 0 {
		left = 0
	}
	rem := Remaining{Actions: left, Steps: make([]int, len(p.steps()))}
	for i, step := range p.steps() {
		if step.Max <= 0 {
			rem.Steps[i] = -1
			continue
		}
		n := step.Max - p.stepUsage(i)
		if n < 0 {
			n = 0
		}
		rem.Steps[i] = n
	}
	return rem
}

// Render returns the workflow as a compact text block for the agent's
// system prompt: the goal, the steps with their notes, the extra
// commands each step allows and the limits.
func (p *Policy) Render() string {
	var b strings.Builder
	if p.Workflow != nil {
		fmt.Fprintf(&b, "workflow: %s\n", p.Workflow.Name)
		if p.Workflow.Goal != "" {
			fmt.Fprintf(&b, "goal: %s\n", p.Workflow.Goal)
		}
	}
	fmt.Fprintf(&b, "limits: %d actions in total", p.maxActions())
	if p.Workflow != nil && !p.Workflow.Instruct {
		b.WriteString("; no free-form instructions")
	}
	b.WriteString("\n")
	b.WriteString("steps:\n")
	for i, step := range p.steps() {
		fmt.Fprintf(&b, "%d. %s\n", i+1, stepLabel(step))
		if step.Note != "" {
			fmt.Fprintf(&b, "   note: %s\n", step.Note)
		}
		if len(step.Allow) > 0 {
			fmt.Fprintf(&b, "   also allowed: %s\n", strings.Join(step.Allow, ", "))
		}
		if step.Max > 0 {
			fmt.Fprintf(&b, "   limit: %d actions\n", step.Max)
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
// once that step has spent its limit. The whole list is empty once
// the workflow's action budget is used up.
func (p *Policy) AllowedCommands() []string {
	if p.usedActions() >= p.maxActions() {
		return nil
	}
	var cmds []string
	seen := map[string]bool{}
	for i, step := range p.steps() {
		spent := step.Max > 0 && p.stepUsage(i) >= step.Max
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

// stepUsage counts the runs taken for the step.
func (p *Policy) stepUsage(idx int) int {
	steps := p.steps()
	if idx < 0 || idx >= len(steps) {
		return 0
	}
	id := steps[idx].ID
	n := 0
	for _, rec := range p.History {
		if rec.Kind == ActionKindRun && rec.StepID == id {
			n++
		}
	}
	return n
}

// lastExecuted returns the step that ran last and its latest
// outcome.
func (p *Policy) lastExecuted() (string, Outcome, bool) {
	for i := len(p.History) - 1; i >= 0; i-- {
		rec := p.History[i]
		if rec.Kind != ActionKindRun {
			continue
		}
		for j := i; j >= 0; j-- {
			if p.History[j].Kind == ActionKindRun && p.History[j].StepID == rec.StepID {
				return rec.StepID, p.History[j].Outcome, true
			}
		}
		return rec.StepID, rec.Outcome, true
	}
	return "", Outcome{}, false
}

// usedActions counts every action taken, instructions included.
func (p *Policy) usedActions() int {
	return len(p.History)
}

// maxActions returns the workflow's action budget.
func (p *Policy) maxActions() int {
	if p.Workflow != nil && p.Workflow.MaxActions > 0 {
		return p.Workflow.MaxActions
	}
	return DefaultMaxActions
}

// steps returns the workflow's steps, tolerating a missing workflow.
func (p *Policy) steps() []Step {
	if p == nil || p.Workflow == nil {
		return nil
	}
	return p.Workflow.Steps()
}

// normalizeCommand collapses the whitespace of a command so a command
// typed with extra spaces still matches the workflow.
func normalizeCommand(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
