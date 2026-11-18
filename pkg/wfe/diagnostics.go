package wfe

import (
	"strconv"
	"strings"
)

// Diagnostic is one problem found in a workflow.
//
// It carries the place the problem was found and what is wrong with
// it, so a consumer can point a CI log or an editor at it.
type Diagnostic struct {
	// Path is the file the problem is in. It is empty when the
	// workflow came from bytes.
	Path string `json:"path"`
	// Line is the 1-based line the problem is on. It is 0 when the
	// problem has no place in the file.
	Line int `json:"line"`
	// Column is the 1-based column the problem is on. It is 0 when
	// the problem has no place in the file.
	Column int `json:"column"`
	// Message says what is wrong.
	Message string `json:"message"`
}

// Error renders the diagnostic the way a compiler does:
// "path:line:column: message". The segments that are unknown are
// left out.
func (d Diagnostic) Error() string {
	parts := make([]string, 0, 3)
	if d.Path != "" {
		parts = append(parts, d.Path)
	}
	if d.Line > 0 {
		parts = append(parts, strconv.Itoa(d.Line))
		if d.Column > 0 {
			parts = append(parts, strconv.Itoa(d.Column))
		}
	}
	if len(parts) == 0 {
		return d.Message
	}
	return strings.Join(parts, ":") + ": " + d.Message
}

// ValidationError is the error a workflow that does not validate
// produces.
//
// It carries every problem that was found, so a caller learns all of
// them from one call.
type ValidationError struct {
	// Diagnostics is every problem the workflow has.
	Diagnostics []Diagnostic
}

// Error renders every diagnostic it carries, one per line.
func (e *ValidationError) Error() string {
	if e == nil || len(e.Diagnostics) == 0 {
		return ErrValidationFailed.Error()
	}
	lines := make([]string, 0, len(e.Diagnostics))
	for _, d := range e.Diagnostics {
		lines = append(lines, d.Error())
	}
	return strings.Join(lines, "\n")
}

// Unwrap makes errors.Is(err, ErrValidationFailed) true for a
// workflow that does not validate.
func (e *ValidationError) Unwrap() error {
	return ErrValidationFailed
}
