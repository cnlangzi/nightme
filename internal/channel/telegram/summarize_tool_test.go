package telegram

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

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
	// Strict budget — output never exceeds max bytes.
	// Multi-byte UTF-8 codepoints are rune-safe (never sliced mid-sequence).
	for _, tc := range []struct {
		name      string
		in        string
		max       int
		wantOut   string
		wantBytes int
	}{
		{"cjk strict", strings.Repeat("文", 60), 100, strings.Repeat("文", 32) + "...", 99},
		{"emoji strict", strings.Repeat("🔥", 30), 100, strings.Repeat("🔥", 24) + "...", 99},
		{"ascii exact fit", strings.Repeat("a", 100), 100, strings.Repeat("a", 100), 100},
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

// TestFormatToolStartCall_ClaudeStyle — chain entry call line
// matches Claude Code's terminal UX, with rune-safe args truncation.
// Mirrors feishu's TestFormatToolStartCall_ClaudeStyle so the two
// channels produce visually identical tool entries.
func TestFormatToolStartCall_ClaudeStyle(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args string
		want string
	}{
		{"short args", "Read", "/tmp/foo.go", "● Read(/tmp/foo.go)"},
		{"empty args", "Bash", "", "● Bash"},
		{"long ascii args truncates with ellipsis", "Bash",
			strings.Repeat("a", 200),
			"● Bash(" + strings.Repeat("a", 97) + "...)"},
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

// TestSummarizeToolResult_ClaudeStyle — companion to
// TestFormatToolStartCall. Result lines use ⎿  prefix (no args;
// args are on the preceding call line).
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

// TestSummarizeToolLegCompat_FormatsMatchFeishu — guard against
// feishu/telegram helpers drifting apart. The two channels must
// produce byte-identical strings for the same inputs, otherwise
// chain snapshots diverge across channels and operator mental
// models split.
func TestSummarizeToolLegCompat_FormatsMatchFeishu(t *testing.T) {
	samples := []struct {
		tool, args string
	}{
		{"Read", "/tmp/foo.go"},
		{"Bash", "ls -la"},
		{"Edit", ""},
	}
	for _, s := range samples {
		call := formatToolStartCall(s.tool, s.args)
		// Same shape (● name(...)) as feishu's formatToolStartCall.
		if !strings.HasPrefix(call, "● ") {
			t.Errorf("call line missing `● ` prefix: %q", call)
		}
		result := summarizeToolResult(s.tool, "line1\nline2", nil)
		if !strings.HasPrefix(result, "⎿  ") {
			t.Errorf("result line missing `⎿  ` prefix: %q", result)
		}
	}
}
