package cwd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

func TestFactory_Spec(t *testing.T) {
	f := NewFactory()
	s := f.Spec()
	if s.Name != "cwd" {
		t.Fatalf("Spec.Name = %q, want cwd", s.Name)
	}
}

func TestFactory_Handle_NoArgs_RepliesUsage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	input := command.SlashInput{ChatID: "c1", Args: []string{"cwd"}}

	cs, _ := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, mgr, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Usage: /cwd") {
		t.Fatalf("Reply missing usage: %q", out.Reply)
	}
}

func TestFactory_Handle_NonexistentPath_RejectsEarly(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	input := command.SlashInput{ChatID: "c1", Args: []string{"cwd", "/nonexistent-path-xyz"}}

	cs, _ := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, mgr, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Path does not exist") {
		t.Fatalf("Reply missing not-exist: %q", out.Reply)
	}
}

func TestFactory_Handle_RegularFile_RejectsNotDirectory(t *testing.T) {
	// Create a regular file and try /cwd to it.
	tmp := t.TempDir()
	file := filepath.Join(tmp, "regular-file.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	mgr := chatsession.NewManager()
	f := NewFactory()
	input := command.SlashInput{ChatID: "c1", Args: []string{"cwd", file}}

	cs, _ := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, mgr, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Not a directory") {
		t.Fatalf("Reply missing not-a-directory: %q", out.Reply)
	}
}

func TestFactory_Handle_ValidDir_SetsActiveCwd(t *testing.T) {
	tmp := t.TempDir()
	mgr := chatsession.NewManager()
	f := NewFactory()

	input := command.SlashInput{ChatID: "c1", Args: []string{"cwd", tmp}}
	cs, _ := mgr.GetOrCreate(input.ChatID, "test")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, mgr, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Workspace set to") {
		t.Fatalf("Reply missing set-confirmation: %q", out.Reply)
	}
	if got := mgr.Get("c1").SelectedCwd(); got != tmp {
		t.Fatalf("SelectedCwd() = %q, want %q", got, tmp)
	}
}

// TestFactory_Handle_FullWidthPath_Normalised covers the
// IME-guard: a user with a Chinese/Japanese/Korean IME
// active might type "/cwd ／tmp" with a full-width slash.
// Without normalisation that path is treated as relative
// on Unix (filepath.IsAbs("／tmp") is false) and gets
// joined with $HOME — so on a default Linux box it would
// silently resolve to $HOME/／tmp instead of /tmp. With
// normalisation the full-width slash becomes '/', the
// path is classified as absolute, and we /cwd into /tmp.
//
// Unix-only: the test relies on `/tmp` being a real
// top-level path (the full-width slash is just a Unicode
// form of `/`). On Windows `/tmp` is not a real path so
// the post-resolve chdir fails. The IME-guard behaviour
// itself is platform-agnostic and covered by the unit
// tests in normalize_test.go / path_windows_test.go; this
// integration test verifies the end-to-end flow including
// the chdir.
func TestFactory_Handle_FullWidthPath_Normalised(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("test relies on /tmp existing; Unix-only")
	}
	// Use a directory that exists on every Unix-like test
	// box: /tmp. We pass it with a full-width slash so the
	// test exercises the normalisation path, then verify
	// resolvePath classifies it as absolute (no $HOME
	// prefix in the reply).
	mgr := chatsession.NewManager()
	f := NewFactory()

	input := command.SlashInput{ChatID: "c1", Args: []string{"cwd", "／tmp"}}
	cs, _ := mgr.GetOrCreate(input.ChatID, "test")

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, mgr, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Workspace set to") {
		t.Fatalf("Reply missing set-confirmation: %q", out.Reply)
	}
	// The reply should mention "/tmp" — NOT "$HOME/／tmp".
	if strings.Contains(out.Reply, "／") {
		t.Errorf("reply still contains full-width slash (normalisation missed): %q", out.Reply)
	}
}

// TestFactory_Handle_TooManyArgs_Rejected covers the
// multi-argument rejection. "/cwd foo bar" should fail
// with a clear "too many arguments" message instead of
// silently using only "foo". Wording comes from the shared
// command.ParseCmdArgs lexer (issue #291).
func TestFactory_Handle_TooManyArgs_Rejected(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	input := command.SlashInput{ChatID: "c1", Args: []string{"cwd", "first", "second"}}

	cs, _ := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, mgr, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "too many arguments") {
		t.Fatalf("expected 'too many arguments' in reply, got: %q", out.Reply)
	}
	if !strings.Contains(out.Reply, "Usage: /cwd") {
		t.Fatalf("expected usage hint in reply, got: %q", out.Reply)
	}
}

// TestFactory_Handle_MultilinePath_Rejected covers the
// defensive newline check. Even though gateway's parser
// normally splits on whitespace, an embedded \n in a
// quoted arg would still come through. We reject explicitly
// rather than passing it to os.Stat.
func TestFactory_Handle_MultilinePath_Rejected(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	input := command.SlashInput{ChatID: "c1", Args: []string{"cwd", "/tmp\n/etc"}}

	cs, _ := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, mgr, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "multiple lines") {
		t.Fatalf("expected 'multiple lines' rejection, got: %q", out.Reply)
	}
}

func TestExpandTilde(t *testing.T) {
	tests := []struct {
		in   string
		want string // empty = expect same-as-input
	}{
		{"", ""},
		{"~", ""}, // resolved to $HOME at runtime
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
	}
	for _, tc := range tests {
		got, err := expandTilde(tc.in)
		if err != nil {
			t.Fatalf("expandTilde(%q): %v", tc.in, err)
		}
		switch tc.want {
		case "":
			// For "~" expect $HOME; for others expect passthrough.
			if tc.in == "~" {
				home, _ := os.UserHomeDir()
				if got != home {
					t.Fatalf("expandTilde(%q) = %q, want HOME=%q", tc.in, got, home)
				}
			} else if got != tc.in {
				t.Fatalf("expandTilde(%q) = %q, want passthrough", tc.in, got)
			}
		default:
			if got != tc.want {
				t.Fatalf("expandTilde(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	}
}

// TestFactory_Handle_UnknownFlag_Rejected pins the option/arg
// split issue #291 asked for on /cwd. /cwd declares no flags, so
// a flag-shaped token is reported as an unknown flag rather than
// being resolved as a relative path under $HOME (pre-#291
// `/cwd --typo` produced a confusing "Path does not exist:
// $HOME/--typo").
func TestFactory_Handle_UnknownFlag_Rejected(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	input := command.SlashInput{ChatID: "c1", Args: []string{"cwd", "--force"}}

	cs, _ := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, mgr, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "unknown flag") {
		t.Fatalf("expected 'unknown flag' in reply, got: %q", out.Reply)
	}
	if cs.SelectedCwd() != "" {
		t.Fatalf("SelectedCwd = %q, want unchanged after a parse error", cs.SelectedCwd())
	}
}

// TestFactory_Handle_DashPathViaTerminator covers the payoff of
// the option/arg split: a directory whose name starts with "-"
// is reachable through the conventional `--` end-of-flags
// marker.
func TestFactory_Handle_DashPathViaTerminator(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory()
	dir := filepath.Join(t.TempDir(), "-dashdir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	input := command.SlashInput{ChatID: "c1", Args: []string{"cwd", "--", dir}}
	cs, _ := mgr.GetOrCreate(input.ChatID, "test")
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, mgr, cs, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Workspace set to") {
		t.Fatalf("expected workspace set, got: %q", out.Reply)
	}
	if cs.SelectedCwd() != dir {
		t.Fatalf("SelectedCwd = %q, want %q", cs.SelectedCwd(), dir)
	}
}
