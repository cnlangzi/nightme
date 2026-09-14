// control_wire.go — wire types for the dsh session/control stream's
// modelSelection projection. Lives in the host package because the
// stream translator (host/stream.go) needs to decode it on the WS
// readLoop goroutine; the dsh parent package has its own copy in
// protocol.go that mirrors the same field names for the driver-side
// post-handshake read path (which is going away in favor of the
// projection-based live read).

package host

// modelSelectionWire mirrors dsh's `ModelSelection`
// (`packages/api/session-controller/src/types.ts`). `Provider` is the
// registered route key (e.g. "minimax-cn"), `Model` is the
// provider-owned model id (e.g. "MiniMax-M3"). ReasoningEffort is
// optional and only populated when the adapter exposes it for this
// exact route.
//
// The bridge carries Model verbatim through to the runtime footer
// (provider:model would be too wide — runtime compares against the
// `agent.UsageInfo.ContextWindow` table and the channel footer wants
// a single token).
type modelSelectionWire struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

// modelSelectionProjection is the projection value the host
// publishes per session on the session/control stream
// (`packages/api/session-controller/src/model-selection-projection.ts::wire`).
//
//   - lastUsed: model selection consumed by the latest recorded
//     model request (null until the first turn completes).
//   - next: selection the next request will use, falling back to
//     lastUsed when no user override is pending.
//
// Bridge resolves the live model as `next ?? lastUsed` — matches
// dsh's own resolution in
// `defaultSelection: () => current.next ?? defaultSelection()`.
type modelSelectionProjection struct {
	LastUsed *modelSelectionWire `json:"lastUsed"`
	Next     *modelSelectionWire `json:"next"`
}

// resolveModel picks `next ?? lastUsed` and returns just the model
// id. Returns "" when both are nil (fresh session that hasn't
// picked yet).
func (p modelSelectionProjection) resolveModel() string {
	if p.Next != nil && p.Next.Model != "" {
		return p.Next.Model
	}
	if p.LastUsed != nil {
		return p.LastUsed.Model
	}
	return ""
}
