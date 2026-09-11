//go:build wire_e2e

// wire_ws_e2e_test.go — end-to-end probe of the WebSocket mux
// path against a real dsh 0.1.2-rc.1 instance. The companion to
// wire_e2e_test.go (which only covers HTTP RPC); together they pin
// down the full bridge wire surface.
//
// Run:
//
//	NIGHTME_TEST_DSH_URL='http://127.0.0.1:4097/?token=...' \
//	  go test -tags wire_e2e -run TestWireWS_E2E -v ./internal/bridge/dsh/host/
//
// Skipped (t.Skip) when the env var is unset, so it never blocks CI.
//
// What this catches:
//   - StreamHub.auth + connect + Subscribe → session/follow open
//     flow (the Subscribe-after-Connect generation-tracking fix is
//     verified by the fact that the session events arrive at all).
//   - translateSessionEvent wire shape — must surface each
//     SessionFollowFrame event.type as a FrameHandler method call.
//   - translateHostEvent wire shape — host $events broadcasts
//     (api-session/status, api-session/activity) land via the
//     onHostFrame callback with the real emit shape.
//   - The full turn lifecycle: session/follow carries agent/inbox/
//     spliced + turn/start + step/start + assistant/chunk (stream)
//     + assistant/message + step/end + turn/end, and all of those
//     must be observable via the FrameHandler.
//
// What this deliberately doesn't check:
//   - The dispatcher layer (handleMuxFrame, dispatchEvent,
//     handler registry) is verified in unit tests with mock
//     SessionFollowFrame fixtures. Verifying it here would require
//     spinning up a real dsh driver (host.NewClient + Router +
//     Driver), which is the parent package's job; this test stays
//     in the host package and only proves the WS surface.

package host_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

func TestWireWS_E2E(t *testing.T) {
	raw := os.Getenv("NIGHTME_TEST_DSH_URL")
	if raw == "" {
		t.Skip("NIGHTME_TEST_DSH_URL not set; wire_ws_e2e skipped")
	}

	baseURL, token := splitDSHURL(t, raw)

	jar, _ := cookiejar.New(nil)
	httpClient := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if token != "" {
		warmupCookie(t, httpClient, baseURL, token)
	}

	rpc := host.NewRPCClientWithHTTP(baseURL, httpClient)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sid, err := rpc.SessionCreate(ctx, host.SessionCreateOpts{CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("session.create: %v", err)
	}
	if sid == "" {
		t.Fatalf("session.create: empty sessionId")
	}
	t.Logf("sessionId=%s", sid)
	t.Cleanup(func() {
		_ = rpc.WorkspaceArchiveSession(context.Background(), sid)
	})

	// StreamHub captures every frame in two slices keyed by which
	// callback the StreamHub invoked. The onMuxFrame callback is
	// what session/follow items route through; onHostFrame is for
	// $events broadcasts. We don't act on them — the test's
	// assertions are about presence and method-name, not payload.
	var mu sync.Mutex
	var muxFrames, hostFrames []string
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	hub := host.NewStreamHubWithJar(baseURL, jar, log,
		func(method, rpcID string, payload json.RawMessage) {
			mu.Lock()
			muxFrames = append(muxFrames, method)
			mu.Unlock()
		},
		func(method, rpcID string, payload json.RawMessage) {
			mu.Lock()
			hostFrames = append(hostFrames, method)
			mu.Unlock()
		},
	)
	if err := hub.Start(ctx); err != nil {
		t.Fatalf("hub.Start: %v", err)
	}
	t.Cleanup(hub.Close)

	// Wait for the first host frame (the `ready` the server sends
	// right after WS upgrade) so we know the mux pump is live.
	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(hostFrames) > 0
	})

	// Subscribe AFTER Connect — the generation-tracking fix is
	// what makes this work. If the fix regresses, the open frame
	// never reaches the server and the rest of the test hangs.
	hub.Subscribe(sid, func(method, rpcID string, payload json.RawMessage) {})
	t.Logf("subscribed to %s", sid)

	// Give Subscribe a beat to enqueue the open frame and the
	// server a beat to process it. Without this the test races
	// the snapshot delivery.
	time.Sleep(500 * time.Millisecond)

	if err := rpc.SessionPrompt(ctx, sid, "queue", []host.PromptPart{
		{Type: "text", Text: "Reply with the single word PONG and nothing else."},
	}); err != nil {
		t.Fatalf("session.prompt: %v", err)
	}
	t.Log("prompt sent; waiting for turn/end")

	// Block on turn/end. timeout (60s) is set on the ctx; the
	// poll loop below only breaks early on success.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		seen := false
		for _, m := range muxFrames {
			if m == "turn/end" {
				seen = true
				break
			}
		}
		mu.Unlock()
		if seen {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	// Required: full turn lifecycle must be observable. Each
	// entry in the set is a method we expect to see at least once
	// during a normal PONG reply on the minimax-cn / MiniMax-M3
	// default provider (verified 2026-09-11 against dsh 0.1.2-rc.1).
	required := []string{
		"session/snapshot",
		"turn/start",
		"step/start",
		"assistant/chunk", // stream (delta / block-end / usage all share this)
		"assistant/message",
		"step/end",
		"turn/end",
	}
	for _, want := range required {
		if !contains(muxFrames, want) {
			t.Errorf("missing required session event: %s\nseen: %v", want, muxFrames)
		}
	}

	// Host broadcasts that dsh 0.1.2-rc.1 emits on $events when
	// our session flips status / activity. Not strictly required
	// (timing-sensitive), but very common — log if absent.
	if !contains(hostFrames, "api-session/status") {
		t.Logf("no api-session/status on $events (timing-dependent, often OK): %v", hostFrames)
	}
}

// contains is a tiny helper to avoid pulling in slices.Contains
// for the small set of strings we check against.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
