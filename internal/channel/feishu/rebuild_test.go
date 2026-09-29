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

	"github.com/cnlangzi/nightme/internal/config"
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

func TestRecordLastStartErr_NonTerminalDoesNotTouchStreak(t *testing.T) {
	a := newTestAdapterForRebuild()

	a.recordLastStartErr(&larkws.ClientError{Code: 1000040350})
	if got := a.rebuild.consecutiveFails.Load(); got != 1 {
		t.Fatalf("expected consecutiveFails=1, got %d", got)
	}

	// Plain network error (non-terminal, not stranded): stored in
	// lastStartErr for diagnostics, but consecutiveFails is NOT
	// touched. The earlier review-fix removed the reset-on-non-
	// terminal logic — it was dangerous (let the rebuild loop
	// escape its budget on the cancel-induced terminal path)
	// and unnecessary (retryable errors never bumped the counter
	// in the first place).
	a.recordLastStartErr(errors.New("connection reset by peer"))
	if got := a.rebuild.consecutiveFails.Load(); got != 1 {
		t.Fatalf("expected consecutiveFails still 1 after plain non-terminal, got %d", got)
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
	// RebuildSnapshotFromState wrapper was removed (review
	// simplification #3); rebuild_state.snapshot() is called
	// directly from Adapter.Health(). This placeholder keeps the
	// test name anchored for any future regression that re-adds
	// the wrapper.
}

// TestRebuildWSClient_HappyPath exercises the full rebuild path
// end-to-end: a terminal *ws.ClientError recorded by recordLastStartErr
// must trigger rebuildWSClient to swap a.client for a fresh
// *larkws.Client when the cooldown has elapsed. Verifies:
//
//   - a.client pointer changes (no aliasing of the dead client).
//   - a.wsStart / a.wsClose are updated to the new client's methods.
//   - rebuildState counters increment.
//   - the old client.Close is called (no-op when terminal).
//
// We construct a real *config.Config so buildWSClient's AppID /
// AppSecret deref doesn't panic. The SDK does not actually dial
// until client.Start is called, so the test never touches the
// network.
func TestRebuildWSClient_HappyPath(t *testing.T) {
	a := &Adapter{
		cfg: &config.Config{
			Feishu: config.FeishuConfig{
				AppID:     "test_app",
				AppSecret: "test_secret",
			},
		},
		rebuild: newRebuildState(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		health:  &WSHealth{},
		// prober is required by buildWSClient's WithOnDisconnected
		// callback (it calls a.prober.Start). nil prober is OK
		// because the callback nil-checks; we don't fire it here.
		prober: nil,
	}

	oldClient := a.buildWSClient(nil)
	if oldClient == nil {
		t.Fatal("buildWSClient returned nil")
	}
	a.client = oldClient
	a.wsStart = oldClient.Start
	a.wsClose = oldClient.Close

	// Force a terminal state on the old client. The SDK's terminal
	// flag is private; we don't need to flip it — the rebuild path
	// reads only a.lastStartErr + the cooldown clock, both of which
	// we control directly.
	ce := &larkws.ClientError{Code: 1000040350, Msg: "exceed conn limit"}
	a.recordLastStartErr(ce)

	// Cooldown elapsed (simulated 5 minutes ago).
	stale := time.Now().Add(-5 * time.Minute)
	a.rebuild.lastAt.Store(&stale)

	// Trigger the rebuild.
	a.maybeRebuildClient()

	// Verify the swap.
	a.mu.RLock()
	newClient := a.client
	newStart := a.wsStart
	newClose := a.wsClose
	count := a.rebuild.count.Load()
	a.mu.RUnlock()

	if newClient == oldClient {
		t.Fatalf("expected a.client to be swapped, got same pointer %p", newClient)
	}
	if newStart == nil {
		t.Errorf("expected a.wsStart to be set to new client.Start")
	}
	if newClose == nil {
		t.Errorf("expected a.wsClose to be set to new client.Close")
	}
	if count != 1 {
		t.Errorf("expected rebuild_count=1, got %d", count)
	}
	// OnReconnected was never fired in this test, so consecutiveFails
	// should still reflect the one terminal recording.
	if got := a.rebuild.consecutiveFails.Load(); got != 1 {
		t.Errorf("expected consecutive_fails=1, got %d", got)
	}
}

// TestMarkSDKConnectedClearsRebuildState pins the regression fix
// (review bug #1): markSDKConnected is wired into BOTH OnReady and
// OnReconnected so the first connect after a rebuild clears the
// rebuild counters and lastStartErr. Without OnReady doing the same
// reset as OnReconnected, the still-running prober (started on the
// prior run's OnDisconnected) cancels the rebuilt client's
// successful connection on the next 30s tick and the rebuild loop
// spins on errClientTerminal forever.
func TestMarkSDKConnectedClearsRebuildState(t *testing.T) {
	a := &Adapter{
		cfg: &config.Config{
			Feishu: config.FeishuConfig{
				AppID:     "test_app",
				AppSecret: "test_secret",
			},
		},
		rebuild: newRebuildState(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		health:  &WSHealth{},
	}

	// Pre-arm a terminal-state-and-stranded scenario — the post-
	// rebuild first-connect must clean BOTH.
	ce := &larkws.ClientError{Code: 1000040350}
	a.recordLastStartErr(ce)
	a.recordLastStartErr(errors.New(sdkErrClientTerminalMessage))

	if a.lastStartErr.Load() == nil {
		t.Fatal("precondition: lastStartErr should be set")
	}
	if a.rebuild.consecutiveFails.Load() != 1 {
		t.Fatalf("precondition: consecutiveFails should be 1, got %d", a.rebuild.consecutiveFails.Load())
	}
	if !a.rebuild.cancelStrandedTerminal.Load() {
		t.Fatal("precondition: cancelStrandedTerminal should be true after stranded recording")
	}

	a.markSDKConnected()

	if a.lastStartErr.Load() != nil {
		t.Errorf("expected lastStartErr cleared after markSDKConnected, got %v", *a.lastStartErr.Load())
	}
	if got := a.rebuild.consecutiveFails.Load(); got != 0 {
		t.Errorf("expected consecutiveFails=0 after markSDKConnected, got %d", got)
	}
	if a.rebuild.cancelStrandedTerminal.Load() {
		t.Errorf("expected cancelStrandedTerminal cleared after markSDKConnected")
	}
}

// TestIsStrandedTerminal_DetectsSDKMessage pins the string-match
// assumption against future SDK changes. The SDK's errClientTerminal
// sentinel (ws/error.go:12) is unexported, so we can't errors.Is
// against it; we match by exact-message. Any SDK release that
// changes this message will surface as a test failure.
func TestIsStrandedTerminal_DetectsSDKMessage(t *testing.T) {
	err := errors.New(sdkErrClientTerminalMessage)
	if !isStrandedTerminal(err) {
		t.Fatal("expected exact-match to catch SDK errClientTerminal sentinel")
	}
}

func TestIsStrandedTerminal_FalseOnDifferentMessages(t *testing.T) {
	if isStrandedTerminal(nil) {
		t.Fatal("expected nil to be non-stranded")
	}
	if isStrandedTerminal(errors.New("connection reset")) {
		t.Fatal("expected different message to be non-stranded")
	}
	if isStrandedTerminal(&larkws.ClientError{Code: 1000040350}) {
		t.Fatal("expected *ws.ClientError to NOT be classified as stranded (use isClientError for that)")
	}
}

// TestIsClientError_PinsPriorityOrder verifies the two classifiers
// stay distinct: *ws.ClientError bumps consecutiveFails, stranded
// sentinel does NOT. Conflating would let every prober iteration
// (30s) burn the max-streak limit on a credential-misconfigured
// bot that triggers ExceedConnLimit on every dial.
func TestIsClientError_TrueOnClientError(t *testing.T) {
	if !isClientError(&larkws.ClientError{Code: 1000040350}) {
		t.Fatal("expected *ws.ClientError to be classified as client error")
	}
	if !isClientError(fmt.Errorf("wrapping: %w", &larkws.ClientError{Code: 403})) {
		t.Fatal("expected wrapped *ws.ClientError to be classified as client error")
	}
	if isClientError(errors.New(sdkErrClientTerminalMessage)) {
		t.Fatal("expected errClientTerminal message NOT to be a client error")
	}
	if isClientError(nil) {
		t.Fatal("expected nil to be non-client-error")
	}
}

// TestRecordLastStartErr_StrandedDoesntBumpStreak pins that the
// cancel-induced terminal path (errClientTerminal) only marks the
// stranded flag + bumps strandedStreak, NOT consecutiveFails.
// Without this invariant, the prober's 30s cadence would trip the
// sdkMaxConsecutiveRebuilds limit on a real *ClientError case (the
// very first cycle produces both errors in succession), and the rebuild
// loop would give up too early.
func TestRecordLastStartErr_StrandedDoesntBumpStreak(t *testing.T) {
	a := newTestAdapterForRebuild()

	// First record a *ClientError to bump consecutiveFails.
	a.recordLastStartErr(&larkws.ClientError{Code: 1000040350})
	if got := a.rebuild.consecutiveFails.Load(); got != 1 {
		t.Fatalf("expected consecutiveFails=1 after *ClientError, got %d", got)
	}

	// Now record the stranded sentinel that the same rebuilt client
	// would return on its next Start.
	a.recordLastStartErr(errors.New(sdkErrClientTerminalMessage))

	if got := a.rebuild.consecutiveFails.Load(); got != 1 {
		t.Errorf("expected consecutiveFails still 1 after stranded (not bumped), got %d", got)
	}
	if !a.rebuild.cancelStrandedTerminal.Load() {
		t.Errorf("expected cancelStrandedTerminal=true after stranded recording")
	}
	if got := a.rebuild.strandedStreak.Load(); got != 1 {
		t.Errorf("expected strandedStreak=1, got %d", got)
	}
}

// TestMaybeRebuildClient_StrandedTriggersRebuild pins that the
// cancel-induced terminal path (no *ClientError in lastStartErr,
// only the cancelStrandedTerminal flag) still engages the rebuild.
// This is the path that breaks the WS when a prober tick cancels a
// still-blocked Start: no *ClientError ever appears in lastStartErr
// (recordLastStartErr filters the Canceled return), only the
// subsequent errClientTerminal on the next Start. Without the
// stranded flag, maybeRebuildClient would never rebuild for this
// case.
func TestMaybeRebuildClient_StrandedTriggersRebuild(t *testing.T) {
	a := &Adapter{
		cfg: &config.Config{
			Feishu: config.FeishuConfig{
				AppID:     "test_app",
				AppSecret: "test_secret",
			},
		},
		rebuild: newRebuildState(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		health:  &WSHealth{},
	}

	oldClient := a.buildWSClient(nil)
	if oldClient == nil {
		t.Fatal("buildWSClient returned nil")
	}
	a.client = oldClient
	a.wsStart = oldClient.Start
	a.wsClose = oldClient.Close

	// Simulate the cancel-induced stranded state: no *ClientError,
	// just the stranded flag from a prior errClientTerminal wait.
	a.rebuild.cancelStrandedTerminal.Store(true)

	// Cooldown elapsed.
	stale := time.Now().Add(-5 * time.Minute)
	a.rebuild.lastAt.Store(&stale)

	a.maybeRebuildClient()

	a.mu.RLock()
	newClient := a.client
	count := a.rebuild.count.Load()
	strandedAfter := a.rebuild.cancelStrandedTerminal.Load()
	a.mu.RUnlock()

	if newClient == oldClient {
		t.Fatalf("expected a.client swapped (stranded-triggered rebuild), got same pointer %p", newClient)
	}
	if count != 1 {
		t.Errorf("expected rebuild_count=1, got %d", count)
	}
	if strandedAfter {
		t.Errorf("expected cancelStrandedTerminal cleared after successful rebuild")
	}
}
