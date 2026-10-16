// Package feishu — tenant_access_token cache invalidation.
//
// lark-sdk-go's TokenManager is a process-wide singleton with a
// 3-minute expiry buffer (core/constants.go:expiryDelta). nightme
// installs invalidateableCache via WithTokenCache and drops the
// entry on every WS (re)connect, so the first REST call after a
// reconnect always hits the cache-miss refresh path.
package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

// invalidateableCache wraps larkcore.Cache with the extra
// InvalidateTenant method that nightme's WS-reconnect hook uses
// to evict a tenant_access_token entry on reconnect. Matches
// lark-sdk-go's localCache semantics: Get returns "" on miss or
// expiry; Set stores (value, now+ttl). One mutex guards the map
// for concurrent Get/Set/Invalidate.
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
// given self-built app. Returns true when an entry was present and
// evicted, false when nothing matched (empty creds or cache miss).
// Next REST call hits cache-miss → SDK re-issues the token → fresh
// entry cached.
func (c *invalidateableCache) InvalidateTenant(appID, appSecret string) bool {
	if appID == "" || appSecret == "" {
		return false
	}
	key := tenantAccessTokenKey(appID, appSecret, "")
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; !ok {
		return false
	}
	delete(c.m, key)
	return true
}

// tenantAccessTokenKey mirrors the SDK's unexported
// core/tokenmanager.go:tenantAccessTokenKey. Format:
// `tenant_access_token:app_secret:<appID>:<sha256(appSecret)>:<tenantKey>`.
// KeyFormulaMatchesSDK in cache_test.go pins this against SDK drift.
func tenantAccessTokenKey(appID, appSecret, tenantKey string) string {
	sum := sha256.Sum256([]byte(appSecret))
	return "tenant_access_token:app_secret:" + appID + ":" + hex.EncodeToString(sum[:]) + ":" + tenantKey
}

// Compile-time guard: invalidateableCache satisfies the SDK's Cache interface.
var _ larkcore.Cache = (*invalidateableCache)(nil)
