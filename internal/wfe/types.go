// Package wfe — workflow engine. Pure library: parses YAML,
// validates the schema, computes the plan of a run and evaluates
// ${{ }} expressions. Zero I/O, no clock, no secrets. Every
// external capability is injected through the Runtime interface (see
// runtime.go).
package wfe

import "time"

// Workflow is the parsed+validated workflow definition.
//
// A workflow is the only automation artifact: it carries the jobs,
// the steps and the arguments they take. It names no agent — the run
// uses the agent of the chat that started it.
type Workflow struct {
	Name       string         `yaml:"name" json:"name"`
	Args       []Arg          `yaml:"args" json:"args,omitempty"`
	Goal       string         `yaml:"goal" json:"goal,omitempty"`
	MaxActions int            `yaml:"max_actions" json:"max_actions"`
	Instruct   bool           `yaml:"instruct" json:"instruct"`
	Workspaces []string       `yaml:"workspaces" json:"workspaces,omitempty"`
	On         Trigger        `yaml:"on" json:"on"`
	Jobs       map[string]Job `yaml:"jobs" json:"jobs"`
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
	Steps []Step   `yaml:"steps" json:"steps"`
}

// Step is one unit of work inside a job: exactly one of message,
// shell or use is set.
//
// Max caps the number of actions the step may take — its own message
// and the commands in its allow list. Allow lists the extra commands
// the agent may use inside the step. Note explains when to use them.
type Step struct {
	ID              string            `yaml:"id,omitempty" json:"id,omitempty"`
	If              string            `yaml:"if,omitempty" json:"if,omitempty"`
	Env             map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	ContinueOnError bool              `yaml:"continue-on-error,omitempty" json:"continue-on-error,omitempty"`
	Note            string            `yaml:"note,omitempty" json:"note,omitempty"`
	Allow           []string          `yaml:"allow,omitempty" json:"allow,omitempty"`
	Max             int               `yaml:"max,omitempty" json:"max,omitempty"`

	// Message is the text sent to the work chat.
	Message string `yaml:"message,omitempty" json:"message,omitempty"`
	// Shell is the shell command run in the workspace.
	Shell string `yaml:"shell,omitempty" json:"shell,omitempty"`
	// Use is the name of a registered action.
	Use  string         `yaml:"use,omitempty" json:"use,omitempty"`
	With map[string]any `yaml:"with,omitempty" json:"with,omitempty"`
}

// StepKind is the dispatch category of a Step.
type StepKind int

const (
	// StepKindNone means the step names no kind at all.
	StepKindNone StepKind = iota
	// StepKindMessage sends text to the work chat.
	StepKindMessage
	// StepKindShell runs a shell command in the workspace.
	StepKindShell
	// StepKindUse runs a registered action.
	StepKindUse
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
	// Steps holds every step that ran, in the order it ran.
	Steps []StepState `json:"steps"`
	// StartedAt is when the run started.
	StartedAt time.Time `json:"started_at"`
	// UpdatedAt is when the run last moved.
	UpdatedAt time.Time `json:"updated_at"`
}

// StepState is the record of one step that ran.
type StepState struct {
	// ID is the step's id.
	ID string `json:"id"`
	// Job is the job the step belongs to.
	Job string `json:"job"`
	// Outcome is the step's verdict.
	Outcome Outcome `json:"outcome"`
	// Skipped is true when the step's `if:` did not hold.
	Skipped bool `json:"skipped,omitempty"`
	// Pending is true when the step started and has not settled yet.
	// The step is still the ready one, but the plan cannot go on
	// without the agent: the command is waiting for an answer.
	Pending bool `json:"pending,omitempty"`
}

// Status is the lifecycle state of a run.
type Status string

const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// DefaultMaxActions is the action budget of a workflow that does not
// set max_actions.
const DefaultMaxActions = 30

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

// ActionKind is the kind of action recorded in the history.
type ActionKind string

const (
	// ActionKindRun is a command from one of the workflow's steps.
	ActionKindRun ActionKind = "run"
	// ActionKindInstruct is a free-form prompt to the work agent.
	ActionKindInstruct ActionKind = "instruct"
)

// ActionRecord is one action the run has taken. StepID points at the
// step the action served; only a run belongs to a step, so an
// instruction carries an empty one.
type ActionRecord struct {
	Kind    ActionKind `json:"kind"`
	Command string     `json:"command"`
	StepID  string     `json:"step_id,omitempty"`
	Outcome Outcome    `json:"outcome"`
	At      time.Time  `json:"at"`
}
