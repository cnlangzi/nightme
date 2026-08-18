// Package login defines the per-channel credential onboarding contract.
//
// Each channel (Feishu, Lark, WhatsApp, ...) exposes a Provider that
// performs the interactive flow which yields a Credentials struct. The
// returned credentials are written into the on-disk Config; Channel
// adapters then read them at startup (see F-08).
//
// Provider implementations live in sub-packages (feishu, ...). v0.1
// ships only Feishu; the interface is designed to accommodate the
// international Lark flavour and future channels without changing
// call sites.
package login

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
)

// Provider performs a channel-specific interactive login flow and
// returns credentials suitable for storing in Config.
//
// Implementations are expected to honour ctx cancellation: the user
// gave up, the network died, the timeout elapsed -> return promptly
// with an error wrapping ErrLoginTimeout or ctx.Err().
type Provider interface {
	// Name returns the channel name (e.g. "feishu", "lark"). It
	// exists primarily so log lines can attribute a Login call to
	// the right provider; it is not currently part of the CLI
	// command tree (see cmd/nightme/login.go) since v0.1 ships a
	// single channel. Kept on the interface so adding a second
	// provider (e.g. Lark) does not require a signature change.
	Name() string

	// Login blocks until the user completes the flow (or ctx is
	// cancelled). On success the caller persists the returned
	// Credentials into the on-disk Config.
	Login(ctx context.Context) (*Credentials, error)

	// Greet dispatches the canonical NightMe greeting to the user
	// who just completed the flow. Implementations are expected
	// to honour the best-effort contract: never a pre-requisite
	// for credential persistence, and silent when the channel
	// did not return an owner ID.
	//
	// The login package is intentionally data-only: messages
	// carries an ordered list of bilingual units (each
	// GreetingBody has both Chinese and English) and the
	// provider decides how to lay them out in the channel's
	// native envelope. For Feishu, each GreetingBody becomes one
	// `post` message carrying both `zh_cn` and `en_us` blocks
	// — the receiver's client picks the locale tag matching its
	// UI language, so the same payload renders correctly for any
	// user regardless of locale. See docs/channel/feishu.md §19
	// for the empirical verification.
	//
	// Greet is called by the CLI orchestrator AFTER Login returns
	// AND config.SaveDefault has succeeded — see cmd/nightme/login.go
	// for ordering rationale. The ctx parameter is honored as the
	// parent of per-message deadlines; a cancelled parent aborts
	// subsequent sends.
	Greet(ctx context.Context, messages GreetingMessages) error
}

// Credentials is the on-disk shape of a successful Login.
//
// The fields are channel-specific. Feishu uses AppID + AppSecret;
// Telegram uses BotToken. Providers populate whichever fields apply
// and leave the others empty. The CLI orchestrator is the single
// place that knows how to map these onto Config.
type Credentials struct {
	// AppID is the channel-side identifier of the bot application;
	// for Feishu this is result.ClientID. Empty for Telegram.
	AppID string `json:"app_id,omitempty"`

	// AppSecret is the channel-side secret; for Feishu this is
	// result.ClientSecret. Never log this value. Empty for Telegram.
	AppSecret string `json:"app_secret,omitempty"`

	// BotToken is the @BotFather-issued HTTP API token for a
	// user-created Telegram bot. Never log this value. Empty for
	// Feishu.
	BotToken string `json:"bot_token,omitempty"`

	// AppName is the human-readable application name chosen on the
	// channel's consent page (Feishu) or via /setname in BotFather
	// (Telegram).
	AppName string `json:"app_name,omitempty"`

	// CreatedAt records when nightme received the credentials. The
	// channel may not echo this back; we stamp it locally so audit
	// logs can answer "when did this app join?".
	CreatedAt time.Time `json:"created_at"`
}

// ErrLoginTimeout wraps the upstream context-deadline-exceeded path.
// Providers return this (or wrap ctx.Err()) when the user took too
// long to scan / confirm.
var ErrLoginTimeout = errors.New("login timeout")

// ErrLoginFailed is the generic "the channel rejected the registration"
// marker. Providers should wrap it with whatever concrete code the
// channel returned (e.g. feishu's *registration.RegisterAppError).
var ErrLoginFailed = errors.New("login failed")
// ProviderBuilder constructs a *cobra.Command that wraps a
// channel-specific login provider. Each provider package exposes
// one via init() + RegisterProvider, and the CLI orchestrator
// walks the registry to assemble `nightme login <channel>`.
//
// The builder receives the shared flags struct so the CLI can
// pass per-command settings (timeout, etc.) consistently across
// channels. Returning nil + nil means "this channel declines to
// register" — used by tests to skip providers in specific setups.
//
// Cobra import kept inside the function signature so callers in
// other packages don't pull cobra into their build graph.
type ProviderBuilder func(flags *ProviderFlags) *cobra.Command

// ProviderFlags is the shared flag bag threaded through every
// login subcommand. Currently just the timeout — extending it
// doesn't require touching provider packages.
type ProviderFlags struct {
	// Timeout caps the entire login flow (token read, getMe
	// validation, greeting wait). Per-channel providers may
	// impose shorter internal deadlines; this is the outer
	// envelope.
	Timeout time.Duration
}

// registry holds the channel name -> ProviderBuilder map. Built
// lazily at process start via each provider's init().
//
// Concurrency: Registry reads happen on every CLI invocation
// (cheap) but writes happen once per init() (also cheap). The
// RWMutex protects against accidental concurrent access during
// tests that re-register; production code never sees a write
// after main().
var (
	registryMu sync.RWMutex
	registry   = map[string]ProviderBuilder{}
)

// RegisterProvider makes a channel's login command available
// under `nightme login <name>`. Subsequent calls with the same
// name SILENTLY OVERWRITE the previous registration — init()
// order across packages is otherwise undefined, and we don't want
// to break an otherwise working binary because two providers raced
// for the same slot. Tests can use the Reset hook below to clear
// the map.
func RegisterProvider(name string, builder ProviderBuilder) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = builder
}

// AvailableChannels returns the registered channel names in
// alphabetical order. Used by the CLI to render "no channel
// specified" / "unknown channel" error messages, and by tests.
func AvailableChannels() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// GetBuilder returns the builder registered for name, or nil if
// no provider is registered under that name. Used by the CLI
// orchestrator when assembling the `login` subcommand tree at
// process start.
func GetBuilder(name string) ProviderBuilder {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[name]
}

// Reset clears the registry. Test-only: production code MUST NOT
// call this. Each provider package's init() re-registers on next
// CLI startup, so Reset + re-import gives a fresh slate.
func Reset() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]ProviderBuilder{}
}

// LoginWith runs the standard login orchestration against a
// channel-specific provider:
//
//  1. Call provider.Login(ctx) to perform the interactive flow
//     and obtain Credentials.
//  2. Write the credentials onto a fresh Config and persist
//     it via SaveDefault. On write failure, surface the
//     credentials verbatim on errOut so the user can paste them
//     by hand (Feishu F-22 §4 "preserve in-memory creds on disk
//     write failure").
//  3. Call provider.Greet(ctx, GreetingTexts()) — best-effort;
//     a failed greeting never rolls back the successful save.
//  4. Print the success summary to out.
//
// This is the single canonical login path shared by every
// channel. Provider packages MUST NOT reimplement it: doing so
// risks drifting semantics across channels (e.g. one provider
// logging the greeting success before the save succeeded).
//
// out / errOut are decoupled from cobra so this function stays
// usable from non-CLI callers (tests, future integration layers).
func LoginWith(ctx context.Context, provider Provider, out, errOut io.Writer) error {
	if provider == nil {
		return errors.New("login: provider is nil")
	}
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}

	creds, err := provider.Login(ctx)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}

	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("login: load config: %w", err)
	}

	// Per-channel credential write: dispatch on provider.Name() so
	// logging in to one channel never stomps the other channel's
	// credentials (e.g. `nightme login telegram` after `nightme
	// login feishu` must keep the feishu AppID/AppSecret intact).
	switch provider.Name() {
	case "feishu":
		cfg.Feishu.AppID = creds.AppID
		cfg.Feishu.AppSecret = creds.AppSecret
	case "telegram":
		// Telegram uses a single BotToken (no app_id / app_secret
		// pair). Providers that don't populate BotToken leave it
		// empty, so the assignment is always safe.
		cfg.Telegram.BotToken = creds.BotToken
	default:
		return fmt.Errorf("login: unknown provider %q", provider.Name())
	}

	if err := config.SaveDefault(cfg); err != nil {
		// Surface credentials verbatim so the user can paste
		// them by hand if the disk write fails (permission
		// denied, disk full, ...). Per-channel labels so the
		// right field shows up in the dump — `app_id`/`app_secret`
		// are Feishu-specific; Telegram uses `bot_token`.
		fmt.Fprintf(errOut,
			"warning: failed to persist credentials: %v\n"+
				"in-memory credentials (please paste into config.yaml):\n",
			err)
		switch provider.Name() {
		case "feishu":
			fmt.Fprintf(errOut, "  app_id:     %s\n", creds.AppID)
			fmt.Fprintf(errOut, "  app_secret: %s\n", creds.AppSecret)
		case "telegram":
			fmt.Fprintf(errOut, "  bot_token:  %s\n", creds.BotToken)
		}
		if creds.AppName != "" {
			fmt.Fprintf(errOut, "  app_name:   %s\n", creds.AppName)
		}
		return fmt.Errorf("login: %w", err)
	}

	if greetErr := provider.Greet(ctx, GreetingTexts()); greetErr != nil {
		// Best-effort: surface but don't fail the registration.
		fmt.Fprintf(errOut, "warning: greeting DM failed: %v\n", greetErr)
	}

	fmt.Fprintf(out, "✓ App registered successfully!\n")
	// Per-channel summary — Feishu uses AppID/AppName; Telegram
	// uses BotToken. Don't print an empty "App ID:" line for
	// channels that don't have one.
	switch provider.Name() {
	case "feishu":
		fmt.Fprintf(out, "  App ID:    %s\n", creds.AppID)
		if creds.AppName != "" {
			fmt.Fprintf(out, "  App Name:  %s\n", creds.AppName)
		}
	case "telegram":
		fmt.Fprintf(out, "  Bot:       %s\n", creds.AppName)
	}
	fmt.Fprintf(out, "  Saved to:  %s\n", config.DefaultPath())
	fmt.Fprintf(out, "\nNext: run `nightme start` to launch the gateway daemon.\n")
	return nil
}
