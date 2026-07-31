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

// emitAskFromText is the text-fallback emit path used when
// AskUserQuestion was detected in plain assistant text (per
// detectAskInText). The struct shape matches the tool_use path so
// downstream consumers don't need a separate branch.
//
// The text fallback cannot recover a tool_use_id (no tool was
// actually invoked), so we synthesize a stable ID derived from the
// question header + text hash. The session.SendPermission answer
// will still be sent as a user-role message, but it will not be
// tied to any real tool_use. Claude Code will treat it as the
// next user turn after seeing the question text, which is the
// closest approximation we can produce without the tool surface.
func emitAskFromText(q Question, events chan<- agent.AgentEvent, logger *slog.Logger) {
	opts := make([]string, 0, len(q.Options)+1)
	for _, o := range q.Options {
		opts = append(opts, o.Label)
	}
	opts = append(opts, "Other")

	synthID := "text-fallback-" + strings.ReplaceAll(strings.ToLower(q.Header), " ", "-")
	if len(synthID) > 64 {
		synthID = synthID[:64]
	}

	events <- agent.AgentEvent{
		Kind: agent.EventPermission,
		Permission: &agent.PermissionRequest{
			Tool:       "AskUserQuestion",
			Action:     formatQuestionAction(q),
			Options:    opts,
			ResponseCh: make(chan string, 1),
		},
	}
	if logger != nil {
		logger.Info("claudecode: text-fallback AskUserQuestion emitted",
			"header", q.Header,
			"synth_id", synthID,
			"options", len(q.Options))
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
// AskUserQuestion is not exposed as a tool_use block (e.g. third-party
// model providers that strip Claude Code-specific tools from the
// system prompt). It scans EventText payloads for the markdown-table
// pattern Claude Code emits when the tool is unavailable.
//
// Detection heuristic (deliberately conservative — false positives
// would render a useless permission card):
//
//  1. The text must contain an explicit ask prompt (numbered list
//     with "pick one" / "please pick" / "select one" / etc.).
//  2. The text must contain a markdown table OR a numbered list
//     with at least 2 options.
//  3. The question line (the line just before the options block)
//     ends with '?'.
//
// Returns nil if any of the checks fail. We accept "Other" option
// always (added by the channel layer in the calling code).
func detectAskInText(text string) *Question {
	if text == "" {
		return nil
	}

	// Normalize whitespace for matching; preserve original for output.
	normalized := strings.TrimSpace(text)
	if normalized == "" {
		return nil
	}

	// 1. Must contain an explicit ask prompt.
	lower := strings.ToLower(normalized)
	askKeywords := []string{
		"please pick one",
		"pick one",
		"please select one",
		"select one",
		"please choose one",
		"choose one",
		"which would you like",
		"please select",
	}
	hasAskKeyword := false
	for _, kw := range askKeywords {
		if strings.Contains(lower, kw) {
			hasAskKeyword = true
			break
		}
	}
	if !hasAskKeyword {
		return nil
	}

	// 2. Extract options. Two patterns supported:
	//
	//    Pattern A — markdown table:
	//      | Option | Description |
	//      |---------|-------------|
	//      | **PostgreSQL** | Production |
	//      | **SQLite**      | Dev        |
	//
	//    Pattern B — numbered list:
	//      1. PostgreSQL - Production
	//      2. SQLite - Dev
	options := extractOptionsFromMarkdown(normalized)
	if len(options) < 2 {
		options = extractOptionsFromNumberedList(normalized)
	}
	if len(options) < 2 {
		return nil
	}

	// 3. Question line: take the line ending with '?' that precedes
	//    the first option. Fall back to a generic header if not found.
	question := extractQuestionBeforeOptions(normalized, options[0].Label)
	if question == "" {
		// Last-resort: any line ending with '?' in the first half.
		question = firstQuestionLine(normalized)
	}
	if question == "" {
		return nil
	}

	// Question must end with '?'.
	if !strings.HasSuffix(strings.TrimSpace(question), "?") {
		return nil
	}

	return &Question{
		Question:    strings.TrimSpace(question),
		Header:      headerFromQuestion(question),
		MultiSelect: false, // table fallback cannot determine; default false
		Options:     options,
	}
}

// extractOptionsFromMarkdown parses a 2-column markdown table whose
// first column starts with '**'. Returns up to 8 options (max
// expected from Claude Code's UI).
func extractOptionsFromMarkdown(text string) []Option {
	var out []Option
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := splitMarkdownRow(line)
		if len(cells) < 2 {
			continue
		}
		label := cleanMarkdownCell(cells[0])
		if label == "" {
			continue
		}
		// Skip header & separator rows.
		if strings.HasPrefix(strings.TrimSpace(cells[0]), "-") ||
			strings.EqualFold(strings.TrimSpace(cells[0]), "option") {
			continue
		}
		desc := cleanMarkdownCell(cells[1])
		out = append(out, Option{Label: label, Description: desc})
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// extractOptionsFromNumberedList parses lines like "1. PostgreSQL -
// Production". Returns up to 8 options.
func extractOptionsFromNumberedList(text string) []Option {
	var out []Option
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Match "N. label" or "N) label".
		var labelPart string
		for _, sep := range []string{". ", ") "} {
			if idx := strings.Index(line, sep); idx > 0 {
				digits := strings.TrimSpace(line[:idx])
				if isAllDigits(digits) {
					labelPart = line[idx+len(sep):]
					break
				}
			}
		}
		if labelPart == "" {
			continue
		}
		// Split on " - " or " — " for description.
		label := labelPart
		desc := ""
		for _, sep := range []string{" - ", " — ", " – "} {
			if idx := strings.Index(labelPart, sep); idx > 0 {
				label = strings.TrimSpace(labelPart[:idx])
				desc = strings.TrimSpace(labelPart[idx+len(sep):])
				break
			}
		}
		if label == "" {
			continue
		}
		out = append(out, Option{Label: label, Description: desc})
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// extractQuestionBeforeOptions finds the first sentence ending with
// '?' that appears before `firstOptionLabel` in the text.
func extractQuestionBeforeOptions(text, firstOptionLabel string) string {
	idx := strings.Index(text, firstOptionLabel)
	if idx < 0 {
		return ""
	}
	prefix := text[:idx]
	// Walk backwards from the end of prefix to find '?'.
	end := len(prefix)
	for end > 0 {
		q := strings.LastIndex(prefix[:end], "?")
		if q < 0 {
			break
		}
		// Get the line containing '?'.
		lineStart := strings.LastIndexAny(prefix[:q], "\n.!?")
		var line string
		if lineStart < 0 {
			line = prefix[:q+1]
		} else {
			line = prefix[lineStart+1 : q+1]
		}
		line = strings.TrimSpace(line)
		// Strip markdown emphasis (**bold**, *italic*, etc.).
		line = stripMarkdownEmphasis(line)
		if line == "" {
			// Try earlier '?' on the same line.
			end = q
			continue
		}
		return line
	}
	return ""
}

// firstQuestionLine returns the first non-empty line ending with '?'.
func firstQuestionLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, "?") && len(line) > 3 {
			return stripMarkdownEmphasis(line)
		}
	}
	return ""
}

// headerFromQuestion picks a short header from the question text.
// Strips common prefix words like "Which", "What", "Please select".
func headerFromQuestion(q string) string {
	q = strings.TrimSpace(q)
	q = strings.TrimSuffix(q, "?")
	for _, prefix := range []string{
		"Which ", "What ", "Please select ", "Please choose ", "Select ", "Choose ",
	} {
		q = strings.TrimPrefix(q, prefix)
	}
	// Take first 4 words or 30 chars, whichever is shorter.
	words := strings.Fields(q)
	if len(words) > 4 {
		words = words[:4]
	}
	header := strings.Join(words, " ")
	if len(header) > 30 {
		header = header[:30]
	}
	if header == "" {
		header = "Question"
	}
	return header
}

// --- markdown helpers ---

// splitMarkdownRow splits a single markdown table row by '|' into
// cells, dropping the leading and trailing empty cells produced by
// the pipes at row start/end.
func splitMarkdownRow(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	return strings.Split(row, "|")
}

// cleanMarkdownCell trims whitespace and strips leading/trailing
// pipe characters and bold markers (**text** -> text).
func cleanMarkdownCell(cell string) string {
	cell = strings.TrimSpace(cell)
	cell = strings.TrimPrefix(cell, "|")
	cell = strings.TrimSuffix(cell, "|")
	cell = strings.TrimSpace(cell)
	cell = stripMarkdownEmphasis(cell)
	return cell
}

// stripMarkdownEmphasis removes **bold**, *italic*, and `code`
// wrappers.
func stripMarkdownEmphasis(s string) string {
	for _, pair := range [][2]string{{"**", "**"}, {"*", "*"}, {"`", "`"}} {
		open, close := pair[0], pair[1]
		if strings.HasPrefix(s, open) && strings.HasSuffix(s, close) && len(s) >= 2*len(open) {
			s = s[len(open) : len(s)-len(close)]
		}
	}
	return s
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
