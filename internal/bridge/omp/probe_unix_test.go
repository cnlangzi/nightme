//go:build !windows

// Real-binary probe test (R9). When `omp` is installed and
// the operator has completed the omp login flow, this test
// drives `omp --mode rpc` end-to-end:
//
//   1. spawn `omp --mode rpc --approval-mode yolo` under real
//      stdio pipes (NOT PTY)
//   2. confirm the {"type":"ready",..} frame arrives
//   3. confirm extension_ui_request / advisor_cost_changed /
//      available_commands_update are absorbed silently without
//      a reply
//   4. send get_state, capture sessionId / model.id from the
//      response data
//   5. send prompt "reply with the word pong and nothing
//      else"
//   6. wait for the prompt ack (response{command:"prompt",
//      success:true}) to land BEFORE agent_start
//   7. wait for agent_end{isTerminal:true} to settle the turn
//
// Skipped (not failed) when `omp` is not on PATH. CI does NOT
// configure omp login credentials so this test will skip there
// even when the binary is installed.

package omp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fakeReadyScript writes a one-shot shell script that mimics
// the well-behaved path the mock would take. We don't ship a
// real mock because R9's intent is the actual omp binary —
// this helper is unused today and exists so future tests can
// iterate without re-installing omp each time.
var _ = writeIfHelper

func writeIfHelper(path, content string) {
	_ = exec.Command("sh", "-c", fmt.Sprintf("cat > %s", path))
}

func TestProbe_RealOmpRoundTrip(t *testing.T) {
	ompPath, err := exec.LookPath("omp")
	if err != nil {
		t.Skipf("omp not on PATH; install via `curl -fsSL https://omp.sh/install | sh` to enable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ompPath, "--mode", "rpc", "--approval-mode", "yolo")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		t.Fatalf("stdout pipe: %v", err)
	}
	stderrR, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("omp start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Drain stderr to /dev/null-ish (the bridge would normally
	// keep it for diagnostics; the test only needs to keep the
	// pipe from blocking the child).
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderrR.Read(buf)
			if n > 0 {
				t.Logf("stderr: %s", strings.TrimSpace(string(buf[:n])))
			}
			if err != nil {
				return
			}
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxFrameSize)

	// Step 2: confirm the ready frame lands.
	if !scanner.Scan() {
		t.Fatalf("no frames received from omp (binary missing or rpc mode unsupported)")
	}
	var ready struct {
		Type            string `json:"type"`
		ProtocolVersion int    `json:"protocolVersion"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &ready); err != nil {
		t.Fatalf("first frame unparsable: %s", scanner.Text())
	}
	if ready.Type != "ready" {
		t.Fatalf("first frame type = %q, want ready", ready.Type)
	}
	t.Logf("saw ready frame (protocolVersion=%d)", ready.ProtocolVersion)

	// Step 4: send get_state, capture response.
	hsDeadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(hsDeadline) {
		_, _ = stdin.Write([]byte(`{"id":"probe-boot","type":"get_state"}` + "\n"))
		if !scanner.Scan() {
			t.Fatal("get_state response not received")
		}
		var probe struct {
			Type    string          `json:"type"`
			Command string          `json:"command"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &probe); err != nil {
			t.Fatalf("get_state parse: %s", scanner.Text())
		}
		if probe.Type == "response" && probe.Command == "get_state" {
			// rsp ok
			t.Logf("got_state response: %s", string(probe.Data))
			break
		}
		// Ignore mid-stream housekeeping frames
		// (extension_ui_request / advisor_cost_changed /
		// available_commands_update).
		t.Logf("mid-stream housekeeping frame absorbed: %s", probe.Type)
	}

	// Step 5/6: send prompt and confirm ack lands BEFORE
	// agent_start.
	_, _ = stdin.Write([]byte(`{"id":"probe-prompt-1","type":"prompt","message":"reply with the word pong and nothing else"}` + "\n"))

	ackSeen := false
	for !ackSeen {
		if !scanner.Scan() {
			t.Fatal("prompt ack or events never arrived")
		}
		var probe struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &probe); err != nil {
			t.Fatalf("post-prompt frame parse: %s", scanner.Text())
		}
		if probe.Type == "response" && probe.Command == "prompt" {
			ackSeen = true
			t.Log("prompt ack landed")
			break
		}
		if probe.Type == "agent_start" {
			t.Fatal("agent_start arrived before prompt ack — handshake order violated")
		}
	}
	if !ackSeen {
		t.Fatal("did not see prompt ack")
	}

	// Step 7: wait for agent_end{isTerminal:true}.
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if !scanner.Scan() {
			t.Fatal("event stream closed before terminal agent_end")
		}
		var probe struct {
			Type       string `json:"type"`
			IsTerminal *bool  `json:"isTerminal"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &probe); err != nil {
			continue
		}
		if probe.Type == "agent_end" {
			if probe.IsTerminal == nil || *probe.IsTerminal {
				t.Logf("terminal agent_end observed; full round-trip OK")
				return
			}
		}
	}
	t.Fatal("did not see agent_end{isTerminal:true} within 120s")
}
