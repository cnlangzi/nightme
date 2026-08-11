// Package shell dispatches `!cmd` user input to a real shell
// (sh -c on Unix, cmd /c on Windows). All shell-related logic
// — prefix detection, async execution, reply posting — lives
// here. The gateway sees only a thin Handle(ir) → Consumed-bool
// contract; no shell-specific knowledge leaks into the gateway
// or the runtime shim.
//
// Routes:
//
//	User text "!" → shell package (this file)
//	User text "/" → command package (commander.go)
//	Other         → agent prompt
//
// Each package owns its own prefix parser (parseShell here,
// parseCommand in commander.go); the contract between the two
// is locked in by the parallel test matrices in dispatch_test.go
// and commander_test.go (both share the 13-row normalization
// cases).
//
// Platform-specific execution lives in dispatch_unix.go /
// dispatch_windows.go (build-tag isolated). This file is
// platform-agnostic.
package shell

import (
	"context"
	"fmt"
	"log/slog"
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

// shellTimeout caps how long a single shell command can run
// (background goroutine lifetime). 5 minutes is generous for
// normal use; longer commands should run inside a screen/tmux
// the user sets up themselves.
const shellTimeout = 5 * time.Minute

// replyTimeout caps the final summary-card reply send.
const replyTimeout = 5 * time.Second

// Request is the input to Dispatch (and to Dispatcher.Handle
// via InboundRequest).
//
// Text is the raw inbound message (including the leading "!").
// Cwd is the chat's SelectedCwd — where to run the command.
// An empty Cwd is treated as a user-facing error (the summary
// card reports it; nothing executes).
type Request struct {
	Text string
	Cwd  string
}

// InboundRequest is what the runtime shim hands to
// Dispatcher.Handle. It augments Request with routing info so
// the shell package can post replies without depending on the
// gateway/Feishu types.
type InboundRequest struct {
	Request
	ChatID    string // routing target for replies
	MessageID string // Feishu message_id; used as ReplyTo anchor
}

// result is the outcome of dispatch (low-level, synchronous
// runner). Consumed mirrors gateway semantics: false means
// "not a shell command, gateway falls through". true means
// shell owned it; Reply is the rendered summary card (caller
// decides whether to post it).
//
// Unexported: this struct is internal to the shell package.
// Tests in the same package access it directly; the runtime
// uses only Dispatcher.Handle (which exposes Consumed as
// HandleResult — see below).
type result struct {
	Consumed bool

	Reply string // C-style summary card (already rendered)

	Stdout   string        // decoded stdout
	Stderr   string        // decoded stderr
	ExitCode int           // 0 = success; non-zero from exec.ExitError; -1 = signal/abort
	Duration time.Duration // wall-clock time spent in the child
	Cmd      string        // the command after parseShell stripping
	Cwd      string        // the Cwd from Request
}

// HandleResult is what Dispatcher.Handle returns. Consumed
// false → gateway falls through to message dispatch. Consumed
// true → gateway stops here; the result reply (if any) was
// already posted via Sender.
type HandleResult struct {
	Consumed bool
}

// Outbound is a minimal message struct the Sender posts to
// whatever channel it wraps (Feishu, Slack, log, etc.). Defined
// here so the shell package stays decoupled from the gateway /
// channel-adapter types.
type Outbound struct {
	ChatID  string
	Text    string
	ReplyTo string // optional: when non-empty, post as thread reply
}

// Sender is the abstraction the shell package uses to post
// messages back to the user. Implementations are thin adapters
// over whatever the runtime uses (Feishu adapter, log, etc.).
//
// Send may block bounded by ctx. The shell package does NOT
// retry on failure — replies are best-effort, especially the
// goroutine-posted result which may run after a daemon restart.
type Sender interface {
	Send(ctx context.Context, msg Outbound) error
}

// Dispatcher handles shell commands end-to-end: prefix check,
// placeholder, async execution, reply delivery. All shell-
// related logic lives here, not in the gateway or the runtime
// shim. The shim is reduced to type adaptation
// (gateway.InboundMessage → InboundRequest,
// HandleResult → gateway.CommandResult).
//
// Dispatcher is stateless and safe for concurrent use.
type Dispatcher struct {
	sender Sender
}

// NewDispatcher constructs a Dispatcher that posts replies via
// the given Sender.
//
// Sender may be nil: the dispatcher will still parse prefix and
// spawn the async goroutine, but the result reply (and any
// future placeholder) is silently dropped. This lets the
// runtime keep the dispatcher wired even if the channel layer
// is unavailable (e.g. during shutdown), and gives tests a way
// to assert the Consumed contract without mocking a Sender.
func NewDispatcher(sender Sender) *Dispatcher {
	return &Dispatcher{sender: sender}
}

// Handle is the package's single runtime entry point. It runs
// the FULL shell dispatch flow:
//
//  1. Detect prefix via parseShell (FW→HW + trim + empty-body guard)
//  2. Not matched → return Consumed=false (gateway falls through
//     to tryMessageDispatch — the original "agent prompt" route)
//  3. Matched → spawn a goroutine that runs Dispatch and posts the
//     C-style summary card via the Sender. The goroutine is
//     intentionally NOT tracked by the gateway's wg, so
//     dispatchLoop isn't blocked and the daemon can shut down
//     cleanly without deadlocking on its own restart.
//  4. Return Consumed=true (gateway stops the chain here)
//
// Placeholder / "🔄 running …" delivery is OUT OF SCOPE for
// this method — that lives at a higher layer (the inbound
// pipeline has its own progress UX). This method focuses on
// executing the command and posting the final result.
//
// All shell-related logic — prefix detection, async execution,
// reply delivery — lives in this method.
// Handle is the package's single runtime entry point.
//
// It does NOT take a context.Context parameter — by design. The
// async goroutine below uses context.Background() so the shell
// command outlives any inbound-ctx cancellation (including the
// daemon shutdown triggered by `!make restart`). Adding a ctx
// parameter would be misleading: callers might assume cancelling
// it cancels the shell command, which it wouldn't.
//
// Flow:
//  1. Detect prefix via parseShell (FW→HW + trim + empty-body guard)
//  2. Not matched → return Consumed=false (gateway falls through
//     to tryMessageDispatch — the original "agent prompt" route)
//  3. Matched → spawn a goroutine that runs dispatch and posts
//     the C-style summary card via the Sender. The goroutine is
//     intentionally NOT tracked by the gateway's wg, so
//     dispatchLoop isn't blocked and the daemon can shut down
//     cleanly without deadlocking on its own restart.
//  4. Return Consumed=true (gateway stops the chain here)
//
// Panic safety: the background goroutine recovers from any
// panic so a misbehaving shell command (or a Sender bug) cannot
// crash the daemon process. The panic is logged and swallowed —
// the user loses a reply but the daemon stays up.
func (d *Dispatcher) Handle(ir InboundRequest) HandleResult {
	_, matched := parseShell(ir.Text)
	if !matched {
		return HandleResult{Consumed: false}
	}

	// Async execution. The goroutine outlives this Handle call
	// (and possibly the daemon, in the !make restart case) —
	// see shellTimeout for the lifetime bound.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Default().Error("shell: panic in background dispatch",
					"panic", fmt.Sprint(r),
					"chat_id", ir.ChatID,
					"message_id", ir.MessageID)
			}
		}()

		shellCtx, cancel := context.WithTimeout(context.Background(), shellTimeout)
		defer cancel()

		out := dispatch(shellCtx, ir.Request)
		if !out.Consumed || out.Reply == "" || d.sender == nil {
			return
		}

		replyCtx, cancel := context.WithTimeout(context.Background(), replyTimeout)
		defer cancel()
		if err := d.sender.Send(replyCtx, Outbound{
			ChatID:  ir.ChatID,
			Text:    out.Reply,
			ReplyTo: ir.MessageID,
		}); err != nil {
			// Reply delivery is best-effort (the user may still
			// see the result via the new daemon after a restart),
			// but a Send failure is worth a diagnostic line — a
			// silent drop makes "my `!cmd` produced nothing"
			// debugging painful.
			slog.Default().Warn("shell: reply send failed",
				"err", err,
				"chat_id", ir.ChatID,
				"message_id", ir.MessageID)
		}
	}()

	return HandleResult{Consumed: true}
}

// dispatch is the synchronous, low-level shell command runner.
// It does NOT post replies — it just runs the command and
// returns the result. Used by Dispatcher.Handle (which wraps
// it with reply delivery) and by tests that want to inspect
// stdout/stderr/exit-code directly.
//
// Unexported: callers must go through Dispatcher.Handle.
//
// dispatch never returns an error: execution failures are
// surfaced as result.ExitCode (non-zero) plus a populated
// Stderr so the summary card reports them. The signature
// drops the error return so callers don't need a dead
// `if err != nil` branch.
func dispatch(ctx context.Context, req Request) *result {
	cmd, matched := parseShell(req.Text)
	if !matched {
		return &result{Consumed: false}
	}
	if strings.TrimSpace(req.Cwd) == "" {
		return &result{
			Consumed: true,
			Reply:    "❌ shell: no CWD configured for this chat\nTry `/use <path>` first.",
			Cmd:      cmd,
		}
	}
	return executeShell(ctx, req.Cwd, cmd)
}

// parseShell is the canonical "!" prefix detector for this
// package and the only place where the FW→HW + trim +
// empty-body contract is implemented.
//
// Rules:
//  1. Trim leading whitespace (TrimLeft)
//  2. First character normalized: '!' (U+0021) or '！' (U+FF01)
//  3. Trim leading whitespace after prefix
//  4. Empty body → matched=false (防呆: lone "!" should fall through)
//
// Mirrors parseCommand in internal/command/commander.go —
// both share the same normalization contract. Lock-step changes
// required; the parallel test matrices in this package's
// dispatch_test.go and internal/command's commander_test.go
// both encode the 13-row behavior table.
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
func renderSummary(r *result) string {
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
// block reads as a code block in the Feishu card. A trailing
// newline is trimmed before indenting so the output doesn't
// end with a whitespace-only line that renders as a blank
// in the Feishu card. renderSummary's caller adds the section
// separator.
func indent(s string) string {
	if s == "" {
		return s
	}
	trimmed := strings.TrimSuffix(s, "\n")
	lines := strings.Split(trimmed, "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}
