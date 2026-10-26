package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/daemoncontrol"
)

// mustSummaryJSON encodes a normalized health summary the way the
// daemon does.
func mustSummaryJSON(t *testing.T, summary channel.HealthSummary) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	return data
}

// doctorSection returns the part of the doctor output that belongs
// to one channel.
func doctorSection(out, name string) string {
	marker := "CHANNEL " + name
	start := strings.Index(out, marker)
	if start < 0 {
		return ""
	}
	rest := out[start:]
	if next := strings.Index(rest[len(marker):], "\nCHANNEL "); next >= 0 {
		return rest[:len(marker)+next]
	}
	return rest
}

// doctorField returns the value of a "label: value" line.
func doctorField(section, label string) string {
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, label+":") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, label+":"))
		}
	}
	return ""
}

// TestWriteDoctorText_RendersOneSectionPerChannel is the payoff of
// the multi-channel health RPC: every running channel gets its own
// section, and a channel that is down says so with its error.
func TestWriteDoctorText_RendersOneSectionPerChannel(t *testing.T) {
	now := time.Now()
	payload := daemoncontrol.HealthPayload{Channels: []daemoncontrol.ChannelHealth{
		{
			Channel: "feishu",
			Summary: mustSummaryJSON(t, channel.HealthSummary{
				Channel:        "feishu",
				Connected:      true,
				LinkVerified:   true,
				LastInboundAt:  now.Add(-90 * time.Second),
				LastOutboundAt: now.Add(-30 * time.Second),
			}),
			Health: json.RawMessage(`{"connected":true,"reconnect_count":2}`),
		},
		{
			Channel: "telegram",
			Summary: mustSummaryJSON(t, channel.HealthSummary{
				Channel:      "telegram",
				Connected:    false,
				LinkVerified: false,
				LastError:    "getUpdates failed: 409 Conflict",
			}),
			Health: json.RawMessage(`{"connected":false}`),
		},
	}}

	var sb strings.Builder
	if err := writeDoctorText(&sb, payload); err != nil {
		t.Fatalf("writeDoctorText: %v", err)
	}
	out := sb.String()

	if strings.Count(out, "CHANNEL ") != 2 {
		t.Errorf("expected one section per channel:\n%s", out)
	}
	feishu := doctorSection(out, "feishu")
	if feishu == "" {
		t.Fatalf("no feishu section:\n%s", out)
	}
	if got := doctorField(feishu, "status"); got != "connected" {
		t.Errorf("feishu status = %q, want connected", got)
	}
	if got := doctorField(feishu, "last inbound"); got != "1m30s ago" {
		t.Errorf("feishu last inbound = %q, want 1m30s ago", got)
	}
	if got := doctorField(feishu, "last outbound"); got != "30s ago" {
		t.Errorf("feishu last outbound = %q, want 30s ago", got)
	}

	telegram := doctorSection(out, "telegram")
	if telegram == "" {
		t.Fatalf("no telegram section:\n%s", out)
	}
	if got := doctorField(telegram, "status"); got != "disconnected" {
		t.Errorf("telegram status = %q, want disconnected", got)
	}
	if got := doctorField(telegram, "last inbound"); got != "never" {
		t.Errorf("telegram last inbound = %q, want never", got)
	}
	if got := doctorField(telegram, "last outbound"); got != "never" {
		t.Errorf("telegram last outbound = %q, want never", got)
	}
	if got := doctorField(telegram, "last error"); !strings.Contains(got, "getUpdates failed: 409 Conflict") {
		t.Errorf("telegram last error = %q", got)
	}
}

// TestWriteDoctorText_UnverifiedLinkIsNotConnected pins the
// distinction the doctor has to make: an adapter that was started
// but never verified its link must not be shown as connected.
func TestWriteDoctorText_UnverifiedLinkIsNotConnected(t *testing.T) {
	payload := daemoncontrol.HealthPayload{Channels: []daemoncontrol.ChannelHealth{{
		Channel: "telegram",
		Summary: mustSummaryJSON(t, channel.HealthSummary{
			Channel:      "telegram",
			Connected:    true,
			LinkVerified: false,
		}),
	}}}

	var sb strings.Builder
	if err := writeDoctorText(&sb, payload); err != nil {
		t.Fatalf("writeDoctorText: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "started (link not verified)") {
		t.Errorf("unverified link must not read as connected:\n%s", out)
	}
	if strings.Contains(out, "status:\tconnected") {
		t.Errorf("unverified link reported as connected:\n%s", out)
	}
}

// TestWriteDoctorText_NoChannels asserts the empty daemon says so
// instead of printing a zero-valued table.
func TestWriteDoctorText_NoChannels(t *testing.T) {
	var sb strings.Builder
	if err := writeDoctorText(&sb, daemoncontrol.HealthPayload{}); err != nil {
		t.Fatalf("writeDoctorText: %v", err)
	}
	if !strings.Contains(sb.String(), "NO CHANNELS RUNNING") {
		t.Errorf("expected an explicit empty state:\n%s", sb.String())
	}
}

// TestWriteDoctorJSON_KeepsSummaryAndRawPayload asserts the JSON
// output carries both the normalized view and the channel's own
// payload, for every channel.
func TestWriteDoctorJSON_KeepsSummaryAndRawPayload(t *testing.T) {
	payload := daemoncontrol.HealthPayload{Channels: []daemoncontrol.ChannelHealth{
		{
			Channel: "feishu",
			Summary: mustSummaryJSON(t, channel.HealthSummary{
				Channel:      "feishu",
				Connected:    true,
				LinkVerified: true,
			}),
			Health: json.RawMessage(`{"connected":true,"reconnect_count":2}`),
		},
		{
			Channel: "telegram",
			Summary: mustSummaryJSON(t, channel.HealthSummary{
				Channel:      "telegram",
				Connected:    true,
				LinkVerified: true,
			}),
			Health: json.RawMessage(`{"username":"testbot","connected":true}`),
		},
	}}

	var sb strings.Builder
	if err := writeDoctorJSON(&sb, payload); err != nil {
		t.Fatalf("writeDoctorJSON: %v", err)
	}
	var decoded struct {
		Channels []struct {
			Channel string          `json:"channel"`
			Summary json.RawMessage `json:"summary"`
			Health  json.RawMessage `json:"health"`
		} `json:"channels"`
	}
	if err := json.Unmarshal([]byte(sb.String()), &decoded); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(decoded.Channels) != 2 {
		t.Fatalf("channels = %d, want 2:\n%s", len(decoded.Channels), sb.String())
	}
	if decoded.Channels[0].Channel != "feishu" || decoded.Channels[1].Channel != "telegram" {
		t.Errorf("channels = %q, %q", decoded.Channels[0].Channel, decoded.Channels[1].Channel)
	}
	// The raw payload must survive untouched — it carries the
	// channel-specific detail the operator needs.
	var feishuHealth map[string]any
	if err := json.Unmarshal(decoded.Channels[0].Health, &feishuHealth); err != nil {
		t.Fatalf("decode feishu payload: %v", err)
	}
	if feishuHealth["reconnect_count"] != float64(2) {
		t.Errorf("raw payload was rewritten: %s", decoded.Channels[0].Health)
	}
	var telegramHealth map[string]any
	if err := json.Unmarshal(decoded.Channels[1].Health, &telegramHealth); err != nil {
		t.Fatalf("decode telegram payload: %v", err)
	}
	if telegramHealth["username"] != "testbot" {
		t.Errorf("raw payload was rewritten: %s", decoded.Channels[1].Health)
	}
	var summary map[string]any
	if err := json.Unmarshal(decoded.Channels[0].Summary, &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary["link_verified"] != true {
		t.Errorf("summary = %v, want link_verified true", summary)
	}
}
