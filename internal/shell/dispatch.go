// Package shell dispatches `!cmd` user input to a real shell
// (sh -c on Unix, cmd /c on Windows). All shell-related logic
// — prefix detection, async execution, streaming reply via
// OutReply — lives here. The gateway sees only a thin
// Handle(ir) → Consumed-bool contract; no shell-specific
// knowledge leaks into the gateway or the runtime shim.
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
//
// Streaming reply (F-shell-stream): runShell posts three OutReply
// messages per command — header, chunks, footer — all sharing
// the same ReplyTo so the Feishu / Slack adapter PATCHes a
// single placeholder card in place. See runShell doc.
package shell

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/timeouts"
)

// shellFlushInterval is the debouncer cadence for chunk sends.
// coalesceLines produces size-based chunks (4 KiB); this layer
// adds time-based coalescing on top so the channel sees at most
// ~1/interval sends/sec regardless of how fast the child writes.
//
// 1.5s chosen to: (1) push well below the human "is this stuck?"
// threshold (perceived as ~2s) so the user sees updates during
// long-running commands without flooding the channel; (2) batch
// bursty output (`make test` printing per-line progress) into
// one PATCH per ~1.5s window; (3) leave channel-side throttling
// (~300ms Feishu PATCH rate) as the final gate, not the
// primary one.
//
// Why shell-side (not channel-side): no channel can keep up
// with raw shell output (multi-MB/s bursts), and pushing rate-
// limiting into every channel adapter couples shell-streaming
// concerns into Feishu/Slack/etc. The shell debounces so
// channels stay simple (their existing PATCH throttle handles
// what little remains).
const shellFlushInterval = 1500 * time.Millisecond

// chunkDebouncer accumulates onChunk payloads (typically
// coalesceLines's size-based 4 KiB chunks) and flushes them
// to flush every interval, capping the send rate at
// ~1/interval. Use:
//
//	db := newChunkDebouncer(send, 250*time.Millisecond)
//	coalesceLines(r, sink, db.Add, false)
//	defer db.Stop() // final flush
//
// Add is goroutine-safe (called from drainer goroutines).
// Stop must be called once to release the timer goroutine and
// flush any remaining content; usually via defer at the
// caller's scope.
type chunkDebouncer struct {
	flush    func(string) error
	interval time.Duration
	mu       sync.Mutex
	buf      strings.Builder
	stop     chan struct{}
	done     chan struct{}
}

func newChunkDebouncer(flush func(string) error, interval time.Duration) *chunkDebouncer {
	d := &chunkDebouncer{
		flush:    flush,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go d.loop()
	return d
}

// loop runs the periodic flush ticker until Stop is called.
// On each tick (or on Stop), flushes whatever is buffered.
// Uses sync.Mutex to serialise against Add from drainer
// goroutines.
func (d *chunkDebouncer) loop() {
	defer close(d.done)
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			d.fire()
		case <-d.stop:
			d.fire()
			return
		}
	}
}

// fire drains the buffer under lock and invokes flush once.
// Empty buffers are a no-op.
func (d *chunkDebouncer) fire() {
	d.mu.Lock()
	if d.buf.Len() == 0 {
		d.mu.Unlock()
		return
	}
	text := d.buf.String()
	d.buf.Reset()
	d.mu.Unlock()
	_ = d.flush(text)
}

// Add appends text to the pending buffer. Goroutine-safe;
// called from drainer goroutines that don't otherwise
// coordinate.
func (d *chunkDebouncer) Add(text string) {
	d.mu.Lock()
	d.buf.WriteString(text)
	d.mu.Unlock()
}

// Stop signals the loop to exit and blocks until the final
// flush has completed. Idempotent: a second call is a no-op.
func (d *chunkDebouncer) Stop() {
	select {
	case <-d.stop:
		// already stopped
	default:
		close(d.stop)
	}
	<-d.done
}

// Request is the input to Dispatch (and to Dispatcher.Handle
// via InboundRequest).
//
// Text is the raw inbound message (including the leading "!").
// Cwd is the chat's SelectedCwd — where to run the command.
// An empty Cwd is treated as a user-facing error (the reply
// card reports it; nothing executes).
type Request struct {
	Text string
	Cwd  string
}

// InboundRequest is what the runtime shim hands to
// Dispatcher.Handle. It augments Request with routing info so
// the shell package can post replies through the wired
// messages.Emitter.
//
// ChatID is the routing target for replies. MessageID is the
// user-side message id; used as the ReplyTo anchor for the
// outbound streaming reply AND as the userMsgID for the
// framework ⏳→✅ MessageStateBus emissions.
type InboundRequest struct {
	Request
	ChatID    string
	MessageID string
}

// result is the outcome of executeShell (low-level streaming
// runner). Stdout / Stderr hold the full captured output
// (always populated, even when chunks were streamed via
// onChunk); ExitCode / Duration / Cwd describe the run.
//
// Consumed, Reply, and Cmd were removed in F-shell-stream:
// Consumed was only used by the deleted dispatch() wrapper,
// Reply was the pre-rendered summary card (now split across
// header/chunk/footer OutReply messages), and Cmd was only
// read by the deleted renderSummary helper.
//
// Unexported: this struct is internal to the shell package.
// Tests in the same package access it directly.
type result struct {
	Stdout   string        // decoded stdout (full, even when streamed)
	Stderr   string        // decoded stderr (full, even when streamed)
	ExitCode int           // 0 = success; non-zero from exec.ExitError; -1 = signal/abort / drainer panic
	Duration time.Duration // wall-clock time spent in the child
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
// framework ⏳→✅ MessageStateBus emission, async streaming
// execution, reply delivery. All shell-related logic lives here,
// not in the gateway or the runtime shim.
//
// Dispatcher is stateless and safe for concurrent use.
type Dispatcher struct{}

// NewDispatcher constructs a Dispatcher. v1.3+ multi-channel: the
// Emitter is no longer held in the Dispatcher — the per-channel
// chatsession.Manager (passed to Handle per call) carries the
// Emitter bound to the channel that produced the inbound.
//
// Tests can pass nil mgr to assert the Consumed contract without
// mocking an emitter; the streaming replies are silently dropped.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{}
}

// Handle is the package's single runtime entry point. It runs
// the FULL shell dispatch flow:
//
//  1. Detect prefix via parseShell (FW→HW + trim + empty-body guard)
//  2. Not matched → return (nil, false) — gateway falls through
//     to tryMessageDispatch (the original "agent prompt" route)
//  3. Matched → emit framework ⏳ on the user message, spawn a
//     goroutine that streams the command via OutReply messages
//     (header / chunks / footer), then return
//     (&ShellOutput{Consumed: true}, true). The goroutine is
//     intentionally NOT tracked by the gateway's wg, so
//     dispatchLoop isn't blocked and the daemon can shut down
//     cleanly without deadlocking on its own restart.
//  4. The goroutine emits framework ✅ on the user message at
//     exit (success, failure, panic) so the channel can render
//     the completion reaction.
//
// v1.3+ multi-channel: mgr is the per-channel chatsession.Manager
// that produced the inbound. The Emitter used to post replies
// comes from mgr — that's the channel that owns this chatID.
//
// Handle does NOT take a context.Context parameter — by design.
// The async goroutine uses context.Background() so the shell
// command outlives any inbound-ctx cancellation (including the
// daemon shutdown triggered by `!make restart`). Adding a ctx
// parameter would be misleading: callers might assume cancelling
// it cancels the shell command, which it wouldn't.
//
// Panic safety: the background goroutine recovers from any
// panic so a misbehaving shell command (or an emitter bug)
// cannot crash the daemon process. The panic is logged and
// swallowed — the user loses a reply but the daemon stays up.
func (d *Dispatcher) Handle(mgr *chatsession.Manager, cs *chatsession.ChatSession, ir InboundRequest) (*ShellOutput, bool) {
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

	go d.runShell(mgr, cs, ir)

	return &ShellOutput{Consumed: true}, true
}

// runShell is the body of the async goroutine spawned by Handle.
// It executes the shell command and streams the output to the
// channel via three OutReply messages:
//
//  1. **Header** — "⌨️ $ <cmd>\n". Cold-starts the receipt via
//     Feishu's ensureReceiptForReplyWithFooter (or Slack's
//     streamFor) so subsequent chunks PATCH the same card.
//  2. **Chunks** — coalesced output. Each one lands on
//     AppendEntryWithFooter → PatchMessage(replyMsgID, ...).
//     On emitter failure, runShell cancels shellCtx so
//     CommandContext kills the child; otherwise the drainers
//     would stall on a wedged pipe for the rest of
//     timeouts.Shell.
//  3. **Footer** — "\n✅/❌ exit <code> · <ms>ms · <cwd>".
//     Final AppendEntryWithFooter. The receipt then
//     transitions to ✅ via OnPromptEnded (wired by the
//     MessageDone defer below).
//
// runShell exits in one of three modes:
//
//   - normal completion → 3 OutReply messages sent, ✅ emitted
//   - empty CWD or drainErr → only header (with error text) or
//     partial chunks sent, ✅ still emitted
//   - panic → recovered, logged, ✅ still emitted (so the
//     user sees the completion reaction even when the
//     dispatcher itself crashed)
//
// The MessageDone emit is the FIRST defer so it runs LAST (LIFO
// defer order). All three modes above exit through the defer,
// guaranteeing the user always sees ✅ for any matched !cmd.
//
// v1.3+ multi-channel: mgr carries the per-channel Emitter used
// to post the reply. Same mgr is forwarded from Handle.
func (d *Dispatcher) runShell(mgr *chatsession.Manager, cs *chatsession.ChatSession, ir InboundRequest) {
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

	emitter := mgr.Emitter()
	if emitter == nil {
		return
	}

	shellCtx, cancel := context.WithTimeout(context.Background(), timeouts.Shell)
	defer cancel()

	cmd, matched := parseShell(ir.Text)
	if !matched {
		return
	}
	if strings.TrimSpace(ir.Request.Cwd) == "" {
		// Empty CWD: post a single OutReply with the friendly
		// error and return. The defer will still fire MessageDone
		// (→ receipt ✅ via OnPromptEnded).
		_ = emitter.Send(shellCtx, messages.OutboundMessage{
			ChatID:  ir.ChatID,
			Kind:    messages.OutReply,
			Text:    "❌ shell: no CWD configured for this chat\nTry `/use <path>` first.",
			ReplyTo: ir.MessageID,
		})
		return
	}

	send := func(text string) error {
		return emitter.Send(shellCtx, messages.OutboundMessage{
			ChatID:  ir.ChatID,
			Kind:    messages.OutReply,
			Text:    text,
			ReplyTo: ir.MessageID,
		})
	}

	// ① header — cold-starts the receipt. If this Send fails,
	// subsequent chunks take the orphan / cold-start path
	// (Feishu's postOrphanReplyCard / ensureReceiptForReplyWithFooter)
	// instead of PATCHing a missing receipt — the user still sees
	// output, just on separate cards instead of one rolling card.
	if err := send(fmt.Sprintf("⌨️ $ %s\n", cmd)); err != nil {
		slog.Default().Warn("shell: header send failed",
			"err", err,
			"chat_id", ir.ChatID,
			"message_id", ir.MessageID)
	}

	// ② chunks — coalesceLines (size-based, 4 KiB chunks) drives
	// a shell-side debouncer that flushes every shellFlushInterval
	// (250 ms). The debouncer caps the send rate at ~4/s regardless
	// of how fast the child writes — no channel can keep up with
	// raw shell output, and pushing rate-limiting into the
	// channel adapter would couple shell-streaming concerns into
	// every adapter (Feishu, Slack, …).
	//
	// On emitter failure the debouncer records drainErr and
	// cancels shellCtx so CommandContext kills the child;
	// otherwise the drainers stall on a wedged pipe and the
	// child keeps running for the rest of timeouts.Shell.
	//
	// drainErr is written from BOTH drainer goroutines (stdout +
	// stderr may both fail to deliver their respective chunks).
	// drainErrMu serialises the writes; we only need the value
	// stable by the time executeShell returns (wg.Wait inside
	// executeShell is the barrier), but -race flags the
	// unprotected writes so we lock explicitly. cancel() and
	// context.CancelFunc are themselves safe for concurrent use;
	// only the variable read/write needs the mutex.
	var (
		drainErrMu sync.Mutex
		drainErr   error
	)
	db := newChunkDebouncer(func(text string) error {
		if err := send(text); err != nil {
			drainErrMu.Lock()
			drainErr = err
			drainErrMu.Unlock()
			cancel()
			return err
		}
		return nil
	}, shellFlushInterval)
	defer db.Stop() // safety net: flush if anything panics before the explicit Stop below
	// db.Add matches the onChunk signature except for the
	// error return. coalesceLines's flush helper ignores the
	// return value (it captures it via the captured `err`
	// variable), so we bridge with a thin wrapper. The
	// debouncer's own send-error handling runs in the flush
	// callback above (drainErr path).
	r := executeShell(shellCtx, ir.Request.Cwd, cmd, nil, func(text string) error {
		db.Add(text)
		return nil
	})
	drainErrMu.Lock()
	gotDrainErr := drainErr
	drainErrMu.Unlock()

	// Flush any buffered chunks BEFORE the footer so the user
	// sees the final chunk before the exit-code summary.
	// db.Stop is idempotent — the defer above is a safety net
	// for panic paths only.
	db.Stop()

	// ③ footer — exit code + duration + cwd. Even if executeShell
	// panicked (recovered, ExitCode=-1) or was cancelled
	// (ExitCode=-1), the footer fires so the user sees a clean
	// ❌ instead of a stuck ⌨️ card.
	icon := "✅"
	if r.ExitCode != 0 {
		icon = "❌"
	}
	footer := fmt.Sprintf("\n%s exit %d · %dms · %s",
		icon, r.ExitCode, r.Duration.Milliseconds(), r.Cwd)
	if err := send(footer); err != nil && gotDrainErr == nil {
		slog.Default().Warn("shell: footer send failed",
			"err", err,
			"chat_id", ir.ChatID,
			"message_id", ir.MessageID)
	}
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
