// Package feishu — rebuild path tests (F-fix-feishu-reconnect).
//
// These tests exercise the pure-function pieces of the rebuild
// path (isTerminalSDKError, recordLastStartErr, maybeRebuildClient)
// and the snapshot reader. The end-to-end "real Feishu handshake
// after macOS wake" flow is exercised by the existing
// adapter_test.go integration tests; this file just locks down the
// decision logic so a careless edit can't silently widen the rebuild
// loop.
package feishu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// newTestAdapterForRebuild constructs the minimum Adapter needed to
// drive recordLastStartErr / maybeRebuildClient without booting the
// full Feishu surface (no event handler, no prober, no health). We
// don't call NewAdapter here because that path would try to dial.
func newTestAdapterForRebuild() *Adapter {
	return &Adapter{
		rebuild: newRebuildState(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		// cfg is left nil. The rebuild path reads cfg.Feishu.AppID
		// only for log fields — slog with io.Discard handles the
		// nil deref safely because the closures that touch cfg
		// never run under the rebuild-only test paths. Tests that
		// exercise the full rebuild happy path inject cfg directly.
	}
}

// storeErr wraps an error into the *error shape required by
// Adapter.lastStartErr (atomic.Pointer[error]).
func storeErr(err error) *error { return &err }

func TestIsTerminalSDKError_TrueOnClientError(t *testing.T) {
	err := &larkws.ClientError{Code: 1000040350, Msg: "exceed conn limit"}
	if !isTerminalSDKError(err) {
		t.Fatalf("expected *ws.ClientError to be terminal, got false")
	}
}

func TestIsTerminalSDKError_TrueOnWrappedClientError(t *testing.T) {
	base := &larkws.ClientError{Code: 403, Msg: "permission revoked"}
	wrapped := fmt.Errorf("feishu: handshake: %w", base)
	if !isTerminalSDKError(wrapped) {
		t.Fatalf("expected wrapped *ws.ClientError to be terminal, got false")
	}
}

func TestIsTerminalSDKError_FalseOnPlainError(t *testing.T) {
	if isTerminalSDKError(errors.New("network: connection reset")) {
		t.Fatalf("expected plain error to be non-terminal, got true")
	}
	if isTerminalSDKError(context.Canceled) {
		t.Fatalf("expected context.Canceled to be non-terminal, got true")
	}
	if isTerminalSDKError(nil) {
		t.Fatalf("expected nil to be non-terminal, got true")
	}
}

func TestRecordLastStartErr_IgnoresNilAndContextCancel(t *testing.T) {
	a := newTestAdapterForRebuild()

	a.recordLastStartErr(nil)
	if got := a.lastStartErr.Load(); got != nil {
		t.Fatalf("expected lastStartErr to remain nil, got %v", *got)
	}

	a.recordLastStartErr(context.Canceled)
	if got := a.lastStartErr.Load(); got != nil {
		t.Fatalf("expected context.Canceled to be ignored, got %v", *got)
	}
}

func TestRecordLastStartErr_ClassifiesTerminal(t *testing.T) {
	a := newTestAdapterForRebuild()

	ce := &larkws.ClientError{Code: 1000040350, Msg: "exceed conn limit"}
	a.recordLastStartErr(ce)

	if got := a.lastStartErr.Load(); got == nil {
		t.Fatalf("expected lastStartErr populated, got nil")
	} else if !errors.Is(*got, ce) {
		t.Fatalf("expected stored err to match input, got %v", *got)
	}
	if got := a.rebuild.consecutiveFails.Load(); got != 1 {
		t.Fatalf("expected consecutiveFails=1, got %d", got)
	}
	if got := a.rebuild.lastTerminalErr.Load(); got == nil {
		t.Fatalf("expected lastTerminalErr stamped, got nil")
	} else if *got != ce.Error() {
		t.Fatalf("expected lastTerminalErr=%q, got %q", ce.Error(), *got)
	}
	if got := a.rebuild.lastTerminalErrAt.Load(); got == nil {
		t.Fatalf("expected lastTerminalErrAt stamped, got nil")
	}

	ce2 := &larkws.ClientError{Code: 403, Msg: "permission revoked"}
	a.recordLastStartErr(ce2)
	if got := a.rebuild.consecutiveFails.Load(); got != 2 {
		t.Fatalf("expected consecutiveFails=2, got %d", got)
	}
}

func TestRecordLastStartErr_NonTerminalResetsStreak(t *testing.T) {
	a := newTestAdapterForRebuild()

	a.recordLastStartErr(&larkws.ClientError{Code: 1000040350})
	if got := a.rebuild.consecutiveFails.Load(); got != 1 {
		t.Fatalf("expected consecutiveFails=1, got %d", got)
	}

	a.recordLastStartErr(errors.New("connection reset by peer"))
	if got := a.rebuild.consecutiveFails.Load(); got != 0 {
		t.Fatalf("expected consecutiveFails reset to 0, got %d", got)
	}
}

func TestMaybeRebuildClient_NoLastErr_Noop(t *testing.T) {
	a := newTestAdapterForRebuild()
	a.maybeRebuildClient()
	if got := a.rebuild.count.Load(); got != 0 {
		t.Fatalf("expected rebuild count=0, got %d", got)
	}
}

func TestMaybeRebuildClient_NonTerminalLastErr_Noop(t *testing.T) {
	a := newTestAdapterForRebuild()
	a.lastStartErr.Store(storeErr(errors.New("connection reset")))

	a.maybeRebuildClient()
	if got := a.rebuild.count.Load(); got != 0 {
		t.Fatalf("expected non-terminal lastErr to skip rebuild, got count=%d", got)
	}
}

func TestMaybeRebuildClient_MaxConsecutiveSkips(t *testing.T) {
	a := newTestAdapterForRebuild()
	for i := int64(0); i < sdkMaxConsecutiveRebuilds; i++ {
		a.rebuild.consecutiveFails.Add(1)
	}
	a.lastStartErr.Store(storeErr(&larkws.ClientError{Code: 1000040350}))

	before := a.rebuild.skippedMaxStreak.Load()
	a.maybeRebuildClient()
	after := a.rebuild.skippedMaxStreak.Load()

	if after-before != 1 {
		t.Fatalf("expected skippedMaxStreak +1, got %d → %d", before, after)
	}
	if got := a.rebuild.count.Load(); got != 0 {
		t.Fatalf("expected no rebuild under max-streak guard, got count=%d", got)
	}
}

func TestMaybeRebuildClient_CooldownBlocks(t *testing.T) {
	a := newTestAdapterForRebuild()
	recent := time.Now().Add(-(sdkRebuildCooldown / 2))
	a.rebuild.lastAt.Store(&recent)

	a.lastStartErr.Store(storeErr(&larkws.ClientError{Code: 1000040350}))
	before := a.rebuild.skippedCooldown.Load()
	a.maybeRebuildClient()
	after := a.rebuild.skippedCooldown.Load()

	if after-before != 1 {
		t.Fatalf("expected skippedCooldown +1 within cooldown, got %d → %d", before, after)
	}
	if got := a.rebuild.count.Load(); got != 0 {
		t.Fatalf("expected no rebuild within cooldown, got count=%d", got)
	}
}

func TestMaybeRebuildClient_NilClientNoop(t *testing.T) {
	a := newTestAdapterForRebuild()
	a.lastStartErr.Store(storeErr(&larkws.ClientError{Code: 1000040350}))

	a.maybeRebuildClient()
	if got := a.rebuild.count.Load(); got != 0 {
		t.Fatalf("expected rebuild count=0 when client is nil, got %d", got)
	}
}

func TestRebuildStateSnapshot_ZeroAndPopulated(t *testing.T) {
	rs := newRebuildState()
	if snap := rs.snapshot(); snap != (RebuildSnapshot{}) {
		t.Fatalf("expected zero snapshot on fresh state, got %+v", snap)
	}

	now := time.Now()
	rs.count.Add(3)
	rs.lastAt.Store(&now)
	rs.consecutiveFails.Add(2)
	errStr := "boom"
	rs.lastTerminalErr.Store(&errStr)
	rs.lastTerminalErrAt.Store(&now)

	snap := rs.snapshot()
	if snap.RebuildCount != 3 {
		t.Errorf("RebuildCount=%d, want 3", snap.RebuildCount)
	}
	if snap.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures=%d, want 2", snap.ConsecutiveFailures)
	}
	if snap.LastRebuildAt.IsZero() {
		t.Errorf("LastRebuildAt should be set")
	}
	if snap.LastTerminalErr != "boom" {
		t.Errorf("LastTerminalErr=%q, want %q", snap.LastTerminalErr, "boom")
	}
	if snap.LastTerminalErrAt.IsZero() {
		t.Errorf("LastTerminalErrAt should be set")
	}
}

func TestRebuildSnapshotFromState_NilSafe(t *testing.T) {
	if got := RebuildSnapshotFromState(nil); got != (RebuildSnapshot{}) {
		t.Fatalf("expected zero snapshot for nil state, got %+v", got)
	}
}
