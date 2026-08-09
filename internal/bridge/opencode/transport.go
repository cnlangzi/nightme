// SSE event stream reader for the opencode server's
// GET /api/session/{id}/event endpoint.
//
// opencode emits one JSON event per SSE frame:
//
//	event: message
//	data: {"type":"message.part.updated","properties":{"part": ...}}
//
// (Multi-line event: comment lines and field lines are possible per
// the SSE spec; we only care about `data:`.) Frames are separated by
// a blank line. Some servers also emit `id:` and `retry:` fields which
// we ignore.
//
// Failure semantics:
//   - Network read error: returned to the caller; the events channel
//     is closed by the lifecycle goroutine via the package's normal
//     paths.
//   - One bad event: we log and continue, not close. A single
//     wire-level error is not a reason to die; the opencode server
//     can recover from a missed frame.
package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// SessionEvent is the discriminated union the opencode server emits on
// the SSE stream. We keep `type` and the payload as raw JSON so the
// translator can dispatch on `type` without us re-typing every shape.
//
// opencode has two slightly different envelope shapes:
//
//	per-session: {"type":"...","properties":{...}}
//	global:      {"type":"...","data":{...}}
//
// We normalise both into Properties. The translator dispatches on
// Type without caring about which endpoint produced the event.
type SessionEvent struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
	Data       json.RawMessage `json:"data"`
}

// properties returns the effective payload regardless of whether
// the event came from the per-session or global stream. The
// decoder maps both into the same field so the translator can
// stay simple.
func (e SessionEvent) properties() json.RawMessage {
	if len(e.Properties) > 0 {
		return e.Properties
	}
	return e.Data
}

// Subscribe opens the SSE stream for an opencode session and returns
// an io.ReadCloser the caller can read events from. The caller is
// responsible for closing the body when done.
//
// Implementation note: opencode 1.18.x's per-session endpoint
// (/api/session/{id}/event) returns 500 ServeError because the
// EventV2Bridge.Service layer is not available without an
// active InstanceContext. We fall back to the global event
// stream (/api/event) which is always wired and routes events
// server-wide; the bridge's `x-opencode-directory` header is
// already set by newRequest so the server-side event filter
// applies (per-session events include the location field, the
// global stream includes them too).
//
// The returned io.ReadCloser is the underlying response body. We do
// not parse it here — the caller passes the body to decodeSSE.
func (c *Client) Subscribe(ctx context.Context, sessionID string) (io.ReadCloser, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("opencode: empty session id")
	}
	_ = sessionID // sessionID is informational; the global stream
	// is filtered by the x-opencode-directory header we always set.
	return c.subscribeGlobal(ctx)
}

// subscribeGlobal connects to /api/event, the server-wide SSE
// stream. Used because the per-session endpoint is broken in
// opencode 1.18.x (it returns 500 ServeError).
func (c *Client) subscribeGlobal(ctx context.Context) (io.ReadCloser, error) {
	req, err := c.newRequest(ctx, "GET", "/api/event", nil)
	if err != nil {
		return nil, err
	}
	// SSE; do not let the http client auto-decompress.
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode: subscribe: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("opencode: subscribe: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Body, nil
}

// pathEscape is a tiny wrapper so the rest of the file does not need
// to import net/url just for one call.
func pathEscape(s string) string {
	// opencode session IDs are URL-safe (alphanumeric + dash), but
	// path-escape defensively so a malformed id never crashes the
	// request.
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '~':
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}

// sseHandler is the per-event callback signature passed to decodeSSE.
// Returning a non-nil error short-circuits the stream.
type sseHandler func(ev SessionEvent) error

// decodeSSE reads r as an SSE stream and invokes onEvent for each
// successfully parsed event. It returns nil on graceful EOF (the
// upstream closed the stream) and the underlying read error otherwise.
func decodeSSE(r io.Reader, onEvent sseHandler) error {
	scanner := bufio.NewScanner(r)
	// opencode can emit fairly large message-part.updated payloads
	// (full tool argument dumps). 10 MiB matches the codex
	// MaxFrameSize and is generous without being unbounded.
	scanner.Buffer(make([]byte, 0, 64*1024), sseBufferSize)

	var dataBuf strings.Builder
	flush := func() error {
		if dataBuf.Len() == 0 {
			return nil
		}
		payload := dataBuf.String()
		dataBuf.Reset()
		var ev SessionEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			oLog("sse decode error", "err", err.Error(), "payload", truncateForLog(payload))
			return nil // skip bad event, keep stream alive
		}
		if ev.Type == "" {
			return nil
		}
		return onEvent(ev)
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			// End of one SSE frame.
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "data:"):
			// SSE allows multiple `data:` lines per frame,
			// concatenated with "\n". For opencode this is
			// never used, but handle defensively.
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(strings.TrimPrefix(line, "data:"))
			// Allow a single leading space per SSE spec.
			if s := dataBuf.String(); len(s) > 0 && s[0] == ' ' {
				dataBuf.Reset()
				dataBuf.WriteString(strings.TrimPrefix(s, " "))
			}
		case strings.HasPrefix(line, ":") || strings.HasPrefix(line, "id:") ||
			strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "retry:"):
			// Standard SSE fields we ignore.
		default:
			// Unknown line — ignore so the server can grow the
			// protocol without breaking us.
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("opencode: sse read: %w", err)
	}
	// Flush any trailing frame at EOF.
	_ = flush()
	return nil
}

// truncateForLog bounds the size of a payload we dump into a debug log
// so a multi-MB event does not blow up the log file.
func truncateForLog(s string) string {
	const max = 256
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
