package chatsession

import (
	"strings"
	"testing"
)

// TestNew_PrimaryAgentSeedsActiveAgent documents the v1.2-final
// init contract: ChatSession constructed with primaryAgent=claude
// has activeAgent=claude from the start (no runtime promotion
// needed). LookupSelectedAgentSession uses this seeded value
// directly, with no fallback path.
func TestNew_PrimaryAgentSeedsActiveAgent(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)

	if cs.SelectedAgent() != "claude" {
		t.Fatalf("SelectedAgent: got %q, want claude (seeded from primary)", cs.SelectedAgent())
	}
	if cs.PrimaryAgent() != "claude" {
		t.Fatalf("PrimaryAgent: got %q, want claude (snapshot)", cs.PrimaryAgent())
	}
}

// TestNew_EmptyPrimaryAgent_SelectedAgentEmpty covers the misconfigured
// case: cfg.Primary snapshot was empty at construction. Both
// primaryAgent and activeAgent stay empty, and any subsequent
// LookupSelectedAgentSession fails with ErrNoSelectedAgent.
func TestNew_EmptyPrimaryAgent_SelectedAgentEmpty(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "")
	cs = cs.WithPersistence(csFile, asFile)

	if cs.SelectedAgent() != "" {
		t.Fatalf("SelectedAgent: got %q, want empty (no primary)", cs.SelectedAgent())
	}

	cs.SetSelectedCwd("/code/bailing")
	_, err := cs.LookupSelectedAgentSession()
	if err == nil {
		t.Fatalf("expected error when activeAgent is empty")
	}
	if !strings.Contains(err.Error(), "selectedAgent") {
		t.Fatalf("error message should mention 'selectedAgent', got: %v", err)
	}
}

// TestLookupActiveAgentSession_UseOverridesPrimary covers the
// invariant that /use takes priority over the primary-agent seed:
// after /use codex, the lookup resolves (codex, cwd) regardless of
// what cfg.Primary was at ChatSession construction.
func TestLookupActiveAgentSession_UseOverridesPrimary(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetSelectedCwd("/code/bailing")
	// First lookup uses the seeded primary.
	claudeAS, _ := cs.LookupSelectedAgentSession() // spawns (claude, /code/bailing)
	if claudeAS.Agent != "claude" {
		t.Fatalf("claudeAS.Agent = %q, want claude (from primary seed)", claudeAS.Agent)
	}

	// /use codex overrides the seeded activeAgent; pool miss for
	// (codex, /code/bailing) → spawn.
	cs.SetSelectedAgent("codex")
	codexAS, _ := cs.LookupSelectedAgentSession() // spawns (codex, /code/bailing)

	if codexAS.Agent != "codex" {
		t.Fatalf("codexAS.Agent = %q, want codex", codexAS.Agent)
	}
	if codexAS.ID == claudeAS.ID {
		t.Fatalf("expected different AS for claude vs codex, both got %s", codexAS.ID)
	}
}