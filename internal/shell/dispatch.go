// Package shell dispatches `!cmd` user input to a real shell
// (sh -c on Unix, cmd /c on Windows). All shell-related logic
// — prefix detection, async execution, streaming reply — lives
// here. The gateway sees only a Handle → Consumed-bool contract.
//
// Routes:
//
//	User text "!" → shell package (this file)
//	User text "/" → command package (commander.go)
//	Other         → agent prompt
//
// Platform-specific execution lives in dispatch_unix.go /
// dispatch_windows.go (build-tag isolated).
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

// shellFlushInterval caps the chunk send rate at the channel
// boundary.
const shellFlushInterval = 1500 * time.Millisecond

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

func (d *chunkDebouncer) Add(text string) {
	d.mu.Lock()
	d.buf.WriteString(text)
	d.mu.Unlock()
}

func (d *chunkDebouncer) Stop() {
	select {
	case <-d.stop:
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

// result is the outcome of executeShell. Stdout / Stderr hold
// the full captured output even when chunks were streamed via
// onChunk; ExitCode / Duration / Cwd describe the run.
type result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	Cwd      string
}

// ShellOutput is the framework-level return of Dispatcher.Handle.
// Reply is intentionally absent: shell replies are async (posted
// by the runShell goroutine, not by Handle's caller), so the
// runtime shim has nothing to translate.
type ShellOutput struct {
	Consumed bool
}

// Dispatcher is stateless and safe for concurrent use.
type Dispatcher struct{}

func NewDispatcher() *Dispatcher { return &Dispatcher{} }

// Handle is the package's single runtime entry point.
//
//	!cmd not matched → return (nil, false) — gateway falls through
//	                 to tryMessageDispatch.
//	!cmd matched     → emit framework ⏳, spawn runShell goroutine,
//	                 return (&ShellOutput{Consumed: true}, true).
//
// The async goroutine uses context.Background() so the shell
// command outlives any inbound-ctx cancellation.
func (d *Dispatcher) Handle(mgr *chatsession.Manager, cs *chatsession.ChatSession, ir InboundRequest) (*ShellOutput, bool) {
	if _, matched := parseShell(ir.Text); !matched {
		return nil, false
	}
	chatsession.PublishMessageState(cs, ir.MessageID, agent.MessageQueued)
	go d.runShell(mgr, cs, ir)
	return &ShellOutput{Consumed: true}, true
}

// runShell streams the command via three OutReply messages:
//   1. header — "⌨️ $ <cmd>\n", cold-starts the receipt card
//   2. chunks — coalesced output, debounced at shellFlushInterval
//   3. footer — "\n✅/❌ exit <code> · <ms>ms · <cwd>"
//
// On emitter failure runShell cancels shellCtx so CommandContext
// kills the child (otherwise drainers stall on a wedged pipe).
//
// drainErrMu serialises drainErr writes from both drainer
// goroutines; cancel() is itself safe for concurrent use.
func (d *Dispatcher) runShell(mgr *chatsession.Manager, cs *chatsession.ChatSession, ir InboundRequest) {
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

	if err := send(fmt.Sprintf("⌨️ $ %s\n", cmd)); err != nil {
		slog.Default().Warn("shell: header send failed",
			"err", err, "chat_id", ir.ChatID, "message_id", ir.MessageID)
	}

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
	r := executeShell(shellCtx, ir.Request.Cwd, cmd, nil, func(text string) error {
		db.Add(text)
		return nil
	})
	drainErrMu.Lock()
	gotDrainErr := drainErr
	drainErrMu.Unlock()

	// Flush buffered chunks before the footer so the user sees
	// the final output before the exit-code summary. db.Stop is
	// idempotent — the defer above is the panic-path safety net.
	db.Stop()

	icon := "✅"
	if r.ExitCode != 0 {
		icon = "❌"
	}
	footer := fmt.Sprintf("\n%s exit %d · %dms · %s",
		icon, r.ExitCode, r.Duration.Milliseconds(), r.Cwd)
	if err := send(footer); err != nil && gotDrainErr == nil {
		slog.Default().Warn("shell: footer send failed",
			"err", err, "chat_id", ir.ChatID, "message_id", ir.MessageID)
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
