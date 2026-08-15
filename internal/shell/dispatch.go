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

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/timeouts"
)

// MaxStdoutLines caps the number of stdout lines inlined into
// the summary card. Anything beyond is summarised as
// "… N more lines truncated" so a runaway command can't blow
// up the Feishu message size limit.
const MaxStdoutLines = 50

// replyTimeout has moved to internal/timeouts (timeouts.Reply).

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
// the shell package can post replies through the wired
// outbound.Emitter.
//
// ChatID is the routing target for replies. MessageID is the
// user-side message id; used as the ReplyTo anchor for the
// outbound summary card AND as the userMsgID for the framework
// ⏳→✅ MessageStateBus emissions.
type InboundRequest struct {
	Request
	ChatID    string
	MessageID string
}

// result is the outcome of dispatch (low-level, synchronous
// runner). Consumed mirrors gateway semantics: false means
// "not a shell command, gateway falls through". true means
// shell owned it; Reply is the rendered summary card (caller
// decides whether to post it).
//
// Unexported: this struct is internal to the shell package.
// Tests in the same package access it directly; the runtime
// uses only Dispatcher.Handle (which exposes Consumed via
// ShellOutput — see below).
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

// ShellOutput is the framework-level return of Dispatcher.Handle,
// parallel to command.SlashOutput. The bool return carries
// "handled" (slash command attempt, not necessarily consumed);
// ShellOutput's Consumed carries the gateway-meaningful "this
// dispatcher ran the work" flag.
//
// Reply is intentionally absent: shell replies are async (posted
// by the runShell goroutine, not by Handle's caller), so the
// runtime shim has nothing to translate. Slash commands are
// synchronous, which is why SlashOutput carries Reply; the
// asymmetry mirrors the execution-model difference, not a
// missed symmetry.
type ShellOutput struct {
	Consumed bool
}

// Dispatcher handles shell commands end-to-end: prefix check,
// framework ⏳→✅ MessageStateBus emission, async execution,
// reply delivery. All shell-related logic lives here, not in
// the gateway or the runtime shim.
//
// Dispatcher is stateless and safe for concurrent use.
type Dispatcher struct {
	emitter outbound.Emitter
}

// NewDispatcher constructs a Dispatcher that posts replies via
// the given outbound.Emitter.
//
// Emitter may be nil: the dispatcher will still parse prefix
// and spawn the async goroutine, but the result reply is silently
// dropped. This lets the runtime keep the dispatcher wired even
// if the channel layer is unavailable (e.g. during shutdown),
// and gives tests a way to assert the Consumed contract without
// mocking an emitter.
func NewDispatcher(emitter outbound.Emitter) *Dispatcher {
	return &Dispatcher{emitter: emitter}
}

// Handle is the package's single runtime entry point. It runs
// the FULL shell dispatch flow:
//
//  1. Detect prefix via parseShell (FW→HW + trim + empty-body guard)
//  2. Not matched → return (nil, false) — gateway falls through
//     to tryMessageDispatch (the original "agent prompt" route)
//  3. Matched → emit framework ⏳ on the user message, spawn a
//     goroutine that runs dispatch and posts the C-style
//     summary card via the wired emitter, then return
//     (&ShellOutput{Consumed: true}, true). The goroutine is
//     intentionally NOT tracked by the gateway's wg, so
//     dispatchLoop isn't blocked and the daemon can shut down
//     cleanly without deadlocking on its own restart.
//  4. The goroutine emits framework ✅ on the user message at
//     exit (success, failure, panic) so the channel can render
//     the completion reaction.
//
// Handle does NOT take a context.Context parameter — by design.
// The async goroutine uses context.Background() so the shell
// command outlives any inbound-ctx cancellation (including the
// daemon shutdown triggered by `!make restart`). Adding a ctx
// parameter would be misleading: callers might assume cancelling
// it cancels the shell command, which it wouldn't. (Mirrors the
// pre-refactor doc; the rationale survived the Sender→Emitter
// swap.)
//
// Panic safety: the background goroutine recovers from any
// panic so a misbehaving shell command (or an emitter bug)
// cannot crash the daemon process. The panic is logged and
// swallowed — the user loses a reply but the daemon stays up.
func (d *Dispatcher) Handle(cs *chatsession.ChatSession, ir InboundRequest) (*ShellOutput, bool) {
	if _, matched := parseShell(ir.Text); !matched {
		// Fall-through contract: not a !cmd, gateway continues
		// to tryMessageDispatch. No MessageState emission — we
		// don't want ⏳ on inputs that aren't actually commands.
		return nil, false
	}

	// Framework ⏳ — emitted synchronously before the goroutine
	// spawns so the channel has the placeholder reaction ready
	// before any user-visible reply text arrives. Symmetric with
	// commander.Dispatch's pre-Handle MessageQueued emit; see
	// docs/feat/slash-command-reactions.md.
	chatsession.PublishMessageState(cs, ir.MessageID, agent.MessageQueued)

	go d.runShell(cs, ir)

	return &ShellOutput{Consumed: true}, true
}

// runShell is the body of the async goroutine spawned by Handle.
// It executes the shell command and posts the summary card via
// the wired emitter. Exits in one of three modes:
//
//   - normal completion → reply sent via emitter, ✅ emitted
//   - command failed (exit non-zero, dispatch error) → reply
//     sent (if any), ✅ still emitted
//   - panic → recovered, logged, ✅ still emitted (so the
//     user sees the completion reaction even when the
//     dispatcher itself crashed)
//
// The MessageDone emit is the FIRST defer so it runs LAST (LIFO
// defer order). All three modes above exit through the defer,
// guaranteeing the user always sees ✅ for any matched !cmd.
func (d *Dispatcher) runShell(cs *chatsession.ChatSession, ir InboundRequest) {
	// LIFO: this defer runs LAST (after the inner defer recovers
	// from panic). Putting MessageDone here guarantees ✅ fires
	// on every exit path — success, error, panic.
	defer chatsession.PublishMessageState(cs, ir.MessageID, agent.MessageDone)

	defer func() {
		if r := recover(); r != nil {
			slog.Default().Error("shell: panic in background dispatch",
				"panic", fmt.Sprint(r),
				"chat_id", ir.ChatID,
				"message_id", ir.MessageID)
		}
	}()

	shellCtx, cancel := context.WithTimeout(context.Background(), timeouts.Shell)
	defer cancel()

	out := dispatch(shellCtx, ir.Request)
	if !out.Consumed || out.Reply == "" || d.emitter == nil {
		return
	}

	replyCtx, cancel := context.WithTimeout(context.Background(), timeouts.Reply)
	defer cancel()
	if err := d.emitter.Send(replyCtx, messages.OutboundMessage{
		ChatID:  ir.ChatID,
		Kind:    messages.OutCommandReply,
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
}

// dispatch is the synchronous, low-level shell command runner.
// It does NOT post replies — it just runs the command and
// returns the result. Used by Dispatcher.runShell (which wraps
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
	// dispatch.go is the "!cmd" interactive-shell path. No
	// GTW_* vars here — those only apply to gtw hooks.
	return executeShell(ctx, req.Cwd, cmd, nil)
}

// parseShell is the canonical "!" prefix detector for this
// package and the only place where the FW→HW + trim +
// empty-body contract is implemented.
//
// Rules:
//
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
