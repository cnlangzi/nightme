package chatsession

import (
	"strings"
	"testing"
)

// TestLookupActiveAgentSession_ActiveAgentEmpty_UsesDefaultAgent is
// the commit fix-3 regression test: when activeAgent is empty
// (user has /cwd'd but never /use'd), the lookup must fall back
// to defaultAgent for both pool-hit and spawn paths.
//
// Previously (commit 9 etc.) the spawn step used cs.activeAgent
// directly, which was empty → Spawner returned "unknown agent".
// Fix: a pre-step promotes defaultAgent to effectiveAgent when
// activeAgent is empty.
func TestLookupActiveAgentSession_ActiveAgentEmpty_UsesDefaultAgent(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "p2p", "claude").
		WithPersistence(csFile, asFile)

	cs.SetActiveCwd("/code/bailing")
	// Note: no SetActiveAgent — activeAgent is "".
	if cs.ActiveAgent() != "" {
		t.Fatalf("precondition: ActiveAgent should be empty, got %q", cs.ActiveAgent())
	}

	// Lookup should succeed (uses defaultAgent fallback).
	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
	}
	if as.Agent != "claude" {
		t.Fatalf("Agent: got %q, want claude (defaultAgent fallback)", as.Agent)
	}
}

// TestLookupActiveAgentSession_BothEmpty_ErrNoActiveAgent covers the
// edge case where neither activeAgent nor defaultAgent is set
// (e.g., runtime passed "" as primary in cfg.Primary). The lookup
// must fail with ErrNoActiveAgent, not panic.
func TestLookupActiveAgentSession_BothEmpty_ErrNoActiveAgent(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "p2p", ""). // empty defaultAgent
		WithPersistence(csFile, asFile)

	cs.SetActiveCwd("/code/bailing")
	// activeAgent stays empty; defaultAgent is "".

	_, err := cs.LookupActiveAgentSession()
	if err == nil {
		t.Fatalf("expected error when both activeAgent and defaultAgent are empty")
	}
	if !strings.Contains(err.Error(), "no activeAgent") {
		t.Fatalf("error message should mention 'no activeAgent', got: %v", err)
	}
}

// TestLookupActiveAgentSession_ActiveAgentSet_PrefersActiveAgent
// covers the invariant that /use takes priority over defaultAgent
// when looking up the pool entry. If /use codex left
// (codex, cwd) in pool, LookupActiveAgentSession returns that —
// NOT the (claude, cwd) entry that might still exist.
func TestLookupActiveAgentSession_ActiveAgentSet_PrefersActiveAgent(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "p2p", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetActiveCwd("/code/bailing")
	cs.SetActiveAgent("claude")
	claudeAS, _ := cs.LookupActiveAgentSession() // spawns (claude, /code/bailing)

	// Now /use codex — activeAgent = codex, defaultAgent = claude.
	cs.SetActiveAgent("codex")
	codexAS, _ := cs.LookupActiveAgentSession() // spawns (codex, /code/bailing)

	if claudeAS.Agent != "claude" {
		t.Fatalf("claudeAS.Agent = %q, want claude", claudeAS.Agent)
	}
	if codexAS.Agent != "codex" {
		t.Fatalf("codexAS.Agent = %q, want codex", codexAS.Agent)
	}
	if codexAS.ID == claudeAS.ID {
		t.Fatalf("expected different AS for claude vs codex, both got %s", codexAS.ID)
	}
}