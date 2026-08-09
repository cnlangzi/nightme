package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// fakeCmd is a minimal SlashCommandFactory used to exercise the
// Commander dispatch logic. It records what Handle() was called
// with and returns a canned SlashOutput.
type fakeCmd struct {
	spec   Spec
	output *SlashOutput
	err    error
	got    *SlashInput
	calls  int
}

func (f *fakeCmd) Spec() Spec { return f.spec }
func (f *fakeCmd) Handle(_ context.Context, _ RuntimeServices, _ *chatsession.ChatSession, in SlashInput) (*SlashOutput, error) {
	f.got = &in
	f.calls++
	return f.output, f.err
}

func newFakeCmd(name string, summary string) *fakeCmd {
	return &fakeCmd{
		spec:   Spec{Name: name, Summary: summary, Usage: "usage-" + name},
		output: &SlashOutput{Reply: "ok-" + name, Consumed: true},
	}
}

func TestNewCommander_EmptyRegistry_NonCommandText(t *testing.T) {
	c := NewCommander(NewRegistry())
	cs := &chatsession.ChatSession{}
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, cs,
		SlashInput{Text: "hello world"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Errorf("non-slash text should report handled=false, got handled=true")
	}
	if got != nil {
		t.Errorf("non-slash text should return nil output, got %+v", got)
	}
}

func TestNewCommander_EmptyRegistry_SlashUnknown_FallsThrough(t *testing.T) {
	// Unknown slash commands report handled=true + Consumed=false so
	// the gateway can fall through to the agent loop, preserving the
	// existing passthrough behavior.
	c := NewCommander(NewRegistry())
	cs := &chatsession.ChatSession{}
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, cs, SlashInput{Text: "/nope arg1 arg2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Errorf("unknown /cmd should report handled=true (it WAS a slash command attempt), got handled=false")
	}
	if got == nil {
		t.Fatalf("unknown /cmd should return non-nil output (Consumed=false), got nil")
	}
	if got.Consumed {
		t.Errorf("unknown /cmd should report Consumed=false (gateway falls through to agent), got Consumed=true with reply %q", got.Reply)
	}
	if got.Reply != "" {
		t.Errorf("unknown /cmd should not produce a reply (gateway forwards to agent), got %q", got.Reply)
	}
}

func TestNewCommander_EmptyRegistry_SlashPath_FallsThrough(t *testing.T) {
	// /etc/passwd style input — slash command attempt that the
	// user might have intended as a file path. Should fall
	// through to the agent loop, NOT be rejected with
	// "Unknown command".
	c := NewCommander(NewRegistry())
	cs := &chatsession.ChatSession{}
	for _, text := range []string{
		"/etc/passwd",
		"/api/v1/foo",
		"/@everyone hi",
	} {
		got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, cs, SlashInput{Text: text})
		if err != nil {
			t.Fatalf("Dispatch(%q): %v", text, err)
		}
		if !handled {
			t.Errorf("Dispatch(%q): handled=false, want handled=true (it was a slash command attempt)", text)
		}
		if got == nil || got.Consumed {
			t.Errorf("Dispatch(%q): want Consumed=false (fall through), got %+v", text, got)
		}
	}
}

func TestNewCommander_RoutedToRegisteredCmd(t *testing.T) {
	reg := NewRegistry()
	gtw := newFakeCmd("gtw", "team workflow")
	reg.Register(gtw)

	c := NewCommander(reg)
	cs := &chatsession.ChatSession{}
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, cs, SlashInput{Text: "/gtw fix 42", Args: []string{"gtw", "fix", "42"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Errorf("known /cmd should report handled=true, got handled=false")
	}
	if !got.Consumed || got.Reply != "ok-gtw" {
		t.Errorf("expected routed to gtw reply, got %+v", got)
	}
	if gtw.calls != 1 {
		t.Errorf("expected gtw.Handle called once, got %d", gtw.calls)
	}
	if gtw.got == nil || gtw.got.Text != "/gtw fix 42" {
		t.Errorf("expected full text in SlashInput, got %+v", gtw.got)
	}
}

func TestNewCommander_AliasRoutesToPrimary(t *testing.T) {
	reg := NewRegistry()
	gtw := newFakeCmd("gtw", "team workflow")
	gtw.spec.Aliases = []string{"w"}
	reg.Register(gtw)

	c := NewCommander(reg)
	cs := &chatsession.ChatSession{}
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, cs, SlashInput{Text: "/w list"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Errorf("alias should report handled=true, got handled=false")
	}
	if !got.Consumed || got.Reply != "ok-gtw" {
		t.Errorf("alias /w should route to gtw, got %+v", got)
	}
	if gtw.calls != 1 {
		t.Errorf("expected gtw.Handle called once via alias, got %d", gtw.calls)
	}
}

func TestNewCommander_NoArgsStillFillsArgs(t *testing.T) {
	reg := NewRegistry()
	gtw := newFakeCmd("gtw", "")
	reg.Register(gtw)

	c := NewCommander(reg)
	// gateway did NOT pre-parse; Args is empty.
	cs := &chatsession.ChatSession{}
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, cs, SlashInput{Text: "/gtw fix 42"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Errorf("known /cmd should report handled=true")
	}
	if !got.Consumed {
		t.Errorf("expected Consumed, got %+v", got)
	}
	if gtw.got == nil {
		t.Fatalf("expected Handle called")
	}
	if len(gtw.got.Args) < 3 || gtw.got.Args[0] != "gtw" || gtw.got.Args[1] != "fix" || gtw.got.Args[2] != "42" {
		t.Errorf("expected Args=[gtw fix 42], got %v", gtw.got.Args)
	}
}

func TestNewCommander_CaseInsensitive(t *testing.T) {
	reg := NewRegistry()
	gtw := newFakeCmd("gtw", "")
	reg.Register(gtw)
	c := NewCommander(reg)

	cs := &chatsession.ChatSession{}
	got, handled, _ := c.Dispatch(context.Background(), RuntimeServices{}, cs, SlashInput{Text: "/GTW hi"})
	if !handled || !got.Consumed || got.Reply != "ok-gtw" {
		t.Errorf("expected /GTW to route to gtw, got handled=%v output=%+v", handled, got)
	}
}

func TestNewCommander_EmptyAfterSlash_FallsThrough(t *testing.T) {
	c := NewCommander(NewRegistry())
	cs := &chatsession.ChatSession{}
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, cs, SlashInput{Text: "/   trailing only"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// "/" alone or "/ " is not a slash command — fall through.
	if handled {
		t.Errorf("'/   ' (empty name) should report handled=false, got handled=true")
	}
	if got != nil {
		t.Errorf("'/   ' should return nil output, got %+v", got)
	}
}

func TestNewCommander_HandlerError_BecomesReply(t *testing.T) {
	// A command whose Handle returns an error should surface as
	// a Consumed=true reply (handled=true). The user gets a
	// visible error message; the gateway does NOT fall through.
	reg := NewRegistry()
	cmd := newFakeCmd("boom", "")
	cmd.err = errors.New("kaboom")
	reg.Register(cmd)

	c := NewCommander(reg)
	cs := &chatsession.ChatSession{}
	got, handled, dispatchErr := c.Dispatch(context.Background(), RuntimeServices{}, cs, SlashInput{Text: "/boom"})
	if dispatchErr != nil {
		t.Fatalf("unexpected error: %v", dispatchErr)
	}
	if !handled {
		t.Errorf("erroring command should report handled=true")
	}
	if got == nil || !got.Consumed {
		t.Fatalf("erroring command should return Consumed=true reply, got %+v", got)
	}
	if !strings.Contains(got.Reply, "kaboom") {
		t.Errorf("reply should contain the error message, got %q", got.Reply)
	}
}
