//go:build !windows

// ACP handshake probe against the real copilot binary.
//
// Sends the standard ACP JSON-RPC 2.0 handshake
// (initialize → session/new) over stdio and verifies Copilot
// responds with a valid protocolVersion + sessionId. Skipped
// when no `copilot` binary on PATH.
//
// This is a wire-level probe — it does NOT use the nightme
// ACP bridge, just `copilot --acp --stdio` directly, so we can
// observe Copilot's actual protocol surface independent of
// the bridge layer. If this test breaks, the bridge's ACP path
// will break too.
package copilot

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestACP_Handshake_RealBinary spawns copilot under PTY-less
// stdio pipes (exec.Cmd), sends initialize + session/new as
// NDJSON, and verifies the responses. Uses os.Pipe for stdin
// so the process keeps reading until we close.
//
// Verifies:
//  1. initialize responds with protocolVersion=1 + agentInfo
//  2. session/new returns a non-empty sessionId
//  3. notifications/initialized is accepted (no error)
func TestACP_Handshake_RealBinary(t *testing.T) {
	if _, err := exec.LookPath("copilot"); err != nil {
		t.Skipf("copilot binary not on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "copilot", "--allow-all-tools", "--acp", "--stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	defer func() {
		stdin.Close()
		_ = cmd.Wait()
	}()

	// Drain stderr to keep the pipe from blocking.
	go io.Copy(io.Discard, stderr)

	// 1. initialize
	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": 1,
			"clientCapabilities": map[string]any{},
			"clientInfo": map[string]any{
				"name":    "nightme-probe",
				"version": "0.0.1",
			},
		},
	}
	if err := writeNDJSON(stdin, initReq); err != nil {
		t.Fatalf("write initialize: %v", err)
	}

	initResp, err := readJSONRPC(stdout, 30*time.Second)
	if err != nil {
		t.Fatalf("read initialize response: %v", err)
	}
	if initResp.Error != nil {
		t.Fatalf("initialize returned error: %+v", initResp.Error)
	}
	if initResp.Result == nil {
		t.Fatal("initialize: result is nil")
	}
	var initResult struct {
		ProtocolVersion int    `json:"protocolVersion"`
		AgentInfo       struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"agentInfo"`
	}
	if err := json.Unmarshal(initResp.Result, &initResult); err != nil {
		t.Fatalf("unmarshal initialize result: %v", err)
	}
	if initResult.ProtocolVersion != 1 {
		t.Errorf("protocolVersion = %d, want 1", initResult.ProtocolVersion)
	}
	if initResult.AgentInfo.Name == "" {
		t.Errorf("agentInfo.name empty")
	}
	t.Logf("copilot ACP initialize OK: protocolVersion=%d agentInfo=%s/%s",
		initResult.ProtocolVersion,
		initResult.AgentInfo.Name, initResult.AgentInfo.Version)

	// 2. notifications/initialized (notification — no id, no
	// response expected)
	initialized := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params":  map[string]any{},
	}
	if err := writeNDJSON(stdin, initialized); err != nil {
		t.Fatalf("write initialized: %v", err)
	}

	// 3. session/new
	sessionReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "session/new",
		"params": map[string]any{
			"cwd":        t.TempDir(),
			"mcpServers": []any{},
		},
	}
	if err := writeNDJSON(stdin, sessionReq); err != nil {
		t.Fatalf("write session/new: %v", err)
	}
	sessionResp, err := readJSONRPC(stdout, 30*time.Second)
	if err != nil {
		t.Fatalf("read session/new response: %v", err)
	}
	if sessionResp.Error != nil {
		t.Fatalf("session/new returned error: %+v", sessionResp.Error)
	}
	var sessionResult struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(sessionResp.Result, &sessionResult); err != nil {
		t.Fatalf("unmarshal session/new result: %v", err)
	}
	if sessionResult.SessionID == "" {
		t.Errorf("session/new returned empty sessionId")
	}
	t.Logf("copilot ACP session/new OK: sessionId=%s", sessionResult.SessionID)

	// NOTE: We intentionally stop at session/new here. The
	// end-to-end prompt flow is exercised by
	// TestPrintMode_RealBinary_RunsAndReturnsText (covers the
	// wire path that RunOnce uses) and by manual `nightme test
	// --agent copilot` smoke tests in production. Driving
	// session/prompt through a hand-rolled NDJSON probe is
	// brittle (the model call takes 30+s and a single
	// notification structure difference would make this test
	// flaky), so we don't gate the bridge contract on it.
}

// writeNDJSON writes one JSON object + \n.
func writeNDJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		return err
	}
	return nil
}

// jsonRPCResponse is the subset of a JSON-RPC 2.0 response we
// care about for the handshake probe.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data,omitempty"`
	} `json:"error,omitempty"`
	Method string          `json:"method,omitempty"` // for notifications
	Params json.RawMessage `json:"params,omitempty"`
}

// readJSONRPC reads one JSON object (terminated by \n) and
// returns it. Notifications (no id, no result/error) are
// skipped — we only return real responses.
func readJSONRPC(r io.Reader, timeout time.Duration) (*jsonRPCResponse, error) {
	type result struct {
		resp *jsonRPCResponse
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		// Allow up to 1 MiB per line — should be enough for any
		// handshake response.
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var resp jsonRPCResponse
			if err := json.Unmarshal(line, &resp); err != nil {
				ch <- result{nil, fmt.Errorf("unmarshal: %w (line=%q)", err, string(line))}
				return
			}
			// Skip notifications (method present, no id).
			if resp.Method != "" {
				continue
			}
			ch <- result{&resp, nil}
			return
		}
		if err := scanner.Err(); err != nil {
			ch <- result{nil, err}
			return
		}
		ch <- result{nil, io.EOF}
	}()
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout after %s", timeout)
	}
}

// _ = strings.Contains keeps the import in case future
// assertions need it (test compiles regardless).
var _ = strings.Contains