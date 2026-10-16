// Package feishu — tenant_access_token cache invalidation.
//
// Lark SDK's TokenManager is a process-wide singleton (see
// lark-sdk-go core/tokenmanager.go: `var tokenManager = TokenManager{...}`).
// Every *lark.Client created in the process shares one in-memory
// map keyed by tenant_access_token + sha256(AppSecret) + tenantKey,
// with a 3-minute expiry buffer (expiryDelta in core/constants.go).
//
// When the WS connection drops for long enough that the server
// revokes the cached tenant_access_token, the SDK's own retry
// loop in httptransport.go keeps reusing the same stale entry
// until the buffered expireTime passes — observable as a burst
// of 99991663 errors on the first REST call after WS reconnect.
//
// invalidateableCache is installed via WithTokenCache so nightme
// can drop the entry proactively from markSDKConnected (which
// fires on OnReady and OnReconnected), letting the next REST call
// hit the cache-miss refresh path instead.
package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

// invalidateableCache implements larkcore.Cache with the extra
// InvalidateTenant method that nightme's WS-reconnect hook uses
// to evict a stale tenant_access_token entry. State is kept
// in-process; matches lark-sdk-go's localCache semantics
// (Get returns "" on miss or expiry; Set stores (value,
// now+ttl)).
//
// Concurrent-safe: one mutex guards both the map and the
// InvalidateTenant path so an in-flight Get that races with an
// invalidation sees a coherent view.
type invalidateableCache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}

type cacheEntry struct {
	value    string
	expireAt time.Time
}

func newInvalidatableCache() *invalidateableCache {
	return &invalidateableCache{}
}

func (c *invalidateableCache) Get(ctx context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || !e.expireAt.After(time.Now()) {
		return "", nil
	}
	return e.value, nil
}

func (c *invalidateableCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]cacheEntry)
	}
	c.m[key] = cacheEntry{value: value, expireAt: time.Now().Add(ttl)}
	return nil
}

// InvalidateTenant drops the cached tenant_access_token for the
// given self-built app. Next REST call hits cache-miss → SDK
// re-issues the token → fresh entry cached. No-op when appID or
// appSecret is empty (shouldn't happen in practice but keeps the
// hook defensive against partial config).
func (c *invalidateableCache) InvalidateTenant(appID, appSecret string) {
	if appID == "" || appSecret == "" {
		return
	}
	key := tenantAccessTokenKey(appID, appSecret, "")
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

// tenantAccessTokenKey mirrors lark-sdk-go
// core/tokenmanager.go:tenantAccessTokenKey. Re-implemented here
// because the SDK function is unexported. If a future SDK
// release changes the prefix or hashing scheme, the key
// compatibility assertion in cache_test.go will catch the drift.
//
// Format: `tenant_access_token:app_secret:<appID>:<sha256(appSecret)>:<tenantKey>`
// (matches `tenantAccessTokenKeyPrefix` constant in core/constants.go).
func tenantAccessTokenKey(appID, appSecret, tenantKey string) string {
	sum := sha256.Sum256([]byte(appSecret))
	return "tenant_access_token:app_secret:" + appID + ":" + hex.EncodeToString(sum[:]) + ":" + tenantKey
}

// Compile-time guard: invalidateableCache satisfies the SDK's
// Cache interface. If a future SDK release changes the interface
// signature, the build breaks here rather than at runtime.
var _ larkcore.Cache = (*invalidateableCache)(nil)
