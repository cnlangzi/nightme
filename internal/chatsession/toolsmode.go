// F-38 §3.1.3: per-chat tool-event display toggle.
//
// The ToolsMode enum lives in package registry (see
// internal/registry/tools_mode.go) so that registry.ChatSessionEntry
// can persist it without an import cycle. This file re-exports the
// type as chatsession.ToolsMode for callers that already import
// the chatsession package and don't want to add a second import
// path.
//
// See docs/SPEC.md §3.1.3 + docs/channel/feishu.md for the
// design rationale.
package chatsession

import "github.com/cnlangzi/nightme/internal/registry"

// ToolsMode is a thin alias for registry.ToolsMode so callers in
// this package (ChatSession.SetToolsMode / ToolsMode, the /tools
// handler, etc.) can refer to the type without a separate import.
type ToolsMode = registry.ToolsMode

// Re-export the enum constants so callers don't need to import
// both packages.
const (
	ToolsModeHide = registry.ToolsModeHide
	ToolsModeShow = registry.ToolsModeShow
)

// ParseToolsMode parses the slash-command arg into a ToolsMode.
// Returns false for unknown values so the caller can reply with
// a usage hint. Delegates to registry.ParseToolsMode.
func ParseToolsMode(s string) (ToolsMode, bool) {
	return registry.ParseToolsMode(s)
}
