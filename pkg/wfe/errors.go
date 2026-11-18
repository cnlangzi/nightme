package wfe

import "errors"

// Public error sentinels. Callers (internal/me) compare with
// errors.Is.
var (
	// ErrStepFailed is wrapped in errors returned by Tick when a
	// step errored and ContinueOnError was false. The drive loop
	// persists the run state in failed status.
	ErrStepFailed = errors.New("wfe: step failed")

	// ErrUnknownAction is wrapped when Runtime.RunAction looks up
	// an action name that no registered action provides.
	ErrUnknownAction = errors.New("wfe: unknown action")

	// ErrValidationFailed is wrapped in the error of a workflow that
	// does not validate. Parse/LoadFile/LoadDir return it as a
	// *ValidationError, which carries every problem that was found.
	ErrValidationFailed = errors.New("wfe: validation failed")
)
