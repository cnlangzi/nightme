// Package openai implements the OpenAI-compatible chat protocol.
//
// The protocol covers OpenAI, OpenRouter, DeepSeek, Qwen, Ollama and
// most gateways: they all speak /chat/completions with function
// tools. The implementation stays lenient — a missing usage block, a
// null content next to tool calls and empty tool arguments are all
// facts of the wild, not errors.
package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
)

func init() {
	llm.Register("openai", New)
}

// Provider talks to an OpenAI-compatible chat completions endpoint.
type Provider struct {
	baseURL string
	model   string
	apiKey  string
	maxTok  int
	client  *http.Client
}

// New builds a provider from the configuration.
func New(cfg llm.Config) (llm.Provider, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("llm/openai: base_url is required")
	}
	key, err := llm.APIKey(cfg)
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
func (p *Provider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	body := chatRequest{
		Model:      p.model,
		Messages:   toWireMessages(req),
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
	url := p.baseURL + "/chat/completions"
	if err := llm.PostJSON(ctx, p.client, url, headers, body, &answer); err != nil {
		return nil, err
	}

	out := &llm.Response{StopReason: llm.StopEnd}
	if len(answer.Choices) > 0 {
		choice := answer.Choices[0]
		out.Message = fromWireMessage(choice.Message)
		out.StopReason = stopReason(choice.FinishReason, out.Message)
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

// toWireMessages maps a conversation onto the wire protocol.
//
// The system prompt becomes a system message, the assistant's tool
// calls become tool_calls, and every tool result becomes its own
// message with the id of the call it answers.
func toWireMessages(req llm.Request) []chatMessage {
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

// fromWireMessage maps the assistant's answer back onto the neutral
// shape.
func fromWireMessage(m chatMessage) llm.Message {
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

// stopReason maps the wire finish_reason onto the neutral one. A
// response that carries tool calls is a tool call whatever the server
// said.
func stopReason(finish string, msg llm.Message) string {
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
