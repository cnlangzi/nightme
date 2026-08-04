// F-think §3.1.2: per-chat thinking-content display toggle.
//
// The ThinkMode enum lives in package registry (see
// internal/registry/think_mode.go) so that registry.ChatSessionEntry
// can persist it without an import cycle. This file re-exports the
// type as chatsession.ThinkMode for callers that already import the
// chatsession package and don't want to add a second import path.
//
// See docs/SPEC.md §3.1.2 + docs/channel/feishu.md for the
// design rationale.
package chatsession

import "github.com/cnlangzi/nightme/internal/registry"

// ThinkMode is a thin alias for registry.ThinkMode so callers in
// this package (ChatSession.SetThinkMode / ThinkMode, the /think
// handler, etc.) can refer to the type without a separate import.
type ThinkMode = registry.ThinkMode

// Re-export the enum constants so callers don't need to import
// both packages.
const (
	ThinkModeShow = registry.ThinkModeShow
	ThinkModeHide = registry.ThinkModeHide
)

// ParseThinkMode parses the slash-command arg into a ThinkMode.
// Returns false for unknown values so the caller can reply with
// a usage hint. Delegates to registry.ParseThinkMode.
func ParseThinkMode(s string) (ThinkMode, bool) {
	return registry.ParseThinkMode(s)
}