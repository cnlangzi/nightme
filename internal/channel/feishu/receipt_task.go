// F-38: task checklist rendering and budget accounting.
//
// buildTaskChecklistChunks turns the receipt's latest typed task
// snapshot into one or more markdown chunks, each kept within
// divTextCharLimit so the buildReceiptCard loop can map each
// chunk to its own <markdown> card element. Multiple chunks
// share the 50-element budget — Feishu's hard limit — so a long
// list is still rendered correctly without truncation.
//
// The renderer emits a standard markdown todo list so Feishu's
// lark_md parser renders each row as a checkbox:
//   - [ ]  → pending / in-progress task (open checkbox)
//   - [x]  → completed task (checked checkbox)
// In-progress rows also append the optional `ActiveForm` phrase
// in a soft-grey suffix (Feishu `lark_md` ignores backticks in
// the middle of a line, so plain parens are used). The
// in-progress / pending / completed display order is a Feishu-
// local decision; other Channels are free to pick their own
// ordering and presentation. The generic status enum
// (agent.AgentTaskStatus) is the only input the renderer reads.
package feishu

import (
	"strings"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
)

// checklistMore is appended to the last visible line when the
// input did not fit within the budget. Pure ellipsis — no
// label — keeps the visual shape of a single todo row and
// avoids mixed-language suffixes (the user's locale may not be
// the same as the source-code author).
const checklistMore = "…"

// checklistOverflowPlaceholder is the single-line fallback used
// when the renderer has to drop every line to fit the budget.
// It is a single todo row so the user still sees a checkbox
// block and the receipt body stays a well-formed markdown todo
// list.
const checklistOverflowPlaceholder = "- [ ] …"

// checklistBudgetRunes is the maximum total length of the
// rendered checklist (rune count, summed across all chunks).
// Picked to comfortably fit inside the Feishu 24 KB body budget
// for the worst-case 1 task × 200 chars subject × CJK triple-byte
// envelope, with plenty of slack for the icon and a few tasks.
// Long task lists are truncated (in-progress first, then pending,
// then completed).
const checklistBudgetRunes = 4000

// buildTaskChecklistChunks renders the receipt's tasks slice
// into one or more markdown chunks suitable for Feishu <div>
// elements. Each returned chunk is at most divTextCharLimit
// runes; the caller maps each chunk to its own card element.
//
// The output is empty when the input is empty so the caller can
// detect the "no checklist" state and skip the element entirely.
// The total rune count across all chunks is guaranteed to fit
// within checklistBudgetRunes.
func buildTaskChecklistChunks(items []agent.AgentTaskItem) []string {
	if len(items) == 0 {
		return nil
	}
	// Stable partition by status so the user's eye lands on
	// the currently-active task first. Within each bucket we
	// keep the bridge-supplied order (the bridge has its own
	// insertion-order tracking) for a deterministic render.
	buckets := make(map[agent.AgentTaskStatus][]int, 3)
	for i, it := range items {
		// Filter TaskDeleted defensively: the bridge is supposed
		// to remove deleted tasks before emitting, but a corrupt
		// snapshot must not produce a visible "deleted" row.
		switch it.Status {
		case agent.TaskInProgress, agent.TaskPending, agent.TaskCompleted:
			buckets[it.Status] = append(buckets[it.Status], i)
		}
	}
	order := append(append([]int{}, buckets[agent.TaskInProgress]...), buckets[agent.TaskPending]...)
	order = append(order, buckets[agent.TaskCompleted]...)

	// Render the visible lines in order. The line collector
	// tracks how many bytes the budget has consumed; once the
	// next line would push it past checklistBudgetRunes the
	// remainder is dropped and a footer is appended to the last
	// chunk.
	lines := make([]string, 0, len(order))
	total := 0
	rendered := 0
	for _, idx := range order {
		line := renderTaskLine(items[idx])
		cost := utf8.RuneCountInString(line) + 1 // +1 for the joining newline
		if total+cost > checklistBudgetRunes {
			break
		}
		lines = append(lines, line)
		total += cost
		rendered++
	}
	if rendered == 0 {
		return []string{checklistOverflowPlaceholder}
	}
	omitted := len(order) - rendered

	// Join the rendered lines with newlines and split into
	// per-element chunks that each respect divTextCharLimit.
	// splitMarkdownForDivs already preserves code blocks / list
	// atomicity; our checklist is a list of `- [ ]` / `- [x]`
	// task lines, all of which share the same list item shape
	// and are trivially paragraph-safe to split.
	//
	// F-42: prepend the markdown section header `**📋 Tasks**`
	// so the checklist is visually distinct from the surrounding
	// reply body. The title is added here (after the line budget
	// is enforced) so the title itself can never push a real
	// task line past `checklistBudgetRunes` — the divider is
	// purely cosmetic, the lines below it are the user-visible
	// content. Title is unconditional: it shows whether the
	// checklist stands alone (no OutReply) or coexists with
	// reply entries, keeping the section shape consistent.
	const checklistHeader = "**📋 Tasks**"
	joined := checklistHeader + "\n\n" + joinLines(lines)
	chunks := splitMarkdownForDivs(joined, divTextCharLimit)
	if len(chunks) == 0 {
		// Defensive: splitMarkdownForDivs returns [] on empty
		// input. joinLines never returns empty when lines is
		// non-empty, so this branch is unreachable in practice.
		return nil
	}
	if omitted > 0 {
		// Append an inline "…" tail to the LAST visible line so
		// the markdown list shape is preserved. Feishu lark_md
		// leaves it as a plain inline suffix.
		chunks[len(chunks)-1] += " " + checklistMore
	}
	return chunks
}

// joinLines concatenates lines with newlines, trimming the
// trailing newline so the output is paragraph-shaped (matches
// splitMarkdownForDivs's expectations).
func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	// Two-pass string builder: pre-size the buffer to the
	// exact total so we don't pay the O(n) grow cost on
	// large checklists.
	size := 0
	for _, l := range lines {
		size += len(l) + 1
	}
	buf := make([]byte, 0, size)
	for i, l := range lines {
		if i > 0 {
			buf = append(buf, '\n')
		}
		buf = append(buf, l...)
	}
	return string(buf)
}

// renderTaskLine builds one row of the markdown todo list. The
// status enum decides only the checkbox state and the (optional)
// activeForm suffix; the row shape is identical for every status
// so the output reads as a single coherent list.
//
//   - pending      → - [ ] Subject
//   - in_progress  → - [ ] Subject (ActiveForm)         (open checkbox + grey note)
//   - completed    → - [x] Subject
func renderTaskLine(it agent.AgentTaskItem) string {
	checkbox := "- [ ]"
	if it.Status == agent.TaskCompleted {
		checkbox = "- [x]"
	}
	subject := strings.TrimSpace(it.Subject)
	if subject == "" {
		subject = it.ID
	}
	line := checkbox + " " + subject
	if it.Status == agent.TaskInProgress && it.ActiveForm != "" {
		line += " (" + it.ActiveForm + ")"
	}
	return line
}

