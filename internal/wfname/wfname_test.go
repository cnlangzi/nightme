package wfname

import (
	"slices"
	"testing"
)

// TestExpand_ANameWithoutAnOwnerIsTheProjectsOwn — "coder" is the
// workflow "nightme/coder".
func TestExpand_ANameWithoutAnOwnerIsTheProjectsOwn(t *testing.T) {
	if got := Expand("coder"); got != "nightme/coder" {
		t.Errorf("Expand(%q) = %q, want nightme/coder", "coder", got)
	}
}

// TestExpand_ANameThatCarriesAnOwnerIsAsItIs — the expansion only
// fills in an owner: every other name is handed back untouched.
func TestExpand_ANameThatCarriesAnOwnerIsAsItIs(t *testing.T) {
	for _, s := range []string{"nightme/coder", "acme/foo", ""} {
		if got := Expand(s); got != s {
			t.Errorf("Expand(%q) = %q, want it unchanged", s, got)
		}
	}
}

// TestNames_TheWorkflowOfThatNameComesFirst — a name without an owner
// is a workflow of its own, and the project's own namespace is the
// fallback.
func TestNames_TheWorkflowOfThatNameComesFirst(t *testing.T) {
	got := Names("coder")
	want := []string{"coder", "nightme/coder"}
	if !slices.Equal(got, want) {
		t.Errorf("Names(%q) = %v, want %v", "coder", got, want)
	}
}

// TestNames_ANameThatCarriesAnOwnerIsTheOnlyOne — a name that carries
// an owner answers to itself alone.
func TestNames_ANameThatCarriesAnOwnerIsTheOnlyOne(t *testing.T) {
	for _, s := range []string{"nightme/coder", "acme/foo", ""} {
		got := Names(s)
		if len(got) != 1 || got[0] != s {
			t.Errorf("Names(%q) = %v, want just itself", s, got)
		}
	}
}
