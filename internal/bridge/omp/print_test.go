package omp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestParsePrintStream_ProviderError asserts that a print-mode
// run whose upstream provider failed is reported as an error
// rather than handed back as a successful result.
//
// omp reports the failure on the assistant message_end
// (stopReason "error" plus errorMessage) and leaves the content
// empty. Without this the provider's message would come back as
// RunResult.Text, and callers that parse that text as the
// agent's own output (/gtw commit builds a commit body from it)
// would treat an upstream failure as the model's answer.
func TestParsePrintStream_ProviderError(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"session","version":3,"id":"s-1"}`,
		`{"type":"message_end","message":{"role":"assistant","content":[],"model":"kimi-k3","stopReason":"error","errorMessage":"InvalidHTTPResponse fetching upstream","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0}}}`,
		`{"type":"agent_end","isTerminal":true}`,
	}, "\n") + "\n"

	_, err := parsePrintStream(context.Background(), strings.NewReader(stream), t.TempDir(), nil)
	if err == nil {
		t.Fatal("parsePrintStream() = nil error, want the provider failure")
	}
	if !errors.Is(err, ErrModelError) {
		t.Errorf("err = %v, want it to wrap ErrModelError", err)
	}
	if !strings.Contains(err.Error(), "InvalidHTTPResponse") {
		t.Errorf("err = %v, want the provider's own message", err)
	}
}

// TestParsePrintStream_SuccessUnaffected asserts the ordinary
// print-mode path still returns the assistant's text.
func TestParsePrintStream_SuccessUnaffected(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"session","version":3,"id":"s-1"}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"pong"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_end","contentIndex":0}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"pong"}],"model":"kimi-k3","usage":{"input":10,"output":2,"cacheRead":0,"cacheWrite":0,"totalTokens":12}}}`,
		`{"type":"agent_end","isTerminal":true}`,
	}, "\n") + "\n"

	res, err := parsePrintStream(context.Background(), strings.NewReader(stream), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("parsePrintStream() = %v, want nil", err)
	}
	if res.Text != "pong" {
		t.Errorf("Text = %q, want %q", res.Text, "pong")
	}
}
