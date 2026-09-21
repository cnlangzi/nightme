//go:build live

// zz_live_ready_capture_test.go — live-verifies the dispatch item-path
// capture of the $events ready handshake against a real dsh on
// 127.0.0.1:3080. dsh 0.1.5-rc.1 delivers ready as a host stream item;
// if dispatch fails to capture its clientId, SendPermission can never
// POST /api/$events/result and AskUserQuestion answers are lost.
//
// Run: go test -tags live -run TestLiveReadyCapture -v ./internal/bridge/dsh/host/
// Build-tagged so make test / CI never run it.

package host

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestLiveReadyCapture(t *testing.T) {
	const authority = "127.0.0.1:3080"
	jar, err := mintDSHAuthCookieFromCredentials(authority)
	if err != nil {
		t.Skipf("mint cookie (no ~/.dsh credentials?): %v", err)
	}
	ResetHostClientIDForTest()
	t.Cleanup(ResetHostClientIDForTest)

	cli := NewWithJar("http://"+authority, jar, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("cli.Start (live dsh on %s?): %v", authority, err)
	}
	defer cli.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if id := GetHostClientID(); id != "" {
			t.Logf("captured live ready clientId=%s — dispatch item-path capture works against real dsh", id)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("GetHostClientID empty after 5s — dispatch did not capture the $events ready item from the live dsh")
}
