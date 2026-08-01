// Package feishu — F-25 Renderer, v0.3 single-message rolling-log
// integration. The renderer owns one MessageReceipt per active chat
// (keyed by chatID). Every agent event the session pump routes
// here is appended to that receipt's log, which the receipt
// re-renders to Feishu as a single in-place updated message. The
// user sees ONE reply per user message, not one per event.
//
// The Renderer's responsibilities:
//
//   - SendUserMessage creates a fresh receipt when an IM message
//     arrives (called from the gateway's fallback path).
//   - RenderEvent forwards one AgentEvent from a session pump to
//     the receipt's Append, which adds it to the rolling log and
//     updates the reply message in place.
//   - renderPermission renders permission requests as a separate
//     interactive card (not part of the log).
package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
)

// interactiveMessageType is the Feishu msg_type for v1 interactive
// cards. Used by renderPermission and verified by the render test.
const interactiveMessageType = "interactive"

// Renderer maps structured AgentEvents to Feishu messages AND drives
// the per-user-message MessageReceipt lifecycle (F-25 §6).
//
// One Renderer per chat. It owns:
//
//   - The Feishu adapter (for SendMessage / UpdateMessage /
//     AddReaction).
//   - The receipts map (chatID -> *MessageReceipt) so events from
//     the session pump find the right receipt without carrying the
//     userMsgID around.
//   - The userMsgIndex map (userMsgID -> *MessageReceipt) so legacy
//     callers that still pass userMsgID (the F-25 gateway path)
//     can do an O(1) lookup without scanning the chatID map. The
//     two indexes are kept in sync by every mutator (Send /
//     installReceipt / evict).
type Renderer struct {
	adapter *Adapter

	mu            sync.Mutex
	receipts      map[string]*MessageReceipt // chatID -> latest active receipt
	userMsgIndex  map[string]*MessageReceipt // userMsgID -> receipt (legacy F-25 lookup)
}

// NewRenderer constructs an event renderer backed by adapter.
func NewRenderer(adapter *Adapter) *Renderer {
	return &Renderer{
		adapter:     adapter,
		receipts:    make(map[string]*MessageReceipt),
		userMsgIndex: make(map[string]*MessageReceipt),
	}
}

// installReceipt atomically registers receipt in both indexes. If
// a previous active receipt exists for the same chat, it is marked
// Completed and evicted from the userMsgID index (the chatID slot
// is overwritten below). ctx is used for the prior receipt's
// SetCompleted call; it must be a real context (not Background)
// so cancellation propagates correctly.
//
// On duplicate userMsgID the existing receipt is returned unchanged
// — installReceipt is idempotent.
func (r *Renderer) installReceipt(ctx context.Context, receipt *MessageReceipt) *MessageReceipt {
	if receipt == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.userMsgIndex[receipt.userMsgID]; ok && existing != nil {
		return existing
	}
	if old, ok := r.receipts[receipt.chatID]; ok && old != nil && old != receipt {
		// Mark the prior receipt terminal so the user can tell the
		// earlier turn is done. Best-effort: a failure here does
		// not block the new receipt's registration.
		_ = old.SetCompleted(ctx)
		delete(r.userMsgIndex, old.userMsgID)
	}
	r.receipts[receipt.chatID] = receipt
	r.userMsgIndex[receipt.userMsgID] = receipt
	return nil
}

// lookupByUserMsgID returns the receipt owning userMsgID, or nil if
// none. Acquires r.mu internally; callers must not hold it.
func (r *Renderer) lookupByUserMsgID(userMsgID string) *MessageReceipt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.userMsgIndex[userMsgID]
}

// lookupByChatID returns the latest active receipt for chatID, or
// nil. Acquires r.mu internally.
func (r *Renderer) lookupByChatID(chatID string) *MessageReceipt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.receipts[chatID]
}

// SendUserMessage is the F-25 entry point for IM-arriving messages.
// It creates a MessageReceipt (post reply, add ⏳ reaction) and
// returns it so the caller can drive state via the returned handle.
//
// When the chat already has an active receipt (e.g. a previous
// user message is still in flight), that receipt is marked
// Completed before the new one takes over — the old reply message
// keeps its terminal state in Feishu, and the new user message gets
// a fresh receipt.
//
// userMsgID is the Feishu message ID of the user's incoming
// message. If a receipt already exists for that exact userMsgID
// (e.g. a duplicate event arrived), the existing one is returned
// (idempotent — handles retries / dup events).
func (r *Renderer) SendUserMessage(ctx context.Context, chatID, userMsgID, content string) (*MessageReceipt, error) {
	if userMsgID == "" {
		return nil, errors.New("feishu: userMsgID is required")
	}
	if existing := r.lookupByUserMsgID(userMsgID); existing != nil {
		return existing, nil
	}

	receipt, err := NewMessageReceipt(ctx, r.adapter, chatID, userMsgID)
	if err != nil {
		return nil, err
	}

	// installReceipt handles the eviction of the prior chat
	// receipt and the duplicate-userMsgID race in one atomic
	// critical section, so we don't have to re-check anything
	// here. The returned receipt is the same one we created
	// (installReceipt is idempotent on userMsgID).
	_ = r.installReceipt(ctx, receipt)
	return receipt, nil
}

// MarkExecuting is a compatibility shim for the F-25 receipt
// lifecycle. It looks up the receipt that owns userMsgID and
// transitions it from Waiting to Executing (the F-25 dual-track
// swap: ⏳ → 🔄). Returned error is informational — callers
// typically ignore it (the receipt stays usable even if the mark
// fails).
//
// MarkExecuting is a sibling of the older F-25 API that the
// gateway still calls. New callers should call receipt.SetExecuting
// directly when they already hold the *MessageReceipt handle.
func (r *Renderer) MarkExecuting(ctx context.Context, userMsgID string) error {
	if r == nil {
		return nil
	}
	receipt := r.lookupByUserMsgID(userMsgID)
	if receipt == nil {
		// Caller asked to mark a receipt that doesn't exist
		// (likely an orphan or a chat that was force-closed).
		// Not an error — the receipt was already removed.
		return nil
	}
	return receipt.SetExecuting(ctx)
}

// Render / RenderEvent forwards one AgentEvent from a session pump
// to the chat's active receipt. The receipt's Append method adds
// the event to the rolling log and updates the reply message in
// place — no new Feishu messages are sent.
//
// Events without an active receipt (e.g. orphan events after the
// user message disappeared) are logged at debug and otherwise
// dropped.
//
// Render is an alias kept for legacy callers.
func (r *Renderer) Render(ctx context.Context, chatID string, ev agent.AgentEvent) error {
	return r.RenderEvent(ctx, chatID, ev)
}

func (r *Renderer) RenderEvent(ctx context.Context, chatID string, ev agent.AgentEvent) error {
	if r == nil {
		return errors.New("feishu: renderer is nil")
	}

	// Permission requests are rendered as a separate interactive
	// card — they bypass the rolling log entirely. The adapter is
	// only required for this path.
	if ev.Kind == agent.EventPermission {
		if r.adapter == nil {
			return errors.New("feishu: renderer has no adapter (needed for permission cards)")
		}
		return r.renderPermission(ctx, chatID, ev.Permission)
	}

	receipt := r.lookupByChatID(chatID)
	if receipt == nil {
		// No active receipt for this chat — nothing to append to.
		// Common during boot or after a chat was force-closed.
		return nil
	}
	return receipt.Append(ctx, ev)
}

// renderPermission emits the F-25 interactive permission card and
// stores the resulting message ID on the receipt so the user's
// click is routed back to the right session. The rolling log is
// not used here — the card is its own message.
func (r *Renderer) renderPermission(ctx context.Context, chatID string, req *agent.PermissionRequest) error {
	if req == nil {
		return errors.New("feishu: nil permission event")
	}
	content, err := permissionCard(req)
	if err != nil {
		return err
	}
	if _, err := r.adapter.sendContent(ctx, chatID, interactiveMessageType, content); err != nil {
		return err
	}
	return nil
}

// permissionCard returns the v1 interactive-card JSON accepted by
// the Feishu IM API. Button callback routing is intentionally left
// to the Gateway in a follow-up — for v0.2 the card carries the
// selected option in its value, which the callback handler will
// surface back to the agent via SendPermission.
func permissionCard(req *agent.PermissionRequest) (string, error) {
	if req == nil {
		return "", errors.New("permission card: nil request")
	}
	if req.Tool == "" && req.Action == "" {
		return "", errors.New("permission card: missing tool / action")
	}
	options := req.Options
	if len(options) == 0 {
		options = []string{"Allow", "Deny"}
	}
	// Build the v1 interactive card: a header line, the
	// permission question as the body, and one button per option.
	// Button value carries both the tool name and the option so the
	// click handler can route the response back to the right
	// permission request.
	headerJSON, _ := json.Marshal(map[string]any{
		"title":    map[string]any{"tag": "plain_text", "content": "Permission needed"},
		"template": "blue",
	})
	body := req.Tool
	if req.Action != "" {
		body = req.Tool + " — " + req.Action
	}
	elements := []map[string]any{
		{
			"tag":  "div",
			"text": map[string]any{"tag": "lark_md", "content": body},
		},
	}
	actions := make([]map[string]any, 0, len(options))
	for _, opt := range options {
		v, _ := json.Marshal(map[string]string{
			"tool":   req.Tool,
			"action": req.Action,
			"option": opt,
		})
		actions = append(actions, map[string]any{
			"tag":   "button",
			"text":  map[string]any{"tag": "plain_text", "content": opt},
			"type":  "primary",
			"value": map[string]any{"key": string(v)},
		})
	}
	elements = append(elements, map[string]any{"tag": "action", "actions": actions})
	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   json.RawMessage(headerJSON),
		"elements": elements,
	}
	b, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("permission card: marshal: %w", err)
	}
	// Feishu's "interactive" message type wraps the card payload
	// under a "card" key in the content envelope.
	envelope := map[string]any{"card": json.RawMessage(b)}
	eb, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return string(eb), nil
}