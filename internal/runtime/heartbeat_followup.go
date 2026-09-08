package runtime

import (
	"context"
	"log/slog"

	"github.com/cnlangzi/nightme/internal/messages"
)

// sendHeartbeatFollowUp emits a single OutHeartbeat through em
// carrying the given snapshot. Shared by the three call sites
// that produce heartbeat follow-ups:
//
//   - runtime handler's Observe branch (F-63 §3.2 invariant —
//     counter increments that survive /think off / /tools off)
//   - runtime handler's OutResult branch (terminal ✅ prefix)
//   - runtime eventbus's OnPromptEnded subscriber (terminal
//     ✅ prefix for turns that exit without an OutResult)
//
// The source tag is included in the empty-snapshot drop log
// and the send-error log so a noisy channel's logs can be
// attributed to the trigger site. Snapshots are dropped
// silently when Empty() (HeartbeatSnapshot.Empty handles
// Done=true correctly — see its doc), saving a cross-channel
// round-trip and a log line per turn-start LRU eviction race.
// Caller passes the snapshot it already Snapshotted from the
// tracker; this helper does NOT touch the tracker.
//
// Returns silently on Send error (logged) — the runtime never
// blocks the originating event because the heartbeat follow-up
// couldn't be delivered. The channel adapter's own
// !msg.Heartbeat.Empty() gate is a second line of defence.
func sendHeartbeatFollowUp(
	em messages.Emitter,
	logger *slog.Logger,
	chatID, userMsgID, source string,
	snap messages.HeartbeatSnapshot,
) {
	if snap.Empty() {
		if logger != nil {
			logger.Debug("heartbeat dropped (empty snapshot)",
				"chat_id", chatID,
				"user_msg_id", userMsgID,
				"source", source)
		}
		return
	}
	hb := messages.OutboundMessage{
		ChatID:    chatID,
		Kind:      messages.OutHeartbeat,
		ReplyTo:   userMsgID,
		Heartbeat: &snap,
	}
	if err := em.Send(context.Background(), hb); err != nil && logger != nil {
		logger.Warn("heartbeat follow-up send failed",
			"chat_id", chatID,
			"user_msg_id", userMsgID,
			"source", source,
			"err", err)
	}
}
