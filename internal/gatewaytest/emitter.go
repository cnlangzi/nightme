// Package gatewaytest provides shared test fixtures for the
// gateway-side packages (gateway, gateway/outbound, command).
// It is a leaf — only depends on internal/messages (a leaf
// type-only package) and stdlib.
//
// Lives in its own package so test code in the gateway
// (in-package) and the command (external) packages can both
// reach the same fixtures without copy-paste. The package
// itself is internal so production binaries don't pull in
// test-only code.
//
// Why messages.OutboundMessage and not gateway.OutboundMessage:
// the latter is a type alias for the former, so the two are
// structurally identical. Using messages.* keeps gatewaytest
// a leaf (it can't import gateway, which would create a cycle
// with the gateway package's own tests).
package gatewaytest

import (
	"context"

	"github.com/cnlangzi/nightme/internal/messages"
)

// emitterLike is the local structural interface that NoopEmitter
// must satisfy. Both outbound.Emitter and gateway.Emitter have
// the same method set (Send / SendCard on messages.OutboundMessage
// / gateway.OutboundMessage which is a type alias), so a single
// concrete type satisfies both interfaces by virtue of Go's
// structural interface satisfaction. We assert this with a local
// interface so the package doesn't have to import either
// outbound or gateway (both of which would create cycles).
type emitterLike interface {
	Send(ctx context.Context, msg messages.OutboundMessage) error
	SendCard(ctx context.Context, msg messages.OutboundMessage) (string, error)
}

// NoopEmitter is a do-nothing implementation that satisfies
// outbound.Emitter and gateway.Emitter. Tests use it to construct
// a Gateway (which requires an Emitter at New() time) or to
// bind to a ChatSession without exercising the outbound path.
// Send returns nil; SendCard returns the empty message id.
type NoopEmitter struct{}

// Compile-time guard: NoopEmitter must satisfy the local
// emitterLike shape. Any drift in either of the two real
// Emitter interface signatures (outbound.Emitter or
// gateway.Emitter) will surface at test compile time as a
// "wrong type for method Send" or similar at the test
// call sites.
var _ emitterLike = NoopEmitter{}

// Send is a no-op.
func (NoopEmitter) Send(context.Context, messages.OutboundMessage) error {
	return nil
}

// SendCard is a no-op; returns the empty message id.
func (NoopEmitter) SendCard(context.Context, messages.OutboundMessage) (string, error) {
	return "", nil
}
