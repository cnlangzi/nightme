package gtw

import (
	"context"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// nopCh is a no-op chatsession.Channel for tests in this package.
// Shared across all _test.go files in the gtw package so each test
// can call newTestChannel() without redeclaring the type.
//
// Mirrors the shape in internal/chatsession/test_helpers_test.go.
type nopCh struct{}

func (nopCh) Send(_ context.Context, _ chatsession.OutboundMessage) error { return nil }
func (nopCh) SendCard(_ context.Context, _ chatsession.OutboundMessage) (string, error) {
	return "", nil
}
func (nopCh) Patch(_ context.Context, _ chatsession.OutboundMessage) error { return nil }

func newTestChannel() chatsession.Channel { return nopCh{} }