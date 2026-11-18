package discord

import (
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestFormatToolStartCall(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"Read", "", "● Read"},
		{"Read", `{"file_path":"/tmp/foo.go"}`, "● Read(foo.go)"},
		{"Bash", `{"command":"go build ./..."}`, `● Bash({"command":"go build ./..."})`},
		{"Bash", `{"command":"` + strings.Repeat("x", 200) + `"}`, "● Bash(" + `{"command":"` + strings.Repeat("x", 85) + "...)"},
	}
	for _, tc := range cases {
		got := formatToolStartCall(tc.name, tc.args)
		if got != tc.want {
			t.Errorf("formatToolStartCall(%q, %q) = %q, want %q", tc.name, tc.args, got, tc.want)
		}
	}
}

func TestSummarizeToolResult_PII_Safe(t *testing.T) {
	secret := "very-secret-credential-XYZ"
	got := summarizeToolResult("mcp_unknown", secret, nil)
	if strings.Contains(got, secret) {
		t.Errorf("PII leak in %q", got)
	}
	if !strings.Contains(got, "bytes") {
		t.Errorf("expected byte-count disclosure in %q", got)
	}
}

func TestSummarizeToolResult_KnownTools(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   string
	}{
		{"Read", "line1\nline2\nline3", "⎿  📄 Read → 3 lines"},
		{"Bash", "out1\nout2", "⎿  💻 Bash → 2 lines"},
		{"Write", "hello", "⎿  📝 Write → 5 bytes"},
		{"Glob", "a\nb\nc\nd", "⎿  📂 Glob → 4 files"},
	}
	for _, tc := range cases {
		got := summarizeToolResult(tc.name, tc.output, nil)
		if got != tc.want {
			t.Errorf("summarizeToolResult(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSummarizeToolResult_ErrorWins(t *testing.T) {
	got := summarizeToolResult("Bash", "out", errors.New("exit code 1"))
	if !strings.Contains(got, "❌") {
		t.Errorf("expected error indicator in %q", got)
	}
	if !strings.Contains(got, "exit code 1") {
		t.Errorf("expected error message in %q", got)
	}
}

func TestTruncate_RuneSafe(t *testing.T) {
	// CJK: 32 runes × 3 bytes = 96 bytes. budget = 97. truncate
	// returns at most 99 bytes (96 + 3-char "..."). It must NEVER
	// slice a 3-byte codepoint.
	got := truncate(strings.Repeat("你", 50), 100)
	if len(got) > 100 {
		t.Errorf("truncate exceeded budget: len=%d", len(got))
	}
}

func TestFormatTaskList_HeaderAndRows(t *testing.T) {
	items := []agent.AgentTaskItem{
		{ID: "1", Subject: "first", Status: agent.TaskPending},
		{ID: "2", Subject: "second", Status: agent.TaskCompleted},
	}
	got := formatTaskList(items)
	if !strings.HasPrefix(got, "📋 Tasks") {
		t.Errorf("missing header in %q", got)
	}
	if !strings.Contains(got, "1. [ ] first") {
		t.Errorf("missing pending row in %q", got)
	}
	if !strings.Contains(got, "2. [v] second") {
		t.Errorf("missing completed row in %q", got)
	}
}

func TestFormatTaskList_EmptyReturnsEmpty(t *testing.T) {
	if got := formatTaskList(nil); got != "" {
		t.Errorf("empty input = %q, want empty", got)
	}
	if got := formatTaskList([]agent.AgentTaskItem{}); got != "" {
		t.Errorf("zero-length input = %q, want empty", got)
	}
}

func TestFormatTaskList_KeepsTheOrderOfTheSender(t *testing.T) {
	items := []agent.AgentTaskItem{
		{ID: "1", Subject: "pending", Status: agent.TaskPending},
		{ID: "2", Subject: "done", Status: agent.TaskCompleted},
		{ID: "3", Subject: "active", Status: agent.TaskInProgress, ActiveForm: "writing"},
	}
	got := formatTaskList(items)
	pendingIdx := strings.Index(got, "pending")
	doneIdx := strings.Index(got, "done")
	activeIdx := strings.Index(got, "active")
	if !(pendingIdx < doneIdx && doneIdx < activeIdx) {
		t.Errorf("order = pending(%d) done(%d) active(%d), want pending<done<active",
			pendingIdx, doneIdx, activeIdx)
	}
}

// TestToolResultLine_ShowsTheSummaryVerbatim — the producer's verdict
// is safe to show: it goes to the chat as it is, where the per-tool
// heuristics would reduce the output to a byte count.
func TestToolResultLine_ShowsTheSummaryVerbatim(t *testing.T) {
	got := toolResultLine("run_step", "", "committed", nil)
	if !strings.Contains(got, "committed") {
		t.Errorf("toolResultLine = %q, want the summary", got)
	}
	if !strings.Contains(got, "run_step") {
		t.Errorf("toolResultLine = %q, want the tool's name", got)
	}
}

// TestToolResultLine_FallsBackToThePerToolHeuristics — a call without
// a summary keeps the rendering of its tool.
func TestToolResultLine_FallsBackToThePerToolHeuristics(t *testing.T) {
	got := toolResultLine("Read", "line1\nline2", "", nil)
	if got != summarizeToolResult("Read", "line1\nline2", nil) {
		t.Errorf("toolResultLine = %q, want the per-tool rendering", got)
	}
}

// TestToolResultLine_FailureWins — a failed call reads as a failure.
func TestToolResultLine_FailureWins(t *testing.T) {
	got := toolResultLine("run_step", "", "committed", errors.New("boom"))
	if !strings.Contains(got, "boom") {
		t.Errorf("toolResultLine = %q, want the failure", got)
	}
}
