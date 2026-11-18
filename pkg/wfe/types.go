// Package wfe — workflow engine. Pure library: parses YAML,
// validates the schema, computes the plan of a run and evaluates
// ${{ }} expressions. Zero I/O, no clock, no secrets. Every
// external capability is injected through the Runtime interface (see
// runtime.go).
//
// The package is published and importable from outside this
// repository:
//
//	import "github.com/cnlangzi/nightme/pkg/wfe"
//
//	wf, err := wfe.LoadFile("workflows", "workflows/nightme/coder.yml")
//	// loads and validates; the workflow's name is "nightme/coder"
package wfe

import (
	"encoding/json"
	"time"
)

// Workflow is the parsed+validated workflow definition.
//
// A workflow is the only automation artifact: it carries the jobs,
// the steps and the arguments they take. It names no agent — the run
// uses the agent of the chat that started it.
type Workflow struct {
	// Title states what the workflow is for. It is what the operator
	// reads and what the agent is given.
	Title string `yaml:"title" json:"title"`
	// Name is the workflow's identity: the path of the file it was
	// loaded from under the workflows directory, without the extension.
	// "nightme/coder.yml" is the workflow "nightme/coder". The loader
	// sets it; the file does not carry it.
	Name       string         `yaml:"-" json:"name"`
	Args       []Arg          `yaml:"args" json:"args,omitempty"`
	Instruct   bool           `yaml:"instruct" json:"instruct"`
	Workspaces []string       `yaml:"workspaces" json:"workspaces,omitempty"`
	On         Trigger        `yaml:"on" json:"on"`
	Jobs       map[string]Job `yaml:"jobs" json:"jobs"`
}

// UnmarshalJSON reads a workflow, migrating the retired `goal` key
// into the title.
//
// A run that was persisted before the title replaced the goal keeps
// its purpose when the daemon resumes it: the file on disk still
// carries the old key. New writes only carry the title.
func (w *Workflow) UnmarshalJSON(data []byte) error {
	type alias Workflow
	aux := struct {
		*alias
		LegacyGoal string `json:"goal,omitempty"`
	}{
		alias: (*alias)(w),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if w.Title == "" && aux.LegacyGoal != "" {
		w.Title = aux.LegacyGoal
	}
	return nil
}

// Arg is one argument a workflow takes.
//
// A default makes the argument optional: the run starts without a
// value for it. No default makes it required.
type Arg struct {
	Name    string  `yaml:"name" json:"name"`
	Default *string `yaml:"default,omitempty" json:"default,omitempty"`
}

// Job is a named sequence of steps.
//
// Needs lists the jobs that have to finish before this one starts. If
// holds only when the condition is true; an empty condition always
// holds.
type Job struct {
	Needs []string `yaml:"needs" json:"needs,omitempty"`
	If    string   `yaml:"if" json:"if,omitempty"`
	// Iterations is how many rounds the job runs.
	//
	// A missing value means one round: the job's steps run once. Zero
	// means the job's steps run until the run ends. N means the job's
	// steps run N rounds in total.
	Iterations *int   `yaml:"iterations,omitempty" json:"iterations,omitempty"`
	Steps      []Step `yaml:"steps" json:"steps"`
}

// Rounds returns how many rounds the job runs.
//
// Zero means the job repeats until the run ends.
func (j Job) Rounds() int {
	if j.Iterations == nil {
		return 1
	}
	return *j.Iterations
}

// OverrideIterations sets the round count of every job of the
// workflow.
//
// It changes the workflow in place: the caller must own the copy it
// passes. A count of zero means the job repeats until the run ends —
// the same meaning `iterations: 0` has in the file. A count is a
// round count, so it must not be negative: the loader refuses a
// workflow that carries one.
func OverrideIterations(wf *Workflow, rounds int) {
	if wf == nil {
		return
	}
	for name, job := range wf.Jobs {
		count := rounds
		job.Iterations = &count
		wf.Jobs[name] = job
	}
}

// The ways a step reaches its agents.
const (
	// AgentModeUse switches the chat's selected agent for the attempt.
	AgentModeUse = "use"
	// AgentModeArgs passes the agent on the command line.
	AgentModeArgs = "args"
)

// AgentChain is one way a step reaches its agents: the mode says how
// the agent is applied, the names are the agents to try, in order.
type AgentChain struct {
	Mode  string   `yaml:"mode" json:"mode"`
	Names []string `yaml:"names" json:"names"`
}

// Step is one unit of work inside a job: exactly one of message,
// shell, use or wait is set.
//
// Note explains why the step is there and which fixes the owner uses
// for it.
type Step struct {
	ID              string            `yaml:"id,omitempty" json:"id,omitempty"`
	If              string            `yaml:"if,omitempty" json:"if,omitempty"`
	Env             map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	ContinueOnError bool              `yaml:"continue-on-error,omitempty" json:"continue-on-error,omitempty"`
	Note            string            `yaml:"note,omitempty" json:"note,omitempty"`
	// Final marks the step that cleans up after the round. It runs
	// when the round ends, however the round ends.
	Final bool `yaml:"final,omitempty" json:"final,omitempty"`

	// Message is the text sent to the chat.
	Message string `yaml:"message,omitempty" json:"message,omitempty"`
	// Shell is the shell command run in the chat's directory.
	Shell string `yaml:"shell,omitempty" json:"shell,omitempty"`
	// Use is the name of a registered action.
	Use  string         `yaml:"use,omitempty" json:"use,omitempty"`
	With map[string]any `yaml:"with,omitempty" json:"with,omitempty"`
	// Wait is the duration the step waits before it reports ok.
	Wait string `yaml:"wait,omitempty" json:"wait,omitempty"`

	// Agents are the agents the step runs, in order. The first agent
	// that does not break is the step's.
	Agents []AgentChain `yaml:"agents,omitempty" json:"agents,omitempty"`
}

// StepKind is the dispatch category of a Step.
type StepKind int

const (
	// StepKindNone means the step names no kind at all.
	StepKindNone StepKind = iota
	// StepKindMessage sends text to the chat.
	StepKindMessage
	// StepKindShell runs a shell command in the chat's directory.
	StepKindShell
	// StepKindUse runs a registered action.
	StepKindUse
	// StepKindWait waits for a duration.
	StepKindWait
)

// Kind returns the kind of the step.
func (s Step) Kind() StepKind {
	switch {
	case s.Message != "":
		return StepKindMessage
	case s.Shell != "":
		return StepKindShell
	case s.Use != "":
		return StepKindUse
	case s.Wait != "":
		return StepKindWait
	}
	return StepKindNone
}

// Trigger is the parsed `on:` block.
//
// The block is parsed and displayed; nothing fires it yet.
type Trigger struct {
	Schedule    []ScheduleEntry `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	PullRequest *PRTrigger      `yaml:"pull_request,omitempty" json:"pull_request,omitempty"`
	Branch      *BranchTrigger  `yaml:"branch,omitempty" json:"branch,omitempty"`
	Issue       *EventFilter    `yaml:"issue,omitempty" json:"issue,omitempty"`
	Mention     *MentionTrigger `yaml:"mention,omitempty" json:"mention,omitempty"`
}

// ScheduleEntry is one cron entry.
type ScheduleEntry struct {
	Cron string `yaml:"cron" json:"cron"`
}

// PRTrigger fires on PR events; optional branch / event filters.
type PRTrigger struct {
	Branches []string `yaml:"branches,omitempty" json:"branches,omitempty"`
	Events   []string `yaml:"events,omitempty" json:"events,omitempty"`
}

// BranchTrigger fires on branch push events; optional pattern / event filters.
type BranchTrigger struct {
	Patterns []string `yaml:"patterns,omitempty" json:"patterns,omitempty"`
	Events   []string `yaml:"events,omitempty" json:"events,omitempty"`
}

// EventFilter is a generic event filter (used for issue).
type EventFilter struct {
	Events []string `yaml:"events,omitempty" json:"events,omitempty"`
}

// MentionTrigger fires when @owner is mentioned in a PR/issue comment
// (sourced via gitProvider). Optional command whitelist; empty means
// any mention fires.
type MentionTrigger struct {
	Commands []string `yaml:"commands,omitempty" json:"commands,omitempty"`
}

// Event is what a trigger pipeline produces. Type is one of
// "schedule" / "pull_request" / "branch" / "issue" / "mention".
// Data carries the trigger-specific payload.
type Event struct {
	Kind string
	Time time.Time
	Data map[string]any
}

// RunState is the progress of a run: which steps ran, what they
// produced and where the run stands.
//
// The engine owns it. The agent reads it and proposes a directive;
// only the engine writes to it.
type RunState struct {
	// RunID is the run's id.
	RunID string `json:"run_id"`
	// WorkflowName is the workflow the run works through.
	WorkflowName string `json:"workflow"`
	// Workspace is the directory the run works in.
	Workspace string `json:"workspace"`
	// Status is where the run stands.
	Status Status `json:"status"`
	// Env holds the run's environment.
	Env map[string]string `json:"env,omitempty"`
	// Rounds holds the round every job is in. A job that has not
	// started yet is not in the map.
	Rounds map[string]int `json:"rounds,omitempty"`
	// EndedRounds holds the early end of a round, keyed by the job.
	// The plan goes on with the final step of such a job or with its
	// next round — not with a judgment point.
	EndedRounds map[string]EndedRound `json:"ended_rounds,omitempty"`
	// Steps holds every step that ran, in the order it ran.
	Steps []StepState `json:"steps"`
	// StartedAt is when the run started.
	StartedAt time.Time `json:"started_at"`
	// UpdatedAt is when the run last moved.
	UpdatedAt time.Time `json:"updated_at"`
}

// EndedRound is the early end of one round of a job.
type EndedRound struct {
	// Round is the round that was ended.
	Round int `json:"round"`
	// Step is the step whose failure ended it.
	Step string `json:"step"`
}

// StepState is the record of one step that ran.
type StepState struct {
	// ID is the step's id.
	ID string `json:"id"`
	// Job is the job the step belongs to.
	Job string `json:"job"`
	// Round is the round of the job the step ran in.
	Round int `json:"round,omitempty"`
	// Outcome is the step's verdict.
	Outcome Outcome `json:"outcome"`
	// Skipped is true when the step's `if:` did not hold.
	Skipped bool `json:"skipped,omitempty"`
	// Pending is true when the step started and has not settled yet.
	// The step is still the ready one, but the plan cannot go on
	// without the agent: the command is waiting for an answer.
	Pending bool `json:"pending,omitempty"`
}

// RoundOf returns the round a job is in. A job that has not started
// is in its first round.
func (s *RunState) RoundOf(job string) int {
	if s == nil {
		return 1
	}
	if r := s.Rounds[job]; r > 0 {
		return r
	}
	return 1
}

// Status is the lifecycle state of a run.
type Status string

const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// OutcomeStatus is the machine-readable verdict of one action.
type OutcomeStatus string

const (
	OutcomeOK         OutcomeStatus = "ok"
	OutcomeNoop       OutcomeStatus = "noop"
	OutcomeIncomplete OutcomeStatus = "incomplete"
	OutcomeFailed     OutcomeStatus = "failed"
	OutcomeCancelled  OutcomeStatus = "cancelled"
	OutcomeTimeout    OutcomeStatus = "timeout"
	OutcomeUnknown    OutcomeStatus = "unknown"
)

// Outcome is the structured result of one action. It mirrors
// messages.Outcome field for field; wfe keeps its own copy so the
// engine stays independent of the message layer.
type Outcome struct {
	Status  OutcomeStatus     `json:"status"`
	Summary string            `json:"summary"`
	Data    map[string]string `json:"data,omitempty"`
}

// Succeeded reports whether the outcome means the work was done.
func (o Outcome) Succeeded() bool {
	return o.Status == OutcomeOK || o.Status == OutcomeNoop
}

// OutcomeKeyError is the Data key the error description rides in.
//
// It is the key the outcome contract names (see
// messages.OutcomeKeyError): the engine reads it to tell a normal
// result from a breakage. The engine keeps its own copy of the
// outcome type, so it keeps its own copy of the key too.
const OutcomeKeyError = "error"

// Exception reports whether the outcome is an exception: the flow
// cannot go on by itself and the agent has to decide what happens
// next.
//
// The error description says what broke; the status says whether the
// work got done. A failed outcome without an error description is a
// normal branch of the work: the step did its job and its answer was
// "there was nothing to do".
func (o Outcome) Exception() bool {
	if o.Data[OutcomeKeyError] != "" {
		return true
	}
	switch o.Status {
	case OutcomeOK, OutcomeNoop, OutcomeFailed:
		return false
	default:
		return true // cancelled, timeout, incomplete, unknown
	}
}

// ActionKind is the kind of action recorded in the history.
type ActionKind string

const (
	// ActionKindRun is the step's own execution — the step running.
	ActionKindRun ActionKind = "run"
	// ActionKindInstruct is a free-form prompt to the coding agent.
	//
	// It counts against the step the run is on: it is a repair, and a
	// repair is work the agent causes.
	ActionKindInstruct ActionKind = "instruct"
	// ActionKindCommand is a nightme command the run issued as a
	// repair.
	//
	// It counts against the step it was issued for, but it is not a
	// step's own execution, so it never becomes that step's verdict.
	ActionKindCommand ActionKind = "command"
	// ActionKindStall is a turn of the chat model that produced no
	// tool call.
	//
	// It counts against the step the judgment was about, and it never
	// becomes a step's verdict.
	ActionKindStall ActionKind = "stall"
	// ActionKindReset starts a step's budget over.
	//
	// The operator took the chat back and handed the control to the
	// run: the step gets a fresh budget.
	ActionKindReset ActionKind = "reset"
)

// ActionRecord is one action the run has taken. StepID points at the
// step the action served: a run belongs to its step, and a repair — a
// command or an instruction — counts against the step the run is on.
// A stall counts against the step the judgment was about, and a reset
// names the step whose budget starts over.
//
// Round is the round of the step's job the action was taken in. The
// per-step budgets count the actions of the current round only.
type ActionRecord struct {
	Kind    ActionKind `json:"kind"`
	Command string     `json:"command"`
	StepID  string     `json:"step_id,omitempty"`
	Round   int        `json:"round,omitempty"`
	Outcome Outcome    `json:"outcome"`
	At      time.Time  `json:"at"`
}
