package gtw

import (
	"strings"
	"testing"
)

// assertParseErr fails the test unless err is non-nil and its
// message contains substr (the kind of error we expect) AND
// carries the spec's Usage tail (the contract — every parse
// error tells the user what the command accepts). It also
// guards against process-record leakage so the user-facing
// reply stays current-state only.
func assertParseErr(t *testing.T, err error, substr string) string {
	t.Helper()
	if err == nil {
		t.Fatalf("expected parse error containing %q, got nil", substr)
	}
	msg := err.Error()
	if !strings.Contains(msg, substr) {
		t.Errorf("error %q lacks %q", msg, substr)
	}
	if !strings.Contains(msg, fixCmdSpec.Usage) {
		t.Errorf("error %q does not carry the spec Usage tail", msg)
	}
	// Anti-leak: user-facing error text must reflect current
	// state, not process records.
	for _, banned := range []string{"F-XX", "docs/feat/", "removed in", "see docs/"} {
		if strings.Contains(msg, banned) {
			t.Errorf("error %q leaks process record %q", msg, banned)
		}
	}
	return msg
}

// TestParseFixArgs_YesFlag pins the `-y` / `--yes` flag:
// positional-independent boolean, "any --yes wins". Threads
// through to RunFix for the Plan/Execute mode split.
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
		// code can make the call. (Factory-level filter is
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

// TestParseFixArgs_ForceFlagRejected pins that --force / -f are
// rejected as unknown flags by the shared lexer. The error
// must carry the recognised-flags hint AND the spec Usage
// (the user learns what is accepted now).
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
			assertParseErr(t, err, "unknown flag")
		})
	}
}

// TestParseFixArgs_UnknownFlagRejected pins the CLI-consistent
// behaviour: any token starting with "-" that isn't in the
// recognised set is hard-rejected with the spec Usage tail.
// This protects against typos silently no-oping (e.g.
// `/gtw fix 42 --dry-run` would otherwise be parsed as a
// ModeRemote fix on issue 42 with `--dry-run` silently
// ignored).
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
			assertParseErr(t, err, "unknown flag")
		})
	}
}

// TestParseFixArgs_LocalModeTooManyArgs pins the strict-arity
// check: `--name <branch> <extra>` is rejected (rather than
// silently dropping <extra>). Matches git CLI conventions.
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
			assertParseErr(t, err, "too many arguments")
		})
	}
}

// TestParseFixArgs_RemoteModeTooManyArgs pins the same arity
// check for ModeRemote.
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
			assertParseErr(t, err, "too many arguments")
		})
	}
}

// TestParseFixArgs_MissingArgument pins the empty-argv /
// only-flags case (e.g. `/gtw fix -y` with no issue id and
// no --name). The shared lexer passes (no arity violation
// with MinArgs=0); parseFixArgs catches it in the post-parse
// step.
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
			assertParseErr(t, err, "needs <issue-id> or --name <branch>")
		})
	}
}

// TestParseFixArgs_NameAndPositionalMutuallyExclusive pins the
// post-parse mutual-exclusion check: <issue-id> and --name
// <branch> cannot both be supplied. /gtw fix-specific; the
// shared lexer can't enforce it because it doesn't know
// which positional pattern a command accepts.
func TestParseFixArgs_NameAndPositionalMutuallyExclusive(t *testing.T) {
	cases := [][]string{
		{"--name", "my-branch", "42"},   // --name + extra positional
		{"42", "--name", "my-branch"},   // positional first, --name second
		{"-n", "my-branch", "42", "-y"}, // -n + extra + yes
	}
	for _, in := range cases {
		t.Run(strings.Join(in, " "), func(t *testing.T) {
			_, err := parseFixArgs(in)
			if err == nil {
				t.Fatalf("parseFixArgs(%v) returned no error; want mutual-exclusion error", in)
			}
			assertParseErr(t, err, "not both")
		})
	}
}

// TestParseFixArgs_NameValueFlagShaped pins git CLI
// behaviour: a value-taking option consumes the next token
// as its value even if that token starts with "-". This is
// what makes `--name -foo` (a branch literally named "-foo")
// work, and what makes `--name foo bar` (multi-token value)
// NOT silently work — only the immediately-following token is
// the value, and the next token starts a new arg.
func TestParseFixArgs_NameValueFlagShaped(t *testing.T) {
	// `--name -foo` → local mode, branch literally named "-foo"
	got, err := parseFixArgs([]string{"--name", "-foo"})
	if err != nil {
		t.Fatalf("parseFixArgs unexpected error: %v", err)
	}
	if got.Mode != ModeLocal || got.RawArg != "-foo" {
		t.Errorf("--name -foo = %+v, want ModeLocal RawArg=-foo", got)
	}
}

// TestParseFixArgs_NameMissingValue pins the
// required-value-missing error: `--name` with no following
// token (or with the following token already consumed as a
// flag's value) is a hard error.
func TestParseFixArgs_NameMissingValue(t *testing.T) {
	cases := [][]string{
		{"--name"}, // no value at all
		{"-n"},     // no value at all (short form)
		{"-y", "--name"},
	}
	for _, in := range cases {
		t.Run(strings.Join(in, " "), func(t *testing.T) {
			_, err := parseFixArgs(in)
			if err == nil {
				t.Fatalf("parseFixArgs(%v) returned no error; want 'missing value for' error", in)
			}
			assertParseErr(t, err, "missing value")
		})
	}
}

// TestParseFixArgs_PositionalOrdering covers the CLI-style
// ordering where flags can be interleaved with positional
// args: `42 -y`, `-y 42`, `42 -y --name foo`, `-y --name foo
// 42`. The flag-only / positional-only split doesn't depend
// on order.
func TestParseFixArgs_PositionalOrdering(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want fixArgs
	}{
		{"yes-id", []string{"-y", "42"}, fixArgs{Mode: ModeRemote, RawArg: "42", Yes: true}},
		{"id-yes", []string{"42", "-y"}, fixArgs{Mode: ModeRemote, RawArg: "42", Yes: true}},
		{"yes-id-yes", []string{"-y", "42", "-y"}, fixArgs{Mode: ModeRemote, RawArg: "42", Yes: true}},
		{"yes-name-foo", []string{"-y", "--name", "foo"}, fixArgs{Mode: ModeLocal, RawArg: "foo", Yes: true}},
		{"name-foo-yes", []string{"--name", "foo", "-y"}, fixArgs{Mode: ModeLocal, RawArg: "foo", Yes: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseFixArgs(c.in)
			if err != nil {
				t.Fatalf("parseFixArgs(%v) error: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("parseFixArgs(%v) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}

// TestFixCmdSpec pins the argv grammar — the single source of
// truth for both the parse-errors' Usage tail and the
// Spec().Subcommands entry. Adding or retiring a flag means
// updating fixCmdSpec; both surfaces stay in lockstep.
//
// The Usage text is rendered into every parse error message,
// so a stray edit here changes user-visible output across
// the whole /gtw fix surface. The pinned text below is
// intentional: minimal, current-state, no F-XX IDs or
// process records.
func TestFixCmdSpec(t *testing.T) {
	wantUsage := "/gtw fix <issue-id> [-y|--yes]\n" +
		"                /gtw fix --name <branch> | -n <branch>"
	if got := fixCmdSpec.Usage; got != wantUsage {
		t.Errorf("fixCmdSpec.Usage =\n%q\nwant\n%q", got, wantUsage)
	}

	// Recognised flags: -y/--yes boolean; -n/--name value-taking.
	wantFlags := map[string]command.FlagSpec{
		"-y":     {Name: "yes"},
		"--yes":  {Name: "yes"},
		"-n":     {Name: "name", TakesValue: true},
		"--name": {Name: "name", TakesValue: true},
	}
	if len(fixCmdSpec.Flags) != len(wantFlags) {
		t.Errorf("fixCmdSpec.Flags has %d entries, want %d", len(fixCmdSpec.Flags), len(wantFlags))
	}
	for tok, want := range wantFlags {
		got, ok := fixCmdSpec.Flags[tok]
		if !ok {
			t.Errorf("fixCmdSpec.Flags missing %q", tok)
			continue
		}
		if got.Name != want.Name || got.TakesValue != want.TakesValue {
			t.Errorf("fixCmdSpec.Flags[%q] = %+v, want %+v", tok, got, want)
		}
	}

	// UsageTail must include the Usage string verbatim.
	if !strings.Contains(fixCmdSpec.UsageTail(), fixCmdSpec.Usage) {
		t.Errorf("UsageTail %q does not carry Usage", fixCmdSpec.UsageTail())
	}
}

// TestSpec_SubcommandsIncludesFix pins that /help lists /gtw
// fix as a subcommand with the spec Usage. Adds a regression
// guard against future refactors that drop the subcommand
// entry (the parent Spec().Usage is terser — /help relies on
// Subcommands for per-subcommand help).
func TestSpec_SubcommandsIncludesFix(t *testing.T) {
	f := NewFactory(NewManager())
	spec := f.Spec()

	var found *command.SubcommandSpec
	for i := range spec.Subcommands {
		if spec.Subcommands[i].Name == "fix" {
			found = &spec.Subcommands[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("Spec().Subcommands missing 'fix' entry; got %+v", spec.Subcommands)
	}
	if found.Usage != fixCmdSpec.Usage {
		t.Errorf("Spec().Subcommands[fix].Usage =\n%q\nwant\n%q", found.Usage, fixCmdSpec.Usage)
	}
}
