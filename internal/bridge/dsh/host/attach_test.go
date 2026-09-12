// attach_test.go — verifies tryAttachExistingDSH reuses a running
// dsh instead of spawning a fresh one when the cookie validates.
//
// Test layout:
//   - Spawn a real dsh on 3080 in TestMain (gated on env var)
//   - mintDSHAuthCookieFromCredentials signs the same secret the
//     dsh loaded — so the cookie validates against any dsh on
//     this user account
//   - Call tryAttachExistingDSH, assert attached=true and exactly
//     one dsh is running on this host (the pre-existing one — no
//     spawn happened)
//
// Test is gated on NIGHTME_TEST_DSH_AUTH_URL (same env as the
// auth-mint live test). Without dsh running, it skips — the byte-
// for-byte cookie verification (auth_mint_test.go) is the offline
// proof of correctness, this test is just the integration glue.

package host

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"log/slog"
)

const attachTestURLEnv = "NIGHTME_TEST_DSH_AUTH_URL"

// TestTryAttachExistingDSH_ReusesRunningDSH confirms that when dsh
// is already listening on 3080, StartSharedHost's attach probe
// detects it via the minted cookie and skips spawning a fresh dsh.
func TestTryAttachExistingDSH_ReusesRunningDSH(t *testing.T) {
	rawURL := os.Getenv(attachTestURLEnv)
	if rawURL == "" {
		t.Skipf("%s not set; skipping attach integration test", attachTestURLEnv)
	}
	baseURL, _ := splitDSHURL(t, rawURL)
	authority := strings.TrimPrefix(baseURL, "http://")
	_ = authority

	// Sanity: confirm the dsh on 3080 is reachable AND accepts our
	// minted cookie. We need this baseline before tryAttachExistingDSH
	// can succeed.
	jar, err := mintDSHAuthCookieFromCredentials(authority)
	if err != nil {
		t.Fatalf("mintDSHAuthCookieFromCredentials: %v", err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	// session.list is a typed POST (args._request); see
	// @deepseek-ai/dsh-api-session-controller/lib/typert.host.js.
	body := []byte(`{"type":"client-request","rpcId":"probe","method":"session/list","payload":{"args":{"_request":{}}}}`)
	req, _ := http.NewRequest("POST", baseURL+"/api/session/list", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/session/list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("/api/session/list returned %d (want 200): %s", resp.StatusCode, respBody)
	}

	// Now call the production attach path and verify it succeeds
	// without spawning a new dsh.
	dshBefore := pgrepDSH(t)
	t.Logf("dsh processes before attach: %d", dshBefore)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, attached := tryAttachExistingDSH(ctx, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if !attached {
		t.Fatal("tryAttachExistingDSH returned attached=false; want true (cookie validated against running dsh)")
	}
	defer a.host.cli.Close()

	dshAfter := pgrepDSH(t)
	t.Logf("dsh processes after attach: %d", dshAfter)
	if dshAfter != dshBefore {
		t.Errorf("dsh process count changed: before=%d after=%d (expected no spawn)", dshBefore, dshAfter)
	}
	if a.port != 3080 {
		t.Errorf("attached port = %d, want 3080", a.port)
	}
}

// pgrepDSH returns the number of running dsh --profile web
// processes owned by the current user. Used to verify that
// tryAttachExistingDSH did not spawn a new one.
func pgrepDSH(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", "dsh --profile web").Output()
	if err != nil {
		// pgrep returns 1 when no match — treat as 0.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return 0
		}
		t.Fatalf("pgrep: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return 0
	}
	return len(lines)
}
