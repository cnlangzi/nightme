// Package chatsession — extracted test helpers (Phase 1 rename protection).
//
// These helpers were originally in chatsession_test.go, which was
// renamed to .skip for Phase 1 refactor cleanup. The helpers are
// still needed by other test files (init_primary_agent_test.go,
// manager_test.go, message_state_test.go), so extracted here.
package chatsession

import (
	"os"
	"testing"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/chatstore"
	"github.com/cnlangzi/nightme/internal/registry"
)

// TestMain registers the chat-id namespace prefixes the chatsession
// tests rely on (e.g. "oc_" for feishu) so chatstore.New can
// validate loaded chat_sessions.json entries.
//
// Production code wires these prefixes via each adapter's init()
// (channel/feishu, channel/telegram, …). The chatsession test
// package CANNOT side-effect-import channel/feishu because
// channel/feishu imports chatsession — that would be an import
// cycle (chatsession_test → feishu → chatsession). Registering
// the prefix directly bypasses the cycle: chatstore only reads
// the registry for the prefix string, not the builder, and the
// tests never call BuildAll, so a nil builder is harmless.
func TestMain(m *testing.M) {
	channel.Reset()
	// nil Builder is safe here: Register only stores it, and
	// chatsession tests never call BuildAll.
	channel.Register("feishu", "oc_", nil)
	channel.Register("telegram", "tg_", nil)
	os.Exit(m.Run())
}

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
