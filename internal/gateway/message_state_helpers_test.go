package gateway

import (
	"context"
	"log"

	"github.com/cnlangzi/nightme/internal/agent"
)

// emitMessageState is the v1.3 (F-31) legacy translation path
// preserved as a test helper. Production code uses the runtime's
// MessageStateBus subscriber (cmd/nightme/run.go) which adds the
// F-48 SessionContext stamp; this helper is the un-stamped
// equivalent, kept around so tests that target the translation
// logic itself don't have to wire a full ChatSession.
//
// Failure semantics (per F-31 §9):
//   - No channel for chatID: silent drop (debug log only).
//   - Channel.Send error: log warn, never block caller.
//   - Empty chatID or userMsgID: silent drop.
//
// Lives in _test.go so it does not pollute the production
// gateway package surface.
func emitMessageState(gw *Router, chatID, userMsgID string, state agent.MessageState) {
	if chatID == "" || userMsgID == "" {
		return
	}
	ch := gw.resolveChannel(chatID)
	if ch == nil {
		log.Printf("gateway: emitMessageState no channel for chat=%s, dropping", chatID)
		return
	}
	out := OutboundMessage{
		Kind:    OutMessageState,
		ChatID:  chatID,
		ReplyTo: userMsgID, // anchor for Typing placeholder + AddReaction target
		MessageState: &MessageStatePayload{
			State:     state,
			MessageID: userMsgID,
		},
	}
	if err := ch.Send(context.Background(), out); err != nil {
		log.Printf("gateway: MessageState send failed (chat=%s, state=%s): %v", chatID, state, err)
	}
}