//go:build windows

// Windows regression tests for the two halves of the cmd.exe story.
//
//  1. A prompt larger than cmd.exe's 8191-character command-line
//     limit must still reach the agent. The prompt travels over the
//     protocol pipe and never becomes an argv element, so the
//     .cmd shim that npm installs is spawned with a short argv and
//     the payload arrives intact over stdin.
//
//  2. The stderr of a Windows console program carries the console
//     code page, not UTF-8. A zh-CN host writes cmd.exe's messages
//     as GBK; the bridge must decode them so the diagnostic reaches
//     the chat readable instead of mojibake.
//
// Both tests drive a real `pi.cmd` — the shape npm installs on
// Windows — through proc.New, so the child really is
// `cmd.exe /d /c pi.cmd`. The shim delegates to this test binary,
// which speaks pi's RPC protocol; no model is called.

package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/codepage"
)

const (
	// shimEnv marks the re-executed test binary as the pi stand-in.
	// Without it the helper skips, so a normal `go test` run does
	// not mistake it for a real test.
	shimEnv = "NIGHTME_PI_SHIM_HELPER"

	// shimOutEnv is the file the helper records what it observed.
	shimOutEnv = "NIGHTME_PI_SHIM_OUT"

	// shimStderrEnv switches the helper into the stderr variant: it
	// writes a code-page-encoded message to stderr and dies.
	shimStderrEnv = "NIGHTME_PI_SHIM_STDERR"
)

// cmdTooLongGBK is what cmd.exe writes for "the command line is too
// long" on a zh-CN host (CP936), captured from a CREATE_NO_WINDOW
// spawn.
var cmdTooLongGBK = []byte{
	0xc3, 0xfc, 0xc1, 0xee, 0xd0, 0xd0, 0xcc, 0xab,
	0xb3, 0xa4, 0xa1, 0xa3, 0x0d, 0x0a,
}

// shimObservation is what the helper records for the parent test.
type shimObservation struct {
	Argv   []string `json:"argv"`
	Prompt string   `json:"prompt"`
}

// TestPiCmdShimHelper is not a test: it is the subprocess body the
// pi.cmd shim runs. It answers the bridge's get_state handshake and,
// in the default mode, one prompt.
//
// Under a normal `go test` run it skips.
func TestPiCmdShimHelper(t *testing.T) {
	if os.Getenv(shimEnv) == "" {
		t.Skip("helper subprocess only")
	}

	out := os.Stdout
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	obs := shimObservation{Argv: shimArgv(os.Args[1:])}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		switch req.Type {
		case "get_state":
			writeShimLine(out, `{"id":"`+req.ID+`","type":"response","command":"get_state","success":true,`+
				`"data":{"model":{"id":"mock-model","name":"Mock Model","provider":"mock","contextWindow":8192},`+
				`"sessionId":"mock-session-1","sessionName":"mock"}}`)
		case "prompt":
			obs.Prompt = req.Message
			writeShimObservation(obs)
			if os.Getenv(shimStderrEnv) != "" {
				// The stderr variant: the child dies mid-turn, the
				// way a console program that refuses its
				// arguments does.
				_, _ = os.Stderr.Write(cmdTooLongGBK)
				os.Exit(1)
			}
			writeShimLine(out, `{"id":"`+req.ID+`","type":"response","command":"prompt","success":true}`)
			writeShimLine(out, `{"type":"agent_start"}`)
			writeShimLine(out, `{"type":"message_start","message":{"role":"assistant"}}`)
			writeShimLine(out, `{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0,"partial":{"type":"text","text":""}}}`)
			writeShimLine(out, `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"shim reply"}}`)
			writeShimLine(out, `{"type":"message_update","assistantMessageEvent":{"type":"text_end","contentIndex":0,"content":"shim reply"}}`)
			writeShimLine(out, `{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"shim reply"}],`+
				`"usage":{"input":1,"output":1,"totalTokens":2},"model":"mock-model","provider":"mock"}}`)
			writeShimLine(out, `{"type":"agent_end","willRetry":false}`)
			writeShimLine(out, `{"type":"agent_settled"}`)
		default:
			writeShimLine(out, `{"id":"`+req.ID+`","type":"response","command":"`+req.Type+`","success":true}`)
		}
	}
}

// writeShimLine writes one JSONL frame to the bridge.
func writeShimLine(w *os.File, line string) {
	_, _ = w.WriteString(line + "\n")
}

// shimArgv returns the arguments the shim was invoked with —
// everything after the "--" separator that hands pi's argv over.
func shimArgv(args []string) []string {
	for i, a := range args {
		if a == "--" {
			return args[i+1:]
		}
	}
	return nil
}

// writeShimObservation records what the shim saw.
func writeShimObservation(obs shimObservation) {
	path := os.Getenv(shimOutEnv)
	if path == "" {
		return
	}
	blob, err := json.Marshal(obs)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, blob, 0o644)
}

// writePiCmdShim writes the `pi.cmd` npm installs on Windows into
// dir and returns its path. The shim delegates to this test binary,
// so the spawned child is `cmd.exe /d /c pi.cmd` — the same shape as
// a real npm-installed agent.
//
// The pi argv is handed over after a "--" separator: the Go test
// binary rejects unknown flags, and "--" is where the flag parser
// stops.
func writePiCmdShim(t *testing.T, dir string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	path := filepath.Join(dir, "pi.cmd")
	body := "@echo off\r\n\"" + self + "\" -test.run=TestPiCmdShimHelper -- %*\r\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write pi.cmd: %v", err)
	}
	return path
}

// TestRunOnce_LongPromptOverPipe_Windows: a prompt larger than
// cmd.exe's command-line limit reaches the agent intact through a
// .cmd shim, and never appears in the child's argv.
func TestRunOnce_LongPromptOverPipe_Windows(t *testing.T) {
	dir := t.TempDir()
	shim := writePiCmdShim(t, dir)
	obsPath := filepath.Join(dir, "observation.json")

	t.Setenv(shimEnv, "1")
	t.Setenv(shimOutEnv, obsPath)

	// The payload mixes bulk ASCII with the characters a Windows
	// command line mangles: quotes, percent signs, ampersands,
	// carets, pipes, newlines and CJK text.
	payload := strings.Repeat("x", 9000) +
		"\r\n中文 \"quoted\" & | ^ %PATH%\r\nend"
	if len(payload) <= 8191 {
		t.Fatalf("payload is %d bytes; it must exceed cmd.exe's 8191-character limit", len(payload))
	}

	s := NewStarter("pi", shim, nil)
	if err := s.Detect(); err != nil {
		t.Fatalf("Detect: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := s.RunOnce(ctx, agent.StartConfig{Workspace: dir},
		[]agent.ContentBlock{{Type: agent.ContentText, Text: payload}})
	if err != nil {
		t.Fatalf("RunOnce with a %d-byte prompt: %v", len(payload), err)
	}
	if res.Text == "" {
		t.Fatal("RunResult.Text empty; the oversized prompt did not reach the agent")
	}

	obs := readShimObservation(t, obsPath)
	if obs.Prompt != payload {
		t.Errorf("shim received %d bytes, want the %d-byte prompt intact", len(obs.Prompt), len(payload))
	}
	for _, arg := range obs.Argv {
		if strings.Contains(arg, "中文") {
			t.Errorf("prompt leaked into the child's argv: %q", arg)
		}
	}
	if joined := strings.Join(obs.Argv, " "); len(joined) >= 8191 {
		t.Errorf("child argv is %d bytes; it must stay under cmd.exe's limit", len(joined))
	}
}

// TestBridgeStderr_CP936IsDecoded_Windows: the stderr of a Windows
// console program reaches the bridge diagnostic as readable text.
//
// The decode is gated on the host's ANSI code page, so the
// readability assertion only holds on a zh-CN host; the plumbing
// assertion holds everywhere.
func TestBridgeStderr_CP936IsDecoded_Windows(t *testing.T) {
	dir := t.TempDir()
	shim := writePiCmdShim(t, dir)

	t.Setenv(shimEnv, "1")
	t.Setenv(shimOutEnv, filepath.Join(dir, "observation.json"))
	t.Setenv(shimStderrEnv, "1")

	s := NewStarter("pi", shim, nil)
	if err := s.Detect(); err != nil {
		t.Fatalf("Detect: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := s.Start(ctx, agent.StartConfig{Workspace: dir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close()

	// The shim dies when it receives the prompt, so the bridge
	// reports the child's death while the turn is in flight.
	_ = sess.SendBlocks(ctx, []agent.ContentBlock{{Type: agent.ContentText, Text: "hello"}})

	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatal("events channel closed before the bridge reported the child's death")
			}
			if ev.Kind != agent.EventAgentError || ev.Diagnostic == nil {
				continue
			}
			tail := ev.Diagnostic.StderrTail
			if tail == "" {
				t.Fatal("Diagnostic.StderrTail is empty; the child's stderr never reached the bridge")
			}
			if codepage.LocalACP() == codepage.ChineseACP {
				if !strings.Contains(tail, "命令行太长。") {
					t.Errorf("StderrTail = %q, want the decoded CP936 message", tail)
				}
			}
			return
		case <-deadline:
			t.Fatal("timeout waiting for the bridge to report the child's death")
		}
	}
}

// readShimObservation reads what the shim recorded.
func readShimObservation(t *testing.T, path string) shimObservation {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shim observation: %v", err)
	}
	var obs shimObservation
	if err := json.Unmarshal(blob, &obs); err != nil {
		t.Fatalf("decode shim observation: %v", err)
	}
	return obs
}
