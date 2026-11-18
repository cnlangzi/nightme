package chatsession

import "testing"

// TestSetSelectedAgentTransient_DoesNotWriteTheStore — the transient
// switch lives in this process: the chat store keeps the agent the
// operator picked.
func TestSetSelectedAgentTransient_DoesNotWriteTheStore(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	// The chat store knows the chat: the operator picked claude.
	if err := cs.SetSelectedCwd(testCwdBailing); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	if err := cs.SetSelectedAgentTransient("omp"); err != nil {
		t.Fatalf("SetSelectedAgentTransient: %v", err)
	}
	if got := cs.SelectedAgent(); got != "omp" {
		t.Errorf("SelectedAgent = %q, want omp", got)
	}
	entry, ok := csFile.Get("oc_xxx")
	if !ok {
		t.Fatal("the chat store has no entry")
	}
	if entry.SelectedAgent != "claude" {
		t.Errorf("the chat store holds %q, want the agent the operator picked", entry.SelectedAgent)
	}
}

// TestSetSelectedAgentTransient_DetachesTheAgentSession — the next
// message mounts the session of the new agent, so the session of the
// old one is detached.
func TestSetSelectedAgentTransient_DetachesTheAgentSession(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs.SetSelectedCwd(testCwdBailing)
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}
	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	if cs.selectedAS == nil {
		t.Fatal("no agent session is mounted")
	}

	if err := cs.SetSelectedAgentTransient("omp"); err != nil {
		t.Fatalf("SetSelectedAgentTransient: %v", err)
	}
	if cs.selectedAS != nil {
		t.Error("the session of the old agent is still mounted")
	}
	if as.Agent != "claude" {
		t.Errorf("the session works with %q, want claude", as.Agent)
	}
}

// TestSetSelectedAgentTransient_AnEmptyNameClearsTheSelection — the
// caller that switches back may have found no agent.
func TestSetSelectedAgentTransient_AnEmptyNameClearsTheSelection(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)

	if err := cs.SetSelectedAgentTransient(""); err != nil {
		t.Fatalf("SetSelectedAgentTransient: %v", err)
	}
	if got := cs.SelectedAgent(); got != "" {
		t.Errorf("SelectedAgent = %q, want none", got)
	}
}
