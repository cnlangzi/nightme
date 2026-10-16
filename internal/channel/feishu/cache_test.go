// Package feishu — tenant_access_token cache tests.
//
// These tests lock down the behaviour of invalidateableCache and
// the cache-key formula it uses to evict SDK-cached tokens. The
// key formula mirrors an unexported SDK function, so the
// integration tests at the bottom of the file (KeyFormulaMatchesSDK
// and InvalidateForcesRefetch) drive a real lark.Client against an
// httptest mock Feishu endpoint and verify our InvalidateTenant
// actually hits the entry the SDK wrote. If the SDK ever changes
// tenantAccessTokenKey's prefix or hashing scheme, those tests fail
// with a clear message — without them a silent drift would leave
// the invalidate hook firing on a stale key while the SDK keeps
// its own entry, masking the 99991663 burst we're trying to fix.
package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

func TestInvalidatableCache_GetSet(t *testing.T) {
	c := newInvalidatableCache()
	ctx := context.Background()

	if got, err := c.Get(ctx, "k"); err != nil || got != "" {
		t.Fatalf("Get on empty cache = (%q, %v), want (\"\", nil)", got, err)
	}
	if err := c.Set(ctx, "k", "v1", time.Minute); err != nil {
		t.Fatalf("Set returned err: %v", err)
	}
	if got, _ := c.Get(ctx, "k"); got != "v1" {
		t.Fatalf("Get after Set = %q, want \"v1\"", got)
	}
}

func TestInvalidatableCache_GetReturnsEmptyAfterTTL(t *testing.T) {
	c := newInvalidatableCache()
	ctx := context.Background()
	// 0-TTL Set is exactly the trick invalidateableCache itself
	// relies on; mirror it here to confirm Get honours expireAt.
	if err := c.Set(ctx, "k", "v", 0); err != nil {
		t.Fatalf("Set err: %v", err)
	}
	if got, _ := c.Get(ctx, "k"); got != "" {
		t.Fatalf("Get after 0-TTL Set = %q, want \"\"", got)
	}
}

func TestInvalidatableCache_InvalidateTenant(t *testing.T) {
	c := newInvalidatableCache()
	ctx := context.Background()
	appID, appSecret := "cli_test", "secret-xyz"
	key := tenantAccessTokenKey(appID, appSecret, "")
	if err := c.Set(ctx, key, "stale-token", time.Hour); err != nil {
		t.Fatalf("Set err: %v", err)
	}
	if got, _ := c.Get(ctx, key); got != "stale-token" {
		t.Fatalf("precondition Get = %q, want \"stale-token\"", got)
	}
	c.InvalidateTenant(appID, appSecret)
	if got, _ := c.Get(ctx, key); got != "" {
		t.Fatalf("Get after InvalidateTenant = %q, want \"\"", got)
	}
}

func TestInvalidatableCache_InvalidateTenantEmptyCreds(t *testing.T) {
	// Defensive: a partial config (e.g., AppID present, AppSecret
	// blank) must not silently wipe every cached tenant under an
	// empty key. Empty input → no-op.
	c := newInvalidatableCache()
	ctx := context.Background()
	// Pre-populate the canonical key so we can detect a wrong-key
	// deletion if the guard ever regresses.
	key := tenantAccessTokenKey("cli_real", "secret-real", "")
	_ = c.Set(ctx, key, "untouched", time.Hour)

	c.InvalidateTenant("", "")
	c.InvalidateTenant("cli_only", "")
	c.InvalidateTenant("", "secret_only")

	if got, _ := c.Get(ctx, key); got != "untouched" {
		t.Fatalf("empty-cred InvalidateTenant wiped canonical key (got %q)", got)
	}
}

func TestInvalidatableCache_ImplementsCoreCache(t *testing.T) {
	// Compile-time guard in cache.go already enforces this, but a
	// runtime assertion makes the contract visible to anyone
	// reading this test file.
	var _ larkcore.Cache = newInvalidatableCache()
}

// TestTenantAccessTokenKey_Format locks down the key formula. If
// lark-sdk-go ever changes tenantAccessTokenKey (prefix, hashing,
// or separator), this test must be updated alongside the
// production function so the two stay in sync.
//
// The expected value below is hand-computed from
// `tenant_access_token:app_secret:<appID>:<sha256(appSecret)>:<tenantKey>`
// using appID="cli_a", appSecret="secret-a", tenantKey="tenant-a"
// (mirrors the example in lark-sdk-go's own
// tokenmanager_test.go).
func TestTenantAccessTokenKey_Format(t *testing.T) {
	const (
		appID     = "cli_a"
		appSecret = "secret-a"
		tenantKey = "tenant-a"
	)
	sum := sha256.Sum256([]byte(appSecret))
	want := "tenant_access_token:app_secret:" + appID + ":" + hex.EncodeToString(sum[:]) + ":" + tenantKey
	got := tenantAccessTokenKey(appID, appSecret, tenantKey)
	if got != want {
		t.Fatalf("tenantAccessTokenKey formula drifted:\n got: %q\nwant: %q", got, want)
	}
}

// fakeTokenServer is a minimal stand-in for the Feishu auth API:
// serves a fresh tenant_access_token on the SDK's expected path,
// and a 200 OK with empty data on any other path so the SDK's
// downstream call doesn't blow up before we get to assert on the
// cache. tokenCalls counts how many times the token endpoint was
// hit so tests can assert refetch behaviour.
type fakeTokenServer struct {
	server     *httptest.Server
	tokenCalls atomic.Int64
	token      string
}

func newFakeTokenServer(token string) *fakeTokenServer {
	f := &fakeTokenServer{token: token}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == larkcore.TenantAccessTokenInternalUrlPath {
			f.tokenCalls.Add(1)
			_ = json.NewEncoder(w).Encode(larkcore.TenantAccessTokenResp{
				// CodeError is embedded in TenantAccessTokenResp, so
				// its Code field is promoted — write Code directly
				// rather than wrapping in larkcore.CodeError{...}.
				Code:              0,
				Expire:            7200,
				TenantAccessToken: token,
			})
			return
		}
		// Any other endpoint: pretend success so the SDK's POST
		// flow returns without an HTTP error. Body content is
		// irrelevant — the test only inspects the cache.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	return f
}

func (f *fakeTokenServer) Close() { f.server.Close() }

// TestInvalidatableCache_KeyFormulaMatchesSDK is the canary that
// catches lark-sdk-go changing its internal tenant_access_token
// cache key. Without this test, a SDK upgrade that tweaks the
// prefix or hashing scheme would silently leave our invalidate
// hook firing on a wrong key while the SDK kept its own entry —
// we'd see 99991663 bursts come back without the health counter
// (TokenInvalidations) being a smoking gun.
//
// The test triggers a real SDK token fetch through lark.Client and
// asserts our tenantAccessTokenKey lookup finds what the SDK just
// wrote. A failure here means: re-derive the formula from
// lark-sdk-go core/tokenmanager.go:tenantAccessTokenKey and update
// our mirror in cache.go.
func TestInvalidatableCache_KeyFormulaMatchesSDK(t *testing.T) {
	fake := newFakeTokenServer("sdk-cached-token-from-real-call")
	defer fake.Close()

	cache := newInvalidatableCache()
	const appID = "cli_keytest"
	const appSecret = "secret-keytest"

	cli := lark.NewClient(appID, appSecret,
		lark.WithTokenCache(cache),
		lark.WithOpenBaseUrl(fake.server.URL),
		// Quiet the SDK's default logger so test output stays clean.
		lark.WithLogger(larkcore.NewDefaultLogger(larkcore.LogLevelError)),
	)

	// Trigger any tenant_access_token-bearing call. We hit a
	// harmless POST path; the SDK will fetch the token first,
	// then issue our request. The fake server returns ok on both
	// legs.
	_, _ = cli.Post(context.Background(), "/open-apis/test/noop",
		struct{}{}, larkcore.AccessTokenTypeTenant)

	// 1) Token endpoint must have been hit exactly once.
	if got := fake.tokenCalls.Load(); got != 1 {
		t.Fatalf("expected 1 token fetch, got %d", got)
	}

	// 2) Our key formula must find what the SDK just cached.
	key := tenantAccessTokenKey(appID, appSecret, "")
	got, err := cache.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("cache.Get returned err: %v", err)
	}
	if got != "sdk-cached-token-from-real-call" {
		t.Fatalf("key formula does NOT match lark-sdk-go's internal key.\n"+
			"  cache key we built: %q\n"+
			"  cached value:       %q (want \"sdk-cached-token-from-real-call\")\n"+
			"  most likely cause: lark-sdk-go changed tenantAccessTokenKey prefix or hash.\n"+
			"  fix: re-derive from lark-sdk-go core/tokenmanager.go and update cache.go.",
			key, got)
	}

	// 3) Invalidate must drop that entry.
	cache.InvalidateTenant(appID, appSecret)
	got, _ = cache.Get(context.Background(), key)
	if got != "" {
		t.Fatalf("InvalidateTenant did not clear SDK-written entry; got %q", got)
	}
}

// TestInvalidatableCache_InvalidateForcesRefetch proves the
// invalidate hook fires the SDK's cache-miss refresh path on the
// next REST call, not just empties our view of the cache. Without
// this confirmation we'd be one indirection away from the actual
// SDK behaviour: it's possible (in principle) to delete the
// entry from our wrapper while the SDK's singleton still holds a
// reference, in which case the next call would reuse the stale
// token. This test fails closed if that ever happens.
func TestInvalidatableCache_InvalidateForcesRefetch(t *testing.T) {
	fake := newFakeTokenServer("sdk-cached-token-v1")
	defer fake.Close()

	cache := newInvalidatableCache()
	const appID = "cli_refetch"
	const appSecret = "secret-refetch"

	cli := lark.NewClient(appID, appSecret,
		lark.WithTokenCache(cache),
		lark.WithOpenBaseUrl(fake.server.URL),
		lark.WithLogger(larkcore.NewDefaultLogger(larkcore.LogLevelError)),
	)

	// First call: SDK fetches + caches.
	_, _ = cli.Post(context.Background(), "/open-apis/test/noop",
		struct{}{}, larkcore.AccessTokenTypeTenant)
	if got := fake.tokenCalls.Load(); got != 1 {
		t.Fatalf("precondition: expected 1 token fetch, got %d", got)
	}

	// Second call within TTL: SDK must reuse cache, no new fetch.
	_, _ = cli.Post(context.Background(), "/open-apis/test/noop",
		struct{}{}, larkcore.AccessTokenTypeTenant)
	if got := fake.tokenCalls.Load(); got != 1 {
		t.Fatalf("expected cached token to be reused (1 fetch), got %d", got)
	}

	// Invalidate, then make a third call. SDK must see the
	// cache miss and re-fetch.
	cache.InvalidateTenant(appID, appSecret)
	_, _ = cli.Post(context.Background(), "/open-apis/test/noop",
		struct{}{}, larkcore.AccessTokenTypeTenant)
	if got := fake.tokenCalls.Load(); got != 2 {
		t.Fatalf("expected InvalidateTenant to force a refetch (2 fetches total), got %d.\n"+
			"  most likely cause: lark-sdk-go retained a copy of the token outside our cache,\n"+
			"  or the invalidate hook fired on a wrong key.",
			got)
	}
}
