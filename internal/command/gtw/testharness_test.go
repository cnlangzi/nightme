// Test-only shared helpers for the gtw package. Exposes
// recordingCh (a messages.Emitter mock that records every
// Send call) and pathsEqual (symlink-safe
// path comparison for macOS test fixtures). Both used to be
// duplicated across close_test.go, close_integration_test.go,
// fix_remote_integration_test.go, force_test.go,
// preflight_test.go. Centralised here so every gtw test file
// picks up the same definition.
package gtw

import (
	"context"
	"github.com/cnlangzi/nightme/internal/messages"
	"path/filepath"
	"sync"
)

// recordingCh captures every Send call's
// payload for assertion. Used by integration tests after the
// cs.Emitter() migration; previous deps.Send mock is no longer
// the actual path. Field-by-field copy of OutboundMessage.
type recordingCh struct {
	mu    sync.Mutex
	sends []messages.OutboundMessage
}

func (r *recordingCh) Send(_ context.Context, m messages.OutboundMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, m)
	return nil
}

// lastText returns the most recent captured message's Text field,
// or "" if no captures. Tests inspect a single response after
// the dispatcher returns; this helper covers that case.
func (r *recordingCh) lastText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sends) == 0 {
		return ""
	}
	return r.sends[len(r.sends)-1].Text
}

// count returns how many messages the emitter received. Tests that
// assert a command stayed silent use it to prove nothing was sent.
func (r *recordingCh) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sends)
}

// pathsEqual compares two filesystem paths. On macOS, t.TempDir()
// returns the realpath (e.g. /var/folders/...) but os.Getwd() and
// chat session SetActiveCwd record the symlink form
// (e.g. /private/var/folders/...). Without canonicalization, the
// same logical directory compares as different. Use
// filepath.EvalSymlinks to canonicalize both before comparing;
// on Linux EvalSymlinks is a no-op for non-symlinked paths.
func pathsEqual(a, b string) bool {
	if a == b {
		return true
	}
	ca, errA := filepath.EvalSymlinks(a)
	cb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return ca == cb
}

// gitSubcommand returns the git subcommand of an argv, skipping a
// leading `--no-optional-locks` flag. Fakes key on the subcommand
// ("status", "add", "commit") rather than on the raw first token,
// so they keep matching when a call site adds the flag.
func gitSubcommand(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if args[0] == "--no-optional-locks" {
		if len(args) < 2 {
			return ""
		}
		return args[1]
	}
	return args[0]
}
