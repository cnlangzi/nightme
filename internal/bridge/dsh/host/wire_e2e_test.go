//go:build wire_e2e

// wire_e2e_test.go — end-to-end probe of every RPC method the
// bridge uses, against a real dsh 0.1.2-rc.1 instance.
//
// Run:
//
//	NIGHTME_TEST_DSH_URL='http://127.0.0.1:4097/?token=...' \
//	  go test -tags wire_e2e -run TestWireE2E -v ./internal/bridge/dsh/host/
//
// Skipped (t.Skip) when the env var is unset, so it never blocks CI.
//
// Purpose: catch wire-shape drift between the bridge code and the
// dsh gateway. Each subtest exercises one RPC method with the
// exact args shape client.go Post() builds and asserts the result
// is reachable. Business-level failures (invalid path, etc.) are
// acceptable as long as the wire is sound.

package host_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

func TestWireE2E(t *testing.T) {
	raw := os.Getenv("NIGHTME_TEST_DSH_URL")
	if raw == "" {
		t.Skip("NIGHTME_TEST_DSH_URL not set; wire_e2e skipped")
	}

	baseURL, token := splitDSHURL(t, raw)

	// Build a cookie-bearing http.Client so the dsh-auth cookie
	// from the dashboard warmup flows through every RPC.
	jar, _ := cookiejar.New(nil)
	httpClient := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
	if token != "" {
		warmupCookie(t, httpClient, baseURL, token)
	}

	c := host.NewRPCClientWithHTTP(baseURL, httpClient)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	wsDir := t.TempDir()
	t.Run("workspace/create", func(t *testing.T) {
		ws, err := c.WorkspaceCreate(ctx, wsDir)
		if err != nil {
			t.Fatalf("workspace/create: %v", err)
		}
		if ws.WorkspaceID == "" {
			t.Errorf("workspace/create: empty workspaceId")
		}
		t.Logf("workspaceId=%s", ws.WorkspaceID)

		t.Run("session/create", func(t *testing.T) {
			sid, err := c.SessionCreate(ctx, host.SessionCreateOpts{
				WorkspaceID: ws.WorkspaceID,
			})
			if err != nil {
				t.Fatalf("session/create: %v", err)
			}
			if sid == "" {
				t.Errorf("session/create: empty sessionId")
			}
			t.Logf("sessionId=%s", sid)

			t.Run("session/prompt", func(t *testing.T) {
				if err := c.SessionPrompt(ctx, sid, "queue",
					[]host.PromptPart{{Type: "text", Text: "say PONG"}}); err != nil {
					t.Fatalf("session/prompt: %v", err)
				}
			})

			t.Run("commands/execute", func(t *testing.T) {
				if err := c.CommandsExecute(ctx, sid, "/permission danger-full-access"); err != nil {
					t.Logf("commands/execute (non-fatal, may already be set): %v", err)
				}
			})

			t.Run("session/cancel", func(t *testing.T) {
				_ = c.SessionCancel(ctx, sid)
			})

			t.Run("workspace/archiveSession", func(t *testing.T) {
				if err := c.WorkspaceArchiveSession(ctx, sid); err != nil {
					t.Logf("workspace/archiveSession: %v", err)
				}
			})
		})

		t.Run("workspace/delete", func(t *testing.T) {
			if err := c.WorkspaceDelete(ctx, ws.WorkspaceID); err != nil {
				t.Logf("workspace/delete: %v", err)
			}
		})
	})

	// session.list uses "_request" wrapper key (NOT "request" —
	// unique among session.* endpoints; verified 2026-09-10).
	t.Run("session/list", func(t *testing.T) {
		items, err := c.SessionList(ctx)
		if err != nil {
			t.Fatalf("session/list: %v", err)
		}
		t.Logf("session.list: %d items", len(items))
	})

	// workspace.list does NOT exist in dsh 0.1.2-rc.1 (the
	// workspace typert namespace has archiveSession / create /
	// delete / follow / insertBefore / insertSessionBefore /
	// rename — no list). Removed from the bridge on 2026-09-10
	// after live-wire verification; if dsh adds it later, the
	// verification fixture (typert.remote-client.d.ts in the
	// dsh-api-workspace-controller package) will surface it.
}

// splitDSHURL splits "http://host:port/?token=ABC" into
// ("http://host:port", "ABC"). Token may be empty if not provided.
func splitDSHURL(t *testing.T, raw string) (string, string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	tok := u.Query().Get("token")
	u.RawQuery = ""
	u.Fragment = ""
	base := u.String()
	base = strings.TrimRight(base, "/")
	return base, tok
}

// warmupCookie hits the dashboard root with the token so the
// dsh-auth cookie is set on the shared cookie jar. dsh 0.1.2-rc.1
// requires this cookie on /api/* (auth middleware runs on every
// request, even for endpoints that look unauthenticated).
func warmupCookie(t *testing.T, c *http.Client, baseURL, token string) {
	t.Helper()
	r, err := http.NewRequest("GET", baseURL+"/?token="+token, nil)
	if err != nil {
		t.Fatalf("warmup: build req: %v", err)
	}
	resp, err := c.Do(r)
	if err != nil {
		t.Fatalf("warmup: do: %v", err)
	}
	defer resp.Body.Close()
}
