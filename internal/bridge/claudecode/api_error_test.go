package claudecode

import (
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// streamEvents runs pumpStream over an in-memory stream and collects
// the events it produced.
func streamEvents(t *testing.T, input string) []agent.AgentEvent {
	t.Helper()
	events := make(chan agent.AgentEvent, 16)
	go func() {
		pumpStream(strings.NewReader(input), events, nil, nil, nil, "claude", "/tmp", "main", nil)
		close(events)
	}()
	var got []agent.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	return got
}

// resultError returns the error the result event of a stream carries.
func resultError(t *testing.T, events []agent.AgentEvent) error {
	t.Helper()
	for _, ev := range events {
		if ev.Kind != agent.EventAgentResult {
			continue
		}
		return ev.Err
	}
	t.Fatal("the stream produced no result event")
	return nil
}

// TestPumpStream_ResultCarriesTheAPIStatus — the status the CLI
// reported on the wire reaches the event as a structured error, so the
// layers above branch on the number instead of on the words.
func TestPumpStream_ResultCarriesTheAPIStatus(t *testing.T) {
	input := `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429,"result":"API Error: Request rejected (429)"}` + "\n"

	err := resultError(t, streamEvents(t, input))
	if err == nil {
		t.Fatal("the result event carries no error")
	}
	var apiErr *agent.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("the error %v is not an *agent.APIError", err)
	}
	if apiErr.Status != 429 {
		t.Fatalf("status = %d, want 429", apiErr.Status)
	}
}

// TestPumpStream_TheAssistantFrameCarriesTheAPIStatus — the CLI puts
// the status on the assistant frame of the turn that hit the failure,
// and the result frame that closes the turn may carry none.
func TestPumpStream_TheAssistantFrameCarriesTheAPIStatus(t *testing.T) {
	input := `{"type":"assistant","is_api_error_message":true,"api_error_status":429,"api_error":"rate_limit","error_details":"Request rejected","message":{"id":"m1","role":"assistant","content":[]}}` + "\n" +
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: Request rejected (429)"}` + "\n"

	err := resultError(t, streamEvents(t, input))
	var apiErr *agent.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("the error %v is not an *agent.APIError", err)
	}
	if apiErr.Status != 429 {
		t.Fatalf("status = %d, want 429", apiErr.Status)
	}
	if apiErr.Code != "rate_limit" {
		t.Fatalf("code = %q, want %q", apiErr.Code, "rate_limit")
	}
	if apiErr.Message != "Request rejected" {
		t.Fatalf("message = %q, want %q", apiErr.Message, "Request rejected")
	}
}

// TestPumpStream_TheCamelCaseSpellingIsRead — the transcript the CLI
// writes to disk spells the fields in camelCase.
func TestPumpStream_TheCamelCaseSpellingIsRead(t *testing.T) {
	input := `{"type":"result","subtype":"error_during_execution","is_error":true,"apiErrorStatus":503,"result":"overloaded"}` + "\n"

	err := resultError(t, streamEvents(t, input))
	var apiErr *agent.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("the error %v is not an *agent.APIError", err)
	}
	if apiErr.Status != 503 {
		t.Fatalf("status = %d, want 503", apiErr.Status)
	}
}

// TestPumpStream_AFailureDoesNotCarryIntoTheNextTurn — the status a
// turn reported belongs to that turn.
//
// The CLI reports an API failure on the assistant frame of the turn
// that hit it, and the result frame that closes the turn reads it
// back. A turn that comes after must not inherit it.
func TestPumpStream_AFailureDoesNotCarryIntoTheNextTurn(t *testing.T) {
	input := `{"type":"assistant","is_api_error_message":true,"api_error_status":429,"message":{"id":"m1","role":"assistant","content":[]}}` + "\n" +
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"rate limited"}` + "\n" +
		`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"the turn ran out of turns"}` + "\n"

	var results []error
	for _, ev := range streamEvents(t, input) {
		if ev.Kind != agent.EventAgentResult {
			continue
		}
		results = append(results, ev.Err)
	}
	if len(results) != 2 {
		t.Fatalf("the stream produced %d results, want 2", len(results))
	}
	var first, second *agent.APIError
	if !errors.As(results[0], &first) || first.Status != 429 {
		t.Fatalf("the first turn's error is %v, want a 429", results[0])
	}
	if !errors.As(results[1], &second) {
		t.Fatalf("the second turn's error is %v, want an API error", results[1])
	}
	if second.Status != 0 {
		t.Errorf("the second turn's status = %d, want none — it inherits the first turn's", second.Status)
	}
	if second.Code == first.Code {
		t.Errorf("the second turn's code = %q, want none — it inherits the first turn's", second.Code)
	}
}

// TestPumpStream_AResultWithoutAStatusStillFails — a CLI that reports
// no status leaves the error without one; the caller falls back to the
// reason the turn ended.
func TestPumpStream_AResultWithoutAStatusStillFails(t *testing.T) {
	input := `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"the turn ran out of turns"}` + "\n"

	err := resultError(t, streamEvents(t, input))
	if err == nil {
		t.Fatal("the result event carries no error")
	}
	var apiErr *agent.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("the error %v is not an *agent.APIError", err)
	}
	if apiErr.Status != 0 {
		t.Fatalf("status = %d, want 0", apiErr.Status)
	}
	if apiErr.Code != "error_max_turns" {
		t.Fatalf("code = %q, want %q", apiErr.Code, "error_max_turns")
	}
}
