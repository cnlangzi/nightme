package codex

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// userInputAnswersFrame models the JSON shape on the wire for
// item/tool/requestUserInput responses:
//
//   {"jsonrpc":"2.0","id":"...","result":{"answers":{"q1":{"answers":["A"]}}}}
type userInputAnswersFrame struct {
	Result struct {
		Answers map[string]struct {
			Answers []string `json:"answers"`
		} `json:"answers"`
	} `json:"result"`
}

// ─── helpers ───

// capturedRPCWrites records every Write the bridge performs on its
// rpcClient's write side. Used to assert that the bridge sends the
// expected JSON-RPC response to the child.
type capturedRPCWrites struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturedRPCWrites) record(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, string(b))
}

func (c *capturedRPCWrites) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

func (c *capturedRPCWrites) findFrame(substr string) string {
	for _, line := range c.all() {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}

// capturedRPC is a tiny io.ReadWriter that records every Write.
// Read returns EOF immediately so nothing in the test path tries
// to consume bytes from it.
type capturedRPC struct {
	captured *capturedRPCWrites
}

func (c *capturedRPC) Write(p []byte) (int, error) {
	c.captured.record(p)
	return len(p), nil
}

func (c *capturedRPC) Read([]byte) (int, error) { return 0, nil }

// permTestRig wires a *session with a captured RPC and an in-memory
// events channel. Compression: permissionTimeout is shrunk to 200ms
// for the duration of the test so the timeout-defaults-to-decline
// test completes quickly.
type permTestRig struct {
	session *session
	writes  *capturedRPCWrites
}

func newPermTestRig(t *testing.T) *permTestRig {
	t.Helper()

	writes := &capturedRPCWrites{}
	rpc := &capturedRPC{captured: writes}

	oldTimeout := permissionTimeout
	permissionTimeout = 200 * time.Millisecond
	t.Cleanup(func() { permissionTimeout = oldTimeout })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	s := &session{
		rpc:              newRPCClient(rpc, nil, nil),
		pendingApprovals: make(map[string]chan string),
		ctx:              ctx,
		cancel:           cancel,
		events:           make(chan agent.AgentEvent, 8),
	}
	// deliver must call back into the same session; assign after s is
	// built so the closure can capture it by reference.
	s.deliver = func(ev agent.AgentEvent) agent.AgentEvent {
		select {
		case s.events <- ev:
		default:
		}
		return ev
	}

	return &permTestRig{session: s, writes: writes}
}

// respond manually pushes a string into the pending approval channel
// for the given request id. Mirrors what Agent.SendPermission does
// in production but stays in-package so we don't need the full Agent.
func (r *permTestRig) respond(t *testing.T, requestID, resp string) {
	t.Helper()
	r.session.pendingMu.Lock()
	ch, ok := r.session.pendingApprovals[requestID]
	r.session.pendingMu.Unlock()
	if !ok {
		t.Fatalf("no pending approval for %q", requestID)
	}
	ch <- resp
}

// waitForFrame polls the captured writes for a frame containing
// substr. Fails the test if not seen before the timeout.
func (r *permTestRig) waitForFrame(t *testing.T, substr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if frame := r.writes.findFrame(substr); frame != "" {
			return frame
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no frame containing %q within %s. all frames:\n%s",
		substr, timeout, strings.Join(r.writes.all(), "\n"))
	return ""
}

// ─── tests ───

func TestPerm_CommandApprovalAccept(t *testing.T) {
	rig := newPermTestRig(t)

	rig.session.handleApprovalRequest(
		"item/commandExecution/requestApproval",
		json.RawMessage(`"req-1"`),
		json.RawMessage(`{"command":"ls -la","cwd":"/tmp"}`),
	)
	rig.respond(t, "req-1", "accept")

	frame := rig.waitForFrame(t, `"decision":"accept"`, 1*time.Second)

	// Verify id echoes back.
	var env rpcResponseEnvelope
	if err := json.Unmarshal([]byte(frame), &env); err != nil {
		t.Fatalf("frame is not an rpcResponseEnvelope: %v\n%s", err, frame)
	}
	if string(bytesOrZero(env.ID)) != `"req-1"` {
		t.Errorf("response id = %q, want %q", string(env.ID), `"req-1"`)
	}
}

func TestPerm_CommandApprovalDecline(t *testing.T) {
	rig := newPermTestRig(t)

	rig.session.handleApprovalRequest(
		"item/commandExecution/requestApproval",
		json.RawMessage(`"req-2"`),
		json.RawMessage(`{"command":"rm -rf /","cwd":"/"}`),
	)
	rig.respond(t, "req-2", "decline")

	rig.waitForFrame(t, `"decision":"decline"`, 1*time.Second)
}

func TestPerm_FileChangeApprovalUsesPatch(t *testing.T) {
	rig := newPermTestRig(t)

	// The decision response should still fire; the tool name is
	// surfaced in EventAgentPermission's Action field. We can't
	// assert the channel here (it goes to deliver / events), so
	// only assert the response.
	rig.session.handleApprovalRequest(
		"item/fileChange/requestApproval",
		json.RawMessage(`"req-3"`),
		json.RawMessage(`{"reason":"will modify /etc/hosts"}`),
	)
	rig.respond(t, "req-3", "decline")

	rig.waitForFrame(t, `"decision":"decline"`, 1*time.Second)
}

func TestPerm_ApprovalTimeoutDefaultsToDecline(t *testing.T) {
	rig := newPermTestRig(t)

	rig.session.handleApprovalRequest(
		"item/commandExecution/requestApproval",
		json.RawMessage(`"req-timeout"`),
		json.RawMessage(`{"command":"sleep 100"}`),
	)

	// permissionTimeout is 200ms in test; wait 350ms to be safe.
	time.Sleep(350 * time.Millisecond)

	rig.waitForFrame(t, `"decision":"decline"`, 500*time.Millisecond)
}

func TestPerm_UnknownServerRequestGetsMethodNotFound(t *testing.T) {
	rig := newPermTestRig(t)

	rig.session.handleServerRequest(
		"item/doesNotExist",
		json.RawMessage(`"req-9"`),
		json.RawMessage(`{}`),
	)

	rig.waitForFrame(t, `-32601`, 500*time.Millisecond)
}

func TestPerm_RequestUserInputAnswers(t *testing.T) {
	rig := newPermTestRig(t)

	params := json.RawMessage(`{
		"questions":[
			{"id":"q1","header":"Pick one","question":"which?","options":[{"label":"A"},{"label":"B"}],"multiSelect":false},
			{"id":"q2","header":"Pick many","question":"which many?","options":[{"label":"X"},{"label":"Y"}],"multiSelect":true}
		]
	}`)
	rig.session.handleRequestUserInput(
		json.RawMessage(`"req-q"`),
		params,
	)
	rig.respond(t, "req-q", "q1:A|q2:X,Y")

	frame := rig.waitForFrame(t, `"answers"`, 1*time.Second)

	var resp userInputAnswersFrame
	if err := json.Unmarshal([]byte(frame), &resp); err != nil {
		t.Fatalf("frame not parseable: %v\n%s", err, frame)
	}
	if got := resp.Result.Answers["q1"].Answers; len(got) != 1 || got[0] != "A" {
		t.Errorf("q1 answers = %v, want [A]", got)
	}
	if got := resp.Result.Answers["q2"].Answers; len(got) != 2 || got[0] != "X" || got[1] != "Y" {
		t.Errorf("q2 answers = %v, want [X Y]", got)
	}
}

func TestPerm_RequestUserInputFallsBackOnGarbage(t *testing.T) {
	rig := newPermTestRig(t)

	params := json.RawMessage(`{
		"questions":[
			{"id":"q1","header":"","question":"?","options":[{"label":"A"},{"label":"B"}],"multiSelect":false}
		]
	}`)
	rig.session.handleRequestUserInput(json.RawMessage(`"req-g"`), params)
	// Garbage response — should fall back to first option.
	rig.respond(t, "req-g", "totally not parseable")

	frame := rig.waitForFrame(t, `"answers"`, 1*time.Second)
	var resp userInputAnswersFrame
	if err := json.Unmarshal([]byte(frame), &resp); err != nil {
		t.Fatalf("frame not parseable: %v\n%s", err, frame)
	}
	got := resp.Result.Answers["q1"].Answers
	if len(got) != 1 || got[0] != "A" {
		t.Errorf("fallback answers = %v, want [A]", got)
	}
}

func TestPerm_DynamicToolCallReturnsNotAvailable(t *testing.T) {
	rig := newPermTestRig(t)

	rig.session.handleDynamicToolCall(json.RawMessage(`"req-tool"`), json.RawMessage(`{}`))

	rig.waitForFrame(t, "tool not available", 500*time.Millisecond)
}

func TestPerm_EmptyResponseDefaultsToDecline(t *testing.T) {
	rig := newPermTestRig(t)

	rig.session.handleApprovalRequest(
		"item/commandExecution/requestApproval",
		json.RawMessage(`"req-empty"`),
		json.RawMessage(`{"command":"ls"}`),
	)
	rig.respond(t, "req-empty", "")

	// Empty → decline.
	rig.waitForFrame(t, `"decision":"decline"`, 1*time.Second)
}

func TestParseRequestUserInputResponse_RoundTrip(t *testing.T) {
	qs := []appServerRequestUserInputQuestion{
		{ID: "q1", Options: []appServerRequestUserInputOption{
			{Label: "A"}, {Label: "B"},
		}},
	}
	got := parseRequestUserInputResponse("q1:A", qs)
	if v := got["q1"].Answers; len(v) != 1 || v[0] != "A" {
		t.Errorf("round-trip = %v, want [A]", v)
	}
}

func TestParseRequestUserInputResponse_FillsMissing(t *testing.T) {
	qs := []appServerRequestUserInputQuestion{
		{ID: "q1", Options: []appServerRequestUserInputOption{
			{Label: "A"}, {Label: "B"},
		}},
		{ID: "q2", Options: []appServerRequestUserInputOption{
			{Label: "X"},
		}},
	}
	// Missing q2 — should fall back to its first option (X).
	got := parseRequestUserInputResponse("q1:B", qs)
	if v := got["q2"].Answers; len(v) != 1 || v[0] != "X" {
		t.Errorf("q2 fallback = %v, want [X]", v)
	}
}

// ─── helpers ───

func bytesOrZero(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}
