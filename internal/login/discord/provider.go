// Package discord implements login.Provider for the Discord channel.
//
// Like Telegram bots, Discord bots are user-created: the operator
// opens the Developer Portal (https://discord.com/developers/applications),
// creates an application, adds a bot, toggles the privileged
// MESSAGE_CONTENT intent on, copies the bot token + OAuth2
// application id, and pastes them back here. There is no
// third-party app-registration API.
//
// Login flow:
//
//  1. Print Developer Portal walkthrough (Discord-specific, no QR).
//  2. Read token + application id from stdin until non-empty.
//  3. Call /users/@me to validate (token is alive, account is bot).
//  4. Return Credentials{BotToken, ApplicationID}.
//
// Greet flow (best-effort, fires AFTER CLI saves the config):
//
//  1. Open a temporary Gateway connection with intent mask
//     (MESSAGE_CONTENT | DIRECT_MESSAGES | DIRECT_MESSAGE_REACTIONS)
//     for at most greetWaitTimeout.
//  2. Wait for the first MESSAGE_CREATE authored by a non-bot
//     user (the bot owner).
//  3. POST /channels/{id}/messages with the canonical greeting
//     bodies. Telegram/Slack use the English copy only — see
//     sendGreeting.
//  4. On timeout: log + exit cleanly. The daemon still answers
//     the owner's first runtime message and never replays the
//     login greeting.
package discord

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/httpclient"
	"github.com/cnlangzi/nightme/internal/login"
	"github.com/gorilla/websocket"
)

const greetWaitTimeout = 15 * time.Second

// greetIntents is the mask sent in the greeting IDENTIFY. The
// full Phase 1 mask is 46593; the greeting window only needs to
// receive direct messages, so the smaller mask keeps the bot
// from accidentally being subscribed to events we will discard.
//
//	1<<12  DIRECT_MESSAGES
//	1<<13  DIRECT_MESSAGE_REACTIONS
//	1<<15  MESSAGE_CONTENT (privileged)
const greetIntents = 4096 + 8192 + 32768

// Options configures a single login flow run. Tests override
// fields; production uses defaults.
type Options struct {
	Out io.Writer
	In  io.Reader

	// HTTPClient is the transport used for /users/@me validation
	// and the greeting POST. nil = default 10s timeout client.
	HTTPClient *http.Client

	// Token bypasses the bot-token stdin prompt when set.
	Token string

	// ApplicationID bypasses the application-id stdin prompt
	// when set. Required for the OAuth URL the CLI prints after
	// login; /users/@me does not return it.
	ApplicationID string

	// Dialer is the WebSocket dialer used by Greet to open the
	// temporary Gateway connection. nil = the gorilla default.
	Dialer *websocket.Dialer
}

// Provider implements login.Provider for Discord.
type Provider struct {
	opts Options
	out  io.Writer
	in   io.Reader
	http *http.Client

	// Captured during Login; consumed by Greet.
	botToken string
	botInfo  *userInfo
}

// New constructs a Provider.
func New(opts Options) *Provider {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	in := opts.In
	if in == nil {
		in = os.Stdin
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = httpclient.DefaultWithTimeout(10 * time.Second)
	}
	return &Provider{
		opts: opts,
		out:  out,
		in:   in,
		http: httpClient,
	}
}

// Name implements login.Provider.
func (p *Provider) Name() string { return "discord" }

// Login implements login.Provider.
func (p *Provider) Login(ctx context.Context) (*login.Credentials, error) {
	token := strings.TrimSpace(p.opts.Token)
	if token == "" {
		p.printInstructions()
		var err error
		token, err = p.readField(ctx, "Bot token (paste from Developer Portal → Bot → Reset Token): ")
		if err != nil {
			return nil, err
		}
	}
	applicationID := strings.TrimSpace(p.opts.ApplicationID)
	if applicationID == "" {
		var err error
		applicationID, err = p.readField(ctx, "Application ID (paste from Developer Portal → General Information): ")
		if err != nil {
			return nil, err
		}
	}
	if applicationID == "" {
		return nil, fmt.Errorf("%w: application_id is required (paste it from the Developer Portal → General Information; /users/@me does not return it)", login.ErrLoginFailed)
	}

	info, err := p.validateToken(ctx, token)
	if err != nil {
		return nil, err
	}
	p.botToken = token
	p.botInfo = info

	fmt.Fprintln(p.out)
	fmt.Fprintf(p.out, "✓ Bot verified!\n")
	fmt.Fprintf(p.out, "  Bot ID:    %s\n", info.ID)
	if info.Username != "" {
		fmt.Fprintf(p.out, "  Username:  %s\n", info.Username)
	}
	fmt.Fprintf(p.out, "  App ID:    %s\n", applicationID)

	displayName := info.Username
	if info.GlobalName != "" {
		displayName = info.GlobalName
	}

	return &login.Credentials{
		BotToken:      token,
		ApplicationID: applicationID,
		AppName:       displayName,
		CreatedAt:     time.Now().UTC(),
	}, nil
}

// Greet implements login.Provider. Best-effort.
func (p *Provider) Greet(ctx context.Context, messages login.GreetingMessages) error {
	if p.botToken == "" {
		return nil
	}

	fmt.Fprintln(p.out)
	fmt.Fprintln(p.out, "📨 Greeting setup")
	fmt.Fprintln(p.out, "-----------------")
	if p.botInfo != nil && p.botInfo.Username != "" {
		fmt.Fprintf(p.out, "Open Discord, search for %s, and send any message.\n", p.botInfo.Username)
	} else {
		fmt.Fprintln(p.out, "Open a DM with your bot and send any message.")
	}
	fmt.Fprintf(p.out, "Waiting up to %s for the first message...\n", greetWaitTimeout)

	waitCtx, cancel := context.WithTimeout(ctx, greetWaitTimeout)
	defer cancel()
	channelID, err := p.waitForFirstMessage(waitCtx)
	if err != nil {
		fmt.Fprintln(p.out)
		fmt.Fprintln(p.out, "Skipped — send the bot any message once the daemon is")
		fmt.Fprintln(p.out, "running (`nightme start`).")
		return nil
	}

	fmt.Fprintf(p.out, "  ✓ Got first DM (channel %s)\n", channelID)
	fmt.Fprintln(p.out, "  Sending greeting...")

	if err := p.sendGreeting(waitCtx, channelID, messages); err != nil {
		return fmt.Errorf("discord: send greeting: %w", err)
	}
	fmt.Fprintln(p.out, "  ✓ Greeting sent")
	return nil
}

func (p *Provider) printInstructions() {
	fmt.Fprintln(p.out, "Discord bot setup")
	fmt.Fprintln(p.out, "==================")
	fmt.Fprintln(p.out)
	fmt.Fprintln(p.out, "Discord bots are user-created via the Developer Portal,")
	fmt.Fprintln(p.out, "not by nightme. To register a bot:")
	fmt.Fprintln(p.out)
	fmt.Fprintln(p.out, "  1. Open https://discord.com/developers/applications")
	fmt.Fprintln(p.out, "     → New Application → name it → Create.")
	fmt.Fprintln(p.out, "  2. Bot tab → Add Bot → confirm.")
	fmt.Fprintln(p.out, "  3. Bot tab → Privileged Gateway Intents → toggle")
	fmt.Fprintln(p.out, "     MESSAGE_CONTENT on (required; without it the bot")
	fmt.Fprintln(p.out, "     can't read message content in guilds).")
	fmt.Fprintln(p.out, "  4. Bot tab → Reset Token → copy the token.")
	fmt.Fprintln(p.out, "  5. General Information → copy Application ID.")
	fmt.Fprintln(p.out)
}

// readField reads one non-empty line, honouring ctx.
func (p *Provider) readField(ctx context.Context, label string) (string, error) {
	fmt.Fprint(p.out, label)
	scanner := bufio.NewScanner(p.in)
	scanner.Buffer(make([]byte, 0, 256), 4096)
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		for scanner.Scan() {
			text := strings.TrimSpace(scanner.Text())
			if text != "" {
				done <- result{line: text}
				return
			}
		}
		done <- result{err: scanner.Err()}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return "", fmt.Errorf("discord: read field: %w", r.err)
		}
		return r.line, nil
	case <-ctx.Done():
		return "", fmt.Errorf("discord: %w: %v", login.ErrLoginTimeout, ctx.Err())
	}
}

// userInfo mirrors the subset of /users/@me we display.
type userInfo struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name,omitempty"`
	Bot        bool   `json:"bot,omitempty"`
}

func (p *Provider) validateToken(ctx context.Context, token string) (*userInfo, error) {
	endpoint := "https://discord.com/api/v10/users/@me"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("discord: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/cnlangzi/nightme)")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", login.ErrLoginFailed, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("discord: read getMe: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: getMe status %d: %s",
			login.ErrLoginFailed, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var info userInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("%w: decode getMe: %v", login.ErrLoginFailed, err)
	}
	if !info.Bot {
		return nil, fmt.Errorf("%w: token does not belong to a bot account", login.ErrLoginFailed)
	}
	return &info, nil
}

// waitForFirstMessage opens a temporary Gateway connection
// listening for the first MESSAGE_CREATE authored by a non-bot
// user. On success returns the channel id the message arrived
// in; the caller posts the greeting there.
//
// Mirrors telegram.waitForFirstMessage in shape (poll-until-event
// or deadline). The transport differs (WebSocket vs long-poll),
// but the contract is identical: surface a chat id or a
// deadline-typed error after greetWaitTimeout.
func (p *Provider) waitForFirstMessage(ctx context.Context) (string, error) {
	dialer := p.opts.Dialer
	if dialer == nil {
		dialer = &websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	}

	// Use the public gateway URL for the greeting window — we
	// don't have a saved session yet, so RESUME doesn't apply.
	gatewayURL := "wss://gateway.discord.gg/?v=10&encoding=json"
	headers := http.Header{}
	headers.Set("User-Agent", "DiscordBot (https://github.com/cnlangzi/nightme)")

	ws, _, err := dialer.DialContext(ctx, gatewayURL, headers)
	if err != nil {
		return "", fmt.Errorf("discord: greeting dial: %w", err)
	}
	defer ws.Close()

	// Read op=10 HELLO.
	var hello struct {
		Op int `json:"op"`
		D  struct {
			HeartbeatInterval int `json:"heartbeat_interval"`
		} `json:"d"`
	}
	if err := ws.ReadJSON(&hello); err != nil {
		return "", fmt.Errorf("discord: greeting hello: %w", err)
	}
	if hello.Op != 10 {
		return "", fmt.Errorf("discord: greeting: expected op=10, got op=%d", hello.Op)
	}

	// Send op=2 IDENTIFY. Discord does not require a heartbeat
	// during the greeting window — the connection will be torn
	// down before the first interval elapses — but we send a
	// heartbeat opportunistically on a timer to avoid the 30s
	// gateway-side idle disconnect for users who wait the full
	// 15s.
	identifyBody, _ := json.Marshal(map[string]any{
		"token":      p.botToken,
		"intents":    greetIntents,
		"properties": map[string]string{"os": "linux", "browser": "nightme-greet", "device": "nightme-greet"},
	})
	if err := ws.WriteJSON(map[string]any{"op": 2, "d": json.RawMessage(identifyBody)}); err != nil {
		return "", fmt.Errorf("discord: greeting identify: %w", err)
	}

	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		interval := time.Duration(hello.D.HeartbeatInterval) * time.Millisecond
		if interval <= 0 {
			interval = 30 * time.Second
		}
		t := time.NewTimer(interval)
		defer t.Stop()
		for {
			select {
			case <-heartbeatStop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_ = ws.WriteJSON(map[string]any{"op": 1, "d": 0})
				t.Reset(interval)
			}
		}
	}()
	defer func() {
		close(heartbeatStop)
		<-heartbeatDone
	}()

	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return "", fmt.Errorf("timeout: no message received within %s", greetWaitTimeout)
			}
			return "", err
		}
		var frame struct {
			Op int             `json:"op"`
			D  json.RawMessage `json:"d"`
			T  string          `json:"t"`
		}
		if err := ws.ReadJSON(&frame); err != nil {
			return "", fmt.Errorf("discord: greeting read: %w", err)
		}
		if frame.Op != 0 || frame.T != "MESSAGE_CREATE" {
			continue
		}
		var msg struct {
			Author struct {
				ID  string `json:"id"`
				Bot bool   `json:"bot"`
			} `json:"author"`
			ChannelID string `json:"channel_id"`
		}
		if err := json.Unmarshal(frame.D, &msg); err != nil {
			continue
		}
		if msg.Author.Bot || msg.Author.ID == "" {
			continue
		}
		return msg.ChannelID, nil
	}
}

// sendGreeting fires each English greeting body via
// POST /channels/{id}/messages. Same "English only" policy
// telegram / slack use (Discord has no Feishu-style bilingual
// post block).
func (p *Provider) sendGreeting(ctx context.Context, channelID string, messages login.GreetingMessages) error {
	for index, body := range messages {
		if body.English == "" {
			continue
		}
		endpoint := "https://discord.com/api/v10/channels/" + url.PathEscape(channelID) + "/messages"
		payload, _ := json.Marshal(map[string]any{"content": body.English})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
		if err != nil {
			return fmt.Errorf("body %d: %w", index, err)
		}
		req.Header.Set("Authorization", "Bot "+p.botToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "DiscordBot (https://github.com/cnlangzi/nightme)")
		resp, err := p.http.Do(req)
		if err != nil {
			return fmt.Errorf("body %d send: %w", index, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("body %d send: status %d: %s", index, resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
	return nil
}
