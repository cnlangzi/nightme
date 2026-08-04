// Package feishu — WebSocket lifecycle observability.
//
// F-40 (planned): when the Feishu WS disconnects (network blip, computer
// sleep, server maintenance, etc.) the SDK larkws auto-reconnects, but
// nightme had no visibility into the lifecycle. This file provides the
// in-memory state struct + atomic updates so the daemoncontrol server
// can serve `nightme health` answers, and so the adapter's lifecycle
// callbacks can emit structured slog lines ops can grep for.
//
// Thread safety: all fields are accessed under wsHealthMu (RWMutex).
// The struct is updated from three places:
//
//   - SDK lifecycle callbacks (larkws.WithOnReconnecting / OnReconnected
//     / OnDisconnected) on the adapter's main goroutine
//   - Inbound message dispatch (handleMessage in receipt.go etc.) on
//     the per-event goroutine from receiveMessageLoop
//   - Outbound send success path (logOutgoing in adapter.go) on whatever
//     goroutine called Send
//
// Read path: Adapter.Health() and the daemoncontrol server serve reads.
// Updates are infrequent (only on lifecycle events, not per-message)
// so a single mutex is sufficient — no atomic primitives needed.
package feishu

import (
	"sync"
	"time"
)

// WSHealth tracks the live state of the Feishu WebSocket connection.
// Persisted to ~/.local/share/nightme/health.json on every update so
// `nightme health` can read it without IPC.
type WSHealth struct {
	mu sync.RWMutex

	// Connected is the latest known state. Set true on OnReady /
	// OnReconnected; false on OnDisconnected / first-disconnect.
	Connected bool

	// LastConnectedAt is when the SDK last reported a successful
	// connect (OnReady / OnReconnected).
	LastConnectedAt time.Time

	// LastDisconnectedAt is when the SDK last reported a disconnect.
	LastDisconnectedAt time.Time

	// ReconnectCount is the cumulative number of reconnects since
	// daemon start. Reset never (the CLI tool can compute hourly
	// window counts from EventRing).
	ReconnectCount int

	// LastError is the most recent error string from OnError /
	// reconnect failure. Empty when healthy.
	LastError string
	LastErrorAt time.Time

	// LastInboundAt is when we last successfully dispatched an
	// inbound event (the message we wanted to handle). Drives
	// "inbound stuck" alerts.
	LastInboundAt time.Time
	LastInboundChatID string

	// LastOutboundAt is when we last successfully posted a message
	// to Feishu (SendCard / PatchMessage / SendMessageText). Drives
	// "outbound stuck" alerts.
	LastOutboundAt time.Time

	// EventRing is a fixed-size ring of the most recent lifecycle
	// events. Used by the health command to show the user what
	// happened recently without grepping the log file.
	EventRing []HealthEvent

	// InboundRing / OutboundRing are smaller rings of recent
	// successfully-handled events. Drives stale-detection.
	InboundRing  []InboundSample
	OutboundRing []OutboundSample
}

// HealthEventKind enumerates the kinds of WS lifecycle events we record.
type HealthEventKind string

const (
	HealthEventConnect       HealthEventKind = "connect"        // SDK OnReady / OnReconnected
	HealthEventDisconnect    HealthEventKind = "disconnect"     // SDK OnDisconnected
	HealthEventReconnecting  HealthEventKind = "reconnecting"   // SDK OnReconnecting
	HealthEventError         HealthEventKind = "error"           // SDK OnError or send retry exhausted
	HealthEventInbound       HealthEventKind = "inbound"         // received an event from Feishu
	HealthEventOutbound      HealthEventKind = "outbound"        // sent a message to Feishu
)

// HealthEvent is one entry in the WSHealth.EventRing.
type HealthEvent struct {
	At      time.Time         `json:"at"`
	Kind    HealthEventKind   `json:"kind"`
	Message string            `json:"message,omitempty"`
}

// InboundSample is a tiny sample of a successfully-handled inbound
// event. Used by `nightme health` to surface "last inbound" details.
type InboundSample struct {
	At     time.Time `json:"at"`
	ChatID string    `json:"chat_id"`
	Kind   string    `json:"kind"` // "text" / "tool_start" / ...
}

// OutboundSample is the same for outbound.
type OutboundSample struct {
	At     time.Time `json:"at"`
	ChatID string    `json:"chat_id"`
	Kind   string    `json:"kind"` // "send_card" / "patch_message" / ...
}

const (
	healthEventRingSize    = 32
	healthInboundRingSize  = 8
	healthOutboundRingSize = 8
)

// Snapshot returns a deep-enough copy of the health state for the
// `nightme health` subcommand. Read-locked; callers may mutate freely.
func (h *WSHealth) Snapshot() WSHealthSnapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	snap := WSHealthSnapshot{
		Connected:           h.Connected,
		LastConnectedAt:     h.LastConnectedAt,
		LastDisconnectedAt:  h.LastDisconnectedAt,
		ReconnectCount:      h.ReconnectCount,
		LastError:           h.LastError,
		LastErrorAt:         h.LastErrorAt,
		LastInboundAt:       h.LastInboundAt,
		LastInboundChatID:   h.LastInboundChatID,
		LastOutboundAt:      h.LastOutboundAt,
		EventRing:           append([]HealthEvent(nil), h.EventRing...),
		InboundRing:         append([]InboundSample(nil), h.InboundRing...),
		OutboundRing:        append([]OutboundSample(nil), h.OutboundRing...),
	}
	return snap
}

// WSHealthSnapshot is the cross-process wire format — it must be
// JSON-marshalable so `nightme health` can read it via the daemon's
// status file (or via a future daemoncontrol "health" command).
type WSHealthSnapshot struct {
	Connected           bool             `json:"connected"`
	LastConnectedAt     time.Time        `json:"last_connected_at"`
	LastDisconnectedAt  time.Time        `json:"last_disconnected_at"`
	ReconnectCount      int              `json:"reconnect_count"`
	LastError           string           `json:"last_error"`
	LastErrorAt         time.Time        `json:"last_error_at"`
	LastInboundAt       time.Time        `json:"last_inbound_at"`
	LastInboundChatID   string           `json:"last_inbound_chat_id"`
	LastOutboundAt      time.Time        `json:"last_outbound_at"`
	EventRing           []HealthEvent    `json:"event_ring"`
	InboundRing         []InboundSample  `json:"inbound_ring"`
	OutboundRing        []OutboundSample `json:"outbound_ring"`
}

// --- Mutators (called from adapter callbacks and message handlers) ---

// recordConnect bumps the connect counters and pushes a HealthEvent.
func (h *WSHealth) recordConnect(at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Connected = true
	h.LastConnectedAt = at
	h.LastError = ""
	h.pushEventLocked(HealthEvent{At: at, Kind: HealthEventConnect})
}

// recordDisconnect flips Connected to false and pushes a HealthEvent.
func (h *WSHealth) recordDisconnect(at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Connected {
		// First disconnect since last connect.
		h.LastDisconnectedAt = at
	}
	h.Connected = false
	h.pushEventLocked(HealthEvent{At: at, Kind: HealthEventDisconnect})
}

// recordReconnecting bumps ReconnectCount and pushes a HealthEvent.
// SDK calls this BEFORE the reconnect attempt loop runs (i.e. once per
// disconnect cycle, not once per retry).
func (h *WSHealth) recordReconnecting(at time.Time, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ReconnectCount++
	h.pushEventLocked(HealthEvent{At: at, Kind: HealthEventReconnecting, Message: msg})
}

// recordError stores the latest error string and pushes a HealthEvent.
// Used by SDK OnError and by F-36 retry_exhausted degradation paths.
func (h *WSHealth) recordError(at time.Time, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.LastError = msg
	h.LastErrorAt = at
	h.pushEventLocked(HealthEvent{At: at, Kind: HealthEventError, Message: msg})
}

// recordInbound stamps the last-inbound-at timestamp and pushes a
// sample into the inbound ring. Caller passes chatID + event kind.
func (h *WSHealth) recordInbound(at time.Time, chatID, kind string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.LastInboundAt = at
	h.LastInboundChatID = chatID
	h.pushEventLocked(HealthEvent{At: at, Kind: HealthEventInbound})
	h.pushInboundLocked(InboundSample{At: at, ChatID: chatID, Kind: kind})
}

// recordOutbound stamps the last-outbound-at timestamp and pushes a
// sample into the outbound ring.
func (h *WSHealth) recordOutbound(at time.Time, chatID, kind string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.LastOutboundAt = at
	h.pushEventLocked(HealthEvent{At: at, Kind: HealthEventOutbound})
	h.pushOutboundLocked(OutboundSample{At: at, ChatID: chatID, Kind: kind})
}

// pushEventLocked appends to the ring, evicting oldest if over capacity.
// Caller holds h.mu.
func (h *WSHealth) pushEventLocked(ev HealthEvent) {
	if len(h.EventRing) >= healthEventRingSize {
		h.EventRing = h.EventRing[1:]
	}
	h.EventRing = append(h.EventRing, ev)
}

// pushInboundLocked same, for the smaller inbound ring.
func (h *WSHealth) pushInboundLocked(s InboundSample) {
	if len(h.InboundRing) >= healthInboundRingSize {
		h.InboundRing = h.InboundRing[1:]
	}
	h.InboundRing = append(h.InboundRing, s)
}

// pushOutboundLocked same, for the outbound ring.
func (h *WSHealth) pushOutboundLocked(s OutboundSample) {
	if len(h.OutboundRing) >= healthOutboundRingSize {
		h.OutboundRing = h.OutboundRing[1:]
	}
	h.OutboundRing = append(h.OutboundRing, s)
}