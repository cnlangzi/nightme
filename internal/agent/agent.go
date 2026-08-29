// Package agent defines the abstract Agent interface that wraps any
// AI Coding CLI behind a single structured event stream.
//
// Three pieces:
//
//   - Info    — static metadata (Name, Mode, Command, Args, Env),
//     readable from both Starter and the running Agent.
//   - Starter — the spawn recipe; the only polymorphic point. Each
//     bridge implements its own Start (fork+exec+handshake).
//   - Agent   — the runtime handle. PID, events chan, sessionID,
//     close machinery all live here; per-bridge protocol logic is
//     hidden behind the unexported driver interface.
//
// The Mode selector (ACP / SDK / PTY) lets the Bridge pick a backend
// without leaking protocol details to Session Manager. See
// docs/feat/F-09-agent-abstraction.md and F-21-agent-modes.md.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Mode identifies how the Bridge should communicate with the agent.
type Mode int

const (
	// ModeACP is the preferred mode: Agent Client Protocol over stdio
	// (JSON-RPC). Provides structured events, permission requests, and
	// tool progress out of the box.
	ModeACP Mode = iota

	// ModeSDK is a vendor-specific SDK (e.g. Claude Code Agent SDK).
	// Used when the CLI does not implement ACP.
	ModeSDK

	// ModePTY is the transparent fallback: spawn the CLI in a PTY and
	// pipe bytes. No structured events; ANSI / spinner / progress
	// indicators appear as raw text in the IM channel.
	ModePTY

	// ModeJSONIO is the v0.2 mode used by Claude Code. It spawns the
	// CLI with --input-format stream-json --output-format stream-json
	// and parses line-delimited JSON events from stdout. Provides
	// structured events (text, tool_use, tool_result, AskUserQuestion,
	// result) without requiring an ACP / SDK implementation. See
	// docs/feat/F-24-claudecode-bridge.md.
	ModeJSONIO
)

// String renders a Mode for logs and error messages.
func (m Mode) String() string {
	switch m {
	case ModeACP:
		return "acp"
	case ModeSDK:
		return "sdk"
	case ModePTY:
		return "pty"
	case ModeJSONIO:
		return "json-io"
	default:
		return fmt.Sprintf("mode(%d)", int(m))
	}
}

// EventKind discriminates the payload attached to an AgentEvent.
type EventKind int

const (
	// EventAgentText is a stream of text bytes from the CLI (PTY mode) or a
	// structured assistant message chunk (ACP / SDK modes).
	//
	// GRANULARITY CONTRACT (F-52): one EventAgentText is ONE COMPLETE
	// SEMANTIC BLOCK — a paragraph the user should see as a single
	// unit. It is NOT a streaming delta.
	//
	// This matters because gateway.Translate maps EventAgentText to
	// OutReply, and OutReply means "append one entry" to the channel's
	// rolling log. A bridge that forwards per-token deltas therefore
	// shatters one sentence into dozens of chat bubbles (and dozens of
	// Feishu card PATCHes). That was the F-52 bug in the pi bridge:
	// aggregate in the bridge, where the wire's own block boundaries
	// are visible.
	//
	// Conformance:
	//   - claudecode — yes, one per assistant content block.
	//   - pi         — yes since F-52; buffers text_delta and emits at
	//                  text_end / the tool boundary.
	//   - acp        — yes: chunks accumulate in textBuf; sliding idle
	//                  flushDebounce from last token, then flush only
	//                  on true sentence/paragraph (min size), or
	//                  immediately on tool / turn-end. Dotted ids
	//                  ("session.idle_timeout") are not sentence ends.
	//   - pty        — exempt; raw byte stream with no block structure.
	EventAgentText EventKind = iota

	// EventAgentPermission is a permission request from the agent. The
	// Channel renders it as an interactive UI (e.g. Feishu card with
	// buttons) and the user choice is fed back via SendPermission.
	EventAgentPermission

	// EventAgentToolStart marks the beginning of a tool invocation.
	EventAgentToolStart

	// EventAgentToolEnd marks the end of a tool invocation (success or error).
	EventAgentToolEnd

	// EventAgentDone signals the session has ended cleanly (or with an exit
	// code). After EventAgentDone no further events will be emitted.
	EventAgentDone

	// EventAgentError carries an unrecoverable error during the session.
	EventAgentError

	// EventAgentResult is the assistant's final reply for the turn. In
	// Claude Code stream-json this is the value of `result` on the
	// result event — distinct from EventAgentText (which is mid-stream
	// assistant text). Channels typically render it with a different
	// icon than the rolling-log entries.
	//
	// AgentResultEvent carries the per-turn Usage (token counts + cost) on
	// the same field — bridges populate it from the source event's
	// usage / modelUsage instead of emitting a separate EventUsage.
	// Runtime accumulates Usage on receipt before stamping
	// StatusBar, so the footer sees this turn's tokens on the
	// first try.
	EventAgentResult

	// EventAgentReady carries session bootstrap data (session_id + model)
	// from the agent's system/init event. Channels use it to surface
	// "session <id> · model <name>" in the receipt header.
	//
	// Semantic: the bridge has finished its initial bootstrap and
	// the agent's session metadata (SessionID + Model + AgentName +
	// Workspace + Branch) is now known to the runtime. Subsequent
	// user prompts can flow. This is the only readiness signal today;
	// if a bridge ever needs to distinguish "metadata known" from
	// "warm-up complete", add a second event rather than overloading
	// this one.
	EventAgentReady

	// EventAgentTaskCreate signals the first confirmable task operation
	// (e.g. Claude TaskCreate success). The payload carries the
	// current full task snapshot, not a delta, so that any consumer
	// can render a complete checklist without cross-event correlation.
	EventAgentTaskCreate

	// EventAgentTaskUpdate signals a confirmable task mutation after
	// create (status change, edit, delete). The payload also carries
	// the current full task snapshot so receipts can replace the
	// checklist wholesale.
	EventAgentTaskUpdate

	// EventAgentPermissionSettled means the host already resolved
	// the outstanding approval / AskUserQuestion (dashboard click,
	// timeout, or cancel). The mux event stream is NOT blocked on
	// Feishu answering; this event only PATCHes the outstanding
	// Action Needed card so it does not linger.
	EventAgentPermissionSettled
)

// String renders an EventKind for logs.
func (k EventKind) String() string {
	switch k {
	case EventAgentText:
		return "text"
	case EventAgentPermission:
		return "permission"
	case EventAgentToolStart:
		return "tool_start"
	case EventAgentToolEnd:
		return "tool_end"
	case EventAgentDone:
		return "done"
	case EventAgentError:
		return "error"
	case EventAgentResult:
		return "result"
	case EventAgentReady:
		return "agent_ready"
	case EventAgentTaskCreate:
		return "task_create"
	case EventAgentTaskUpdate:
		return "task_update"
	case EventAgentPermissionSettled:
		return "permission_settled"
	default:
		return fmt.Sprintf("event(%d)", int(k))
	}
}

// AgentPermissionRequest is the structured payload for EventAgentPermission.
//
// The Bridge exposes the available options (e.g. "once", "session",
// "reject") and the Channel renders them. The user's choice is sent
// back via ResponseCh exactly once; the Bridge consumes it and
// forwards the decision to the agent.
type AgentPermissionRequest struct {
	// Tool is the tool name (e.g. "Bash", "Write", "Edit").
	Tool string

	// Action is a human-readable description of the action being
	// requested (e.g. "Run command: rm -rf build/").
	Action string

	// Options enumerates the user-selectable choices. The first option
	// is treated as the default / safe choice. For a multi-question
	// AskUserQuestion batch, this is the first question's labels;
	// the full batch lives in Questions.
	Options []string

	// Questions is the AskUserQuestion batch when the permission
	// is a multi-choice prompt rather than a tool approval.
	// Empty for allow/deny-style approvals. A single-question
	// ask still populates this (len==1) so the channel can
	// render header + question text without a concatenated dump.
	Questions []AgentPermissionQuestion

	// Kind is PermissionKindApproval or PermissionKindQuestion.
	// Approvals map to dashboard Allow once / Reject and POST
	// ApprovalResponse; questions use the in-card wizard and
	// POST QuestionResponse. Mixing the two wire shapes leaves
	// the dashboard card pending (host respond() returns
	// bad-response).
	Kind string

	// ResponseCh receives the user's selected option string. Buffer 1,
	// non-blocking — the Bridge reads it once and proceeds.
	ResponseCh chan string
}

const (
	PermissionKindApproval = "approval"
	PermissionKindQuestion = "question"
)

// AgentPermissionSettled is the payload for EventAgentPermissionSettled.
// Outcome uses the host vocabulary (allowed-once / rejected / answered /
// cancelled). Source is "dashboard" when the mux resolved frame arrived
// without a local SendPermission.
type AgentPermissionSettled struct {
	Outcome string
	Source  string
}

// AgentPermissionQuestion is one AskUserQuestion item on
// EventAgentPermission. ID is echoed in the /api/respond batch.
type AgentPermissionQuestion struct {
	ID       string
	Header   string
	Question string
	Options  []string
}

// AgentToolStartEvent carries metadata when a tool invocation begins.
type AgentToolStartEvent struct {
	// ID is opaque, stable for the lifetime of one invocation. Pair
	// with AgentToolEndEvent.ID to correlate start/end.
	ID string

	// Name is the tool name (e.g. "Bash", "Read", "Write").
	Name string

	// Args are the raw or structured arguments passed to the tool.
	Args string
}

// AgentToolEndEvent carries the result of a finished tool invocation.
type AgentToolEndEvent struct {
	// ID matches the corresponding AgentToolStartEvent.ID.
	ID string

	// Name mirrors the tool name for symmetry with AgentToolStartEvent.
	Name string

	// Args are the raw or structured arguments passed to the tool.
	// Bridges populate this from the corresponding tool_use block;
	// it may be empty if the bridge couldn't correlate the result.
	Args string

	// Output is a short textual summary of the tool's result, suitable
	// for the renderer to surface in the rolling log. Bridges should
	// populate this from the tool's stdout / structured result /
	// response payload. The renderer truncates to perEntryMaxBytes
	// before display, so bridges may pass large payloads verbatim
	// without pre-truncating.
	Output string
}

// AgentDoneEvent is the terminal payload for a clean session end.
//
// EventAgentDone carries two related but distinct lifecycle signals:
//
//   - For one-shot bridges (Claude Code stream-json, PTY) EventAgentDone
//     means "the process is done; no more events will follow."
//     The bridge closes the events channel after emitting EventAgentDone.
//   - For long-lived bridges (Pi --mode rpc) EventAgentDone means
//     "the current turn settled; the process is still alive and
//     may produce more events on the next user turn." The events
//     channel stays open until process exit or Close().
//
// Channels and the ChatSession read pump can therefore rely on
// "channel closed" as the universal "session is over" signal and
// use the Reason field (when non-empty) to disambiguate turn-end
// from process-end.
//
// Usage rides on AgentDoneEvent (F-52 universal-prompt-end design) so
// the runtime can read per-turn stats from EITHER EventAgentResult or
// EventAgentDone uniformly — EventAgentResult is emitted only when there is a
// text payload, but EventAgentDone fires every turn, including empty /
// aborted ones. AgentResultEvent.Usage remains populated for callers
// that already read from the result-bearing event, but the runtime's
// accumulation path reads from Done.Usage (single source of truth).
// See docs/feat/F-52-pi-stream-aggregation.md.
type AgentDoneEvent struct {
	// ExitCode follows Unix convention: 0 = success, non-zero = error.
	// -1 indicates an abnormal termination (e.g. PTY EOF without a
	// child exit code).
	ExitCode int

	// Reason is an optional, bridge-defined tag describing why
	// EventAgentDone was emitted. Empty string means "use the bridge
	// default" (process exit for one-shot bridges). Bridges that
	// multiplex turns over a single process set Reason to
	// "settled" (or another agreed value) so callers can tell a
	// turn-end EventAgentDone from a process-end one. See
	// docs/feat/F-32-pi-rpc-bridge.md §3.
	Reason string

	// Usage is the per-turn token usage observed on the same wire
	// event as the bridge's turn-end signal. Bridges populate this
	// on the SAME AgentDoneEvent they emit — for one-shot bridges this
	// happens at process exit; for long-lived bridges (Pi) this
	// happens at every settled turn. The runtime is a passive
	// pass-through: it does NOT aggregate across turns and does NOT
	// fold Usage into any AgentSession state. Channel adapters read
	// Usage directly from the OutboundMessage (populated by
	// gateway.Translate from Done.Usage / Result.Usage) and render
	// it as footer Line 2. nil is a valid "no usage reported" value
	// (zero-usage turn, synthetic assistant message, etc.) — the
	// footer omits Line 2 in that case.
	Usage *UsageInfo
}

// AgentResultEvent is the payload for EventAgentResult — the assistant's final
// reply for the turn. Bridges populate Text from the stream-json
// result event's `result` field; DurationMs / Subtype are
// pass-through metadata for the channel to surface alongside the text
// (e.g. "📝 <text> (12.3s)").
//
// Error indication: when the result represents an error turn, bridges
// populate the top-level AgentEvent.Err instead of an IsError flag on
// this struct. Channels check `ev.Err != nil` (Kind == EventAgentResult)
// to render the error icon.
//
// Usage is the per-turn token usage that the bridge observed on the
// same wire event that delivered Text (Claude Code's `result.usage`
// + `result.modelUsage`; Pi's `message_end.usage`). Bridges populate
// this on the SAME AgentResultEvent rather than emitting a separate
// EventUsage — the data is contextually attached to the turn's
// result, not a peer event. Runtime accumulates Usage on receipt;
// channels fold it into the StatusBar footer. nil is a valid
// "no usage reported" value (the bridge may legitimately observe a
// zero-usage turn, e.g. a synthetic assistant message).
type AgentResultEvent struct {
	// Text is the final assistant reply. May be empty when the turn
	// ended with an error; channels typically still emit an EventAgentResult
	// so the header line can flip to an error state.
	Text string

	// DurationMs is the wall-clock duration of the turn in
	// milliseconds (Claude Code: result.duration_ms).
	DurationMs int64

	// Subtype is the result event's subtype (Claude Code: e.g.
	// "success", "error_max_turns"). Categorisation only — error
	// detection has moved to the top-level AgentEvent.Err field.
	Subtype string

	// Usage is the per-turn token usage observed on the same wire
	// event as Text. See struct doc above. Populated by bridges;
	// consumed by the runtime's newEventHandler via
	// agent.AgentResultEvent.Usage before stamping StatusBar.
	Usage *UsageInfo
}

// UsageInfo is the turn's token usage statistics, packaged inside
// AgentResultEvent (replaces the standalone EventUsage kind removed in the
// footer's "calc-then-reply" cleanup). All four counts default to
// zero when missing; channels decide how to surface (e.g. "1.2k
// tokens (in 800 · out 400) · $0.012").
//
// CostUSD is optional; bridges populate it from
// `result.modelUsage[<model>].costUSD` when present. Zero means
// "unknown / not reported" — channels must NOT render "$0.00".
//
// ContextWindowPct (see struct field below) is the per-turn
// context-fill percentage, bridge-computed via the Doc 1 formula:
//
//	pct = (InputTokens + OutputTokens + CacheCreation + CacheRead)
//	     / contextWindow * 100
//
// `contextWindow` is a bridge-local value: claudecode reads it
// from `modelUsage[<model>].contextWindow`, pi reads it from
// `get_state.data.model.contextWindow`. F-54 originally dropped
// the field from UsageInfo as dead (0 read / 0 write); F-55
// re-introduced it (UsageInfo.ContextWindow below) so the
// channel footer can render `(window)` alongside the percentage.
// See docs/feat/F-55-footer-show-context-window.md for the
// re-introduction rationale.
//
// i.e. exact wire fields divided by API-reported window — no
// client-side model table needed. The runtime does NOT recompute
// or overwrite these; the channel footer renders them verbatim
// as the "X% (window)" segment. See
// docs/feat/F-45-session-footer.md §1.5 / §1.6,
// docs/feat/F-54-pi-contextwindow-from-get-state.md, and
// docs/feat/F-55-footer-show-context-window.md.
type UsageInfo struct {
	// InputTokens is the non-cached input token count.
	InputTokens int

	// OutputTokens is the generated output token count this turn.
	OutputTokens int

	// CacheCreationInputTokens is the input tokens that wrote to
	// the prompt cache this turn (Claude Code:
	// cache_creation_input_tokens).
	CacheCreationInputTokens int

	// CacheReadInputTokens is the input tokens served from the
	// prompt cache this turn (Claude Code:
	// cache_read_input_tokens).
	CacheReadInputTokens int

	// CostUSD is the optional per-turn cost in USD; 0 when unknown.
	CostUSD float64

	// ContextWindow is the API/CLI-reported model context-window
	// size in tokens, forwarded verbatim from the bridge wire
	// payload (Claude Code: `modelUsage[<model>].contextWindow`;
	// Pi: `get_state.data.model.contextWindow`). F-55: re-introduced
	// after F-54 §1.2 dropped it as a dead field — it now has a
	// single consumer (the channel footer) so the user can see the
	// denominator alongside the percentage and judge upstream
	// compatibility-layer mismatches (e.g. `101.6% (200k)` against
	// an actual 1M model). 0 means "not reported" and the footer
	// omits the `(window)` segment alongside X%.
	//
	// The runtime does NOT recompute pct based on this value, does
	// NOT maintain a model catalog, and does NOT clamp pct > 100%.
	// It is purely a render-side diagnostic.
	ContextWindow int

	// ContextWindowPct is the per-turn context-fill percentage
	// (0–100), bridge-computed via the Doc 1 formula in the
	// struct doc. The runtime does NOT recompute or overwrite
	// this; the channel footer renders it verbatim.
	ContextWindowPct float64
}

type AgentTaskStatus int

const (
	// TaskPending is the default state for a freshly created task
	// that has not started running.
	TaskPending AgentTaskStatus = iota
	// TaskInProgress marks the task the agent is currently working
	// on. The receipt may show an ActiveForm suffix ("... ·
	// writing unit tests…") to give the user a live status hint.
	TaskInProgress
	// TaskCompleted marks a task the agent has finished. The
	// receipt renders a struck-through / check-glyph variant; the
	// task may still appear in the checklist as a historical row
	// until the bridge removes it.
	TaskCompleted
	// TaskDeleted is a transient signal: the bridge parses a
	// provider-native delete, removes the task from its session
	// state, and re-emits a full snapshot where the deleted id is
	// no longer present. The Gateway / Channel must not see a
	// AgentTaskItem with Status == TaskDeleted — by contract the
	// snapshot's Items only contains the live tasks.
	TaskDeleted
	// TaskCancelled marks a task that was cancelled before completion.
	// Cursor uses this status; mapped from Cursor's "cancelled" state.
	TaskCancelled
)

// String renders a AgentTaskStatus for log lines.
func (s AgentTaskStatus) String() string {
	switch s {
	case TaskPending:
		return "pending"
	case TaskInProgress:
		return "in_progress"
	case TaskCompleted:
		return "completed"
	case TaskDeleted:
		return "deleted"
	case TaskCancelled:
		return "cancelled"
	}
	return "task(unknown)"
}

// AgentTaskItem is one row in the per-turn checklist. ID is the
// provider-assigned stable identifier (e.g. Claude's `Task #1`);
// bridges MUST populate it so follow-up updates can correlate by
// ID. Subject is the user-visible label. ActiveForm is the
// optional present-continuous phrase the agent emits while the
// task is in progress.
type AgentTaskItem struct {
	ID         string
	Subject    string
	ActiveForm string
	Status     AgentTaskStatus
}

// AgentTaskListEvent is the typed payload for EventAgentTaskCreate and
// EventAgentTaskUpdate. Items is the full current snapshot of the
// provider session's task list (NOT a delta). An empty Items
// slice is a valid "clear the checklist" signal — channels may
// choose to render an empty section or hide it entirely.
type AgentTaskListEvent struct {
	Items []AgentTaskItem
}

// AgentEvent is the wire format on the AgentSession.Events() channel.
//
// Exactly one action-payload field is meaningful per Kind:
//
//	EventAgentText       -> Text
//	EventAgentPermission -> Permission
//	EventAgentToolStart  -> ToolStart
//	EventAgentToolEnd    -> ToolEnd (failure also sets top-level Err)
//	EventAgentDone       -> Done
//	EventAgentError      -> Err (top-level, no sub-struct)
//	EventAgentResult     -> Result (error turns also set top-level Err)
//	EventAgentReady      -> (no action payload; context fields only)
//	EventAgentTaskCreate -> TaskList
//	EventAgentTaskUpdate -> TaskList
//	EventAgentPermissionSettled -> PermissionSettled
//
// Bridge-side session context fields (SessionID / Model / AgentName /
// Workspace / Branch) are stamped on every event by the bridge's
// deliver() helper. They are stable for the lifetime of one bridge
// session and only change on /new (re-emit EventAgentReady to update).
type AgentEvent struct {
	Kind EventKind

	// ───── Bridge-side session context (always populated) ─────
	// Bridges maintain a "current context" snapshot and stamp these
	// fields on every event before delivery. Runtime reads them
	// directly; AgentSession holds no mirror state for these.
	SessionID string
	Model     string
	AgentName string
	Workspace string
	Branch    string

	// Source is an optional, aggregator-set identifier for the
	// sub-job that produced this event. Zero (= "") means
	// "single-job / unscoped" — bridges never set it; only
	// multi-job aggregators (DelegateReview's eventAggregator
	// when Tier 2 has multiple rule groups) stamp a non-empty
	// value before forwarding to the outer sink. Downstream
	// consumers MAY render it as a "[group-N]" label or use it
	// for log filtering. Forwarding layers (StreamRunOnceToEmitter
	// → dispatchSinkEvent) treat it as opaque metadata — they do
	// not consult it for policy / heartbeat decisions.
	Source string

	// ───── Action payload (per Kind; usually exactly one is meaningful) ─────
	// Text is the payload for EventAgentText. Other Kinds leave it empty.
	Text string

	Permission *AgentPermissionRequest
	ToolStart  *AgentToolStartEvent
	ToolEnd    *AgentToolEndEvent
	Done       *AgentDoneEvent

	// Err is the unified error indicator. Populated on:
	//   - EventAgentError  — unrecoverable session error
	//   - EventAgentToolEnd — when the tool invocation failed
	//   - EventAgentResult — when the result turn ended in error
	// Channels check `ev.Err != nil` to render error UI.
	Err error

	Result *AgentResultEvent

	// TaskList is the payload for EventAgentTaskCreate / EventAgentTaskUpdate.
	// Every event carries a full snapshot of the current task list
	// (not a delta) so consumers can replace the rendered checklist
	// wholesale. An Items slice with length 0 is a valid "clear the
	// checklist" signal.
	TaskList *AgentTaskListEvent

	// PermissionSettled is the payload for EventAgentPermissionSettled.
	PermissionSettled *AgentPermissionSettled

	// Diagnostic carries structured info about a bridge child
	// process exit when Kind == EventAgentError. Populated by the
	// bridge's lifecycle() on every non-graceful exit; nil on
	// other Kinds and on EventAgentError when the bridge died
	// without a deterministic cause (e.g. Err only). Downstream
	// consumers (gateway translate, RecoveryPolicy, /diagnose) read
	// this to render error UI and to detect systematic failures.
	Diagnostic *BridgeDiagnostic
}

// StartConfig is the per-session configuration handed to Agent.Start.
type StartConfig struct {
	// Workspace is the working directory the agent will operate in. It
	// is the session's immutable binding (created by /cwd, consumed by
	// /run).
	Workspace string

	// Args are extra argv items appended after the agent's own
	// defaults. v0.1 typically empty.
	Args []string

	// Env is extra environment variables (key=value form). v0.1
	// typically empty; users configure ANTHROPIC_API_KEY etc. in
	// their shell.
	Env []string

	// PermissionMode is an agent-specific permission-mode override.
	// Empty string means "use the agent's default". Bridges that
	// support a `--permission-mode` flag (Claude Code) translate this
	// into the corresponding CLI value; bridges that don't support
	// the knob silently ignore it. v0.3 ships only the Claude Code
	// bridge; the field is here so future agents can also opt in
	// without changing the Start signature.
	PermissionMode string

	// SessionID is the agent's own session id, captured from the
	// previous run's init event (e.g. Claude Code's
	// `system/init.session_id`). Bridges that support resume (Claude
	// Code) translate this into their native flag (e.g. `--resume
	// <id>`); bridges that don't (ACP / Pi / PTY) silently ignore it.
	// Empty means "no --resume; start a fresh session".
	SessionID string
}

// AgentSpec is the static, read-only description of an agent.
//
// This is the surface used by tooling that only needs to enumerate
// or display registered agents (`nightme agents`, config
// validation, help generation). It deliberately does NOT include
// Start / Events / Send* — those live on Agent. A consumer holding
// an AgentSpec is guaranteed (by the type system) not to call
// runtime methods, so it cannot accidentally spawn a process or
// leak a live handle.
//
// The per-bridge Agent struct satisfies both this interface and
// Agent. In template state (after the constructor, before Start)
// only AgentSpec methods are meaningful; in live state (after
// Start, before Close) both halves are valid.
type AgentSpec interface {
	// Name is the unique identifier used in config and the registry.
	Name() string

	// Mode tells the Bridge which backend to instantiate.
	Mode() Mode

	// Command is the spawn recipe's executable: the CLI binary name
	// (resolved via PATH at Start time) or an absolute path. Surfaced
	// by `nightme agents` so users can see what /run would spawn.
	Command() string

	// Args returns a defensive copy of the spawn recipe's default
	// argv (after the binary). Callers may not mutate the returned
	// slice; per-session overrides arrive separately via StartConfig.
	Args() []string

	// Env returns a defensive copy of the spawn recipe's default
	// environment entries (KEY=VALUE strings merged into the child
	// environment). Most bridges return nil; claudecode / pty return
	// whatever they were constructed with.
	Env() []string

	// Detect verifies the agent is runnable (binary on PATH, SDK
	// available, etc.). Called before Start; an error aborts session
	// creation with a clear "X not found" message to the user.
	Detect() error
}

// ContentBlockType discriminates the payload shape on a ContentBlock.
type ContentBlockType string

const (
	// ContentText is a plain-text segment. Text field is set.
	ContentText ContentBlockType = "text"

	// ContentImage is an image the agent can see. Path (absolute
	// filesystem path) and MediaType (MIME, e.g. "image/png") are
	// set. Implementations that support vision (Claude Code
	// stream-json in content-array mode) base64-encode and inline
	// the image; implementations without vision fall back to
	// emitting the path so the agent can read it with its file
	// tools.
	ContentImage ContentBlockType = "image"

	// ContentFile is any non-image file the agent can read (PDF,
	// source code, audio, video, etc.). Path is set; MediaType is
	// optional (advisory only — implementations that stream the
	// binary use it, others ignore it).
	ContentFile ContentBlockType = "file"
)

// ContentBlock is one element of a structured user turn. A turn
// contains zero or more blocks; implementations decide how to
// express them (see AgentSession.SendBlocks).
//
// Exactly one of Text / (Path, MediaType) is meaningful per block,
// based on Type:
//
//	ContentText  -> Text
//	ContentImage -> Path + MediaType
//	ContentFile  -> Path (+ optional MediaType)
type ContentBlock struct {
	Type ContentBlockType

	// Text is the segment for ContentText blocks. Empty for other
	// block types.
	Text string

	// Path is the absolute filesystem path for ContentImage /
	// ContentFile blocks. Empty for ContentText. Implementations
	// that stream the binary (e.g. Claude Code stream-json
	// content-array) read from this path at send time.
	Path string

	// MediaType is the MIME type for ContentImage (required for
	// vision-streaming implementations; e.g. "image/png",
	// "image/jpeg") and advisory for ContentFile. Empty for
	// ContentText.
	MediaType string
}

// ─── Info: fixed metadata (was AgentSpec) ──────────────────────────

// Info is the fixed metadata about an agent — its identity in the
// registry plus the spawn recipe it was registered with.
//
// "Infer" describes how this is obtained: by calling Info() on any
// Starter or *Agent handle. Info is not a separately-registered
// entity; it is the read-only metadata that every Starter/Agent
// exposes about itself.
//
// Info is a value type and immutable from the caller's perspective
// after construction. Args and Env are defensively copied on every
// Info() call so the caller can retain, mutate, or pass them on
// without affecting the underlying Starter/Agent.
//
// Use NewInfo when constructing an Info inside a bridge (it applies
// the defensive-copy invariant); bridges construct one via NewInfo
// in their Starter.Info method.
type Info struct {
	// Name is the unique identifier in the registry (e.g.
	// "claude", "pi", "codex").
	Name string

	// Mode reports which backend the bridge uses (PTY / ACP / SDK
	// / JSON-IO).
	Mode Mode

	// Command is the CLI binary name (resolved via PATH at Start
	// time) or absolute path. Surfaced by `nightme agents`.
	Command string

	// Args is the default argv after the binary. Per-session
	// overrides arrive via StartConfig.Args. Defensively copied on
	// every Info() call.
	Args []string

	// Env is the default env entries (KEY=VALUE) merged into the
	// child environment. Defensively copied on every Info() call.
	Env []string
}

// NewInfo constructs an Info, taking defensive copies of args and
// env so the Agent can retain them independently of the caller's
// slices. Nil slices are preserved as nil.
//
// Bridges call this in their Starter.infoValue() method; external
// callers should rarely need it directly.
func NewInfo(name string, mode Mode, command string, args, env []string) Info {
	return Info{
		Name:    name,
		Mode:    mode,
		Command: command,
		Args:    copyStrings(args),
		Env:     copyStrings(env),
	}
}

func copyStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// ─── Agent: shared runtime handle (struct) ─────────────────────

// Agent is the shared runtime handle for any bridge. It is a
// concrete struct (not an interface) because the runtime surface
// (PID / Events / Send* / Close / New) is uniform across bridges —
// per-bridge protocol details live in the unexported driver.
//
// The lifecycle is:
//
//   - Template (held in agent.Builtins as a Starter) — only
//     fields populated are the spec-half Info() returns.
//   - Live (returned by Starter.Start) — Agent populated with
//     events chan, pid, sessionID slot, and driver. Close() is
//     idempotent and stops the underlying process.
//
// sessionID is captured from EventAgentReady and stored atomically
// here so the runtime can read it without any cross-bridge type
// switching (F-45).
type Agent struct {
	// Info is the fixed metadata. Exported so bridges and test
	// fakes outside the package can construct a Agent via a
	// struct literal.
	Info Info

	pid    int
	events chan AgentEvent

	// sessionID is written by the bridge's readPump on
	// EventAgentReady and read by AgentSession.SessionID(). atomic
	// because write happens on the readPump goroutine, read on
	// AgentSession callers; both are short-lived accesses so a
	// mutex would also be fine but atomic.Value is cheaper.
	sessionID atomic.Value // string

	driver driver

	closeOnce sync.Once
	closed    chan struct{}
}

// PID returns the OS process id of the underlying child, or 0 when
// the session has no process (e.g. SDK backends that do not spawn
// one) or before Start. The Session Manager caches this value
// for /run reconnect logic and the registry.
func (a *Agent) PID() int { return a.pid }

// Keepalive is the chat layer's canonical "is this bridge
// usable right now?" entry point. Thin shim around
// driver.Keepalive(ctx, onRecover) — see the driver interface
// for the cohesive detect-and-recover contract. Called by
// AgentSession.Submit before every SendBlocks.
//
// Bridges with a per-AS subprocess use procutil.AlivePID +
// invoke onRecover to respawn; shared-host bridges (dsh)
// inspect the host connection and reconnect on severance;
// SDK-style bridges (future ACP) query the upstream service.
func (a *Agent) Keepalive(ctx context.Context, onRecover func(context.Context) error) error {
	return a.driver.Keepalive(ctx, onRecover)
}

// Events streams AgentEvent values until the session ends. The
// channel is closed by the bridge implementation only when the
// underlying process (or transport) terminates — NOT after every
// EventAgentDone. Long-lived bridges that multiplex many turns
// over a single process (e.g. Pi --mode rpc) emit EventAgentDone
// at the end of each turn and keep the channel open until process
// exit or Close(). Channels and ChatSession rely on the channel
// being closed as the universal "session is over" signal;
// AgentDoneEvent.Reason disambiguates turn-end from process-end.
func (a *Agent) Events() <-chan AgentEvent { return a.events }

// SessionID returns the agent's own session id captured on the
// last EventAgentReady (e.g. Claude Code's
// `system/init.session_id`). Empty when the agent has no resume
// semantics or has not yet emitted its init event. Read is
// concurrent-safe; write happens on the bridge's readPump via
// setSessionID.
func (a *Agent) SessionID() string {
	v, _ := a.sessionID.Load().(string)
	return v
}

// SendBlocks delivers a structured user turn. Delegates to the
// bridge-specific driver.
func (a *Agent) SendBlocks(ctx context.Context, blocks []ContentBlock) error {
	return a.driver.SendBlocks(ctx, blocks)
}

// SendPermission responds to the most recent EventAgentPermission.
// Only meaningful in ACP/SDK modes; PTY mode writes it verbatim
// to stdin. See the driver interface for per-bridge semantics.
func (a *Agent) SendPermission(resp string) error {
	return a.driver.SendPermission(resp)
}

// New resets the conversation context on the running session.
// The underlying process (or transport, for long-lived bridges)
// stays alive. Events() stays open. PID stays the same.
//
// Bridge-specific implementations (F-34):
//   - claudecode: writeLine("/clear")            // stdin slash command
//   - pi:         send {"type":"new_session"}    // RPC
//   - acp:        send "session/new"             // JSON-RPC over existing transport
//
// After New returns, the bridge MUST emit a fresh EventAgentReady
// carrying the new SessionID; the runtime's EventAgentBus
// subscriber captures it via setSessionID.
func (a *Agent) New(ctx context.Context) error { return a.driver.Reset(ctx) }

// Close terminates the session and releases resources.
// Idempotent. Triggers driver.Close, which stops the underlying
// process and closes the events channel (after pump goroutines
// have drained). The driver is responsible for ordering.
func (a *Agent) Close() error {
	var err error
	a.closeOnce.Do(func() {
		close(a.closed)
		err = a.driver.Close()
	})
	return err
}

// Driver returns the unexported driver interface for callers
// that need access to bridge-specific state (e.g. tests
// inspecting a *closedSpy behind the handle). Exposed as
// interface{} because the driver interface is package-private.
// Production code should call the typed methods on *Agent
// (SendBlocks, Events, …) rather than going through this.
func (a *Agent) Driver() interface{} { return a.driver }

// Stop halts execution of the in-flight turn. Delegates to the
// bridge's driver; bridges that can't honor the call return
// agent.ErrNotSupported.
//
// Stop is fire-and-forget: it does NOT block waiting for the bridge
// to settle the turn. Whether the bridge emits a clean
// EventAgentDone, the child process exits, or nothing visibly
// happens on the wire is bridge-specific. The chat layer's TryFlush
// watches IsReady() and reschedules the next queued prompt once
// the bridge settles — the caller does not need to coordinate that
// transition.
func (a *Agent) Stop(ctx context.Context) error {
	return a.driver.Stop(ctx)
}

// NewAgent builds a *Agent from its constituent parts.
// Exported so bridges and test fakes (outside the agent package)
// can construct one. The driver is passed as interface{} because
// the driver interface itself is package-private; production
// callers pass the bridge-specific struct that implements the
// five Send*/Reset/Close methods. The closed channel is freshly
// allocated; the caller does not need to manage it.
func NewAgent(info Info, pid int, events chan AgentEvent, d interface{}) *Agent {
	return &Agent{
		Info:   info,
		pid:    pid,
		events: events,
		driver: d.(driver),
		closed: make(chan struct{}),
	}
}

// driver is the per-bridge protocol interface. Each bridge
// implements this with its own runtime state (exec.Cmd / RPC
// client / Transport / pump goroutines). It is package-private —
// external code interacts only with *Agent.
//
// The 6 methods capture exactly what bridges expose at runtime;
// the static metadata is on Starter.Info, the spawning logic
// is on Starter.Start, the close machinery is on Agent.Close.
type driver interface {
	SendBlocks(ctx context.Context, blocks []ContentBlock) error
	SendPermission(resp string) error
	Reset(ctx context.Context) error
	Close() error

	// Stop is a bridge-specific runtime method that the shared
	// *Agent wrapper exposes. Each driver returns
	// agent.ErrNotSupported if the bridge cannot honor the call.
	Stop(ctx context.Context) error

	// Keepalive is the chat layer's canonical "is this bridge
	// usable right now?" entry point. It encapsulates the
	// detection-and-recover decision cohesively in the bridge:
	// subprocess bridges probe the OS PID via
	// procutil.AlivePID, shared-host bridges (dsh) inspect the
	// WS host connection state, SDK-style bridges query the
	// upstream service. On detection of dead / unhealthy
	// state, Keepalive invokes onRecover so the bridge's
	// underlying transport is rebuilt; the callback comes
	// from the chat layer because the Spawner reference
	// lives there (and is the only thing outside the bridge
	// that knows how to re-spawn via the bridge's Starter).
	//
	// Contract:
	//   - Returns nil when the bridge is alive (caller proceeds
	//     with SendBlocks without further action).
	//   - On dead state, invokes onRecover(ctx) once. If it
	//     returns nil, Keepalive returns nil — the bridge is
	//     back and the caller's next SendBlocks lands on the
	//     fresh transport.
	//   - Returns a non-nil error ONLY when recovery itself
	//     fails (binary missing, auth refused, host unreachable
	//     after retries, etc.). The caller surfaces the error
	//     to the user; no retry loop.
	//
	// Concurrent calls MUST be serialized inside Keepalive
	// (sync.Once-style guard or mutex) so a burst of Submits
	// doesn't kick off parallel recoveries. The caller does not
	// coordinate.
	//
	// Cost: alive check is microseconds (syscall.Kill(pid, 0)
	// for subprocess bridges; a connection-state field read for
	// dsh). Recovery only fires when dead and is bounded to one
	// attempt — persistent failure is surfaced, not retried.
	Keepalive(ctx context.Context, onRecover func(context.Context) error) error
}

// ─── Starter: spawn recipe (interface, the only one) ──────────────

// Starter is the spawn recipe for an agent. It is the only
// interface in this package — the runtime surface (PID / Events /
// Send* / Close) is uniform across bridges and lives on the
// concrete *Agent struct, while the spawn recipe itself varies
// per bridge and stays polymorphic.
//
// Each bridge's init() registers one Starter per agent name into
// agent.Builtins. The registry stores Starter values, not *Agent.
// Spawner calls Starter.Start(ctx, cfg) to obtain a live *Agent.
//
// Lifecycle:
//
//   - Starter.Info returns the fixed metadata. Observable at
//     any time; used by `nightme agents`.
//   - Starter.Detect() is the pre-flight check (binary on PATH,
//     SDK available). Called by Spawner before Start; an error
//     aborts session creation with a clear "X not found" message.
//
// RunResult is the per-call output of Starter.RunOnce.
// Captures everything a one-shot caller might want after the
// turn — final text plus the per-turn metadata the bridge
// could observe (token usage, model, duration, bridge-
// specific subtype). Replaces the previous `(string, error)`
// signature so /gtw commit etc. can audit cost / timing
// without re-querying the bridge layer.
//
// Bridges populate as many fields as their wire protocol
// carries; fields the bridge cannot observe are left at the
// zero value. Callers MUST treat nil/empty as "not reported"
// rather than as "zero cost / zero tokens / zero duration".
type RunResult struct {
	// Text is the agent's final response. /gtw pr parses this.
	// Empty when the bridge rejected the prompt (caller sees
	// an error in the second return value).
	Text string

	// Usage is the per-turn token usage observed on the same
	// wire event as Text. nil when the bridge couldn't observe
	// usage (PTY heuristic impl). Populated by bridges whose
	// wire format carries `usage` / `modelUsage`.
	Usage *UsageInfo

	// RecoveredText holds a secondary body of text that the
	// bridge observed during the turn but did NOT propagate
	// into Text. Empty unless the bridge detected a recoverable
	// case (today: claudecode's /code-review plugin in `-p`
	// mode, where the plugin finishes with an AskUserQuestion
	// and the actual review sits in an earlier `assistant`
	// event — the recovery layer promotes it into Text and
	// keeps a copy here for audit).
	//
	// Renamed from AssistantText in v15b: "assistant" is
	// claudecode's specific wire-event name; the field's role
	// is semantic (a recoverable body of text), not source-
	// specific. Future bridges that hit similar terminal-vs-
	// stream skew can populate this without renaming again.
	//
	// Always empty for non-claudecode bridges and for
	// claudecode non-review print-mode runs (see
	// parsePrintStream's isReview gate in print.go).
	RecoveredText string

	// Model is the model name that actually produced Text.
	// Important when RunOnce uses a different model than the
	// chat session's selectedAgent (e.g. /gtw commit might
	// run on a cheaper model via cfg override) — callers
	// rendering a StatusBar should prefer Result.Model over
	// the chat session's selectedAgent model. Empty when
	// the bridge didn't observe model metadata. Pi
	// print-mode peeks the model name off the assistant
	// message_start/message_update/message_end wire frames
	// (see internal/bridge/pi/print.go:peekPrintMeta) so
	// /gtw commit et al. surface it on the AgentBar footer
	// (F-PI-PRINT-002).
	Model string

	// SessionID is the bridge-side session identifier for
	// the call (Claude Code: system/init.session_id; Codex:
	// thread.started.thread_id; Pi print-mode: peeked from the
	// `{"type":"session","id":..}` frame — see
	// internal/bridge/pi/print.go:peekPrintMeta). Bridges
	// populate when their wire format carries the value;
	// empty when it doesn't. Print-mode sessions are
	// short-lived (one per `-p` invocation), so the value is
	// mostly useful for audit (logging which session produced
	// which output) and future resume flows (`--resume <sid>`
	// on retry) rather than as a persistent identifier.
	SessionID string

	// DurationMs is the wall-clock duration of the call in
	// milliseconds. Useful for /gtw commit timing logs and
	// for tracking slow one-shot paths.
	DurationMs int64

	// Subtype is the bridge-specific turn category. Values
	// are HETEROGENEOUS across bridges — see the per-bridge
	// comment below. Callers MUST NOT do cross-bridge string
	// matching; only use Subtype for within-bridge audit or
	// for the coarse "is this an error category?" check.
	//
	// Per-bridge values:
	//   - claudecode: result.subtype
	//       "success" / "error_max_turns" / "refusal" / "compact"
	//   - codex:     turn status
	//       "completed" / "failed"
	//   - pi:        message_end.stopReason
	//       "stop" / "toolUse" / "error"
	//   - opencode:  terminal SSE event type
	//       "session.idle" / "error" / etc.
	//   - acp:       bridge-defined; consult the bridge's docs
	//   - pty:       always "" (no structured event)
	Subtype string
}

//   - Starter.Start(ctx, cfg) spawns (or attaches to) the agent
//     and returns a live *Agent. The receiver is unchanged;
//     Starter is reusable across many sessions.
type Starter interface {
	Info() Info
	Detect() error
	Start(ctx context.Context, cfg StartConfig) (*Agent, error)

	// RunOnce runs a single synchronous turn: the implementation
	// owns the full spawn → send → wait → close cycle. cfg.Workspace
	// is the agent's cwd for this call. Returns a RunResult
	// carrying the agent's final text plus any per-turn metadata
	// (usage / session id / model / duration / subtype) the
	// bridge could observe. Errors when the turn didn't produce
	// a usable response.
	//
	// RunOnce is the "one-shot" counterpart to Start. Start returns
	// a live *Agent for multi-turn / chat sessions; RunOnce is for
	// callers (e.g. /gtw commit, /gtw pr) that want a single
	// synchronous turn and don't need the session handle.
	//
	// opts configures per-call behaviour (see RunOnceOption).
	// Bridges that don't support a given option ignore it — variadic
	// opts keep this signature backwards-compatible with all
	// existing RunOnce callers (no compile breakage).
	RunOnce(ctx context.Context, cfg StartConfig, blocks []ContentBlock, opts ...RunOnceOption) (RunResult, error)

	// Review runs the /review slash command against this agent and
	// returns the raw review output text. The bridge owns ONE thing:
	// producing the review. The caller (the /review dispatcher in
	// internal/command/review) is responsible for wrapping the text
	// in agent.FormatReviewMessage and routing it to BOTH the
	// AgentSession (via as.SendBlocks, so the main agent can act on
	// "fix the blockers"-style follow-ups) and the channel (via the
	// chat session's emitter, so the user sees the findings directly
	// in chat without waiting for the AS's downstream reply).
	//
	// Takes StartConfig (not a dedicated ReviewContext) because the
	// only review-specific data we needed was Workspace, and that
	// field already exists on StartConfig. Reusing StartConfig keeps
	// the bridge signature symmetric with RunOnce / Start and avoids
	// a one-field type that leaks "review is a special thing"
	// framing into the agent package.
	//
	// opts configures per-call behaviour (see RunOnceOption).
	// Bridges that don't support a given option ignore it.
	//
	// ===== F-review.md §13 "codex/claude use native review" rule =====
	//
	// Bridges that have a native review subcommand MUST invoke it
	// directly instead of running our generic BuiltinPrompt:
	//   - claudecode: `claude -p code-review` (built-in slash command;
	//     note: NO leading slash in `[command]` slot — verified
	//     2.1.220 that `claude -p /code-review` runs 0 turns and
	//     returns empty result, while `claude -p code-review`
	//     dispatches the slash command and fires the multi-agent
	//     pipeline)
	//   - codex:      `codex review --base <default-branch>` (subcommand;
	//     review rejects exec-only flags like --json / -o /
	//     --dangerously-bypass-approvals-and-sandbox / -C /
	//     --skip-git-repo-check — uses its own argv assembly in
	//     print.go, NOT runPrintMode)
	//
	// Bridges that have NO native review subcommand call
	// s.RunOnce(ctx, cfg, [BuiltinPrompt()]) inline — the
	// canonical review path is "spawn a fresh subprocess with the
	// shared prompt" and inlining keeps the contract symmetric
	// with RunOnce:
	//   - dsh, opencode, pi, acp
	//
	// Return ErrReviewNotSupported when this agent type cannot do
	// review (e.g. pty/bash fallback). The dispatcher surfaces a
	// friendly "agent X does not support /review" reply (via
	// cs.Emitter().Send from the async goroutine, since the inline
	// reply is already gone by the time the bridge returns).
	//
	// opts configures per-call behaviour (see RunOnceOption).
	Review(ctx context.Context, cfg StartConfig, opts ...RunOnceOption) (RunResult, error)
}

// RunOnceOption configures per-call behaviour of Starter.RunOnce /
// Starter.Review. Implementations are functional-options: each option
// sets a field on the private runOnceConfig the bridge reads before
// starting the call. Bridges that don't recognise a given option
// silently ignore it (the option is kept in the package-private
// config struct, not on the public Starter API).
//
// Today only WithEventSink is defined. The pattern is open for
// future options (timeout overrides, abort hooks, etc.).
type RunOnceOption func(*runOnceConfig)

// runOnceConfig is the per-call config resolved from RunOnceOption
// before RunOnce / Review drives the bridge. Field semantics:
//
//   - OnEvent (sink): every AgentEvent the bridge would have
//     emitted to its internal *agent.Agent.Events() channel during
//     the call is also delivered here, synchronously, in order.
//     Bridges are NOT responsible for buffering / threading — the
//     caller-side sink (typically outbound.StreamRunOnceToEmitter)
//     MUST be non-blocking or it will stall the bridge's wire
//     parser / drain loop. Empty (nil) means "no observer; behave
//     as before this option existed".
//
// The struct is package-private so bridges don't accidentally grow
// their own public RunOnceOption type. The "Options" pattern is
// exposed via exported functional helpers (e.g. WithEventSink)
// that callers compose freely.
type runOnceConfig struct {
	OnEvent func(AgentEvent)
}

// WithEventSink returns a RunOnceOption that installs sink as the
// per-call AgentEvent observer. The sink is invoked synchronously by
// the bridge for every event the bridge emits (including Ready /
// Text / ToolStart / ToolEnd / Permission / TaskCreate / TaskUpdate
// / Error / Done / Result). For one-shot / review flows where the
// bridge has already drained to a terminal event before returning,
// the sink also sees that final event.
//
// IMPORTANT — sink semantics:
//   - sink is called on the bridge's own goroutine. If sink blocks
//     (e.g. a slow channel send), the bridge stalls. Callers MUST
//     use a buffered chan + drain goroutine (see
//     outbound.StreamRunOnceToEmitter for the canonical pattern).
//   - sink receives EVERY event. Bridges do NOT filter — that's
//     the caller's job. A sink that only wants text can match on
//     ev.Kind == EventAgentText.
//   - One-shot flows (RunOnce / Review) do not await sink response.
//     Sink is observational only. Permission decisions on the dsh
//     RunOnce path are made by drainForRunResult (auto-Allow once),
//     not via Permission.ResponseCh through the sink.
func WithEventSink(sink func(AgentEvent)) RunOnceOption {
	return func(c *runOnceConfig) { c.OnEvent = sink }
}

// ParseRunOnceOptions resolves a variadic RunOnceOption slice into
// the private runOnceConfig that bridges read before driving the
// call. Bridges that don't recognise a given option silently skip
// it (the option setter on the config is just a no-op for fields
// the bridge doesn't read). This is the canonical helper bridges
// call from RunOnce / Review — never read opts by hand, since
// future options may add fields without per-bridge plumbing.
//
//	opts := agent.ParseRunOnceOptions(runOpts)
//	// opts.OnEvent is nil if no WithEventSink was passed
func ParseRunOnceOptions(opts []RunOnceOption) runOnceConfig {
	var cfg runOnceConfig
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	return cfg
}

// Errors surfaced by the registry.
var (
	// ErrUnknownAgent is returned by Registry.Get when no agent with
	// the requested name has been registered.
	ErrUnknownAgent = errors.New("agent: unknown agent")

	// ErrRestartRequired is returned by AgentSession.New when the
	// bridge cannot perform an in-place conversation reset (no
	// protocol-level /clear or equivalent). The wrapper layer
	// (agentsession.AgentSession.New) catches this sentinel and
	// falls back to a kill-and-respawn via the configured Spawner.
	// Returning nil here would be wrong: callers must distinguish
	// "successfully reset in-place" from "needs full restart".
	ErrRestartRequired = errors.New("agent: bridge requires restart for reset")

	// ErrNotSupported is returned by Stop when the bridge does
	// not implement the requested operation. The runtime can
	// detect this with errors.Is and surface a user-friendly
	// "not supported for this agent" message instead of treating
	// it as a generic bridge error.
	//
	// Bridges that DO implement the operation return nil on
	// success and a real error on failure. The sentinel is only
	// for "operation is not implemented on this bridge".
	ErrNotSupported = errors.New("agent: operation not supported on this bridge")

	// ErrResumeUnhealthy is returned by bridges (notably
	// claudecode) when Start was called with a SessionID that the
	// bridge refuses to resume — e.g. claude's "No conversation
	// found with session ID" stderr diagnostic. Sentinel lives in
	// the agent package so chat-layer code (chatsession) can
	// detect it via errors.Is without importing the bridge
	// package directly. Bridges that surface a resume rejection
	// wrap their bridge-specific error with %w of this sentinel.
	//
	// T-alive (2026-08-07): the chat layer's policy on
	// ErrResumeUnhealthy is to clear as.sessionID and retry the
	// spawn once without a resume id (silently landing the user
	// on a fresh session). The bridge stays loud (T-alive's
	// resume-preservation invariant); the chat layer auto-recovers
	// so the user's next message does not die when the saved
	// sessionID has gone stale (daemon restart against an
	// upstream-cleaned thread, /close on a CLI whose --resume
	// was rejected, or a claudecode SIGINT-fallback path before
	// the fix-stop control_request path landed).
	//
	// Note: claudecode's primary /stop path (post-fix-stop)
	// uses control_request{interrupt} on stdin and keeps the
	// same session alive — it does NOT trigger this sentinel.
	// ErrResumeUnhealthy is only relevant for /close,
	// daemon-restart, and the SIGINT-fallback edge case.
	ErrResumeUnhealthy = errors.New("agent: resume session unhealthy")

	// ErrNoDiff is returned by ReviewWithPrompt / ReviewWithOcr when
	// the workspace has zero reviewable + zero untracked files in the
	// precomputed reviewContext. This collapses to: clean working
	// tree, with no staged/unstaged/untracked changes — and (only when
	// the workspace is itself empty) nothing to review at all. Note
	// that `detectDefaultBranch` failing does NOT trigger ErrNoDiff by
	// itself: the staged/unstaged/untracked git sources run
	// unconditionally and can populate reviewContext even without a
	// detected base. The /review dispatcher detects this sentinel via
	// errors.Is and short-circuits with a user-friendly "nothing to
	// review" inline message — without spawning the agent subprocess
	// for a zero-context review. Sentinel lives here (not in
	// command/review) so the agent package owns the contract for
	// "the review cannot produce findings".
	ErrNoDiff = errors.New("agent: no diff to review")
)

// sentinelErr is a small helper so tests can match errors with
// errors.Is without depending on the package-level variable identity.
type sentinelErr struct{ name string }

func (e *sentinelErr) Error() string { return "agent: " + e.name }

// duplicateRegistrationExists is returned by Registry.Register when
// an agent with the same name is already registered. Latest-wins
// semantics: the new instance replaces the old one. The bool returned
// by Register reports whether a replacement happened (useful for tests).
type duplicateError struct{ name string }

func (e *duplicateError) Error() string {
	return "agent: duplicate registration for " + e.name
}
