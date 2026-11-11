package anthropic

import (
	"context"
	"encoding/json"
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
	p, err := New(llm.Config{Provider: "anthropic", BaseURL: url, Model: "test-model"})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	return p
}

func TestProvider_TextReply(t *testing.T) {
	var body map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(`{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`))
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
	if path != "/v1/messages" {
		t.Fatalf("the call went to %q, want /v1/messages", path)
	}
	if body["system"] != "be brief" {
		t.Fatalf("the system prompt is %v, want it in the top-level field", body["system"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("the request carried %d messages, want the user message", len(msgs))
	}
	user, _ := msgs[0].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("the first message is %v, want the user message", user)
	}
}

func TestProvider_SendsTheVersionHeader(t *testing.T) {
	var version string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version = r.Header.Get("anthropic-version")
		w.Write([]byte(`{"content":[],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	if _, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if version == "" {
		t.Fatal("the call carried no anthropic-version header")
	}
}

func TestProvider_SendsTheKeyHeader(t *testing.T) {
	t.Setenv("NIGHTME_TEST_ANTHROPIC_KEY", "secret-key")
	var key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("x-api-key")
		w.Write([]byte(`{"content":[],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	p, err := New(llm.Config{
		Provider: "anthropic", BaseURL: srv.URL, Model: "test-model", APIKeyEnv: "NIGHTME_TEST_ANTHROPIC_KEY",
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	if _, err := p.Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if key != "secret-key" {
		t.Fatalf("the call carried the key %q, want secret-key", key)
	}
}

func TestProvider_ToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"content":[{"type":"tool_use","id":"toolu_1","name":"run_step","input":{"command":"/gtw commit"}}],"stop_reason":"tool_use"}`))
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
	if calls[0].ToolCallID != "toolu_1" {
		t.Fatalf("the tool call has the id %q, want toolu_1", calls[0].ToolCallID)
	}
	if !strings.Contains(string(calls[0].Args), "/gtw commit") {
		t.Fatalf("the tool call lost its arguments: %s", calls[0].Args)
	}
}

func TestProvider_ToolResultRoundTrip(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(`{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`))
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
	assistant, _ := msgs[0].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("the first message is %v, want the assistant turn", assistant)
	}
	blocks, _ := assistant["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("the assistant turn carried %d blocks, want the tool call", len(blocks))
	}
	call, _ := blocks[0].(map[string]any)
	if call["type"] != "tool_use" {
		t.Fatalf("the first block is %v, want a tool call", call)
	}
	if call["id"] != "c1" || call["name"] != "run_step" {
		t.Fatalf("the tool call went out as %v, want the call of c1", call)
	}
	result, _ := msgs[1].(map[string]any)
	if result["role"] != "user" {
		t.Fatalf("the second message is %v, want the tool result as a user message", result)
	}
	resultBlocks, _ := result["content"].([]any)
	if len(resultBlocks) != 1 {
		t.Fatalf("the tool result carried %d blocks, want 1", len(resultBlocks))
	}
	toolResult, _ := resultBlocks[0].(map[string]any)
	if toolResult["type"] != "tool_result" {
		t.Fatalf("the block is %v, want a tool result", toolResult)
	}
	if toolResult["tool_use_id"] != "c1" {
		t.Fatalf("the tool result has the id %v, want c1", toolResult["tool_use_id"])
	}
	if toolResult["content"] != "ok" {
		t.Fatalf("the tool result has the content %v, want ok", toolResult["content"])
	}
}

func TestProvider_MergesConsecutiveToolResults(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(`{"content":[],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	_, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, Parts: []llm.Part{{
				Type: llm.PartToolCall, ToolCallID: "c1", ToolName: "run_step",
			}}},
			{Role: llm.RoleTool, Parts: []llm.Part{{
				Type: llm.PartToolResult, ToolCallID: "c1", ToolName: "run_step", Text: "ok",
			}}},
			{Role: llm.RoleTool, Parts: []llm.Part{{
				Type: llm.PartToolResult, ToolCallID: "c2", ToolName: "repo_status", Text: "clean",
			}}},
		},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("the request carried %d messages, want the assistant turn and one user turn for the results", len(msgs))
	}
	assistant, _ := msgs[0].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("the first message is %v, want the assistant turn", assistant)
	}
	result, _ := msgs[1].(map[string]any)
	if result["role"] != "user" {
		t.Fatalf("the second message is %v, want the tool results as a user message", result)
	}
	blocks, _ := result["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("the user turn carried %d blocks, want both tool results", len(blocks))
	}
	for i, want := range []string{"c1", "c2"} {
		block, _ := blocks[i].(map[string]any)
		if block["tool_use_id"] != want {
			t.Fatalf("the block %d answers %v, want %s", i, block["tool_use_id"], want)
		}
	}
}

func TestProvider_SendsTheToolCatalogue(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(`{"content":[],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	_, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{
		Tools: []llm.Tool{{
			Name:        "run_step",
			Description: "run a step",
			Schema:      json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("the request carried %d tools, want 1", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "run_step" {
		t.Fatalf("the tool went out as %v, want run_step", tool)
	}
	schema, _ := tool["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("the tool schema went out as %v, want the JSON schema", tool["input_schema"])
	}
}

func TestProvider_RetriesAfterARateLimit(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
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

func TestProvider_RetriesAfterAnOverloadedError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(529)
			w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
			return
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

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
}

func TestProvider_ReportsANonRetryableFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	}))
	defer srv.Close()

	_, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{})
	if err == nil {
		t.Fatal("the call was accepted")
	}
	if llm.Retryable(err) {
		t.Fatal("an authentication failure must not be retryable")
	}
	if !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Fatalf("error = %q, want the provider's message", err)
	}
}

func TestProvider_DropsAnEmptyTextBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"content":[{"type":"text","text":""},{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	resp, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(resp.Message.Parts) != 1 {
		t.Fatalf("the answer has %d parts, want only the text that carries something", len(resp.Message.Parts))
	}
	if resp.Message.Text() != "hi" {
		t.Fatalf("text = %q, want hi", resp.Message.Text())
	}
}

func TestProvider_KeepsTheHistorySendable(t *testing.T) {
	// A gateway that answers a tool call with arguments that are not
	// an object leaves the history unsendable: every turn that
	// replays it fails. The call carries no arguments instead.
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(`{"content":[{"type":"tool_use","id":"toolu_1","name":"run_step","input":"oops"}],"stop_reason":"tool_use"}`))
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
		t.Fatalf("the tool call carries the arguments %s, want an empty object", calls[0].Args)
	}

	// The call replays into the next turn without a failure.
	_, err = newProvider(t, srv.URL).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{resp.Message},
	})
	if err != nil {
		t.Fatalf("the call was not replayable: %v", err)
	}
	msgs, _ := body["messages"].([]any)
	assistant, _ := msgs[0].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	call, _ := blocks[0].(map[string]any)
	input, _ := call["input"].(map[string]any)
	if input == nil || len(input) != 0 {
		t.Fatalf("the tool call went out with the input %v, want an empty object", call["input"])
	}
}

func TestProvider_ReadsAnUntypedPartAsText(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(`{"content":[],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	_, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("the request carried %d messages, want the user message", len(msgs))
	}
	user, _ := msgs[0].(map[string]any)
	blocks, _ := user["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("the user turn carried %d blocks, want the text", len(blocks))
	}
	block, _ := blocks[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hi" {
		t.Fatalf("the block went out as %v, want the text of the part", block)
	}
}

func TestProvider_DropsAnEmptyTextPart(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(`{"content":[],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	_, err := newProvider(t, srv.URL).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{
			{Type: llm.PartText, Text: ""},
			{Type: llm.PartText, Text: "hi"},
		}}},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	msgs, _ := body["messages"].([]any)
	user, _ := msgs[0].(map[string]any)
	blocks, _ := user["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("the user turn carried %d blocks, want only the text that carries something", len(blocks))
	}
	block, _ := blocks[0].(map[string]any)
	if block["text"] != "hi" {
		t.Fatalf("the block went out as %v, want the text that carries something", block)
	}
}

func TestNew_IsRegisteredUnderItsName(t *testing.T) {
	p, err := llm.New(llm.Config{Provider: "anthropic", BaseURL: "https://example.test", Model: "m"})
	if err != nil {
		t.Fatalf("the provider is not registered: %v", err)
	}
	if p == nil {
		t.Fatal("the registry returned no provider")
	}
}

func TestNew_RequiresABaseURL(t *testing.T) {
	if _, err := New(llm.Config{Provider: "anthropic", Model: "m"}); err == nil {
		t.Fatal("a provider without a base url was accepted")
	}
}

func TestNew_ReportsAMissingKey(t *testing.T) {
	t.Setenv("NIGHTME_TEST_ANTHROPIC_KEY", "")
	if _, err := New(llm.Config{
		Provider: "anthropic", BaseURL: "https://example.test", Model: "m", APIKeyEnv: "NIGHTME_TEST_ANTHROPIC_KEY",
	}); err == nil {
		t.Fatal("a missing api key was accepted")
	}
}
