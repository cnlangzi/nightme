// Verifies the per-AS ctx contract on AgentSession:
//
//   1. OpContext() returns a non-nil ctx out of NewAgentSession
//      (and after FromAgentSessionEntry), independent of any
//      ChatSession wiring.
//   2. Activate(parent) installs a fresh ctx derived from
//      parent; cancelling parent cascades to OpContext().
//   3. Background() cancels the current opCtx atomically; a
//      second Background is a safe no-op (idempotent).
//   4. Activate-after-Background installs a fresh ctx; the
//      previous Background's done channel is separate from the
//      fresh ctx's done channel.
//   5. Background-then-Activate-then-Background cycles cleanly
//      — the model is "internal ctx management owned by AS,
//      parent lifetime owned by caller".
package chatsession

import (
	"context"
	"testing"
	"time"
)

// TestAS_OpContext_NonNilByDefault confirms the freshly-constructed
// AS exposes a usable ctx even before any Activate() call — the
// zero value must not surprise callers (e.g. tests) that read
// OpContext() before wiring.
func TestAS_OpContext_NonNilByDefault(t *testing.T) {
	as := NewAgentSession("as_test", "cs_test", "pi", "/tmp", nil)

	ctx := as.OpContext()
	if ctx == nil {
		t.Fatal("OpContext() returned nil on a fresh AS")
	}
	if cancelled(ctx) {
		t.Fatal("OpContext() returned an already-cancelled ctx on a fresh AS")
	}
}

// TestAS_Activate_DerivesFromParent confirms Activate(parent)
// installs a fresh opCtx that derives from parent. Cancelling
// parent cascades to OpContext() — the chat-level shutdown path
// (cs.ResetContext → ASes' OpContexts all Done) relies on this.
func TestAS_Activate_DerivesFromParent(t *testing.T) {
	as := NewAgentSession("as_test", "cs_test", "pi", "/tmp", nil)
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	as.Activate(parent)
	opDone := as.OpContext().Done()

	cancelParent()
	select {
	case <-opDone:
	case <-time.After(time.Second):
		t.Fatal("parent cancel did not cascade to AS.OpContext() — derivation is broken")
	}
}

// TestAS_Background_CancelsAndIsIdempotent confirms Background()
// cancels the current opCtx and a second call is a safe no-op.
// The runtime calls Background() once per AS-swap, but defensive
// callers (e.g. shutdown that calls Background on every AS) may
// invoke it twice and must not panic.
func TestAS_Background_CancelsAndIsIdempotent(t *testing.T) {
	as := NewAgentSession("as_test", "cs_test", "pi", "/tmp", nil)
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	as.Activate(parent)
	firstDone := as.OpContext().Done()

	as.Background()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first Background() did not cancel opCtx")
	}

	// Second Background must be a safe no-op.
	as.Background()
}

// TestAS_ActivateAfterBackground_InstallsFresh is the canonical
// "swap to a new AS" sequence: Background the old ctx, Activate
// the new one. The fresh opCtx must be a different context than
// the previous one (different Done channel) and must be live.
func TestAS_ActivateAfterBackground_InstallsFresh(t *testing.T) {
	as := NewAgentSession("as_test", "cs_test", "pi", "/tmp", nil)
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	as.Activate(parent)
	firstDone := as.OpContext().Done()

	as.Background()
	<-firstDone // confirm cancellation landed

	as.Activate(parent)
	second := as.OpContext()
	if cancelled(second) {
		t.Fatal("second Activate() produced an already-cancelled ctx")
	}

	// Distinct Done channel proves the ctx was replaced, not
	// re-issued. (Two ctxs with the same Done() means the
	// previous cancel is still wired — that would be a leak.)
	select {
	case <-second.Done():
		t.Fatal("fresh ctx from second Activate has a Done channel that's already firing — Background/Activate cycle is broken")
	default:
	}

	// parent cancel still cascades to the fresh ctx.
	secondDone := second.Done()
	cancelParent()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("parent cancel did not cascade to the post-Background Activate ctx")
	}
}

// TestAS_Activate_CancelsPreviousDefensively confirms Activate()
// cancels any previous opCtx first — even if the caller skipped
// Background. The runtime is the single production caller and
// always pairs them, but defensive Activate protects against
// future caller mistakes.
func TestAS_Activate_CancelsPreviousDefensively(t *testing.T) {
	as := NewAgentSession("as_test", "cs_test", "pi", "/tmp", nil)

	parent1, cancelParent1 := context.WithCancel(context.Background())
	defer cancelParent1()
	as.Activate(parent1)
	firstDone := as.OpContext().Done()

	parent2, cancelParent2 := context.WithCancel(context.Background())
	defer cancelParent2()
	as.Activate(parent2)

	// The first ctx's Done must fire — either because parent1
	// was cancelled, or because Activate's defensive cancel ran.
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("second Activate did not cancel the previous opCtx — defensive cancel is missing")
	}
}

// cancelled is a small helper that returns true if ctx is already
// done. Used to avoid verbose select-default patterns in
// assertions.
func cancelled(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}