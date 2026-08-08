// F-45 §4.1 — AgentSession metadata API unit tests.
//
// Covers: SetModel idempotency, AccumulateUsage race-free
// accumulation, ResetCumulative clears + dirty flag, PersistIfDirty
// no-op when clean + dirty flag reset, and roundtrip via
// Entry → JSON → FromAgentSessionEntry.
package chatsession

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
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
	// bridges may re-emit EventAgentConnected with blank Model after a
	// child restart and we don't want to wipe the prior capture.
	as.SetModel("")
	if got := as.Model(); got != "opus-4-5" {
		t.Fatalf("Model() after empty SetModel = %q, want %q (must not overwrite)", got, "opus-4-5")
	}

	// Non-empty replacement IS allowed (e.g. /new reset via
	// bridge.New() emitting a fresh EventAgentConnected with a new model).
	as.SetModel("haiku-4-5")
	if got := as.Model(); got != "haiku-4-5" {
		t.Fatalf("Model() after replacement SetModel = %q, want %q", got, "haiku-4-5")
	}
}

func TestAccumulateUsage_RaceFree(t *testing.T) {
	as := newTestAgentSession()

	const goroutines = 50
	const perGoroutine = 1000
	// Per-goroutine contribution: 1 in / 1 out / 1 cache_read +
	// 0.001 cost. After N goroutines: N in / N out / N cache_read /
	// N*0.001 cost.
	const expectedTotal = goroutines * perGoroutine

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				as.AccumulateUsage(&agent.UsageEvent{
					InputTokens:          1,
					OutputTokens:         1,
					CacheReadInputTokens: 1,
					CostUSD:              0.001,
				})
			}
		}()
	}
	wg.Wait()

	got := as.CumulativeUsage()
	if got.InputTokens != expectedTotal {
		t.Fatalf("InputTokens = %d, want %d", got.InputTokens, expectedTotal)
	}
	if got.OutputTokens != expectedTotal {
		t.Fatalf("OutputTokens = %d, want %d", got.OutputTokens, expectedTotal)
	}
	if got.CacheReadInputTokens != expectedTotal {
		t.Fatalf("CacheReadInputTokens = %d, want %d", got.CacheReadInputTokens, expectedTotal)
	}
	wantCost := float64(expectedTotal) * 0.001
	const eps = 1e-6
	if diff := got.CostUSD - wantCost; diff < -eps || diff > eps {
		t.Fatalf("CostUSD = %f, want %f (diff %f)", got.CostUSD, wantCost, diff)
	}
}

func TestAccumulateUsage_NilSafe(t *testing.T) {
	as := newTestAgentSession()
	as.AccumulateUsage(nil) // must be a no-op, not panic
	if got := as.CumulativeUsage(); got.InputTokens != 0 {
		t.Fatalf("nil AccumulateUsage must not change state, got %+v", got)
	}
}

func TestResetCumulative_ClearsAndDirties(t *testing.T) {
	as := newTestAgentSession()
	as.AccumulateUsage(&agent.UsageEvent{InputTokens: 100, CostUSD: 0.5})
	if as.CumulativeUsage().InputTokens != 100 {
		t.Fatalf("precondition: expected InputTokens=100")
	}

	as.ResetCumulative()
	got := as.CumulativeUsage()
	if got.InputTokens != 0 || got.OutputTokens != 0 ||
		got.CacheCreationInputTokens != 0 || got.CacheReadInputTokens != 0 ||
		got.CostUSD != 0 {
		t.Fatalf("ResetCumulative did not zero all fields: %+v", got)
	}

	// dirty flag must be set so PersistIfDirty actually fires.
	called := false
	if err := as.PersistIfDirty(func(e *registry.AgentSessionEntry) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("PersistIfDirty: %v", err)
	}
	if !called {
		t.Fatal("ResetCumulative must mark dirty; PersistIfDirty did not invoke callback")
	}
}

func TestPersistIfDirty_NoOpWhenClean(t *testing.T) {
	as := newTestAgentSession()
	called := false
	if err := as.PersistIfDirty(func(e *registry.AgentSessionEntry) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("PersistIfDirty: %v", err)
	}
	if called {
		t.Fatal("PersistIfDirty invoked callback on clean state")
	}
}

func TestEntry_RoundtripPreserves(t *testing.T) {
	as := newTestAgentSession()
	as.SetModel("opus-4-5")
	as.SetResumeID("resume_xyz")
	as.AccumulateUsage(&agent.UsageEvent{
		InputTokens:              11_700,
		OutputTokens:             1_500,
		CacheCreationInputTokens: 600,
		CacheReadInputTokens:     8_200,
		CostUSD:                  0.087,
	})
	as.pid = 42

	entry := as.Entry()
	if entry.Model != "opus-4-5" {
		t.Fatalf("entry.Model = %q, want opus-4-5", entry.Model)
	}
	if entry.ResumeID != "resume_xyz" {
		t.Fatalf("entry.ResumeID = %q, want resume_xyz", entry.ResumeID)
	}
	if entry.CumulativeUsage == nil {
		t.Fatal("entry.CumulativeUsage is nil; expected non-nil (even if zero)")
	}
	if entry.CumulativeUsage.InputTokens != 11_700 {
		t.Fatalf("entry.CumulativeUsage.InputTokens = %d, want 11700", entry.CumulativeUsage.InputTokens)
	}

	// JSON marshal / unmarshal — exercises omitempty + zero-value
	// handling. CumulativeUsage is a non-omitempty *UsageInfo
	// pointer, so legacy files without the field decode to nil
	// and FromAgentSessionEntry treats nil as "never ran".
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
	got := restored.CumulativeUsage()
	if got.InputTokens != 11_700 || got.CostUSD != 0.087 {
		t.Fatalf("restored cumulative = %+v, want InputTokens=11700 CostUSD=0.087", got)
	}
}

func TestEntry_LegacyFileWithMissingCumulativeUsage(t *testing.T) {
	// Old agent_sessions.json (pre-F-45) lacks cumulativeUsage /
	// model. Go JSON unmarshal tolerates missing fields — verify
	// the resulting AgentSession starts at zero (not nil panic).
	legacy := []byte(`{
		"id": "as_legacy",
		"chatSessionId": "cs_legacy",
		"agent": "claude",
		"cwd": "/code/A",
		"pid": 0,
		"status": "detached",
		"resumeId": "old_id",
		"createdAt": "2026-01-01T00:00:00Z",
		"lastRunAt": "2026-01-01T00:00:00Z"
	}`)
	var entry registry.AgentSessionEntry
	if err := json.Unmarshal(legacy, &entry); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	as := FromAgentSessionEntry(&entry)
	if as.Model() != "" {
		t.Fatalf("legacy Model() should be empty, got %q", as.Model())
	}
	got := as.CumulativeUsage()
	if got.InputTokens != 0 || got.CostUSD != 0 {
		t.Fatalf("legacy CumulativeUsage should be zero, got %+v", got)
	}
	// AccumulateUsage still works on a legacy-restored session.
	as.AccumulateUsage(&agent.UsageEvent{InputTokens: 42})
	if as.CumulativeUsage().InputTokens != 42 {
		t.Fatal("AccumulateUsage after legacy restore did not work")
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
