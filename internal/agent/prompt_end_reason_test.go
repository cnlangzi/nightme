package agent

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/pkg/outcome"
)

// TestPromptEndReason_Status — the end reason folds into the HTTP
// status of the outcome the turn produces.
func TestPromptEndReason_Status(t *testing.T) {
	cases := []struct {
		name   string
		reason PromptEndReason
		want   int
	}{
		{"clean", PromptEndClean, http.StatusOK},
		{"error", PromptEndError, http.StatusInternalServerError},
		{"process died", PromptEndProcessDied, http.StatusInternalServerError},
		{"stalled and killed", PromptEndStalledKilled, http.StatusRequestTimeout},
		{"user killed", PromptEndUserKilled, outcome.StatusClientClosedRequest},
		{"user stopped", PromptEndUserStopped, outcome.StatusClientClosedRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.reason.Status(); got != tc.want {
				t.Fatalf("Status() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestPromptEndReason_AnUnknownReasonIsAFailure — a reason nobody
// named leaves the work unfinished, which is a failure.
func TestPromptEndReason_AnUnknownReasonIsAFailure(t *testing.T) {
	if got := PromptEndReason(99).Status(); got != http.StatusInternalServerError {
		t.Fatalf("Status() = %d, want %d", got, http.StatusInternalServerError)
	}
}

// TestAPIError_Error — the error names the status, the code and the
// upstream's own words.
func TestAPIError_Error(t *testing.T) {
	err := &APIError{Status: http.StatusTooManyRequests, Code: "rate_limit", Message: "Request rejected"}
	msg := err.Error()
	for _, want := range []string{"429", "rate_limit", "Request rejected"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the message %q does not mention %q", msg, want)
		}
	}
}
