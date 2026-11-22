package dsh

// The liveness probe.
//
// The shared dsh host exposes session.list — the cheapest
// auth-gated call, independent of session and agent state — which
// the host's own health probe already uses to decide whether the
// host is wedged. The bridge's probe reuses that call: the
// question is whether the host still answers at all, and a dsh
// session can be busy with a turn while the host answers.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

// fakeHost answers session.list with the envelope the host's RPC
// client expects: the rpcId has to come back unchanged, or the
// client treats the answer as a mismatch.
func fakeHost(t *testing.T, ok bool) *host.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session/list" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var request struct {
			RPCID string `json:"rpcId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		result := map[string]any{"ok": ok}
		if ok {
			result["value"] = map[string]any{"items": []any{}}
		} else {
			result["error"] = map[string]any{
				"code": "bad-request", "message": "synthetic", "details": map[string]any{},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":   "server-response",
			"rpcId":  request.RPCID,
			"result": result,
		})
	}))
	t.Cleanup(server.Close)
	return host.New(server.URL, nil)
}

// TestPing_FakeHostAnswers — the probe resolves against a fake host.
func TestPing_FakeHostAnswers(t *testing.T) {
	d := &driver{cli: fakeHost(t, true)}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestPing_AHostThatRefusesFails — a host that answers with a
// failure envelope is not answering: the probe reports it.
func TestPing_AHostThatRefusesFails(t *testing.T) {
	d := &driver{cli: fakeHost(t, false)}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Ping(ctx); err == nil {
		t.Fatal("Ping() = nil, want an error when the host refuses")
	}
}

// TestPing_WithoutAHostFails — a driver with no host client has
// nothing to probe.
func TestPing_WithoutAHostFails(t *testing.T) {
	d := &driver{}
	if err := d.Ping(context.Background()); err == nil {
		t.Fatal("Ping() = nil, want an error without a host client")
	}
}
