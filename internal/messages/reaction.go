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
	// ReactionEdit: ✏️ — "let me change something" (v1: no-op; reserved).
	ReactionEdit ReactionKind = "✏️"
	// ReactionCancel: ❌ — abort the current gtw step; rollback side
	// effects where possible.
	ReactionCancel ReactionKind = "❌"
	// ReactionNewV2: 🆕 — branch already exists; create a -v2 variant.
	ReactionNewV2 ReactionKind = "🆕"
	// ReactionJoin: 🔗 — branch already exists; reuse the existing one
	// without creating a new worktree.
	ReactionJoin ReactionKind = "🔗"
	// ReactionForce: 🤝 — somebody else holds the label; force-take it.
	ReactionForce ReactionKind = "🤝"
	// ReactionRetry: 🔄 — last step failed; re-run.
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
// spelling. The renderer currently emits:
//
//	act:/gtw/cancel         → ReactionCancel  (WorktreeFailChoice)
//	act:/gtw/worktree-retry → ReactionRetry   (WorktreeFailChoice)
//
// (F-XX §3.1 removed the BranchExistsChoice card and its
// 🆕 / 🔗 buttons; branch-newv2 and branch-join are no longer
// recognised — see feishu-rendering.md action map.)
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
