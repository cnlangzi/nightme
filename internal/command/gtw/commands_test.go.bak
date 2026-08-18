package gtw

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// TestFactory_Spec covers the Factory.Spec() contract: the
// returned Spec is what command.Registry uses to build the
// dispatch table. Name + Aliases are the keys; Summary + Usage
// surface in /help.
func TestFactory_Spec(t *testing.T) {
	f := NewFactory(NewManager())
	s := f.Spec()
	if s.Name != "gtw" {
		t.Errorf("expected Name=gtw, got %q", s.Name)
	}
	if !contains(s.Aliases, "team") {
		t.Errorf("expected alias 'team' in %v", s.Aliases)
	}
	if s.Summary == "" {
		t.Errorf("expected non-empty Summary, got empty")
	}
	if !strings.Contains(s.Usage, "fix") {
		t.Errorf("expected Usage to mention 'fix' subcommand, got %q", s.Usage)
	}
}

// TestFactory_Handle_NoArgs covers the "no subcommand given"
// path: the Factory returns a usage hint (Consumed=true) so
// the user sees feedback instead of falling through to the
// agent loop.
//
// F-51 argv convention: commander.Dispatch prefixes Args
// with the command name, so production callers see
// Args = ["gtw", ...]. Args[1] is the subcommand slot —
// when only "gtw" is present (Args[1] out of range), the
// Factory returns the usage hint.
func TestFactory_Handle_NoArgs(t *testing.T) {
	cs := &chatsession.ChatSession{}
	f := NewFactory(NewManager())
	got, err := f.Handle(context.Background(),
		command.RuntimeServices{},
		cs,
		command.SlashInput{Text: "/gtw", Args: []string{"gtw"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Consumed {
		t.Errorf("expected Consumed=true for empty /gtw, got %+v", got)
	}
	if !strings.Contains(got.Reply, "fix") {
		t.Errorf("expected Reply to mention 'fix' (usage hint), got %q", got.Reply)
	}
}

// TestFactory_Handle_List covers the /gtw list subcommand
// with no drafts. Should reply with "(none in this chat)".
// (list / reset subcommands removed — see wip/gtw.md step 37.
// Manager.ListDrafts / Manager.Reset / Manager.DraftCount are
// still used by cmd/nightme/debug.go for the CLI debug
// interface.)

// TestFactory_Handle_Fix_NoArgs covers /gtw fix without an
// issue id. Should reply with a usage hint.
//
// F-51 argv convention: commander.Dispatch prefixes Args
// with the command name, so production callers see
// Args = ["gtw", "fix", "<id>", ...]. The subcommand lives
// at Args[1] and the subcommand's args start at Args[2].
func TestFactory_Handle_Fix_NoArgs(t *testing.T) {
	cs := &chatsession.ChatSession{}
	f := NewFactory(NewManager())
	got, _ := f.Handle(context.Background(), command.RuntimeServices{},
		cs,
	command.SlashInput{Text: "/gtw fix", Args: []string{"gtw", "fix"}})
	if !got.Consumed {
		t.Errorf("expected Consumed, got %+v", got)
	}
	if !strings.Contains(got.Reply, "fix") {
		t.Errorf("expected Reply to mention 'fix' (usage), got %q", got.Reply)
	}
}

// TestFactory_Handle_Fix_BadIssueID covers /gtw fix with a
// non-numeric id. Should reply with a hint.
func TestFactory_Handle_Fix_BadIssueID(t *testing.T) {
	cs := &chatsession.ChatSession{}
	f := NewFactory(NewManager())
	got, _ := f.Handle(context.Background(), command.RuntimeServices{},
		cs,
	command.SlashInput{Text: "/gtw fix abc", Args: []string{"gtw", "fix", "abc"}})
	if !got.Consumed {
		t.Errorf("expected Consumed, got %+v", got)
	}
	if !strings.Contains(got.Reply, "abc") {
		t.Errorf("expected Reply to mention 'abc' in error, got %q", got.Reply)
	}
}

// TestFactory_Handle_UnknownSubcommand covers the
// "/gtw bogus" path. Commander passes Args[0]="gtw" (the
// command name); the factory must look at Args[1] for the
// subcommand so the unknown-subcommand reply quotes "bogus"
// (not "gtw").
func TestFactory_Handle_UnknownSubcommand(t *testing.T) {
	cs := &chatsession.ChatSession{}
	f := NewFactory(NewManager())
	got, _ := f.Handle(context.Background(), command.RuntimeServices{},
		cs,
	command.SlashInput{Text: "/gtw bogus", Args: []string{"gtw", "bogus"}})
	if !got.Consumed {
		t.Errorf("expected Consumed, got %+v", got)
	}
	if !strings.Contains(got.Reply, "bogus") {
		t.Errorf("expected Reply to mention 'bogus', got %q", got.Reply)
	}
}

// contains is a tiny helper (Go 1.21+ has slices.Contains,
// but we want to be explicit and avoid the import).
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// --- F-XX tests for parseFixMode ---

// TestParseFixMode_BareID covers the legacy default path:
// bare numeric argv → ModeRemote + the numeric value.
func TestParseFixMode_BareID(t *testing.T) {
	mode, raw, err := parseFixMode([]string{"42"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mode != ModeRemote {
		t.Errorf("mode = %q, want %q", mode, ModeRemote)
	}
	if raw != "42" {
		t.Errorf("raw = %q, want %q", raw, "42")
	}
}

// TestParseFixMode_NameLong covers `--name <branch>`.
func TestParseFixMode_NameLong(t *testing.T) {
	mode, raw, err := parseFixMode([]string{"--name", "login-fix"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mode != ModeLocal {
		t.Errorf("mode = %q, want %q", mode, ModeLocal)
	}
	if raw != "login-fix" {
		t.Errorf("raw = %q, want %q", raw, "login-fix")
	}
}

// TestParseFixMode_NameShort covers `-n <branch>`.
func TestParseFixMode_NameShort(t *testing.T) {
	mode, raw, err := parseFixMode([]string{"-n", "x"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mode != ModeLocal {
		t.Errorf("mode = %q, want %q", mode, ModeLocal)
	}
	if raw != "x" {
		t.Errorf("raw = %q, want %q", raw, "x")
	}
}

// TestParseFixMode_NameMissingValue covers the
// `--name` (or `-n`) flag with no value following it.
func TestParseFixMode_NameMissingValue(t *testing.T) {
	for _, argv := range [][]string{{"--name"}, {"-n"}, {"--name", ""}, {"-n", "   "}} {
		if _, _, err := parseFixMode(argv); err == nil {
			t.Errorf("expected err for argv=%v, got nil", argv)
		}
	}
}

// TestParseFixMode_EmptyArgv covers the "no args at all" path.
func TestParseFixMode_EmptyArgv(t *testing.T) {
	if _, _, err := parseFixMode(nil); err == nil {
		t.Errorf("expected err for empty argv")
	}
	if _, _, err := parseFixMode([]string{}); err == nil {
		t.Errorf("expected err for empty argv")
	}
}
