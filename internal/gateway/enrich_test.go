package gateway

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/session"
)

// TestEnrichOutboundMeta_OutInit covers the session-metadata
// enrichment path used by translateAndSend to stamp agent_name
// and workspace onto the OutboundMessage Meta so the channel can
// render the receipt footer. Other outbound kinds must NOT be
// touched (the translator's Meta keys belong to it; the gateway
// shouldn't pollute them with session-level fields that have no
// meaning for non-init events).
func TestEnrichOutboundMeta_OutInit(t *testing.T) {
	cases := []struct {
		name     string
		session  *session.Session
		input    OutboundMessage
		wantKeys map[string]string
	}{
		{
			name:    "all fields present",
			session: &session.Session{Agent: "claude", Workspace: "/code/nightme"},
			input: OutboundMessage{
				Kind: OutInit,
				Meta: map[string]any{"model": "MiniMax-M3", "session_id": "abc"},
			},
			wantKeys: map[string]string{
				"agent_name": "claude",
				"workspace":  "/code/nightme",
				"model":      "MiniMax-M3",
			},
		},
		{
			name:    "empty session fields dropped",
			session: &session.Session{},
			input: OutboundMessage{
				Kind: OutInit,
				Meta: map[string]any{"model": "MiniMax-M3"},
			},
			wantKeys: map[string]string{
				"model": "MiniMax-M3",
			},
		},
		{
			name:    "preserves existing meta keys",
			session: &session.Session{Agent: "codex", Workspace: "/code/pangolin"},
			input: OutboundMessage{
				Kind: OutInit,
				Meta: map[string]any{
					"session_id": "xyz",
					"model":      "gpt-5",
					// Existing agent_name wins (don't clobber).
					"agent_name": "preset",
				},
			},
			wantKeys: map[string]string{
				"agent_name": "preset",
				"workspace":  "/code/pangolin",
				"session_id": "xyz",
				"model":      "gpt-5",
			},
		},
		{
			name:    "non-init kind passes through untouched",
			session: &session.Session{Agent: "claude", Workspace: "/code/nightme"},
			input: OutboundMessage{
				Kind: OutText,
				Meta: map[string]any{"model": "MiniMax-M3"},
			},
			wantKeys: map[string]string{
				"model": "MiniMax-M3",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enrichOutboundMeta(tc.input, tc.session)
			for k, want := range tc.wantKeys {
				got, ok := tc.input.Meta[k]
				if !ok {
					t.Errorf("Meta[%q] missing", k)
					continue
				}
				if got != want {
					t.Errorf("Meta[%q] = %v, want %v", k, got, want)
				}
			}
		})
	}
}

// TestEnrichOutboundMeta_NilSafety covers the defensive paths
// where the caller hands us a nil session or a Meta-less
// OutboundMessage — the enrichment is a no-op rather than a
// panic.
func TestEnrichOutboundMeta_NilSafety(t *testing.T) {
	cases := []struct {
		name    string
		session *session.Session
		out     OutboundMessage
	}{
		{"nil session", nil, OutboundMessage{Kind: OutInit, Meta: map[string]any{"model": "x"}}},
		{"nil meta", &session.Session{Agent: "claude"}, OutboundMessage{Kind: OutInit}},
		{"both nil", nil, OutboundMessage{Kind: OutInit}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enrichOutboundMeta(tc.out, tc.session)
		})
	}
}

// TestReceiptsForSession_FanOut verifies that translateAndSend's
// per-session fan-out produces one OutboundMessage per bound
// receipt, each anchored to its own userMsgID. The gateway's
// 1 request : n response model means each userMsgID gets its own
// ReplyTo even though the underlying agent event is one shared
// stream.
func TestReceiptsForSession_FanOut(t *testing.T) {
	gw := &gateway{}

	// Register three receipts bound to one session; a fourth
	// bound to a different session; the receipts map is
	// unexported so we use the package-internal access via
	// gateway.mu / gateway.receipts.
	gw.mu.Lock()
	gw.receipts = map[string]*receiptEntry{
		"om_a": {sessionID: "sess-1"},
		"om_b": {sessionID: "sess-1"},
		"om_c": {sessionID: "sess-1"},
		"om_d": {sessionID: "sess-2"},
	}
	gw.mu.Unlock()

	got := gw.receiptsForSession("sess-1")
	if len(got) != 3 {
		t.Errorf("sess-1 fan-out = %v, want 3 entries (om_a, om_b, om_c)", got)
	}
	for _, want := range []string{"om_a", "om_b", "om_c"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("sess-1 fan-out missing %q; got %v", want, got)
		}
	}
	if got := gw.receiptsForSession("sess-2"); len(got) != 1 || got[0] != "om_d" {
		t.Errorf("sess-2 fan-out = %v, want [om_d]", got)
	}
	if got := gw.receiptsForSession("nonexistent"); len(got) != 0 {
		t.Errorf("nonexistent session = %v, want empty", got)
	}
}
