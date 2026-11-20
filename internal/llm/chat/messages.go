package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/cnlangzi/nightme/internal/llm"
)

func init() {
	llm.RegisterChatProtocol(protocolMessages, NewMessages)
}

// protocolMessages is the name the messages protocol is registered
// under.
const protocolMessages = "messages"

// messagesPath is the path the protocol appends to the base URL.
const messagesPath = "/messages"

// apiVersion is the version of the Messages API the protocol speaks.
const apiVersion = "2026-10-21"

// defaultMaxTokens is the length cap of a call that does not name
// one. The API requires the field on every request.
const defaultMaxTokens = 16000

// Wire roles.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// Messages talks to a Messages endpoint.
type Messages struct {
	settings
}

// NewMessages builds a provider for the Messages protocol.
//
// The base URL is the root of the API; the provider appends the path
// of the endpoint it calls.
func NewMessages(cfg llm.ChatConfig) (llm.Provider, error) {
	s, err := newSettings(cfg)
	if err != nil {
		return nil, err
	}
	return &Messages{settings: s}, nil
}

// messagesRequest is the body of a Messages call.
type messagesRequest struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	System    string         `json:"system,omitempty"`
	Messages  []messageTurn  `json:"messages"`
	Tools     []messagesTool `json:"tools,omitempty"`
}

// messageTurn is one message of the wire protocol.
type messageTurn struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

// contentBlock is one content block of a message.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   *string         `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// messagesTool is a callable tool in the catalogue.
type messagesTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// messagesResponse is the answer of a Messages call.
type messagesResponse struct {
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Complete implements llm.Provider.
func (p *Messages) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	msgs, err := toMessageTurns(req)
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
		body.Tools = append(body.Tools, messagesTool{
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
	url := llm.Endpoint(p.baseURL, messagesPath)
	if err := llm.PostJSON(ctx, p.client, url, p.requestHeaders(headers), body, &answer); err != nil {
		return nil, err
	}

	out := &llm.Response{Message: llm.Message{Role: llm.RoleAssistant}}
	for _, b := range answer.Content {
		switch b.Type {
		case "text":
			// A text block without text is not one: the API refuses
			// it, and it would poison every turn that replays it.
			if b.Text == "" {
				continue
			}
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
	out.StopReason = messagesStopReason(answer.StopReason, out.Message)
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
func (p *Messages) maxTokens(requested int) int {
	if requested > 0 {
		return requested
	}
	if p.maxTok > 0 {
		return p.maxTok
	}
	return defaultMaxTokens
}

// toMessageTurns maps a conversation onto the Messages protocol.
//
// A tool result is a user message to the API, and the API wants one
// user message per turn: the results of a round of calls travel
// together, and the text that follows them joins them. The messages
// therefore alternate between user and assistant, which is what the
// API asks for.
func toMessageTurns(req llm.Request) ([]messageTurn, error) {
	var out []messageTurn
	for _, m := range req.Messages {
		msg, err := toMessageTurn(m)
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

// toMessageTurn maps one message of the conversation onto the wire
// protocol.
func toMessageTurn(m llm.Message) (messageTurn, error) {
	role := roleUser
	if m.Role == llm.RoleAssistant {
		role = roleAssistant
	}
	blocks := make([]contentBlock, 0, len(m.Parts))
	for _, p := range m.Parts {
		switch p.Type {
		case llm.PartText:
			// The API refuses a text block without text.
			if p.Text == "" {
				continue
			}
			blocks = append(blocks, contentBlock{Type: "text", Text: p.Text})
		case llm.PartToolCall:
			input, err := wireInput(p.Args)
			if err != nil {
				return messageTurn{}, fmt.Errorf("llm/chat: the arguments of %s are not a JSON object: %w", p.ToolName, err)
			}
			blocks = append(blocks, contentBlock{
				Type:  "tool_use",
				ID:    p.ToolCallID,
				Name:  p.ToolName,
				Input: input,
			})
		case llm.PartToolResult:
			text := p.Text
			blocks = append(blocks, contentBlock{
				Type:      "tool_result",
				ToolUseID: p.ToolCallID,
				Content:   &text,
				IsError:   p.IsError,
			})
		default:
			// A part without a type that carries text is text — the
			// neutral message reads it that way.
			if p.Type == "" && p.Text != "" {
				blocks = append(blocks, contentBlock{Type: "text", Text: p.Text})
			}
		}
	}
	return messageTurn{Role: role, Content: blocks}, nil
}

// wireInput renders the arguments of a tool call as the JSON object
// the API wants. An empty argument list is an empty object; arguments
// that are not an object cannot be sent at all.
func wireInput(args json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return json.RawMessage(`{}`), nil
	}
	return json.RawMessage(trimmed), nil
}

// toolArgs reads the arguments of a tool call out of an answer.
//
// The API sends an object. A call that carries anything else carries
// no arguments: the history has to stay sendable, and a tool call
// without arguments comes back as an error the model can correct.
func toolArgs(input json.RawMessage) json.RawMessage {
	args, err := wireInput(input)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return args
}

// toolSchema renders the schema of a tool. The API wants a JSON
// Schema object, so a tool without one gets the object schema.
func toolSchema(schema json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(schema)) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	return schema
}

// messagesStopReason maps the wire stop_reason onto the neutral one.
// A response that carries tool calls is a tool call whatever the
// server said.
func messagesStopReason(reason string, msg llm.Message) string {
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
