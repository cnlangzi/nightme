package runtime

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// TestChatAgentSwitcher_PutsThePreviousAgentBack — the switch lasts
// as long as the attempt: the chat's own agent comes back.
func TestChatAgentSwitcher_PutsThePreviousAgentBack(t *testing.T) {
	mgr := chatsession.NewManager().WithPrimaryAgent("claude")
	registerMgrInAllMgrs(mgr)
	t.Cleanup(func() { unregisterMgrFromAllMgrs(mgr) })
	cs, _ := mgr.GetOrCreate("oc_switch", "claude")

	restore, err := chatAgentSwitcher{}.SwitchAgent("oc_switch", "omp")
	if err != nil {
		t.Fatalf("SwitchAgent: %v", err)
	}
	if got := cs.SelectedAgent(); got != "omp" {
		t.Errorf("the chat works with %q, want omp", got)
	}

	restore()
	if got := cs.SelectedAgent(); got != "claude" {
		t.Errorf("the chat works with %q, want the agent it had before", got)
	}
}

// TestChatAgentSwitcher_TheOperatorChoiceStands — an operator who
// switched the chat's agent while the attempt ran keeps their choice,
// even when it is the agent the attempt works with.
func TestChatAgentSwitcher_TheOperatorChoiceStands(t *testing.T) {
	for _, agent := range []string{"codex", "omp"} {
		t.Run(agent, func(t *testing.T) {
			mgr := chatsession.NewManager().WithPrimaryAgent("claude")
			registerMgrInAllMgrs(mgr)
			t.Cleanup(func() { unregisterMgrFromAllMgrs(mgr) })
			cs, _ := mgr.GetOrCreate("oc_switch", "claude")

			restore, err := chatAgentSwitcher{}.SwitchAgent("oc_switch", "omp")
			if err != nil {
				t.Fatalf("SwitchAgent: %v", err)
			}
			// The operator takes the chat back.
			if err := cs.SetSelectedAgent(agent); err != nil {
				t.Fatalf("SetSelectedAgent: %v", err)
			}

			restore()
			if got := cs.SelectedAgent(); got != agent {
				t.Errorf("the chat works with %q, want the agent the operator picked", got)
			}
		})
	}
}

// TestChatAgentSwitcher_AnUnknownChatIsRefused — a chat that has no
// session cannot work with another agent.
func TestChatAgentSwitcher_AnUnknownChatIsRefused(t *testing.T) {
	if _, err := (chatAgentSwitcher{}).SwitchAgent("oc_nobody", "omp"); err == nil {
		t.Fatal("the switch of an unknown chat was accepted")
	}
}
