package wfe

import "errors"

// Public error sentinels. Callers (bot) compare with errors.Is.
var (
	// ErrStepFailed is wrapped in errors returned by Tick when a
	// step errored and ContinueOnError was false. Bot's drive loop
	// persists the run state in failed status.
	ErrStepFailed = errors.New("wfe: step failed")

	// ErrUnknownAction is wrapped when Runtime.RunAction looks up
	// an action name that no registered action provides.
	ErrUnknownAction = errors.New("wfe: unknown action")

	// ErrValidationFailed wraps schema validation errors from
	// Parse/LoadFile/LoadDir.
	ErrValidationFailed = errors.New("wfe: validation failed")
)
