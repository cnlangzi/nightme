package command

import "context"

// MeRequest is a request to run a workflow.
//
// The types here are the command package's own view of a run, so the
// command layer does not depend on the personal agent's internals.
type MeRequest struct {
	// Name is the workflow's name.
	Name string
	// Args are the arguments of the command line. A token of the form
	// name=value assigns that argument by name; every other token
	// fills the next declared argument, in order.
	Args []string
	// ObserverChatID is the chat the run reports to.
	ObserverChatID string
	// Cwd is the working directory the run works in.
	Cwd string
	// Agent is the agent the work chat uses.
	Agent string
}

// MeStatus is the state of a run.
type MeStatus struct {
	// Phase is where the run stands: idle, running, waiting, paused,
	// done, failed or stopped.
	Phase string
	// RunID is the run's id.
	RunID string
	// Step is the command the run is on.
	Step string
	// Execution is the phase of the running command.
	Execution string
	// Progress is the heartbeat view of the running command.
	Progress string
	// Actions is the number of actions the run took.
	Actions int
	// System1Decisions is the number of decisions the decision model
	// made.
	System1Decisions int
	// System2Decisions is the number of turns the chat model took.
	System2Decisions int
	// System1Tokens is the token usage of the decision model.
	System1Tokens int
	// System2Tokens is the token usage of the chat model.
	System2Tokens int
	// Question is the question the run is waiting for.
	Question string
	// Error is why the run cannot go on, when it cannot.
	Error string
}

// MeController is the personal agent the /me command drives.
//
// One run is active per chat: the chat that starts a run is the chat
// that gets the progress and answers the questions.
type MeController interface {
	// StartWorkflow starts a workflow and returns the run's id.
	StartWorkflow(ctx context.Context, req MeRequest) (string, error)
	// Reply hands the user's reply to the run that waits for one.
	Reply(ctx context.Context, observerChatID, text string) error
	// Stop cancels the run of a chat.
	Stop(ctx context.Context, observerChatID string) error
	// Status returns the state of the run of a chat.
	Status(observerChatID string) MeStatus
}
