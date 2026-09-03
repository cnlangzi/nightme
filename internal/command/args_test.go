package command

import (
	"strings"
	"testing"
)

// Tests for the shared slash-command argv lexer (issue #291).
// Each block pins one clause of the contract documented in
// args.go / docs/feat/F-gtw-fix.md §1.2 + §10, mirroring the
// style of gtw/parse_fix_args_test.go (table-driven subtests
// with error-message assertions).

// agentSpec is the canonical "one value-taking flag, no
// positional args" shape — /gtw commit / push / pr.
var agentSpec = CmdSpec{
	Name:  "/gtw push",
	Usage: "/gtw push [-a <agent>]",
	Flags: map[string]FlagSpec{
		"-a":      {Name: "agent", TakesValue: true},
		"--agent": {Name: "agent", TakesValue: true},
	},
	MinArgs: 0,
	MaxArgs: 0,
}

// singleArgSpec is the canonical "exactly one positional, no
// flags" shape — /use, /cwd.
var singleArgSpec = CmdSpec{
	Name:    "/use",
	Usage:   "/use <agent>",
	MinArgs: 1,
	MaxArgs: 1,
}

// mixedSpec exercises boolean + value flags together with an
// optional positional — the /gtw fix shape, used here to pin the
// generic lexer against the reference implementation's rules.
var mixedSpec = CmdSpec{
	Name:  "/demo",
	Usage: "/demo [<id>] [-y] [--name <branch>]",
	Flags: map[string]FlagSpec{
		"-y":      {Name: "yes"},
		"--yes":   {Name: "yes"},
		"-n":      {Name: "name", TakesValue: true},
		"--name":  {Name: "name", TakesValue: true},
		"--quiet": {}, // canonical name derived from the key
	},
	MinArgs: 0,
	MaxArgs: 1,
}

// TestParseCmdArgs_Positional pins contract rule 1: anything
// that is not flag-shaped is a positional arg, kept verbatim and
// in order. (Flag-shaped tokens take the error path — see
// TestParseCmdArgs_UnknownFlagRejected.)
func TestParseCmdArgs_Positional(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"single", []string{"claude"}, []string{"claude"}},
		// A lone "-" is conventionally a positional placeholder
		// (stdin), not a flag — git / cat / kubectl all treat it
		// that way.
		{"lone dash is positional", []string{"-"}, []string{"-"}},
		// Whitespace-only tokens survive untrimmed so handlers can
		// keep their own "blank name → usage" replies.
		{"whitespace token kept verbatim", []string{"  "}, []string{"  "}},
		{"embedded dash", []string{"my-agent"}, []string{"my-agent"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseCmdArgs(c.in, singleArgSpec)
			if err != nil {
				t.Fatalf("ParseCmdArgs(%q): %v", c.in, err)
			}
			if got.NArgs() != len(c.want) {
				t.Fatalf("NArgs = %d, want %d", got.NArgs(), len(c.want))
			}
			for i, w := range c.want {
				if got.Arg(i) != w {
					t.Errorf("Arg(%d) = %q, want %q", i, got.Arg(i), w)
				}
			}
		})
	}
}

// TestParseCmdArgs_ValueFlag pins contract rule 3: a
// value-taking flag consumes the next token, both aliases land
// in the same canonical slot, and the last occurrence wins.
func TestParseCmdArgs_ValueFlag(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"short", []string{"-a", "claude"}, "claude"},
		{"long", []string{"--agent", "codex"}, "codex"},
		{"absent", []string{}, ""},
		{"last wins", []string{"-a", "claude", "--agent", "codex"}, "codex"},
		// Rule 3 explicitly: the value is consumed even when it
		// is itself flag-shaped. `git branch -m --weird` works
		// the same way; refusing it would make a legitimate (if
		// odd) value unreachable.
		{"flag-shaped value", []string{"-a", "--codex"}, "--codex"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseCmdArgs(c.in, agentSpec)
			if err != nil {
				t.Fatalf("ParseCmdArgs(%q): %v", c.in, err)
			}
			if got.Value("agent") != c.want {
				t.Errorf("Value(agent) = %q, want %q", got.Value("agent"), c.want)
			}
			if got.NArgs() != 0 {
				t.Errorf("NArgs = %d, want 0 (flag values are not positionals)", got.NArgs())
			}
		})
	}
}

// TestParseCmdArgs_MissingValue pins contract rule 3's second
// half: a value-taking flag at the end of argv is a hard error,
// not an empty value.
func TestParseCmdArgs_MissingValue(t *testing.T) {
	for _, in := range [][]string{{"-a"}, {"--agent"}} {
		t.Run(strings.Join(in, " "), func(t *testing.T) {
			got, err := ParseCmdArgs(in, agentSpec)
			if err == nil {
				t.Fatalf("ParseCmdArgs(%q) = %+v, want error", in, got)
			}
			if !strings.Contains(err.Error(), "missing value for") {
				t.Errorf("error lacks 'missing value for': %q", err)
			}
			if !strings.Contains(err.Error(), "Usage: /gtw push") {
				t.Errorf("error lacks usage tail: %q", err)
			}
		})
	}
}

// TestParseCmdArgs_BooleanFlagPositionIndependent pins contract
// rule 4: boolean flags take no value, work anywhere in argv,
// and repeat idempotently ("any --yes wins").
func TestParseCmdArgs_BooleanFlagPositionIndependent(t *testing.T) {
	cases := [][]string{
		{"42", "-y"},
		{"-y", "42"},
		{"42", "--yes"},
		{"--yes", "42"},
		{"-y", "--yes", "42"},
	}
	for _, in := range cases {
		t.Run(strings.Join(in, " "), func(t *testing.T) {
			got, err := ParseCmdArgs(in, mixedSpec)
			if err != nil {
				t.Fatalf("ParseCmdArgs(%q): %v", in, err)
			}
			if !got.Bool("yes") {
				t.Errorf("Bool(yes) = false, want true")
			}
			if got.Arg(0) != "42" {
				t.Errorf("Arg(0) = %q, want 42 (boolean must not swallow the positional)", got.Arg(0))
			}
		})
	}

	// Absent boolean reads false, and a flag whose FlagSpec.Name
	// is empty derives its key from the token.
	got, err := ParseCmdArgs([]string{"--quiet"}, mixedSpec)
	if err != nil {
		t.Fatalf("ParseCmdArgs(--quiet): %v", err)
	}
	if got.Bool("yes") {
		t.Error("Bool(yes) = true, want false when the flag is absent")
	}
	if !got.Bool("quiet") {
		t.Error(`Bool(quiet) = false, want true (canonical name derived from "--quiet")`)
	}
}

// TestParseCmdArgs_UnknownFlagRejected pins contract rule 2: an
// undeclared flag-shaped token is an error, never a silent
// no-op. This is the whole point of issue #291 — `/gtw push
// --dr-run` (typo for --dry-run) must not run the push.
func TestParseCmdArgs_UnknownFlagRejected(t *testing.T) {
	cases := []struct {
		name string
		spec CmdSpec
		in   []string
		// hint text the message must carry so the user can
		// self-correct
		wantHint string
	}{
		{"typo on value flag", agentSpec, []string{"--dr-run"}, "recognised flags: -a/--agent"},
		{"unknown after good flag", agentSpec, []string{"-a", "claude", "--amend"}, "recognised flags: -a/--agent"},
		{"unknown before good flag", agentSpec, []string{"--draft", "-a", "claude"}, "recognised flags: -a/--agent"},
		{"short unknown", agentSpec, []string{"-x"}, "recognised flags: -a/--agent"},
		{"flagless command", singleArgSpec, []string{"--typo"}, "/use takes no flags"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseCmdArgs(c.in, c.spec)
			if err == nil {
				t.Fatalf("ParseCmdArgs(%q) = %+v, want error", c.in, got)
			}
			if !strings.Contains(err.Error(), "unknown flag") {
				t.Errorf("error lacks 'unknown flag': %q", err)
			}
			if !strings.Contains(err.Error(), c.wantHint) {
				t.Errorf("error lacks hint %q: %q", c.wantHint, err)
			}
		})
	}
}

// TestParseCmdArgs_FlagHintIsDeterministic guards against Go's
// randomised map iteration leaking into user-facing text: the
// recognised-flag list must render identically every call, with
// aliases grouped short-form-first.
func TestParseCmdArgs_FlagHintIsDeterministic(t *testing.T) {
	want := "recognised flags: -n/--name, --quiet, -y/--yes"
	for i := 0; i < 20; i++ {
		_, err := ParseCmdArgs([]string{"--nope"}, mixedSpec)
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("iteration %d: flag hint unstable or misordered.\n got: %q\nwant substring: %q", i, err, want)
		}
	}
}

// TestParseCmdArgs_ArityTooMany pins contract rule 5: extra
// positional args are hard-rejected, not silently dropped. The
// wording differs for "takes none at all" vs "takes at most N"
// because the user's mistake is different.
func TestParseCmdArgs_ArityTooMany(t *testing.T) {
	cases := []struct {
		name     string
		spec     CmdSpec
		in       []string
		wantText string
	}{
		{"no positional allowed", agentSpec, []string{"extra"}, "unexpected positional argument"},
		{"no positional allowed, after flag", agentSpec, []string{"-a", "claude", "extra"}, "unexpected positional argument"},
		// Flagless + zero-arity spec: the message collapses the
		// redundant "no positional args" + "no flags" into one
		// statement — a /stop / /gtw close user only needs to
		// hear it once.
		{"flagless no-args", CmdSpec{Name: "/stop", Usage: "/stop", MaxArgs: 0}, []string{"extra"}, "takes no arguments"},
		{"one too many", singleArgSpec, []string{"claude", "extra"}, "too many arguments"},
		{"several too many", singleArgSpec, []string{"a", "b", "c"}, "too many arguments"},
		{"positional after value flag", mixedSpec, []string{"--name", "br", "42", "43"}, "too many arguments"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseCmdArgs(c.in, c.spec)
			if err == nil {
				t.Fatalf("ParseCmdArgs(%q) = %+v, want error", c.in, got)
			}
			msg := err.Error()
			if !strings.Contains(msg, c.wantText) {
				t.Errorf("error lacks %q: %q", c.wantText, msg)
			}
			// Both variants must say "positional" so the user can
			// tell an arity error from a flag error at a glance.
			if !strings.Contains(msg, "positional") {
				t.Errorf("error lacks 'positional': %q", msg)
			}
			if !strings.Contains(msg, "Usage: ") {
				t.Errorf("error lacks usage tail: %q", msg)
			}
		})
	}
}

// TestParseCmdArgs_ArityTooFew pins the MinArgs half of rule 5,
// including singular/plural noun agreement in the message.
func TestParseCmdArgs_ArityTooFew(t *testing.T) {
	got, err := ParseCmdArgs(nil, singleArgSpec)
	if err == nil {
		t.Fatalf("ParseCmdArgs(nil) = %+v, want error", got)
	}
	if !strings.Contains(err.Error(), "missing argument") {
		t.Errorf("error lacks 'missing argument': %q", err)
	}
	if !strings.Contains(err.Error(), "needs 1 argument (got 0)") {
		t.Errorf("error lacks singular arg count: %q", err)
	}

	twoArgSpec := CmdSpec{Name: "/demo", Usage: "/demo <a> <b>", MinArgs: 2, MaxArgs: 2}
	_, err = ParseCmdArgs([]string{"only"}, twoArgSpec)
	if err == nil {
		t.Fatal("want error for 1 of 2 required args")
	}
	if !strings.Contains(err.Error(), "needs 2 arguments (got 1)") {
		t.Errorf("error lacks plural arg count: %q", err)
	}
}

// TestParseCmdArgs_UnboundedArgs covers the free-form-body shape
// (/steer, /queue): any number of positional args is legal, and
// flags are still checked.
func TestParseCmdArgs_UnboundedArgs(t *testing.T) {
	spec := CmdSpec{
		Name:    "/steer",
		Usage:   "/steer <message>",
		MinArgs: 1,
		MaxArgs: UnboundedArgs,
	}
	got, err := ParseCmdArgs([]string{"please", "stop", "and", "rebase"}, spec)
	if err != nil {
		t.Fatalf("ParseCmdArgs: %v", err)
	}
	if got.NArgs() != 4 {
		t.Errorf("NArgs = %d, want 4", got.NArgs())
	}
	if _, err := ParseCmdArgs(nil, spec); err == nil {
		t.Error("want error for empty body")
	}
}

// TestParseCmdArgs_Terminator pins the conventional `--`
// end-of-flags marker: everything after it is positional, even
// flag-shaped tokens. This is what makes an otherwise
// unreachable value (a path starting with "-") addressable.
func TestParseCmdArgs_Terminator(t *testing.T) {
	got, err := ParseCmdArgs([]string{"--", "-weird-dir"}, singleArgSpec)
	if err != nil {
		t.Fatalf("ParseCmdArgs: %v", err)
	}
	if got.Arg(0) != "-weird-dir" {
		t.Errorf("Arg(0) = %q, want -weird-dir", got.Arg(0))
	}

	// A second "--" after the terminator is itself positional,
	// and flags before the terminator still parse normally.
	got, err = ParseCmdArgs([]string{"-a", "claude", "--", "--"}, CmdSpec{
		Name:    "/demo",
		Flags:   agentSpec.Flags,
		MaxArgs: 1,
	})
	if err != nil {
		t.Fatalf("ParseCmdArgs: %v", err)
	}
	if got.Value("agent") != "claude" {
		t.Errorf("Value(agent) = %q, want claude", got.Value("agent"))
	}
	if got.NArgs() != 1 || got.Arg(0) != "--" {
		t.Errorf("Args = %q, want [--]", got.Args)
	}
}

// TestParsedArgs_Accessors covers the zero-value / out-of-range
// reads handlers rely on to avoid bounds checks.
func TestParsedArgs_Accessors(t *testing.T) {
	got, err := ParseCmdArgs([]string{"claude"}, singleArgSpec)
	if err != nil {
		t.Fatalf("ParseCmdArgs: %v", err)
	}
	if got.Arg(-1) != "" {
		t.Errorf("Arg(-1) = %q, want empty", got.Arg(-1))
	}
	if got.Arg(1) != "" {
		t.Errorf("Arg(1) = %q, want empty", got.Arg(1))
	}
	if got.Value("nope") != "" {
		t.Errorf("Value(nope) = %q, want empty", got.Value("nope"))
	}
	if got.Bool("nope") {
		t.Error("Bool(nope) = true, want false")
	}
}

// TestParsedArgs_Has distinguishes "absent" from "supplied
// with empty value" — Value() returns "" in both cases, so
// callers that need flag presence (e.g. mutual-exclusion
// between a value-taking flag and a bare positional) must use
// Has() instead.
//
// The empty-string case is the critical regression guard:
// `ParseCmdArgs(["--name", ""], spec)` stores name → "" in
// values, and a Value()-based presence check would falsely
// report "not supplied". Has() must still report true.
func TestParsedArgs_Has(t *testing.T) {
	valueSpec := CmdSpec{
		Name: "/cmd",
		Flags: map[string]FlagSpec{
			"-n":     {Name: "name", TakesValue: true},
			"--name": {Name: "name", TakesValue: true},
		},
		MinArgs: 0,
		MaxArgs: 0,
	}
	boolSpec := CmdSpec{
		Name: "/cmd",
		Flags: map[string]FlagSpec{
			"-y":    {Name: "yes"},
			"--yes": {Name: "yes"},
		},
		MinArgs: 0,
		MaxArgs: 0,
	}

	cases := []struct {
		name   string
		spec   CmdSpec
		argv   []string
		flag   string
		expect bool
	}{
		// Value-taking: present with value
		{"value-set", valueSpec, []string{"--name", "foo"}, "name", true},
		// Value-taking: present with empty value (the critical case)
		{"value-empty", valueSpec, []string{"--name", ""}, "name", true},
		{"value-empty-short", valueSpec, []string{"-n", ""}, "name", true},
		// Value-taking: absent
		{"value-absent", valueSpec, []string{}, "name", false},
		// Boolean: present
		{"bool-set", boolSpec, []string{"-y"}, "yes", true},
		{"bool-set-long", boolSpec, []string{"--yes"}, "yes", true},
		// Boolean: absent
		{"bool-absent", boolSpec, []string{}, "yes", false},
		// Unknown flag name always false
		{"unknown-flag", valueSpec, []string{}, "nope", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parsed, err := ParseCmdArgs(c.argv, c.spec)
			if err != nil {
				t.Fatalf("ParseCmdArgs(%v) unexpected error: %v", c.argv, err)
			}
			if got := parsed.Has(c.flag); got != c.expect {
				t.Errorf("Has(%q) = %v, want %v", c.flag, got, c.expect)
			}
		})
	}
}

// TestParseCmdArgs_NamelessSpec covers the fallback labels used
// when a caller leaves CmdSpec.Name / Usage empty — the error
// must still be a complete sentence.
func TestParseCmdArgs_NamelessSpec(t *testing.T) {
	_, err := ParseCmdArgs([]string{"--x"}, CmdSpec{})
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "this command takes no flags") {
		t.Errorf("error lacks generic command label: %q", err)
	}
	if strings.Contains(err.Error(), "Usage:") {
		t.Errorf("empty Usage must not render a usage tail: %q", err)
	}

	_, err = ParseCmdArgs([]string{"extra"}, CmdSpec{})
	if err == nil {
		t.Fatal("want error for positional against zero-arity spec")
	}
	if !strings.Contains(err.Error(), "unexpected positional argument") {
		t.Errorf("unexpected message: %q", err)
	}
}

// TestIsFlagToken pins the token classifier used by both the
// lexer and handlers that need the same flag/positional split.
func TestIsFlagToken(t *testing.T) {
	cases := map[string]bool{
		"-a":      true,
		"--agent": true,
		"--":      true, // flag-shaped; the lexer handles it as the terminator
		"-":       false,
		"":        false,
		"agent":   false,
		"42":      false,
		"a-b":     false,
	}
	for tok, want := range cases {
		if got := IsFlagToken(tok); got != want {
			t.Errorf("IsFlagToken(%q) = %v, want %v", tok, got, want)
		}
	}
}

// TestParseCmdArgs_ArityErrorNamesOffendingArgs pins that the
// arity error echoes the surplus tokens. In a group chat the
// usual cause is a trailing @-mention (only leading mentions are
// stripped before dispatch), and "got 2" alone doesn't tell the
// user which token to delete.
func TestParseCmdArgs_ArityErrorNamesOffendingArgs(t *testing.T) {
	_, err := ParseCmdArgs([]string{"claude", "@_user_1"}, singleArgSpec)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), `unexpected: "@_user_1"`) {
		t.Errorf("error does not name the surplus token: %q", err)
	}
	if strings.Contains(err.Error(), `"claude"`) {
		t.Errorf("error should not blame the accepted arg: %q", err)
	}

	// Multiple extras are all listed, in order.
	_, err = ParseCmdArgs([]string{"a", "b", "c"}, singleArgSpec)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), `unexpected: "b", "c"`) {
		t.Errorf("error does not list all surplus tokens: %q", err)
	}
}

// TestParseCmdArgs_NegativeMaxArgsIsUnbounded guards the
// surplus-token slice in checkArity: any negative MaxArgs must
// be read as "unbounded", so a miswritten spec degrades to
// lenient instead of panicking on a negative slice index.
func TestParseCmdArgs_NegativeMaxArgsIsUnbounded(t *testing.T) {
	for _, max := range []int{UnboundedArgs, -2, -99} {
		got, err := ParseCmdArgs([]string{"a", "b", "c"}, CmdSpec{Name: "/demo", MaxArgs: max})
		if err != nil {
			t.Fatalf("MaxArgs=%d: %v", max, err)
		}
		if got.NArgs() != 3 {
			t.Errorf("MaxArgs=%d: NArgs = %d, want 3", max, got.NArgs())
		}
	}
}
