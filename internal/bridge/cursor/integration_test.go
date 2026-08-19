package cursor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// pipeSession runs `agent acp` via stdin/stdout pipes (no PTY).
// All stdout reading goes through a single goroutine that
// dispatches RPC responses to pending callers and server
// notifications to the events channel.
type pipeSession struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	next int
	stdin *bufio.Writer

	// RPC response dispatch: keyed by request ID string.
	pendingMu sync.Mutex
	pending   map[string]chan rpcMsg

	// Server-initiated events (text, tool, task, etc.).
	events chan agent.AgentEvent

	// Session ID from session/new handshake.
	sessionID string

	cancel context.CancelFunc
}

func startPipeSession(t *testing.T, workspace string) *pipeSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	cmd := exec.CommandContext(ctx, "agent", "acp")
	cmd.Dir = workspace
	stdinW, _ := cmd.StdinPipe()
	stdoutR, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	s := &pipeSession{
		cmd:     cmd,
		stdin:   bufio.NewWriter(stdinW),
		pending: make(map[string]chan rpcMsg),
		events:  make(chan agent.AgentEvent, 512),
		cancel:  cancel,
	}
	// Single read loop — all stdout goes through here.
	go s.readLoop(bufio.NewReader(stdoutR))
	t.Cleanup(func() {
		cancel()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return s
}

// readLoop is the ONLY goroutine reading stdout. It dispatches:
//   - Messages with an ID → pending RPC response channel
//   - Messages with a Method → server notification (ACP events, extensions)
func (s *pipeSession) readLoop(r *bufio.Reader) {
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var msg rpcMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		// JSON-RPC response: has ID but NO method.
		// Messages with BOTH id AND method are server→client method calls
		// (e.g. cursor/update_todos), not responses.
		if msg.ID != nil && msg.Method == "" {
			idStr := string(msg.ID)
			s.pendingMu.Lock()
			ch, ok := s.pending[idStr]
			if ok {
				delete(s.pending, idStr)
			}
			s.pendingMu.Unlock()
			if ok {
				ch <- msg
			}
			continue
		}
		// Server-initiated method call or notification.
		if msg.Method != "" {
			s.dispatch(msg)
		}
	}
}

func (s *pipeSession) nextID() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return json.RawMessage(fmt.Sprintf("%d", s.next))
}

func (s *pipeSession) call(method string, params any) (rpcMsg, error) {
	id := s.nextID()
	ch := make(chan rpcMsg, 1)
	s.pendingMu.Lock()
	s.pending[string(id)] = ch
	s.pendingMu.Unlock()

	msg := rpcMsg{JSONRPC: "2.0", ID: id, Method: method}
	if params != nil {
		b, _ := json.Marshal(params)
		msg.Params = b
	}
	data, _ := json.Marshal(msg)
	s.mu.Lock()
	s.stdin.Write(data)
	s.stdin.WriteByte('\n')
	s.stdin.Flush()
	s.mu.Unlock()

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(30 * time.Second):
		s.pendingMu.Lock()
		delete(s.pending, string(id))
		s.pendingMu.Unlock()
		return rpcMsg{}, fmt.Errorf("call %s timeout", method)
	}
}

func (s *pipeSession) notify(method string, params any) {
	msg := rpcMsg{JSONRPC: "2.0", Method: method}
	if params != nil {
		b, _ := json.Marshal(params)
		msg.Params = b
	}
	data, _ := json.Marshal(msg)
	s.mu.Lock()
	s.stdin.Write(data)
	s.stdin.WriteByte('\n')
	s.stdin.Flush()
	s.mu.Unlock()
}

func (s *pipeSession) handshake(t *testing.T, workspace string) string {
	t.Helper()
	resp, err := s.call("initialize", map[string]any{
		"protocolVersion": 1,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "nightme-test", "version": "1.0"},
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("initialize error: %v", resp.Error.Message)
	}
	s.notify("initialized", nil)

	resp, err = s.call("session/new", map[string]any{
		"cwd":        workspace,
		"mcpServers": []any{},
	})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("session/new error: %v", resp.Error.Message)
	}
	var r struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(resp.Result, &r)
	s.sessionID = r.SessionID
	t.Logf("session: %s", r.SessionID)
	return r.SessionID
}

func (s *pipeSession) prompt(text string) {
	// Fire-and-forget: send prompt but don't block waiting for response.
	// The response arrives after the turn ends; the readLoop dispatches
	// all intermediate events (text, tool, task) to s.events.
	id := s.nextID()
	ch := make(chan rpcMsg, 1)
	s.pendingMu.Lock()
	s.pending[string(id)] = ch
	s.pendingMu.Unlock()

	msg := rpcMsg{JSONRPC: "2.0", ID: id, Method: "session/prompt"}
	b, _ := json.Marshal(map[string]any{
		"sessionId": s.sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": text}},
	})
	msg.Params = b
	data, _ := json.Marshal(msg)
	s.mu.Lock()
	s.stdin.Write(data)
	s.stdin.WriteByte('\n')
	s.stdin.Flush()
	s.mu.Unlock()

	// Drain response in background — when prompt response arrives,
	// the turn is done. Emit EventAgentDone so the test can exit.
	go func() {
		select {
		case resp := <-ch:
			if resp.Error == nil {
				s.events <- agent.AgentEvent{
					Kind: agent.EventAgentResult,
					Result: &agent.AgentResultEvent{
						Text: "turn complete",
					},
				}
				s.events <- agent.AgentEvent{
					Kind: agent.EventAgentDone,
					Done: &agent.AgentDoneEvent{ExitCode: 0},
				}
			}
		case <-time.After(120 * time.Second):
		}
	}()
}

// dispatch maps server methods to AgentEvents on s.events.
// For method calls that carry an id (server→client RPC), we respond
// after processing so the server doesn't block.
func (s *pipeSession) dispatch(msg rpcMsg) {
	switch msg.Method {
	case "session/update":
		s.dispatchSessionUpdate(msg.Params)
	case "session/request_permission", "permission_request":
		s.dispatchPermission(msg.Params)
	case "tool_start":
		s.dispatchToolEvent(msg.Params, true)
	case "tool_end":
		s.dispatchToolEvent(msg.Params, false)
	case "session_end":
		s.events <- agent.AgentEvent{Kind: agent.EventAgentDone, Done: &agent.AgentDoneEvent{ExitCode: 0}}
	case "cursor/update_todos":
		s.dispatchUpdateTodos(msg.Params)
		s.respondIfCalled(msg)
	case "cursor/create_plan":
		s.dispatchCreatePlan(msg.Params)
		s.respondOK(msg)
	case "cursor/task":
		s.events <- agent.AgentEvent{
			Kind:    agent.EventAgentToolEnd,
			ToolEnd: &agent.AgentToolEndEvent{Name: "cursor/task"},
		}
		s.respondIfCalled(msg)
	case "cursor/ask_question":
		s.dispatchAskQuestion(msg.Params)
		s.respondIfCalled(msg)
	case "cursor/generate_image":
		s.dispatchGenerateImage(msg.Params)
		s.respondIfCalled(msg)
	}
}

// respondIfCalled sends an empty OK response if the message had an id.
func (s *pipeSession) respondIfCalled(msg rpcMsg) {
	if msg.ID == nil {
		return
	}
	resp := rpcMsg{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage(`{}`)}
	data, _ := json.Marshal(resp)
	s.mu.Lock()
	s.stdin.Write(data)
	s.stdin.WriteByte('\n')
	s.stdin.Flush()
	s.mu.Unlock()
}

// respondOK sends an approved response for blocking methods.
func (s *pipeSession) respondOK(msg rpcMsg) {
	if msg.ID == nil {
		return
	}
	resp := rpcMsg{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage(`{"approved":true}`)}
	data, _ := json.Marshal(resp)
	s.mu.Lock()
	s.stdin.Write(data)
	s.stdin.WriteByte('\n')
	s.stdin.Flush()
	s.mu.Unlock()
}

func (s *pipeSession) dispatchSessionUpdate(raw json.RawMessage) {
	var w struct {
		Update json.RawMessage `json:"update"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return
	}
	var u struct {
		SessionUpdate string          `json:"sessionUpdate"`
		Type          string          `json:"type"`
		Content       json.RawMessage `json:"content"`
	}
	json.Unmarshal(w.Update, &u)
	kind := u.SessionUpdate
	if kind == "" {
		kind = u.Type
	}
	switch kind {
	case "agent_message_chunk", "message_chunk", "agent_thought_chunk":
		var c struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(u.Content, &c) == nil && c.Text != "" {
			s.events <- agent.AgentEvent{Kind: agent.EventAgentText, Text: c.Text}
		}
	case "tool_call":
		s.dispatchToolEvent(w.Update, true)
	case "tool_call_update":
		s.dispatchToolEvent(w.Update, false)
	}
}

func (s *pipeSession) dispatchToolEvent(raw json.RawMessage, isStart bool) {
	var e struct {
		ID   string `json:"toolCallId"`
		Name string `json:"title"`
	}
	json.Unmarshal(raw, &e)
	if isStart {
		s.events <- agent.AgentEvent{Kind: agent.EventAgentToolStart, ToolStart: &agent.AgentToolStartEvent{ID: e.ID, Name: e.Name}}
	} else {
		s.events <- agent.AgentEvent{Kind: agent.EventAgentToolEnd, ToolEnd: &agent.AgentToolEndEvent{ID: e.ID, Name: e.Name}}
	}
}

func (s *pipeSession) dispatchPermission(raw json.RawMessage) {
	var p struct {
		Tool    string `json:"tool"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
		} `json:"options"`
	}
	json.Unmarshal(raw, &p)
	opts := make([]string, 0, len(p.Options))
	for _, o := range p.Options {
		if o.OptionID != "" {
			opts = append(opts, o.OptionID)
		} else {
			opts = append(opts, o.Name)
		}
	}
	s.events <- agent.AgentEvent{
		Kind: agent.EventAgentPermission,
		Permission: &agent.AgentPermissionRequest{
			Tool: p.Tool, Options: opts,
		},
	}
}

func (s *pipeSession) dispatchUpdateTodos(raw json.RawMessage) {
	var p struct {
		Todos []cursorTodo `json:"todos"`
	}
	json.Unmarshal(raw, &p)
	items := make([]agent.AgentTaskItem, 0, len(p.Todos))
	for _, t := range p.Todos {
		items = append(items, agent.AgentTaskItem{ID: t.ID, Subject: t.Content, Status: cursorStatusToAgent(t.Status)})
	}
	s.events <- agent.AgentEvent{Kind: agent.EventAgentTaskUpdate, TaskList: &agent.AgentTaskListEvent{Items: items}}
}

func (s *pipeSession) dispatchCreatePlan(raw json.RawMessage) {
	var p struct {
		Todos []cursorTodo `json:"todos"`
	}
	json.Unmarshal(raw, &p)
	items := make([]agent.AgentTaskItem, 0, len(p.Todos))
	for _, t := range p.Todos {
		items = append(items, agent.AgentTaskItem{ID: t.ID, Subject: t.Content, Status: cursorStatusToAgent(t.Status)})
	}
	s.events <- agent.AgentEvent{Kind: agent.EventAgentTaskCreate, TaskList: &agent.AgentTaskListEvent{Items: items}}
}

func (s *pipeSession) dispatchAskQuestion(raw json.RawMessage) {
	var p struct {
		Title     string `json:"title"`
		Questions []struct {
			ID      string `json:"id"`
			Prompt  string `json:"prompt"`
			Options []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	json.Unmarshal(raw, &p)
	if len(p.Questions) > 0 {
		q := p.Questions[0]
		opts := make([]string, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, o.ID)
		}
		s.events <- agent.AgentEvent{
			Kind: agent.EventAgentPermission,
			Permission: &agent.AgentPermissionRequest{
				Tool: "cursor/ask_question", Action: q.Prompt, Options: opts,
			},
		}
	}
}

func (s *pipeSession) dispatchGenerateImage(raw json.RawMessage) {
	var p struct {
		Description string `json:"description"`
	}
	json.Unmarshal(raw, &p)
	s.events <- agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: fmt.Sprintf("[Image: %s]", p.Description),
	}
}

// rawCapture logs every JSON-RPC message from agent acp to t.Log,
// giving full visibility into the protocol.
type rawCapture struct {
	msgs []string
	mu   sync.Mutex
}

func (rc *rawCapture) add(line string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.msgs = append(rc.msgs, line)
}

func (rc *rawCapture) dump(t *testing.T) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	t.Logf("=== raw JSON-RPC messages (%d) ===", len(rc.msgs))
	for i, m := range rc.msgs {
		if len(m) > 500 {
			m = m[:500] + "...(truncated)"
		}
		t.Logf("  [%d] %s", i, m)
	}
}

// startRawSession is like startPipeSession but logs every raw JSON-RPC
// line to rawCapture for protocol analysis.
func startRawSession(t *testing.T, workspace string) (*pipeSession, *rawCapture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	cmd := exec.CommandContext(ctx, "agent", "acp")
	cmd.Dir = workspace
	stdinW, _ := cmd.StdinPipe()
	stdoutR, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	cap := &rawCapture{}
	s := &pipeSession{
		cmd:     cmd,
		stdin:   bufio.NewWriter(stdinW),
		pending: make(map[string]chan rpcMsg),
		events:  make(chan agent.AgentEvent, 512),
		cancel:  cancel,
	}
	go s.readLoopCapture(bufio.NewReader(stdoutR), cap)
	t.Cleanup(func() {
		cancel()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return s, cap
}

// readLoopCapture is like readLoop but also logs every raw line.
func (s *pipeSession) readLoopCapture(r *bufio.Reader, cap *rawCapture) {
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		cap.add(string(trimmed))
		var msg rpcMsg
		if err := json.Unmarshal(trimmed, &msg); err != nil {
			continue
		}
		if msg.ID != nil && msg.Method == "" {
			idStr := string(msg.ID)
			s.pendingMu.Lock()
			ch, ok := s.pending[idStr]
			if ok {
				delete(s.pending, idStr)
			}
			s.pendingMu.Unlock()
			if ok {
				ch <- msg
			}
			continue
		}
		if msg.Method != "" {
			s.dispatch(msg)
		}
	}
}

// ─── Integration Tests ────────────────────────────────────────────

func skipNoAgent(t *testing.T) {
	if _, err := exec.LookPath("agent"); err != nil {
		t.Skip("agent not on PATH")
	}
}

// runWithRetry runs fn up to maxAttempts times, skipping retries when
// the Cursor backend returns RetriableError.
func runWithRetry(t *testing.T, maxAttempts int, fn func(t *testing.T)) {
	t.Helper()
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			t.Logf("retry attempt %d/%d", attempt, maxAttempts)
		}
		subt := fmt.Sprintf("attempt_%d", attempt)
		t.Run(subt, func(t *testing.T) {
			fn(t)
		})
		if !t.Failed() {
			return
		}
		if attempt < maxAttempts {
			t.Log("retrying after failure...")
			time.Sleep(2 * time.Second)
		}
	}
}

// isRetriableError checks if any text event contained a Cursor backend error.
func isRetriableError(events []agent.AgentEvent) bool {
	for _, ev := range events {
		if ev.Kind == agent.EventAgentText && ev.Text != "" {
			if strings.Contains(ev.Text, "RetriableError") || strings.Contains(ev.Text, "Connection stalled") {
				return true
			}
		}
	}
	return false
}

// TestIntegration_RawCapture logs every JSON-RPC method Cursor sends.
// This is the ground-truth test for protocol understanding.
func TestIntegration_RawCapture(t *testing.T) {
	skipNoAgent(t)
	tmp := t.TempDir()
	s, cap := startRawSession(t, tmp)
	s.handshake(t, tmp)
	s.prompt("Write hello.txt and create a todo list with 2 items")

	var events []agent.EventKind
	timeout := time.After(120 * time.Second)
	for {
		select {
		case ev := <-s.events:
			events = append(events, ev.Kind)
			if ev.Kind == agent.EventAgentDone {
				goto done
			}
		case <-timeout:
			t.Fatal("timeout")
		}
	}
done:
	cap.dump(t)
	t.Logf("=== event kinds seen: %d total ===", len(events))
}

func TestIntegration_OutReply_OutResult(t *testing.T) {
	skipNoAgent(t)
	s := startPipeSession(t, t.TempDir())
	s.handshake(t, t.TempDir())
	s.prompt("Reply with exactly the word READY and nothing else.")

	var gotText bool
	timeout := time.After(60 * time.Second)
	for {
		select {
		case ev := <-s.events:
			switch ev.Kind {
			case agent.EventAgentText:
				gotText = true
				t.Logf("OutReply: %q", ev.Text)
			case agent.EventAgentToolStart:
				t.Logf("OutToolStart: %s", ev.ToolStart.Name)
			case agent.EventAgentToolEnd:
				t.Logf("OutToolEnd: %s", ev.ToolEnd.Name)
			case agent.EventAgentTaskCreate:
				t.Logf("OutTaskCreate: %d items", len(ev.TaskList.Items))
			case agent.EventAgentTaskUpdate:
				t.Logf("OutTaskUpdate: %d items", len(ev.TaskList.Items))
			case agent.EventAgentPermission:
				t.Logf("OutPermission: %s", ev.Permission.Tool)
			case agent.EventAgentDone:
				goto done
			}
		case <-timeout:
			t.Fatal("timeout")
		}
	}
done:
	if !gotText {
		t.Error("expected OutReply")
	}
}

func TestIntegration_OutToolXXX(t *testing.T) {
	skipNoAgent(t)
	runWithRetry(t, 3, func(t *testing.T) {
		tmp := t.TempDir()
		s := startPipeSession(t, tmp)
		s.handshake(t, tmp)
		s.prompt("Create a file hello.txt with content 'hello world'.")

		var toolStarts, toolEnds int
		var retriable bool
		timeout := time.After(90 * time.Second)
		for {
			select {
			case ev := <-s.events:
				switch ev.Kind {
				case agent.EventAgentToolStart:
					toolStarts++
					t.Logf("OutToolStart [%d]: %s", toolStarts, ev.ToolStart.Name)
				case agent.EventAgentToolEnd:
					toolEnds++
					t.Logf("OutToolEnd [%d]: %s", toolEnds, ev.ToolEnd.Name)
				case agent.EventAgentText:
					t.Logf("OutReply: %q", ev.Text)
					if strings.Contains(ev.Text, "RetriableError") || strings.Contains(ev.Text, "Connection stalled") {
						retriable = true
					}
				case agent.EventAgentDone:
					goto done
				}
			case <-timeout:
				t.Fatal("timeout")
			}
		}
	done:
		t.Logf("tool starts=%d ends=%d retriable=%v", toolStarts, toolEnds, retriable)
		if retriable {
			t.Skip("Cursor backend retriable error — skipping assertion")
		}
		if toolStarts == 0 {
			t.Error("expected at least one OutToolStart")
		}
	})
}

func TestIntegration_OutTaskXXX(t *testing.T) {
	skipNoAgent(t)
	runWithRetry(t, 3, func(t *testing.T) {
		tmp := t.TempDir()
		s := startPipeSession(t, tmp)
		s.handshake(t, tmp)
		s.prompt("Create a todo list with 3 items: write code, write tests, run tests. Do the first item.")

		var taskCreates, taskUpdates, toolStarts int
		var retriable bool
		timeout := time.After(90 * time.Second)
		for {
			select {
			case ev := <-s.events:
				switch ev.Kind {
				case agent.EventAgentTaskCreate:
					taskCreates++
					t.Logf("OutTaskCreate [%d]: %d items", taskCreates, len(ev.TaskList.Items))
					for _, item := range ev.TaskList.Items {
						t.Logf("  %s [%s] %s", item.ID, item.Status, item.Subject)
					}
				case agent.EventAgentTaskUpdate:
					taskUpdates++
					t.Logf("OutTaskUpdate [%d]: %d items", taskUpdates, len(ev.TaskList.Items))
				case agent.EventAgentToolStart:
					toolStarts++
					t.Logf("OutToolStart: %s", ev.ToolStart.Name)
				case agent.EventAgentToolEnd:
					t.Logf("OutToolEnd: %s", ev.ToolEnd.Name)
				case agent.EventAgentText:
					t.Logf("OutReply: %q", ev.Text)
					if strings.Contains(ev.Text, "RetriableError") || strings.Contains(ev.Text, "Connection stalled") {
						retriable = true
					}
				case agent.EventAgentDone:
					goto done
				}
			case <-timeout:
				t.Fatal("timeout")
			}
		}
	done:
		t.Logf("task creates=%d updates=%d tool_starts=%d retriable=%v", taskCreates, taskUpdates, toolStarts, retriable)
		if retriable {
			t.Skip("Cursor backend retriable error — skipping assertion")
		}
		// cursor/update_todos arrives as a top-level method call with id;
		// after the readLoop fix it should reach dispatch and emit TaskUpdate.
		if taskUpdates == 0 && toolStarts > 0 {
			t.Error("expected OutTaskUpdate from cursor/update_todos")
		}
	})
}

func TestIntegration_OutThinking(t *testing.T) {
	skipNoAgent(t)
	s := startPipeSession(t, t.TempDir())
	s.handshake(t, t.TempDir())
	s.prompt("Think step by step: what is 2+2? Show your reasoning.")

	var textChunks int
	timeout := time.After(60 * time.Second)
	for {
		select {
		case ev := <-s.events:
			switch ev.Kind {
			case agent.EventAgentText:
				textChunks++
				t.Logf("OutReply chunk %d: %q", textChunks, ev.Text)
			case agent.EventAgentDone:
				goto done
			}
		case <-timeout:
			t.Fatal("timeout")
		}
	}
done:
	t.Logf("text chunks: %d", textChunks)
	if textChunks == 0 {
		t.Error("expected text chunks")
	}
}

func TestIntegration_FullCover(t *testing.T) {
	skipNoAgent(t)
	s := startPipeSession(t, t.TempDir())
	s.handshake(t, t.TempDir())
	s.prompt("Create a file hello.go containing: package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"hello\") }")

	var textCount, toolStarts, toolEnds, taskCreates, taskUpdates, perms int
	timeout := time.After(120 * time.Second)
	for {
		select {
		case ev := <-s.events:
			switch ev.Kind {
			case agent.EventAgentText:
				textCount++
			case agent.EventAgentToolStart:
				toolStarts++
				t.Logf("OutToolStart [%d]: %s", toolStarts, ev.ToolStart.Name)
			case agent.EventAgentToolEnd:
				toolEnds++
				t.Logf("OutToolEnd [%d]: %s", toolEnds, ev.ToolEnd.Name)
			case agent.EventAgentTaskCreate:
				taskCreates++
				t.Logf("OutTaskCreate [%d]: %d items", taskCreates, len(ev.TaskList.Items))
			case agent.EventAgentTaskUpdate:
				taskUpdates++
				t.Logf("OutTaskUpdate [%d]: %d items", taskUpdates, len(ev.TaskList.Items))
			case agent.EventAgentPermission:
				perms++
				t.Logf("OutPermission: %s", ev.Permission.Tool)
			case agent.EventAgentDone:
				goto done
			}
		case <-timeout:
			t.Fatal("timeout")
		}
	}
done:
	t.Logf("=== Full Cover ===")
	t.Logf("  OutReply:      %d chunks", textCount)
	t.Logf("  OutToolStart:  %d", toolStarts)
	t.Logf("  OutToolEnd:    %d", toolEnds)
	t.Logf("  OutTaskCreate: %d", taskCreates)
	t.Logf("  OutTaskUpdate: %d", taskUpdates)
	t.Logf("  OutPermission: %d", perms)
	if toolStarts == 0 {
		t.Error("expected tool calls")
	}
}
