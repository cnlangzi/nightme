// Package gatewaytest provides shared test fixtures for the
// gateway-side packages (gateway, gateway/outbound, gateway/inbound,
// command). It is a leaf — only depends on internal/messages
// (a leaf type-only package) and stdlib.
//
// Lives in its own package so test code in the gateway
// (in-package) and the command (external) packages can both
// reach the same fixtures without copy-paste. The package
// itself is internal so production binaries don't pull in
// test-only code.
//
// Why messages.OutboundMessage: outbound.Emitter's parameter
// is messages.OutboundMessage directly. Using messages.* keeps
// gatewaytest a leaf (it can't import outbound, which would
// create a cycle: gateway → outbound, and gateway tests
// already import gatewaytest).
package gatewaytest

import (
	"context"

	"github.com/cnlangzi/nightme/internal/messages"
)

// emitterLike is the local structural interface that NoopEmitter
// must satisfy. outbound.Emitter has the same method set (Send
// / SendCard on messages.OutboundMessage), so the single
// concrete type satisfies it via Go's structural interface
// satisfaction. We assert this with a local interface so the
// package doesn't have to import outbound (which would create
// a cycle).
type emitterLike interface {
	Send(ctx context.Context, msg messages.OutboundMessage) error
	SendCard(ctx context.Context, msg messages.OutboundMessage) (string, error)
}

// NoopEmitter is a do-nothing implementation that satisfies
// outbound.Emitter. Tests use it to construct a Gateway (which
// requires an Emitter at New() time) or to bind to a
// ChatSession without exercising the outbound path. Send
// returns nil; SendCard returns the empty message id.
type NoopEmitter struct{}

// Compile-time guard: NoopEmitter must satisfy the local
// emitterLike shape. Any drift in outbound.Emitter's signature
// will surface at test compile time as a "wrong type for method
// Send" or similar at the test call sites.
var _ emitterLike = NoopEmitter{}

// Send is a no-op.
func (NoopEmitter) Send(context.Context, messages.OutboundMessage) error {
	return nil
}

// SendCard is a no-op; returns the empty message id.
func (NoopEmitter) SendCard(context.Context, messages.OutboundMessage) (string, error) {
	return "", nil
}
