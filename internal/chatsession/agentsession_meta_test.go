// F-45 §4.1 — AgentSession metadata API unit tests.
//
// Covers: SetModel idempotency, PersistIfDirty passthrough
// (the cumulative-dirty trigger is gone with cross-turn usage
// aggregation), and roundtrip via Entry → JSON →
// FromAgentSessionEntry. The old AccumulateUsage /
// CumulativeUsage / ResetCumulative tests were deleted when
// usage aggregation moved out of AgentSession — usage is now a
// per-turn snapshot that flows straight from bridge → channel
// footer.
package chatsession

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cnlangzi/nightme/internal/registry"
)

func newTestAgentSession() *AgentSession {
	return NewAgentSession("as_test", "cs_test", "claude", "/code/A", nil)
}

func TestSetModel_Idempotent(t *testing.T) {
	as := newTestAgentSession()

	// First set: capture.
	as.SetModel("opus-4-5")
	if got := as.Model(); got != "opus-4-5" {
		t.Fatalf("Model() after first SetModel = %q, want %q", got, "opus-4-5")
	}

	// Empty incoming value must NOT overwrite a captured model —
	// bridges may re-emit EventInit with blank Model after a
	// child restart and we don't want to wipe the prior capture.
	as.SetModel("")
	if got := as.Model(); got != "opus-4-5" {
		t.Fatalf("Model() after empty SetModel = %q, want %q (must not overwrite)", got, "opus-4-5")
	}

	// Non-empty replacement IS allowed (e.g. /new reset via
	// bridge.New() emitting a fresh EventInit with a new model).
	as.SetModel("haiku-4-5")
	if got := as.Model(); got != "haiku-4-5" {
		t.Fatalf("Model() after replacement SetModel = %q, want %q", got, "haiku-4-5")
	}
}

// TestPersistIfDirty_PassesThrough pins the new contract: every
// prior caller relied on the cumulativeDirty trigger (gone with
// cross-turn usage aggregation), so PersistIfDirty is now a
// straightforward pass-through. Future per-AS dirty state can
// re-introduce the dirty flag without changing call sites.
//
// nil persist stays a no-op (defensive: callers occasionally
// hand in nil during test wiring).
func TestPersistIfDirty_PassesThrough(t *testing.T) {
	as := newTestAgentSession()
	called := false
	err := as.PersistIfDirty(func(e *registry.AgentSessionEntry) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("PersistIfDirty: %v", err)
	}
	if !called {
		t.Fatal("PersistIfDirty should invoke the callback (now a pass-through)")
	}

	// nil persist is the only no-op case.
	if err := as.PersistIfDirty(nil); err != nil {
		t.Fatalf("PersistIfDirty(nil) = %v, want nil", err)
	}
}

func TestEntry_RoundtripPreserves(t *testing.T) {
	// The persistence roundtrip covers the per-AgentSession
	// fields that DO get persisted (Model, ResumeID, status,
	// PID). Usage is no longer on the entry — per-turn usage
	// snapshots flow from bridge → out.Usage → channel footer
	// without touching AgentSession state.
	as := newTestAgentSession()
	as.SetModel("opus-4-5")
	as.SetResumeID("resume_xyz")
	as.pid = 42

	entry := as.Entry()
	if entry.Model != "opus-4-5" {
		t.Fatalf("entry.Model = %q, want opus-4-5", entry.Model)
	}
	if entry.ResumeID != "resume_xyz" {
		t.Fatalf("entry.ResumeID = %q, want resume_xyz", entry.ResumeID)
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded registry.AgentSessionEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	restored := FromAgentSessionEntry(&decoded)
	if restored.Model() != "opus-4-5" {
		t.Fatalf("restored Model() = %q, want opus-4-5", restored.Model())
	}
	if restored.ResumeID() != "resume_xyz" {
		t.Fatalf("restored ResumeID() = %q, want resume_xyz", restored.ResumeID())
	}
}

// TestFromAgentSessionEntry_OpContextNotNil is the regression lock
// for a daemon-killing crash.
//
// AgentSessions restored from disk went through FromAgentSessionEntry,
// which — unlike NewAgentSession — never pre-installed opCtx. The
// default FlushHook passes OpContext() straight into
// bridge.SendBlocks, and the pi bridge calls ctx.Deadline() on entry,
// so the FIRST message after any daemon restart panicked the whole
// daemon with a nil-pointer dereference.
func TestFromAgentSessionEntry_OpContextNotNil(t *testing.T) {
	as := FromAgentSessionEntry(&registry.AgentSessionEntry{
		ID:            "as-restored",
		ChatSessionID: "cs-1",
		Agent:         "pi",
		Cwd:           "/tmp/ws",
		Status:        StatusRunning, // demoted to Detached on restore
	})
	if as == nil {
		t.Fatal("FromAgentSessionEntry returned nil")
	}
	if as.OpContext() == nil {
		t.Fatal("OpContext() is nil; the first SendBlocks after a daemon restart would panic")
	}
	// A restored session has no live cancel yet — it must not be
	// mistaken for an activated one, or promoteActiveLocked would
	// skip wiring it to the chat ctx.
	if as.IsActivated() {
		t.Error("IsActivated() = true on a freshly restored session, want false")
	}
}

// TestNewAgentSession_OpContextNotNil pins the same invariant on the
// other constructor so the two cannot drift apart again.
func TestNewAgentSession_OpContextNotNil(t *testing.T) {
	as := NewAgentSession("as-new", "cs-1", "pi", "/tmp/ws", nil)
	if as.OpContext() == nil {
		t.Fatal("OpContext() is nil")
	}
	if as.IsActivated() {
		t.Error("IsActivated() = true before Activate, want false")
	}
	as.Activate(context.Background())
	if !as.IsActivated() {
		t.Error("IsActivated() = false after Activate, want true")
	}
}
