package codex

import (
	"testing"
	"time"
)

// TestHandshakeTimeout_OutlastsColdStart pins the initialize
// handshake budget above the slowest codex app-server cold start
// observed on this host. A budget at or below the cold start loses
// the session before it exists: the handshake gives up, the thread id
// never arrives, and a fresh session has nothing to resume from.
//
// Bump the constant deliberately — a cold app-server that needs more
// than the budget still fails, but a session that only needed a
// little more room does not.
func TestHandshakeTimeout_OutlastsColdStart(t *testing.T) {
	const coldStart = 88 * time.Second
	if handshakeTimeout <= coldStart {
		t.Fatalf("handshakeTimeout = %s, must exceed the %s cold start — a cold app-server must not lose the session", handshakeTimeout, coldStart)
	}
}
