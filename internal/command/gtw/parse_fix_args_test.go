package gtw

import (
	"strings"
	"testing"
)

// TestParseFixArgs_YesFlag pins the F-XX `-y` / `--yes` flag:
// it's a positional-independent boolean flag — appears anywhere
// in argv, takes no value, "any --yes wins" (matches git's own
// CLI conventions). Threads through to RunFix.
func TestParseFixArgs_YesFlag(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want fixArgs
	}{
		// Remote mode
		{"yes after id", []string{"42", "-y"}, fixArgs{Mode: ModeRemote, RawArg: "42", Yes: true}},
		{"yes before id", []string{"-y", "42"}, fixArgs{Mode: ModeRemote, RawArg: "42", Yes: true}},
		{"yes long form", []string{"42", "--yes"}, fixArgs{Mode: ModeRemote, RawArg: "42", Yes: true}},
		{"yes long form before id", []string{"--yes", "42"}, fixArgs{Mode: ModeRemote, RawArg: "42", Yes: true}},
		{"default plan", []string{"42"}, fixArgs{Mode: ModeRemote, RawArg: "42", Yes: false}},

		// Local mode — `-y` is silently dropped by Factory.runFix,
		// but parseFixArgs still records Yes=true so downstream
		// code can make the call. (The Factory-level filter is
		// tested separately.)
		{"yes with local short", []string{"-n", "my-branch", "--yes"}, fixArgs{Mode: ModeLocal, RawArg: "my-branch", Yes: true}},
		{"yes with local long-name", []string{"--name", "my-branch", "-y"}, fixArgs{Mode: ModeLocal, RawArg: "my-branch", Yes: true}},

		// Multiple -y is idempotent
		{"duplicate yes", []string{"42", "-y", "--yes"}, fixArgs{Mode: ModeRemote, RawArg: "42", Yes: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseFixArgs(c.in)
			if err != nil {
				t.Fatalf("parseFixArgs(%v) error: %v", c.in, err)
			}
			if got.Mode != c.want.Mode {
				t.Errorf("Mode = %q, want %q", got.Mode, c.want.Mode)
			}
			if got.RawArg != c.want.RawArg {
				t.Errorf("RawArg = %q, want %q", got.RawArg, c.want.RawArg)
			}
			if got.Yes != c.want.Yes {
				t.Errorf("Yes = %v, want %v", got.Yes, c.want.Yes)
			}
		})
	}
}

// TestParseFixArgs_ForceFlagRejected pins the F-XX removal of
// --force / -f: those tokens now produce an explicit error
// rather than being silently accepted (which was the prior
// regression — `/gtw fix 42 --force` would parse successfully
// and dispatch as if --force was a no-op, leaving users who
// relied on the old flag in an inconsistent state).
func TestParseFixArgs_ForceFlagRejected(t *testing.T) {
	cases := [][]string{
		{"42", "--force"},
		{"--force", "42"},
		{"42", "-f"},
		{"-f", "42"},
		{"--force"},
		{"-f"},
		{"42", "-y", "--force"},
	}
	for _, in := range cases {
		t.Run(strings.Join(in, " "), func(t *testing.T) {
			got, err := parseFixArgs(in)
			if err == nil {
				t.Fatalf("parseFixArgs(%v) returned no error; want --force/-f rejected. got=%+v", in, got)
			}
			msg := err.Error()
			if !strings.Contains(msg, "removed in F-XX") && !strings.Contains(msg, "unknown flag") {
				t.Errorf("error message lacks F-XX context; got %q", msg)
			}
		})
	}
}

// TestParseFixArgs_UnknownFlagRejected pins the CLI-consistent
// behaviour: any token starting with "-" that isn't in the
// recognised list is hard-rejected. This protects against
// typos silently no-oping (e.g. `/gtw fix 42 --dry-run` would
// otherwise be parsed as a ModeRemote fix on issue 42 with
// `--dry-run` silently ignored).
func TestParseFixArgs_UnknownFlagRejected(t *testing.T) {
	cases := [][]string{
		{"42", "--dry-run"},
		{"--dry-run", "42"},
		{"42", "-d"},
		{"--foo"},
		{"42", "--bar=baz"},
		{"-y", "42", "--unknown"},
	}
	for _, in := range cases {
		t.Run(strings.Join(in, " "), func(t *testing.T) {
			_, err := parseFixArgs(in)
			if err == nil {
				t.Fatalf("parseFixArgs(%v) returned no error; want unknown flag rejected", in)
			}
			msg := err.Error()
			if !strings.Contains(msg, "unknown flag") {
				t.Errorf("error message lacks 'unknown flag'; got %q", msg)
			}
		})
	}
}

// TestParseFixArgs_LocalModeTooManyArgs pins the
// strict-arity check: `/gtw fix --name <branch> <extra>` is
// rejected (rather than silently dropping <extra>). This
// matches git CLI conventions.
func TestParseFixArgs_LocalModeTooManyArgs(t *testing.T) {
	cases := [][]string{
		{"--name", "my-branch", "extra"},
		{"-n", "my-branch", "extra", "-y"},
	}
	for _, in := range cases {
		t.Run(strings.Join(in, " "), func(t *testing.T) {
			_, err := parseFixArgs(in)
			if err == nil {
				t.Fatalf("parseFixArgs(%v) returned no error; want arity error", in)
			}
			if !strings.Contains(err.Error(), "exactly one argument") {
				t.Errorf("error message lacks 'exactly one argument'; got %q", err.Error())
			}
		})
	}
}

// TestParseFixArgs_RemoteModeTooManyArgs pins the same
// arity check for ModeRemote.
func TestParseFixArgs_RemoteModeTooManyArgs(t *testing.T) {
	cases := [][]string{
		{"42", "extra"},
		{"42", "extra", "-y"},
	}
	for _, in := range cases {
		t.Run(strings.Join(in, " "), func(t *testing.T) {
			_, err := parseFixArgs(in)
			if err == nil {
				t.Fatalf("parseFixArgs(%v) returned no error; want arity error", in)
			}
			if !strings.Contains(err.Error(), "exactly one argument") {
				t.Errorf("error message lacks 'exactly one argument'; got %q", err.Error())
			}
		})
	}
}

// TestParseFixArgs_MissingArgument pins the empty-argv /
// only-flags case (e.g. `/gtw fix -y` with no issue id and
// no --name).
func TestParseFixArgs_MissingArgument(t *testing.T) {
	cases := [][]string{
		{},
		{"-y"},
		{"--yes"},
		{"-y", "--yes"},
	}
	for _, in := range cases {
		t.Run(strings.Join(in, " "), func(t *testing.T) {
			_, err := parseFixArgs(in)
			if err == nil {
				t.Fatalf("parseFixArgs(%v) returned no error; want missing-argument error", in)
			}
			if !strings.Contains(err.Error(), "missing argument") {
				t.Errorf("error message lacks 'missing argument'; got %q", err.Error())
			}
		})
	}
}
