package me

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/llm"
)

// nudgeText is what the run says when the chat model answered with
// words instead of a tool call.
const nudgeText = "Call a tool to make progress. A reply without a tool call does not move the run forward."

// system2 runs one turn of the chat model.
//
// It reports whether the run is over.
func (s *Service) system2(ctx context.Context, r *run) (bool, error) {
	conversation := r.conversation()
	if len(conversation) == 0 {
		// The run talks to the chat model for the first time at a
		// judgment point. A chat model with nothing but a system
		// prompt has no conversation to answer: the API refuses the
		// request. The opening turn says what the run is up
		// against.
		r.appendMessage(llm.Message{
			Role:  llm.RoleUser,
			Parts: []llm.Part{{Type: llm.PartText, Text: s.firstTurnPrompt(r)}},
		})
		conversation = r.conversation()
	}
	req := llm.Request{
		System:   s.systemPrompt(r),
		Messages: compact(conversation),
		Tools:    toolCatalogue(),
	}
	resp, err := s.cfg.Provider.Complete(ctx, req)
	if err != nil {
		return false, fmt.Errorf("the chat model failed: %w", err)
	}
	if resp == nil {
		return false, errors.New("the chat model answered nothing")
	}

	r.mu.Lock()
	r.system2Decisions++
	r.system2Usage.Add(resp.Usage)
	spent := r.system2Usage.Total()
	r.mu.Unlock()
	r.setDecision(decisionChat, "the chat model took the turn")

	if spent > s.cfg.TokenBudget {
		return false, fmt.Errorf("the run reached its token budget of %d tokens", s.cfg.TokenBudget)
	}

	// The model's own words are what the operator reads in the run's
	// overview.
	if text := strings.TrimSpace(resp.Message.Text()); text != "" {
		r.setNote(text)
		s.updateCard(r)
	}

	calls := resp.Message.ToolCalls()
	if len(calls) == 0 {
		if r.wasNudged() {
			// The model answered with words twice. The run cannot go
			// on, so it ends as a failure.
			r.markNudged()
			r.markFinished()
			r.setSummary("the chat model did not call a tool")
			s.finish(r, PhaseFailed, "the chat model did not call a tool")
			s.updateCard(r)
			return true, nil
		}
		r.markNudged()
		r.appendMessage(llm.Message{
			Role:  llm.RoleUser,
			Parts: []llm.Part{{Type: llm.PartText, Text: nudgeText}},
		})
		return false, nil
	}

	r.appendMessage(resp.Message)
	pending := make([]pendingCall, 0, len(calls))
	for _, c := range calls {
		pending = append(pending, pendingCall{ID: c.ToolCallID, Name: c.ToolName, Args: c.Args})
	}
	r.setPendingCalls(pending)
	return false, nil
}

// firstTurnPrompt renders the opening message of the chat model's
// conversation.
//
// The run reaches the chat model at a judgment point: the model has
// to know what the run is up against before it can choose.
func (s *Service) firstTurnPrompt(r *run) string {
	reason := r.judgmentReason()
	if reason == "" {
		reason = "the workflow cannot decide what comes next"
	}
	var b strings.Builder
	b.WriteString("The run needs a decision: ")
	b.WriteString(reason)
	b.WriteString("\n")
	if out, ok := r.lastOutcome(); ok {
		fmt.Fprintf(&b, "The last step ended as %q: %s\n", out.Status, out.Summary)
	}
	b.WriteString("Pick the next step, repair the current one, or finish the run.")
	return b.String()
}

// compact shortens the conversation for the model.
//
// Every tool result but the last one keeps its status and its
// summary; the transcript — the long part — is dropped. The latest
// result keeps everything, because that is the one the next decision
// is about.
func compact(msgs []llm.Message) []llm.Message {
	last := -1
	for i, m := range msgs {
		if m.Role == llm.RoleTool {
			last = i
		}
	}
	if last < 0 {
		return msgs
	}
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if i >= last || out[i].Role != llm.RoleTool {
			continue
		}
		// The parts are copied before they are rewritten: the
		// messages share their backing array with the run's
		// conversation, and the run keeps it.
		parts := make([]llm.Part, len(out[i].Parts))
		copy(parts, out[i].Parts)
		out[i].Parts = parts
		for j := range parts {
			if parts[j].Type != llm.PartToolResult {
				continue
			}
			parts[j].Text = compactResult(parts[j].Text)
		}
	}
	return out
}

// compactResult keeps the verdict of a tool result and drops the
// rest.
func compactResult(text string) string {
	var res stepResult
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		return text
	}
	short := stepResult{Status: res.Status, Summary: res.Summary}
	raw, err := json.Marshal(short)
	if err != nil {
		return text
	}
	return string(raw)
}

// wasNudged reports whether the chat model was already nudged for
// answering without a tool call.
func (r *run) wasNudged() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nudged
}

// markNudged records that the chat model was nudged.
func (r *run) markNudged() {
	r.mu.Lock()
	r.nudged = true
	r.mu.Unlock()
}
