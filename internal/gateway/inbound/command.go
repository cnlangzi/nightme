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

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// tryCommandDispatch claims /-prefixed text via commander.Match.
// Falls through (handled=false) when Match reports no — i.e.
// the input is not a slash command, or is a slash command with
// no registered factory (e.g. "/etc/passwd"). When Match
// reports yes, it spawns a goroutine that runs commander.Dispatch
// and emits the reply, then returns handled=true synchronously.
func (r *Router) tryCommandDispatch(ctx context.Context, mgr *chatsession.Manager, msg *messages.InboundMessage) (bool, *CommandResult, error) {
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
		r.runCommand(context.Background(), mgr, commander, msg)
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
// the per-call Emitter (cs.Emitter()) so replies land on the
// originating channel. r.emitter is kept only as the fallback
// for paths that never resolve a ChatSession (e.g. early errors).
func (r *Router) runCommand(ctx context.Context, mgr *chatsession.Manager, commander CommandDispatcher, msg *messages.InboundMessage) {
	// Defer recover MUST be first so a panic in GetOrCreate
	// (e.g. nil deref in chatsession code) is caught and the
	// goroutine doesn't crash the daemon. Matches runAction /
	// runMessage which also recover at function entry.
	defer func() {
		if rec := recover(); rec != nil {
			slog.Default().Error("inbound: runCommand panicked",
				"chat_id", msg.ChatID, "panic", rec)
			r.emitCommandReply(ctx, nil, msg, "❌ internal error (see daemon log)")
		}
	}()

	// v1.3+ multi-channel: resolve the ChatSession through the
	// per-channel mgr passed by the pump's closure, not the
	// router-level csMgr stub (which is the legacy noOpMgr).
	cs, err := mgr.GetOrCreate(msg.ChatID, r.primary)
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
	}, mgr, cs, command.SlashInput{
		ChatID:     msg.ChatID,
		UserID:     msg.UserID,
		Text:       msg.Text,
		MessageID:  msg.MessageID,
		HasMention: msg.HasMention,
	})
	if err != nil {
		r.emitCommandReply(ctx, cs, msg, "❌ "+err.Error())
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
	r.routeOutbound(ctx, cs, out.Outbound)
	if out.Reply != "" {
		r.emitCommandReply(ctx, cs, msg, out.Reply)
	}
}

// resolveEmitter picks the per-channel Emitter from cs (set by
// buildStack for each channel) and falls back to the router's
// fallback Emitter when cs is nil or has no Emitter bound.
// The fallback path is what tests that drive the dispatch chain
// directly (with a stub router csMgr) rely on; production
// always uses the per-channel Emitter.
func (r *Router) resolveEmitter(cs *chatsession.ChatSession) messages.Emitter {
	if cs != nil {
		if em := cs.Emitter(); em != nil {
			return em
		}
	}
	return r.emitter
}

// emitCommandReply writes a single one-shot command reply through
// the per-channel Emitter (cs.Emitter()) so the message lands on
// the originating channel. Falls back to r.emitter when cs is nil
// or has no Emitter bound (which is rare in production — every
// runX takes a ChatSession path; tests that drive the dispatch
// chain directly are the common fallback user).
//
// The Kind is OutCommandReply (not OutReply) because every call
// site is a short, static, one-shot text — slash command
// confirmations from internal/command/<name>/cmd.go, runtime
// errors surfaced to the user, and the panic-recovery breadcrumb.
// OutReply is reserved for the agent's streaming reply pipeline
// (chatsession.EventHandler → translate.OutReply → receipt card);
// command replies must NOT enter that pipeline because they are
// not part of the agent turn (they arrive before/after the turn,
// or in error paths where no turn exists at all).
//
// Channel-side routing for OutCommandReply is documented per
// channel: Feishu renders it as a top-level Create (ReplyInChat,
// `TestSend_OutCommandReply_TopLevelCreate_EmojiPrefixed`),
// Telegram appends to the active chain segment, bot folds the
// payload into its run-reply channel, and Slack uses it for the
// `chat.postMessage` parent-thread path of `/cwd` / `/use` / `/new`
// / `/stop`. See docs/channel/slack.md §4 and
// internal/messages/outbound.go OutCommandReply doc.
func (r *Router) emitCommandReply(ctx context.Context, cs *chatsession.ChatSession, msg *messages.InboundMessage, text string) {
	em := r.resolveEmitter(cs)
	if text == "" || em == nil {
		return
	}
	if sendErr := em.Send(ctx, messages.OutboundMessage{
		ChatID:  msg.ChatID,
		Kind:    messages.OutCommandReply,
		Text:    text,
		ReplyTo: msg.MessageID,
	}); sendErr != nil {
		slog.Default().Warn("inbound: emit reply failed",
			"chat_id", msg.ChatID, "err", sendErr)
	}
}

// routeOutbound forwards the explicit Outbound list from a
// SlashOutput through the per-channel Emitter (cs.Emitter()).
// Every kind — including OutChoice / OutChoicePatch — goes
// through Send; Channel routes by Kind and correlates choice
// prompts via Choice.RequestID.
func (r *Router) routeOutbound(ctx context.Context, cs *chatsession.ChatSession, outbound []messages.OutboundMessage) {
	em := r.resolveEmitter(cs)
	if len(outbound) == 0 || em == nil {
		return
	}
	for _, ob := range outbound {
		if err := em.Send(ctx, ob); err != nil {
			slog.Default().Warn("inbound: outbound Send failed", "kind", ob.Kind, "err", err)
		}
	}
}
