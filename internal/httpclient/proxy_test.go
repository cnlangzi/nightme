// proxy_test.go — the process-wide outbound proxy.
//
// Every channel adapter builds its own &http.Client{} without a
// Transport, so mutating http.DefaultTransport is what actually
// reaches them; the Feishu SDK client builds its own Transport and
// reads ProxyFunc instead.

package httpclient

import (
	"net/http"
	"testing"
)

// resetProcessProxy undoes what SetProcessProxy installed so tests
// do not leak a configured proxy into each other.
func resetProcessProxy() {
	processProxyMu.Lock()
	processProxy = nil
	processProxyMu.Unlock()
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		tr.Proxy = http.ProxyFromEnvironment
	}
}

// TestSetProcessProxy_EmptyIsNoOp pins the "configured means
// honoured, absent means unchanged" rule: an unset proxy must leave
// the standard HTTP_PROXY / HTTPS_PROXY handling exactly as it was.
func TestSetProcessProxy_EmptyIsNoOp(t *testing.T) {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is no longer an *http.Transport")
	}
	if tr.Proxy == nil {
		t.Fatal("precondition: http.DefaultTransport has no proxy resolver")
	}
	if err := SetProcessProxy(""); err != nil {
		t.Fatalf("SetProcessProxy(\"\"): %v", err)
	}
	if tr.Proxy == nil {
		t.Error("SetProcessProxy(\"\") cleared the default proxy resolver")
	}
	if ProxyFunc() == nil {
		t.Error("SetProcessProxy(\"\") left no proxy resolver")
	}
}

func TestSetProcessProxy_ReachesDefaultTransport(t *testing.T) {
	defer resetProcessProxy()

	if err := SetProcessProxy("http://proxy.example:3128"); err != nil {
		t.Fatalf("SetProcessProxy: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, "https://api.telegram.org/bot123/getMe", nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := ProxyFunc()(req)
	if err != nil {
		t.Fatalf("ProxyFunc: %v", err)
	}
	if u == nil || u.Host != "proxy.example:3128" {
		t.Fatalf("ProxyFunc routed to %v, want the configured proxy", u)
	}

	// A client that leaves Transport at the default — which is what
	// every channel adapter does — must land on the same proxy.
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is no longer an *http.Transport")
	}
	u, err = tr.Proxy(req)
	if err != nil || u == nil || u.Host != "proxy.example:3128" {
		t.Fatalf("http.DefaultTransport routed to %v (err=%v), want the configured proxy", u, err)
	}
}

// TestProxyFunc_DefaultsWhenNothingConfigured: with no process proxy
// installed, ProxyFunc still hands back a usable resolver so a config
// that never mentions a proxy behaves exactly as it always has.
// (The specific destination it picks depends on the process
// environment, which is not this test's to control.)
func TestProxyFunc_DefaultsWhenNothingConfigured(t *testing.T) {
	defer resetProcessProxy()
	resetProcessProxy()

	req, err := http.NewRequest(http.MethodGet, "https://api.telegram.org/bot123/getMe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ProxyFunc() == nil {
		t.Fatal("ProxyFunc returned nil resolver with nothing configured")
	}
	if _, err := ProxyFunc()(req); err != nil {
		t.Fatalf("ProxyFunc: %v", err)
	}
}

func TestSetProcessProxy_RejectsUnusableURLs(t *testing.T) {
	defer resetProcessProxy()
	for _, bad := range []string{"http://", "://nope", "not a url at all"} {
		if err := SetProcessProxy(bad); err == nil {
			t.Errorf("SetProcessProxy(%q): want error, got nil", bad)
		}
	}
}
