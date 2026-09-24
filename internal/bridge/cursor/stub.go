// stub.go — cursor's store.db timing workaround.
//
// cursor-agent's session/new returns a sessionId immediately, but
// it does NOT create store.db at that moment — store.db is only
// written after the first session/prompt completes. Until then,
// session/load on that sessionId fails with -32602 "Session ... not
// found", and nightme's persisted id cannot be resumed.
//
// Fix: pre-write a minimal stub SQLite database with cursor's
// schema (blobs, meta) at ~/.cursor/acp-sessions/<id>/store.db
// right after session/new assigns the id. The stub satisfies
// cursor's "file exists" check; the first real session/prompt
// then populates the database normally, and resume works from
// that point on.
//
// The bytes are captured from a real cursor-agent session after
// session/new + first prompt + a checkpoint + WAL checkpointed
// back to the main db (so the file is a self-contained SQLite
// database with the two tables and no user data). See
// internal/bridge/cursor/store_db_stub.bin for the captured bytes.
//
// Schema-evolution concern: if cursor-agent adds required tables
// or changes the schema in a future version, the stub needs to
// be re-captured. The fix is mechanical: run a probe against the
// new cursor-agent, dump the empty-schema db, replace the file.
//
// All other bridges (claudecode / codex / opencode / copilot / pi /
// dsh / pty) don't need this — they have their own session stores,
// not cursor's ~/.cursor/acp-sessions path.
package cursor

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed store_db_stub.bin
var storeDBStub []byte

// ensureStubStoreDB writes the cursor session's store.db as a
// minimal SQLite database so the freshly-assigned sessionId is
// immediately resumable across a daemon restart or `/new`.
//
// Called from cursor's Starter.Start via acp.WithSessionIDHook,
// which acp fires synchronously after session/new returns its id
// and before EventAgentReady is emitted. Failure is non-fatal:
// the bridge logs and continues — session/load will simply fail
// -32602 on the next spawn (same behavior as before this fix).
func ensureStubStoreDB(sessionID string) error {
	if sessionID == "" {
		return errors.New("cursor: ensureStubStoreDB called with empty sessionId")
	}
	if len(storeDBStub) == 0 {
		return errors.New("cursor: store_db_stub.bin is empty (build misconfig)")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cursor: locate home dir: %w", err)
	}
	dir := filepath.Join(home, ".cursor", "acp-sessions", sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cursor: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "store.db")
	if err := os.WriteFile(path, storeDBStub, 0o600); err != nil {
		return fmt.Errorf("cursor: write %s: %w", path, err)
	}
	return nil
}
