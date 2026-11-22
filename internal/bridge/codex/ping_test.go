package codex

// The liveness probe.
//
// The codex app-server speaks JSON-RPC 2.0. The probe asks for
// getAuthStatus — the cheapest call in the protocol, which reads
// the auth state and touches neither the thread nor the turn, so it
// answers while a turn is running.
//
// An error response counts as an answer: the app-server received
// the request and replied. Only the absence of a reply means "not
// responding".

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestPing_FakeAppServerAnswersGetAuthStatus — the probe resolves
// against a fake app-server that answers getAuthStatus.
func TestPing_FakeAppServerAnswersGetAuthStatus(t *testing.T) {
	stub, client, cleanup := newServerStub(t)
	defer cleanup()

	d := &driver{session: &session{rpc: client}}

	answered := make(chan error, 1)
	go func() { answered <- d.Ping(context.Background()) }()

	frame := stub.readFrame(t, 3*time.Second)
	if frame == "" {
		t.Fatal("the probe wrote no frame")
	}
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(frame), &request); err != nil {
		t.Fatalf("the probe frame is not JSON: %v (%s)", err, frame)
	}
	if request.Method != "getAuthStatus" {
		t.Fatalf("the probe asked for %q, want getAuthStatus", request.Method)
	}
	// The app-server rejects a request whose params field is
	// missing, so the probe carries an empty one.
	var params map[string]json.RawMessage
	if err := json.Unmarshal(request.Params, &params); err != nil {
		t.Fatalf("the probe request carries no params object: %v (%s)", err, frame)
	}

	stub.writeLine(t, `{"id":`+string(request.ID)+`,"result":{"authMethod":"apikey","requiresOpenaiAuth":true}}`)

	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("Ping() = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the probe did not resolve")
	}
}

// TestPing_AnErrorAnswerStillCountsAsAnAnswer — the app-server
// answered; the fact that it answered with an error does not make it
// unresponsive.
func TestPing_AnErrorAnswerStillCountsAsAnAnswer(t *testing.T) {
	stub, client, cleanup := newServerStub(t)
	defer cleanup()

	d := &driver{session: &session{rpc: client}}

	answered := make(chan error, 1)
	go func() { answered <- d.Ping(context.Background()) }()

	frame := stub.readFrame(t, 3*time.Second)
	if frame == "" {
		t.Fatal("the probe wrote no frame")
	}
	var request struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(frame), &request); err != nil {
		t.Fatalf("the probe frame is not JSON: %v", err)
	}
	stub.writeLine(t, `{"id":`+string(request.ID)+`,"error":{"code":-32600,"message":"unknown variant"}}`)

	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("Ping() = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the probe did not resolve")
	}
}

// TestPing_AnUnansweredProbeFails — no answer at all is the failure
// the daemon acts on.
func TestPing_AnUnansweredProbeFails(t *testing.T) {
	_, client, cleanup := newServerStub(t)
	defer cleanup()

	d := &driver{session: &session{rpc: client}}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := d.Ping(ctx); err == nil {
		t.Fatal("Ping() = nil, want an error when the app-server stays silent")
	}
}
