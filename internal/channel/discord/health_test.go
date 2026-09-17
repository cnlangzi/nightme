package discord

import (
	"encoding/json"
	"testing"
	"time"
)

// TestHealthSnapshot_NewFieldsDefaults verifies the five Phase 3
// observability keys are present with zero defaults on a fresh
// adapter. Operators rely on the keys existing unconditionally
// so daemoncontrol's health RPC can render them as zero rather
// than "missing".
func TestHealthSnapshot_NewFieldsDefaults(t *testing.T) {
	a := newTestAdapter(&fakeREST{})
	a.botUserID = "999"
	a.botName = "nightme-bot"
	a.metrics = newHealthMetrics()

	name, payload, err := a.HealthSnapshot()
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if name != "discord" {
		t.Errorf("name = %q", name)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{
		"reconnect_count",
		"last_event_unix_ts",
		"last_close_code",
		"last_reconnect_unix_ts",
		"attachment_download_failures_total",
	} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("snapshot missing key %q", key)
		}
	}
	if got, _ := decoded["reconnect_count"].(float64); got != 0 {
		t.Errorf("reconnect_count = %v, want 0", got)
	}
	if got, _ := decoded["last_close_code"].(float64); got != 0 {
		t.Errorf("last_close_code = %v, want 0", got)
	}
	if got, _ := decoded["attachment_download_failures_total"].(float64); got != 0 {
		t.Errorf("attachment_download_failures_total = %v, want 0", got)
	}
}

// TestHealthSnapshot_NilMetrics verifies the helper is nil-safe.
// In tests that don't wire metrics (e.g. early unit tests), the
// adapter's metrics pointer can be nil; HealthSnapshot must not
// panic.
func TestHealthSnapshot_NilMetrics(t *testing.T) {
	a := newTestAdapter(&fakeREST{})
	a.botUserID = "999"
	a.metrics = nil

	_, payload, err := a.HealthSnapshot()
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := decoded["reconnect_count"]; !ok {
		t.Error("snapshot missing reconnect_count key with nil metrics")
	}
}

// TestHealthMetrics_RecordMethods pins the bump semantics: each
// record method mutates the corresponding field and only that
// field. Operators grep these fields in `daemoncontrol` health
// RPCs to diagnose flapping connections — a bump that touches
// the wrong field corrupts the signal.
func TestHealthMetrics_RecordMethods(t *testing.T) {
	m := newHealthMetrics()
	at := time.Unix(1700000000, 0)

	m.recordReconnect(at)
	m.recordReconnect(at)
	m.recordReconnect(at)
	m.recordEvent(at)
	m.recordCloseCode(4004)
	m.recordDownloadFailure()

	snap := m.snapshot()
	if snap.ReconnectCount != 3 {
		t.Errorf("ReconnectCount = %d, want 3", snap.ReconnectCount)
	}
	if !snap.LastEventUnixTS.Equal(at) {
		t.Errorf("LastEventUnixTS = %v, want %v", snap.LastEventUnixTS, at)
	}
	if snap.LastCloseCode != 4004 {
		t.Errorf("LastCloseCode = %d, want 4004", snap.LastCloseCode)
	}
	if !snap.LastReconnectUnixTS.Equal(at) {
		t.Errorf("LastReconnectUnixTS = %v, want %v", snap.LastReconnectUnixTS, at)
	}
	if snap.AttachmentDownloadFails != 1 {
		t.Errorf("AttachmentDownloadFails = %d, want 1", snap.AttachmentDownloadFails)
	}
}

// TestHealthMetrics_RecordCloseCodeIgnoresZero ensures the close
// recorder skips code 0 (which the gateway emits when the read
// loop exits on ctx cancellation rather than a Discord-side
// close frame).
func TestHealthMetrics_RecordCloseCodeIgnoresZero(t *testing.T) {
	m := newHealthMetrics()
	m.recordCloseCode(0)
	m.recordCloseCode(4004)
	if got := m.snapshot().LastCloseCode; got != 4004 {
		t.Errorf("LastCloseCode = %d, want 4004", got)
	}
}

// TestHealthMetrics_NilSafe pins the nil-pointer contract: every
// record method on a nil *healthMetrics must not panic. The
// gatewayClient guards with `if g.metrics != nil` for the
// hot-path metrics, but the helper-level nil-safety is the
// belt-and-braces contract.
func TestHealthMetrics_NilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil-safe method panicked: %v", r)
		}
	}()
	var m *healthMetrics
	m.recordReconnect(time.Now())
	m.recordEvent(time.Now())
	m.recordCloseCode(4004)
	m.recordDownloadFailure()
	// snapshot on nil returns the zero value, not a panic.
	if got := m.snapshot(); got.ReconnectCount != 0 {
		t.Errorf("nil snapshot.ReconnectCount = %d, want 0", got.ReconnectCount)
	}
}
