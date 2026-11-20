package chat

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cnlangzi/nightme/internal/llm"
)

func init() {
	llm.RegisterChatProtocol(protocolCompletions, completionsPath, NewCompletions)
}

// protocolCompletions is the name the chat completions protocol is
// registered under.
const protocolCompletions = "chat_completions"

// completionsPath is the path the protocol appends to the base URL.
const completionsPath = "/chat/completions"

// Completions talks to a chat completions endpoint.
type Completions struct {
	settings
}

// NewCompletions builds a provider for the chat completions
// protocol.
func NewCompletions(cfg llm.ChatConfig) (llm.Provider, error) {
	s, err := newSettings(cfg)
	if err != nil {
		return nil, err
	}
	return &Completions{settings: s}, nil
}

// chatRequest is the body of a chat completions call.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []chatTool    `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
}

// chatMessage is one message of the wire protocol.
type chatMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// toolCall is a function call the model asks for.
type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

// functionCall carries the name and the JSON arguments.
type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// chatTool is a callable function in the catalogue.
type chatTool struct {
	Type     string       `json:"type"`
	Function functionSpec `json:"function"`
}

// functionSpec describes a callable function.
type functionSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// chatResponse is the answer of a chat completions call.
type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Complete implements llm.Provider.
func (p *Completions) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	body := chatRequest{
		Model:      p.model,
		Messages:   toCompletionsMessages(req),
		ToolChoice: "auto",
		MaxTokens:  p.maxTok,
	}
	if req.MaxTokens > 0 {
		body.MaxTokens = req.MaxTokens
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, chatTool{
			Type: "function",
			Function: functionSpec{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
			},
		})
	}

	var answer chatResponse
	headers := map[string]string{}
	if p.apiKey != "" {
		headers["Authorization"] = "Bearer " + p.apiKey
	}
	url := llm.Endpoint(p.baseURL, completionsPath)
	if err := llm.PostJSON(ctx, p.client, url, p.requestHeaders(headers), body, &answer); err != nil {
		return nil, err
	}

	out := &llm.Response{StopReason: llm.StopEnd}
	if len(answer.Choices) > 0 {
		choice := answer.Choices[0]
		out.Message = fromCompletionsMessage(choice.Message)
		out.StopReason = completionsStopReason(choice.FinishReason, out.Message)
	} else {
		out.Message = llm.Message{Role: llm.RoleAssistant}
	}
	if answer.Usage != nil {
		out.Usage = llm.Usage{
			InputTokens:  answer.Usage.PromptTokens,
			OutputTokens: answer.Usage.CompletionTokens,
		}
	}
	return out, nil
}

// toCompletionsMessages maps a conversation onto the wire protocol.
//
// The system prompt becomes a system message, the assistant's tool
// calls become tool_calls, and every tool result becomes its own
// message with the id of the call it answers.
func toCompletionsMessages(req llm.Request) []chatMessage {
	var out []chatMessage
	if strings.TrimSpace(req.System) != "" {
		system := req.System
		out = append(out, chatMessage{Role: "system", Content: &system})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleTool:
			for _, p := range m.Parts {
				if p.Type != llm.PartToolResult && p.Type != llm.PartText {
					continue
				}
				text := p.Text
				out = append(out, chatMessage{
					Role:       "tool",
					Content:    &text,
					ToolCallID: p.ToolCallID,
					Name:       p.ToolName,
				})
			}
		case llm.RoleAssistant:
			msg := chatMessage{Role: "assistant"}
			var text strings.Builder
			for _, p := range m.Parts {
				switch p.Type {
				case llm.PartText:
					text.WriteString(p.Text)
				case llm.PartToolCall:
					msg.ToolCalls = append(msg.ToolCalls, toolCall{
						ID:   p.ToolCallID,
						Type: "function",
						Function: functionCall{
							Name:      p.ToolName,
							Arguments: wireArguments(p.Args),
						},
					})
				}
			}
			if text.Len() > 0 {
				content := text.String()
				msg.Content = &content
			}
			out = append(out, msg)
		default:
			content := m.Text()
			out = append(out, chatMessage{Role: string(m.Role), Content: &content})
		}
	}
	return out
}

// fromCompletionsMessage maps the assistant's answer back onto the
// neutral shape.
func fromCompletionsMessage(m chatMessage) llm.Message {
	out := llm.Message{Role: llm.RoleAssistant}
	if m.Content != nil && *m.Content != "" {
		out.Parts = append(out.Parts, llm.Part{Type: llm.PartText, Text: *m.Content})
	}
	for _, tc := range m.ToolCalls {
		out.Parts = append(out.Parts, llm.Part{
			Type:       llm.PartToolCall,
			ToolCallID: tc.ID,
			ToolName:   tc.Function.Name,
			Args:       json.RawMessage(wireArguments(json.RawMessage(tc.Function.Arguments))),
		})
	}
	return out
}

// wireArguments renders the arguments of a tool call. A gateway that
// sends an empty string means "no arguments", and one that receives an
// empty string refuses the call — so both directions carry an empty
// object.
func wireArguments(args json.RawMessage) string {
	if strings.TrimSpace(string(args)) == "" {
		return "{}"
	}
	return string(args)
}

// completionsStopReason maps the wire finish_reason onto the neutral
// one. A response that carries tool calls is a tool call whatever the
// server said.
func completionsStopReason(finish string, msg llm.Message) string {
	if len(msg.ToolCalls()) > 0 {
		return llm.StopToolCalls
	}
	switch finish {
	case "tool_calls", "function_call":
		return llm.StopToolCalls
	case "length":
		return llm.StopMaxTokens
	default:
		return llm.StopEnd
	}
}
