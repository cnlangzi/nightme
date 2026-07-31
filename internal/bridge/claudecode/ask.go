package claudecode

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
)

// askHandlerFunc is the callback invoked when pumpStream detects an
// AskUserQuestion tool_use. Implementations translate the raw
// contentBlock into an EventPermission that the channel can render.
//
// nil means "do not handle AskUserQuestion" — the tool_use falls
// through to the default tool_use path (EventToolStart) and the
// channel will not see a permission prompt. This is a graceful
// degradation for environments where AskUserQuestion is not
// surfaced (e.g. certain non-Anthropic model providers).
type askHandlerFunc func(block contentBlock, events chan<- agent.AgentEvent, logger *slog.Logger)

// Question is the structured AskUserQuestion payload. We model only
// the fields nightme needs to render a Feishu card and reconstruct
// the user's answer in the right wire format.
//
// Schema source: chenhg5/cc-connect internal/agent/claudecode/
// claudecode_test.go:55-94 and Piebald-AI/claude-code-system-prompts
// tool-description-askuserquestion.md (ccVersion 2.1.154).
type Question struct {
	Question    string   `json:"question"`
	Header      string   `json:"header"`
	MultiSelect bool     `json:"multiSelect"`
	Options     []Option `json:"options"`
}

// Option is one selectable choice. Label is what the user sees and what
// gets written back as the answer (or one of the labels, for
// multiSelect). Description is shown alongside.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// defaultAskHandler emits an EventPermission with Options built from
// the question's option labels. The channel renders them as buttons
// (or a select widget for multiSelect). The user's choice is fed
// back via Session.SendPermission.
//
// We always include a synthetic "Other" option so the user can type
// a custom answer; Claude Code does this automatically in the CLI but
// the stream-json wire format doesn't expose it (cc-connect's parse
// confirms: see session.go:928-930).
func defaultAskHandler(block contentBlock, events chan<- agent.AgentEvent, logger *slog.Logger) {
	var input struct {
		Questions []Question `json:"questions"`
	}
	if err := json.Unmarshal(block.Input, &input); err != nil {
		if logger != nil {
			logger.Warn("claudecode: AskUserQuestion input parse failed",
				"err", err,
				"raw", string(block.Input))
		}
		return
	}
	if len(input.Questions) == 0 {
		if logger != nil {
			logger.Warn("claudecode: AskUserQuestion has empty questions")
		}
		return
	}

	for _, q := range input.Questions {
		if q.Question == "" {
			continue
		}
		if len(q.Options) < 2 {
			// Per Claude Code guidance (system-reminder-askuserquestion-
			// minimum-options-validation.md), questions with fewer than
			// 2 options are rejected. We log and skip rather than
			// render a broken prompt.
			if logger != nil {
				logger.Warn("claudecode: AskUserQuestion with <2 options, skipped",
					"question", q.Question,
					"options", len(q.Options))
			}
			continue
		}

		opts := make([]string, 0, len(q.Options)+1)
		for _, o := range q.Options {
			// Strip any "(Recommended)" suffix from the label; the
			// channel renders the first option as the recommended
			// one via UI convention rather than textual marker.
			label := strings.TrimSuffix(o.Label, " (Recommended)")
			label = strings.TrimSuffix(label, "(Recommended)")
			opts = append(opts, label)
		}
		// Always append "Other" — Claude Code's CLI surfaces a free-
		// text input alongside the options; we mirror that.
		opts = append(opts, "Other")

		events <- agent.AgentEvent{
			Kind: agent.EventPermission,
			Permission: &agent.PermissionRequest{
				Tool:    "AskUserQuestion",
				Action:  formatQuestionAction(q),
				Options: opts,
				// ResponseCh is buffered(1) — the bridge reads exactly
				// one response and proceeds. The session provides the
				// actual channel (set by Session.SendPermission).
				ResponseCh: make(chan string, 1),
			},
		}
	}
}

// formatQuestionAction renders a single question as a human-readable
// header for the permission card.
func formatQuestionAction(q Question) string {
	header := q.Header
	if header == "" {
		header = "Question"
	}
	multi := ""
	if q.MultiSelect {
		multi = " (multi-select)"
	}
	return fmt.Sprintf("[%s%s] %s", header, multi, q.Question)
}

// encodeUserAnswer constructs the JSON payload to write to Claude
// Code's stdin in response to one or more AskUserQuestion prompts.
//
// Per Anthropic API convention and cc-connect's session.go:943, the
// answer is a user-role message whose content is a tool_result block
// referencing the tool_use_id. The content field can be a string
// (single-select) or an array of strings (multi-select); we always
// emit the array form for multi-select questions because CHANGELOG
// entries for 2.1.x fixed a bug that previously discarded array
// answers.
//
// If qids is empty, returns an empty payload (no answers to send).
func encodeUserAnswer(toolUseID string, selected []string, multi bool) ([]byte, error) {
	if len(selected) == 0 {
		return []byte{}, nil
	}

	var content any
	if multi && len(selected) > 1 {
		content = selected // array form
	} else {
		// Single-select or single-pick multi: join with comma for
		// backwards compatibility with older Claude Code versions.
		content = strings.Join(selected, ",")
	}

	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": toolUseID,
					"content":     content,
				},
			},
		},
	}
	return json.Marshal(payload)
}

// detectAskInText is a TEXT-FALLBACK path for environments where
// AskUserQuestion is not exposed as a tool_use block (e.g. certain
// third-party model providers that strip Claude Code-specific tools
// from the system prompt). It scans EventText payloads for the
// "(pick one)" / numbered-option markdown pattern Claude Code emits
// when the tool is unavailable.
//
// Returns non-nil Question if a match is found; nil otherwise.
// Currently a stub — see F-24 §5.3 for the planned heuristic. We
// return nil here so callers don't false-positive on regular text;
// full detection is left for a follow-up.
func detectAskInText(_ string) *Question {
	return nil
}
