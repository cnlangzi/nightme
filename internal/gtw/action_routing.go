package gtw

// gtwActionPrefixes maps F-46 decision-card button actions
// (`act:/gtw/<action>`) to the ReactionKind synthesised into the
// existing reaction pipeline. Keep this map aligned with the buttons
// actually rendered by render.go / `/gtw test` scenarios (ok, yes-no,
// three, …). Do not park future (F-49+) or alias keys here.
//
// See docs/feat/F-46-interactive-cards.md §3.1.
var gtwActionPrefixes = map[string]ReactionKind{
	"act:/gtw/branch-newv2":   ReactionNewV2,  // §5.3.1 🆕 — /gtw test three
	"act:/gtw/branch-join":    ReactionJoin,   // §5.3.1 🔗 — /gtw test three
	"act:/gtw/worktree-retry": ReactionRetry,  // §5.3.3 🔄 — /gtw test ok
	"act:/gtw/cancel":         ReactionCancel, // ❌ — any decision card
}

// ActionLookup resolves an F-46 `act:/gtw/<action>` string to the
// ReactionKind the gtw action handler should dispatch. Returns false
// for unknown / unmapped strings so the caller can toast and drop the
// click. Exported so channel adapters can route card.action.trigger
// without an import cycle.
func ActionLookup(action string) (ReactionKind, bool) {
	if action == "" {
		return "", false
	}
	if emoji, ok := gtwActionPrefixes[action]; ok {
		return emoji, true
	}
	// Unknown prefix (nav: / cmd: / opt: / …) or retired key: caller decides.
	return "", false
}
