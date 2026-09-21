// Package api holds the dsh bridge's public-facing types and
// the Bridge interface contract. It imports nothing from
// nightme — only the standard library — so the relay package
// can implement the interface without an import cycle.
//
// Why a separate package: the relay (internal/bridge/dsh/relay)
// and the drivers (internal/bridge/dsh) both need these types.
// If they lived in `dsh`, the relay would have to import dsh,
// and the drivers already import relay — instant cycle. The
// `api` sub-package is the lowest common denominator: types
// only, no logic.
package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// SessionID is the opaque dsh.web-issued session uuid. It is
// stable across dsh respawns: re-creating the same id+cwd via
// session.create joins the existing session and its event log.
type SessionID = string

// ConnectOpts captures the inputs to Bridge.Connect. SessionID
// is the only field with re-attach semantics: empty → fresh
// session; non-empty → idempotent re-attach to that sessionId.
//
// Workspace is the dsh "workspace" (repo-scoped key under which
// sessions group); CWD is the runtime cwd passed to the agent.
// The two diverge when the chat is in a subdirectory of a repo:
// Workspace is the repo root, CWD is the subdir. Most bridges
// accept the same value for both.
type ConnectOpts struct {
	SessionID      SessionID
	Workspace      string
	CWD            string
	PermissionMode string // dsh permission preset; "" → bridge default
}

// SessionHandle is the live handle returned by Connect. The
// struct exists so we can grow it later (model, branch, agent
// preset, ...) without touching every call site.
//
// Named SessionHandle to avoid collision with the dsh wire-
// format Session summary in protocol.go (different shape,
// different purpose).
type SessionHandle struct {
	ID        SessionID
	Workspace string
	CWD       string
	CreatedAt time.Time
}

// Event is the relay's wire-shape event. The api package
// re-exports it so drivers can compile against the bridge
// contract without importing the relay package's full surface.
// Drivers translate to agent.AgentEvent through their existing
// translator / wireState / dispatcher pipeline.
type Event struct {
	Seq  int64
	Type string
	Time int64
	Data json.RawMessage
}

// MuxHandler is the per-session mux callback. method is the
// dsh mux method (e.g. "session/event"); payload is the frame's
// typed data payload (raw JSON, ready to decode); rpcID
// carries the seq when the frame is a session/event, empty
// otherwise. Signature matches host.MuxFrameHandler so the
// relay can pass through without re-decoding.
type MuxHandler = func(method, rpcID string, payload MuxHandlerPayload)

// MuxHandlerPayload is the raw payload type passed to
// MuxHandler. It's an alias for json.RawMessage (which is
// a []byte), declared separately so future changes to the
// payload shape don't ripple through Bridge signatures.
type MuxHandlerPayload = json.RawMessage

// Bridge is the central capability surface for talking to dsh.
//
// What this interface deliberately does NOT expose:
//
//   - The mux WebSocket, the RPC HTTP client, the Router.
//   - Per-driver backfill loops. History() is the only read path;
//     the relay decides whether to serve from a ring buffer, a
//     session/page round trip, or both.
//   - The driver's translator / wireState / dispatcher. Those
//     are per-session state machines that live in driver per the
//     design conversation. The relay exposes MuxSubscribe as
//     the one transport-level hook drivers need for protocol
//     translation; ring buffer fan-out and replay are central.
type Bridge interface {
	// Connect is idempotent on SessionID. Empty → fresh
	// session; non-empty → re-attach via dsh's session.create
	// ({id, cwd}).
	Connect(ctx context.Context, opts ConnectOpts) (SessionHandle, error)

	// Disconnect cancels the session and archives it.
	Disconnect(ctx context.Context, id SessionID) error

	// Send delivers a structured user turn. The ack is fast
	// (dsh inbox enqueue); the turn's events arrive via
	// Subscribe.
	Send(ctx context.Context, id SessionID, blocks []agent.ContentBlock) error

	// Respond answers the most recent permission / question
	// the agent asked. FIFO ordering across pending approvals
	// for a session is the caller's job.
	Respond(ctx context.Context, id SessionID, response string) error

	// Subscribe attaches to the session's event stream.
	// First subscriber opens the underlying session/follow
	// stream; later subscribers receive a replay of the
	// relay's ring buffer followed by live events. The
	// channel closes when ctx fires or the relay shuts down.
	//
	// Phase 3 (focused) returns wire-shape Event; the
	// translator / wireState / dispatcher stay in driver.
	Subscribe(ctx context.Context, id SessionID) (<-chan Event, func(), error)

	// History reads past events. sinceSeq < 0 returns the
	// most recent batch from the ring buffer; sinceSeq >= 0
	// returns events with seq > sinceSeq, paging through
	// session/page if the buffer has rolled past sinceSeq.
	History(ctx context.Context, id SessionID, sinceSeq int64) ([]Event, error)

	// MuxSubscribe attaches a per-session mux frame handler.
	// This is the one transport-level hook left to drivers for
	// protocol translation. The fan-out of frames to multiple
	// subscribers is owned by the relay via Subscribe.
	MuxSubscribe(ctx context.Context, id SessionID, workspace string, handler MuxHandler) (func(), error)

	// Health is a single process-level liveness check.
	Health(ctx context.Context) error

	// Close tears down the relay and the shared dsh host.
	Close() error
}
