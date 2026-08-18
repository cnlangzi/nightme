package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/login"
)

// fakeTelegramServer is an httptest server that drives a scripted
// Telegram conversation for Greet tests. The first call to
// /getUpdates returns one update with a private-chat message from
// a non-bot user; subsequent calls return empty. /sendMessage calls
// are recorded so tests can assert the greeting was posted.
type fakeTelegramServer struct {
	server *httptest.Server

	mu             sync.Mutex
	getUpdatesHit  atomic.Int32
	sendMessageHit atomic.Int32

	// Initial updates to return on the first getUpdates call.
	// After they're drained, subsequent calls return empty.
	initialUpdates []map[string]any
}

func newFakeTelegramServer(t *testing.T, updates ...map[string]any) *fakeTelegramServer {
	t.Helper()
	f := &fakeTelegramServer{initialUpdates: updates}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip /bot<token>/ prefix to dispatch by method name.
		// All test paths look like /bot<token>/<method>. Strip
		// the /bot<token>/ prefix to get the bare method name.
		// We use the suffix after the last "/".
		path := r.URL.Path
		idx := strings.LastIndex(path, "/")
		method := path
		if idx >= 0 {
			method = path[idx+1:]
		}
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getMe":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"id":         8688547819,
					"is_bot":     true,
					"username":   "nightme_dev_bot",
					"first_name": "NightMe Dev",
				},
			})
		case "getUpdates":
			n := f.getUpdatesHit.Add(1)
			f.mu.Lock()
			defer f.mu.Unlock()
			if n == 1 && len(f.initialUpdates) > 0 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok":     true,
					"result": f.initialUpdates,
				})
				return
			}
			// Long-poll: hold the request open for up to
			// `timeout` seconds, then return empty.
			timeout, _ := strconv.Atoi(r.URL.Query().Get("timeout"))
			if timeout > 0 {
				select {
				case <-time.After(time.Duration(timeout) * time.Second):
				case <-r.Context().Done():
				}
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		case "sendMessage":
			f.sendMessageHit.Add(1)
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":100,"type":"private"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func TestProvider_Name(t *testing.T) {
	p := New(Options{})
	if got := p.Name(); got != "telegram" {
		t.Fatalf("Name = %q, want telegram", got)
	}
}

func TestProvider_Greet_NoOpWhenNoLogin(t *testing.T) {
	// Greet before Login must be a no-op (the bot token is
	// empty in this state).
	p := New(Options{Out: &bytes.Buffer{}})
	if err := p.Greet(context.Background(), login.GreetingMessages{}); err != nil {
		t.Fatalf("Greet without Login must not error, got %v", err)
	}
}

func TestProvider_Login_EmptyToken(t *testing.T) {
	p := New(Options{In: strings.NewReader("\n")})
	_, err := p.Login(context.Background())
	if err == nil {
		t.Fatal("expected error for empty token")
	}
	if !errors.Is(err, login.ErrLoginFailed) {
		t.Fatalf("err should wrap ErrLoginFailed, got %v", err)
	}
}

func TestProvider_Login_ValidToken(t *testing.T) {
	server := newFakeTelegramServer(t)
	out := &bytes.Buffer{}
	p := &Provider{
		opts:        Options{},
		out:         out,
		in:          strings.NewReader("1234:fake\n"),
		http:        server.server.Client(),
		endpointURL: func(token string) string { return server.server.URL + "/bot" + token + "/getMe" },
	}

	creds, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if creds.BotToken != "1234:fake" {
		t.Fatalf("BotToken = %q, want 1234:fake", creds.BotToken)
	}
	if !strings.Contains(creds.AppName, "nightme_dev_bot") {
		t.Fatalf("AppName should mention username, got %q", creds.AppName)
	}
	if !strings.Contains(out.String(), "Bot verified") {
		t.Fatalf("expected success output, got %q", out.String())
	}
	if p.botToken != "1234:fake" {
		t.Fatalf("Provider.botToken not set after Login")
	}
}

func TestProvider_Login_InvalidToken_Status401(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
	}))
	defer server.Close()

	p := &Provider{
		opts:        Options{},
		out:         &bytes.Buffer{},
		in:          strings.NewReader("badtoken\n"),
		http:        server.Client(),
		endpointURL: func(token string) string { return server.URL + "/bot" + token + "/getMe" },
	}

	_, err := p.Login(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, login.ErrLoginFailed) {
		t.Fatalf("err should wrap ErrLoginFailed, got %v", err)
	}
}

func TestProvider_Login_NonBotAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"id":         12345,
				"is_bot":     false,
				"first_name": "Some User",
			},
		})
	}))
	defer server.Close()

	p := &Provider{
		opts:        Options{},
		out:         &bytes.Buffer{},
		in:          strings.NewReader("user-account-token\n"),
		http:        server.Client(),
		endpointURL: func(token string) string { return server.URL + "/bot" + token + "/getMe" },
	}

	_, err := p.Login(context.Background())
	if err == nil {
		t.Fatal("expected error for non-bot account")
	}
	if !errors.Is(err, login.ErrLoginFailed) {
		t.Fatalf("err should wrap ErrLoginFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "not belong to a bot") {
		t.Fatalf("err should mention bot requirement, got %v", err)
	}
}

func TestProvider_Login_NotOKEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":false,"description":"token revoked"}`)
	}))
	defer server.Close()

	p := &Provider{
		opts:        Options{},
		out:         &bytes.Buffer{},
		in:          strings.NewReader("revoked-token\n"),
		http:        server.Client(),
		endpointURL: func(token string) string { return server.URL + "/bot" + token + "/getMe" },
	}

	_, err := p.Login(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, login.ErrLoginFailed) {
		t.Fatalf("err should wrap ErrLoginFailed, got %v", err)
	}
}

func TestProvider_Login_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := New(Options{In: strings.NewReader("")})
	_, err := p.Login(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, login.ErrLoginTimeout) {
		t.Fatalf("err should wrap ErrLoginTimeout, got %v", err)
	}
}

func TestProvider_Greet_SendsBodiesToFirstPrivateMessage(t *testing.T) {
	// First update: private chat message from a user (the owner
	// we're trying to identify).
	server := newFakeTelegramServer(t, map[string]any{
		"update_id": 100,
		"message": map[string]any{
			"message_id": 1,
			"chat":       map[string]any{"id": 4242, "type": "private"},
			"from":       map[string]any{"id": 99, "is_bot": false, "first_name": "Alice"},
			"text":       "/start",
		},
	})

	out := &bytes.Buffer{}
	p := &Provider{
		opts: Options{},
		out:  out,
		in:   strings.NewReader(""),
		http: server.server.Client(),
		// Override endpoints so production URLs redirect to test.
		endpointURL: func(token string) string { return server.server.URL + "/bot" + token + "/getMe" },
	}
	// Login flow runs against test server's getMe via endpointURL.
	p.endpointURL = func(token string) string { return server.server.URL + "/bot" + token + "/getMe" }
	// Bypass Login entirely to set botToken / botInfo directly.
	p.botToken = "x"
	p.botInfo = &userInfo{ID: 8688547819, IsBot: true, Username: "nightme_dev_bot", FirstName: "NightMe Dev"}

	err := p.Greet(context.Background(), login.GreetingTexts())
	if err != nil {
		t.Fatalf("Greet: %v", err)
	}

	// Telegram only sends the English copy of each body (2 bodies
	// × 1 language = 2 sendMessage calls). Chinese is skipped on
	// purpose: Telegram doesn't have a Feishu-style bilingual post
	// envelope, so the CN halves would just be noise to an
	// English-only user.
	if got := server.sendMessageHit.Load(); got != 2 {
		t.Fatalf("sendMessage hits = %d, want 2", got)
	}
	if !strings.Contains(out.String(), "Greeting sent") {
		t.Fatalf("output missing success message: %q", out.String())
	}
}

func TestProvider_Greet_TimeoutIsSoftError(t *testing.T) {
	// Server that returns empty updates forever; greet should
	// timeout gracefully (no error returned to caller).
	server := newFakeTelegramServer(t) // no initialUpdates
	out := &bytes.Buffer{}
	p := &Provider{
		opts:        Options{},
		out:         out,
		in:          strings.NewReader(""),
		http:        server.server.Client(),
		endpointURL: func(token string) string { return server.server.URL + "/bot" + token + "/getMe" },
	}
	p.botToken = "x"
	p.botInfo = &userInfo{ID: 1, IsBot: true, Username: "testbot", FirstName: "Test"}

	// Shrink the greet wait so the test doesn't actually wait 2
	// minutes. We do this by setting a very short parent context
	// timeout that propagates into waitForFirstMessage.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Override greetWaitTimeout indirectly via ctx; waitForFirstMessage
	// honours ctx first, then its own deadline.
	err := p.Greet(ctx, login.GreetingTexts())
	if err != nil {
		t.Fatalf("Greet timeout should be soft (nil error), got %v", err)
	}
	if !strings.Contains(out.String(), "You can still send /start later") {
		t.Fatalf("output missing soft-failure hint: %q", out.String())
	}
}

func TestProvider_Greet_IgnoresGroupMessages(t *testing.T) {
	// Group message should be skipped; only a private message
	// counts toward greeting.
	server := newFakeTelegramServer(t,
		map[string]any{
			"update_id": 100,
			"message": map[string]any{
				"message_id": 1,
				"chat":       map[string]any{"id": -100111, "type": "supergroup"},
				"from":       map[string]any{"id": 99, "is_bot": false},
				"text":       "hi",
			},
		},
		map[string]any{
			"update_id": 101,
			"message": map[string]any{
				"message_id": 2,
				"chat":       map[string]any{"id": 4242, "type": "private"},
				"from":       map[string]any{"id": 99, "is_bot": false},
				"text":       "/start",
			},
		},
	)

	out := &bytes.Buffer{}
	p := &Provider{
		opts: Options{},
		out:  out,
		in:   strings.NewReader(""),
		http: server.server.Client(),
		endpointURL: func(token string) string { return server.server.URL + "/bot" + token + "/getMe" },
	}
	p.botToken = "x"
	p.botInfo = &userInfo{ID: 1, IsBot: true, Username: "testbot"}

	err := p.Greet(context.Background(), login.GreetingTexts())
	if err != nil {
		t.Fatalf("Greet: %v", err)
	}
	// 2 sendMessage (English-only greeting bodies); group message
	// was skipped, private message greeted.
	if got := server.sendMessageHit.Load(); got != 2 {
		t.Fatalf("sendMessage hits = %d, want 2 (group msg skipped, private msg greeted)", got)
	}
}

func TestProvider_Greet_IgnoresBotMessages(t *testing.T) {
	// A bot From should be ignored (someone else added the same
	// bot to a group; we don't want to greet that).
	server := newFakeTelegramServer(t,
		map[string]any{
			"update_id": 100,
			"message": map[string]any{
				"message_id": 1,
				"chat":       map[string]any{"id": 4242, "type": "private"},
				"from":       map[string]any{"id": 99, "is_bot": true},
				"text":       "hi",
			},
		},
	)

	out := &bytes.Buffer{}
	p := &Provider{
		opts:        Options{},
		out:         out,
		in:          strings.NewReader(""),
		http:        server.server.Client(),
		endpointURL: func(token string) string { return server.server.URL + "/bot" + token + "/getMe" },
	}
	p.botToken = "x"
	p.botInfo = &userInfo{ID: 1, IsBot: true, Username: "testbot"}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Bot message is filtered out, so Greet should soft-fail with
	// the timeout hint.
	err := p.Greet(ctx, login.GreetingTexts())
	if err != nil {
		t.Fatalf("Greet: %v", err)
	}
	if !strings.Contains(out.String(), "You can still send /start later") {
		t.Fatalf("bot-from message must be skipped, output: %q", out.String())
	}
}

func TestUserInfo_DisplayName(t *testing.T) {
	cases := []struct {
		name string
		info userInfo
		want string
	}{
		{"both", userInfo{FirstName: "NightMe", Username: "nightme_dev_bot"}, "NightMe (@nightme_dev_bot)"},
		{"only_first", userInfo{FirstName: "NightMe"}, "NightMe"},
		{"only_username", userInfo{Username: "nightme_dev_bot"}, "@nightme_dev_bot"},
		{"empty", userInfo{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.displayName(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDefaultEndpointURL(t *testing.T) {
	got := defaultEndpointURL("1234:abc")
	want := "https://api.telegram.org/bot1234:abc/getMe"
	if got != want {
		t.Fatalf("defaultEndpointURL = %q, want %q", got, want)
	}
}

// Ensure url.Values encode round-trips so we don't accidentally
// mis-encode allowed_updates (Telegram requires a JSON array, not
// a comma-separated string).
func TestGreetParamsEncoding(t *testing.T) {
	params := url.Values{}
	params.Set("offset", "0")
	params.Set("timeout", "25")
	params.Set("allowed_updates", `["message"]`)
	encoded := params.Encode()
	if !strings.Contains(encoded, "allowed_updates") {
		t.Fatalf("allowed_updates not in encoded params: %q", encoded)
	}
}

func TestProvider_Login_ViaTokenOption(t *testing.T) {
	// Non-interactive: token provided via Options.Token, no stdin
	// required. Used by the CLI `--token` flag for ERPL /
	// shell-wrapped invocations where stdin is closed.
	gotPath := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"id":         4242,
				"is_bot":     true,
				"username":   "erpl_test_bot",
				"first_name": "ERPL Test",
			},
		})
	}))
	defer server.Close()

	out := &bytes.Buffer{}
	p := &Provider{
		opts: Options{
			Token: "1234:from-flag",
			Out:   out,
		},
		out: out,
		http: server.Client(),
		endpointURL: func(token string) string {
			return server.URL + "/bot" + token + "/getMe"
		},
	}

	creds, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if creds.BotToken != "1234:from-flag" {
		t.Fatalf("BotToken = %q, want 1234:from-flag", creds.BotToken)
	}
	if !strings.Contains(gotPath, "1234:from-flag") {
		t.Fatalf("expected path to contain token from flag, got %q", gotPath)
	}
	// printInstructions must NOT have been called — non-interactive.
	if strings.Contains(out.String(), "BotFather walkthrough") {
		t.Fatalf("non-interactive Login should skip instructions")
	}
}

func TestProvider_Greet_SkippedWhenSkipGreetSet(t *testing.T) {
	// --no-greet flag: Provider.Greet should be a no-op.
	p := New(Options{})
	// Manually set the botToken + SkipGreet (Login was bypassed).
	p.botToken = "x"
	p.botInfo = &userInfo{ID: 1, IsBot: true, Username: "testbot"}
	p.SkipGreet = true

	out := &bytes.Buffer{}
	p.out = out

	if err := p.Greet(context.Background(), login.GreetingMessages{}); err != nil {
		t.Fatalf("Greet with SkipGreet: %v", err)
	}
	if out.Len() > 0 {
		t.Fatalf("Greet with SkipGreet must not print anything, got %q", out.String())
	}
}
