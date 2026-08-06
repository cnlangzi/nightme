package command

import (
	"context"
	"strings"
	"testing"
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
func (f *fakeCmd) Handle(_ context.Context, _ RuntimeServices, in SlashInput) (*SlashOutput, error) {
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
	got, err := c.Dispatch(context.Background(), RuntimeServices{},
		SlashInput{Text: "hello world"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Consumed {
		t.Errorf("non-slash text should fall through, got Consumed=true")
	}
}

func TestNewCommander_EmptyRegistry_SlashUnknown(t *testing.T) {
	c := NewCommander(NewRegistry())
	got, err := c.Dispatch(context.Background(), RuntimeServices{},
		SlashInput{Text: "/nope arg1 arg2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Consumed {
		t.Errorf("unknown /cmd should reply with hint (Consumed=true), got Consumed=false")
	}
	if !strings.Contains(got.Reply, "Unknown command") {
		t.Errorf("reply should mention unknown command, got %q", got.Reply)
	}
}

func TestNewCommander_RoutedToRegisteredCmd(t *testing.T) {
	reg := NewRegistry()
	gtw := newFakeCmd("gtw", "team workflow")
	reg.Register(gtw)

	c := NewCommander(reg)
	got, err := c.Dispatch(context.Background(), RuntimeServices{},
		SlashInput{Text: "/gtw fix 42", Args: []string{"gtw", "fix", "42"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
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
	got, err := c.Dispatch(context.Background(), RuntimeServices{},
		SlashInput{Text: "/w list"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
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
	got, err := c.Dispatch(context.Background(), RuntimeServices{},
		SlashInput{Text: "/gtw fix 42"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
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

	got, _ := c.Dispatch(context.Background(), RuntimeServices{},
		SlashInput{Text: "/GTW hi"})
	if !got.Consumed || got.Reply != "ok-gtw" {
		t.Errorf("expected /GTW to route to gtw, got %+v", got)
	}
}

func TestNewCommander_EmptyAfterSlash_FallsThrough(t *testing.T) {
	c := NewCommander(NewRegistry())
	got, err := c.Dispatch(context.Background(), RuntimeServices{},
		SlashInput{Text: "/   trailing only"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// "/" alone or "/ " is not a slash command — fall through.
	if got.Consumed {
		t.Errorf("'/   ' (empty name) should fall through, got Consumed=true")
	}
}
