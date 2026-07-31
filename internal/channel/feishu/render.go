package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
)

const interactiveMessageType = "interactive"

// Renderer maps structured AgentEvents to Feishu messages. It deliberately
// keeps protocol rendering here rather than in Session Manager so other
// channels can choose their own presentation.
type Renderer struct {
	adapter *Adapter
}

// NewRenderer constructs an event renderer backed by adapter.
func NewRenderer(adapter *Adapter) *Renderer {
	return &Renderer{adapter: adapter}
}

// Render sends the Feishu representation of ev to chatID.
func (r *Renderer) Render(ctx context.Context, chatID string, ev agent.AgentEvent) error {
	if r == nil || r.adapter == nil {
		return errors.New("feishu: renderer has no adapter")
	}

	switch ev.Kind {
	case agent.EventText:
		return r.adapter.SendMessage(ctx, chatID, ev.Text)
	case agent.EventPermission:
		return r.renderPermission(ctx, chatID, ev.Permission)
	case agent.EventToolStart:
		name := "tool"
		if ev.ToolStart != nil && ev.ToolStart.Name != "" {
			name = ev.ToolStart.Name
		}
		return r.adapter.SendMessage(ctx, chatID, "🔧 "+name+"...")
	case agent.EventToolEnd:
		name := "tool"
		if ev.ToolEnd != nil && ev.ToolEnd.Name != "" {
			name = ev.ToolEnd.Name
		}
		if ev.ToolEnd != nil && ev.ToolEnd.Err != nil {
			return r.adapter.SendMessage(ctx, chatID, fmt.Sprintf("❌ %s failed: %v", name, ev.ToolEnd.Err))
		}
		return r.adapter.SendMessage(ctx, chatID, "✅ "+name+" done")
	case agent.EventDone:
		code := 0
		if ev.Done != nil {
			code = ev.Done.ExitCode
		}
		return r.adapter.SendMessage(ctx, chatID, fmt.Sprintf("Session ended (exit %d)", code))
	case agent.EventError:
		if ev.Error == nil || ev.Error.Err == nil {
			return r.adapter.SendMessage(ctx, chatID, "Error: unknown error")
		}
		return r.adapter.SendMessage(ctx, chatID, "Error: "+ev.Error.Err.Error())
	default:
		return fmt.Errorf("feishu: unsupported agent event kind %d", ev.Kind)
	}
}

func (r *Renderer) renderPermission(ctx context.Context, chatID string, req *agent.PermissionRequest) error {
	content, err := permissionCard(req)
	if err != nil {
		return err
	}
	return r.adapter.sendContent(ctx, chatID, interactiveMessageType, content)
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
				"tag": "div",
				"text": map[string]string{
					"tag":     "lark_md",
					"content": fmt.Sprintf("**Tool:** %s\n%s", tool, action),
				},
			},
			map[string]any{
				"tag":     "action",
				"actions": buttons,
			},
		},
	}
	encoded, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("feishu: encode permission card: %w", err)
	}
	return string(encoded), nil
}

// cardOption extracts the option value used by a future callback handler.
// Keeping this helper local makes the shape explicit in renderer tests.
func cardOption(content string) string {
	var card struct {
		Elements []struct {
			Actions []struct {
				Value struct {
					Option string `json:"option"`
				} `json:"value"`
			} `json:"actions"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(content), &card); err != nil {
		return ""
	}
	for _, element := range card.Elements {
		for _, action := range element.Actions {
			if strings.TrimSpace(action.Value.Option) != "" {
				return action.Value.Option
			}
		}
	}
	return ""
}
