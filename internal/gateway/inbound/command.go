// Package inbound — tryCommandDispatch: routes /-prefixed
// text through the wired command.Commander.
//
// Priority 2: runs after tryActionDispatch so reaction events
// never reach the commander, and before tryMessageDispatch so
// recognised slash commands short-circuit the per-chat
// WatchMode gate (otherwise applied by chatsession inside
// chatsession.Manager.HandleInbound). The gate is a no-op for
// slash commands — recognised commands return here,
// unrecognised /-inputs fall through to the message branch
// with the original text intact.
//
// Unlike the v0.x shim, this implementation calls the
// commander directly. The translation *messages.InboundMessage
// → command.SlashInput happens inline (the field shapes are
// intentionally close to avoid a separate adapter type).
//
// F-59 async dispatch: the priority chain runs synchronously
// in the dispatchLoop monitor so the monitor is never
// blocked. The chain calls Commander.Match (a cheap, pure
// routing query — see command.Commander doc) to decide
// synchronously whether the slash branch should claim the
// inbound; if yes, the actual commander.Dispatch runs in a
// spawned goroutine and emits its reply via the wired
// Emitter. The monitor's Reply-emit block (formerly in
// gateway.dispatchLoop) is gone — replies are written from
// inside the goroutine so a slow /gtw commit never blocks
// inbound consumption for any chat.
package inbound

import (
	"context"
	"log/slog"

	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// tryCommandDispatch claims /-prefixed text via commander.Match.
// Falls through (handled=false) when Match reports no — i.e.
// the input is not a slash command, or is a slash command with
// no registered factory (e.g. "/etc/passwd"). When Match
// reports yes, it spawns a goroutine that runs commander.Dispatch
// and emits the reply, then returns handled=true synchronously.
func (r *Router) tryCommandDispatch(ctx context.Context, msg *messages.InboundMessage) (bool, *CommandResult, error) {
	commander, ok := r.requireCommander()
	if !ok {
		return false, nil, nil
	}
	// F-59 sync routing decision — Match is cheap (string trim +
	// Fields + map lookup), no IO, no state mutation. Keeps the
	// priority chain's handled signal synchronous so the
	// dispatchLoop monitor never blocks on cmd.Handle (which can
	// be minutes-long for /gtw commit / /gtw pr).
	if _, matched := commander.Match(msg.Text); !matched {
		return false, nil, nil
	}

	// F-59 async dispatch — commander.Dispatch + reply emit run
	// in a worker goroutine. monitor is unblocked.
	//
	// F-59 ctx policy: the goroutine runs with context.Background()
	// (NOT the inbound ctx). Mirrors shell.Dispatcher.Handle's
	// design — "the spawned goroutine intentionally outlives any
	// inbound-ctx cancellation, including daemon shutdown". A
	// /gtw commit running when the user SIGINTs should finish its
	// agent turn, not be killed mid-stream. The goroutine still
	// dies with the process; goroutines leaked past process
	// exit are not a real concern.
	r.execWg.Add(1)
	go func() {
		defer r.execWg.Done()
		r.runCommand(context.Background(), commander, msg)
	}()

	// F-51 fall-through contract preserved: we already know this
	// input is a recognised slash command (Match returned true),
	// so Consumed=true, Dropped=false is the correct
	// dispatchLoop-level signal. Reply field is empty by design —
	// the reply is emitted asynchronously from runCommand below,
	// not via dispatchLoop's reply-emit block (which was removed
	// in F-59).
	return true, &CommandResult{Consumed: true}, nil
}

// runCommand is the body of the goroutine spawned by
// tryCommandDispatch. Resolves the ChatSession, invokes
// commander.Dispatch, and writes the resulting reply through
// the wired Emitter. Panic-safe so a misbehaving command
// factory cannot crash the daemon.
//
// The emitter used here is r.emitter (captured at Router
// construction). Emitting from inside the goroutine is what
// makes the dispatch non-blocking — outbound messages
// arriving from long-running commands no longer block
// dispatchLoop.
func (r *Router) runCommand(ctx context.Context, commander CommandDispatcher, msg *messages.InboundMessage) {
	// Defer recover MUST be first so a panic in GetOrCreate
	// (e.g. nil deref in chatsession code) is caught and the
	// goroutine doesn't crash the daemon. Matches runAction /
	// runMessage which also recover at function entry.
	defer func() {
		if rec := recover(); rec != nil {
			slog.Default().Error("inbound: runCommand panicked",
				"chat_id", msg.ChatID, "panic", rec)
			r.emitReply(ctx, msg, "❌ internal error (see daemon log)")
		}
	}()

	cs, err := r.csMgr.GetOrCreate(msg.ChatID, r.primary)
	if err != nil || cs == nil {
		slog.Default().Warn("inbound: GetOrCreate failed in runCommand",
			"chat_id", msg.ChatID, "err", err)
		return
	}

	out, _, err := commander.Dispatch(ctx, command.RuntimeServices{
		// RuntimeServices currently carries only Logger + Config
		// (see command/runtime.go). The runtime wires the
		// concrete services it needs (primary agent, …) at
		// construction; here we just pass the same shape.
		// F-58 TODO: thread primary agent through RuntimeServices
		// explicitly; current Commander implementations don't
		// need it (they read cs.SelectedAgent()).
	}, cs, command.SlashInput{
		ChatID:     msg.ChatID,
		UserID:     msg.UserID,
		Text:       msg.Text,
		MessageID:  msg.MessageID,
		HasMention: msg.HasMention,
	})
	if err != nil {
		r.emitReply(ctx, msg, "❌ "+err.Error())
		return
	}
	if out == nil {
		return
	}
	// SlashOutput.Outbound is the canonical multi-message reply
	// channel (PATCH cards, sequential cards, etc). The runtime
	// shim contract — see internal/command/event.go
	// SlashOutput.Outbound doc — is "forward each via the chat
	// session's Emitter". routeOutbound below mirrors the
	// replyingCommander shim in internal/command/e2e_slash_test.go
	// so production behaviour matches the documented contract.
	r.routeOutbound(ctx, out.Outbound)
	if out.Reply != "" {
		r.emitReply(ctx, msg, out.Reply)
	}
}

// emitReply writes a single outbound reply to the wired
// Emitter, anchored to the original user message so the
// channel renders it as a thread reply (Feishu) / in-place
// edit (Slack) / DOM append (Web). Shared by runCommand,
// runAction, runShell, runMessage and the F-59 fallback paths.
func (r *Router) emitReply(ctx context.Context, msg *messages.InboundMessage, text string) {
	if text == "" || r.emitter == nil {
		return
	}
	if sendErr := r.emitter.Send(ctx, messages.OutboundMessage{
		ChatID:  msg.ChatID,
		Kind:    messages.OutReply,
		Text:    text,
		ReplyTo: msg.MessageID,
	}); sendErr != nil {
		slog.Default().Warn("inbound: emit reply failed",
			"chat_id", msg.ChatID, "err", sendErr)
	}
}

// routeOutbound forwards the explicit Outbound list from a
// SlashOutput through the wired Emitter, preserving the
// runtime-shim contract documented on SlashOutput.Outbound
// (internal/command/event.go). Each entry is routed based on
// its Kind / Card: OutCardPatch → Send (PATCH semantics fold
// into Send with Kind=OutCardPatch), Card != nil → SendCard,
// everything else → Send. Mirrors the test shim's contract in
// internal/command/e2e_slash_test.go's replyingCommander so
// production behaviour matches the documented intent.
func (r *Router) routeOutbound(ctx context.Context, outbound []messages.OutboundMessage) {
	if len(outbound) == 0 || r.emitter == nil {
		return
	}
	for _, ob := range outbound {
		switch {
		case ob.Kind == messages.OutCardPatch:
			if err := r.emitter.Send(ctx, ob); err != nil {
				slog.Default().Warn("inbound: outbound PATCH send failed", "err", err)
			}
		case ob.Card != nil:
			if _, err := r.emitter.SendCard(ctx, ob); err != nil {
				slog.Default().Warn("inbound: outbound SendCard failed", "err", err)
			}
		default:
			if err := r.emitter.Send(ctx, ob); err != nil {
				slog.Default().Warn("inbound: outbound Send failed", "err", err)
			}
		}
	}
}