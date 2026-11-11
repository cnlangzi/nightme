package llm

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMessage_RoundTripsThroughJSON(t *testing.T) {
	msg := Message{
		Role: RoleAssistant,
		Parts: []Part{
			{Type: PartText, Text: "on it"},
			{Type: PartToolCall, ToolCallID: "c1", ToolName: "run_step", Args: json.RawMessage(`{"command":"/gtw commit"}`)},
			{Type: PartToolResult, ToolCallID: "c1", ToolName: "run_step", Text: "ok", IsError: false},
		},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Message
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Role != msg.Role {
		t.Fatalf("role = %q, want %q", back.Role, msg.Role)
	}
	if len(back.Parts) != len(msg.Parts) {
		t.Fatalf("the message has %d parts, want %d", len(back.Parts), len(msg.Parts))
	}
	if back.Parts[1].ToolName != "run_step" {
		t.Fatalf("the tool call lost its name: %+v", back.Parts[1])
	}
	if string(back.Parts[1].Args) != `{"command":"/gtw commit"}` {
		t.Fatalf("the tool call lost its arguments: %s", back.Parts[1].Args)
	}
}

func TestMessage_TextAndToolCalls(t *testing.T) {
	msg := Message{Role: RoleAssistant, Parts: []Part{
		{Type: PartText, Text: "hello"},
		{Type: PartToolCall, ToolCallID: "c1", ToolName: "run_step"},
	}}
	if msg.Text() != "hello" {
		t.Fatalf("text = %q, want hello", msg.Text())
	}
	if len(msg.ToolCalls()) != 1 {
		t.Fatalf("the message has %d tool calls, want 1", len(msg.ToolCalls()))
	}
}

func TestDecisionRequest_Validate(t *testing.T) {
	valid := DecisionRequest{
		State: "the run is halfway",
		Questions: []Question{
			{Name: "next", Type: QuestionChoice, Options: []Option{{Value: "a"}, {Value: "b"}}},
			{Name: "needs_human", Type: QuestionYesNo},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("the request is valid: %v", err)
	}

	cases := []struct {
		name string
		req  DecisionRequest
		want string
	}{
		{
			name: "no questions",
			req:  DecisionRequest{State: "x"},
			want: "questions",
		},
		{
			name: "no name",
			req:  DecisionRequest{Questions: []Question{{Type: QuestionYesNo}}},
			want: "no name",
		},
		{
			name: "the same name twice",
			req: DecisionRequest{Questions: []Question{
				{Name: "next", Type: QuestionYesNo},
				{Name: "next", Type: QuestionYesNo},
			}},
			want: "asked twice",
		},
		{
			name: "one option",
			req: DecisionRequest{Questions: []Question{
				{Name: "next", Type: QuestionChoice, Options: []Option{{Value: "a"}}},
			}},
			want: "options",
		},
		{
			name: "the same option twice",
			req: DecisionRequest{Questions: []Question{
				{Name: "next", Type: QuestionChoice, Options: []Option{{Value: "a"}, {Value: "a"}}},
			}},
			want: "listed twice",
		},
		{
			name: "one level",
			req: DecisionRequest{Questions: []Question{
				{Name: "score", Type: QuestionScore, Levels: []Level{{Label: "low"}}},
			}},
			want: "levels",
		},
		{
			name: "an unknown type",
			req: DecisionRequest{Questions: []Question{
				{Name: "next", Type: "colour"},
			}},
			want: "unknown question type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if err == nil {
				t.Fatalf("the request was accepted, want an error about %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestRegistry_UnknownProvider(t *testing.T) {
	if _, err := New(Config{Provider: "nope", Model: "m"}); err == nil {
		t.Fatal("an unknown provider was accepted")
	}
	if _, err := New(Config{Model: "m"}); err == nil {
		t.Fatal("a missing provider was accepted")
	}
	if _, err := New(Config{Provider: "openai"}); err == nil {
		t.Fatal("a provider without a model was accepted")
	}
}

func TestRegistry_UnknownDecider(t *testing.T) {
	if _, err := NewDecider(Config{Protocol: "nope", Model: "m"}); err == nil {
		t.Fatal("an unknown protocol was accepted")
	}
	if _, err := NewDecider(Config{Model: "m"}); err == nil {
		t.Fatal("a missing protocol was accepted")
	}
	if _, err := NewDecider(Config{Protocol: "decisions"}); err == nil {
		t.Fatal("a protocol without a model was accepted")
	}
}

func TestRegistry_RegisteredNames(t *testing.T) {
	Register("zzz-test", func(Config) (Provider, error) { return nil, nil })
	RegisterDecider("zzz-test", func(Config) (Decider, error) { return nil, nil })
	if !contains(ProviderNames(), "zzz-test") {
		t.Fatalf("the providers are %v, want zzz-test", ProviderNames())
	}
	if !contains(DeciderNames(), "zzz-test") {
		t.Fatalf("the deciders are %v, want zzz-test", DeciderNames())
	}
}

func TestUsage_Add(t *testing.T) {
	var u Usage
	u.Add(Usage{InputTokens: 3, OutputTokens: 4})
	u.Add(Usage{InputTokens: 1, OutputTokens: 2})
	if u.InputTokens != 4 || u.OutputTokens != 6 {
		t.Fatalf("usage = %+v, want 4 input and 6 output tokens", u)
	}
	if u.Total() != 10 {
		t.Fatalf("total = %d, want 10", u.Total())
	}
}

func TestRetryAfter_ParsesTheDelay(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	if got := retryAfter(resp); got != 0 {
		t.Fatalf("retry after = %v, want 0", got)
	}
	resp.Header.Set("Retry-After", "3")
	if got := retryAfter(resp); got != 3*time.Second {
		t.Fatalf("retry after = %v, want 3s", got)
	}
	resp.Header.Set("Retry-After", "Wed, 21 Oct 2015 07:28:00 GMT")
	if got := retryAfter(resp); got != 0 {
		t.Fatalf("retry after = %v, want 0 for a date", got)
	}
}

func TestStatusError_ClassifiesTheFailures(t *testing.T) {
	unauthorized := statusError(http.StatusUnauthorized, []byte(`{"error":{"message":"bad key"}}`))
	if Retryable(unauthorized) {
		t.Fatal("an authentication failure must not be retryable")
	}
	if !strings.Contains(unauthorized.Error(), "bad key") {
		t.Fatalf("error = %q, want the provider's message", unauthorized)
	}
	rateLimited := statusError(http.StatusTooManyRequests, []byte(`{}`))
	if !Retryable(rateLimited) {
		t.Fatal("a rate limit must be retryable")
	}
	serverError := statusError(http.StatusBadGateway, []byte(`{}`))
	if !Retryable(serverError) {
		t.Fatal("a 5xx must be retryable")
	}
	badRequest := statusError(http.StatusBadRequest, []byte(`{"error":{"code":"x","message":"nope"}}`))
	if Retryable(badRequest) {
		t.Fatal("a bad request must not be retryable")
	}
	if !strings.Contains(badRequest.Error(), "x") {
		t.Fatalf("error = %q, want the provider's code", badRequest)
	}
}

// contains reports whether the list holds the value.
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
