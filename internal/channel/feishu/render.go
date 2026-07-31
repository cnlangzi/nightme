package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

const interactiveMessageType = "interactive"

// Renderer maps structured AgentEvents to Feishu messages AND drives
// the per-user-message MessageReceipt lifecycle (F-25 §6).
//
// One Renderer per chat. It owns:
//   - The Feishu adapter (for SendMessage / UpdateMessage / AddReaction)
//   - The receipts map (userMsgID -> *MessageReceipt) so EventDone
//     can mark every still-open receipt as Completed.
type Renderer struct {
	adapter *Adapter

	mu       sync.Mutex
	receipts map[string]*MessageReceipt // userMsgID -> receipt
}

// NewRenderer constructs an event renderer backed by adapter.
func NewRenderer(adapter *Adapter) *Renderer {
	return &Renderer{
		adapter:  adapter,
		receipts: make(map[string]*MessageReceipt),
	}
}

// SendUserMessage is the F-25 entry point for IM-arriving messages.
// It creates a MessageReceipt (post reply, add ⏳ reaction) and
// returns the receipt so the caller can later drive state via the
// returned handle.
//
// The caller is expected to invoke MessageReceipt.SetExecuting on
// dispatch (so the receipt transitions to 🔄) and Heartbeat on each
// subsequent agent event. EventDone triggers SetCompleted on every
// open receipt automatically via the AgentEvent pump.
//
// userMsgID is the Feishu message ID of the user's incoming message.
// If the renderer has already seen this userMsgID, the existing
// receipt is returned (idempotent — handles retries / dup events).
func (r *Renderer) SendUserMessage(ctx context.Context, chatID, userMsgID, content string) (*MessageReceipt, error) {
	if userMsgID == "" {
		return nil, errors.New("feishu: userMsgID is required")
	}

	r.mu.Lock()
	if existing, ok := r.receipts[userMsgID]; ok {
		r.mu.Unlock()
		return existing, nil
	}
	r.mu.Unlock()

	receipt, err := NewMessageReceipt(ctx, r.adapter, chatID, userMsgID)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	// Double-check after lock — another goroutine may have raced.
	if existing, ok := r.receipts[userMsgID]; ok {
		r.mu.Unlock()
		return existing, nil
	}
	r.receipts[userMsgID] = receipt
	r.mu.Unlock()

	return receipt, nil
}

// RenderEvent handles the F-25 EventPump integration:
//   - EventText / EventToolStart / EventToolEnd: heartbeat every
//     active receipt (so the "🔄 ⏳ N · HH:MM:SS" counter ticks)
//   - EventDone: mark every active receipt as Completed
//   - EventError: same as EventDone (terminal)
//
// RenderEvent also surfaces the structured content via SendMessage
// / sendContent for the user to actually see Claude's output.
//
// Alias for Render kept for legacy callers.
func (r *Renderer) Render(ctx context.Context, chatID string, ev agent.AgentEvent) error {
	return r.RenderEvent(ctx, chatID, ev)
}

func (r *Renderer) RenderEvent(ctx context.Context, chatID string, ev agent.AgentEvent) error {
	if r == nil || r.adapter == nil {
		return errors.New("feishu: renderer has no adapter")
	}

	switch ev.Kind {
	case agent.EventText:
		r.heartbeatAll(ctx)
		return r.adapter.SendLongMessage(ctx, chatID, ev.Text)

	case agent.EventToolStart:
		r.heartbeatAll(ctx)
		name := "tool"
		if ev.ToolStart != nil && ev.ToolStart.Name != "" {
			name = ev.ToolStart.Name
		}
		return r.adapter.SendMessage(ctx, chatID, "🔧 "+name+"...")

	case agent.EventToolEnd:
		r.heartbeatAll(ctx)
		name := "tool"
		if ev.ToolEnd != nil && ev.ToolEnd.Name != "" {
			name = ev.ToolEnd.Name
		}
		if ev.ToolEnd != nil && ev.ToolEnd.Err != nil {
			return r.adapter.SendMessage(ctx, chatID, fmt.Sprintf("❌ %s failed: %v", name, ev.ToolEnd.Err))
		}
		return r.adapter.SendMessage(ctx, chatID, "✅ "+name+" done")

	case agent.EventPermission:
		return r.renderPermission(ctx, chatID, ev.Permission)

	case agent.EventDone:
		r.completeAll(ctx)
		code := 0
		if ev.Done != nil {
			code = ev.Done.ExitCode
		}
		return r.adapter.SendMessage(ctx, chatID, fmt.Sprintf("Session ended (exit %d)", code))

	case agent.EventError:
		r.completeAll(ctx)
		if ev.Error == nil || ev.Error.Err == nil {
			return r.adapter.SendMessage(ctx, chatID, "Error: unknown error")
		}
		return r.adapter.SendMessage(ctx, chatID, "Error: "+ev.Error.Err.Error())

	default:
		return fmt.Errorf("feishu: unsupported agent event kind %d", ev.Kind)
	}
}

// heartbeatAll calls Heartbeat on every active receipt. Best-effort:
// any per-receipt error is logged but does not abort the pump.
func (r *Renderer) heartbeatAll(ctx context.Context) {
	r.mu.Lock()
	receipts := make([]*MessageReceipt, 0, len(r.receipts))
	for _, rec := range r.receipts {
		receipts = append(receipts, rec)
	}
	r.mu.Unlock()

	for _, rec := range receipts {
		_ = rec.Heartbeat(ctx)
	}
}

// completeAll marks every active receipt as Completed. Receipts are
// removed from the map after completion to keep memory bounded —
// a chat with thousands of messages shouldn't accumulate thousands
// of completed receipts.
func (r *Renderer) completeAll(ctx context.Context) {
	r.mu.Lock()
	receipts := make([]*MessageReceipt, 0, len(r.receipts))
	for _, rec := range r.receipts {
		receipts = append(receipts, rec)
	}
	r.receipts = make(map[string]*MessageReceipt) // clear
	r.mu.Unlock()

	for _, rec := range receipts {
		_ = rec.SetCompleted(ctx)
	}
}

// MarkExecuting transitions a specific receipt (by userMsgID) from
// Waiting to Executing. Called when a user message is actually
// dispatched (either idle bypass or buffer flush).
func (r *Renderer) MarkExecuting(ctx context.Context, userMsgID string) error {
	if userMsgID == "" {
		return nil
	}
	r.mu.Lock()
	rec, ok := r.receipts[userMsgID]
	r.mu.Unlock()
	if !ok {
		return nil // unknown receipt — no-op
	}
	return rec.SetExecuting(ctx)
}

// PendingReceipts returns the count of receipts still being tracked
// (for /status).
func (r *Renderer) PendingReceipts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.receipts)
}

// heartbeatAllTimeout is the per-receipt Heartbeat context timeout.
// Short — heartbeat is non-critical, must not block the pump.
const heartbeatAllTimeout = 2 * time.Second

func (r *Renderer) renderPermission(ctx context.Context, chatID string, req *agent.PermissionRequest) error {
	content, err := permissionCard(req)
	if err != nil {
		return err
	}
	_, err = r.adapter.sendContent(ctx, chatID, interactiveMessageType, content)
	return err
}

// permissionCard returns the v1 interactive-card JSON accepted by the Feishu
// IM API. Button callback routing is intentionally left to the Gateway in a
// later milestone; the card still carries the selected option in its value.
func permissionCard(req *agent.PermissionRequest) (string, error) {
	tool := "unknown tool"
	action := "The agent requested permission to continue."
	options := []string{"reject"}
	if req != nil {
		if req.Tool != "" {
			tool = req.Tool
		}
		if req.Action != "" {
			action = req.Action
		}
		if len(req.Options) > 0 {
			options = append([]string(nil), req.Options...)
		}
	}

	buttons := make([]map[string]any, 0, len(options))
	for i, option := range options {
		buttonType := "default"
		if i == 0 {
			buttonType = "primary"
		}
		buttons = append(buttons, map[string]any{
			"tag":  "button",
			"type": buttonType,
			"text": map[string]string{
				"tag":     "plain_text",
				"content": option,
			},
			"value": map[string]string{"option": option},
		})
	}

	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"template": "orange",
			"title": map[string]string{
				"tag":     "plain_text",
				"content": "Permission required",
			},
		},
		"elements": []any{
			map[string]any{
				"tag":     "markdown",
				"content": fmt.Sprintf("**%s**\n\n%s", tool, action),
			},
			map[string]any{
				"tag":     "action",
				"actions": buttons,
			},
		},
	}
	b, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
