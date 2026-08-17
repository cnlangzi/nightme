// permissions.go — approval / question request handling.
//
// dsh surfaces permission requests via two channels:
//  1. mux serverFrame{method:"approval/requested"}   (server-side)
//  2. mux session/event type:"approval/asked"          (model-side)
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
// as EventAgentPermission with Questions populated; Feishu renders
// a one-shot card (len<=1) or an in-card wizard (len>1). The
// answer is POSTed as QuestionResponse (dsh-api.md §2.12.2).
//
// Concurrency: the pendingMu lock guards map reads/writes. The
// response channel is buffered (1); SendPermission is the only
// writer, the runtime is the only reader. Lifecycle fail-path
// (driver.lifecycle) sends "declined" to all pending entries
// before closing the bridge so runtime handlers don't hang.
package dsh

import (
	"fmt"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
	"github.com/cnlangzi/nightme/internal/messages"
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
// Called from handleMuxFrame. The respond key is frameRpcID (host
// pendingApprovals map), NOT payload.approvalId (audit-only).
func (d *driver) handleApprovalRequested(frameRpcID string, ar muxApprovalRequested) {
	if frameRpcID == "" {
		dLog("dsh: approval/requested missing frame rpcId, skipping")
		return
	}
	respCh := d.registerApproval(frameRpcID)
	d.pendingMu.Lock()
	d.lastApprovalID[frameRpcID] = ar.ApprovalID
	d.pendingMu.Unlock()
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentPermission,
		Permission: &agent.AgentPermissionRequest{
			Tool:       ar.ToolName,
			Action:     ar.Reason,
			Options:    []string{approvalAllowOnce, approvalReject},
			Kind:       agent.PermissionKindApproval,
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
		options = []string{approvalAllowOnce, approvalReject}
	}
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentPermission,
		Permission: &agent.AgentPermissionRequest{
			Tool:       toolName,
			Action:     action,
			Options:    options,
			Kind:       agent.PermissionKindApproval,
			ResponseCh: respCh,
		},
	})
}

// handleQuestionRequested is the mux question/requested entry.
// dsh web's AskUserQuestion UX is a batch: host matchesQuestions
// requires answers.length == questions.length, each answer.id
// echoing the question id, in order. We therefore keep the batch
// on AgentPermissionRequest.Questions and only put the first
// question's labels in Options (one-shot / non-wizard channels).
// Feishu pages len>1 as an in-card wizard and POSTs the full
// batch (nm-q: prefix) only after the last step. A plain label
// still maps onto the matching question in questionAnswerFor;
// unmatched text becomes custom on the first question.
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
	d.pendingQuestions[frameRpcID] = qr.Questions
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
			action.WriteString(strings.Join(optionLabels(q.Options), " | "))
			action.WriteString("]")
		}
	}

	qs := make([]agent.AgentPermissionQuestion, len(qr.Questions))
	for i, q := range qr.Questions {
		qs[i] = agent.AgentPermissionQuestion{
			ID:       q.ID,
			Header:   q.Header,
			Question: q.Question,
			Options:  optionLabels(q.Options),
		}
	}
	firstOpts := qs[0].Options

	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentPermission,
		Permission: &agent.AgentPermissionRequest{
			Tool:       "question",
			Action:     action.String(),
			Options:    firstOpts,
			Questions:  qs,
			Kind:       agent.PermissionKindQuestion,
			ResponseCh: respCh,
		},
	})
}

// questionAnswerFor maps a single user reply (clicked label or
// typed text) onto the AskUserQuestionAnswer batch host
// matchesQuestions expects. A label that appears in a question's
// options is selected on that question; remaining questions get
// empty selected. A reply that matches no option becomes custom
// on the first question (empty selected — host rejects custom
// combined with selected on non-multi questions).
func questionAnswerFor(qs []questionPayload, resp string) (host.QuestionAnswer, error) {
	if len(qs) == 0 {
		return host.QuestionAnswer{}, fmt.Errorf("dsh: empty question batch")
	}
	if picks, ok := messages.DecodeQuestionPicks(resp); ok {
		return questionAnswerFromPicks(qs, picks)
	}
	answers := make([]host.QuestionAnswerItem, len(qs))
	matched := -1
	for i, q := range qs {
		if q.ID == "" {
			return host.QuestionAnswer{}, fmt.Errorf("dsh: question %d missing id", i)
		}
		item := host.QuestionAnswerItem{ID: q.ID, Selected: []string{}}
		if matched < 0 {
			for _, opt := range q.Options {
				if opt.Label == resp {
					item.Selected = []string{resp}
					matched = i
					break
				}
			}
		}
		answers[i] = item
	}
	if matched < 0 {
		answers[0].Custom = resp
		answers[0].Selected = []string{}
	}
	return host.QuestionAnswer{Answers: answers}, nil
}

func questionAnswerFromPicks(qs []questionPayload, picks []messages.QuestionPick) (host.QuestionAnswer, error) {
	byID := make(map[string]messages.QuestionPick, len(picks))
	for _, p := range picks {
		byID[p.ID] = p
	}
	answers := make([]host.QuestionAnswerItem, len(qs))
	for i, q := range qs {
		if q.ID == "" {
			return host.QuestionAnswer{}, fmt.Errorf("dsh: question %d missing id", i)
		}
		item := host.QuestionAnswerItem{ID: q.ID, Selected: []string{}}
		if p, ok := byID[q.ID]; ok {
			if p.Custom != "" {
				item.Custom = p.Custom
			} else if len(p.Selected) > 0 {
				item.Selected = append([]string(nil), p.Selected...)
			}
		}
		answers[i] = item
	}
	return host.QuestionAnswer{Answers: answers}, nil
}

// Dashboard / Feishu labels for mux approval/requested. Host wire
// vocabulary is allowed-once | rejected; these are the user-facing
// buttons (matching dsh ApprovalPanel).
const (
	approvalAllowOnce = "Allow once"
	approvalReject    = "Reject"
)

// handleApprovalResolved drops the local pending entry when the
// host already settled the gate (dashboard Allow once / Reject,
// timeout, cancel). Mux session/event keeps flowing either way —
// this only stops nightme from holding an unanswered Feishu card.
func (d *driver) handleApprovalResolved(ar muxApprovalResolved) {
	if !d.dropPendingByApprovalID(ar.ApprovalID) {
		return
	}
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentPermissionSettled,
		PermissionSettled: &agent.AgentPermissionSettled{
			Outcome: ar.Outcome,
			Source:  "dashboard",
		},
	})
}

func (d *driver) handleQuestionResolved(questionRpcID, outcome string) {
	if questionRpcID == "" || !d.dropPendingByRPCID(questionRpcID) {
		return
	}
	d.deliver(agent.AgentEvent{
		Kind: agent.EventAgentPermissionSettled,
		PermissionSettled: &agent.AgentPermissionSettled{
			Outcome: outcome,
			Source:  "dashboard",
		},
	})
}

func (d *driver) dropPendingByApprovalID(approvalID string) bool {
	if approvalID == "" {
		return false
	}
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	for rpcID, aid := range d.lastApprovalID {
		if aid == approvalID {
			return d.dropPendingLocked(rpcID)
		}
	}
	return false
}

func (d *driver) dropPendingByRPCID(rpcID string) bool {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	return d.dropPendingLocked(rpcID)
}

func (d *driver) dropPendingLocked(rpcID string) bool {
	ch, ok := d.pendingApprovals[rpcID]
	if !ok {
		return false
	}
	delete(d.pendingApprovals, rpcID)
	delete(d.pendingQuestions, rpcID)
	delete(d.lastApprovalID, rpcID)
	d.removeFromPendingOrderLocked(rpcID)
	if ch != nil {
		select {
		case ch <- "settled":
		default:
		}
	}
	return true
}
