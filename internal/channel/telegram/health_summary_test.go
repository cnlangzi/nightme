package telegram

import (
	"context"
	"testing"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestHealthSummary_LinkVerifiedNeedsGetMe pins the distinction the
// doctor renders: telegram's adapter state only proves the link once
// getMe has succeeded against Telegram's API.
func TestHealthSummary_LinkVerifiedNeedsGetMe(t *testing.T) {
	a, api := newTestAdapter(t)
	api.GetMeErr = &apiError{Message: "boom"}
	if err := a.Start(context.Background()); err == nil {
		t.Fatal("expected Start to fail when getMe fails")
	}
	if a.HealthSummary().LinkVerified {
		t.Error("LinkVerified = true after a failed getMe")
	}
}

// TestHealthSummary_LinkVerifiedAfterGetMe is the other half: a
// successful getMe means the credentials and the link both work.
func TestHealthSummary_LinkVerifiedAfterGetMe(t *testing.T) {
	a, _ := newTestAdapter(t)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = a.Stop(context.Background()) }()

	summary := a.HealthSummary()
	if !summary.LinkVerified {
		t.Error("LinkVerified = false after a successful getMe")
	}
	if !summary.Connected {
		t.Error("Connected = false for a running adapter")
	}
	if summary.Channel != "telegram" {
		t.Errorf("Channel = %q, want telegram", summary.Channel)
	}
}

// TestHealthSummary_StartedButUnverified covers the state the doctor
// must not report as connected: the adapter was started, but no
// remote round trip ever proved the link.
func TestHealthSummary_StartedButUnverified(t *testing.T) {
	a, _ := newTestAdapter(t)
	a.mu.Lock()
	a.started = true
	a.mu.Unlock()

	summary := a.HealthSummary()
	if !summary.Connected {
		t.Error("Connected = false, want true for a started adapter")
	}
	if summary.LinkVerified {
		t.Error("LinkVerified = true without a successful getMe")
	}
}

// TestHealthSummary_SilentDropDoesNotStampOutbound asserts a send
// that never reaches Telegram doesn't make the channel look alive
// on the outbound side.
func TestHealthSummary_SilentDropDoesNotStampOutbound(t *testing.T) {
	a, _ := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID:    "100",
		Kind:      messages.OutInit,
		AgentName: "claude",
		Model:     "opus",
		SessionID: "sess-1",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !a.HealthSummary().LastOutboundAt.IsZero() {
		t.Error("a silent drop stamped the outbound liveness clock")
	}
}

// TestHealthSummary_SendStampsOutbound is the other half: a real
// outbound advances the clock.
func TestHealthSummary_SendStampsOutbound(t *testing.T) {
	a, _ := newTestAdapter(t)
	if err := a.Send(context.Background(), messages.OutboundMessage{
		ChatID: "100",
		Kind:   messages.OutCommandReply,
		Text:   "cmd reply",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if a.HealthSummary().LastOutboundAt.IsZero() {
		t.Error("a delivered message did not stamp the outbound liveness clock")
	}
}
