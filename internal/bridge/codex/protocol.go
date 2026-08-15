
// Package codex implements a bridge to the Codex CLI via its
// `codex app-server --listen stdio://` JSON-RPC 2.0 transport.
//
// Wire format: LF-delimited JSON-RPC 2.0 messages. Notification methods
// are server-pushed; server-initiated requests (approvals, user input,
// dynamic tool calls) must be answered with a response on the same id.
//
// This file defines the wire types only — no behavior. The bridge
// behavior (handshake, dispatch, translation, permission gating) lives
// in session.go / agent.go / translate.go / permissions.go. Optional /
// evolving fields are typed as `json.RawMessage` so an upstream schema
// bump (0.43 → 0.46+) cannot fail unmarshal on the bridge side.
package codex

import "encoding/json"

// ─── initialize ───

type initializeParams struct {
	ClientInfo   clientInfo              `json:"clientInfo"`
	Capabilities *initializeCapabilities `json:"capabilities,omitempty"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

type initializeCapabilities struct {
	ExperimentalAPI           bool     `json:"experimentalApi"`
	OptOutNotificationMethods []string `json:"optOutNotificationMethods"`
}

type initializeResponse struct {
	UserAgent string          `json:"userAgent"`
	Raw       json.RawMessage `json:"-"`
}

// ─── thread lifecycle ───

type threadStartParams struct {
	Model                  string `json:"model,omitempty"`
	CWD                    string `json:"cwd"`
	ApprovalPolicy         string `json:"approvalPolicy,omitempty"`
	Sandbox                string `json:"sandbox,omitempty"`
	ExperimentalRawEvents  bool   `json:"experimentalRawEvents"`
	PersistExtendedHistory bool   `json:"persistExtendedHistory"`
}

type threadStartResponse struct {
	Thread          threadRef `json:"thread"`
	Model           string    `json:"model"`
	CWD             string    `json:"cwd"`
	ReasoningEffort string    `json:"reasoningEffort"`
}

type threadRef struct {
	ID string `json:"id"`
}

type threadResumeParams struct {
	ThreadID              string `json:"threadId"`
	PersistExtendedHistory bool   `json:"persistExtendedHistory"`
	CWD                   string `json:"cwd,omitempty"`
	ApprovalPolicy        string `json:"approvalPolicy,omitempty"`
	Sandbox               string `json:"sandbox,omitempty"`
}

// ─── turn ───

type turnStartParams struct {
	ThreadID       string      `json:"threadId"`
	Input          []turnInput `json:"input"`
	Model          string      `json:"model,omitempty"`
	Effort         string      `json:"effort,omitempty"`
	ApprovalPolicy string      `json:"approvalPolicy,omitempty"`
}

type turnInput struct {
	Type         string            `json:"type"`
	Text         string            `json:"text,omitempty"`
	TextElements []json.RawMessage `json:"text_elements,omitempty"`
	Path         string            `json:"path,omitempty"`
}

type turnStartResponse struct {
	Turn threadRef `json:"turn"`
}

type turnCompletedNotification struct {
	TurnID string          `json:"turnId"`
	Usage  *appServerUsage `json:"usage"`
	Status string          `json:"status"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// turn/interrupt — cancel an in-flight turn without killing the app-server.
// Codex app-server STAYS ALIVE and emits turn/completed with
// Status="interrupted" shortly after; the bridge's translator routes
// that to EventAgentDone{Reason:"interrupted"} so the chat layer's
// TryFlush picks up the next queued prompt on the same thread.
//
// This is the same wire signal the codex TUI uses for the ESC key.
// Sending SIGINT instead (the pre-fix behaviour) terminates the
// app-server, forces the chat layer to --resume a fresh process, and
// on a thread whose previous turn was interrupted mid-flight the
// resume can wedge in a "ghost turn" state where turn/start is
// accepted but turn/completed never arrives (see fix-stop).
type turnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

// turnInterruptResponse is the body of a successful turn/interrupt
// reply. The protocol documents the body as an empty object, but we
// keep the type for symmetry and future-proofing.
type turnInterruptResponse struct{}

type appServerUsage struct {
	InputTokens           int `json:"inputTokens"`
	CachedInputTokens     int `json:"cachedInputTokens"`
	OutputTokens          int `json:"outputTokens"`
	ReasoningOutputTokens int `json:"reasoningOutputTokens"`
	TotalTokens           int `json:"totalTokens"`
}

// appServerTokenUsageNotification is the payload for
// `thread/tokenUsage/updated` (codex ≥0.125). Code path:
//   - per-turn usage is exposed under `tokenUsage.last` (just-finished turn)
//   - cumulative session usage is exposed under `tokenUsage.total`
//   - `tokenUsage.modelContextWindow` is the API-reported context window
type appServerTokenUsageNotification struct {
	ThreadID   string `json:"threadId"`
	TurnID     string `json:"turnId"`
	TokenUsage struct {
		Last               appServerUsage `json:"last"`
		Total              appServerUsage `json:"total"`
		ModelContextWindow int            `json:"modelContextWindow"`
	} `json:"tokenUsage"`
}

// ─── item ───

// itemRaw is the raw JSON for a single item inside an
// item/started or item/completed notification. ID / Type are
// extracted from the raw bytes at use site via itemSplit so the
// rest of the per-type fields stay flexible.
type itemRaw = json.RawMessage

type itemStartedNotification struct {
	ThreadID string   `json:"threadId"`
	TurnID   string   `json:"turnId"`
	Item     itemRaw  `json:"item"`
}

type itemCompletedNotification struct {
	ThreadID string   `json:"threadId"`
	TurnID   string   `json:"turnId"`
	Item     itemRaw  `json:"item"`
}

// itemSplit extracts the {id,type} discriminator from raw item bytes.
// Returns ("","") if the bytes are not a JSON object.
func itemSplit(raw itemRaw) (id, typ string) {
	var disc struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &disc)
	return disc.ID, disc.Type
}

// item types observed on the wire
const (
	itemTypeAgentMessage      = "agentMessage"
	itemTypeReasoning         = "reasoning"
	itemTypeCommandExecution  = "commandExecution"
	itemTypeFileChange        = "fileChange"
	itemTypeWebSearch         = "webSearch"
	itemTypeMCPToolCall       = "mcpToolCall"
	itemTypeDynamicToolCall   = "dynamicToolCall"
	itemTypePlan              = "plan"
	itemTypeUserMessage       = "userMessage"
	itemTypeHookPrompt        = "hookPrompt"
	itemTypeContextCompaction = "contextCompaction"
)

// ─── server-initiated requests ───

type commandApprovalParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Command  string `json:"command"`
	CWD      string `json:"cwd"`
	Reason   string `json:"reason"`
}

type fileChangeApprovalParams struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	ItemID    string `json:"itemId"`
	Reason    string `json:"reason"`
	GrantRoot string `json:"grantRoot"`
}

type permissionsApprovalParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Reason   string `json:"reason"`
}

type requestUserInputParams struct {
	ThreadID  string                              `json:"threadId"`
	TurnID    string                              `json:"turnId"`
	ItemID    string                              `json:"itemId"`
	Questions []appServerRequestUserInputQuestion `json:"questions"`
}

type appServerRequestUserInputQuestion struct {
	ID          string                            `json:"id"`
	Header      string                            `json:"header"`
	Question    string                            `json:"question"`
	Options     []appServerRequestUserInputOption `json:"options"`
	MultiSelect bool                              `json:"multiSelect"`
}

type appServerRequestUserInputOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type requestUserInputResponse struct {
	Result requestUserInputResponseResult `json:"result"`
}

type requestUserInputResponseResult struct {
	Answers map[string]requestUserInputAnswer `json:"answers"`
}

type requestUserInputAnswer struct {
	Answers []string `json:"answers"`
}

// ─── RPC envelope ───

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  any             `json:"params,omitempty"`
}

type rpcResponseEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements the error interface so rpcError can flow through
// the same channels as a regular Go error.
func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	return "json-rpc error " + itoa(e.Code) + ": " + e.Message
}

// itoa formats a small int without dragging in strconv just for one
// place in an error string. Negative values get a leading "-".
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
