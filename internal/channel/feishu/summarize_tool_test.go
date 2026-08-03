package feishu

import (
	"errors"
	"strings"
	"testing"
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
}
