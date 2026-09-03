package messages

// ReactionKind tags a user-emoji response on an interactive card.
// v1.5 retired the gtw-side producer (WorktreeFailChoice +
// emitWorktreeFailDraft); the constants still exist for:
//   - in-flight Feishu messages that were rendered before the
//     v1.5 upgrade and carry legacy `act:/gtw/<id>` tags;
//   - non-gtw cards (channels package) that may wire the same
//     emoji vocabulary.
//
// ActionLookup translates the legacy wire tags into ReactionKind.
// The whitelist (currently `act:/gtw/cancel` and
// `act:/gtw/worktree-retry`) is back-compat for in-flight
// Feishu reactions; new gtw flows don't emit these tags.
//
// Lives in the messages package (not command/gtw) so that the
// Feishu adapter can translate card actions without depending on
// internal/command/gtw (which would violate the "channel
// adapters only depend on messages / gateway / runtime"
// boundary).
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
// TestActionLookupUnknown in internal/command/gtw tests the
// whitelist semantics directly (unknown tags return ok=false).
// TestRenderActionLookupContract was removed along with the
// gtw-side renderer it was locking down.
func ActionLookup(tag string) (ReactionKind, bool) {
	switch tag {
	case "act:/gtw/cancel":
		return ReactionCancel, true
	case "act:/gtw/worktree-retry":
		return ReactionRetry, true
	}
	return "", false
}
