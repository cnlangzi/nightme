package wfe

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version is a version read from a version string.
//
// Only the numeric trunk of a string is a version: the major, the
// minor and the patch it names, and nothing else.
type Version struct {
	Major int
	Minor int
	Patch int
}

// String renders the version the way it is written: three numbers.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// compare tells which of two versions is the older one: -1, 0 or 1.
func (v Version) compare(other Version) int {
	switch {
	case v.Major != other.Major:
		return sign(v.Major - other.Major)
	case v.Minor != other.Minor:
		return sign(v.Minor - other.Minor)
	case v.Patch != other.Patch:
		return sign(v.Patch - other.Patch)
	}
	return 0
}

// sign turns a difference into -1, 0 or 1.
func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// ParseVersion reads the numeric trunk of a version string.
//
// The numeric trunk is the leading major[.minor[.patch]] of the
// string, with an optional "v" in front: "v0.9.1-12-gabc" is 0.9.1,
// "0.10" is 0.10.0. The parts that are not there are zero, and
// everything behind them — the suffix, the build segment — is
// ignored.
//
// A string that does not start with a number ("dev", a commit sha)
// has no version.
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Version{}, false
	}
	if s[0] == 'v' || s[0] == 'V' {
		s = s[1:]
	}
	parts := strings.Split(s, ".")
	out := Version{}
	nums := []*int{&out.Major, &out.Minor, &out.Patch}
	for i, part := range parts {
		if i >= len(nums) {
			break
		}
		n, ok := leadingNumber(part)
		if !ok {
			if i == 0 {
				return Version{}, false
			}
			break
		}
		*nums[i] = n
	}
	return out, true
}

// parseConstraintVersion reads the version of a comparison.
//
// It is the numeric trunk of the string, but every part that is
// written has to be a number: "0.10" is 0.10.0, "0.x.1" is not a
// version at all.
func parseConstraintVersion(s string) (Version, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Version{}, false
	}
	if s[0] == 'v' || s[0] == 'V' {
		s = s[1:]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		parts = parts[:3]
	}
	out := Version{}
	nums := []*int{&out.Major, &out.Minor, &out.Patch}
	for i, part := range parts {
		n, ok := leadingNumber(part)
		if !ok {
			return Version{}, false
		}
		*nums[i] = n
	}
	return out, true
}

// leadingNumber reads the digits at the front of s.
//
// The digits behind a non-digit do not belong to the number: "1-rc"
// is 1.
func leadingNumber(s string) (int, bool) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

// Constraint is a version constraint: the comparators a version has
// to satisfy.
//
// The comparators are joined by a comma, and all of them have to
// hold.
type Constraint struct {
	comparators []comparator
}

// comparator is one comparison of a constraint.
type comparator struct {
	op      string
	version Version
}

// The operators a constraint is written with.
var constraintOps = []string{">=", "<=", "!=", ">", "<", "="}

// ParseConstraint parses a version constraint.
//
// The grammar is:
//
//	constraint := comparator ("," comparator)*
//	comparator := (">=" | ">" | "<=" | "<" | "=" | "!=") version
//	version    := major ["." minor ["." patch]]
//
// An operator is required: a bare version would be an exact match,
// and a workflow that pins an exact version stops working on the next
// patch release. The operator may be separated from the version by
// spaces.
func ParseConstraint(s string) (Constraint, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return Constraint{}, errors.New("a constraint cannot be empty")
	}
	var comparators []comparator
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return Constraint{}, fmt.Errorf("invalid constraint %q: it has an empty comparison", s)
		}
		op, rest := splitOperator(part)
		if op == "" {
			return Constraint{}, fmt.Errorf("invalid constraint %q: %q has no operator (>=, >, <=, <, =, !=)", s, part)
		}
		if rest == "" {
			return Constraint{}, fmt.Errorf("invalid constraint %q: it needs a version", s)
		}
		version, ok := parseConstraintVersion(rest)
		if !ok {
			return Constraint{}, fmt.Errorf("invalid constraint %q: %q is not a version", s, rest)
		}
		comparators = append(comparators, comparator{op: op, version: version})
	}
	return Constraint{comparators: comparators}, nil
}

// splitOperator splits the operator off the front of a comparison.
//
// The operator is empty when the comparison does not start with one.
func splitOperator(s string) (string, string) {
	for _, op := range constraintOps {
		if strings.HasPrefix(s, op) {
			return op, strings.TrimSpace(s[len(op):])
		}
	}
	return "", s
}

// Allows reports whether the version satisfies the constraint.
//
// Only the numeric trunk of the version is compared: a version that
// carries a suffix ("v0.9.1-12-gabc") is the release it was built
// from. A version that has no numeric trunk at all ("dev", a commit
// sha) cannot be compared, and it satisfies every constraint: a
// version that cannot be read cannot refuse a workflow.
func (c Constraint) Allows(version string) bool {
	v, ok := ParseVersion(version)
	if !ok {
		return true
	}
	for _, cmp := range c.comparators {
		if !cmp.allows(v) {
			return false
		}
	}
	return true
}

// allows reports whether the version satisfies the comparator.
func (c comparator) allows(v Version) bool {
	switch c.op {
	case ">=":
		return v.compare(c.version) >= 0
	case ">":
		return v.compare(c.version) > 0
	case "<=":
		return v.compare(c.version) <= 0
	case "<":
		return v.compare(c.version) < 0
	case "=":
		return v.compare(c.version) == 0
	case "!=":
		return v.compare(c.version) != 0
	}
	return false
}

// String renders the constraint the way it is written: the operator
// and the version, separated by a space, the comparators joined by a
// comma.
func (c Constraint) String() string {
	parts := make([]string, 0, len(c.comparators))
	for _, cmp := range c.comparators {
		parts = append(parts, cmp.op+" "+cmp.version.String())
	}
	return strings.Join(parts, ", ")
}

// Check reports whether the given nightme version satisfies the
// requirement.
//
// A workflow that names no requirement is satisfied by every build.
// The error says what the workflow needs and what the build is.
func (r Requires) Check(current string) error {
	if strings.TrimSpace(r.Nightme) == "" {
		return nil
	}
	c, err := ParseConstraint(r.Nightme)
	if err != nil {
		return err
	}
	if c.Allows(current) {
		return nil
	}
	return fmt.Errorf("this workflow needs nightme %s; this nightme is %s", c, current)
}
