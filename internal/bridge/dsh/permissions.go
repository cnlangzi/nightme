// permissions.go — approval / question request handling.
//
// dsh surfaces permission requests via two channels:
//   1. mux serverFrame{method:"approval/requested"}   (server-side)
//   2. mux session/event type:"approval/asked"          (model-side)
//
// Both funnel through pendingApprovals (keyed by approvalID — the
// server-stable identifier). SendPermission picks the most-recent
// pending approval (mirror of codex §6.4 lastPendingID pattern)
// and POSTs to /api/respond. Single approval in flight keeps
// semantics unchanged; concurrent approvals stay unambiguous
// because each carries a distinct approvalID.
//
// Question requests reuse the same plumbing (they're morally a
// kind of approval with a list of choices). The runtime sees them
// as EventAgentPermission with a multi-line Action string and a
// label set; the answer comes back as a JSON-encoded outcome.
//
// Concurrency: the pendingMu lock guards map reads/writes. The
// response channel is buffered (1); SendPermission is the only
// writer, the runtime is the only reader. Lifecycle fail-path
// (driver.lifecycle) sends "declined" to all pending entries
// before closing the bridge so runtime handlers don't hang.
package dsh

import (
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// permissionTimeout bounds an unanswered approval. After this
// duration, lifecycle / a watchdog sends "declined" so the
// runtime isn't stuck. 5 minutes matches codex / pi / claudecode.
const permissionTimeout = 5 * time.Minute

// registerApproval is the shared entry for both mux and session/event
// approval paths. It mints a respCh, registers it under id (with
// insertion order tracked in pendingOrder for stable SendPermission
// routing — Go map iteration is randomized), spawns the timeout
// watchdog, and returns the channel. The caller wraps it in an
// EventAgentPermission and delivers to the runtime.
//
// Two call sites (handleApprovalRequested / handleInlineApproval)
// share this so the timeout semantics are identical: if neither
// SendPermission nor the bridge-shutdown path consumes the channel
// within permissionTimeout, we silently write "declined" so the
// model doesn't hang waiting for a user who never replies.
func (d *driver) registerApproval(id string) chan string {
	respCh := make(chan string, 1)
	d.pendingMu.Lock()
	d.pendingApprovals[id] = respCh
	d.pendingOrder = append(d.pendingOrder, id)
	d.pendingMu.Unlock()

	go func(approvalID string, ch chan string) {
		select {
		case <-ch:
			return
		case <-time.After(permissionTimeout):
		case <-d.closed:
			return
		}
		d.pendingMu.Lock()
		if pending, ok := d.pendingApprovals[approvalID]; ok && pending == ch {
			delete(d.pendingApprovals, approvalID)
			d.removeFromPendingOrderLocked(approvalID)
			select {
			case pending <- "declined":
			default:
			}
		}
		d.pendingMu.Unlock()
	}(id, respCh)
	return respCh
}

// removeFromPendingOrderLocked removes the first occurrence of id
// from pendingOrder. Caller MUST hold d.pendingMu.
func (d *driver) removeFromPendingOrderLocked(id string) {
	for i, e := range d.pendingOrder {
		if e == id {
			d.pendingOrder = append(d.pendingOrder[:i], d.pendingOrder[i+1:]...)
			return
		}
	}
}

// handleApprovalRequested is the mux approval/requested entry.
// Called from handleMuxFrame (translate.go).
func (d *driver) handleApprovalRequested(ar muxApprovalRequested) {
	respCh := d.registerApproval(ar.ApprovalID)
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentPermission,
		Permission: &agent.AgentPermissionRequest{
			Tool:    ar.ToolName,
			Action:  ar.Reason,
			Options: []string{"approve", "decline"},
			ResponseCh: respCh,
		},
	})
}

// handleInlineApproval is the session/event approval/asked entry.
// Same plumbing as handleApprovalRequested; we synthesize an
// approvalID from ToolCallID (session/event doesn't carry a
// server-issued id). The "evt-" prefix marks this as coming from
// the session/event channel (distinct from mux approval/requested
// IDs), avoiding any chance of collision with server-issued IDs
// in the same map.
func (d *driver) handleInlineApproval(toolCallID, toolName, action string, options []string) {
	if toolCallID == "" {
		return
	}
	respCh := d.registerApproval("evt-" + toolCallID)

	if len(options) == 0 {
		options = []string{"approve", "decline"}
	}
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentPermission,
		Permission: &agent.AgentPermissionRequest{
			Tool:    toolName,
			Action:  action,
			Options: options,
			ResponseCh: respCh,
		},
	})
}

// handleQuestionRequested is the mux question/requested entry.
// dsh web's question UX is multi-choice (zero-to-many options);
// we encode the question + choices into the runtime's Action
// string so existing IM-channel rendering can show the question
// without changes. The runtime's answer is the chosen option's
// label; we forward it as the outcome's `value` field.
//
// `frameRpcID` is the server-frame rpcId of the question/requested
// mux frame — /api/respond is keyed on this, NOT on
// qr.SessionID+":q" (the OLD wrong key).
func (d *driver) handleQuestionRequested(frameRpcID string, qr muxQuestionRequested) {
	if len(qr.Questions) == 0 {
		// dsh sometimes emits a question/requested frame with an
		// empty Questions slice (e.g. a no-op placeholder). Don't
		// index into the empty slice below — log and skip.
		dLog("dsh: question/requested with empty Questions, skipping")
		return
	}

	respCh := d.registerApproval(frameRpcID)
	d.pendingMu.Lock()
	d.lastApprovalID[frameRpcID] = qr.SessionID + ":q"
	d.pendingMu.Unlock()

	// Render Action as "<header> — <question> [<opt1> | <opt2> | ...]"
	// (matches codex §6.3 inline encoding so channels don't need
	// to change). Multi-select questions use "[<multi>]" suffix
	// (legacy from codex); we don't currently encode multi-select
	// in our channel action string but the runtime can detect
	// multi from options len.
	var action strings.Builder
	for i, q := range qr.Questions {
		if i > 0 {
			action.WriteString("\n")
		}
		if q.Header != "" {
			action.WriteString(q.Header)
			action.WriteString(" — ")
		}
		action.WriteString(q.Question)
		if len(q.Options) > 0 {
			action.WriteString(" [")
			action.WriteString(strings.Join(q.Options, " | "))
			action.WriteString("]")
		}
	}

	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentPermission,
		Permission: &agent.AgentPermissionRequest{
			Tool:    "question",
			Action:  action.String(),
			Options: qr.Questions[0].Options, // best-effort; multi-question paths need channel work
			ResponseCh: respCh,
		},
	})
}
