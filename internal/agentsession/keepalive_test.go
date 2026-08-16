// Tests for the bridge.Keepalive / driver.Keepalive cohesive
// detection-and-recover path that lives on AgentSession.Submit.
//
// Why these exist: when a bridge's underlying transport dies
// without its own process-exit handler firing (SIGKILL, OOM,
// shared-host WS severed, transport nil'd by Close), the
// in-memory handle stays non-nil. Without the backstop, the
// next Submit silently writes to a dead stdin pipe and either
// hangs forever or returns a confusing driver-level error.
// With Keepalive, the bridge encapsulates detection + recovery:
// subprocess bridges check the OS PID, shared-host bridges
// (dsh) inspect the WS host connection, and the recovery
// callback (respawnFromDeadHandle, owned by AgentSession)
// rebuilds the bridge via the Spawner.
package agentsession

import (
	"errors"
	"os"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestSubmit_KeepalivePassesThrough: when the bridge's
// Keepalive reports alive, Submit proceeds straight to
// SendBlocks — no recovery, no state mutation.
func TestSubmit_KeepalivePassesThrough(t *testing.T) {
	as, _, driver := makeKeepaliveTestAS(t, os.Getpid())

	if err := as.Submit(&Prompt{
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "ping"}},
	}); err != nil {
		t.Fatalf("Submit on alive PID: %v", err)
	}
	if driver.sent != 1 {
		t.Errorf("driver.SendBlocks called %d times, want 1 (alive path)", driver.sent)
	}
}

// TestSubmit_KeepaliveRecoversViaCallback: when Keepalive
// detects dead state, the recovery callback is invoked, the
// bridge handle is rebuilt, and SendBlocks runs on the fresh
// transport. Submit returns nil — the user sees a normal
// "your prompt was sent" path.
func TestSubmit_KeepaliveRecoversViaCallback(t *testing.T) {
	as, _, driver := makeKeepaliveTestAS(t, 999999999)

	if err := as.Submit(&Prompt{
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "ping"}},
	}); err != nil {
		t.Fatalf("Submit after dead PID (Keepalive + onRecover): %v", err)
	}
	if driver.sent != 1 {
		t.Errorf("driver.SendBlocks called %d times, want 1 (recovery replaced dead handle with live one)", driver.sent)
	}
}

// TestSubmit_KeepaliveFailsWhenNoCallback: if Keepalive fires
// but the caller passed a nil onRecover callback, Submit
// surfaces a wrapped error rather than silently writing to the
// dead handle.
func TestSubmit_KeepaliveFailsWhenNoCallback(t *testing.T) {
	as, _, _ := makeKeepaliveTestASNoCallback(t, 999999999)

	err := as.Submit(&Prompt{
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "ping"}},
	})
	if err == nil {
		t.Fatalf("Submit with nil callback + dead PID returned nil; want error")
	}
	// Error should mention keepalive so operators can localize it.
	if !errors.Is(err, errKeepaliveFailedForTest) {
		t.Errorf("err = %v, want it to wrap a keepalive marker", err)
	}
}

// TestSubmit_KeepaliveFailsWhenCallbackFails: if the recovery
// callback (respawnFromDeadHandle) returns an error, Submit
// surfaces it so the user can intervene (/new, /use, manual
// restart) instead of looping forever.
func TestSubmit_KeepaliveFailsWhenCallbackFails(t *testing.T) {
	as, _, _ := makeKeepaliveTestASFailingRecover(t, 999999999)

	err := as.Submit(&Prompt{
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "ping"}},
	})
	if err == nil {
		t.Fatalf("Submit with failing recovery returned nil; want error")
	}
	if !errors.Is(err, errRecoverFailedForTest) {
		t.Errorf("err = %v, want it to wrap the recovery failure", err)
	}
}

// Sentinel used by the tests above to assert error wrapping
// without depending on the internal fmt.Errorf message text.
var (
	errKeepaliveFailedForTest = errors.New("test: keepalive failed")
	errRecoverFailedForTest   = errors.New("test: recover failed")
)