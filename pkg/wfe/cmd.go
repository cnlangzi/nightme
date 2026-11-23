package wfe

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The builtin commands of the engine.
//
// A builtin is a command the engine runs itself: the workflow names
// it, and the engine knows what it does. The table is the whole list —
// a name the table does not hold is refused when the file is loaded.
type builtin struct {
	// name is the name the workflow file uses.
	name string
	// args are the arguments the command takes.
	args []string
}

// builtins are the commands the engine runs itself.
//
// The list is short on purpose: a command that does something to the
// run is a thing the engine has to know about, and every one of them
// is a rule the operator has to learn.
var builtins = []builtin{
	{name: "skip"},
}

// builtinNames renders the names of the builtin commands.
func builtinNames() string {
	names := make([]string, 0, len(builtins))
	for _, b := range builtins {
		names = append(names, b.name)
	}
	return strings.Join(names, ", ")
}

// builtinByName returns the builtin with the name.
func builtinByName(name string) (builtin, bool) {
	for _, b := range builtins {
		if b.name == name {
			return b, true
		}
	}
	return builtin{}, false
}

// validateBuiltin checks a cmd block against the table of the builtins.
//
// The check belongs to the load path: a workflow that names a command
// the engine does not have is a workflow that cannot run.
func validateBuiltin(cmd *CmdSpec) error {
	b, ok := builtinByName(cmd.Name)
	if !ok {
		return fmt.Errorf("unknown builtin command %q (known: %s)", cmd.Name, builtinNames())
	}
	for _, arg := range cmd.Args {
		if slices.Contains(b.args, arg) {
			continue
		}
		if len(b.args) == 0 {
			return fmt.Errorf("the builtin %q takes no arguments", b.name)
		}
		return fmt.Errorf("the builtin %q does not take the argument %q (it takes: %s)",
			b.name, arg, strings.Join(b.args, ", "))
	}
	return nil
}

// RunBuiltin runs the builtin command a step names.
//
// The engine owns the round: a command that ends a round ends it here,
// and the round leaves nothing behind in the run's report.
//
// A step that names no command has nothing to run: its message is its
// outcome, and the run goes on.
func RunBuiltin(state *RunState, wf *Workflow, job string, step Step) error {
	if state == nil {
		return errors.New("wfe: nil state")
	}
	if wf == nil {
		return errors.New("wfe: nil workflow")
	}
	if step.Cmd == nil {
		return nil
	}
	switch name := strings.TrimSpace(step.Cmd.Name); name {
	case "":
		return nil
	case "skip":
		return skipRound(state, wf, job, step)
	default:
		return fmt.Errorf("wfe: unknown builtin command %q", name)
	}
}

// skipRound ends the round of a job quietly.
//
// The round ends like every round the engine ends: the steps that did
// not run are recorded as skipped. On top of that the round is marked
// as skipped, so the run's report leaves it out — an empty round of a
// polling job would otherwise fill the panel with rows nobody reads.
func skipRound(state *RunState, wf *Workflow, job string, step Step) error {
	round := state.RoundOf(job)
	if err := endRound(state, wf, job, step.ID); err != nil {
		return err
	}
	for _, skipped := range state.SkippedRounds {
		if skipped.Job == job && skipped.Round == round {
			return nil
		}
	}
	state.SkippedRounds = append(state.SkippedRounds, SkippedRound{Job: job, Round: round})
	return nil
}
