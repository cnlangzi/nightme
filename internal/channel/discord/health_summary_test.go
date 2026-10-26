package discord

import (
	"context"
	"errors"
	"testing"
)

// getMeFailedREST rejects the users/@me round trip the way a bad
// token does.
type getMeFailedREST struct {
	*fakeREST
}

func (r *getMeFailedREST) GetMe(context.Context) (User, error) {
	return User{}, errors.New("401 unauthorized")
}

// TestHealthSummary_LinkVerifiedAfterGetMe pins the link semantics:
// users/@me succeeded, so the credentials and the link both work.
func TestHealthSummary_LinkVerifiedAfterGetMe(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	// Wire a no-op gateway so Start's goroutine spawn is safe.
	a.gw = &gatewayClient{
		api:   rest,
		state: a.state,
		cfg:   gatewaySnapshot{Token: "x", Intents: 46593, UserAgent: "test"},
	}
	a.SetLogger(testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()

	summary := a.HealthSummary()
	if !summary.LinkVerified {
		t.Error("LinkVerified = false after a successful users/@me")
	}
	// The gateway never reached READY, so the link is proven but not
	// connected.
	if summary.Connected {
		t.Error("Connected = true without a READY handshake")
	}
}

// TestHealthSummary_LinkVerifiedNeedsGetMe asserts a rejected token
// leaves the link unverified.
func TestHealthSummary_LinkVerifiedNeedsGetMe(t *testing.T) {
	a := newTestAdapter(&getMeFailedREST{fakeREST: &fakeREST{}})
	if err := a.Start(context.Background()); err == nil {
		t.Fatal("expected Start to fail on a rejected token")
	}
	if a.HealthSummary().LinkVerified {
		t.Error("LinkVerified = true after a failed users/@me")
	}
}

// TestHealthSummary_ConnectedAfterReady asserts the gateway
// handshake is what makes the channel connected.
func TestHealthSummary_ConnectedAfterReady(t *testing.T) {
	a := newTestAdapter(&fakeREST{})
	a.onReady(User{ID: "999", Username: "fake-bot"})

	summary := a.HealthSummary()
	if !summary.Connected {
		t.Error("Connected = false after the READY handshake")
	}
	if summary.Channel != "discord" {
		t.Errorf("Channel = %q, want discord", summary.Channel)
	}
}
