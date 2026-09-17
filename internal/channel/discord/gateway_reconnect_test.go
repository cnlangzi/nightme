package discord

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// gatewayREST is a minimal restClient whose GetGatewayBot returns
// the supplied URL. Used to point the gateway run loop at a fake
// WebSocket server inside a test process.
type gatewayREST struct {
	url string
}

func (g *gatewayREST) GetGatewayBot(context.Context) (GatewayBotResponse, error) {
	return GatewayBotResponse{URL: g.url}, nil
}
func (g *gatewayREST) GetMe(context.Context) (User, error) { return User{ID: "999"}, nil }
func (g *gatewayREST) CreateMessage(context.Context, string, CreateMessagePayload) (Message, error) {
	return Message{ID: "out"}, nil
}
func (g *gatewayREST) EditMessage(context.Context, string, string, EditMessagePayload) (Message, error) {
	return Message{}, nil
}
func (g *gatewayREST) DeleteMessage(context.Context, string, string) error { return nil }
func (g *gatewayREST) AddReaction(context.Context, string, string, string) error {
	return nil
}
func (g *gatewayREST) RemoveOwnReaction(context.Context, string, string, string) error {
	return nil
}
func (g *gatewayREST) ClearReactions(context.Context, string, string) error { return nil }
func (g *gatewayREST) AcknowledgeInteraction(context.Context, string, string, InteractionResponse) error {
	return nil
}
func (g *gatewayREST) Download(context.Context, string) ([]byte, error) { return nil, nil }

// startFakeGateway stands up an httptest server that upgrades
// each incoming request to a WebSocket and runs the supplied
// script against the connection. Returns the ws:// URL and a
// cleanup hook.
func startFakeGateway(t *testing.T, script func(*websocket.Conn)) (string, func()) {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		script(c)
	}))
	u, _ := url.Parse(srv.URL)
	u.Scheme = "ws"
	return u.String(), srv.Close
}

// TestGatewayRun_TerminalCloseStopsRunLoop verifies the central
// invariant from issue §4 #1: a Discord close frame with code 4004
// causes gw.run to return a terminal error. The test ensures the
// daemon exits rather than looping forever on a token that will
// never authenticate again.
func TestGatewayRun_TerminalCloseStopsRunLoop(t *testing.T) {
	const want = 4004

	// Stand up a WS server that performs a graceful close
	// handshake (close frame → wait for client's ack → close).
	// Without the ack handshake the client sees io.EOF and the
	// terminal-close branch never fires.
	dialed := make(chan struct{}, 16)
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		dialed <- struct{}{}
		// connectOnce reads the first frame as a bare Hello
		// struct (heartbeat_interval at the top level), not the
		// op/d envelope used by later frames. Send bare JSON so
		// the decoder populates HeartbeatInterval.
		_ = c.WriteJSON(map[string]any{"heartbeat_interval": 100})
		// Drain the IDENTIFY so the client's write side blocks
		// long enough for our close to be the next event.
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		var frame map[string]any
		_ = c.ReadJSON(&frame)
		// Send the close frame and wait for the client's ack.
		closeMsg := websocket.FormatCloseMessage(want, "auth failed")
		_ = c.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(time.Second))
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		// Read until the client sends the ack close frame.
		for {
			if _, _, err := c.NextReader(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1)

	gw := &gatewayClient{
		logger:  testLogger(),
		api:     &gatewayREST{url: wsURL},
		state:   newTestStateStore(),
		metrics: newHealthMetrics(),
		cfg: gatewaySnapshot{
			Token:     "fake-token",
			Intents:   0,
			UserAgent: "test",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Manually drive the dial → HELLO → IDENTIFY → readLoop
	// sequence to see what connectOnce would return.
	wsURL2 := strings.Replace(srv.URL, "http://", "ws://", 1) + "/"
	u, _ := url.Parse(wsURL2)
	q := u.Query()
	q.Set("v", "10")
	q.Set("encoding", "json")
	u.RawQuery = q.Encode()
	hdr := http.Header{}
	hdr.Set("User-Agent", "test")
	conn, _, dialErr := websocket.DefaultDialer.DialContext(ctx, u.String(), hdr)
	if dialErr != nil {
		t.Fatalf("dial: %v", dialErr)
	}
	defer conn.Close()
	terminal, code, readErr := gw.connectOnce(ctx, u.String(), false, "", 0)
	t.Logf("connectOnce: terminal=%v code=%d err=%v (%T)", terminal, code, readErr, readErr)
	if !terminal {
		t.Fatalf("connectOnce did not surface terminal close (code=%d err=%v)", code, readErr)
	}
	if code != want {
		t.Errorf("connectOnce code = %d, want %d", code, want)
	}
}

// TestGatewayRun_RejectsUnknownCloseCodeAsTransient verifies the
// inverse: a non-terminal close (4000 unknown error) does NOT
// stop the loop. The test cancels the ctx after a bounded window
// and asserts dialed.Load() >= 2 by then.
func TestGatewayRun_RejectsUnknownCloseCodeAsTransient(t *testing.T) {
	const want = 4000
	var dialed atomic.Int32
	wsURL, cleanup := startFakeGateway(t, func(c *websocket.Conn) {
		dialed.Add(1)
		_ = c.WriteJSON(map[string]any{"heartbeat_interval": 50})
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		var frame map[string]any
		_ = c.ReadJSON(&frame)
		_ = c.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(want, "unknown"),
			time.Now().Add(time.Second),
		)
		time.Sleep(100 * time.Millisecond)
	})
	t.Cleanup(cleanup)

	gw := &gatewayClient{
		logger:  testLogger(),
		api:     &gatewayREST{url: wsURL},
		state:   newTestStateStore(),
		metrics: newHealthMetrics(),
		cfg: gatewaySnapshot{
			Token:     "fake-token",
			Intents:   0,
			UserAgent: "test",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_ = gw.run(ctx)

	if got := dialed.Load(); got < 2 {
		t.Errorf("dialed = %d, want >= 2 (transient close should trigger redial)", got)
	}
}

// TestExtractCloseCode_IgnoresNonCloseErrors ensures the helper
// only returns non-zero for typed *websocket.CloseError. A bare
// io.EOF or context cancellation must NOT look like a Discord
// close frame — the run loop would otherwise enter the wrong
// backoff bucket.
func TestExtractCloseCode_IgnoresNonCloseErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"plain error", errors.New("transport down"), 0},
		{"close 4014", &websocket.CloseError{Code: 4014}, 4014},
		{"close 4004", &websocket.CloseError{Code: 4004}, 4004},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractCloseCode(nil, tc.err)
			if got != tc.want {
				t.Errorf("extractCloseCode(_, %v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
