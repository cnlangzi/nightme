package command

import (
	"fmt"
	"sort"
	"strings"
)

// This file holds the shared argv lexer for slash commands
// (issue #291). Before it, every command rolled its own inline
// parser — and most of them shared the same bug class: unknown
// flags and extra positional args were silently swallowed, so
// `/gtw push --dr-run` (typo for --dry-run) ran the push anyway.
//
// The contract implemented here is the standard CLI lexer model
// that git / kubectl / docker / gh all follow, and the one
// `parseFixArgs` (internal/command/gtw/cmd.go) established for
// `/gtw fix` in F-XX — see docs/feat/F-gtw-fix.md §1.2 + §10:
//
//  1. Token classification: a token is a flag when it starts
//     with "-" and is not the literal "-" (a lone dash is a
//     conventional stdin/positional placeholder). Everything
//     else is a positional arg.
//  2. Recognised flags are declared explicitly in CmdSpec.Flags.
//     Anything else is an `unknown flag` error — never a silent
//     no-op.
//  3. Value-taking flags consume the immediately-following token
//     as their value, even when that token itself looks like a
//     flag (`--agent --weird` sets agent="--weird", matching
//     git). A missing value is a hard error.
//  4. Boolean flags are positional-independent and idempotent
//     ("any --yes wins").
//  5. Arity is enforced: positional args below MinArgs or above
//     MaxArgs are hard-rejected, not silently dropped.
//
// parseFixArgs itself now uses this helper. Its Plan/Execute
// mode dispatch (Mode + Yes) and the mutual-exclusion between
// `--name` and a bare positional live in a thin wrapper around
// ParseCmdArgs — see internal/command/gtw/cmd.go. Adding a
// new flag means adding one entry to fixCmdSpec.Flags; the
// shared lexer keeps enforcing the "unknown flag" / arity /
// "missing value" contract uniformly across /<cmd> surfaces.

// UnboundedArgs is the CmdSpec.MaxArgs value meaning "any number
// of positional args" — used by commands whose payload is a
// free-form multi-token body (/steer, /queue).
const UnboundedArgs = -1

// ArgTerminator is the conventional end-of-flags marker. Every
// token after it is treated as positional, even flag-shaped
// ones. This is what lets `/cwd -- -weird-dir-name` work
// without the path being misread as a flag.
const ArgTerminator = "--"

// FlagSpec declares one recognised flag. Register every alias
// as its own key in CmdSpec.Flags pointing at the same Name so
// `-a` and `--agent` land in the same slot:
//
//	Flags: map[string]FlagSpec{
//	    "-a":      {Name: "agent", TakesValue: true},
//	    "--agent": {Name: "agent", TakesValue: true},
//	}
type FlagSpec struct {
	// Name is the canonical key ParsedArgs stores the flag
	// under. Aliases share it. Empty means "derive from the
	// map key by trimming leading dashes".
	Name string

	// TakesValue marks a value-taking flag: it consumes the
	// next token as its value (contract rule 3). When false the
	// flag is boolean (contract rule 4).
	TakesValue bool
}

// CmdSpec declares one command's argv grammar: which flags
// exist and how many positional args are legal.
type CmdSpec struct {
	// Name is the user-facing command name used in error
	// messages, with a leading slash: "/use", "/gtw push".
	Name string

	// Usage is the one-line usage string echoed after every
	// parse error, e.g. "/use <agent>". Optional, but every
	// caller in this repo sets it — the "Usage: ..." tail is
	// what the existing handler tests assert on.
	Usage string

	// Flags lists the recognised flag tokens. Nil/empty means
	// the command takes no flags, and any flag-shaped token is
	// rejected as unknown (which is how /stop distinguishes
	// `/stop --typo` from `/stop extra-word`).
	Flags map[string]FlagSpec

	// MinArgs is the number of positional args the command
	// requires. 0 means all positionals are optional.
	MinArgs int

	// MaxArgs is the number of positional args the command
	// accepts. The zero value (0) means "no positional args" —
	// commands that take some MUST set this. Use UnboundedArgs
	// for a free-form multi-token body.
	MaxArgs int
}

// ParsedArgs is the result of ParseCmdArgs: the positional args
// in order, plus the flag values keyed by canonical name.
type ParsedArgs struct {
	// Args holds the positional args verbatim, in argv order.
	// Tokens are NOT trimmed — callers that care about
	// whitespace-only input keep their own strings.TrimSpace
	// check (several handlers reply "Usage: ..." for `/use "  "`
	// and that behaviour is preserved).
	Args []string

	values map[string]string
	bools  map[string]bool
}

// Value returns the value of a value-taking flag, or "" when it
// was not supplied. Cannot distinguish "not supplied" from
// "supplied with empty value" — use Has for that.
func (p ParsedArgs) Value(name string) string { return p.values[name] }

// Has reports whether a flag (boolean OR value-taking) was
// supplied in argv. Useful for post-parse checks that need to
// distinguish "absent" from "supplied with empty value" — e.g.
// mutual exclusion between a value-taking flag and a bare
// positional, where an empty --name value still counts as
// "--name was used".
func (p ParsedArgs) Has(name string) bool {
	_, valueOK := p.values[name]
	_, boolOK := p.bools[name]
	return valueOK || boolOK
}

// Bool reports whether a boolean flag was supplied at least
// once.
func (p ParsedArgs) Bool(name string) bool { return p.bools[name] }

// Arg returns the i-th positional arg, or "" when there is no
// such arg. Lets handlers read an optional trailing arg without
// a bounds check.
func (p ParsedArgs) Arg(i int) string {
	if i < 0 || i >= len(p.Args) {
		return ""
	}
	return p.Args[i]
}

// NArgs returns the number of positional args.
func (p ParsedArgs) NArgs() int { return len(p.Args) }

// IsFlagToken reports whether tok is flag-shaped per contract
// rule 1: starts with "-" and is not the literal "-". Exported
// because handlers occasionally need the same classification
// outside a full parse (e.g. to decide whether a stray token is
// a typo'd flag or a bad positional).
func IsFlagToken(tok string) bool {
	return len(tok) > 1 && strings.HasPrefix(tok, "-")
}

// ParseCmdArgs is the standard CLI lexer for slash commands.
// argv is the token list AFTER the command name (input.Args[1:]
// for top-level commands, input.Args[2:] for /gtw subcommands).
//
// It returns an error — never a partially-applied result — for
// any unknown flag, missing flag value, or arity violation. The
// error text is user-facing: handlers reply with it directly
// (usually prefixed with "❌ ").
func ParseCmdArgs(argv []string, spec CmdSpec) (ParsedArgs, error) {
	out := ParsedArgs{
		values: make(map[string]string),
		bools:  make(map[string]bool),
	}

	// Phase 1: lex. Classify each token, consume values for
	// value-taking flags, reject anything undeclared.
	terminated := false
	for i := 0; i < len(argv); i++ {
		tok := argv[i]

		if !terminated && tok == ArgTerminator {
			// Everything after "--" is positional, flag-shaped
			// or not.
			terminated = true
			continue
		}

		if terminated || !IsFlagToken(tok) {
			out.Args = append(out.Args, tok)
			continue
		}

		fs, ok := spec.Flags[tok]
		if !ok {
			return ParsedArgs{}, fmt.Errorf("unknown flag %q (%s)%s",
				tok, spec.flagHint(), spec.usageTail())
		}

		key := fs.Name
		if key == "" {
			key = strings.TrimLeft(tok, "-")
		}

		if !fs.TakesValue {
			// Boolean: positional-independent, idempotent.
			out.bools[key] = true
			continue
		}

		if i+1 >= len(argv) {
			return ParsedArgs{}, fmt.Errorf("missing value for %s%s",
				tok, spec.usageTail())
		}
		// Consume the next token verbatim — even if it is
		// itself flag-shaped. `--name --foo` names a branch
		// "--foo"; that's what git does, and second-guessing it
		// would make a legitimate (if odd) value unreachable.
		out.values[key] = argv[i+1]
		i++
	}

	// Phase 2: arity. Enforced here rather than at each
	// dispatch site so "extra args silently dropped" cannot
	// come back one command at a time.
	if err := spec.checkArity(out.Args); err != nil {
		return ParsedArgs{}, err
	}
	return out, nil
}

// checkArity enforces MinArgs / MaxArgs against the collected
// positional args (contract rule 5).
func (s CmdSpec) checkArity(args []string) error {
	if len(args) < s.MinArgs {
		return fmt.Errorf("missing argument: %s needs %s (got %d)%s",
			s.name(), plural(s.MinArgs, "argument"), len(args), s.usageTail())
	}
	// Any negative MaxArgs (UnboundedArgs, or a stray -2 from a
	// miswritten spec) means "no upper bound" — never index with
	// it below.
	if s.MaxArgs < 0 || len(args) <= s.MaxArgs {
		return nil
	}
	if s.MaxArgs == 0 {
		// Distinct wording for "takes none at all": the user's
		// mistake is a stray word, not a miscount. When the spec
		// also declares no flags, collapse "no positional args
		// (/cmd takes no flags)" into one statement so the
		// message reads cleanly for the /stop / /gtw close case.
		if len(s.Flags) == 0 {
			return fmt.Errorf("unexpected positional argument %q: %s takes no arguments%s",
				args[0], s.name(), s.usageTail())
		}
		return fmt.Errorf("unexpected positional argument %q: %s takes no positional args (%s)%s",
			args[0], s.name(), s.flagHint(), s.usageTail())
	}
	return fmt.Errorf("too many arguments: %s takes at most %s (got %d, unexpected: %s)%s",
		s.name(), plural(s.MaxArgs, "positional argument"), len(args),
		quoteList(args[s.MaxArgs:]), s.usageTail())
}

// quoteList renders tokens as a quoted, comma-separated list so
// arity errors can name the offending args — a trailing
// @-mention or an unquoted path with a space is much easier to
// spot when echoed back than when only counted.
func quoteList(tokens []string) string {
	quoted := make([]string, 0, len(tokens))
	for _, t := range tokens {
		quoted = append(quoted, fmt.Sprintf("%q", t))
	}
	return strings.Join(quoted, ", ")
}

// name returns the command name for error text, defaulting to a
// generic label when the caller left it empty.
func (s CmdSpec) name() string {
	if s.Name == "" {
		return "this command"
	}
	return s.Name
}

// usageTail renders the ". Usage: ..." suffix appended to every
// parse error, or "" when the spec carries no usage string.
func (s CmdSpec) usageTail() string {
	if s.Usage == "" {
		return ""
	}
	return ". Usage: " + s.Usage
}

// UsageTail is the exported form of usageTail. Use it from
// post-parse error sites (e.g. cross-flag validation the
// shared lexer can't express) so user-facing errors carry the
// same Usage suffix as the lexer's own errors.
func (s CmdSpec) UsageTail() string {
	return s.usageTail()
}

// flagHint renders the recognised-flag list for error messages,
// grouping aliases of the same canonical flag: "-a/--agent".
// Groups are ordered deterministically (by canonical name) so
// the message is stable across runs — Go map iteration is not.
func (s CmdSpec) flagHint() string {
	if len(s.Flags) == 0 {
		return s.name() + " takes no flags"
	}

	groups := make(map[string][]string, len(s.Flags))
	for tok, fs := range s.Flags {
		key := fs.Name
		if key == "" {
			key = strings.TrimLeft(tok, "-")
		}
		groups[key] = append(groups[key], tok)
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	rendered := make([]string, 0, len(keys))
	for _, k := range keys {
		aliases := groups[k]
		// Short form first ("-a/--agent"), matching how every
		// CLI's own --help renders it.
		sort.Slice(aliases, func(i, j int) bool {
			if len(aliases[i]) != len(aliases[j]) {
				return len(aliases[i]) < len(aliases[j])
			}
			return aliases[i] < aliases[j]
		})
		rendered = append(rendered, strings.Join(aliases, "/"))
	}
	return "recognised flags: " + strings.Join(rendered, ", ")
}

// plural renders a count with the right noun form, so error
// messages read "needs 1 argument" not "needs 1 arguments".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
