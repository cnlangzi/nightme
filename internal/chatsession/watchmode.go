// F-watch §3.1.1: per-chat message-watch mode.
//
// The WatchMode enum lives in package registry (see
// internal/registry/watch_mode.go) so that registry.ChatSessionEntry
// can persist it without an import cycle. This file re-exports the
// type as chatsession.WatchMode for callers that already import the
// chatsession package and don't want to add a second import path.
//
// See docs/SPEC.md §3.1.1 + docs/channel/feishu.md §6.11 for the
// design rationale.
package chatsession

import "github.com/cnlangzi/nightme/internal/registry"

// WatchMode is a thin alias for registry.WatchMode so callers in
// this package (ChatSession.SetWatchMode / WatchMode, the /watch
// handler, etc.) can refer to the type without a separate import.
type WatchMode = registry.WatchMode

// Re-export the enum constants so callers don't need to import
// both packages.
const (
	WatchModeMention = registry.WatchModeMention
	WatchModeAll     = registry.WatchModeAll
)

// ParseWatchMode parses the slash-command arg into a WatchMode.
// Returns false for unknown values so the caller can reply with
// a usage hint. Delegates to registry.ParseWatchMode.
func ParseWatchMode(s string) (WatchMode, bool) {
	return registry.ParseWatchMode(s)
}