package chatsession

import (
	"strings"
	"testing"
)

// TestNew_PrimaryAgentSeedsActiveAgent documents the v1.2-final
// init contract: ChatSession constructed with primaryAgent=claude
// has activeAgent=claude from the start (no runtime promotion
// needed). LookupActiveAgentSession uses this seeded value
// directly, with no fallback path.
func TestNew_PrimaryAgentSeedsActiveAgent(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude", newTestChannel()).
		WithPersistence(csFile, asFile)

	if cs.ActiveAgent() != "claude" {
		t.Fatalf("ActiveAgent: got %q, want claude (seeded from primary)", cs.ActiveAgent())
	}
	if cs.PrimaryAgent() != "claude" {
		t.Fatalf("PrimaryAgent: got %q, want claude (snapshot)", cs.PrimaryAgent())
	}
}

// TestNew_EmptyPrimaryAgent_ActiveAgentEmpty covers the misconfigured
// case: cfg.Primary snapshot was empty at construction. Both
// primaryAgent and activeAgent stay empty, and any subsequent
// LookupActiveAgentSession fails with ErrNoActiveAgent.
func TestNew_EmptyPrimaryAgent_ActiveAgentEmpty(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "", newTestChannel()).
		WithPersistence(csFile, asFile)

	if cs.ActiveAgent() != "" {
		t.Fatalf("ActiveAgent: got %q, want empty (no primary)", cs.ActiveAgent())
	}

	cs.SetActiveCwd("/code/bailing")
	_, err := cs.LookupActiveAgentSession()
	if err == nil {
		t.Fatalf("expected error when activeAgent is empty")
	}
	if !strings.Contains(err.Error(), "activeAgent") {
		t.Fatalf("error message should mention 'activeAgent', got: %v", err)
	}
}

// TestLookupActiveAgentSession_UseOverridesPrimary covers the
// invariant that /use takes priority over the primary-agent seed:
// after /use codex, the lookup resolves (codex, cwd) regardless of
// what cfg.Primary was at ChatSession construction.
func TestLookupActiveAgentSession_UseOverridesPrimary(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude", newTestChannel()).
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetActiveCwd("/code/bailing")
	// First lookup uses the seeded primary.
	claudeAS, _ := cs.LookupActiveAgentSession() // spawns (claude, /code/bailing)
	if claudeAS.Agent != "claude" {
		t.Fatalf("claudeAS.Agent = %q, want claude (from primary seed)", claudeAS.Agent)
	}

	// /use codex overrides the seeded activeAgent; pool miss for
	// (codex, /code/bailing) → spawn.
	cs.SetActiveAgent("codex")
	codexAS, _ := cs.LookupActiveAgentSession() // spawns (codex, /code/bailing)

	if codexAS.Agent != "codex" {
		t.Fatalf("codexAS.Agent = %q, want codex", codexAS.Agent)
	}
	if codexAS.ID == claudeAS.ID {
		t.Fatalf("expected different AS for claude vs codex, both got %s", codexAS.ID)
	}
}