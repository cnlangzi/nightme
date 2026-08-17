package feishu

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSummarizeToolEnd(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		args   string
		output string
		err    error
		want   string
	}{
		{
			name:   "read reports line count",
			tool:   "Read",
			args:   "/tmp/foo.go",
			output: "line1\nline2\nline3",
			want:   "📄 Read /tmp/foo.go → 3 lines",
		},
		{
			name:   "write reports byte count",
			tool:   "Write",
			args:   "/tmp/foo.go",
			output: "hello world",
			want:   "📝 Write /tmp/foo.go → 11 bytes",
		},
		{
			name: "edit reports applied",
			tool: "Edit",
			args: "/tmp/foo.go",
			want: "✏️ Edit /tmp/foo.go → applied",
		},
		{
			name:   "multiedit reports applied",
			tool:   "MultiEdit",
			args:   "/tmp/foo.go",
			output: "",
			want:   "✏️ MultiEdit /tmp/foo.go → applied",
		},
		{
			name:   "bash truncates long args and reports lines",
			tool:   "Bash",
			args:   strings.Repeat("a", 200),
			output: "alpha\nbeta",
			want:   "💻 Bash `" + strings.Repeat("a", 77) + "...` → 2 lines",
		},
		{
			name:   "grep reports matches and unique files",
			tool:   "Grep",
			output: "a.go:1:foo\nb.go:2:bar\nb.go:5:foo again",
			want:   "🔍 Grep → 3 matches across 2 files",
		},
		{
			name:   "glob reports file count",
			tool:   "Glob",
			output: "a.go\nb.go\nc.go",
			want:   "📂 Glob → 3 files",
		},
		{
			name:   "webfetch reports chars fetched",
			tool:   "WebFetch",
			args:   "https://example.com",
			output: strings.Repeat("x", 1234),
			want:   "🌐 WebFetch https://example.com → 1234 chars fetched",
		},
		{
			name:   "websearch reports result count",
			tool:   "WebSearch",
			args:   "go context",
			output: "r1\nr2\nr3\nr4\nr5",
			want:   `🔎 WebSearch "go context" → 5 results`,
		},
		{
			name:   "unknown tool truncates output to 200 bytes",
			tool:   "CustomTool",
			output: strings.Repeat("z", 250),
			want:   "🔧 CustomTool → " + strings.Repeat("z", 197) + "...",
		},
		{
			name: "err wins over output",
			tool: "Bash",
			err:  errors.New("exit code 1"),
			want: "❌ Bash failed: exit code 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeToolEnd(tc.tool, tc.args, tc.output, tc.err)
			if got != tc.want {
				t.Errorf("summarizeToolEnd = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCountLines(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"\n", 1},
		{"a", 1},
		{"a\n", 1},
		{"a\nb", 2},
		{"a\nb\n", 2},
		{"a\nb\nc", 3},
	}
	for _, c := range cases {
		if got := countLines(c.in); got != c.want {
			t.Errorf("countLines(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestCountUniqueFiles(t *testing.T) {
	got := countUniqueFiles("a.go:1:foo\nb.go:2:bar\nb.go:5:foo again")
	if got != 2 {
		t.Errorf("countUniqueFiles = %d, want 2", got)
	}
}

func TestTruncate(t *testing.T) {
	if truncate("abc", 10) != "abc" {
		t.Errorf("short input should pass through unchanged")
	}
	got := truncate("abcdef", 5)
	if got != "ab..." {
		t.Errorf("truncate = %q, want %q", got, "ab...")
	}
	if truncate("abc", 1) != "abc" {
		t.Errorf("max<=3 should short-circuit to s")
	}
	// P0-1 regression guard: never slice a multi-byte UTF-8
	// codepoint. Strict budget — output never exceeds max bytes.
	for _, tc := range []struct {
		name      string
		in        string
		max       int
		wantOut   string
		wantBytes int
	}{
		{"cjk strict", strings.Repeat("文", 60), 100, strings.Repeat("文", 32) + "...", 99},
		{"emoji strict", strings.Repeat("🔥", 30), 100, strings.Repeat("🔥", 24) + "...", 99},
		// 100 bytes of single-byte runes fits exactly.
		{"ascii exact fit", strings.Repeat("a", 100), 100, strings.Repeat("a", 100), 100},
		// 101 bytes truncates: 97 ASCII + "..." = 100 bytes.
		{"ascii over budget", strings.Repeat("a", 200), 100, strings.Repeat("a", 97) + "...", 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := truncate(tc.in, tc.max)
			if out != tc.wantOut {
				t.Errorf("truncate = %q, want %q", out, tc.wantOut)
			}
			if len(out) != tc.wantBytes {
				t.Errorf("len(truncate) = %d, want %d (strict budget)", len(out), tc.wantBytes)
			}
			if !utf8.ValidString(out) {
				t.Errorf("truncate returned invalid UTF-8: %q", out)
			}
		})
	}
}

// TestFormatToolStartCall_ClaudeStyle — Devin feedback 2026-08-04:
// tool display should match Claude Code's terminal UX, with
// rune-safe args truncation. Asserts:
//
//	● Tool(args)       when args fits
//	● Tool(args...)    when args exceeds toolCallArgsMaxBytes (CJK-safe)
//	● Tool             when args is empty
func TestFormatToolStartCall_ClaudeStyle(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args string
		want string
	}{
		{"short args", "Read", "/tmp/foo.go", "● Read(/tmp/foo.go)"},
		{"empty args", "Bash", "", "● Bash"},
		// ASCII 200 chars + budget=97 → "first 97 a's + ..." (100 bytes total).
		{"long ascii args truncates with ellipsis", "Bash",
			strings.Repeat("a", 200),
			"● Bash(" + strings.Repeat("a", 97) + "...)"},
		// CJK 60 chars (180 bytes) + budget=97 → 32 runes (96 bytes) + "..." = 99 bytes.
		{"long CJK args rune-safe truncate", "Read",
			strings.Repeat("文", 60), "● Read(" + strings.Repeat("文", 32) + "...)"},
		{"json read keeps basename", "read",
			`{"file_path":"/Users/geax/code/geax/github.com/cnlangzi/nightme.nightme/fix-dsh-tasks/docs/SPEC.md"}`,
			"● read(SPEC.md)"},
		{"json read keeps offset after long path", "read",
			`{"file_path":"/Users/geax/code/geax/github.com/cnlangzi/nightme.nightme/fix-dsh-tasks/docs/SPEC.md","offset":626}`,
			"● read(SPEC.md offset=626)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatToolStartCall(tc.tool, tc.args)
			if got != tc.want {
				t.Errorf("formatToolStartCall = %q, want %q", got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("formatToolStartCall returned invalid UTF-8: %q", got)
			}
		})
	}
}

// TestSummarizeToolResult_ClaudeStyle — companion to TestFormatToolStartCall.
// Result lines use ⎿  prefix (no args; args are on the preceding call
// line). Per-tool-type heuristics preserved from v0.2.
func TestSummarizeToolResult_ClaudeStyle(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		output string
		err    error
		want   string
	}{
		{"read reports line count", "Read", "line1\nline2\nline3", nil, "⎿  📄 Read → 3 lines"},
		{"write reports byte count", "Write", "hello world", nil, "⎿  📝 Write → 11 bytes"},
		{"edit reports applied", "Edit", "", nil, "⎿  ✏️  applied"},
		{"multiedit reports applied", "MultiEdit", "", nil, "⎿  ✏️  applied"},
		{"bash reports lines", "Bash", "alpha\nbeta", nil, "⎿  💻 Bash → 2 lines"},
		{"grep reports matches and unique files", "Grep",
			"a.go:1:foo\nb.go:2:bar\nb.go:5:foo again", nil,
			"⎿  🔍 Grep → 3 matches across 2 files"},
		{"glob reports file count", "Glob", "a.go\nb.go\nc.go", nil, "⎿  📂 Glob → 3 files"},
		{"webfetch reports chars fetched", "WebFetch", strings.Repeat("x", 1234), nil,
			"⎿  🌐 WebFetch → 1234 chars fetched"},
		{"websearch reports result count", "WebSearch", "r1\nr2\nr3\nr4\nr5", nil,
			"⎿  🔎 WebSearch → 5 results"},
		{"unknown tool reports byte count (PII-safe)", "CustomTool",
			strings.Repeat("z", 250), nil,
			"⎿  🔧 CustomTool → 250 bytes"},
		{"err wins over output", "Bash", "", errors.New("exit code 1"),
			"⎿  ❌ Bash failed: exit code 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeToolResult(tc.tool, tc.output, tc.err)
			if got != tc.want {
				t.Errorf("summarizeToolResult = %q, want %q", got, tc.want)
			}
		})
	}
}
