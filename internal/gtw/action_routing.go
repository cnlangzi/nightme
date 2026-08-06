package gtw

// gtwActionPrefixes maps the F-46 `act:/gtw/<scenario>` action strings
// used by decision-card buttons (see docs/feat/F-46-interactive-cards.md
// §3.1) to the ReactionKind the action handler should synthesise.
// The emoji value is what flows into the InboundMessage.Reaction so
// the existing reaction pipeline picks it up unchanged.
var gtwActionPrefixes = map[string]ReactionKind{
	"act:/gtw/branch-newv2":   ReactionNewV2,   // §5.3.1  🆕
	"act:/gtw/branch-join":    ReactionJoin,    // §5.3.1  🔗
	"act:/gtw/worktree-retry": ReactionRetry,   // §5.3.3  🔄
	"act:/gtw/label-force":    ReactionForce,   // §5.3.2  🤝 (F-49)
	"act:/gtw/cancel":         ReactionCancel,  // any draft ❌
	"act:/gtw/worktree-cancel": ReactionCancel, // §5.3.3 ❌ (explicit alias)
}

// ActionLookup resolves an F-46 `act:/gtw/<scenario>` action string
// to the ReactionKind the gtw action handler should dispatch.
// Returns false for unknown / unmapped strings so the caller can
// render an error toast and drop the click. Exported so the
// Feishu adapter (and any future channel adapter) can route
// card.action.trigger callbacks without an import cycle.
func ActionLookup(action string) (ReactionKind, bool) {
	if action == "" {
		return "", false
	}
	if emoji, ok := gtwActionPrefixes[action]; ok {
		return emoji, true
	}
	// Unknown prefix (nav: / cmd: / opt: / …): caller decides.
	return "", false
}
