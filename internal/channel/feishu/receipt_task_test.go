// F-38: task checklist renderer tests. Covers the in-progress
// / pending / completed ordering, the markdown todo list
// checkbox syntax (`- [ ]` / `- [x]`), the activeForm suffix on
// in-progress rows, the truncation footer for long lists, and
// the no-tasks case (must return nil so the caller skips the
// element entirely).
package feishu

import (
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestBuildTaskChecklistChunks_OrderingInProgressFirst asserts
// the renderer emits in-progress rows before pending rows, and
// pending rows before completed rows, with status-appropriate
// checkbox states.
func TestBuildTaskChecklistChunks_OrderingInProgressFirst(t *testing.T) {
	items := []agent.TaskItem{
		{ID: "1", Subject: "completed task", Status: agent.TaskCompleted},
		{ID: "2", Subject: "pending task", Status: agent.TaskPending},
		{ID: "3", Subject: "active task", Status: agent.TaskInProgress, ActiveForm: "writing"},
	}
	chunks := buildTaskChecklistChunks(items)
	if len(chunks) == 0 {
		t.Fatalf("expected at least one chunk, got 0")
	}
	rendered := chunks[0]
	ip := strings.Index(rendered, "active task")
	pn := strings.Index(rendered, "pending task")
	cn := strings.Index(rendered, "completed task")
	if ip < 0 || pn < 0 || cn < 0 {
		t.Fatalf("rendered output missing rows: %q", rendered)
	}
	if !(ip < pn && pn < cn) {
		t.Errorf("order wrong: in_progress=%d pending=%d completed=%d (want strict increasing)\n%s",
			ip, pn, cn, rendered)
	}
	// Markdown todo list syntax:
	//   - in_progress → "- [ ]" (open checkbox)
	//   - pending     → "- [ ]" (open checkbox)
	//   - completed   → "- [x]" (closed checkbox)
	if !strings.Contains(rendered, "- [ ] active task (writing)") {
		t.Errorf("in-progress line missing '- [ ]' checkbox + active form suffix: %q", rendered)
	}
	if !strings.Contains(rendered, "- [ ] pending task") {
		t.Errorf("pending line missing '- [ ]' checkbox: %q", rendered)
	}
	if !strings.Contains(rendered, "- [x] completed task") {
		t.Errorf("completed line missing '- [x]' checkbox: %q", rendered)
	}
	// The legacy glyphs MUST NOT appear anymore.
	for _, g := range []string{"⏳", "🔄", "✅"} {
		if strings.Contains(rendered, g) {
			t.Errorf("legacy glyph %q should not appear in todo list output: %q", g, rendered)
		}
	}
}

// TestBuildTaskChecklistChunks_LongListTruncates asserts a list
// that exceeds checklistBudgetRunes is truncated with a "…N
// 项任务已省略" tail appended to the LAST visible line.
func TestBuildTaskChecklistChunks_LongListTruncates(t *testing.T) {
	items := make([]agent.TaskItem, 80)
	for i := range items {
		items[i] = agent.TaskItem{
			ID:      itoaForTest(i),
			Subject: strings.Repeat("x", 80),
			Status:  agent.TaskCompleted,
		}
	}
	chunks := buildTaskChecklistChunks(items)
	if len(chunks) == 0 {
		t.Fatalf("expected at least one chunk, got 0")
	}
	last := chunks[len(chunks)-1]
	if !strings.Contains(last, "…") {
		t.Errorf("last chunk missing truncation tail: %q", last)
	}
	if !strings.HasSuffix(strings.TrimSpace(last), "…") {
		t.Errorf("last chunk missing trailing '…' suffix: %q", last)
	}
}

// TestBuildTaskChecklistChunks_EmptyListReturnsNil asserts the
// no-tasks case produces nil so the caller can skip the card
// element entirely (no empty section).
func TestBuildTaskChecklistChunks_EmptyListReturnsNil(t *testing.T) {
	if got := buildTaskChecklistChunks(nil); got != nil {
		t.Errorf("nil input = %v, want nil", got)
	}
	if got := buildTaskChecklistChunks([]agent.TaskItem{}); got != nil {
		t.Errorf("empty input = %v, want nil", got)
	}
}

// TestBuildTaskChecklistChunks_FilterTaskDeleted defensively
// removes TaskDeleted rows from the rendered output, even if a
// corrupt bridge snapshot still contains them.
func TestBuildTaskChecklistChunks_FilterTaskDeleted(t *testing.T) {
	items := []agent.TaskItem{
		{ID: "1", Subject: "live task", Status: agent.TaskPending},
		{ID: "2", Subject: "leaked deleted", Status: agent.TaskDeleted},
	}
	chunks := buildTaskChecklistChunks(items)
	if len(chunks) == 0 {
		t.Fatalf("expected at least one chunk, got 0")
	}
	rendered := chunks[0]
	if !strings.Contains(rendered, "live task") {
		t.Errorf("expected live task to render: %q", rendered)
	}
	if strings.Contains(rendered, "leaked deleted") {
		t.Errorf("deleted task leaked into render: %q", rendered)
	}
}

// itoaForTest is a small helper to avoid strconv import noise.
func itoaForTest(i int) string {
	if i == 0 {
		return "0"
	}
	const digits = "0123456789"
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = digits[i%10]
		i /= 10
	}
	return string(buf[pos:])
}
