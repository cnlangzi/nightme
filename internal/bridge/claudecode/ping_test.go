package claudecode

// The liveness probe.
//
// Claude Code answers a control_request on its control channel. The
// probe asks for the usage summary — a request the CLI answers while
// a turn runs, and one that changes nothing about the turn.
//
// Two details of the wire shape matter:
//
//   - The request_id has to sit at the top level of the request,
//     next to "type" and "request". The CLI echoes it back only
//     from there; nested inside "request" the answer carries no id
//     and cannot be correlated.
//   - The answer is resolved from the bridge's own pending-probe
//     map, not from the event channel. The probe exists to answer
//     "is the CLI still responsive", so its answer must not travel
//     the path that can be wedged.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// frameSink collects the frames the bridge writes to its stdin and
// hands each complete line to the test.
type frameSink struct {
	mu    sync.Mutex
	buf   []byte
	lines chan string
}

func newFrameSink() *frameSink {
	return &frameSink{lines: make(chan string, 16)}
}

func (s *frameSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.buf = append(s.buf, p...)
	var ready []string
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		ready = append(ready, string(s.buf[:i]))
		s.buf = s.buf[i+1:]
	}
	s.mu.Unlock()
	for _, line := range ready {
		s.lines <- line
	}
	return len(p), nil
}

// probeDriver returns a driver whose stdin is the given sink and
// whose pending-probe map is ready.
func probeDriver(sink *frameSink) *driver {
	d := &driver{stdin: bufio.NewWriter(sink)}
	d.pendingProbes = make(map[string]chan struct{})
	return d
}

// TestPing_WritesAControlRequestWithATopLevelRequestID — the
// request_id has to be a sibling of "type" and "request": the CLI
// echoes it back from nowhere else.
func TestPing_WritesAControlRequestWithATopLevelRequestID(t *testing.T) {
	sink := newFrameSink()
	d := probeDriver(sink)

	go func() {
		_ = d.Ping(context.Background())
	}()

	var frame string
	select {
	case frame = <-sink.lines:
	case <-time.After(3 * time.Second):
		t.Fatal("the probe wrote no frame")
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(frame), &top); err != nil {
		t.Fatalf("the probe frame is not JSON: %v (%s)", err, frame)
	}
	if string(top["type"]) != `"control_request"` {
		t.Fatalf("the probe frame type = %s, want control_request", top["type"])
	}
	if _, ok := top["request_id"]; !ok {
		t.Fatalf("the probe frame carries no top-level request_id: %s", frame)
	}

	var request map[string]json.RawMessage
	if err := json.Unmarshal(top["request"], &request); err != nil {
		t.Fatalf("the probe request is not JSON: %v", err)
	}
	if string(request["subtype"]) != `"get_usage"` {
		t.Fatalf("the probe subtype = %s, want get_usage", request["subtype"])
	}
	if _, ok := request["request_id"]; ok {
		t.Fatalf("the request_id is nested inside request; the CLI would not echo it: %s", frame)
	}
}

// TestPing_ResolvesFromTheMatchingControlResponse — the probe
// resolves when the answer carries the request_id the probe wrote.
func TestPing_ResolvesFromTheMatchingControlResponse(t *testing.T) {
	sink := newFrameSink()
	d := probeDriver(sink)

	answered := make(chan error, 1)
	go func() { answered <- d.Ping(context.Background()) }()

	var frame string
	select {
	case frame = <-sink.lines:
	case <-time.After(3 * time.Second):
		t.Fatal("the probe wrote no frame")
	}
	var sent struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(frame), &sent); err != nil {
		t.Fatalf("the probe frame is not JSON: %v", err)
	}
	if sent.RequestID == "" {
		t.Fatalf("the probe frame carries no request_id: %s", frame)
	}

	// An answer for a different probe must not resolve this one.
	d.resolveControlResponse(streamEvent{
		Type:     "control_response",
		Response: &controlResponse{Subtype: "success", RequestID: "some-other-probe"},
	})
	select {
	case err := <-answered:
		t.Fatalf("a foreign answer resolved the probe: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	d.resolveControlResponse(streamEvent{
		Type:     "control_response",
		Response: &controlResponse{Subtype: "success", RequestID: sent.RequestID},
	})
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("Ping() = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the probe did not resolve")
	}
}

// TestPing_AnErrorAnswerStillCountsAsAnAnswer — the CLI answering
// with "Unsupported control request subtype" still proves it is
// responsive.
func TestPing_AnErrorAnswerStillCountsAsAnAnswer(t *testing.T) {
	sink := newFrameSink()
	d := probeDriver(sink)

	answered := make(chan error, 1)
	go func() { answered <- d.Ping(context.Background()) }()

	var frame string
	select {
	case frame = <-sink.lines:
	case <-time.After(3 * time.Second):
		t.Fatal("the probe wrote no frame")
	}
	var sent struct {
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal([]byte(frame), &sent)

	d.resolveControlResponse(streamEvent{
		Type: "control_response",
		Response: &controlResponse{
			Subtype:   "error",
			RequestID: sent.RequestID,
			Error:     "Unsupported control request subtype: get_usage",
		},
	})
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("Ping() = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the probe did not resolve")
	}
}

// TestPing_AnUnansweredProbeFails — no answer is the failure the
// daemon acts on.
func TestPing_AnUnansweredProbeFails(t *testing.T) {
	sink := newFrameSink()
	d := probeDriver(sink)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := d.Ping(ctx); err == nil {
		t.Fatal("Ping() = nil, want an error when the CLI stays silent")
	}
}

// TestControlResponse_NeverReachesTheEventChannel — the answer to
// the probe is resolved from the bridge's pending-probe map. It is
// not published to the event channel: the probe has to stay
// answerable when that channel is wedged.
func TestControlResponse_NeverReachesTheEventChannel(t *testing.T) {
	resolved := make(chan streamEvent, 1)
	events := make(chan agent.AgentEvent, 8)
	input := `{"type":"control_response","response":{"subtype":"success","request_id":"probe-1","response":{}}}` + "\n"

	pumpStream(strings.NewReader(input), events, nil, nil, nil,
		"claude", "/tmp", "main", nil,
		withControlResponder(func(ev streamEvent) { resolved <- ev }))

	select {
	case ev := <-resolved:
		if ev.Response == nil || ev.Response.RequestID != "probe-1" {
			t.Fatalf("the resolver saw %+v", ev.Response)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the control_response was not resolved")
	}

	select {
	case ev := <-events:
		t.Fatalf("the control_response reached the event channel: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}
