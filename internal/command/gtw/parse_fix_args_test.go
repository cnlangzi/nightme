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
	}
	for _, in := range cases {
		t.Run(strings.Join(in, " "), func(t *testing.T) {
			got, err := parseFixArgs(in)
			if err == nil {
				t.Fatalf("parseFixArgs(%v) returned no error; want --force/-f rejected. got=%+v", in, got)
			}
			// Error message must mention the removed flag name
			// so the user understands why their input failed
			// instead of just "missing argument" or similar.
			msg := err.Error()
			if !strings.Contains(msg, "removed in F-XX") && !strings.Contains(msg, "unknown flag") {
				t.Errorf("error message lacks F-XX context; got %q", msg)
			}
		})
	}
}
