package daemoncontrol

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestHealthRPCReportsEveryChannel pins the multi-channel contract:
// the "health" RPC answers with one entry per registered channel, not
// just the last one registered. Before the fix, SetHealthProvider
// overwrote the previous registration, so a daemon running two
// channels reported one — and `nightme doctor` showed nothing about
// the other.
func TestHealthRPCReportsEveryChannel(t *testing.T) {
	dir := t.TempDir()
	paths, err := ResolvePaths(dir)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := Listen(paths.Socket, Status{PID: 42, StartedAt: time.Now()}, cancel)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer server.Close()
	go func() { _ = server.Serve() }()

	server.AddHealthSource(HealthSource{
		Channel:  "feishu",
		Snapshot: func() (json.RawMessage, error) { return json.RawMessage(`{"connected":true}`), nil },
		Summary:  func() (json.RawMessage, error) { return json.RawMessage(`{"channel":"feishu","connected":true}`), nil },
	})
	server.AddHealthSource(HealthSource{
		Channel:  "telegram",
		Snapshot: func() (json.RawMessage, error) { return json.RawMessage(`{"connected":false}`), nil },
		Summary: func() (json.RawMessage, error) {
			return json.RawMessage(`{"channel":"telegram","connected":false}`), nil
		},
	})

	payload, err := GetHealth(paths.Socket, 2*time.Second)
	if err != nil {
		t.Fatalf("health RPC: %v", err)
	}
	if len(payload.Channels) != 2 {
		t.Fatalf("channels = %d, want 2: %+v", len(payload.Channels), payload.Channels)
	}
	if payload.Channels[0].Channel != "feishu" || payload.Channels[1].Channel != "telegram" {
		t.Errorf("channel order = %q, %q — registration order must be preserved",
			payload.Channels[0].Channel, payload.Channels[1].Channel)
	}
	for _, entry := range payload.Channels {
		if len(entry.Summary) == 0 {
			t.Errorf("%s: missing summary", entry.Channel)
		}
		if len(entry.Health) == 0 {
			t.Errorf("%s: missing raw payload", entry.Channel)
		}
	}
}

// TestHealthRPCKeepsOtherChannelsWhenOneFails asserts the failure of
// one channel's payload doesn't hide the others: the broken channel
// reports its error, the healthy one still answers.
func TestHealthRPCKeepsOtherChannelsWhenOneFails(t *testing.T) {
	dir := t.TempDir()
	paths, err := ResolvePaths(dir)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := Listen(paths.Socket, Status{PID: 42, StartedAt: time.Now()}, cancel)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer server.Close()
	go func() { _ = server.Serve() }()

	server.AddHealthSource(HealthSource{
		Channel:  "broken",
		Snapshot: func() (json.RawMessage, error) { return nil, errors.New("snapshot unavailable") },
		Summary:  func() (json.RawMessage, error) { return json.RawMessage(`{"channel":"broken"}`), nil },
	})
	server.AddHealthSource(HealthSource{
		Channel:  "healthy",
		Snapshot: func() (json.RawMessage, error) { return json.RawMessage(`{"connected":true}`), nil },
		Summary:  func() (json.RawMessage, error) { return json.RawMessage(`{"channel":"healthy"}`), nil },
	})

	payload, err := GetHealth(paths.Socket, 2*time.Second)
	if err != nil {
		t.Fatalf("health RPC: %v", err)
	}
	if len(payload.Channels) != 2 {
		t.Fatalf("channels = %d, want 2", len(payload.Channels))
	}
	if payload.Channels[0].Error == "" {
		t.Error("broken channel: no error reported")
	}
	if len(payload.Channels[0].Summary) == 0 {
		t.Error("broken channel: summary must survive a failing snapshot")
	}
	if payload.Channels[1].Error != "" {
		t.Errorf("healthy channel: unexpected error %q", payload.Channels[1].Error)
	}
	if len(payload.Channels[1].Health) == 0 {
		t.Error("healthy channel: missing raw payload")
	}
}
