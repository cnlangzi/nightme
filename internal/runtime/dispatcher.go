// dispatcher.go — the default-branch inbound message handler.
//
// Lives here (not in cmd/nightme) so the inbound.Router can
// construct it via a small closure in its constructor without
// pulling in the runtime package directly (Router imports
// chatsession / command / shell / services; the dispatcher is
// just a chatsession-bound function).
//
// The dispatcher is exported as NewMessageDispatcher because
// future inbound variants (e.g. a no-queue dry-run, or a
// hookable test dispatcher) may want to install it on a
// hand-rolled Router.

package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
)

// MessageDispatchFunc is the type returned by NewMessageDispatcher.
// Named so the signature is readable at call sites (vs. an
// inline `func(context.Context, *messages.InboundMessage) error`
// that requires the reader to count parens).
type MessageDispatchFunc = func(context.Context, *messages.InboundMessage) error

// NewMessageDispatcher builds the runtime-injected
// messageDispatcher (the default branch of the inboundDispatcher).
// It is invoked when no slash command matches; it routes the
// inbound message to the chat's active AgentSession via the
// InputBuffer.
//
// The ch parameter is used as the BuildBlocks source for the
// "msg.Blocks empty" fallback path (Phase 2.2: every Channel
// implements BuildBlocks itself, so the dispatcher stays
// channel-agnostic). Pass nil only in tests that never
// trigger the fallback.
//
// Flow:
//
//  1. cs = mgr.GetOrCreate(chatID, cfg.Primary)   // F-33: chatType removed
//  2. cs.LookupSelectedAgentSession() (lazy spawn)
//  3. cs.QueueUserMessage(blocks, userMsgID) (Idle → flush now;
//     Busy → queue)
//  4. SetBusy on first event (drive FSM)
func NewMessageDispatcher(mgr *chatsession.Manager, em outbound.Emitter, ch channel.Channel, primary string, logger *slog.Logger) MessageDispatchFunc {
	return func(ctx context.Context, msg *messages.InboundMessage) error {
		if msg == nil {
			return nil
		}
		// F-watch §3.1.1: per-chat WatchMode gate (formerly in
		// gateway.applyWatchModeGate). Lives here now so the
		// policy sits next to its state — chatsession owns both
		// the WatchMode field and the AcceptInbound decision.
		// Drop early, before any GetOrCreate / spawn work, so
		// filtered messages don't allocate state or wake pumps.
		// Slash commands never reach this branch (the commander
		// shim returns first inside DispatchInbound).
		if !mgr.AcceptInbound(msg.ChatID, msg.HasMention) {
			slog.Default().Info("dispatcher: drop non-mention group message (WatchMode != All)",
				"chat_id", msg.ChatID, "message_id", msg.MessageID)
			return nil
		}
		userMsgID := msg.MessageID
		if userMsgID == "" {
			userMsgID = msg.UserID + ":" + msg.Time.UTC().Format(time.RFC3339Nano)
		}

		cs, _ := mgr.GetOrCreate(msg.ChatID, primary)

		// F-31 / F-53: ChatSession has accepted the message. Emit
		// MessageQueued synchronously so the channel can render
		// ⏳ even before spawn resolves (FastAck UX).
		cs.EmitMessageState(userMsgID, agent.MessageQueued)

		// Resolve active AgentSession (lazy spawn on miss).
		_, err := cs.LookupSelectedAgentSession()
		if err != nil {
			if errors.Is(err, chatsession.ErrNoSelectedCwd) {
				return em.Send(ctx, messages.OutboundMessage{
					ChatID: msg.ChatID,
					Kind:   messages.OutReply,
					Text:   "No workspace set. Send /cwd <path> first.",
				})
			}
			// Spawn failed (binary missing, etc.); let the user know.
			return em.Send(ctx, messages.OutboundMessage{
				ChatID: msg.ChatID,
				Kind:   messages.OutReply,
				Text:   fmt.Sprintf("Failed to spawn agent: %v", err),
			})
		}

		// CS-AS 边界重构 Phase 1: readpump is now per-AS (started
		// by Spawn inside AgentSession). The chat layer just consumes
		// the enriched event stream via cs.PumpEvents (launched in
		// wireRuntimeCallbacksAndRestore). No StartReadPump call here
		// — the old per-CS readpump file is gone.

		// F-31 / F-53: dispatch successful — message has reached the
		// AgentSession. Emit MessageSubmitted so the channel flips
		// ⏳ → 🔄. (Emitted before QueueUserMessage so the visual
		// transition is visible even if queueing is slow.)
		cs.EmitMessageState(userMsgID, agent.MessageSubmitted)

		// Build structured blocks and queue to InputBuffer.
		// F-14 v1.4b: post rich-text messages arrive with
		// msg.Blocks already populated (ordered by Feishu paragraph)
		// and LocalPath back-filled. Prefer msg.Blocks when non-nil;
		// otherwise fall back to ch.BuildBlocks(msg.Text,
		// msg.Attachments) (single-resource msg_types). The
		// channel-specific fallback now lives on the Channel
		// interface (Phase 2.2), so this dispatcher is
		// channel-agnostic — Feishu paragraphs vs. plain text
		// is each adapter's own concern.
		var blocks []agent.ContentBlock
		var blocksPath string
		if len(msg.Blocks) > 0 {
			blocks = msg.Blocks
			blocksPath = "ordered_blocks"
		} else if ch != nil {
			blocks = ch.BuildBlocks(msg.Text, msg.Attachments)
			blocksPath = "channel_build_blocks"
		}
		// F-14 visibility: before queuing, trace what the agent will
		// actually receive. Specifically: if blocks only contains
		// ContentText (no ContentImage/File), the build layer dropped
		// the attachments — most likely DownloadAttachments was not
		// called upstream. With logging.level=debug this line shows the
		// block types + total length so we can pinpoint the loss layer.
		if logger != nil {
			types := make([]string, 0, len(blocks))
			for _, b := range blocks {
				types = append(types, string(b.Type))
			}
			logger.Debug("dispatcher: blocks built for queue",
				"chat_id", msg.ChatID,
				"user_msg_id", userMsgID,
				"path", blocksPath,
				"inbound_attachments", len(msg.Attachments),
				"block_count", len(blocks),
				"block_types", types,
			)
		}
		// F-53: build the per-message domain object. The
		// `ReceivedAt` is set to the inbound timestamp so log /
		// debug surfaces see the true arrival time (not the
		// dispatcher-pass time, which may be a hair later when
		// the spawn path took a moment).
		userMsg := chatsession.Message{
			ID:         userMsgID,
			ChatID:     msg.ChatID,
			Blocks:     blocks,
			ReceivedAt: msg.Time,
		}
		if err := cs.QueueUserMessage(userMsg); err != nil {
			if errors.Is(err, chatsession.ErrQueueFull) {
				return em.Send(ctx, messages.OutboundMessage{
					ChatID: msg.ChatID,
					Kind:   messages.OutReply,
					Text:   "Input queue full — the agent is behind. Wait for it to catch up before sending more.",
				})
			}
			return err
		}
		return nil
	}
}