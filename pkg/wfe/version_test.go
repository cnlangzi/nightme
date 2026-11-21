package wfe

import (
	"strings"
	"testing"
)

// TestParseConstraint_TheGrammar — a constraint is a comparison with a
// version, and a comma joins two of them.
func TestParseConstraint_TheGrammar(t *testing.T) {
	for _, src := range []string{
		">=0.10.0",
		">= 0.10.0",
		">=0.10.0, <0.12.0",
		">=0.10.0,<0.12.0",
		">0.10.0",
		"<=0.10.0",
		"=0.10.0",
		"!=0.10.0",
		">=0.10",
		">=0",
	} {
		if _, err := ParseConstraint(src); err != nil {
			t.Errorf("ParseConstraint(%q) = %v, want no error", src, err)
		}
	}
}

// TestParseConstraint_TheErrors — an operator is required, the version
// has to be a version, and the operator has to be one the grammar
// knows.
func TestParseConstraint_TheErrors(t *testing.T) {
	for _, src := range []string{
		"",
		"0.10.0",
		">=x.y.z",
		"^0.10.0",
		"~0.10.0",
		">= ",
		">=0.10.0,",
		">=0.10.0, ,<0.12.0",
	} {
		if _, err := ParseConstraint(src); err == nil {
			t.Errorf("ParseConstraint(%q) = no error, want one", src)
		}
	}
}

// TestConstraint_Allows — only the numeric trunk of a version is
// compared, and a version that cannot be read satisfies every
// constraint.
func TestConstraint_Allows(t *testing.T) {
	tests := []struct {
		constraint string
		version    string
		want       bool
	}{
		{constraint: ">=0.9.1", version: "v0.9.1-12-gabc", want: true},
		{constraint: ">=0.10.0", version: "0.10", want: true},
		{constraint: ">=0.10.0", version: "0.9.1", want: false},
		{constraint: ">=0.10.0", version: "0.10.0", want: true},
		{constraint: ">=0.10.0, <0.12.0", version: "0.11.0", want: true},
		{constraint: ">=0.10.0, <0.12.0", version: "0.12.0", want: false},
		{constraint: "<0.10.0", version: "0.9.1", want: true},
		{constraint: "<0.10.0", version: "0.10.0", want: false},
		{constraint: "=0.10.0", version: "0.10", want: true},
		{constraint: "=0.10.0", version: "0.10.1", want: false},
		{constraint: "!=0.10.0", version: "0.9.1", want: true},
		{constraint: "!=0.10.0", version: "0.10.0", want: false},
		{constraint: ">0.10.0", version: "0.10.0", want: false},
		{constraint: ">0.10.0", version: "0.10.1", want: true},
		// A version that cannot be read cannot refuse a workflow.
		{constraint: ">=0.10.0", version: "dev", want: true},
		{constraint: ">=0.10.0", version: "", want: true},
		{constraint: ">=0.10.0", version: "abc1234", want: true},
	}
	for _, tt := range tests {
		c, err := ParseConstraint(tt.constraint)
		if err != nil {
			t.Fatalf("ParseConstraint(%q): %v", tt.constraint, err)
		}
		if got := c.Allows(tt.version); got != tt.want {
			t.Errorf("constraint %q allows %q = %t, want %t", tt.constraint, tt.version, got, tt.want)
		}
	}
}

// TestConstraint_String — the constraint is written the way it is read.
func TestConstraint_String(t *testing.T) {
	tests := []struct {
		constraint string
		want       string
	}{
		{constraint: ">=0.10.0", want: ">= 0.10.0"},
		{constraint: ">= 0.10.0", want: ">= 0.10.0"},
		{constraint: ">=0.10.0, <0.12.0", want: ">= 0.10.0, < 0.12.0"},
		{constraint: ">=0.10", want: ">= 0.10.0"},
	}
	for _, tt := range tests {
		c, err := ParseConstraint(tt.constraint)
		if err != nil {
			t.Fatalf("ParseConstraint(%q): %v", tt.constraint, err)
		}
		if got := c.String(); got != tt.want {
			t.Errorf("constraint %q renders as %q, want %q", tt.constraint, got, tt.want)
		}
	}
}

// TestParseVersion_TheNumericTrunk — the numeric trunk of a version
// string is its leading numbers, and a string that does not start with
// a number has no version.
func TestParseVersion_TheNumericTrunk(t *testing.T) {
	tests := []struct {
		src       string
		wantMajor int
		wantMinor int
		wantPatch int
		wantOK    bool
	}{
		{src: "0.9.1", wantMajor: 0, wantMinor: 9, wantPatch: 1, wantOK: true},
		{src: "v0.9.1-12-gabc", wantMajor: 0, wantMinor: 9, wantPatch: 1, wantOK: true},
		{src: "0.10", wantMajor: 0, wantMinor: 10, wantPatch: 0, wantOK: true},
		{src: "1.2.3.4", wantMajor: 1, wantMinor: 2, wantPatch: 3, wantOK: true},
		{src: "dev"},
		{src: ""},
		{src: "abc1234"},
	}
	for _, tt := range tests {
		got, ok := ParseVersion(tt.src)
		if ok != tt.wantOK {
			t.Errorf("ParseVersion(%q) ok = %t, want %t", tt.src, ok, tt.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Major != tt.wantMajor || got.Minor != tt.wantMinor || got.Patch != tt.wantPatch {
			t.Errorf("ParseVersion(%q) = %v, want %d.%d.%d", tt.src, got,
				tt.wantMajor, tt.wantMinor, tt.wantPatch)
		}
	}
}

// TestRequires_Check — the error says what the workflow needs and what
// the build is.
func TestRequires_Check(t *testing.T) {
	req := Requires{Nightme: ">=0.10.0"}
	err := req.Check("0.9.1")
	if err == nil {
		t.Fatal("Check() = no error, want one")
	}
	want := "this workflow needs nightme >= 0.10.0; this nightme is 0.9.1"
	if err.Error() != want {
		t.Errorf("Check() = %q, want %q", err.Error(), want)
	}
	if err := req.Check("0.10.1"); err != nil {
		t.Errorf("Check() = %v, want no error", err)
	}
	// A workflow that names no requirement is satisfied by every build.
	if err := (Requires{}).Check("0.9.1"); err != nil {
		t.Errorf("Check() = %v, want no error", err)
	}
}

// TestRequires_CheckOfAnEmptyConstraint — an empty constraint is no
// requirement.
func TestRequires_CheckOfAnEmptyConstraint(t *testing.T) {
	if err := (Requires{Nightme: "  "}).Check("0.9.1"); err != nil {
		t.Errorf("Check() = %v, want no error", err)
	}
}

// TestRequires_TheConstraintOfARequirementIsRead — the requirement of
// the nightme is the constraint the file names.
func TestRequires_TheConstraintOfARequirementIsRead(t *testing.T) {
	req := Requires{Nightme: ">=0.10.0"}
	c, err := ParseConstraint(req.Nightme)
	if err != nil {
		t.Fatalf("ParseConstraint: %v", err)
	}
	if !strings.Contains(c.String(), "0.10.0") {
		t.Errorf("the constraint renders as %q, want it to name 0.10.0", c.String())
	}
}
