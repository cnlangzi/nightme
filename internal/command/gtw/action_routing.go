package gtw

// ActionLookup maps a Feishu card-action tag (e.g. "act:/gtw/label-force")
// to the corresponding ReactionKind emoji. Returns (kind, true) on
// match; (zero, false) on no match. Used by the Feishu adapter
// to translate button clicks into reaction events on the same
// code path as emoji reactions (F-46 §5.3.2).
func ActionLookup(tag string) (ReactionKind, bool) {
	switch tag {
	case "act:/gtw/confirm", "act:/gtw/branch-exists-confirm":
		return ReactionConfirm, true
	case "act:/gtw/cancel", "act:/gtw/branch-exists-cancel":
		return ReactionCancel, true
	case "act:/gtw/new-v2", "act:/gtw/branch-exists-new-v2":
		return ReactionNewV2, true
	case "act:/gtw/join", "act:/gtw/branch-exists-join":
		return ReactionJoin, true
	case "act:/gtw/label-force":
		return ReactionForce, true
	case "act:/gtw/retry", "act:/gtw/worktree-fail-retry":
		return ReactionRetry, true
	}
	return "", false
}

// EmojiToKind is the inverse of Kind's string. Returns the
// ReactionKind for a raw reaction emoji string ("✅" / "🆕" / ...).
func EmojiToKind(emoji string) (ReactionKind, bool) {
	switch emoji {
	case "✅":
		return ReactionConfirm, true
	case "✏️":
		return ReactionEdit, true
	case "❌":
		return ReactionCancel, true
	case "🆕":
		return ReactionNewV2, true
	case "🔗":
		return ReactionJoin, true
	case "🤝":
		return ReactionForce, true
	case "🔄":
		return ReactionRetry, true
	}
	return "", false
}
