package runtime

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// TestProcScope_Cancel_MarksChatsShuttingDown — the process scope is
// what stops the agent children, so cancelling it has to mark the
// chats as shutting down first: the exit events that follow must be
// attributed to the shutdown instead of being reported as crashes.
func TestProcScope_Cancel_MarksChatsShuttingDown(t *testing.T) {
	mgr := chatsession.NewManager()
	cs, err := mgr.GetOrCreate("oc_scope", "claude")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	registerMgrInAllMgrs(mgr)
	t.Cleanup(func() { unregisterMgrFromAllMgrs(mgr) })

	scope, cancel := newProcScope()
	if cs.IsShuttingDown() {
		t.Fatal("the chat was marked as shutting down before the scope was cancelled")
	}

	cancel()

	if !cs.IsShuttingDown() {
		t.Error("the chat was not marked as shutting down after the scope was cancelled")
	}
	if scope.Err() == nil {
		t.Error("the process scope was not cancelled")
	}
}
