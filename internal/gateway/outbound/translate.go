// Package outbound — translator from agent.AgentEvent to the
// abstract messages.OutboundMessage stream. The translator is an
// outbound concern (not a Channel concern) because:
//
//   - AgentEvent is part of the agent protocol; only the outbound
//     package speaks it.
//   - OutboundMessage is the abstract wire format that Channels
//     consume. Channels do not see AgentEvent.
//   - Translation may need to merge multiple AgentEvents into one
//     OutboundMessage (e.g. OutToolStart + OutToolEnd pair collapsing
//     into a single line in the rolled log) — an outbound-level
//     decision. Channels render the result.
//
// The current translator is a 1:1 mapping: one AgentEvent produces
// one OutboundMessage. Channels like Feishu may further roll the
// OutboundMessage stream into a single edited message (see F-25
// v0.3 + F-26 Stage 3).
//
// Moved from internal/gateway/translate.go in the F-?-outbound
// refactor — see package doc for the broader rationale.
package outbound

import (
	"fmt"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// thinkingPrefix is the sentinel the claudecode bridge prepends to
// every thinking block before emitting EventAgentText. Renderer.render
// (and Channel-specific renderers) use it to tell thinking from a
// final reply and surface them with different icons (💭 vs 💬).
// ThinkingPrefix is the sentinel the claudecode bridge (and the dsh
// bridge's reasoning-block path) prepends to every thinking block
// before emitting EventAgentText. Renderer.render (and Channel-
// specific renderers) use it to tell thinking from a final reply
// and surface them with different icons (💭 vs 💬).
//
// Exported so the dsh bridge (and any future bridge that wants
// thinking to flow on OutThinking rather than OutReply) can write
// the same prefix without copy-pasting the literal across the
// bridge / gateway boundary.
const ThinkingPrefix = "[思考] "

// Translate converts one agent.AgentEvent into the abstract
// messages.OutboundMessage stream. Returns the message to send and a
// boolean indicating whether anything should be sent at all:
//
//   - (msg, true)  → Emitter.Send should send msg
//   - (zero, false) → drop (e.g. terminal events that have no
//     user-facing content; the receipt already reflects the
//     final state)
//
// Terminal events (Done, Error) are NOT emitted as separate
// OutboundMessages; the receipt's terminal header carries that
// signal. Permission events are mapped to OutChoice; the Channel
// renders the choice natively (Feishu interactive card, Slack block kit,
// Web HTML).
func Translate(chatID string, ev agent.AgentEvent) (messages.OutboundMessage, bool) {
	switch ev.Kind {
	case agent.EventAgentText:
		text := strings.TrimSpace(ev.Text)
		if text == "" {
			return messages.OutboundMessage{}, false
		}
		if strings.HasPrefix(text, ThinkingPrefix) {
			return messages.OutboundMessage{
				ChatID: chatID,
				Kind:   messages.OutThinking,
				Text:   strings.TrimPrefix(text, ThinkingPrefix),
			}, true
		}
		return messages.OutboundMessage{
			ChatID: chatID,
			Kind:   messages.OutReply,
			Text:   text,
		}, true

	case agent.EventAgentToolStart:
		if ev.ToolStart == nil {
			return messages.OutboundMessage{}, false
		}
		name := ev.ToolStart.Name
		if name == "" {
			name = "tool"
		}
		// Outbound only transports the unified ToolInfo —
		// channel decides how to render "🔧 name(args)" or its
		// own equivalent. Outbound does NOT pre-format Text
		// (removed) or stash per-tool fields in Meta (those were
		// Feishu-specific implicit keys leaking into the
		// abstract layer; see F-34 review P0-2 / Devin
		// architecture feedback 2026-08-04).
		return messages.OutboundMessage{
			ChatID: chatID,
			Kind:   messages.OutToolStart,
			Tool: &messages.ToolInfo{
				Name: name,
				Args: ev.ToolStart.Args,
			},
		}, true

	case agent.EventAgentToolEnd:
		if ev.ToolEnd == nil {
			return messages.OutboundMessage{}, false
		}
		name := ev.ToolEnd.Name
		if name == "" {
			name = "tool"
		}
		// Same as EventAgentToolStart above — ToolInfo carries the
		// generic fields; outbound does not format Text or use
		// Meta for tool data.
		return messages.OutboundMessage{
			ChatID: chatID,
			Kind:   messages.OutToolEnd,
			Tool: &messages.ToolInfo{
				Name:   name,
				Args:   ev.ToolEnd.Args,
				Output: ev.ToolEnd.Output,
				Err:    ev.Err,
			},
		}, true

	case agent.EventAgentPermission:
		if ev.Permission == nil {
			return messages.OutboundMessage{}, false
		}
		req := ev.Permission
		isQuestion := req.Kind == agent.PermissionKindQuestion || len(req.Questions) > 0
		title := "Waiting for approval"
		kind := messages.ChoiceKindPermission
		if isQuestion {
			title = "Action Needed"
			kind = messages.ChoiceKindQuestion
		}
		card := &messages.Choice{
			RequestID: fmt.Sprintf("perm:%s:%d", chatID, time.Now().UnixNano()),
			Kind:      kind,
			Title:     title,
			Body:      req.Tool + ": " + req.Action,
			Options:   messages.ChoiceOptionsFromLabels(req.Options),
		}
		if n := len(req.Questions); n > 0 {
			card.Title = "Action Needed"
			card.Kind = messages.ChoiceKindQuestion
			card.Questions = make([]messages.ChoiceQuestion, n)
			for i, q := range req.Questions {
				card.Questions[i] = messages.ChoiceQuestion{
					ID:       q.ID,
					Header:   q.Header,
					Question: q.Question,
					Options:  messages.ChoiceOptionsFromLabels(q.Options),
				}
			}
			if n > 0 {
				card.Options = append([]messages.ChoiceOption(nil), card.Questions[0].Options...)
			}
		}
		return messages.OutboundMessage{
			ChatID: chatID,
			Kind:   messages.OutChoice,
			Choice: card,
		}, true

	case agent.EventAgentPermissionSettled:
		outcome := ""
		if ev.PermissionSettled != nil {
			outcome = ev.PermissionSettled.Outcome
		}
		if outcome == "" {
			outcome = "resolved"
		}
		return messages.OutboundMessage{
			ChatID: chatID,
			Kind:   messages.OutChoicePatch,
			Choice: &messages.Choice{
				Title:   "Waiting for approval",
				Body:    "✓ **" + outcome + "**（dashboard）",
				Kind:    messages.ChoiceKindPermission,
				Settled: true,
			},
		}, true

	case agent.EventAgentError:
		// Non-graceful bridge death. We emit a dedicated
		// OutError card carrying the structured Diagnostic so the
		// user gets a clear "bridge died because X" signal —
		// the receipt alone only flips to ✅, which is
		// indistinguishable from a clean turn end.
		//
		// When Diagnostic is nil, return the legacy silent-drop
		// behaviour: the receipt's terminal emoji flips the same
		// way regardless, and pre-Diagnostic-era bridges (or
		// EventAgentError events without a populated Diagnostic,
		// e.g. Err-only) keep working as before this kind.
		if ev.Diagnostic == nil {
			return messages.OutboundMessage{}, false
		}
		// Short body = first line of Err; longer detail (stderr
		// tail, waitErr) goes via Diagnostic for channels that
		// render it.
		body := ""
		if ev.Err != nil {
			body = ev.Err.Error()
			// Trim to the first line so the card body stays
			// scannable — the stderr tail is rendered below the
			// fold by channels that respect Diagnostic.StderrTail.
			if nl := strings.IndexByte(body, '\n'); nl >= 0 {
				body = body[:nl]
			}
		}
		if body == "" {
			// Err was nil but Diagnostic was populated — synthesize
			// a short body. Skip the agent name segment when it's
			// empty rather than emitting a leading-space "bridge"
			// label with no attribution.
			if ev.Diagnostic.AgentName != "" {
				body = fmt.Sprintf("%s process exited (%s)",
					ev.Diagnostic.AgentName, ev.Diagnostic.ExitKind)
			} else {
				body = fmt.Sprintf("process exited (%s)", ev.Diagnostic.ExitKind)
			}
		}
		return messages.OutboundMessage{
			ChatID:     chatID,
			Kind:       messages.OutError,
			Text:       body,
			Err:        ev.Err,
			Diagnostic: ev.Diagnostic,
			AgentName:  ev.AgentName,
			Workspace:  ev.Workspace,
		}, true

	case agent.EventAgentDone:
		// Terminal events are reflected in the receipt's terminal
		// header; the Stage 3 Feishu renderer flips the reaction
		// emoji and edits the header line. We don't emit a separate
		// OutboundMessage for them here.
		//
		// Translate is a pass-through — runtime (newEventHandler in
		// cmd/nightme/run.go) does NOT read Done.Usage separately;
		// per-turn Usage flows exclusively from EventAgentResult.Usage
		// (co-located with the final assistant reply). Bridges that
		// only emit EventAgentDone-with-Usage and no EventAgentResult will not
		// see their Usage reach the channel footer — by design
		// (F-45 §1.5 / F-52).
		return messages.OutboundMessage{}, false

	case agent.EventAgentResult:
		// Final assistant reply (Claude Code: result.Result).
		// Distinct from EventAgentText so channels can render it with a
		// dedicated icon (📝) instead of as a rolling-log entry.
		// We emit even when Text is empty AND IsError is true so
		// the channel can flip its header to an error state.
		if ev.Result == nil {
			return messages.OutboundMessage{}, false
		}
		if ev.Result.Text == "" && ev.Err == nil {
			return messages.OutboundMessage{}, false
		}
		out := messages.OutboundMessage{
			ChatID: chatID,
			Kind:   messages.OutResult,
			Text:   ev.Result.Text,
			Result: ev.Result,
		}
		// Bridges populate ev.Result.Usage from the same wire event
		// they took Text from (Claude Code result.usage +
		// result.modelUsage; Pi message_end.usage on the assistant
		// role). Co-locating the usage on the OutResult eliminates
		// the EventAgentResult-then-EventUsage ordering hazard that
		// forced the runtime to buffer OutResult. nil usage
		// (zero-usage turn / synthetic message) is fine — the
		// runtime is a passive pass-through, so a nil Usage just
		// means the channel footer omits Line 2.
		if u := ev.Result.Usage; u != nil {
			out.Usage = (*messages.UsageInfo)(u)
		}
		return out, true

	case agent.EventAgentReady:
		// Session bootstrap (Claude Code: system/init). Carries
		// session_id + model; channels surface them in the receipt
		// header so users can identify the session for /resume.
		// agent_name + workspace are forwarded too so the Feishu
		// receipt card's foot note can render
		// "Agent | cwd · tokens" (see docs/channel/feishu.md §9.3).
		//
		// Note: bridges always stamp the 5 context fields on every
		// event (incl. EventAgentReady), so we just pass them
		// through. No "if Ready != nil" guard needed.
		return messages.OutboundMessage{
			ChatID:    chatID,
			Kind:      messages.OutInit,
			Text:      fmt.Sprintf("session initialized (model: %s)", ev.Model),
			SessionID: ev.SessionID,
			Model:     ev.Model,
			AgentName: ev.AgentName,
			Workspace: ev.Workspace,
			Branch:    ev.Branch,
		}, true

	case agent.EventAgentTaskCreate:
		// F-38: full task snapshot replaces the per-turn checklist.
		// The Feishu adapter writes it into the current receipt and
		// PATCHes the card; other Channels may render or drop.
		// The bridge MUST only emit this after a confirmed success
		// result (see internal/bridge/claudecode/task.go).
		if ev.TaskList == nil {
			return messages.OutboundMessage{}, false
		}
		return messages.OutboundMessage{
			ChatID:   chatID,
			Kind:     messages.OutTaskCreate,
			TaskList: ev.TaskList,
		}, true

	case agent.EventAgentTaskUpdate:
		// F-38: subsequent mutations / deletions. Same snapshot
		// semantics as EventAgentTaskCreate; an empty Items slice is a
		// valid "clear the checklist" signal (e.g. all tasks done).
		if ev.TaskList == nil {
			return messages.OutboundMessage{}, false
		}
		return messages.OutboundMessage{
			ChatID:   chatID,
			Kind:     messages.OutTaskUpdate,
			TaskList: ev.TaskList,
		}, true
	}
	return messages.OutboundMessage{}, false
}
