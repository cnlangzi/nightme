// Package anthropic implements the Anthropic Messages protocol.
//
// The protocol is the Messages API: a system prompt, a conversation
// and a tool catalogue go out, an assistant turn with text and tool
// calls comes back. The implementation stays lenient — a missing usage
// block, a tool call without arguments and an unknown content block
// are all facts of the wild, not errors.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
)

func init() {
	llm.Register("anthropic", New)
}

// apiVersion is the version of the Messages API the provider speaks.
const apiVersion = "2026-10-21"

// defaultMaxTokens is the length cap of a call that does not name
// one. The API requires the field on every request.
const defaultMaxTokens = 16000

// Wire roles.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// Provider talks to an Anthropic-compatible Messages endpoint.
type Provider struct {
	baseURL string
	model   string
	apiKey  string
	maxTok  int
	client  *http.Client
}

// New builds a provider from the configuration.
//
// The base URL is the root of the API; the provider appends the path
// of the endpoint it calls.
func New(cfg llm.Config) (llm.Provider, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("llm/anthropic: base_url is required")
	}
	key, err := llm.APIKey(cfg.APIKeyEnv)
	if err != nil {
		return nil, err
	}
	return &Provider{
		baseURL: base,
		model:   strings.TrimSpace(cfg.Model),
		apiKey:  key,
		maxTok:  cfg.MaxTokens,
		client:  &http.Client{Timeout: 10 * time.Minute},
	}, nil
}

// messagesRequest is the body of a Messages call.
type messagesRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
}

// wireMessage is one message of the wire protocol.
type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

// wireBlock is one content block of a message.
type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   *string         `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// wireTool is a callable tool in the catalogue.
type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// messagesResponse is the answer of a Messages call.
type messagesResponse struct {
	Content    []wireBlock `json:"content"`
	StopReason string      `json:"stop_reason"`
	Usage      *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	msgs, err := toWireMessages(req)
	if err != nil {
		return nil, err
	}
	body := messagesRequest{
		Model:     p.model,
		MaxTokens: p.maxTokens(req.MaxTokens),
		System:    req.System,
		Messages:  msgs,
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, wireTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: toolSchema(t.Schema),
		})
	}

	var answer messagesResponse
	headers := map[string]string{"anthropic-version": apiVersion}
	if p.apiKey != "" {
		headers["x-api-key"] = p.apiKey
	}
	url := p.baseURL + "/v1/messages"
	if err := llm.PostJSON(ctx, p.client, url, headers, body, &answer); err != nil {
		return nil, err
	}

	out := &llm.Response{
		StopReason: llm.StopEnd,
		Message:    llm.Message{Role: llm.RoleAssistant},
	}
	for _, b := range answer.Content {
		switch b.Type {
		case "text":
			out.Message.Parts = append(out.Message.Parts, llm.Part{Type: llm.PartText, Text: b.Text})
		case "tool_use":
			out.Message.Parts = append(out.Message.Parts, llm.Part{
				Type:       llm.PartToolCall,
				ToolCallID: b.ID,
				ToolName:   b.Name,
				Args:       toolArgs(b.Input),
			})
		}
	}
	out.StopReason = stopReason(answer.StopReason, out.Message)
	if answer.Usage != nil {
		out.Usage = llm.Usage{
			InputTokens:  answer.Usage.InputTokens,
			OutputTokens: answer.Usage.OutputTokens,
		}
	}
	return out, nil
}

// maxTokens caps the length of an answer. The API requires the field,
// so a call that names no cap gets the default.
func (p *Provider) maxTokens(requested int) int {
	if requested > 0 {
		return requested
	}
	if p.maxTok > 0 {
		return p.maxTok
	}
	return defaultMaxTokens
}

// toWireMessages maps a conversation onto the Messages protocol.
//
// A tool result is a user message to the API, and the API wants one
// user message per turn: the results of a round of calls travel
// together, and the text that follows them joins them. The messages
// therefore alternate between user and assistant, which is what the
// API asks for.
func toWireMessages(req llm.Request) ([]wireMessage, error) {
	var out []wireMessage
	for _, m := range req.Messages {
		msg, err := toWireMessage(m)
		if err != nil {
			return nil, err
		}
		if len(msg.Content) == 0 {
			continue
		}
		if len(out) > 0 && out[len(out)-1].Role == msg.Role {
			out[len(out)-1].Content = append(out[len(out)-1].Content, msg.Content...)
			continue
		}
		out = append(out, msg)
	}
	return out, nil
}

// toWireMessage maps one message of the conversation onto the wire
// protocol.
func toWireMessage(m llm.Message) (wireMessage, error) {
	role := roleUser
	if m.Role == llm.RoleAssistant {
		role = roleAssistant
	}
	blocks := make([]wireBlock, 0, len(m.Parts))
	for _, p := range m.Parts {
		switch p.Type {
		case llm.PartText:
			blocks = append(blocks, wireBlock{Type: "text", Text: p.Text})
		case llm.PartToolCall:
			input, err := wireInput(p.Args, p.ToolName)
			if err != nil {
				return wireMessage{}, err
			}
			blocks = append(blocks, wireBlock{
				Type:  "tool_use",
				ID:    p.ToolCallID,
				Name:  p.ToolName,
				Input: input,
			})
		case llm.PartToolResult:
			text := p.Text
			blocks = append(blocks, wireBlock{
				Type:      "tool_result",
				ToolUseID: p.ToolCallID,
				Content:   &text,
				IsError:   p.IsError,
			})
		}
	}
	return wireMessage{Role: role, Content: blocks}, nil
}

// wireInput renders the arguments of a tool call as the JSON object
// the API wants. An empty argument list is an empty object; arguments
// that are not an object cannot be sent at all.
func wireInput(args json.RawMessage, tool string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, fmt.Errorf("llm/anthropic: the arguments of %s are not a JSON object: %w", tool, err)
	}
	if obj == nil {
		return json.RawMessage(`{}`), nil
	}
	return json.RawMessage(trimmed), nil
}

// toolArgs reads the arguments of a tool call out of an answer. The
// API sends an object; a call without one carries no arguments.
func toolArgs(input json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(input)) == 0 {
		return json.RawMessage(`{}`)
	}
	return input
}

// toolSchema renders the schema of a tool. The API wants a JSON
// Schema object, so a tool without one gets the object schema.
func toolSchema(schema json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(schema)) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	return schema
}

// stopReason maps the wire stop_reason onto the neutral one. A
// response that carries tool calls is a tool call whatever the server
// said.
func stopReason(reason string, msg llm.Message) string {
	if len(msg.ToolCalls()) > 0 {
		return llm.StopToolCalls
	}
	switch reason {
	case "tool_use":
		return llm.StopToolCalls
	case "max_tokens":
		return llm.StopMaxTokens
	default:
		return llm.StopEnd
	}
}
