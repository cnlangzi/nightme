// Package feishu — PromptState (F-53 channel-private FSM).
//
// F-53 lifts `agent.PromptState` (which was always effectively
// feishu-internal — its only caller was `internal/channel/feishu/
// receipt.go`) into the feishu package itself, and shrinks the
// 4-value enum (`Pending` / `Running` / `Succeeded` / `Failed`)
// down to 2 values (`Running` / `Done`). The 2 dead-code values
// are removed.
//
// Why 2 values and not 1: keeping `Done` as a distinct value
// preserves the type-level affordance to re-introduce a
// "card-header execution state" later (e.g. when a future UX PR
// restores terminal-state rendering on the receipt card). Until
// then, no caller transitions to `Done` — receipt FSM only ever
// stores `Running` (set at construction; never changed).
//
// See docs/feat/message_lifecycle.md §7 for the full rationale.
package feishu

// PromptState is the feishu-internal execution state stored on
// each MessageReceipt. Used only by the receipt FSM; not part of
// the abstract nightme wire contract.
type PromptState int

const (
	// PromptRunning: the receipt has been created and the
	// corresponding Prompt is active. This is the only value
	// MessageReceipt ever holds in Phase 0 — set at construction,
	// never transitioned.
	PromptRunning PromptState = iota

	// PromptDone: reserved for future use. Phase 0 never writes
	// this value. Kept as a 2-value enum to preserve the option
	// of re-introducing terminal-state rendering without an enum
	// migration.
	PromptDone
)

// String renders a PromptState for logs / diagnostics.
func (s PromptState) String() string {
	switch s {
	case PromptRunning:
		return "running"
	case PromptDone:
		return "done"
	}
	return "unknown"
}