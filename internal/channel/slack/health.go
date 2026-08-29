package slack

import (
	"encoding/json"
	"sync"
	"time"
)

// healthEventCap bounds the retained connection-event ring.
const healthEventCap = 32

// HealthEventKind labels a connection lifecycle transition.
type HealthEventKind string

const (
	HealthConnecting   HealthEventKind = "connecting"
	HealthConnected    HealthEventKind = "connected"
	HealthDisconnected HealthEventKind = "disconnected"
	HealthError        HealthEventKind = "error"
)

// HealthEvent is one connection lifecycle transition.
type HealthEvent struct {
	Kind HealthEventKind `json:"kind"`
	At   time.Time       `json:"at"`
	Msg  string          `json:"msg,omitempty"`
}

// socketHealth tracks Socket Mode connection state for the
// daemoncontrol "health" RPC.
//
// Socket Mode is a long-lived WebSocket, same shape as Feishu's
// larkws connection, so it needs the same observability Feishu has
// in internal/channel/feishu/health.go. Telegram's long-polling loop
// has no equivalent because there is no connection to lose.
// Returning an empty payload here would be legal per the Channel
// contract but would leave `nightme doctor` blind to the Slack
// channel.
type socketHealth struct {
	mu sync.Mutex

	connected     bool
	connectedAt   time.Time
	disconnectAt  time.Time
	connectCount  int
	errorCount    int
	lastError     string
	inboundCount  int64
	outboundCount int64
	lastInboundAt time.Time
	events        []HealthEvent
}

// HealthSnapshot is the JSON shape served by the health RPC.
type HealthSnapshot struct {
	Connected     bool          `json:"connected"`
	ConnectedAt   time.Time     `json:"connected_at,omitempty"`
	DisconnectAt  time.Time     `json:"disconnected_at,omitempty"`
	ConnectCount  int           `json:"connect_count"`
	ErrorCount    int           `json:"error_count"`
	LastError     string        `json:"last_error,omitempty"`
	InboundCount  int64         `json:"inbound_count"`
	OutboundCount int64         `json:"outbound_count"`
	LastInboundAt time.Time     `json:"last_inbound_at,omitempty"`
	Events        []HealthEvent `json:"events,omitempty"`
}

func newSocketHealth() *socketHealth {
	return &socketHealth{}
}

func (h *socketHealth) record(kind HealthEventKind, msg string) {
	if h == nil {
		return
	}
	now := time.Now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()

	switch kind {
	case HealthConnected:
		h.connected = true
		h.connectedAt = now
		h.connectCount++
	case HealthDisconnected:
		h.connected = false
		h.disconnectAt = now
	case HealthError:
		h.errorCount++
		h.lastError = msg
	}

	h.events = append(h.events, HealthEvent{Kind: kind, At: now, Msg: msg})
	if len(h.events) > healthEventCap {
		h.events = h.events[len(h.events)-healthEventCap:]
	}
}

func (h *socketHealth) recordInbound() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.inboundCount++
	h.lastInboundAt = time.Now().UTC()
	h.mu.Unlock()
}

func (h *socketHealth) recordOutbound() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.outboundCount++
	h.mu.Unlock()
}

func (h *socketHealth) snapshot() HealthSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	events := make([]HealthEvent, len(h.events))
	copy(events, h.events)
	return HealthSnapshot{
		Connected:     h.connected,
		ConnectedAt:   h.connectedAt,
		DisconnectAt:  h.disconnectAt,
		ConnectCount:  h.connectCount,
		ErrorCount:    h.errorCount,
		LastError:     h.lastError,
		InboundCount:  h.inboundCount,
		OutboundCount: h.outboundCount,
		LastInboundAt: h.lastInboundAt,
		Events:        events,
	}
}

func (h *socketHealth) marshal() (json.RawMessage, error) {
	return json.Marshal(h.snapshot())
}
