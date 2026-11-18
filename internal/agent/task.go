// The vocabulary of a checklist: the marker of a state and the text
// of a row.
//
// Both live in one place, so the channels that render a checklist
// cannot drift apart: a row is the same row in every channel.
package agent

import (
	"fmt"
	"strings"
)

// TaskStatusMarker returns the bracketed marker of a task status.
//
// The marker is the state of a row in a checklist, and every marker
// is one ASCII character between the brackets: the status column then
// has the same width on every row and the labels start at the same
// column. An emoji is two cells wide and would shift every row after
// it, so the markers are plain characters.
func TaskStatusMarker(status AgentTaskStatus) string {
	switch status {
	case TaskPending:
		return "[ ]"
	case TaskInProgress:
		return "[>]"
	case TaskCompleted:
		return "[v]"
	case TaskFailed:
		return "[!]"
	case TaskSkipped:
		return "[-]"
	case TaskCancelled:
		return "[x]"
	case TaskDeleted:
		return "[x]"
	}
	return "[ ]"
}

// FormatTaskRow renders one row of a checklist: the number of the
// row, the marker of its state, its subject and its detail.
//
// The number is part of the text, so the row is a markdown ordered
// list item and every channel shows the same row for the same item.
// The subject falls back to the id of the item when it is empty.
//
// The row never starts with "- ": Feishu's `lark_md` parses the
// leading "[ ]" of a todo row as a checkbox and then renders that row
// by other rules than the rest of the list.
func FormatTaskRow(index int, item AgentTaskItem) string {
	subject := strings.TrimSpace(item.Subject)
	if subject == "" {
		subject = strings.TrimSpace(item.ID)
	}
	row := fmt.Sprintf("%d. %s", index, TaskStatusMarker(item.Status))
	if subject != "" {
		row += " " + subject
	}
	if detail := strings.TrimSpace(item.ActiveForm); detail != "" {
		row += " (" + detail + ")"
	}
	return row
}
