package gtw

import (
	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"context"

)

// nopCh is a no-op outbound.Emitter for tests in this package.
// Shared across all _test.go files in the gtw package so each test
// can call newTestChannel() without redeclaring the type.
//
// Mirrors the shape in internal/chatsession/test_helpers_test.go.
type nopCh struct{}

func (nopCh) Send(_ context.Context, _ gateway.OutboundMessage) error { return nil }
func (nopCh) SendCard(_ context.Context, _ gateway.OutboundMessage) (string, error) {
	return "", nil
}
func (nopCh) Patch(_ context.Context, _ gateway.OutboundMessage) error { return nil }

func newTestChannel() outbound.Emitter { return nopCh{} }
