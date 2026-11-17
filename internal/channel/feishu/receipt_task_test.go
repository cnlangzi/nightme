// F-38: task checklist renderer tests. Covers the order of the rows
// (the order the sender listed them in), the state markers, the
// detail suffix on every row that has one, the truncation footer for
// long lists, and the no-tasks case (must return nil so the caller
// skips the element entirely).
package feishu

import (
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestBuildTaskChecklistChunks_KeepsTheOrderOfTheSender asserts the
// renderer emits the rows in the order the sender listed them in: the
// order is what makes a plan readable, and every row carries the
// marker of its own state.
func TestBuildTaskChecklistChunks_KeepsTheOrderOfTheSender(t *testing.T) {
	items := []agent.AgentTaskItem{
		{ID: "1", Subject: "completed task", Status: agent.TaskCompleted, ActiveForm: "ok: it landed"},
		{ID: "2", Subject: "pending task", Status: agent.TaskPending},
		{ID: "3", Subject: "active task", Status: agent.TaskInProgress, ActiveForm: "writing"},
	}
	chunks := buildTaskChecklistChunks(items)
	if len(chunks) == 0 {
		t.Fatalf("expected at least one chunk, got 0")
	}
	rendered := chunks[0]
	cn := strings.Index(rendered, "completed task")
	pn := strings.Index(rendered, "pending task")
	an := strings.Index(rendered, "active task")
	if cn < 0 || pn < 0 || an < 0 {
		t.Fatalf("rendered output missing rows: %q", rendered)
	}
	if !(cn < pn && pn < an) {
		t.Errorf("order wrong: completed=%d pending=%d in_progress=%d (want strict increasing)\n%s",
			cn, pn, an, rendered)
	}
	// Every row carries the marker of its state, and the detail of
	// the row:
	//   - pending     → "- [ ]"
	//   - in_progress → "- [>]"
	//   - completed   → "- [√]"
	if !strings.Contains(rendered, "- [√] completed task (ok: it landed)") {
		t.Errorf("completed line missing its marker or detail: %q", rendered)
	}
	if !strings.Contains(rendered, "- [ ] pending task") {
		t.Errorf("pending line missing its marker: %q", rendered)
	}
	if !strings.Contains(rendered, "- [>] active task (writing)") {
		t.Errorf("in-progress line missing its marker or detail: %q", rendered)
	}
	// The legacy glyphs MUST NOT appear anymore.
	for _, g := range []string{"⏳", "🔄", "✅"} {
		if strings.Contains(rendered, g) {
			t.Errorf("legacy glyph %q should not appear in todo list output: %q", g, rendered)
		}
	}
}

// TestBuildTaskChecklistChunks_RendersEveryState asserts a snapshot
// that holds a failed, a cancelled and a skipped row renders all of
// them: a step that did not get done must not disappear, and it must
// not read like a step that has not started.
func TestBuildTaskChecklistChunks_RendersEveryState(t *testing.T) {
	items := []agent.AgentTaskItem{
		{ID: "1", Subject: "failed task", Status: agent.TaskFailed, ActiveForm: "failed: the remote refused"},
		{ID: "2", Subject: "cancelled task", Status: agent.TaskCancelled, ActiveForm: "cancelled"},
		{ID: "3", Subject: "skipped task", Status: agent.TaskSkipped, ActiveForm: "skipped"},
	}
	chunks := buildTaskChecklistChunks(items)
	if len(chunks) == 0 {
		t.Fatalf("expected at least one chunk, got 0")
	}
	rendered := chunks[0]
	for _, want := range []string{
		"- [!] failed task (failed: the remote refused)",
		"- [-] cancelled task (cancelled)",
		"- [/] skipped task (skipped)",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the row %q is missing: %q", want, rendered)
		}
	}
}

// TestBuildTaskChecklistChunks_LongListTruncates asserts a list
// that exceeds checklistBudgetRunes is truncated with a "…N
// 项任务已省略" tail appended to the LAST visible line.
func TestBuildTaskChecklistChunks_LongListTruncates(t *testing.T) {
	items := make([]agent.AgentTaskItem, 80)
	for i := range items {
		items[i] = agent.AgentTaskItem{
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
	if got := buildTaskChecklistChunks([]agent.AgentTaskItem{}); got != nil {
		t.Errorf("empty input = %v, want nil", got)
	}
}

// TestBuildTaskChecklistChunks_FilterTaskDeleted defensively
// removes TaskDeleted rows from the rendered output, even if a
// corrupt bridge snapshot still contains them.
func TestBuildTaskChecklistChunks_FilterTaskDeleted(t *testing.T) {
	items := []agent.AgentTaskItem{
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
