// Package feishu — tenant_access_token cache tests.
//
// Unit tests cover invalidateableCache directly. KeyFormulaMatchesSDK
// and InvalidateForcesRefetch drive a real lark.Client to pin the
// unexported tenantAccessTokenKey formula against SDK drift.
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
	if ok := c.InvalidateTenant(appID, appSecret); !ok {
		t.Fatalf("InvalidateTenant on present entry returned false, want true")
	}
	if got, _ := c.Get(ctx, key); got != "" {
		t.Fatalf("Get after InvalidateTenant = %q, want \"\"", got)
	}
	// Second invalidate on the same (now empty) entry returns false.
	if ok := c.InvalidateTenant(appID, appSecret); ok {
		t.Fatalf("InvalidateTenant on empty entry returned true, want false")
	}
}

func TestInvalidatableCache_InvalidateTenantEmptyCreds(t *testing.T) {
	// Defensive: a partial config (e.g., AppID present, AppSecret
	// blank) must not silently wipe every cached tenant under an
	// empty key. Empty input → false (no eviction).
	c := newInvalidatableCache()
	ctx := context.Background()
	// Pre-populate the canonical key so we can detect a wrong-key
	// deletion if the guard ever regresses.
	key := tenantAccessTokenKey("cli_real", "secret-real", "")
	_ = c.Set(ctx, key, "untouched", time.Hour)

	if ok := c.InvalidateTenant("", ""); ok {
		t.Fatalf("InvalidateTenant(\"\", \"\") returned true, want false")
	}
	if ok := c.InvalidateTenant("cli_only", ""); ok {
		t.Fatalf("InvalidateTenant(\"cli_only\", \"\") returned true, want false")
	}
	if ok := c.InvalidateTenant("", "secret_only"); ok {
		t.Fatalf("InvalidateTenant(\"\", \"secret_only\") returned true, want false")
	}

	if got, _ := c.Get(ctx, key); got != "untouched" {
		t.Fatalf("empty-cred InvalidateTenant wiped canonical key (got %q)", got)
	}
}

func TestInvalidatableCache_ImplementsCoreCache(t *testing.T) {
	var _ larkcore.Cache = newInvalidatableCache()
}

// TestTenantAccessTokenKey_Format pins the key formula's expected
// output. Update alongside cache.go if SDK changes the scheme.
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

// fakeTokenServer serves a tenant_access_token on the SDK's expected path
// and a stub OK on any other path so the SDK's POST flow returns cleanly.
// tokenCalls counts how many times the token endpoint was hit.
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
				Code:              0,
				Expire:            7200,
				TenantAccessToken: token,
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	return f
}

func (f *fakeTokenServer) Close() { f.server.Close() }

// TestInvalidatableCache_KeyFormulaMatchesSDK drives a real SDK token
// fetch and asserts our tenantAccessTokenKey lookup finds the entry
// the SDK just wrote. Pins the formula against SDK drift.
func TestInvalidatableCache_KeyFormulaMatchesSDK(t *testing.T) {
	fake := newFakeTokenServer("sdk-cached-token-from-real-call")
	defer fake.Close()

	cache := newInvalidatableCache()
	const appID = "cli_keytest"
	const appSecret = "secret-keytest"

	cli := lark.NewClient(appID, appSecret,
		lark.WithTokenCache(cache),
		lark.WithOpenBaseUrl(fake.server.URL),
		lark.WithLogger(larkcore.NewDefaultLogger(larkcore.LogLevelError)),
	)

	_, _ = cli.Post(context.Background(), "/open-apis/test/noop",
		struct{}{}, larkcore.AccessTokenTypeTenant)

	if got := fake.tokenCalls.Load(); got != 1 {
		t.Fatalf("expected 1 token fetch, got %d", got)
	}

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

	if !cache.InvalidateTenant(appID, appSecret) {
		t.Fatalf("InvalidateTenant returned false on SDK-written entry")
	}
	got, _ = cache.Get(context.Background(), key)
	if got != "" {
		t.Fatalf("InvalidateTenant did not clear SDK-written entry; got %q", got)
	}
}

// TestInvalidatableCache_InvalidateForcesRefetch proves the
// invalidate hook actually triggers the SDK's cache-miss refresh
// path on the next REST call.
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

	_, _ = cli.Post(context.Background(), "/open-apis/test/noop",
		struct{}{}, larkcore.AccessTokenTypeTenant)
	if got := fake.tokenCalls.Load(); got != 1 {
		t.Fatalf("precondition: expected 1 token fetch, got %d", got)
	}

	_, _ = cli.Post(context.Background(), "/open-apis/test/noop",
		struct{}{}, larkcore.AccessTokenTypeTenant)
	if got := fake.tokenCalls.Load(); got != 1 {
		t.Fatalf("expected cached token to be reused (1 fetch), got %d", got)
	}

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
