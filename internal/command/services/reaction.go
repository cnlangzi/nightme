package services

import (
	"context"
	"log/slog"
	"sync"
)

// ReactionEvent is the inbound reaction / action payload.
// Canonical location is THIS file (services/reaction.go).
// command.ReactionEvent is a type alias to this type — see
// command/event.go for the rationale.
//
// This type was moved from chatsession (where it was a F-45
// bridge to gtw) and from internal/gtw (where it was a
// separate struct at types.go:163) as part of F-51. After
// F-51 lands, both `chatsession.ReactionEvent` and
// `gtw.ReactionEvent` are deleted; all callers reference this
// type directly or via the command.ReactionEvent alias.
type ReactionEvent struct {
	// TargetMsgID is the bot's message id that the user
	// reacted to. The reaction router's handler (e.g.
	// gtw.Manager) uses this to look up the relevant state
	// (e.g. a draft card).
	TargetMsgID string
	// Emoji is the raw reaction emoji ("✅" / "🆕" / "🔗" /
	// "❌" / "🔄" / "🤝" for gtw; varies per command).
	Emoji string
	// UserID is the user who reacted. Used for "force-take"
	// attribution and audit log entries.
	UserID string
	// ChatID is the chat the reaction originated in.
	// Required for rendering follow-up replies.
	ChatID string
}

// ReactionRouter dispatches one reaction event to the right
// handler. The runtime holds one shared router (singleton,
// process-wide); slash command packages (gtw, future /follow)
// register themselves at startup via Register.
type ReactionRouter interface {
	// Register binds a handler to a chatID. chatID == "*" means
	// "all chats" (gtw is global). A second Register for the
	// same chatID overwrites.
	Register(chatID string, handler func(ctx context.Context, ev ReactionEvent) bool)
	// Handle dispatches one reaction. Returns true if any
	// registered handler consumed the event; false if no
	// handler matched (caller may log + drop). Handler panics
	// are recovered and logged; handler returns are not
	// converted into errors (callers can't act on them anyway
	// — log + drop is the only sensible response).
	Handle(ctx context.Context, chatID string, ev ReactionEvent) bool
}

// reactionRouter is the concrete runtime impl. The runtime
// instantiates one via NewReactionRouter and shares it across
// the process. Thread-safe; uses a sync.RWMutex around the
// handler map.
type reactionRouter struct {
	mu       sync.RWMutex
	handlers map[string]func(ctx context.Context, ev ReactionEvent) bool
}

// NewReactionRouter returns a fresh, empty ReactionRouter.
func NewReactionRouter() ReactionRouter {
	return &reactionRouter{
		handlers: make(map[string]func(ctx context.Context, ev ReactionEvent) bool),
	}
}

// Register implements ReactionRouter.
func (r *reactionRouter) Register(chatID string, handler func(ctx context.Context, ev ReactionEvent) bool) {
	if chatID == "" || handler == nil {
		return
	}
	r.mu.Lock()
	r.handlers[chatID] = handler
	r.mu.Unlock()
}

// Handle implements ReactionRouter.
//
// Lookup order:
//  1. exact match (chatID)
//  2. wildcard ("*" if registered)
//
// Returns true if any matching handler consumed the event.
// Handler panics are recovered and logged.
func (r *reactionRouter) Handle(ctx context.Context, chatID string, ev ReactionEvent) bool {
	r.mu.RLock()
	handler, ok := r.handlers[chatID]
	if !ok {
		handler, ok = r.handlers["*"]
	}
	r.mu.RUnlock()
	if !ok || handler == nil {
		return false
	}
	consumed := invokeReactionHandler(ctx, handler, ev)
	return consumed
}

// invokeReactionHandler calls handler with panic-safety so a
// buggy gtw reaction handler can't crash the daemon.
func invokeReactionHandler(ctx context.Context, handler func(ctx context.Context, ev ReactionEvent) bool, ev ReactionEvent) (consumed bool) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Default().Error("ReactionRouter: handler panic recovered",
				"chat_id", ev.ChatID,
				"target_msg_id", ev.TargetMsgID,
				"emoji", ev.Emoji,
				"panic", rec)
			consumed = false
		}
	}()
	return handler(ctx, ev)
}
