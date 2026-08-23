package agent

import (
	"context"
	"errors"
	"testing"
)

// fakeAgent is a minimal Starter implementation for testing the
// registry. After the Agent → Starter refactor it only implements
// the spec-half (Info + Detect) and a Start that always errors.
type fakeAgent struct {
	name string
	mode Mode
}

func (f *fakeAgent) Info() Info {
	return NewInfo(f.name, f.mode, "", nil, nil)
}
func (f *fakeAgent) Detect() error       { return nil }
func (f *fakeAgent) Start(context.Context, StartConfig) (*Agent, error) {
	return nil, errors.New("fakeAgent: Start not implemented")
}
func (f *fakeAgent) RunOnce(_ context.Context, _ StartConfig, _ []ContentBlock, _ ...RunOnceOption) (RunResult, error) {
	return RunResult{}, errors.New("fakeAgent: RunOnce not implemented")
}
func (f *fakeAgent) Review(_ context.Context, _ StartConfig, _ ...RunOnceOption) (RunResult, error) {
	return RunResult{}, ErrReviewNotSupported
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
	if got.Info().Mode != ModePTY {
		t.Fatalf("Info().Mode = %s, want pty", got.Info().Mode)
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
		seen[a.Info().Name] = true
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
	if got.Info().Mode != ModeSDK {
		t.Fatalf("Info().Mode = %s, want sdk (second registration)", got.Info().Mode)
	}
}

func TestListEmpty(t *testing.T) {
	r := New()
	if got := r.List(); len(got) != 0 {
		t.Fatalf("List() on empty registry = %d, want 0", len(got))
	}
}

// TestList_PreservesInsertionOrder verifies that List() returns
// starters in first-registration order — the invariant that
// config.LoadDefault auto-detection depends on. Re-Register of
// an existing name must NOT move the entry to the tail of the
// order slice, because that would silently shift the auto-detect
// priority chain every time a bridge package got re-imported.
//
// This test is the contract test for docs/primary-agent-detection.md
// — if it starts failing, every consumer of List() iteration order
// (auto-detect, /agents listing, etc.) is now non-deterministic.
func TestList_PreservesInsertionOrder(t *testing.T) {
	r := New()
	want := []string{"claude", "codex", "opencode", "pi"}

	// Deliberately out of alphabetical order to make a bug obvious.
	for _, n := range want {
		if replaced := r.Register(&fakeAgent{name: n, mode: ModeACP}); replaced {
			t.Fatalf("first Register(%q) should not report replacement", n)
		}
	}

	got := r.List()
	if len(got) != len(want) {
		t.Fatalf("List() returned %d agents, want %d", len(got), len(want))
	}
	for i, a := range got {
		if a.Info().Name != want[i] {
			t.Errorf("List()[%d] = %q, want %q (insertion order must be preserved)",
				i, a.Info().Name, want[i])
		}
	}

	// Re-register "codex" with a different mode — pointer must
	// update, but List() must keep "codex" at index 1.
	updated := &fakeAgent{name: "codex", mode: ModeSDK}
	if replaced := r.Register(updated); !replaced {
		t.Fatalf("re-Register(codex) should report replacement=true")
	}
	got = r.List()
	if len(got) != len(want) {
		t.Fatalf("List() after re-Register returned %d agents, want %d", len(got), len(want))
	}
	for i, n := range want {
		if got[i].Info().Name != n {
			t.Errorf("List()[%d] after re-Register = %q, want %q (replacement must not move entry)",
				i, got[i].Info().Name, n)
		}
	}
	// And the replacement pointer must actually be live.
	if got[1] != updated {
		t.Errorf("replaced entry pointer not updated in List()")
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
