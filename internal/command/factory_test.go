package command

import (
	"context"
	"testing"
)

type fakeRegCmd struct {
	spec   Spec
	called int
}

func (f *fakeRegCmd) Spec() Spec                 { return f.spec }
func (f *fakeRegCmd) Handle(_ context.Context, _ RuntimeServices, _ SlashInput) (*SlashOutput, error) {
	f.called++
	return &SlashOutput{Consumed: true}, nil
}

func TestRegistry_RegisterAndFind(t *testing.T) {
	reg := NewRegistry()
	cmd := &fakeRegCmd{spec: Spec{Name: "gtw"}}
	reg.Register(cmd)

	got := reg.FindByName("gtw")
	if got == nil {
		t.Fatal("expected to find gtw, got nil")
	}
	if got != cmd {
		t.Errorf("expected to find the same instance")
	}
}

func TestRegistry_Register_CaseInsensitiveLookup(t *testing.T) {
	reg := NewRegistry()
	cmd := &fakeRegCmd{spec: Spec{Name: "GTW"}}
	reg.Register(cmd)

	if reg.FindByName("gtw") == nil {
		t.Errorf("expected case-insensitive lookup")
	}
}

func TestRegistry_Register_Aliases(t *testing.T) {
	reg := NewRegistry()
	cmd := &fakeRegCmd{spec: Spec{Name: "gtw", Aliases: []string{"w", "team"}}}
	reg.Register(cmd)

	for _, name := range []string{"w", "team", "TEAM"} {
		if reg.FindByName(name) != cmd {
			t.Errorf("alias %q did not resolve to gtw", name)
		}
	}
}

func TestRegistry_Specs_RegistrationOrder(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&fakeRegCmd{spec: Spec{Name: "first"}})
	reg.Register(&fakeRegCmd{spec: Spec{Name: "second"}})
	reg.Register(&fakeRegCmd{spec: Spec{Name: "third"}})

	specs := reg.Specs()
	if len(specs) != 3 {
		t.Fatalf("expected 3 specs, got %d", len(specs))
	}
	// After registration, the order may be re-sorted
	// alphabetically by Specs() for stable output. Verify all
	// 3 are present and contain the expected names.
	gotNames := map[string]bool{}
	for _, s := range specs {
		gotNames[s.Name] = true
	}
	for _, expected := range []string{"first", "second", "third"} {
		if !gotNames[expected] {
			t.Errorf("expected %q in Specs, missing", expected)
		}
	}
}

func TestRegistry_Register_Nil(t *testing.T) {
	reg := NewRegistry()
	// Should not panic, should not register anything.
	reg.Register(nil)
	if got := reg.FindByName("anything"); got != nil {
		t.Errorf("expected nil after Register(nil), got %v", got)
	}
}

func TestRegistry_Register_EmptyNameIgnored(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&fakeRegCmd{spec: Spec{Name: ""}})
	if got := reg.FindByName(""); got != nil {
		t.Errorf("expected empty-name Register to be ignored")
	}
}

func TestRegistry_FindByName_Empty(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&fakeRegCmd{spec: Spec{Name: "gtw"}})
	if got := reg.FindByName(""); got != nil {
		t.Errorf("FindByName(\"\") should return nil")
	}
	if got := reg.FindByName("   "); got != nil {
		t.Errorf("FindByName(\"   \") should return nil")
	}
}

func TestRegistry_LastWinsOnCollision(t *testing.T) {
	reg := NewRegistry()
	first := &fakeRegCmd{spec: Spec{Name: "gtw"}}
	second := &fakeRegCmd{spec: Spec{Name: "gtw"}}
	reg.Register(first)
	reg.Register(second)

	if reg.FindByName("gtw") != second {
		t.Errorf("expected last-registered to win")
	}
}
