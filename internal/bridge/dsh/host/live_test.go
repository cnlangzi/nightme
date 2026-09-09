//go:build live

// live_test.go — end-to-end probe against a running `dsh --profile web`.
//
// Run with:
//
//	NIGHTME_TEST_DSH_URL='http://127.0.0.1:3080/?token=...' \
//	  go test -tags live -run TestLiveDshWireFormat -v ./internal/bridge/dsh/host/...
//
// Skipped (t.Skip) when the env var is unset, so it never blocks CI.
//
// F-dsh-preset-1 (2026-09-09): live-verifies the wire-format fix
// against the real dsh web. The dashboard's actual request shape
// is captured here as the canonical contract; if a future dsh
// release drifts, this test fails before any user-facing bridge
// code regresses.

package host_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveDshWireFormat(t *testing.T) {
	raw := os.Getenv("NIGHTME_TEST_DSH_URL")
	if raw == "" {
		t.Skip("NIGHTME_TEST_DSH_URL not set; live test skipped")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	apiBase := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()

	// Phase 1: warm up — GET the URL once so dsh sets the auth cookie.
	jar, _ := cookiejar.New(nil)
	warm := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	if resp, err := warm.Get(raw); err == nil {
		_ = resp.Body.Close()
	}
	if len(jar.Cookies(u)) == 0 {
		t.Fatalf("warm-up did not set any cookies — dsh auth flow did not run")
	}

	// Phase 2: drive the wire format through a cookie-bearing http client.
	// The RPCClient hard-codes httpclient.Default; we exercise the same
	// envelope manually so cookies flow.
	api := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rpc := func(method string, args map[string]any) (map[string]any, error) {
		// args is the inner body the gateway's `args` field
		// wraps. Caller-built shape — see host.RPCClient.Post
		// doc on the per-method descriptor (typed: `request` /
		// flat: bare fields / none: empty object).
		wrapped, _ := json.Marshal(map[string]any{"args": args})
		body, _ := json.Marshal(map[string]any{
			"type":    "client-request",
			"rpcId":   fmt.Sprintf("live-%d", time.Now().UnixNano()),
			"method":  method,
			"payload": json.RawMessage(wrapped),
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			apiBase+"/api/"+method, strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := api.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
		}
		var env struct {
			Result struct {
				OK    bool            `json:"ok"`
				Value json.RawMessage `json:"value"`
				Error *struct {
					Code, Message string
				} `json:"error"`
			} `json:"result"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return nil, fmt.Errorf("decode envelope: %w", err)
		}
		if !env.Result.OK {
			return nil, fmt.Errorf("server rejected: %s: %s",
				env.Result.Error.Code, env.Result.Error.Message)
		}
		var out map[string]any
		_ = json.Unmarshal(env.Result.Value, &out)
		return out, nil
	}

	// 1) agentPresets/list — confirm we can talk to the server at all
	t.Run("agentPresets/list", func(t *testing.T) {
		out, err := rpc("agentPresets/list", map[string]any{})
		if err != nil {
			t.Fatalf("agentPresets/list: %v", err)
		}
		presets, _ := out["presets"].([]any)
		if len(presets) == 0 {
			t.Fatalf("no presets returned: %v", out)
		}
		for _, p := range presets {
			m := p.(map[string]any)
			t.Logf("preset id=%v trust=%v isDefault=%v",
				m["id"], m["trust"], m["isDefault"])
		}
	})

	// 2) workspace/create — id needed for session/create
	wsPath := fmt.Sprintf("/tmp/nightme-live-%d", time.Now().UnixNano())
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", wsPath, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wsPath) })

	var wsID string
	t.Run("workspace/create", func(t *testing.T) {
		out, err := rpc("workspace/create", map[string]any{
		"request": map[string]any{"path": wsPath},
	})
		if err != nil {
			t.Fatalf("workspace/create: %v", err)
		}
		w, _ := out["workspace"].(map[string]any)
		wsID, _ = w["workspaceId"].(string)
		if wsID == "" {
			t.Fatalf("workspaceId missing: %v", out)
		}
		t.Logf("workspace id=%s", wsID)
	})
	if wsID == "" {
		t.Skip("workspace/create failed; skipping downstream tests")
	}

	// 3) session/create with each preset id, including empty (server default).
	//    Each session is archived at end so we don't pile up rows in dsh.
	t.Run("session/create x presets", func(t *testing.T) {
		for _, preset := range []string{"", "standard", "minimal", "ptc", "cordis"} {
			label := "<server-default>"
			if preset != "" {
				label = preset
			}
			args := map[string]any{"workspaceId": wsID}
			if preset != "" {
				args["agentPreset"] = preset
			}
			out, err := rpc("session/create", map[string]any{
				"request": args,
			})
			if err != nil {
				t.Errorf("preset=%q: %v", label, err)
				continue
			}
			sid, _ := out["sessionId"].(string)
			got, _ := out["agentPreset"].(string)
			t.Logf("preset=%q → sessionId=%s agentPreset=%s",
				label, sid, got)
			if sid == "" {
				t.Errorf("preset=%q: empty sessionId", label)
			}
			if _, err := rpc("workspace/archiveSession", map[string]any{
				"request": map[string]any{"sessionId": sid},
			}); err != nil {
				t.Errorf("archive %s: %v", sid, err)
			}
		}
	})

	// 4) commands/execute — the path the dashboard's "Full access"
	//    picker uses (verified live 2026-09-09 against dsh 0.1.2-rc.1).
	t.Run("commands/execute", func(t *testing.T) {
		// Pick a fresh sid from step 3 via workspace/create +
		// session/create so we don't rely on leaked state.
		out, err := rpc("session/create", map[string]any{
			"request": map[string]any{"workspaceId": wsID},
		})
		if err != nil {
			t.Fatalf("session/create: %v", err)
		}
		sid, _ := out["sessionId"].(string)
		defer func() {
			if sid != "" {
				_, _ = rpc("workspace/archiveSession", map[string]any{
					"request": map[string]any{"sessionId": sid},
				})
			}
		}()

		val, err := rpc("commands/execute", map[string]any{
			"agentId": sid,
			"line":    "/permission danger-full-access",
			"images":  []any{},
		})
		if err != nil {
			t.Fatalf("commands/execute: %v", err)
		}
		result, _ := val["result"].(map[string]any)
		if result == nil {
			t.Fatalf("commands/execute: missing result: %v", val)
		}
		kind, _ := result["kind"].(string)
		if kind != "success" {
			t.Errorf("commands/execute kind=%q (full=%v)", kind, result)
		}
		t.Logf("commands/execute sessionId=%s kind=%s text=%v",
			sid, kind, result["text"])
	})

	// 5) Full stack — session/create then commands/execute with
	//    /permission danger-full-access (matches what newDriver()
	//    does after the wire-format fix).
	t.Run("end-to-end /permission priming", func(t *testing.T) {
		out, err := rpc("session/create", map[string]any{
			"request": map[string]any{"workspaceId": wsID},
		})
		if err != nil {
			t.Fatalf("session/create: %v", err)
		}
		sid, _ := out["sessionId"].(string)
		defer func() {
			if sid != "" {
				_, _ = rpc("workspace/archiveSession", map[string]any{
					"request": map[string]any{"sessionId": sid},
				})
			}
		}()
		val, err := rpc("commands/execute", map[string]any{
			"agentId": sid,
			"line":    "/permission danger-full-access",
			"images":  []any{},
		})
		if err != nil {
			t.Fatalf("commands/execute: %v", err)
		}
		result, _ := val["result"].(map[string]any)
		if result == nil {
			t.Fatalf("commands/execute: missing result: %v", val)
		}
		kind, _ := result["kind"].(string)
		if kind != "success" {
			t.Errorf("commands/execute kind=%q", kind)
		}
		t.Logf("e2e priming OK sessionId=%s result.kind=%s result.text=%v",
			sid, kind, result["text"])
	})
}