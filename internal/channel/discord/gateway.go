package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// gatewaySnapshot bundles the fields the gateway needs from
// config so the gateway package doesn't import internal/config.
type gatewaySnapshot struct {
	Token     string
	Intents   int
	UserAgent string
}

// gatewayClient owns the WebSocket connection to Discord's
// Gateway. The adapter calls Run once at Start; everything else
// happens on goroutines inside Run.
type gatewayClient struct {
	logger *slog.Logger
	api    restClient
	state  *stateStore
	cfg    gatewaySnapshot

	// Callbacks fired from the read loop. onReady receives the
	// User object Discord returned in READY (so the adapter can
	// cache botUserID); onMessage receives every MESSAGE_CREATE.
	onReady   func(User)
	onMessage func(*Message)
}

// errInvalidSession is returned by run when Discord replies with
// op=9 Invalid Session and d=false. The caller uses this to
// clear the persisted session so the reconnect falls back to
// IDENTIFY rather than looping on RESUME.
var errInvalidSession = errors.New("discord gateway: invalid session")

// run is the long-lived Gateway loop. It dials, identifies (or
// resumes), drives the heartbeat ticker, and dispatches events
// until ctx is cancelled or a non-recoverable close code arrives.
//
// Returns nil on a clean shutdown (ctx cancelled), or a non-nil
// error on terminal failures (close codes 4004 / 4013 / 4014).
// Transient close codes trigger an internal reconnect loop that
// does NOT return to the caller — caller observes a successful
// Start.
func (g *gatewayClient) run(ctx context.Context) error {
	logger := g.logger
	if logger == nil {
		logger = slog.Default()
	}
	backoff := 500 * time.Millisecond
	const maxBackoff = 30 * time.Second

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		sessionID, lastSeq, resumeURL, persistedVersion := g.state.snapshot()
		var gatewayURL string
		var resumeMode bool
		if sessionID != "" && persistedVersion == intentsVersion && lastSeq > 0 {
			gatewayURL = resumeURL
			resumeMode = true
		}
		if gatewayURL == "" {
			gb, err := g.api.GetGatewayBot(ctx)
			if err != nil {
				logger.Warn("discord gateway: get gateway bot failed; backing off",
					"err", err.Error(), "backoff_ms", backoff.Milliseconds())
				if !sleepCtx(ctx, backoff) {
					return nil
				}
				backoff = nextBackoff(backoff, maxBackoff)
				continue
			}
			gatewayURL = gb.URL
			if gatewayURL == "" {
				gatewayURL = "wss://gateway.discord.gg"
			}
		}

		terminal, code, err := g.connectOnce(ctx, gatewayURL, resumeMode, sessionID, lastSeq)
		if terminal {
			return fmt.Errorf("discord gateway: terminal close code %d: %w", code, err)
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && !errors.Is(err, errInvalidSession) && !errors.Is(err, context.Canceled) {
			logger.Warn("discord gateway: connection ended; reconnecting",
				"err", err.Error(), "backoff_ms", backoff.Milliseconds())
		}
		_ = code
		if !sleepCtx(ctx, backoff) {
			return nil
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// connectOnce dials the Gateway, performs IDENTIFY (or RESUME),
// and runs the read loop until the connection ends. Returns
// (terminal, closeCode, err).
func (g *gatewayClient) connectOnce(ctx context.Context, gatewayURL string, resume bool, sessionID string, lastSeq int64) (bool, int, error) {
	logger := g.logger
	if logger == nil {
		logger = slog.Default()
	}

	u, err := url.Parse(gatewayURL)
	if err != nil {
		return false, 0, fmt.Errorf("discord gateway: parse url %q: %w", gatewayURL, err)
	}
	q := u.Query()
	q.Set("v", "10")
	q.Set("encoding", "json")
	u.RawQuery = q.Encode()

	headers := http.Header{}
	if g.cfg.UserAgent != "" {
		headers.Set("User-Agent", g.cfg.UserAgent)
	}

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := dialer.DialContext(ctx, u.String(), headers)
	if err != nil {
		return false, 0, fmt.Errorf("discord gateway: dial: %w", err)
	}
	defer ws.Close()

	// Per-connection write mutex: heartbeat goroutine + any
	// future writers serialise on this.
	var writeMu sync.Mutex
	writeJSON := func(payload GatewayPayload) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return ws.WriteJSON(payload)
	}

	// First frame: op=10 HELLO.
	var hello Hello
	if err := readJSON(ctx, ws, &hello); err != nil {
		return false, 0, fmt.Errorf("discord gateway: read hello: %w", err)
	}
	if hello.HeartbeatInterval <= 0 {
		return false, 0, errors.New("discord gateway: hello missing heartbeat_interval")
	}

	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go heartbeatLoop(ctx, ws, &writeMu, hello.HeartbeatInterval, heartbeatStop, heartbeatDone)

	if resume {
		body, _ := json.Marshal(Resume{
			Token:     g.cfg.Token,
			SessionID: sessionID,
			Seq:       lastSeq,
		})
		if err := writeJSON(GatewayPayload{Op: 6, D: body}); err != nil {
			close(heartbeatStop)
			<-heartbeatDone
			return false, 0, fmt.Errorf("discord gateway: send resume: %w", err)
		}
	} else {
		identify := Identify{
			Token:   g.cfg.Token,
			Intents: g.cfg.Intents,
			Properties: IdentifyProperties{
				OS:      runtime.GOOS,
				Browser: "nightme",
				Device:  "nightme",
			},
		}
		body, _ := json.Marshal(identify)
		if err := writeJSON(GatewayPayload{Op: 2, D: body}); err != nil {
			close(heartbeatStop)
			<-heartbeatDone
			return false, 0, fmt.Errorf("discord gateway: send identify: %w", err)
		}
	}

	terminal, closeCode, readErr := g.readLoop(ctx, ws)
	close(heartbeatStop)
	<-heartbeatDone

	if errors.Is(readErr, errInvalidSession) {
		_ = g.state.clear()
	}
	return terminal, closeCode, readErr
}

// readLoop is the per-connection dispatch loop. Returns when:
//   - ctx is cancelled (terminal=false, code=0, err=nil)
//   - the WebSocket closes cleanly (terminal=false, code=closeCode, err=nil)
//   - a terminal close code (4004 / 4013 / 4014) is observed (terminal=true)
//   - the read errors (terminal=false, code=0, err=<wrapped>)
//   - op=9 Invalid Session with d=false (terminal=false, code=0, err=errInvalidSession)
func (g *gatewayClient) readLoop(ctx context.Context, ws *websocket.Conn) (bool, int, error) {
	logger := g.logger
	if logger == nil {
		logger = slog.Default()
	}

	for {
		if err := ctx.Err(); err != nil {
			return false, 0, nil
		}
		var frame GatewayPayload
		if err := readJSON(ctx, ws, &frame); err != nil {
			closeCode := extractCloseCode(ws, err)
			if closeCode != 0 {
				if isTerminalCloseCode(closeCode) {
					return true, closeCode, fmt.Errorf("close code %d", closeCode)
				}
				return false, closeCode, nil
			}
			return false, 0, err
		}

		if frame.S != nil {
			_ = g.state.setSeq(*frame.S)
		}

		switch frame.Op {
		case 0: // Dispatch
			switch frame.T {
			case "READY":
				var ready Ready
				if err := json.Unmarshal(frame.D, &ready); err != nil {
					logger.Warn("discord gateway: decode READY", "err", err.Error())
					continue
				}
				if ready.SessionID != "" {
					_ = g.state.setSession(ready.SessionID, ready.ResumeGatewayURL)
				}
				logger.Info("discord gateway: READY",
					"session_id", ready.SessionID,
					"bot_id", string(ready.User.ID),
				)
				if g.onReady != nil {
					g.onReady(ready.User)
				}
			case "MESSAGE_CREATE":
				var msg Message
				if err := json.Unmarshal(frame.D, &msg); err != nil {
					logger.Warn("discord gateway: decode MESSAGE_CREATE", "err", err.Error())
					continue
				}
				if g.onMessage != nil {
					g.onMessage(&msg)
				}
			case "RESUMED":
				logger.Info("discord gateway: RESUMED")
			default:
				// Other dispatch events (GUILD_CREATE,
				// MESSAGE_REACTION_ADD, …) are not consumed in
				// Phase 1; silently drop.
			}
		case 7: // Reconnect
			return false, 0, nil
		case 9: // Invalid Session
			var resummable bool
			_ = json.Unmarshal(frame.D, &resummable)
			if !resummable {
				return false, 0, errInvalidSession
			}
			return false, 0, nil
		case 11: // Heartbeat ACK — nothing to do
		default:
			// Unknown opcode — ignore (Discord may add new ones).
		}
	}
}

// heartbeatLoop sends op=1 Heartbeat at the interval Discord
// returned in HELLO, with a jittered first beat.
func heartbeatLoop(ctx context.Context, ws *websocket.Conn, writeMu *sync.Mutex, intervalMs int, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	firstDelay := time.Duration(rand.Int63n(int64(intervalMs))) * time.Millisecond
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	interval := time.Duration(intervalMs) * time.Millisecond

	sendBeat := func() error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		body, _ := json.Marshal(map[string]any{"d": int64(0)})
		return ws.WriteJSON(GatewayPayload{Op: 1, D: body})
	}

	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := sendBeat(); err != nil {
				return
			}
			timer.Reset(interval)
		}
	}
}

// readJSON reads the next text frame and decodes into out. Honours
// ctx cancellation by closing the underlying connection, which
// causes ReadJSON to return promptly.
func readJSON(ctx context.Context, ws *websocket.Conn, out any) error {
	if ws == nil {
		return errors.New("discord gateway: nil connection")
	}
	_ = ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	type result struct {
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ch <- result{err: ws.ReadJSON(out)}
	}()
	select {
	case r := <-ch:
		return r.err
	case <-ctx.Done():
		_ = ws.Close()
		return ctx.Err()
	}
}

// extractCloseCode pulls the WebSocket close code out of an
// error returned from ReadJSON, returning 0 when the error isn't
// a typed close error.
func extractCloseCode(ws *websocket.Conn, err error) int {
	if err == nil {
		return 0
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return closeErr.Code
	}
	return 0
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
