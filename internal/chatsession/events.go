// Package chatsession — EnrichedEvent (CS-AS 边界重构 Phase 1).
//
// `EnrichedEvent` 是 AgentSession 与 ChatSession 之间的唯一事件协议。
// ChatSession 从 `cs.activeAS.Events()` 读取事件流,按 Kind 路由到 runtime。
//
// 设计原则:
//
//  1. 引用优于复制:Prompt / AgentEvent 全部以指针形态流转,避免在
//     CS/AS 边界搬砖。
//  2. 多发不吞:每条桥事件都透传,AS 不替上层做"这事件对 chat 有没有用"
//     的判断。能抽象出来的全部上行。
//  3. 锚点可选:prompt 期事件 (KindAgentEvent / KindPromptEnded) 带
//     UserMsgID + PromptID;AS 生命周期事件 (KindLifecycle) 不带锚点。
package chatsession

import (
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// eventQueueCapacity is the per-AgentSession event buffer size.
//
// 256 is sized for "AS 在后台跑时 ChatSession 切换累积的事件窗口"。
// 终态事件 (KindPromptEnded / KindLifecycle) 由 readpump 强制 flush,
// 稳态事件 (KindAgentEvent) 可丢最老的 —— 256 在内存上完全可承受
// (256 * ~64B ≈ 16KB),不会无限增长。
const eventQueueCapacity = 256

// EnrichedEventKind tags the body of an EnrichedEvent. Exactly one of
// AgentEvent / PromptEnded / Lifecycle is non-nil for any event.
type EnrichedEventKind int

const (
	// KindAgentEvent: bridges a single AgentEvent from the underlying
	// transport (EventInit / EventText / EventToolStart / EventToolEnd /
	// EventResult / EventUsage / EventPermission / EventDone / EventError
	// etc.). AgentEvent field holds the bridge event verbatim.
	KindAgentEvent EnrichedEventKind = iota

	// KindPromptEnded: the in-flight Prompt has terminated. Emitted by
	// `AgentSession.endPrompt(reason)` after clearing `currentPrompt`.
	// PromptEnded holds the change summary; Prompt pointer is still valid
	// (memory not reclaimed yet — until next readpump iteration).
	KindPromptEnded

	// KindLifecycle: AS-level lifecycle transition (Spawned / Exited).
	// UserMsgID and PromptID are empty for this kind — not bound to any
	// Prompt. Lifecycle holds the new status.
	KindLifecycle
)

// String renders EnrichedEventKind for logs / diagnostics.
func (k EnrichedEventKind) String() string {
	switch k {
	case KindAgentEvent:
		return "agent-event"
	case KindPromptEnded:
		return "prompt-ended"
	case KindLifecycle:
		return "lifecycle"
	}
	return "unknown"
}

// EnrichedEvent is the single event type on the AS → ChatSession stream.
//
// Concurrent safety: written by the AS's readpump goroutine only;
// read by ChatSession's runtime reader. The pointer fields (Prompt /
// AgentEvent / PromptEnded / Lifecycle) are read-only after the
// write — ChatSession must NOT mutate them. Lifecycle events have
// empty UserMsgID / PromptID; runtime routes by Kind.
type EnrichedEvent struct {
	// ChatID is the owning ChatSession. Set by AS from
	// `as.ChatSessionID` at enrichment time.
	ChatID string

	// AgentSessionID is the source AS. Set by AS from `as.ID`.
	AgentSessionID string

	// UserMsgID is the receipt-card anchor (the last MessageID of the
	// in-flight Prompt). Empty for KindLifecycle events. AS populates
	// from `as.currentPrompt.LastMessageID` at enrichment time.
	UserMsgID string

	// PromptID is the in-flight Prompt's ID. Empty for KindLifecycle
	// events. AS populates from `as.currentPrompt.ID`.
	PromptID string

	// Prompt is a reference to the in-flight or just-ended Prompt.
	// Pointer stability: valid until AS submits a new Prompt or the
	// Prompt is finally GC'd. Runtime reads EndReason / EndedAt
	// directly via this pointer (no copy).
	Prompt *Prompt

	// Kind tags the body. Exactly one of AgentEvent / PromptEnded /
	// Lifecycle is non-nil.
	Kind EnrichedEventKind

	// AgentEvent is the bridge event verbatim (KindAgentEvent only).
	// Pointer — read-only after enqueue.
	AgentEvent *agent.AgentEvent

	// PromptEnded is the change summary (KindPromptEnded only).
	// Pointer — read-only after enqueue.
	PromptEnded *PromptEndedChange

	// Lifecycle is the status transition (KindLifecycle only).
	// Pointer — read-only after enqueue.
	Lifecycle *LifecycleChange
}

// PromptEndedChange describes a Prompt termination. Fields mirror
// `Prompt.EndedAt` / `Prompt.EndReason` for convenience; runtime can
// read either the inline fields below or `enrichedEvent.Prompt.EndedAt`
// / `enrichedEvent.Prompt.EndReason`.
type PromptEndedChange struct {
	// EndedAt mirrors Prompt.EndedAt at the moment of endPrompt.
	EndedAt time.Time

	// EndReason mirrors Prompt.EndReason at the moment of endPrompt.
	EndReason PromptEndReason
}

// LifecycleChange describes a status transition on the AS itself.
type LifecycleChange struct {
	// PID is the OS process ID at the moment of the transition.
	// 0 if the AS was never spawned or PID is not available.
	PID int

	// Status is the new status. One of:
	//   - StatusRunning (after Spawn completes)
	//   - StatusExited (after process death / Shutdown)
	//   - StatusDetached (reserved; not currently emitted)
	Status Status
}
