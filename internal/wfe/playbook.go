package wfe

import (
	"fmt"
	"strings"
)

// Policy is the constraint policy over a bound playbook and the
// actions taken so far. Every check is pure: it reads the playbook
// and the history and answers with an error the agent can act on.
type Policy struct {
	// Playbook is the bound playbook the policy enforces.
	Playbook *Playbook
	// History holds the actions taken so far, oldest first.
	History []ActionRecord
}

// NewPolicy returns a policy over pb with the given action history.
func NewPolicy(pb *Playbook, history []ActionRecord) *Policy {
	p := &Policy{Playbook: pb}
	if len(history) > 0 {
		p.History = append([]ActionRecord(nil), history...)
	}
	return p
}

// Record appends an action to the history.
func (p *Policy) Record(rec ActionRecord) {
	p.History = append(p.History, rec)
}

// CheckRun reports whether the command may run now and which step it
// belongs to. The command matches a step when it equals that step's
// `run` or one of its `allow` entries, compared with whitespace
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
			return -1, fmt.Errorf("step %d (%s) already ran %d times, its limit is %d", idx+1, step.Run, used, step.Max)
		}
	}
	if used, limit := p.usedActions(), p.maxActions(); used >= limit {
		return -1, fmt.Errorf("the playbook allows %d actions in total, all %d are used", limit, used)
	}
	return idx, nil
}

// CheckInstruct reports whether the agent may send a free-form prompt
// to the work agent. An instruction is an action: it counts against
// the playbook's action budget.
func (p *Policy) CheckInstruct() error {
	if p.Playbook == nil || !p.Playbook.Instruct {
		return fmt.Errorf("this playbook does not allow free-form instructions")
	}
	if used, limit := p.usedActions(), p.maxActions(); used >= limit {
		return fmt.Errorf("the playbook allows %d actions in total, all %d are used", limit, used)
	}
	return nil
}

// CheckFinish reports whether the run may end with the given status.
// "done" needs the last step's latest outcome to be ok or noop;
// "failed" is always allowed.
func (p *Policy) CheckFinish(status string) error {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed":
		return nil
	case "done":
		steps := p.steps()
		if len(steps) == 0 {
			return fmt.Errorf("the playbook has no steps")
		}
		last := len(steps) - 1
		out, ok := p.latestOutcome(last)
		if !ok {
			return fmt.Errorf("the last step %q has not run yet", steps[last].Run)
		}
		if !out.succeeded() {
			return fmt.Errorf("the last step %q finished as %q: it must be ok or noop", steps[last].Run, out.Status)
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
	// Steps holds the actions left per step, index-aligned with
	// Playbook.Steps. -1 means the step has no limit of its own.
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

// Render returns the playbook as a compact text block for the agent's
// system prompt: the goal, the steps with their notes, the extra
// commands each step allows and the limits.
func (p *Policy) Render() string {
	var b strings.Builder
	if p.Playbook != nil {
		fmt.Fprintf(&b, "playbook: %s\n", p.Playbook.Name)
		if p.Playbook.Goal != "" {
			fmt.Fprintf(&b, "goal: %s\n", p.Playbook.Goal)
		}
	}
	fmt.Fprintf(&b, "limits: %d actions in total", p.maxActions())
	if p.Playbook != nil && !p.Playbook.Instruct {
		b.WriteString("; no free-form instructions")
	}
	b.WriteString("\n")
	b.WriteString("steps:\n")
	for i, step := range p.steps() {
		fmt.Fprintf(&b, "%d. %s\n", i+1, step.Run)
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

// stepFor returns the index of the first step that permits the
// command, or -1 when no step does.
func (p *Policy) stepFor(cmd string) int {
	for i, step := range p.steps() {
		if normalizeCommand(step.Run) == cmd {
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

// allowedCommands lists the commands the policy accepts now.
func (p *Policy) allowedCommands() string {
	var cmds []string
	seen := map[string]bool{}
	for i, step := range p.steps() {
		if step.Max > 0 && p.stepUsage(i) >= step.Max {
			continue
		}
		for _, c := range append([]string{step.Run}, step.Allow...) {
			n := normalizeCommand(c)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			cmds = append(cmds, n)
		}
	}
	if len(cmds) == 0 {
		return "none"
	}
	return strings.Join(cmds, ", ")
}

// stepUsage counts the actions taken for the step.
func (p *Policy) stepUsage(idx int) int {
	n := 0
	for _, rec := range p.History {
		if rec.StepIndex == idx {
			n++
		}
	}
	return n
}

// latestOutcome returns the outcome of the most recent action taken
// for the step.
func (p *Policy) latestOutcome(idx int) (Outcome, bool) {
	for i := len(p.History) - 1; i >= 0; i-- {
		if p.History[i].StepIndex == idx {
			return p.History[i].Outcome, true
		}
	}
	return Outcome{}, false
}

// usedActions counts every action taken, instructions included.
func (p *Policy) usedActions() int {
	return len(p.History)
}

// maxActions returns the playbook's action budget.
func (p *Policy) maxActions() int {
	if p.Playbook != nil && p.Playbook.MaxActions > 0 {
		return p.Playbook.MaxActions
	}
	return DefaultMaxActions
}

// steps returns the playbook's steps, tolerating a missing playbook.
func (p *Policy) steps() []PlaybookStep {
	if p == nil || p.Playbook == nil {
		return nil
	}
	return p.Playbook.Steps
}

// normalizeCommand collapses the whitespace of a command so a command
// typed with extra spaces still matches the playbook.
func normalizeCommand(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
