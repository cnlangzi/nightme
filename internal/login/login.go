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
	"time"
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

// Credentials is the on-disk shape of a successful Login. Only
// AppSecret is sensitive; AppID, AppName and CreatedAt are public.
type Credentials struct {
	// AppID is the channel-side identifier of the bot application;
	// for Feishu this is result.ClientID.
	AppID string `json:"app_id"`

	// AppSecret is the channel-side secret; for Feishu this is
	// result.ClientSecret. Never log this value.
	AppSecret string `json:"app_secret"`

	// AppName is the human-readable application name chosen on the
	// channel's consent page.
	AppName string `json:"app_name"`

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