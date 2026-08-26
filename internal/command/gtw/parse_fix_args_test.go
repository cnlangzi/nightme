package gtw

import (
	"strings"
	"testing"
)

// TestParseFixArgs_YesFlag pins the F-XX `-y` / `--yes` flag:
// it can appear anywhere in argv and the Yes field threads
// through to RunFix.
func TestParseFixArgs_YesFlag(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want fixArgs
	}{
		{"yes after id", []string{"42", "-y"}, fixArgs{Mode: ModeRemote, RawArg: "42", Yes: true}},
		{"yes before id", []string{"-y", "42"}, fixArgs{Mode: ModeRemote, RawArg: "42", Yes: true}},
		{"yes long form", []string{"42", "--yes"}, fixArgs{Mode: ModeRemote, RawArg: "42", Yes: true}},
		{"default plan", []string{"42"}, fixArgs{Mode: ModeRemote, RawArg: "42", Yes: false}},
		{"yes with local", []string{"-n", "my-branch", "--yes"}, fixArgs{Mode: ModeLocal, RawArg: "my-branch", Yes: true}},
		{"yes with name and id both — yes wins", []string{"42", "-y"}, fixArgs{Mode: ModeRemote, RawArg: "42", Yes: true}},
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

// TestParseFixArgs_ForceFlagNotSilentlyAccepted pins the F-XX
// removal of --force / -f: those tokens no longer set any
// field. parseFixArgs itself doesn't error on them (they
// fall through parseFixMode unchanged), but the contract is
// "force must not silently set Yes=true". The higher-level
// reject path (Factory.runFix → parseIssueID) is covered by
// /gtw fix error cases elsewhere; here we only verify that
// the parser-level side effect is exactly zero.
func TestParseFixArgs_ForceFlagNotSilentlyAccepted(t *testing.T) {
	cases := [][]string{
		{"42", "--force"},
		{"--force", "42"},
		{"42", "-f"},
		{"-f", "42"},
	}
	for _, in := range cases {
		t.Run(strings.Join(in, " "), func(t *testing.T) {
			got, err := parseFixArgs(in)
			// parseFixArgs may or may not error (depends on
			// whether parseFixMode accepts the leading
			// non-digit). What MUST be true is that Yes is
			// never set.
			if err == nil && got.Yes {
				t.Errorf("parseFixArgs(%v) returned Yes=true; --force/-f must not be silently accepted", in)
			}
		})
	}
}
