// Package chatsession — extracted test helpers (Phase 1 rename protection).
//
// These helpers were originally in chatsession_test.go, which was
// renamed to .skip for Phase 1 refactor cleanup. The helpers are
// still needed by other test files (init_primary_agent_test.go,
// manager_test.go, message_state_test.go), so extracted here.
package chatsession

import (
	"github.com/cnlangzi/nightme/internal/chatstore"
	"testing"

	"github.com/cnlangzi/nightme/internal/registry"
)

// newTestStores returns a ChatSessionFile + AgentSessionFile pair
// rooted in t.TempDir() — auto-cleaned at test exit.
func newTestStores(t *testing.T) (*chatstore.Store, *registry.AgentSessionFile) {
	t.Helper()
	dir := t.TempDir()
	csFile, err := chatstore.New(dir + "/chat_sessions.json")
	if err != nil {
		t.Fatalf("OpenChatSessionFile: %v", err)
	}
	asFile, err := registry.OpenAgentSessionFile(dir + "/agent_sessions.json")
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}
	return csFile, asFile
}
