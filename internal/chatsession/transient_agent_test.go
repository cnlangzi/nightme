package chatsession

import "testing"

// TestSwitchAgentTransient_DoesNotWriteTheStore — the transient
// switch lives in this process: the chat store keeps the agent the
// operator picked.
func TestSwitchAgentTransient_DoesNotWriteTheStore(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	// The chat store knows the chat: the operator picked claude.
	if err := cs.SetSelectedCwd(testCwdBailing); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	restore := cs.SwitchAgentTransient("omp")
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
	restore()
}

// TestSwitchAgentTransient_DetachesTheAgentSession — the next message
// mounts the session of the new agent, so the session of the old one
// is detached.
func TestSwitchAgentTransient_DetachesTheAgentSession(t *testing.T) {
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

	cs.SwitchAgentTransient("omp")
	if cs.selectedAS != nil {
		t.Error("the session of the old agent is still mounted")
	}
	if as.Agent != "claude" {
		t.Errorf("the session works with %q, want claude", as.Agent)
	}
}

// TestSwitchAgentTransient_AnEmptyNameClearsTheSelection — the
// caller that switches back may have found no agent.
func TestSwitchAgentTransient_AnEmptyNameClearsTheSelection(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)

	cs.SwitchAgentTransient("")
	if got := cs.SelectedAgent(); got != "" {
		t.Errorf("SelectedAgent = %q, want none", got)
	}
}

// TestSwitchAgentTransient_TheRestorePutsThePreviousAgentBack — the
// switch lasts as long as the attempt.
func TestSwitchAgentTransient_TheRestorePutsThePreviousAgentBack(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)

	restore := cs.SwitchAgentTransient("omp")
	if got := cs.SelectedAgent(); got != "omp" {
		t.Errorf("SelectedAgent = %q, want omp", got)
	}
	restore()
	if got := cs.SelectedAgent(); got != "claude" {
		t.Errorf("SelectedAgent = %q, want the agent the chat had before", got)
	}
}

// TestSwitchAgentTransient_TheOperatorChoiceStands — an operator who
// chose an agent while the attempt ran keeps their choice, even when
// it is the agent the attempt works with.
func TestSwitchAgentTransient_TheOperatorChoiceStands(t *testing.T) {
	for _, agent := range []string{"codex", "omp"} {
		t.Run(agent, func(t *testing.T) {
			csFile, asFile := newTestStores(t)
			cs, _ := New("oc_xxx", "claude")
			cs = cs.WithPersistence(csFile, asFile)

			restore := cs.SwitchAgentTransient("omp")
			// The operator takes the chat back.
			if err := cs.SetSelectedAgent(agent); err != nil {
				t.Fatalf("SetSelectedAgent: %v", err)
			}
			restore()
			if got := cs.SelectedAgent(); got != agent {
				t.Errorf("SelectedAgent = %q, want the agent the operator chose", got)
			}
		})
	}
}

// TestSwitchAgentTransient_TheOperatorChoiceIsPersisted — the chat
// store holds the agent the chat had before the switch: the operator's
// choice is written even when the chat already works with that agent.
func TestSwitchAgentTransient_TheOperatorChoiceIsPersisted(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	if err := cs.SetSelectedCwd(testCwdBailing); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	// The run holds the chat with the agent the operator wants.
	cs.SwitchAgentTransient("omp")
	if err := cs.SetSelectedAgent("omp"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}
	entry, ok := csFile.Get("oc_xxx")
	if !ok {
		t.Fatal("the chat store has no entry")
	}
	if entry.SelectedAgent != "omp" {
		t.Errorf("the chat store holds %q, want the agent the operator chose", entry.SelectedAgent)
	}
}
