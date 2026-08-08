// Package auth defines the per-channel credential onboarding contract.
//
// Each channel Feishu, Lark, WhatsApp, ... exposes a Provider that
// performs the interactive flow which yields a Credentials struct. The
// returned credentials are written into the on-disk Config; Channel
// adapters then read them at startup (see F-08).
//
// Provider implementations live in sub-packages (feishu, ...). v0.1
// ships only Feishu; the interface is designed to accommodate the
// international Lark flavour and future channels without changing
// call sites.
package auth

import (
	"context"
	"errors"
	"time"
)

// Provider performs a channel-specific interactive auth flow and
// returns credentials suitable for storing in Config.
//
// Implementations are expected to honour ctx cancellation: the user
// gave up, the network died, the timeout elapsed -> return promptly
// with an error wrapping ErrAuthTimeout or ctx.Err().
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

// ErrAuthTimeout wraps the upstream context-deadline-exceeded path.
// Providers return this (or wrap ctx.Err()) when the user took too
// long to scan / confirm.
var ErrAuthTimeout = errors.New("authentication timeout")

// ErrAuthFailed is the generic "the channel rejected the registration"
// marker. Providers should wrap it with whatever concrete code the
// channel returned (e.g. feishu's *registration.RegisterAppError).
var ErrAuthFailed = errors.New("authentication failed")
