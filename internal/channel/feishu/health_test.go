// Package feishu — F-40 tests for WSHealth state tracker.
package feishu

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWSHealth_RecordConnect(t *testing.T) {
	h := &WSHealth{}
	now := time.Now()
	h.recordConnect(now)

	if !h.Connected {
		t.Error("expected Connected=true after recordConnect")
	}
	if !h.LastConnectedAt.Equal(now) {
		t.Errorf("LastConnectedAt = %v, want %v", h.LastConnectedAt, now)
	}
	if h.ReconnectCount != 0 {
		t.Errorf("ReconnectCount = %d, want 0", h.ReconnectCount)
	}
	if len(h.EventRing) != 1 {
		t.Errorf("EventRing len = %d, want 1", len(h.EventRing))
	}
}

func TestWSHealth_RecordDisconnect(t *testing.T) {
	h := &WSHealth{}
	now := time.Now()
	h.recordConnect(now)
	h.recordDisconnect(now.Add(time.Second))

	if h.Connected {
		t.Error("expected Connected=false after recordDisconnect")
	}
	if h.LastDisconnectedAt.IsZero() {
		t.Error("LastDisconnectedAt should be set after first disconnect")
	}
}

func TestWSHealth_RecordReconnecting_IncrementsCount(t *testing.T) {
	h := &WSHealth{}
	now := time.Now()
	for i := 0; i < 5; i++ {
		h.recordReconnecting(now.Add(time.Duration(i)*time.Second), "attempt")
	}
	if h.ReconnectCount != 5 {
		t.Errorf("ReconnectCount = %d, want 5", h.ReconnectCount)
	}
	if len(h.EventRing) != 5 {
		t.Errorf("EventRing len = %d, want 5", len(h.EventRing))
	}
}

func TestWSHealth_RecordError(t *testing.T) {
	h := &WSHealth{}
	now := time.Now()
	h.recordError(now, "boom")

	if h.LastError != "boom" {
		t.Errorf("LastError = %q, want %q", h.LastError, "boom")
	}
	if h.LastErrorAt.IsZero() {
		t.Error("LastErrorAt should be set")
	}
}

func TestWSHealth_RecordInboundAndOutbound(t *testing.T) {
	h := &WSHealth{}
	now := time.Now()
	h.recordInbound(now, "oc_test", "text")
	h.recordOutbound(now.Add(time.Second), "oc_test", "send_card")

	if h.LastInboundAt.IsZero() {
		t.Error("LastInboundAt should be set")
	}
	if h.LastOutboundAt.IsZero() {
		t.Error("LastOutboundAt should be set")
	}
	if h.LastInboundChatID != "oc_test" {
		t.Errorf("LastInboundChatID = %q, want %q", h.LastInboundChatID, "oc_test")
	}
	if len(h.InboundRing) != 1 || h.InboundRing[0].Kind != "text" {
		t.Errorf("InboundRing[0] = %+v, want kind=text", h.InboundRing[0])
	}
	if len(h.OutboundRing) != 1 || h.OutboundRing[0].Kind != "send_card" {
		t.Errorf("OutboundRing[0] = %+v, want kind=send_card", h.OutboundRing[0])
	}
}

func TestWSHealth_RingCapacity(t *testing.T) {
	h := &WSHealth{}
	now := time.Now()
	// Push more than healthEventRingSize events; oldest must be evicted.
	for i := 0; i < healthEventRingSize+5; i++ {
		h.recordReconnecting(now.Add(time.Duration(i)*time.Millisecond), "x")
	}
	if len(h.EventRing) != healthEventRingSize {
		t.Errorf("EventRing len = %d, want %d (cap)", len(h.EventRing), healthEventRingSize)
	}
}

func TestWSHealth_ConcurrentAccess(t *testing.T) {
	h := &WSHealth{}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				h.recordReconnecting(time.Now(), "x")
				h.recordError(time.Now(), "x")
				h.recordInbound(time.Now(), "c", "k")
				_ = h.Snapshot()
			}
		}(i)
	}
	wg.Wait()
	// No assertion on counts (race-prone); just no panic / no data race.
}

func TestWSHealth_SnapshotJSON(t *testing.T) {
	h := &WSHealth{}
	now := time.Now().UTC().Truncate(time.Millisecond) // RFC3339 round-trip
	h.recordConnect(now)
	h.recordError(now.Add(time.Second), "boom")
	h.recordInbound(now.Add(2*time.Second), "oc_1", "text")
	h.recordOutbound(now.Add(3*time.Second), "oc_1", "send_card")

	snap := h.Snapshot()
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, `"connected":true`) {
		t.Errorf("expected connected=true in JSON, got %s", out)
	}
	if !strings.Contains(out, `"reconnect_count":0`) {
		t.Errorf("expected reconnect_count=0, got %s", out)
	}
	if !strings.Contains(out, `"last_error":"boom"`) {
		t.Errorf("expected last_error=boom, got %s", out)
	}
}
