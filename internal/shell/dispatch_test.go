package shell

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestParseShell_Matrix locks in the 13-row normalization contract
// shared between shell.parseShell and commander.parseCommand. See
// wip/feat-shell.md §"防呆示例" for the authoritative table; if you
// change the rules, update both parsers AND both test matrices
// (this file + commander_test.go's TestParseCommand_Matrix) in
// lock-step.
func TestParseShell_Matrix(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantBody string
		wantOK   bool
	}{
		{"empty", "", "", false},
		{"whitespace_only", "   ", "", false},
		{"plain_text", "hello", "", false},
		{"half_bang_cmd", "!cmd", "cmd", true},
		{"full_width_bang_cmd", "！cmd", "cmd", true},
		{"leading_whitespace_bang", "   !cmd", "cmd", true},
		{"bang_followed_by_whitespace", "!   cmd", "cmd", true},
		{"lone_bang", "!", "", false},
		{"bang_only_whitespace", "!   ", "", false},
		{"first_char_is_slash", "/cmd", "", false}, // parseShell only handles !
		{"bang_inside_string", "echo !hi", "", false},
		{"fw_bang_with_trailing_space", "！  hi", "hi", true},
		{"tab_separated", "!cmd\tfix", "cmd\tfix", true}, // trim, not all whitespace-collapse
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotBody, gotOK := parseShell(tc.input)
			if gotBody != tc.wantBody {
				t.Errorf("parseShell(%q) body = %q, want %q", tc.input, gotBody, tc.wantBody)
			}
			if gotOK != tc.wantOK {
				t.Errorf("parseShell(%q) ok = %v, want %v", tc.input, gotOK, tc.wantOK)
			}
		})
	}
}

func TestDispatch_NonBangText_FallsThrough(t *testing.T) {
	// Plain text, slash command, full-width slash — none of these
	// are shell dispatches. Consumed must be false so the gateway
	// falls through to message dispatch.
	cases := []string{
		"hello",
		"/cmd",
		"／cmd",
		"   hello",
		"echo !hi", // bang not at start
	}
	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			r:= Dispatch(context.Background(), Request{Text: text, Cwd: t.TempDir()})
			
			if r.Consumed {
				t.Errorf("Dispatch(%q): expected Consumed=false, got true (reply=%q)", text, r.Reply)
			}
			if r.Reply != "" {
				t.Errorf("Dispatch(%q): expected empty Reply for fall-through, got %q", text, r.Reply)
			}
		})
	}
}

func TestDispatch_LoneBang_FallsThrough(t *testing.T) {
	// 防呆: ! alone or ! followed only by whitespace should not
	// dispatch. Returns Consumed=false so gateway can fall through.
	for _, text := range []string{"!", "!   ", "！", "！  "} {
		t.Run(text, func(t *testing.T) {
			r:= Dispatch(context.Background(), Request{Text: text, Cwd: t.TempDir()})
			
			if r.Consumed {
				t.Errorf("Dispatch(%q): lone bang should NOT consume (防呆), got reply=%q", text, r.Reply)
			}
		})
	}
}

func TestDispatch_EmptyCwd_FriendlyError(t *testing.T) {
	r:= Dispatch(context.Background(), Request{Text: "!ls", Cwd: ""})
	
	if !r.Consumed {
		t.Fatal("empty CWD should still be consumed (with friendly error)")
	}
	if !strings.Contains(r.Reply, "no CWD") {
		t.Errorf("expected friendly no-CWD message, got %q", r.Reply)
	}
}

func TestDispatch_EchoHello_StdoutAndSummary(t *testing.T) {
	if runtime.GOOS == "windows" {
		// sh is not available on Windows; the cmd /c path is
		// exercised by dispatch_windows_test.go separately.
		t.Skip("echo path uses sh -c; skip on Windows (covered by dispatch_windows_test.go)")
	}
	r:= Dispatch(context.Background(), Request{Text: "!echo hello", Cwd: t.TempDir()})
	
	if !r.Consumed {
		t.Fatal("expected Consumed=true")
	}
	if r.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", r.ExitCode)
	}
	if !strings.Contains(r.Stdout, "hello") {
		t.Errorf("expected stdout to contain 'hello', got %q", r.Stdout)
	}
	if !strings.Contains(r.Reply, "✅") {
		t.Errorf("expected summary to have ✅, got %q", r.Reply)
	}
	if !strings.Contains(r.Reply, "echo hello") {
		t.Errorf("expected summary to include command, got %q", r.Reply)
	}
}

func TestDispatch_False_ExitCodeOne_AndCrossMark(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("`false` is a Unix builtin; skip on Windows")
	}
	r:= Dispatch(context.Background(), Request{Text: "!false", Cwd: t.TempDir()})
	
	if r.ExitCode != 1 {
		t.Errorf("expected exit 1, got %d", r.ExitCode)
	}
	if !strings.Contains(r.Reply, "❌") {
		t.Errorf("expected summary to have ❌ on non-zero exit, got %q", r.Reply)
	}
	if strings.Contains(r.Reply, "✅") {
		t.Errorf("non-zero exit should NOT have ✅, got %q", r.Reply)
	}
}

func TestDispatch_NotFoundCommand_Exit127(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX exit-127 semantic; skip on Windows")
	}
	r:= Dispatch(context.Background(), Request{Text: "!definitely-not-a-real-command-xyzzy", Cwd: t.TempDir()})
	
	if r.ExitCode != 127 {
		t.Errorf("expected exit 127 for missing command, got %d", r.ExitCode)
	}
}

func TestDispatch_Pwd_MatchesCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("`pwd` is a Unix builtin; skip on Windows")
	}
	dir := t.TempDir()
	r:= Dispatch(context.Background(), Request{Text: "!pwd", Cwd: dir})
	
	if r.ExitCode != 0 {
		t.Fatalf("pwd failed: exit %d, stderr=%q", r.ExitCode, r.Stderr)
	}
	// On macOS /var is a symlink to /private/var — pwd may resolve
	// the real path. Compare EvalSymlinks on both sides so the test
	// works on both Linux and macOS.
	if !pathsEquivalent(r.Stdout, dir) {
		t.Errorf("pwd stdout %q does not match CWD %q", strings.TrimSpace(r.Stdout), dir)
	}
}

func TestDispatch_LongOutput_Truncated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("`seq` is a Unix utility; skip on Windows")
	}
	dir := t.TempDir()
	// seq 1 200 emits 200 lines, well over MaxStdoutLines=50.
	r:= Dispatch(context.Background(), Request{Text: "!seq 1 200", Cwd: dir})
	
	if !strings.Contains(r.Reply, "truncated") {
		t.Errorf("expected summary to mention truncation, got %q", r.Reply)
	}
	// Verify the head IS preserved (first line "1" should be in the card).
	if !strings.Contains(r.Reply, "\n  1\n") {
		t.Errorf("expected first line of truncated output to appear, got %q", r.Reply)
	}
}

func TestRenderSummary_SuccessShape(t *testing.T) {
	r := &Result{
		Consumed: true,
		Cmd:      "ls -la",
		Cwd:      "/tmp",
		ExitCode: 0,
		Duration: 23 * time.Millisecond,
		Stdout:   "file1\nfile2\n",
	}
	got := renderSummary(r)
	want := []string{"✅", "$ ls -la", "exit 0", "23ms", "/tmp", "stdout:", "  file1", "  file2"}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("renderSummary missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(got, "stderr:") {
		t.Errorf("renderSummary should not include stderr section when stderr empty:\n%s", got)
	}
}

func TestRenderSummary_FailureShape(t *testing.T) {
	r := &Result{
		Consumed: true,
		Cmd:      "false",
		Cwd:      "/tmp",
		ExitCode: 1,
		Duration: 5 * time.Millisecond,
		Stderr:   "boom\n",
	}
	got := renderSummary(r)
	for _, w := range []string{"❌", "$ false", "exit 1", "5ms", "stderr:", "  boom"} {
		if !strings.Contains(got, w) {
			t.Errorf("renderSummary missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(got, "✅") {
		t.Errorf("non-zero exit must not have ✅:\n%s", got)
	}
}

func TestRenderSummary_TruncationNotice(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= MaxStdoutLines+10; i++ {
		if i > 1 {
			b.WriteByte('\n')
		}
		b.WriteString(strings.Repeat("x", 5))
	}
	r := &Result{
		Consumed: true,
		Cmd:      "seq 1 60",
		Cwd:      "/tmp",
		ExitCode: 0,
		Stdout:   b.String(),
	}
	got := renderSummary(r)
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected truncation notice, got:\n%s", got)
	}
	if !strings.Contains(got, "first") {
		t.Errorf("expected 'first N of M lines' framing, got:\n%s", got)
	}
}

// pathsEquivalent compares two filesystem paths, resolving any
// symlinks first (relevant on macOS where t.TempDir() returns a
// /var/... path that pwd resolves to /private/var/...).
func pathsEquivalent(a, b string) bool {
	return evalLinks(a) == evalLinks(b)
}

func evalLinks(p string) string {
	if resolved, err := filepath.EvalSymlinks(strings.TrimSpace(p)); err == nil {
		return resolved
	}
	return strings.TrimSpace(p)
}