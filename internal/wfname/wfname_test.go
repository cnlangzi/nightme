package wfname

import (
	"strings"
	"testing"
)

// TestExpand_TheShorthandIsTheOwnerAndTheName — "@coder" is the
// workflow "nightme/coder".
func TestExpand_TheShorthandIsTheOwnerAndTheName(t *testing.T) {
	name, err := Expand("@coder")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if name != "nightme/coder" {
		t.Errorf("the name is %q, want nightme/coder", name)
	}
}

// TestExpand_ANameThatIsNotTheShorthandIsAsItIs — the expansion only
// rewrites the shorthand: every other name is handed back untouched.
func TestExpand_ANameThatIsNotTheShorthandIsAsItIs(t *testing.T) {
	for _, s := range []string{"nightme/coder", "acme/foo", "fix", ""} {
		name, err := Expand(s)
		if err != nil {
			t.Errorf("Expand(%q): %v", s, err)
			continue
		}
		if name != s {
			t.Errorf("Expand(%q) = %q, want it unchanged", s, name)
		}
	}
}

// TestExpand_TheShorthandNeedsAName — the marker alone names nothing.
func TestExpand_TheShorthandNeedsAName(t *testing.T) {
	_, err := Expand("@")
	if err == nil {
		t.Fatal("the shorthand without a name was accepted")
	}
	if !strings.Contains(err.Error(), "needs a workflow name") {
		t.Errorf("the error is %q, want it to ask for a name", err)
	}
}

// TestExpand_TheShorthandTakesOneName — the shorthand names a
// workflow, not a path.
func TestExpand_TheShorthandTakesOneName(t *testing.T) {
	_, err := Expand("@a/b")
	if err == nil {
		t.Fatal("the shorthand with a path was accepted")
	}
	if !strings.Contains(err.Error(), "takes a workflow name only") {
		t.Errorf("the error is %q, want it to refuse the path", err)
	}
}
