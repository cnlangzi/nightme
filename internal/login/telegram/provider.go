// Package telegram implements login.Provider for the Telegram channel.
//
// Unlike Feishu (where nightme registers a brand-new app via a
// device-authorization QR flow) Telegram bots are user-created:
// the operator opens a chat with @BotFather in the Telegram app,
// runs /newbot, copies the issued HTTP API token, and pastes it
// back here. There is no equivalent of Feishu's registration API
// and no QR code.
//
// Login flow:
//
//   1. Print BotFather walkthrough (Telegram-specific, no QR).
//   2. Read token from stdin until non-empty.
//   3. Call getMe to validate (token is alive, account is bot).
//   4. Return Credentials{BotToken: ...}.
//
// Greet flow (best-effort, fires AFTER CLI saves the config):
//
//   1. Tell user to message the bot ("send /start").
//   2. Poll getUpdates with a 2-minute window for the first
//      private-chat message from any non-bot user.
//   3. Send the canonical bilingual greeting bodies to that
//      chat_id via sendMessage.
//   4. On timeout: log and exit — the greeting is simply
//      skipped. The daemon only answers runtime messages; it
//      never replays the login greeting.
//
// Greet's context is the parent ctx set by the CLI (10-minute
// login timeout). The greeting wait caps itself at 2 minutes so
// a user who walked away doesn't keep the CLI open longer than
// necessary.
//
// See docs/channel/telegram.md §10.2 and §11.2 for the user-facing
// onboarding steps this provider mirrors.
package telegram

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
	"strconv"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/login"
	"github.com/cnlangzi/nightme/internal/httpclient"
)

// greetWaitTimeout caps how long Greet waits for the user's first
// message. Generous enough that someone reading the CLI output,
// picking up their phone, opening Telegram, and finding the bot
// can complete the gesture; tight enough that a user who walked
// away doesn't keep the CLI blocked.
const greetWaitTimeout = 2 * time.Minute

// longPollSeconds is the long-polling timeout sent to getUpdates.
// Telegram holds the connection open for up to this many seconds
// before returning an empty result. 25 s is below Telegram's 30 s
// default and well above the per-call network jitter observed in
// production.
const longPollSeconds = 25

// Options configures a single login flow run. Tests override Out
// and In to feed canned answers; production uses os.Stdout /
// os.Stdin.
type Options struct {
	// Out is where instructions and status go. nil = os.Stdout.
	Out io.Writer
	// In is where the bot token is read from. nil = os.Stdin.
	In io.Reader
	// HTTPClient is the transport used for getMe validation.
	// nil = http.DefaultClient with a 10s timeout (which honours
	// HTTP_PROXY / HTTPS_PROXY / NO_PROXY env vars).
	HTTPClient *http.Client
	// Token bypasses the stdin prompt when set. Used by the CLI
	//  flag for non-interactive ERPL / shell-wrapped
	// invocations. Empty means read from In.
	Token string
}

// Provider implements login.Provider for Telegram.
type Provider struct {
	opts Options
	out  io.Writer
	in   io.Reader
	http *http.Client

	// endpointURL builds the getMe URL for a given token. Production
	// always uses buildTelegramEndpoint; tests override this field
	// to redirect requests at an httptest server without
	// monkey-patching transports.
	endpointURL func(token string) string

	// botToken is populated by Login once the token has been
	// validated. Greet reads it to issue getUpdates + sendMessage
	// calls. Empty when Login was bypassed in tests.
	botToken string
	// botInfo is the getMe result captured during Login. Greet
	// reads Username to print "open @<bot> and send /start".
	botInfo *userInfo

	// SkipGreet, when true, makes Greet a no-op. Used by the CLI
	//  flag to skip the 2-minute owner wait in
	// non-interactive environments.
	SkipGreet bool
}

// New constructs a Provider. opts fields fall back to defaults.
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
		opts:        opts,
		out:         out,
		in:          in,
		http:        httpClient,
		endpointURL: defaultEndpointURL,
	}
}

// defaultEndpointURL is the production URL builder for getMe.
// Factored out so tests can override Provider.endpointURL without
// monkey-patching http.Transport.
func defaultEndpointURL(token string) string {
	return "https://api.telegram.org/bot" + url.PathEscape(token) + "/getMe"
}

// buildAPIURL constructs a Telegram API endpoint URL for any
// method (sendMessage, getUpdates, …). It applies the test-only
// endpointURL override when set, so test code can redirect every
// outbound call at an httptest server with one knob.
func (p *Provider) buildAPIURL(method string, params url.Values) string {
	base := p.apiBaseURL()
	endpoint := base + "/" + method
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	return endpoint
}

// apiBaseURL returns the bot-prefixed base URL (e.g.
// "https://api.telegram.org/bot<token>" for production). When
// the test overrides endpointURL for getMe, we infer the same
// override applies to all methods by reusing the same prefix.
// Concretely: if endpointURL was overridden to point at an
// httptest server, apiBaseURL extracts that server's
// /bot<token>/ prefix so all API calls land on the test server.
func (p *Provider) apiBaseURL() string {
	if p.botToken == "" {
		return ""
	}
	// Production base.
	prod := "https://api.telegram.org/bot" + url.PathEscape(p.botToken)
	// If endpointURL has been overridden to a non-production URL,
	// reuse the same scheme+host+prefix. We detect this by
	// comparing the override output to the production format
	// with the current token.
	if p.endpointURL != nil {
		override := p.endpointURL("__probe__")
		if !strings.HasPrefix(override, "https://api.telegram.org/") {
			// Test override detected. Build the corresponding base
			// by replacing the trailing "/getMe" with the bot prefix.
			idx := strings.LastIndex(override, "/")
			if idx > 0 {
				return override[:idx+1] + "bot" + url.PathEscape(p.botToken)
			}
		}
	}
	return prod
}

// Name implements login.Provider.
func (p *Provider) Name() string { return "telegram" }

// Login implements login.Provider. See package doc for the full
// flow.
func (p *Provider) Login(ctx context.Context) (*login.Credentials, error) {
	token := strings.TrimSpace(p.opts.Token)
	if token == "" {
		p.printInstructions()
		var err error
		token, err = p.readToken(ctx)
		if err != nil {
			return nil, err
		}
	}

	info, err := p.validateToken(ctx, token)
	if err != nil {
		return nil, err
	}

	p.botToken = token
	p.botInfo = info

	fmt.Fprintln(p.out)
	fmt.Fprintf(p.out, "✓ Bot verified!\n")
	fmt.Fprintf(p.out, "  Bot ID:    %d\n", info.ID)
	if info.Username != "" {
		fmt.Fprintf(p.out, "  Username:  @%s\n", info.Username)
	}
	if info.FirstName != "" {
		fmt.Fprintf(p.out, "  Display:   %s\n", info.FirstName)
	}

	return &login.Credentials{
		BotToken:  token,
		AppName:   info.displayName(),
		CreatedAt: time.Now(),
	}, nil
}

// Greet implements login.Provider.
//
// Best-effort: a failed greeting does NOT roll back the successful
// registration. The CLI orchestrator is expected to log + swallow
// the error.
//
// Telegram has no reliable "owner chat_id" surface during login.
// We work around this by polling getUpdates for the first private
// message from any user after the bot was created: when the user
// sends /start (or anything) to the bot, we capture their chat_id
// and send the greeting bodies.
//
// If the user doesn't message the bot within greetWaitTimeout, we
// log a friendly note and return nil — the greeting is simply
// skipped. There is no daemon-side fallback: the daemon only
// answers runtime messages and never sends (or replays) the
// login greeting. Registration still succeeds without it.
func (p *Provider) Greet(ctx context.Context, messages login.GreetingMessages) error {
	if p.botToken == "" || p.SkipGreet {
		// Login was bypassed in tests, OR the caller opted
		// out of the post-login greeting (e.g. --no-greet).
		return nil
	}

	fmt.Fprintln(p.out)
	fmt.Fprintln(p.out, "📨 Greeting setup")
	fmt.Fprintln(p.out, "-----------------")
	if p.botInfo != nil && p.botInfo.Username != "" {
		fmt.Fprintf(p.out, "Open Telegram, search for @%s, and send any message\n", p.botInfo.Username)
		fmt.Fprintf(p.out, "(for example: /start).\n")
	} else {
		fmt.Fprintln(p.out, "Open a private chat with your bot and send any message.")
	}
	fmt.Fprintf(p.out, "Waiting up to %s for your first message...\n", greetWaitTimeout)

	waitCtx, cancel := context.WithTimeout(ctx, greetWaitTimeout)
	defer cancel()
	chatID, err := p.waitForFirstMessage(waitCtx)
	if err != nil {
		// Soft failure: surface the cause but don't propagate as a
		// hard error. The CLI orchestrator will log "greeting
		// failed" if we returned a non-nil error; here we want
		// the user to see the friendly "you can /start later"
		// hint instead of a stack trace.
		fmt.Fprintln(p.out)
		fmt.Fprintf(p.out, "⏱  %v\n", err)
		fmt.Fprintln(p.out, "You can still send /start later — NightMe will respond once")
		fmt.Fprintln(p.out, "the daemon is running (`nightme start`; v1.3+ multi-channel — telegram auto-starts if creds present).")
		return nil
	}

	fmt.Fprintf(p.out, "  ✓ Got first message from chat %d\n", chatID)
	fmt.Fprintln(p.out, "  Sending greeting...")

	if err := p.sendGreeting(waitCtx, chatID, messages); err != nil {
		return fmt.Errorf("telegram: send greeting: %w", err)
	}
	fmt.Fprintln(p.out, "  ✓ Greeting sent")
	return nil
}

// printInstructions prints the @BotFather walkthrough to p.out.
// Kept short: 12 lines, fits a 24x80 terminal.
func (p *Provider) printInstructions() {
	fmt.Fprintln(p.out, "Telegram bot setup")
	fmt.Fprintln(p.out, "==================")
	fmt.Fprintln(p.out)
	fmt.Fprintln(p.out, "Telegram bots are user-created via @BotFather, not by")
	fmt.Fprintln(p.out, "nightme. To register a bot:")
	fmt.Fprintln(p.out)
	fmt.Fprintln(p.out, "  1. Open Telegram and search for @BotFather.")
	fmt.Fprintln(p.out, "  2. Send /newbot and follow the prompts (name + username).")
	fmt.Fprintln(p.out, "  3. BotFather will reply with an HTTP API token.")
	fmt.Fprintln(p.out, "  4. Paste the token below.")
	fmt.Fprintln(p.out)
	fmt.Fprint(p.out, "Bot token: ")
}

// readToken loops on p.in until a non-empty trimmed line comes
// through, or ctx fires (so the user can Ctrl+C cleanly).
func (p *Provider) readToken(ctx context.Context) (string, error) {
	scanner := bufio.NewScanner(p.in)
	// Telegram tokens can be > 64 chars; raise the buffer cap.
	scanner.Buffer(make([]byte, 0, 256), 1024)
	done := make(chan struct{})
	var line string
	var err error
	go func() {
		defer close(done)
		for scanner.Scan() {
			text := strings.TrimSpace(scanner.Text())
			if text != "" {
				line = text
				return
			}
		}
		err = scanner.Err()
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return "", fmt.Errorf("telegram: %w: %v", login.ErrLoginTimeout, ctx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("telegram: read token: %w", err)
	}
	// line == "" means the scanner reached EOF without seeing a
	// non-empty line. If ctx was cancelled concurrently, honour
	// the cancellation contract — return ErrLoginTimeout instead of
	// falling through to the empty-token error. Without this
	// guard, the select above races between `<-done` (goroutine
	// closing EOF fast) and `<-ctx.Done()`; the goroutine can win
	// when input is empty AND ctx is already cancelled at Login
	// entry, producing the misleading "empty token" error. The
	// contract `TestProvider_Login_ContextCancelled` asserts is
	// "ctx cancellation wins" — enforce it explicitly here.
	if line == "" {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("telegram: %w: %v", login.ErrLoginTimeout, ctxErr)
		}
		return "", fmt.Errorf("telegram: %w: empty token", login.ErrLoginFailed)
	}
	return line, nil
}

// userInfo mirrors the relevant subset of the Telegram getMe
// response. We only decode the fields we display / persist.
type userInfo struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// displayName renders the bot's human-readable name. Falls back
// to "@"+username if first_name is empty.
func (u userInfo) displayName() string {
	name := strings.TrimSpace(u.FirstName)
	if u.Username != "" {
		if name == "" {
			return "@" + u.Username
		}
		return name + " (@" + u.Username + ")"
	}
	return name
}

// validateToken calls https://api.telegram.org/bot<token>/getMe
// and decodes the result. Returns userInfo on success or wraps
// ErrLoginFailed for any non-OK response.
func (p *Provider) validateToken(ctx context.Context, token string) (*userInfo, error) {
	endpoint := p.endpointURL(token)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram: build request: %w", err)
	}
	response, err := p.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("telegram: %w: %v", login.ErrLoginFailed, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("telegram: read getMe: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		// Telegram's error envelope uses "description" / "error_code".
		// 401 means token is malformed or revoked.
		return nil, fmt.Errorf("telegram: %w: getMe status %d: %s",
			login.ErrLoginFailed, response.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		OK     bool      `json:"ok"`
		Result *userInfo `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("telegram: %w: decode getMe: %v", login.ErrLoginFailed, err)
	}
	if !envelope.OK || envelope.Result == nil {
		return nil, fmt.Errorf("telegram: %w: getMe returned not-ok: %s",
			login.ErrLoginFailed, strings.TrimSpace(string(body)))
	}
	if !envelope.Result.IsBot {
		return nil, fmt.Errorf("telegram: %w: token does not belong to a bot account",
			login.ErrLoginFailed)
	}
	return envelope.Result, nil
}

// waitForFirstMessage polls getUpdates until a private-chat message
// from a non-bot user arrives, or waitCtx fires.
//
// We use long polling (timeout=25s) so the connection is held open
// for at most 25 seconds per call. The deadline is set to
// greetWaitTimeout (2 minutes) from above; we honour ctx.Err() at
// the top of each iteration so cancellation propagates fast.
//
// Only private-chat messages count. Group messages are skipped
// silently: greeting in a group would be inappropriate (the user
// may have added the bot to a topic for other reasons) and would
// also leak the greeting to other chat members.
func (p *Provider) waitForFirstMessage(ctx context.Context) (int64, error) {
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return 0, fmt.Errorf("timeout: no message received within %s", greetWaitTimeout)
			}
			return 0, err
		}

		params := url.Values{}
		params.Set("offset", strconv.FormatInt(offset, 10))
		params.Set("limit", "1")
		params.Set("timeout", strconv.Itoa(longPollSeconds))
		params.Set("allowed_updates", "[\"message\"]")

		updates, err := p.callAPI(ctx, "getUpdates", params)
		if err != nil {
			// Transient: log and retry (the long-poll timeout may
			// also have fired with an empty result, which is not
			// an error from our side).
			if ctx.Err() == nil {
				fmt.Fprintf(p.out, "  (getUpdates retry: %v)\n", err)
				time.Sleep(time.Second)
			}
			continue
		}

		for _, update := range updates {
			offset = update.UpdateID + 1
			if update.Message == nil {
				continue
			}
			if update.Message.Chat.Type != "private" {
				continue
			}
			if update.Message.From == nil || update.Message.From.IsBot {
				continue
			}
			return update.Message.Chat.ID, nil
		}
	}
}

// sendGreeting fires each greeting body as a Telegram sendMessage.
// Best-effort: a failure on one body does not stop the others.
//
// Telegram only sends the English copy. The GreetingTexts helper
// still exposes a Chinese field for Feishu's post envelope (which
// renders both locales natively), but Telegram has no equivalent
// bilingual block — sending two consecutive messages doubles the
// bot's noise for English-only users without a localisation
// payoff. If a future caller wants Chinese too, plumb it
// through explicitly here rather than re-enabling the auto-send.
func (p *Provider) sendGreeting(ctx context.Context, chatID int64, messages login.GreetingMessages) error {
	for index, body := range messages {
		if body.English == "" {
			continue
		}
		if err := p.sendOne(ctx, chatID, body.English); err != nil {
			return fmt.Errorf("body %d english: %w", index, err)
		}
	}
	return nil
}

// sendOne posts one greeting line via sendMessage.
func (p *Provider) sendOne(ctx context.Context, chatID int64, text string) error {
	params := url.Values{}
	params.Set("chat_id", strconv.FormatInt(chatID, 10))
	params.Set("text", text)
	return p.postAPI(ctx, "sendMessage", params)
}

// update mirrors the relevant subset of a Telegram Update. We
// only decode the fields we use (UpdateID + the message envelope).
type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int    `json:"message_id"`
		Chat      struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		From *struct {
			ID        int64  `json:"id"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
			IsBot     bool   `json:"is_bot"`
		} `json:"from"`
		Text string `json:"text"`
	} `json:"message"`
}

// callAPI issues a GET Telegram Bot API call and returns the
// decoded result slice. Used by waitForFirstMessage (getUpdates).
// On any non-OK envelope or non-200 status, returns a non-nil
// error so the caller can retry.
func (p *Provider) callAPI(ctx context.Context, method string, params url.Values) ([]update, error) {
	endpoint := p.buildAPIURL(method, params)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Per-call deadline so a hung long-poll doesn't outlive the
	// greeting wait window by more than a couple of seconds.
	requestCtx, cancel := context.WithTimeout(ctx, longPollSeconds+10*time.Second)
	defer cancel()
	request = request.WithContext(requestCtx)

	response, err := p.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		OK     bool     `json:"ok"`
		Result []update `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if !envelope.OK {
		return nil, fmt.Errorf("not ok: %s", strings.TrimSpace(string(body)))
	}
	return envelope.Result, nil
}

// postAPI issues a POST Telegram Bot API call. Used by sendOne
// (sendMessage) which Telegram officially documents as a POST
// endpoint (GET also works in practice but POST matches the
// spec).
func (p *Provider) postAPI(ctx context.Context, method string, params url.Values) error {
	// postAPI uses the same URL builder as callAPI so test
	// endpointURL overrides cover both methods. POST bodies are
	// x-www-form-urlencoded so query-string params from the
	// builder don't double-encode. Strip them out here.
	endpoint := p.buildAPIURL(method, nil)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := p.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if !envelope.OK {
		return fmt.Errorf("not ok: %s", strings.TrimSpace(string(body)))
	}
	return nil
}

// errTelegramLogin is a sentinel kept for stable test identifiers.
var errTelegramLogin = errors.New("telegram: login error")
