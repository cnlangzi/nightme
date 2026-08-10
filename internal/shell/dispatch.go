// Package shell dispatches `!cmd` user input to a real shell
// (sh -c on Unix, cmd /c on Windows) and returns the captured
// stdout/stderr/exit-code as a *Result the gateway can post to
// the chat.
//
// Routes:
//
//	User text "!" → shell package (this file)
//	User text "/" → command package (commander.go)
//	Other         → agent prompt
//
// The shell package is a sibling of internal/command — both
// live next to the gateway in the dispatch chain, but neither
// imports the other. Each owns its own prefix parser (parseShell
// here, parseCommand in commander.go) and the parsing contract
// between the two is locked in by the parallel test matrices
// in dispatch_test.go (this package) and commander_test.go
// (internal/command) — both share the same 13-row normalization
// cases.
//
// Platform-specific execution lives in dispatch_unix.go /
// dispatch_windows.go (build-tag isolated). This file is
// platform-agnostic.
package shell

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxStdoutLines caps the number of stdout lines inlined into
// the summary card. Anything beyond is summarised as
// "… N more lines truncated" so a runaway command can't blow
// up the Feishu message size limit.
const MaxStdoutLines = 50

// Request is the input to Dispatch.
//
// Text is the raw inbound message (including the leading "!").
// Cwd is the chat's SelectedCwd — where to run the command.
// An empty Cwd is treated as a user-facing error (the summary
// card reports it; nothing executes).
type Request struct {
	Text string
	Cwd  string
}

// Result is the outcome of Dispatch.
//
// Consumed mirrors the gateway CommandResult semantics: false
// means "this dispatcher didn't handle the input" (the gateway
// should fall through to message dispatch). true means shell
// owned it; Reply is what to post back to the user.
//
// Stdout/Stderr are the raw captured streams (decoded per
// platform — UTF-8 on Unix, GBK-aware on Windows). The summary
// card inlines the first MaxStdoutLines of Stdout; the rest is
// discarded (MVP).
type Result struct {
	Consumed bool

	Reply string // C-style summary card (already rendered)

	Stdout   string        // decoded stdout
	Stderr   string        // decoded stderr
	ExitCode int           // 0 = success; non-zero from exec.ExitError; -1 = signal/abort
	Duration time.Duration // wall-clock time spent in the child
	Cmd      string        // the command after parseShell stripping
	Cwd      string        // the Cwd from Request
}

// Dispatch is the package-level entry point. It detects the "!"
// prefix via parseShell; if not matched, returns Consumed=false
// so the gateway can fall through to message dispatch. If
// matched, it delegates to executeShell (platform-specific) and
// wraps the outcome in a Result with a rendered summary card.
//
// Dispatch never returns an error: execution failures are
// surfaced as Result.ExitCode (non-zero) plus a populated
// Stderr so the summary card reports them. The signature
// drops the error return so the caller doesn't need a dead
// `if err != nil` branch.
func Dispatch(ctx context.Context, req Request) *Result {
	cmd, matched := parseShell(req.Text)
	if !matched {
		return &Result{Consumed: false}
	}
	if strings.TrimSpace(req.Cwd) == "" {
		return &Result{
			Consumed: true,
			Reply:    "❌ shell: no CWD configured for this chat\nTry `/use <path>` first.",
			Cmd:      cmd,
		}
	}
	return executeShell(ctx, req.Cwd, cmd)
}

// parseShell mirrors parseCommand in internal/command/commander.go —
// both share the same normalization contract. Lock-step changes
// required; the parallel test matrices in this package's
// dispatch_test.go and internal/command's commander_test.go
// both encode the 13-row behavior table.
//
// Rules:
//  1. Trim leading whitespace (TrimLeft)
//  2. First character normalized: '!' (U+0021) or '！' (U+FF01)
//  3. Trim leading whitespace after prefix
//  4. Empty body → matched=false (防呆: lone "!" should fall through)
func parseShell(text string) (body string, matched bool) {
	text = strings.TrimLeft(text, " \t")
	if text == "" {
		return "", false
	}
	r, size := utf8.DecodeRuneInString(text)
	switch r {
	case '!', '！': // ! ！
	default:
		return "", false
	}
	rest := strings.TrimLeft(text[size:], " \t")
	if rest == "" {
		return "", false
	}
	return rest, true
}

// renderSummary produces the C-style summary card:
//
//	✅ $ <cmd>
//	exit <code> · <ms>ms · <cwd>
//	stdout:                  (optional, shown FIRST)
//	  line1
//	  line2
//	  … N more lines truncated   (when stdout exceeds MaxStdoutLines)
//	stderr:                  (optional, shown AFTER stdout)
//	  line1
//
// Failure variants flip ✅ → ❌. Exit code -1 from
// exec.ExitError indicates a signal (e.g. SIGKILL) — we
// surface it as-is.
//
// stdout is rendered before stderr to match conventional
// shell UX: the user wanted stdout, stderr is footnote noise.
func renderSummary(r *Result) string {
	var b strings.Builder
	if r.ExitCode == 0 {
		b.WriteString("✅ $ ")
	} else {
		b.WriteString("❌ $ ")
	}
	b.WriteString(r.Cmd)
	b.WriteByte('\n')
	b.WriteString("exit ")
	b.WriteString(strconv.Itoa(r.ExitCode))
	b.WriteString(" · ")
	b.WriteString(strconv.FormatInt(r.Duration.Milliseconds(), 10))
	b.WriteString("ms · ")
	b.WriteString(r.Cwd)

	if r.Stdout != "" {
		lines := splitLines(r.Stdout)
		if len(lines) <= MaxStdoutLines {
			b.WriteString("\nstdout:\n")
			b.WriteString(indent(r.Stdout))
		} else {
			b.WriteString("\nstdout (first ")
			b.WriteString(strconv.Itoa(MaxStdoutLines))
			b.WriteString(" of ")
			b.WriteString(strconv.Itoa(len(lines)))
			b.WriteString(" lines):\n")
			b.WriteString(indent(strings.Join(lines[:MaxStdoutLines], "\n")))
			b.WriteString("\n… ")
			b.WriteString(strconv.Itoa(len(lines) - MaxStdoutLines))
			b.WriteString(" more lines truncated")
		}
	}

	if r.Stderr != "" {
		b.WriteString("\nstderr:\n")
		b.WriteString(indent(r.Stderr))
	}

	return b.String()
}

// splitLines splits on '\n' and drops trailing empty elements
// (so "a\nb\n" and "a\nb\n\n\n" both yield ["a", "b"]).
// Returns nil for empty input. Used to canonicalize stdout
// before truncation so the line count matches the rendered
// output and the trailing empty-element case ("a\nb\n" →
// ["a", "b"] not ["a", "b", ""]) doesn't inflate the count.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	// Drop ALL trailing empty elements (handles "a\n\n\n" too).
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// indent prefixes every line with two spaces so the stdout
// block reads as a code block in the Feishu card. Trailing
// newline is preserved (so the block ends with "\n  " not
// "\n  " + extra noise) — renderSummary's caller adds the
// section separator.
func indent(s string) string {
	if s == "" {
		return s
	}
	// Trim a single trailing newline before indenting so the
	// output doesn't end with a whitespace-only line that
	// renders as a blank in the Feishu card.
	trimmed := s
	if strings.HasSuffix(trimmed, "\n") {
		trimmed = trimmed[:len(trimmed)-1]
	}
	lines := strings.Split(trimmed, "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}
