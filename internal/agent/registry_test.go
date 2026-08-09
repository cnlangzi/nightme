package agent

import (
	"context"
	"errors"
	"testing"
)

// fakeAgent is a minimal Agent implementation for testing the
// registry. It does not spawn any process.
type fakeAgent struct {
	name string
	mode Mode
}

func (f *fakeAgent) Name() string        { return f.name }
func (f *fakeAgent) Mode() Mode          { return f.mode }
func (f *fakeAgent) Command() string     { return "" }
func (f *fakeAgent) Args() []string      { return nil }
func (f *fakeAgent) Env() []string       { return nil }
func (f *fakeAgent) Detect() error       { return nil }
func (f *fakeAgent) Start(context.Context, StartConfig) (Agent, error) {
	return nil, errors.New("fakeAgent: Start not implemented")
}
func (f *fakeAgent) Close() error        { return nil }
func (f *fakeAgent) Events() <-chan AgentEvent { return nil }
func (f *fakeAgent) PID() int            { return 0 }
func (f *fakeAgent) SendText(string) error     { return nil }
func (f *fakeAgent) SendBlocks(context.Context, []ContentBlock) error { return nil }
func (f *fakeAgent) SendPermission(string) error { return nil }
func (f *fakeAgent) New(context.Context) error { return nil }
func (f *fakeAgent) RunOnce(context.Context, StartConfig, []ContentBlock) (string, error) {
	return "", errors.New("fakeAgent: RunOnce not implemented")
}

func TestRegisterAndGet(t *testing.T) {
	r := New()
	a := &fakeAgent{name: "claude", mode: ModePTY}
	if replaced := r.Register(a); replaced {
		t.Fatalf("first Register should not report a replacement")
	}

	got, err := r.Get("claude")
	if err != nil {
		t.Fatalf("Get(claude) returned error: %v", err)
	}
	if got != a {
		t.Fatalf("Get(claude) returned a different pointer")
	}
	if got.Mode() != ModePTY {
		t.Fatalf("Mode() = %s, want pty", got.Mode())
	}
}

func TestUnknownAgent(t *testing.T) {
	r := New()
	if _, err := r.Get("nonexistent"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("Get(nonexistent) error = %v, want ErrUnknownAgent", err)
	}
}

func TestList(t *testing.T) {
	r := New()
	want := []string{"codex", "opencode", "claude"}
	for _, n := range want {
		_ = r.Register(&fakeAgent{name: n, mode: ModeACP})
	}

	got := r.List()
	if len(got) != len(want) {
		t.Fatalf("List() returned %d agents, want %d", len(got), len(want))
	}

	seen := make(map[string]bool, len(got))
	for _, a := range got {
		seen[a.Name()] = true
	}
	for _, n := range want {
		if !seen[n] {
			t.Errorf("List() missing agent %q", n)
		}
	}
}

func TestDuplicateRegistration(t *testing.T) {
	r := New()
	first := &fakeAgent{name: "claude", mode: ModePTY}
	second := &fakeAgent{name: "claude", mode: ModeSDK}

	if replaced := r.Register(first); replaced {
		t.Fatalf("first Register should not report replacement")
	}
	if replaced := r.Register(second); !replaced {
		t.Fatalf("second Register should report replacement")
	}

	got, err := r.Get("claude")
	if err != nil {
		t.Fatalf("Get(claude) returned error: %v", err)
	}
	if got != second {
		t.Fatalf("expected latest registration to win")
	}
	if got.Mode() != ModeSDK {
		t.Fatalf("Mode() = %s, want sdk (second registration)", got.Mode())
	}
}

func TestListEmpty(t *testing.T) {
	r := New()
	if got := r.List(); len(got) != 0 {
		t.Fatalf("List() on empty registry = %d, want 0", len(got))
	}
}

func TestModeString(t *testing.T) {
	cases := map[Mode]string{
		ModeACP: "acp",
		ModeSDK: "sdk",
		ModePTY: "pty",
	}
	for m, want := range cases {
		if got := m.String(); got != want {
			t.Errorf("Mode(%d).String() = %q, want %q", int(m), got, want)
		}
	}
	if got := Mode(99).String(); got != "mode(99)" {
		t.Errorf("unknown Mode().String() = %q, want mode(99)", got)
	}
}

func TestEventKindString(t *testing.T) {
	cases := map[EventKind]string{
		EventAgentText:       "text",
		EventAgentPermission: "permission",
		EventAgentToolStart:  "tool_start",
		EventAgentToolEnd:    "tool_end",
		EventAgentDone:       "done",
		EventAgentError:      "error",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("EventKind(%d).String() = %q, want %q", int(k), got, want)
		}
	}
	if got := EventKind(99).String(); got != "event(99)" {
		t.Errorf("unknown EventKind().String() = %q, want event(99)", got)
	}
}
