package httpclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDefault_TimeoutMatchesDefaultTimeout(t *testing.T) {
	client := Default()
	if client == nil {
		t.Fatal("Default returned nil")
	}
	if client.Timeout != DefaultTimeout {
		t.Fatalf("Default.Timeout = %v, want %v", client.Timeout, DefaultTimeout)
	}
}

func TestDefault_ClientIsUsable(t *testing.T) {
	// We can't directly assert that client.Transport is
	// http.DefaultTransport (client.Transport is nil and
	// http.Client.Do falls back to http.DefaultTransport at
	// request time). Instead, we hit an httptest server to
	// confirm the client dials correctly.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := Default()
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do failed: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
}

func TestDefault_TransportProxyFuncIsSet(t *testing.T) {
	// Sanity: http.DefaultTransport has a non-nil Proxy func
	// (ProxyFromEnvironment). We rely on this for env-var
	// routing. http.Client's Transport defaults to nil and
	// resolves to DefaultTransport at Do-time, so we check the
	// global directly.
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("http.DefaultTransport.Proxy must be non-nil (ProxyFromEnvironment)")
	}
}

func TestDefaultWithTimeout_HonoursDuration(t *testing.T) {
	cases := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		30 * time.Second,
		90 * time.Second,
	}
	for _, d := range cases {
		client := DefaultWithTimeout(d)
		if client.Timeout != d {
			t.Fatalf("DefaultWithTimeout(%v).Timeout = %v", d, client.Timeout)
		}
	}
}

func TestDefault_DoesNotShareState(t *testing.T) {
	// Two Default() calls must produce independent clients.
	a := Default()
	b := Default()
	if a == b {
		t.Fatal("Default must not return shared state")
	}
}

func TestDefaultTimeout_Reasonable(t *testing.T) {
	// Sanity check: DefaultTimeout must be long enough for a
	// slow upstream API call but short enough not to hang the
	// daemon. 45s sits comfortably in that band — if this test
	// starts failing, someone has decided to change the value
	// and probably should write a CHANGELOG entry.
	if DefaultTimeout < 5*time.Second {
		t.Fatalf("DefaultTimeout = %v is too aggressive for upstream APIs", DefaultTimeout)
	}
	if DefaultTimeout > 5*time.Minute {
		t.Fatalf("DefaultTimeout = %v is too long for daemon health", DefaultTimeout)
	}
	if strings.TrimSpace(DefaultTimeout.String()) == "" {
		t.Fatal("DefaultTimeout has no string representation")
	}
}
