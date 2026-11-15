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
// order. The budget caps the actions taken against one step in the
// current round — its own execution, the commands issued for it and
// the turns the agent produced no tool call in.
type Policy struct {
	// Workflow is the bound workflow the policy enforces.
	Workflow *Workflow
	// History holds the actions taken so far, oldest first.
	History []ActionRecord
	// Commands holds the names of the nightme commands the run may
	// issue outside the workflow's steps. The names carry no leading
	// slash.
	//
	// A command of the list belongs to no step: it counts against the
	// step the run is on, but it never becomes that step's verdict.
	Commands []string
	// Budget is the number of chances the agent has at one step in
	// one round.
	//
	// One number bounds every step: the step's own execution, the
	// nightme commands issued for it and the turns the agent produced
	// no tool call in each cost one chance. Zero means no limit.
	Budget int

	// stepList is the workflow's step list, computed once: the
	// topological order does not change while the run goes on.
	stepList []Step
	// rounds holds the round every job is in. The per-step budgets
	// count the actions of the current round only.
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

// Record appends an action to the history. Only the actions that
// count against a step carry one, so any other action is recorded
// without one.
func (p *Policy) Record(rec ActionRecord) {
	if !belongsToStep(rec.Kind) {
		rec.StepID = ""
	}
	p.History = append(p.History, rec)
}

// belongsToStep reports whether an action of the kind counts against
// a step.
func belongsToStep(kind ActionKind) bool {
	switch kind {
	case ActionKindRun, ActionKindCommand, ActionKindStall, ActionKindReset:
		return true
	}
	return false
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

// CheckRun reports whether the command may run now and which step the
// action counts against.
//
// The command matches a step when it equals that step's message,
// compared with whitespace normalised away. The step that owns the
// command is the first one with that message.
//
// A command of the registry that belongs to no step is accepted too:
// it counts against the step the run is on, but it never becomes that
// step's verdict.
func (p *Policy) CheckRun(command string) (int, error) {
	cmd := normalizeCommand(command)
	if cmd == "" {
		return -1, fmt.Errorf("no command given")
	}
	idx := p.stepFor(cmd)
	if idx >= 0 {
		if err := p.CheckStep(idx); err != nil {
			return -1, err
		}
		return idx, nil
	}
	if p.isRegistered(cmd) {
		cur := p.currentStep()
		if cur >= 0 {
			if err := p.CheckStep(cur); err != nil {
				return -1, err
			}
		}
		return cur, nil
	}
	return -1, fmt.Errorf("%q is not allowed: the run may run the steps %s", command, p.stepCommandList())
}

// isRegistered reports whether the command is one of the nightme
// commands the run may issue.
func (p *Policy) isRegistered(cmd string) bool {
	name := commandName(cmd)
	if name == "" {
		return false
	}
	for _, c := range p.Commands {
		if c == name {
			return true
		}
	}
	return false
}

// currentStep returns the index of the step the run is on: the step
// of the last run the history holds. -1 when nothing ran yet.
func (p *Policy) currentStep() int {
	for i := len(p.History) - 1; i >= 0; i-- {
		rec := p.History[i]
		if rec.Kind != ActionKindRun || rec.StepID == "" {
			continue
		}
		for j, step := range p.steps() {
			if step.ID == rec.StepID {
				return j
			}
		}
	}
	return -1
}

// commandName returns the name of a nightme command: the first token
// of the command line when it starts with a slash. Empty otherwise.
func commandName(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	first := fields[0]
	if !strings.HasPrefix(first, "/") {
		return ""
	}
	return strings.TrimPrefix(first, "/")
}

// CheckStep reports whether the step may take an action now: its
// budget for the current round.
//
// The workflow's own plan runs through this check too: a step that
// spent its budget is a judgment point, not a silent overrun.
func (p *Policy) CheckStep(idx int) error {
	steps := p.steps()
	if idx < 0 || idx >= len(steps) {
		return fmt.Errorf("there is no step %d in this workflow", idx+1)
	}
	if p.Budget <= 0 {
		return nil
	}
	if used := p.StepUsage(idx); used >= p.Budget {
		return fmt.Errorf("the step %q already ran %d times, its limit is %d",
			steps[idx].ID, used, p.Budget)
	}
	return nil
}

// IsStepMessage reports whether the command is the step's own message.
//
// It is the single ownership test: a step's message belongs to that
// step, and a command that is no step's message belongs to no step.
func (p *Policy) IsStepMessage(idx int, command string) bool {
	steps := p.steps()
	if idx < 0 || idx >= len(steps) {
		return false
	}
	msg := steps[idx].Message
	return msg != "" && normalizeCommand(msg) == normalizeCommand(command)
}

// CheckInstruct reports whether the agent may send a free-form prompt
// to the coding agent.
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
	// Steps holds the chances left per step, index-aligned with the
	// workflow's steps. -1 means the policy has no budget at all.
	Steps []int
}

// Remaining returns the chances left per step.
func (p *Policy) Remaining() Remaining {
	rem := Remaining{Steps: make([]int, len(p.steps()))}
	for i := range p.steps() {
		if p.Budget <= 0 {
			rem.Steps[i] = -1
			continue
		}
		n := p.Budget - p.StepUsage(i)
		if n < 0 {
			n = 0
		}
		rem.Steps[i] = n
	}
	return rem
}

// Render returns the workflow as a compact text block for the agent's
// system prompt: the title, the steps with their notes and the budget.
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
	if p.Budget > 0 {
		fmt.Fprintf(&b, "budget: %d chances per step in this round\n", p.Budget)
	}
	b.WriteString("steps:\n")
	for i, step := range p.steps() {
		fmt.Fprintf(&b, "%d. %s\n", i+1, stepLabel(step))
		if step.Note != "" {
			fmt.Fprintf(&b, "   note: %s\n", step.Note)
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

// stepFor returns the index of the step whose message is the command,
// or -1 when no step has it.
func (p *Policy) stepFor(cmd string) int {
	for i, step := range p.steps() {
		if step.Message != "" && normalizeCommand(step.Message) == cmd {
			return i
		}
	}
	return -1
}

// StepCommands lists the step messages the policy accepts now.
//
// A command belongs to the first step that has it, so it is left out
// once that step has spent its budget.
func (p *Policy) StepCommands() []string {
	var cmds []string
	seen := map[string]bool{}
	for i, step := range p.steps() {
		spent := p.Budget > 0 && p.StepUsage(i) >= p.Budget
		n := normalizeCommand(step.Message)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		if spent {
			continue
		}
		cmds = append(cmds, n)
	}
	return cmds
}

// stepCommandList renders the step messages the policy accepts now.
func (p *Policy) stepCommandList() string {
	cmds := p.StepCommands()
	if len(cmds) == 0 {
		return "none"
	}
	return strings.Join(cmds, ", ")
}

// StepUsage counts the chances the step spent in the current round:
// its own runs, the commands issued for it and the turns the agent
// produced no tool call in.
//
// A reset starts the step's budget over: only the actions after the
// last one count.
func (p *Policy) StepUsage(idx int) int {
	steps := p.steps()
	if idx < 0 || idx >= len(steps) {
		return 0
	}
	id := steps[idx].ID
	round := p.Round(JobOf(p.Workflow, id))
	after := -1
	n := 0
	for i, rec := range p.History {
		if rec.StepID != id {
			continue
		}
		if recordRound(rec) != round {
			continue
		}
		switch rec.Kind {
		case ActionKindReset:
			after = i
			n = 0
		case ActionKindRun, ActionKindCommand, ActionKindStall:
			if i > after {
				n++
			}
		}
	}
	return n
}

// ConsecutiveStalls returns the number of turns the agent produced no
// tool call in, counted from the end of the history.
//
// A turn that did something ends the count: the agent acted.
func (p *Policy) ConsecutiveStalls() int {
	n := 0
	for i := len(p.History) - 1; i >= 0; i-- {
		if p.History[i].Kind != ActionKindStall {
			break
		}
		n++
	}
	return n
}

// CurrentStepID returns the id of the step the run is on: the step of
// the last run the history holds. Empty when nothing ran yet.
func (p *Policy) CurrentStepID() string {
	idx := p.currentStep()
	if idx < 0 {
		return ""
	}
	steps := p.steps()
	if idx >= len(steps) {
		return ""
	}
	return steps[idx].ID
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
