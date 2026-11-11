// Package llm is nightme's LLM client layer.
//
// It holds two provider-neutral capabilities:
//
//   - Provider — chat and tool calling (System 2). A request carries a
//     system prompt, a conversation and a tool catalogue; the response
//     carries an assistant message, a stop reason and token usage.
//
//   - Decider — typed decision models (System 1). A request carries a
//     state and a list of questions; every question is answered with a
//     probability, a choice or a score.
//
// Both are registered by name, so a protocol is added by registering a
// factory and nothing else changes. The concrete protocols live in
// their own packages (openai, decisions, systemone) and register
// themselves.
package llm

import (
	"context"
	"encoding/json"
)

// Role is the author of a message in a conversation.
type Role string

const (
	// RoleSystem is the instruction that frames the conversation.
	RoleSystem Role = "system"
	// RoleUser is an input from the operator.
	RoleUser Role = "user"
	// RoleAssistant is a reply from the model.
	RoleAssistant Role = "assistant"
	// RoleTool is the result of a tool the assistant asked for.
	RoleTool Role = "tool"
)

// Part types.
const (
	// PartText is plain text.
	PartText = "text"
	// PartToolCall is a request from the assistant to run a tool.
	PartToolCall = "tool_call"
	// PartToolResult is the outcome of a tool call.
	PartToolResult = "tool_result"
)

// Part is one piece of a message. A message is a list of parts, so a
// single assistant turn can carry text and tool calls at once.
type Part struct {
	// Type is one of text, tool_call or tool_result.
	Type string `json:"type"`
	// Text is the text of a text part, the note of a tool call, or
	// the payload of a tool result.
	Text string `json:"text,omitempty"`
	// ToolCallID ties a tool result to the call that asked for it.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolName is the tool a call names or a result answers.
	ToolName string `json:"tool_name,omitempty"`
	// Args holds the JSON arguments of a tool call.
	Args json.RawMessage `json:"args,omitempty"`
	// IsError marks a tool result that failed. The model sees the
	// failure and corrects itself.
	IsError bool `json:"is_error,omitempty"`
}

// Message is one turn of a conversation.
type Message struct {
	// Role is the author of the message.
	Role Role `json:"role"`
	// Parts holds the content of the message.
	Parts []Part `json:"parts"`
}

// Text returns the concatenated text parts of the message.
func (m Message) Text() string {
	var out string
	for _, p := range m.Parts {
		if p.Type == PartText || (p.Type == "" && p.Text != "") {
			if out != "" {
				out += "\n"
			}
			out += p.Text
		}
	}
	return out
}

// ToolCalls returns the tool calls of the message.
func (m Message) ToolCalls() []Part {
	var out []Part
	for _, p := range m.Parts {
		if p.Type == PartToolCall {
			out = append(out, p)
		}
	}
	return out
}

// Tool is a callable tool the model may use.
type Tool struct {
	// Name is the tool's name — the model calls it by this name.
	Name string `json:"name"`
	// Description tells the model what the tool does.
	Description string `json:"description,omitempty"`
	// Schema is the JSON Schema of the tool's arguments.
	Schema json.RawMessage `json:"schema,omitempty"`
}

// Stop reasons.
const (
	// StopEnd means the model finished its turn.
	StopEnd = "end"
	// StopToolCalls means the model asked for tools to run.
	StopToolCalls = "tool_calls"
	// StopMaxTokens means the answer was cut short by the token cap.
	StopMaxTokens = "max_tokens"
)

// Usage counts the tokens a call consumed.
type Usage struct {
	// InputTokens are the tokens the request carried.
	InputTokens int `json:"input_tokens"`
	// OutputTokens are the tokens the response carried.
	OutputTokens int `json:"output_tokens"`
}

// Total returns the number of tokens the call consumed.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

// Add accumulates another usage into this one.
func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
}

// Request is one call to a chat model.
type Request struct {
	// System frames the conversation.
	System string `json:"system,omitempty"`
	// Messages is the conversation so far.
	Messages []Message `json:"messages"`
	// Tools is the catalogue the model may call from.
	Tools []Tool `json:"tools,omitempty"`
	// MaxTokens caps the length of the answer. Zero means the
	// provider's default.
	MaxTokens int `json:"max_tokens,omitempty"`
}

// Response is the model's answer.
type Response struct {
	// Message is the assistant turn.
	Message Message `json:"message"`
	// StopReason is why the model stopped: end, tool_calls or
	// max_tokens.
	StopReason string `json:"stop_reason"`
	// Usage counts the tokens the call consumed.
	Usage Usage `json:"usage"`
}

// Provider is a chat model that can call tools.
type Provider interface {
	// Complete runs one turn. The returned message carries the
	// assistant's text and the tool calls it wants to run.
	Complete(ctx context.Context, req Request) (*Response, error)
}
