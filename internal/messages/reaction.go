package messages

// ReactionKind tags a single user-emoji response on a gtw draft card.
// Each draft kind has a fixed set of accepted reaction kinds; other
// emojis are no-ops.
//
// Lives in the messages package (not command/gtw) because the same
// vocabulary is shared between transport (Feishu card buttons +
// user-emoji reactions) and the gtw draft flow. Channels need
// ActionLookup to translate card-action tags into a ReactionKind so
// they can stamp the value on the InboundMessage.Reaction payload
// that gateway routes to gtw's action dispatcher.
//
// Moved from internal/command/gtw (was ReactionKind in types.go +
// constants block) so that the Feishu adapter can drop its import
// of internal/command/gtw — the original home forced every channel
// package that wanted to translate card actions to depend on the
// command layer, which violates the "channel adapters only depend
// on messages / gateway / runtime" boundary.
type ReactionKind string

const (
	// ReactionConfirm: ✅ — accept the current draft, advance the flow.
	ReactionConfirm ReactionKind = "✅"
	// ReactionEdit: ✏️ — reserved; no current ActionLookup mapping.
	// Reintroduce when a card that emits an "edit" reaction lands.
	ReactionEdit ReactionKind = "✏️"
	// ReactionCancel: ❌ — abort the current gtw step; rollback side
	// effects where possible. v1.5: gtw no longer emits a card
	// with this emoji (WorktreeFailChoice retired); the constant
	// stays for channels / future flows that wire it up.
	ReactionCancel ReactionKind = "❌"
	// ReactionRetry: 🔄 — last step failed; re-run. v1.5: same
	// as ReactionCancel — gtw no longer emits this emoji
	// (WorktreeFailChoice retired); constant kept for future use.
	ReactionRetry ReactionKind = "🔄"
)

// ActionLookup maps a Feishu card-action tag to the corresponding
// ReactionKind. Returns (kind, true) on match; (zero, false) on no
// match. Used by the Feishu adapter to translate button clicks into
// reaction events on the same code path as emoji reactions
// (F-46 §5.3.2).
//
// The tag strings are the contract between gtw's card renderer
// (internal/command/gtw/render.go) and any channel adapter that
// wants to translate clicks — both sides MUST agree on the exact
// spelling. v1.5 retired WorktreeFailChoice; gtw no longer emits
// any of these tags:
//
//	act:/gtw/cancel         → ReactionCancel  (WorktreeFailChoice, retired)
//	act:/gtw/worktree-retry → ReactionRetry   (WorktreeFailChoice, retired)
//
// (F-XX §3.1 removed the BranchExistsChoice card; the previous
// 🆕 / 🔗 reactions and the branch-newv2 / branch-join action
// tags are no longer recognised. See feishu-rendering.md
// action map.)
//
// reaction_test.go pins this contract by walking every Choice in
// rendered cards and asserting ActionLookup recognises it.
func ActionLookup(tag string) (ReactionKind, bool) {
	switch tag {
	case "act:/gtw/cancel":
		return ReactionCancel, true
	case "act:/gtw/worktree-retry":
		return ReactionRetry, true
	}
	return "", false
}
