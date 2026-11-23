package agent

import (
	"fmt"
	"strings"
)

// APIError is a failure an upstream reported with an HTTP status.
//
// The bridge reads the status off the wire and hands it over as it
// is: the layers above branch on the number, not on the words the
// upstream wrote. A 429 here is a rate limit, and the run waits for
// the operator instead of retrying into the same wall.
type APIError struct {
	// Status is the HTTP status of the failure.
	Status int
	// Code is the upstream's own machine-readable code, when it sent
	// one.
	Code string
	// Message is the upstream's own words.
	Message string
}

// Error renders the failure for a person: the status is the fact, the
// code and the message say what the upstream said about it.
func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("the turn failed")
	if e.Status > 0 {
		fmt.Fprintf(&b, " with status %d", e.Status)
	}
	if e.Code != "" {
		fmt.Fprintf(&b, " (%s)", e.Code)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}
