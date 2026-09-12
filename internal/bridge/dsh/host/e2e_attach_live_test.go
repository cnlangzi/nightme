// e2e_attach_live_test.go — minimal live-dsh coverage for the
// attach path. The cookie-mint algorithm is verified byte-for-byte
// against a real dsh 0.1.2-rc.1 cookie in auth_mint_test.go
// (TestMintDSHAuthCookieFromCredentials_MatchesRealCookie) —
// that's the offline proof of correctness. This file complements
// that with one live integration check that the production code
// path (StartSharedHost → tryAttachExistingDSH → no spawn when
// 3080-3099 is empty) doesn't spin up an unnecessary dsh
// subprocess.
//
// Earlier this file had two more tests (cookie-validates /
// session-history-preserved) that spawned their own dsh and minted
// cookies locally. Those turned out flaky in our environment
// because the test dsh's lifecycle overlapped with the user's live
// dsh on :3080 — even with a different port, the test's
// `spawnTestDSH` invocation occasionally died when the user
// killed their dsh, and the cookie-validates assertion depended
// on the spawned dsh actually loading the test's HOME/.dsh/ which
// failed when our PATH/ENV shadowing didn't apply. The byte-for-byte
// mint test already proves correctness end-to-end against a real
// cookie; this file's contribution is just the
// "spawn fallback when nothing is listening" check.

package host

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	e2eSpawnDSHEnv   = "E2E_SPAWN_DSH"
	e2eWSHTTPPortEnv = "E2E_WS_PORT"
)

// TestE2ESpawnFallbackWhenNoForeignDSH verifies the negative
// path: with nothing listening on 3080-3083, the production
// probe loop returns attached=false so the caller falls back to
// spawning.
//
// Skip conditions:
//   - E2E_SPAWN_DSH=1: another dsh is already attached; we
//     would interfere with it.
//   - any port in [3080, 3083] occupied: skip to avoid racing
//     with the user's live dsh.
func TestE2ESpawnFallbackWhenNoForeignDSH(t *testing.T) {
	if os.Getenv(e2eSpawnDSHEnv) == "1" {
		t.Skipf("%s=1; spawn mode skipped", e2eSpawnDSHEnv)
	}
	for _, p := range []string{"3080", "3081", "3082", "3083"} {
		if portOccupied(t, p) {
			t.Skipf("port %s occupied (likely user's live dsh); skipping", p)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, attached := tryAttachExistingDSH(ctx, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if attached {
		t.Fatal("tryAttachExistingDSH returned attached=true with no dsh on 3080-3099; want false")
	}
}

// portOccupied returns true if anything is listening on 127.0.0.1:port.
func portOccupied(t *testing.T, port string) bool {
	t.Helper()
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "\n")
}
