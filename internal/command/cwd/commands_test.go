package cwd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

func TestFactory_Spec(t *testing.T) {
	f := NewFactory(chatsession.NewManager(), "claude")
	s := f.Spec()
	if s.Name != "cwd" {
		t.Fatalf("Spec.Name = %q, want cwd", s.Name)
	}
}

func TestFactory_Handle_NoArgs_RepliesUsage(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr, "claude")
	input := command.SlashInput{ChatID: "c1", Args: []string{"cwd"}}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Usage: /cwd") {
		t.Fatalf("Reply missing usage: %q", out.Reply)
	}
}

func TestFactory_Handle_NonexistentPath_RejectsEarly(t *testing.T) {
	mgr := chatsession.NewManager()
	f := NewFactory(mgr, "claude")
	input := command.SlashInput{ChatID: "c1", Args: []string{"cwd", "/nonexistent-path-xyz"}}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, input)
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
	f := NewFactory(mgr, "claude")
	input := command.SlashInput{ChatID: "c1", Args: []string{"cwd", file}}

	out, err := f.Handle(context.Background(), command.RuntimeServices{}, input)
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
	f := NewFactory(mgr, "claude")

	input := command.SlashInput{ChatID: "c1", Args: []string{"cwd", tmp}}
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out.Reply, "Workspace set to") {
		t.Fatalf("Reply missing set-confirmation: %q", out.Reply)
	}
	if got := mgr.Get("c1").ActiveCwd(); got != tmp {
		t.Fatalf("ActiveCwd() = %q, want %q", got, tmp)
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