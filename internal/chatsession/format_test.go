// F-42 tests for FormatKillResults / FormatResetResults.
//
// Pure-function tests — no ChatSession, no AgentSession, no Spawner.
// Output strings are asserted byte-for-byte to lock the publicly
// observable reply shape that gateway handlers + channels depend on.
package chatsession

import (
	"errors"
	"strings"
	"testing"
)

func TestFormatKillResults_Empty(t *testing.T) {
	got := FormatKillResults(nil)
	if got != "No active agents to kill." {
		t.Fatalf("empty results: want %q, got %q", "No active agents to kill.", got)
	}
	got = FormatKillResults([]KillResult{})
	if got != "No active agents to kill." {
		t.Fatalf("empty slice: want %q, got %q", "No active agents to kill.", got)
	}
}

func TestFormatKillResults_AllKilled(t *testing.T) {
	results := []KillResult{
		{Agent: "claude", Cwd: "/code/A", Action: "killed", BeforeState: StatusRunning},
		{Agent: "codex", Cwd: "/code/B", Action: "killed", BeforeState: StatusRunning},
	}
	got := FormatKillResults(results)
	if !strings.Contains(got, "Stopped 2 agent session(s):") {
		t.Errorf("missing header: %q", got)
	}
	if !strings.Contains(got, "✓ claude @ /code/A") {
		t.Errorf("missing row 1: %q", got)
	}
	if !strings.Contains(got, "✓ codex @ /code/B") {
		t.Errorf("missing row 2: %q", got)
	}
	if strings.Contains(got, "✗") || strings.Contains(got, "•") {
		t.Errorf("unexpected non-success marker: %q", got)
	}
}

func TestFormatKillResults_AllStale(t *testing.T) {
	results := []KillResult{
		{Agent: "claude", Cwd: "/code/A", Action: "stale-cleared", BeforeState: StatusExited},
		{Agent: "codex", Cwd: "/code/B", Action: "stale-cleared", BeforeState: StatusDetached},
	}
	got := FormatKillResults(results)
	if !strings.Contains(got, "Cleared 2 stale agent session(s) (no live processes):") {
		t.Errorf("missing header: %q", got)
	}
	if !strings.Contains(got, "• claude @ /code/A — already exited, entry cleaned") {
		t.Errorf("missing row 1: %q", got)
	}
	if !strings.Contains(got, "• codex @ /code/B — already exited, entry cleaned") {
		t.Errorf("missing row 2: %q", got)
	}
}

func TestFormatKillResults_Mixed(t *testing.T) {
	results := []KillResult{
		{Agent: "claude", Cwd: "/code/A", Action: "killed", BeforeState: StatusRunning},
		{Agent: "codex", Cwd: "/code/B", Action: "stale-cleared", BeforeState: StatusExited},
		{Agent: "pi", Cwd: "/code/C", Action: "killed", Error: errors.New("timeout"), BeforeState: StatusRunning},
	}
	got := FormatKillResults(results)
	if !strings.Contains(got, "Stopped 1, 1 stale entry cleared, 1 failed:") {
		t.Errorf("missing mixed header: %q", got)
	}
	if !strings.Contains(got, "✓ claude @ /code/A") {
		t.Errorf("missing killed row: %q", got)
	}
	if !strings.Contains(got, "• codex @ /code/B") {
		t.Errorf("missing stale row: %q", got)
	}
	if !strings.Contains(got, "✗ pi @ /code/C — kill: timeout") {
		t.Errorf("missing failed row: %q", got)
	}
}

func TestFormatKillResults_SortedAlphabetically(t *testing.T) {
	// Intentionally unsorted input. Output should be sorted by line.
	results := []KillResult{
		{Agent: "codex", Cwd: "/code/Z", Action: "killed", BeforeState: StatusRunning},
		{Agent: "claude", Cwd: "/code/A", Action: "killed", BeforeState: StatusRunning},
		{Agent: "pi", Cwd: "/code/M", Action: "killed", BeforeState: StatusRunning},
	}
	got := FormatKillResults(results)
	idxClaude := strings.Index(got, "✓ claude")
	idxCodex := strings.Index(got, "✓ codex")
	idxPi := strings.Index(got, "✓ pi")
	if !(idxClaude < idxCodex && idxCodex < idxPi) {
		t.Errorf("lines not sorted alphabetically: %q", got)
	}
}

func TestFormatKillResults_TruncatesAt20Lines(t *testing.T) {
	results := make([]KillResult, 25)
	for i := range results {
		results[i] = KillResult{
			Agent: "ag", Cwd: "/code/A", Action: "killed", BeforeState: StatusRunning,
		}
	}
	got := FormatKillResults(results)
	// Header + 20 rows + "and N more" = 22 lines
	lines := strings.Split(got, "\n")
	if len(lines) != 22 {
		t.Errorf("expected 22 lines, got %d: %q", len(lines), got)
	}
	if !strings.Contains(got, "... and 5 more") {
		t.Errorf("missing truncation tail: %q", got)
	}
}

func TestFormatResetResults_Empty(t *testing.T) {
	got := FormatResetResults(nil)
	if got != "Reset 0 sessions." {
		t.Fatalf("empty results: want %q, got %q", "Reset 0 sessions.", got)
	}
}

func TestFormatResetResults_AllRunning(t *testing.T) {
	results := []ResetResult{
		{Agent: "claude", Cwd: "/code/A", Action: "in-place-reset", BeforeState: StatusRunning},
		{Agent: "codex", Cwd: "/code/B", Action: "in-place-reset", BeforeState: StatusRunning},
	}
	got := FormatResetResults(results)
	if !strings.Contains(got, "Reset 2 session(s):") {
		t.Errorf("missing header: %q", got)
	}
	if !strings.Contains(got, "✓ claude @ /code/A — reset in-place") {
		t.Errorf("missing row 1: %q", got)
	}
	if !strings.Contains(got, "✓ codex @ /code/B — reset in-place") {
		t.Errorf("missing row 2: %q", got)
	}
}

func TestFormatResetResults_AllDead(t *testing.T) {
	results := []ResetResult{
		{Agent: "claude", Cwd: "/code/A", Action: "marked-fresh", BeforeState: StatusExited},
		{Agent: "codex", Cwd: "/code/B", Action: "marked-fresh", BeforeState: StatusDetached},
	}
	got := FormatResetResults(results)
	if !strings.Contains(got, "Marked 2 session(s) fresh for next spawn:") {
		t.Errorf("missing header: %q", got)
	}
	if !strings.Contains(got, "✓ claude @ /code/A — already exited, marked fresh") {
		t.Errorf("missing row 1: %q", got)
	}
}

func TestFormatResetResults_Mixed(t *testing.T) {
	results := []ResetResult{
		{Agent: "claude", Cwd: "/code/A", Action: "in-place-reset", BeforeState: StatusRunning},
		{Agent: "codex", Cwd: "/code/B", Action: "marked-fresh", BeforeState: StatusExited},
		{Agent: "pi", Cwd: "/code/C", Action: "in-place-reset", Error: errors.New("rpc down"), BeforeState: StatusRunning},
	}
	got := FormatResetResults(results)
	if !strings.Contains(got, "Reset 2 session(s), 1 reset in-place, 1 marked fresh, 1 failed:") {
		t.Errorf("missing mixed header: %q", got)
	}
	if !strings.Contains(got, "✓ claude @ /code/A — reset in-place") {
		t.Errorf("missing reset row: %q", got)
	}
	if !strings.Contains(got, "✓ codex @ /code/B — already exited, marked fresh") {
		t.Errorf("missing marked-fresh row: %q", got)
	}
	if !strings.Contains(got, "✗ pi @ /code/C — bridge reset: rpc down") {
		t.Errorf("missing failed row: %q", got)
	}
}

func TestFormatResetResults_TruncatesAt20Lines(t *testing.T) {
	results := make([]ResetResult, 25)
	for i := range results {
		results[i] = ResetResult{
			Agent: "ag", Cwd: "/code/A", Action: "marked-fresh", BeforeState: StatusExited,
		}
	}
	got := FormatResetResults(results)
	lines := strings.Split(got, "\n")
	if len(lines) != 22 {
		t.Errorf("expected 22 lines, got %d", len(lines))
	}
	if !strings.Contains(got, "... and 5 more") {
		t.Errorf("missing truncation tail: %q", got)
	}
}
