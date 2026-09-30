package host

import (
	"encoding/json"
	"testing"
)

func TestTranslateSessionEvent_AssistantStreamChunk(t *testing.T) {
	raw := []byte(`{
		"type": "assistant-stream",
		"frame": {
			"type": "chunk",
			"index": 2,
			"chunk": {"type": "block-end", "index": 0, "block": {"type": "reasoning", "text": "think"}}
		}
	}`)
	method, rpcID, payload := translateSessionEvent(raw, "session-1")
	if method != "assistant/chunk" {
		t.Fatalf("method = %q", method)
	}
	if rpcID != "astream-2" {
		t.Fatalf("rpcID = %q", rpcID)
	}
	var body struct {
		SessionID string `json:"sessionId"`
		Chunk     struct {
			Type  string `json:"type"`
			Block struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"block"`
		} `json:"chunk"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if body.SessionID != "session-1" || body.Chunk.Type != "block-end" || body.Chunk.Block.Text != "think" {
		t.Fatalf("payload = %+v", body)
	}
}

func TestTranslateSessionEvent_AssistantStreamStartDropped(t *testing.T) {
	raw := []byte(`{"type":"assistant-stream","frame":{"type":"start","turn":1,"step":0}}`)
	method, rpcID, payload := translateSessionEvent(raw, "session-1")
	if method != "" || rpcID != "" || payload != nil {
		t.Fatalf("start frame translated: method=%q rpcID=%q payload=%s", method, rpcID, payload)
	}
}

func TestSessionOpenPayload_RequestsAssistantStream(t *testing.T) {
	var body struct {
		Args struct {
			Request struct {
				AssistantStream bool `json:"assistantStream"`
				Address         struct {
					Kind      string `json:"kind"`
					SessionID string `json:"sessionId"`
				} `json:"address"`
			} `json:"request"`
		} `json:"args"`
	}
	if err := json.Unmarshal(sessionOpenPayload("session-9"), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Args.Request.AssistantStream {
		t.Fatal("assistantStream not set")
	}
	if body.Args.Request.Address.Kind != "session" || body.Args.Request.Address.SessionID != "session-9" {
		t.Fatalf("address = %+v", body.Args.Request.Address)
	}
}
