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
// between the two is locked in by the 13-row test matrix in
// wip/feat-shell.md.
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
func Dispatch(ctx context.Context, req Request) (*Result, error) {
	cmd, matched := parseShell(req.Text)
	if !matched {
		return &Result{Consumed: false}, nil
	}
	if strings.TrimSpace(req.Cwd) == "" {
		return &Result{
			Consumed: true,
			Reply:    "❌ shell: no CWD configured for this chat\nTry `/use <path>` first.",
			Cmd:      cmd,
		}, nil
	}
	return executeShell(ctx, req.Cwd, cmd)
}

// parseShell mirrors parseCommand in internal/command/commander.go —
// both share the same normalization contract. Lock-step changes
// required; see wip/feat-shell.md for the 13-row matrix.
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
//	stdout:                  (optional)
//	  line1
//	  line2
//	  … N more lines truncated   (when stdout exceeds MaxStdoutLines)
//	stderr:                  (optional)
//	  line1
//
// Failure variants flip ✅ → ❌. Exit code -1 from
// exec.ExitError indicates a signal (e.g. SIGKILL) — we
// surface it as-is.
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

	if r.Stderr != "" {
		b.WriteString("\nstderr:\n")
		b.WriteString(indent(r.Stderr))
	}

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

	return b.String()
}

// splitLines splits on '\n' and drops a single trailing empty
// element caused by a terminal newline (so "a\nb\n" yields
// ["a", "b"], not ["a", "b", ""]). Returns nil for empty input.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// indent prefixes every line with two spaces so the stdout
// block reads as a code block in the Feishu card.
func indent(s string) string {
	if s == "" {
		return s
	}
	// strings.Split preserves empty trailing element on a
	// terminal \n — we keep it here so the indent matches the
	// original line count (matters for the truncated case).
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}
