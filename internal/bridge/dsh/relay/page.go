// page.go — dsh session/page RPC wrapper.
//
// Background: the legacy bridge called /api/session.history,
// which doesn't exist in dsh 0.1.5-rc.1 (404). The actual history
// surface is the typed session/page endpoint (verified against
// the dsh-api-session-controller typert.host.js —
// `session/page` takes {address, throughSeq, beforeSeq?,
// maxMessages?} and returns {records, hasMore}).
//
// This file wraps that one call. The relay's backfill (see
// backfill.go) is the only caller; drivers reach history through
// Bridge.History.
package relay

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

// pageRequest matches @deepseek-ai/dsh-api-session-controller
// types#SessionPageRequest. Address carries sessionId; throughSeq
// is the cursor the caller has already seen (events with seq ≤
// throughSeq are excluded from the response, so first call uses
// throughSeq=-1 to read from the beginning).
type pageRequest struct {
	Address     sessionAddress `json:"address"`
	ThroughSeq  int64          `json:"throughSeq"`
	BeforeSeq   *int64         `json:"beforeSeq,omitempty"`
	MaxMessages *int           `json:"maxMessages,omitempty"`
}

type sessionAddress struct {
	Kind      string `json:"kind"` // always "session"
	SessionID string `json:"sessionId"`
}

// pageRecord is one event in a page response. We keep the raw
// fields the bridge needs to advance lastSeq and (in a follow-up)
// to translate to agent.AgentEvent; the relay exposes the
// transport, the driver (or future translator inside the relay)
// owns the protocol shape.
type pageRecord struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
}

type pageResponse struct {
	Records []pageRecord `json:"records"`
	HasMore bool         `json:"hasMore"`
}

// fetchPage issues one session/page call. Returns the records
// (ascending seq) and whether more pages remain.
func (r *Relay) fetchPage(ctx context.Context, sessionID string, throughSeq int64, beforeSeq *int64, maxMessages *int) ([]pageRecord, bool, error) {
	req := pageRequest{
		Address:     sessionAddress{Kind: "session", SessionID: sessionID},
		ThroughSeq:  throughSeq,
		BeforeSeq:   beforeSeq,
		MaxMessages: maxMessages,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, false, fmt.Errorf("relay: page: marshal: %w", err)
	}

	// session/page is unary, not stream — go through RPCClient.Post
	// directly (not SessionPrompt / SessionList which use typed
	// helpers built around their own request shape).
	resp, err := r.host.RPC.Post(ctx, "session.page", map[string]any{
		"request": json.RawMessage(body),
	})
	if err != nil {
		return nil, false, fmt.Errorf("relay: session.page: %w", err)
	}
	if !resp.Result.OK {
		return nil, false, fmt.Errorf("relay: session.page rejected: %s", resp.Result.ErrorMessage())
	}
	var out pageResponse
	if err := json.Unmarshal(resp.Result.Value, &out); err != nil {
		return nil, false, fmt.Errorf("relay: session.page decode: %w", err)
	}
	return out.Records, out.HasMore, nil
}

// Compile-time hint that host.RPC is reachable via the
// unqualified Post symbol (the unexported method on
// *RPCClient) — keeps this file's deps honest.
var _ = (*host.RPCClient)(nil)
