// Server-initiated request handling for the codex app-server bridge.
//
// App-server pushes 4 kinds of requests that need a response on the
// same id:
//
//   - item/commandExecution/requestApproval → EventAgentPermission (Bash)
//   - item/fileChange/requestApproval       → EventAgentPermission (Patch)
//   - item/permissions/requestApproval      → EventAgentPermission (Permissions)
//   - item/tool/requestUserInput            → EventAgentPermission (AskUserQuestion, multi-question)
//   - item/tool/call                        → response with success:false (tool not available in MVP)
//
// All paths emit EventAgentPermission so the channel layer can render
// a single approval UI (claudecode's AskUserQuestion follows the
// same shape). SendPermission(resp string) on the Agent looks up the
// pending channel by requestId and writes resp to it; this file is
// the producer side.
//
// Concurrency: each server request spawns its own decision goroutine
// so a slow approval does not stall the read pump. The goroutine
// selects on (responseCh | ctx.Done | 5min timer) and writes the
// JSON-RPC response back on the same id.
package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// handleServerRequest dispatches by method. Unknown methods get a
// json-rpc "method not found" (-32601) response so the child is not
// left hanging.
func (s *session) handleServerRequest(method string, rawID, params json.RawMessage) {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		s.handleApprovalRequest(method, rawID, params)
	case "item/permissions/requestApproval":
		s.handlePermissionsApproval(rawID, params)
	case "item/tool/requestUserInput":
		s.handleRequestUserInput(rawID, params)
	case "item/tool/call":
		s.handleDynamicToolCall(rawID, params)
	default:
		cLog("server request: unknown method", "method", method)
		_ = s.rpc.respondErr(rawID, codeMethodNotFound, "method not found")
	}
}

// handleApprovalRequest handles commandExecution / fileChange approval.
// Tool name and action are mapped from the wire params; the user sees
// options = ["accept", "decline"].
func (s *session) handleApprovalRequest(method string, rawID, params json.RawMessage) {
	var requestID string
	_ = json.Unmarshal(rawID, &requestID)

	var toolName, action string
	switch method {
	case "item/commandExecution/requestApproval":
		var p commandApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			_ = s.rpc.respondErr(rawID, codeParseError, "bad params")
			return
		}
		toolName = "Bash"
		action = "Run command: " + p.Command
		if p.CWD != "" {
			action += "\n(in " + p.CWD + ")"
		}
	case "item/fileChange/requestApproval":
		var p fileChangeApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			_ = s.rpc.respondErr(rawID, codeParseError, "bad params")
			return
		}
		toolName = "Patch"
		action = p.Reason
		if action == "" {
			action = "Apply patch"
		}
	}

	s.spawnApproval(requestID, toolName, action, []string{"accept", "decline"},
		func(resp string) {
			decision := "decline"
			if resp == "accept" {
				decision = "accept"
			}
			_ = s.rpc.respond(rawID, map[string]any{"decision": decision})
		})
}

// handlePermissionsApproval handles the umbrella "permissions" gate.
// Same options as a regular approval.
func (s *session) handlePermissionsApproval(rawID, params json.RawMessage) {
	var p permissionsApprovalParams
	if err := json.Unmarshal(params, &p); err != nil {
		_ = s.rpc.respondErr(rawID, codeParseError, "bad params")
		return
	}
	var requestID string
	_ = json.Unmarshal(rawID, &requestID)
	action := p.Reason
	if action == "" {
		action = "Permissions required"
	}
	s.spawnApproval(requestID, "Permissions", action,
		[]string{"accept", "decline"},
		func(resp string) {
			decision := "decline"
			if resp == "accept" {
				decision = "accept"
			}
			_ = s.rpc.respond(rawID, map[string]any{"decision": decision})
		})
}

// handleRequestUserInput forwards the multi-question gate to the
// channel layer as a single EventAgentPermission. The channel must
// understand the inline encoding:
//
//	Action = "<qid1>: <header1> — <question1> [option1 | option2 | ...]\n<qid2>: ..."
//	SendPermission("<qid1>:<labels-逗号分隔>|<qid2>:...")
//
// This keeps AgentPermissionRequest.ResponseCh a single string channel
// while still carrying the per-question answer back to the bridge.
//
// For multi-select questions, the labels are joined with "," in the
// option list display and split back by "," in the response parser.
func (s *session) handleRequestUserInput(rawID, params json.RawMessage) {
	var p requestUserInputParams
	if err := json.Unmarshal(params, &p); err != nil {
		_ = s.rpc.respondErr(rawID, codeParseError, "bad params")
		return
	}
	if len(p.Questions) == 0 {
		_ = s.rpc.respondErr(rawID, codeInvalidRequest, "no questions")
		return
	}

	var actionLines []string
	for _, q := range p.Questions {
		var opts []string
		for _, o := range q.Options {
			opts = append(opts, o.Label)
		}
		multiselect := ""
		if q.MultiSelect {
			multiselect = " (multi)"
		}
		line := fmt.Sprintf("%s%s: %s — %s [%s]",
			q.ID, multiselect, q.Header, q.Question, strings.Join(opts, " | "))
		actionLines = append(actionLines, line)
	}
	action := strings.Join(actionLines, "\n")
	var requestID string
	_ = json.Unmarshal(rawID, &requestID)

	s.spawnApproval(requestID, "AskUserQuestion", action,
		// Option strings exposed to the channel renderer. The channel
		// sends back a string in our internal "<qid>:labels|<qid>:labels"
		// format; we surface a generic "ok" so the renderer has a
		// single primary button.
		[]string{"ok"},
		func(resp string) {
			answers := parseRequestUserInputResponse(resp, p.Questions)
			_ = s.rpc.respond(rawID, requestUserInputResponseResult{Answers: answers})
		})
}

// handleDynamicToolCall returns a "tool not available" error to the
// child. Matches cc-connect's behaviour — dynamic tools are out of
// scope for the MVP.
func (s *session) handleDynamicToolCall(rawID, _ json.RawMessage) {
	cLog("item/tool/call: returning tool not available")
	_ = s.rpc.respond(rawID, map[string]any{
		"success": false,
		"contentItems": []map[string]any{
			{"type": "inputText", "text": "tool not available in nightme bridge"},
		},
	})
}

// spawnApproval is the shared plumbing for every approval request:
//
//   - register a pending channel keyed by requestID
//   - emit EventAgentPermission with Tool / Action / Options / ResponseCh
//   - spawn a decision goroutine that waits on (resp | ctx.Done | 5min timer)
//   - the decision goroutine calls reply() with the chosen response, then
//     unregisters the pending channel
//
// reply is called exactly once (the pending channel is buffer 1, and
// the goroutine unregisters on every path).
func (s *session) spawnApproval(
	requestID, tool, action string,
	options []string,
	reply func(resp string),
) {
	ch := make(chan string, 1)
	s.pendingMu.Lock()
	s.pendingApprovals[requestID] = ch
	s.lastPendingID = requestID
	s.pendingMu.Unlock()

	s.deliver(agent.AgentEvent{
		Kind: agent.EventAgentPermission,
		Permission: &agent.AgentPermissionRequest{
			Tool:       tool,
			Action:     action,
			Options:    options,
			ResponseCh: ch,
		},
	})

	go func() {
		timer := time.NewTimer(permissionTimeout)
		defer timer.Stop()
		var resp string
		select {
		case resp = <-ch:
		case <-s.ctx.Done():
			resp = "decline"
		case <-timer.C:
			resp = "decline"
			slog.Default().Info("codex: approval timed out, defaulting to decline",
				slog.String("tool", tool),
				slog.String("request_id", requestID))
		}

		s.pendingMu.Lock()
		delete(s.pendingApprovals, requestID)
		if s.lastPendingID == requestID {
			s.lastPendingID = ""
		}
		s.pendingMu.Unlock()

		reply(resp)
	}()
}

// ─── request_user_input helpers ───

// parseRequestUserInputResponse decodes the channel-supplied string
// back into the structured answers map. Encoding:
//
//	"<qid1>:<label>[,<label>...]|<qid2>:<label>[,<label>...]"
//
// Single-select questions: <label>. Multi-select: <label1>,<label2>,...
// Unparseable answers default to the first option for the question so
// we never produce a malformed response to the child.
func parseRequestUserInputResponse(raw string, questions []appServerRequestUserInputQuestion) map[string]requestUserInputAnswer {
	out := make(map[string]requestUserInputAnswer, len(questions))
	if raw == "" {
		return fallbackAnswers(questions)
	}
	parts := strings.Split(raw, "|")
	for _, part := range parts {
		idx := strings.IndexByte(part, ':')
		if idx <= 0 {
			continue
		}
		qid := part[:idx]
		body := part[idx+1:]
		var labels []string
		for _, l := range strings.Split(body, ",") {
			l = strings.TrimSpace(l)
			if l != "" {
				labels = append(labels, l)
			}
		}
		if len(labels) == 0 {
			continue
		}
		out[qid] = requestUserInputAnswer{Answers: labels}
	}
	// Fill in any missing qid with the first option as a safety net.
	for _, q := range questions {
		if _, ok := out[q.ID]; !ok {
			if len(q.Options) > 0 {
				out[q.ID] = requestUserInputAnswer{Answers: []string{q.Options[0].Label}}
			} else {
				out[q.ID] = requestUserInputAnswer{Answers: []string{}}
			}
		}
	}
	return out
}

func fallbackAnswers(questions []appServerRequestUserInputQuestion) map[string]requestUserInputAnswer {
	out := make(map[string]requestUserInputAnswer, len(questions))
	for _, q := range questions {
		if len(q.Options) > 0 {
			out[q.ID] = requestUserInputAnswer{Answers: []string{q.Options[0].Label}}
		} else {
			out[q.ID] = requestUserInputAnswer{Answers: []string{}}
		}
	}
	return out
}

// ─── expose translator field & handleServerRequest on session ───

// errPermissionsReply is a sentinel returned when SendPermission is
// called with no pending approval. The runtime surfaces it as a
// "you didn't have a pending approval" error, not a panic.
var errPermissionsReply = errors.New("codex: no pending approval")
