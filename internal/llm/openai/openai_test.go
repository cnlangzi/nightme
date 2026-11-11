package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
)

// newProvider builds a provider pointed at a test server.
func newProvider(t *testing.T, url string) llm.Provider {
	t.Helper()
	p, err := New(llm.Config{Provider: "openai", BaseURL: url, Model: "test-model"})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	return p
}

func TestProvider_TextReply(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
	}))
	defer srv.Close()

	resp, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{
		System:   "be brief",
		Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if resp.Message.Text() != "hello" {
		t.Fatalf("text = %q, want hello", resp.Message.Text())
	}
	if resp.StopReason != llm.StopEnd {
		t.Fatalf("stop reason = %q, want %q", resp.StopReason, llm.StopEnd)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 4 {
		t.Fatalf("usage = %+v, want 3 input and 4 output tokens", resp.Usage)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("the request carried %d messages, want the system prompt and the user message", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("the first message is %v, want the system prompt", first)
	}
}

func TestProvider_SingleToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"run_step","arguments":"{\"command\":\"/gtw commit\"}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	resp, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if resp.StopReason != llm.StopToolCalls {
		t.Fatalf("stop reason = %q, want %q", resp.StopReason, llm.StopToolCalls)
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("the answer has %d tool calls, want 1", len(calls))
	}
	if calls[0].ToolName != "run_step" {
		t.Fatalf("the tool call names %q, want run_step", calls[0].ToolName)
	}
	if calls[0].ToolCallID != "c1" {
		t.Fatalf("the tool call has the id %q, want c1", calls[0].ToolCallID)
	}
	if !strings.Contains(string(calls[0].Args), "/gtw commit") {
		t.Fatalf("the tool call lost its arguments: %s", calls[0].Args)
	}
}

func TestProvider_ParallelToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"run_step","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"repo_status","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	resp, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 2 {
		t.Fatalf("the answer has %d tool calls, want 2", len(calls))
	}
	if calls[1].ToolName != "repo_status" {
		t.Fatalf("the second tool call names %q, want repo_status", calls[1].ToolName)
	}
}

func TestProvider_ToolResultRoundTrip(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	_, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, Parts: []llm.Part{{
				Type: llm.PartToolCall, ToolCallID: "c1", ToolName: "run_step",
				Args: json.RawMessage(`{"command":"/gtw commit"}`),
			}}},
			{Role: llm.RoleTool, Parts: []llm.Part{{
				Type: llm.PartToolResult, ToolCallID: "c1", ToolName: "run_step", Text: "ok",
			}}},
		},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("the request carried %d messages, want the assistant turn and the tool result", len(msgs))
	}
	tool, _ := msgs[1].(map[string]any)
	if tool["role"] != "tool" {
		t.Fatalf("the second message is %v, want the tool result", tool)
	}
	if tool["tool_call_id"] != "c1" {
		t.Fatalf("the tool result has the id %v, want c1", tool["tool_call_id"])
	}
	if tool["content"] != "ok" {
		t.Fatalf("the tool result has the content %v, want ok", tool["content"])
	}
}

func TestProvider_CompatQuirks(t *testing.T) {
	t.Run("no usage", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
		}))
		defer srv.Close()
		resp, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{})
		if err != nil {
			t.Fatalf("complete: %v", err)
		}
		if resp.Usage.Total() != 0 {
			t.Fatalf("usage = %+v, want zero", resp.Usage)
		}
	})

	t.Run("empty arguments", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"repo_status","arguments":""}}]},"finish_reason":"tool_calls"}]}`))
		}))
		defer srv.Close()
		resp, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{})
		if err != nil {
			t.Fatalf("complete: %v", err)
		}
		calls := resp.Message.ToolCalls()
		if len(calls) != 1 {
			t.Fatalf("the answer has %d tool calls, want 1", len(calls))
		}
		if string(calls[0].Args) != "{}" {
			t.Fatalf("the arguments are %s, want an empty object", calls[0].Args)
		}
	})
}

func TestProvider_RetriesAfterARateLimit(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	started := time.Now()
	resp, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if resp.Message.Text() != "ok" {
		t.Fatalf("text = %q, want ok", resp.Message.Text())
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("the server saw %d calls, want 2", calls)
	}
	// The server asked for a one second wait. The local backoff is
	// half a second, so the elapsed time tells the two apart.
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("the retry waited %v, want it to honour the Retry-After of 1s", elapsed)
	}
}

func TestProvider_ReplaysAToolCallWithoutArguments(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	_, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleAssistant, Parts: []llm.Part{{
			Type: llm.PartToolCall, ToolCallID: "c1", ToolName: "repo_status",
		}}}},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	msgs, _ := body["messages"].([]any)
	assistant, _ := msgs[0].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("the request carried %d tool calls, want 1", len(calls))
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if fn["arguments"] != "{}" {
		t.Fatalf("the tool call went out with the arguments %v, want an empty object", fn["arguments"])
	}
}

func TestProvider_ReportsANonRetryableFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid api key","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	_, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{})
	if err == nil {
		t.Fatal("the call was accepted")
	}
	if llm.Retryable(err) {
		t.Fatal("an authentication failure must not be retryable")
	}
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("error = %q, want the provider's message", err)
	}
}

func TestProvider_HonoursTheContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := newProvider(t, srv.URL).Complete(ctx, llm.Request{})
	if err == nil {
		t.Fatal("the call was not cancelled")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %q, want the deadline", err)
	}
}

func TestNew_RequiresABaseURL(t *testing.T) {
	if _, err := New(llm.Config{Provider: "openai", Model: "m"}); err == nil {
		t.Fatal("a provider without a base url was accepted")
	}
}

func TestNew_ReportsAMissingKey(t *testing.T) {
	t.Setenv("NIGHTME_TEST_LLM_KEY", "")
	if _, err := New(llm.Config{
		Provider: "openai", BaseURL: "https://example.test", Model: "m", APIKeyEnv: "NIGHTME_TEST_LLM_KEY",
	}); err == nil {
		t.Fatal("a missing api key was accepted")
	}
}
