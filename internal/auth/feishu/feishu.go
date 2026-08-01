// Package feishu implements auth.Provider for the Feishu (飞书) channel.
//
// The provider wraps the larksuite/oapi-sdk-go v3 `registration`
// scene: it asks Feishu for a device code, prints the verification
// URL + ASCII QR to the terminal via the callback, then blocks
// polling until the user scans and approves on the Feishu consent
// page (or ctx is cancelled).
//
// Upon success the registration returns the new App's ClientID /
// ClientSecret, which we surface as a generic auth.Credentials. The
// CLI is responsible for persisting them into the on-disk Config.
package feishu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"

	"github.com/cnlangzi/nightme/internal/auth"
)

// FeishuAuthOptions configures a single auth flow run. AppPreset and
// DefaultAddons are merged with built-in fallbacks in NewFeishuAuth
// so callers only need to set what they care about.
type FeishuAuthOptions struct {
	// Addons overrides the default scopes/events. nil = use
	// DefaultAddons.
	Addons *registration.AppAddons

	// AppPreset pre-fills the app's name/description/avatar on the
	// consent page; nil means use DefaultAppPreset (the nightme
	// brand default — "NightMe" / "Sleep tight, code all night.").
	// The user can still edit the fields on the consent page
	// before submitting; the final values are whatever they enter.
	AppPreset *registration.AppPreset

	// ExistingAppID set with CreateOnly or as update-mode: asks
	// Feishu to use this app instead of creating a new one.
	ExistingAppID string

	// CreateOnly avoids update flows when true.
	CreateOnly bool

	// Out is where the QR + status messages go. nil = os.Stdout.
	// Tests pass a bytes.Buffer.
	Out io.Writer
}

// FeishuAuth is an auth.Provider for Feishu.
type FeishuAuth struct {
	opts   FeishuAuthOptions
	addons *registration.AppAddons
	preset *registration.AppPreset
	out    io.Writer
}

// NewFeishuAuth returns a ready-to-Login Provider. opts.Addons is
// defaulted to DefaultAddons(); opts.AppPreset is defaulted to
// DefaultAppPreset(); opts.Out falls back to os.Stdout.
//
// ExistingAppID and CreateOnly pass through unchanged.
func NewFeishuAuth(opts FeishuAuthOptions) *FeishuAuth {
	addons := opts.Addons
	if addons == nil {
		addons = DefaultAddons()
	}
	preset := opts.AppPreset
	if preset == nil {
		preset = DefaultAppPreset()
	}
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	return &FeishuAuth{
		opts:   opts,
		addons: addons,
		preset: preset,
		out:    out,
	}
}

// Name implements auth.Provider.
func (f *FeishuAuth) Name() string { return "feishu" }

// Login runs the device-authorization flow against Feishu and
// returns the freshly-issued credentials. It blocks until the user
// scans + approves (default ~10 min, see registration.Options).
//
// The implementation is intentionally thin: registration.RegisterApp
// does the heavy lifting (HTTP, polling, error wrapping). All we add
// is the QR callback, the status callback, and a sentinel-error wrap
// so callers can errors.Is-match without depending on the SDK.
func (f *FeishuAuth) Login(ctx context.Context) (*auth.Credentials, error) {
	opts := &registration.Options{
		AppID:      f.opts.ExistingAppID,
		CreateOnly: f.opts.CreateOnly,
		Addons:     f.addons,
		AppPreset:  f.preset,
		OnQRCode: func(info *registration.QRCodeInfo) {
			f.printQRCode(info)
		},
		OnStatusChange: func(info *registration.StatusChangeInfo) {
			f.printStatus(info)
		},
	}

	result, err := registration.RegisterApp(ctx, opts)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("feishu: %w: %v", auth.ErrAuthTimeout, err)
		}
		var regErr *registration.RegisterAppError
		if errors.As(err, &regErr) {
			return nil, fmt.Errorf("feishu: %w: %s: %s", auth.ErrAuthFailed, regErr.Code, regErr.Description)
		}
		return nil, fmt.Errorf("feishu: register: %w", err)
	}

	name := ""
	if f.preset != nil {
		name = f.preset.Name
	}
	return &auth.Credentials{
		AppID:     result.ClientID,
		AppSecret: result.ClientSecret,
		AppName:   name,
		CreatedAt: time.Now(),
	}, nil
}

// printQRCode renders the QR for the user. The QR itself is the
// thing humans visually compare to a screenshot — get it right.
//
// The trailing "Waiting for you to scan…" line tells the user what
// the next step is. We intentionally do not show it inside the
// OnStatusChange callback: the SDK calls OnStatusChange on every
// poll cycle while waiting, so printing there would spam the
// terminal once a second for ten minutes.
func (f *FeishuAuth) printQRCode(info *registration.QRCodeInfo) {
	fmt.Fprintf(f.out, "Scan this QR code with Feishu mobile, or open this URL:\n%s\n(expires in %d seconds)\n\n",
		info.URL, info.ExpireIn)
	// Errors here mean stdout is broken (closed pipe); nothing for
	// the user to do, and registration.RegisterApp will still
	// block on the polling loop.
	_ = RenderASCII(info.URL, f.out, false)
	fmt.Fprintln(f.out, "Waiting for you to scan and confirm in Feishu...")
}

// printStatus translates the SDK's raw status codes into messages a
// human running a CLI actually cares about. "polling" is suppressed
// entirely — the "Waiting…" line printed alongside the QR already
// conveys that, and printing on every poll would spam the terminal.
func (f *FeishuAuth) printStatus(info *registration.StatusChangeInfo) {
	switch info.Status {
	case registration.StatusPolling:
		// Silent: covered by the "Waiting…" line in printQRCode.
	case registration.StatusSlowDown:
		fmt.Fprintln(f.out, "Server asked us to slow polling; backing off...")
	case registration.StatusDomainSwitched:
		fmt.Fprintln(f.out, "Switched to Lark international domain.")
	default:
		// Unknown status: surface it so a future SDK addition is
		// visible to the operator instead of silently swallowed.
		fmt.Fprintf(f.out, "Auth flow status: %s\n", info.Status)
	}
}

// DefaultAppPreset returns the brand default pre-fill for the
// consent page: the app name "NightMe" with the tagline
// "Sleep tight, code all night.". Callers can override any field
// at construction time via FeishuAuthOptions.AppPreset; the user
// can still edit them on the consent page before submitting.
func DefaultAppPreset() *registration.AppPreset {
	return &registration.AppPreset{
		Name: "NightMe",
		Desc: "Sleep tight, code all night.",
	}
}

// DefaultAddons returns the scope + event + callback set nightme
// asks for at QR-code registration time. Callers may override at
// construction.
//
// Scope rationale (covers all v0.2 + near-term v0.3 use cases):
//
//	im:message:send_as_bot             send text / interactive / image / file
//	im:message:update                  update / edit already-sent messages
//	                                  (UpdateMessage in adapter)
//	im:message:receive_v1              receive message events
//	im:message.reactions:write_only    ⏳/🔄/✅ receipts on incoming msgs
//	im:message.reactions:read          read existing reactions (counterpart)
//	im:message:readonly                fetch historical messages AND
//	                                  download inbound attachment resources
//	                                  via F-14 passthrough
//	im:message.group_at_msg:readonly   bot triggered by @-mention in groups
//	im:message.p2p_msg:readonly        bot triggered in 1:1 chats
//	im:message.pins:read               read pinned-message state
//	im:message.pins:write_only         pin / unpin messages
//	im:message:recall                  recall bot-sent messages
//	im:message:send_multi_users        batch DM for notifications
//	im:message:send_sys_msg            system notifications
//	im:resource                        upload images/files for sending
//	im:chat:read                       read chat metadata
//	im:chat:update                     modify chat settings (name / topic)
//	im:chat.members:bot_access         read member list of chats bot is in
//	contact:contact.base:readonly      look up user basics (name, avatar)
//	cardkit:card:write                 create / update interactive cards
//	cardkit:card:read                  read card state (for sync handlers)
//	application:application:self_manage
//	                                  self-permission introspection
//	                                  (diagnostic foundation)
//
// The set intentionally mirrors larksuite/openclaw-lark's
// REQUIRED_APP_SCOPES (the official Lark/Feishu OpenClaw plugin)
// minus Docx/Base/Calendar/Task scopes that nightme does not yet
// implement. Adding everything at install time avoids forcing a
// re-authorize round the next time a feature lands.
//
// Callbacks:
//
//	card.action.trigger                receive interactive-card button
//	                                  clicks; without this our permission
//	                                  card buttons are inert.
//
// Events:
//
//	im.message.receive_v1              the canonical message-receive event
//
// The reaction *event* subscription is intentionally absent — nightme
// does not design user-driven reactions as input (see
// docs/feat/F-25-message-receipt.md), so the dispatcher swallows
// im.message.reaction.created_v1 events.
func DefaultAddons() *registration.AppAddons {
	preset := false
	return &registration.AppAddons{
		Preset: &preset,
		Scopes: registration.AppAddonsScopes{
			Tenant: []string{
				// Core message send / receive / update.
				"im:message:send_as_bot",
				"im:message:update",
				"im:message:receive_v1",
				// Receipts (F-25).
				"im:message.reactions:write_only",
				"im:message.reactions:read",
				// History + attachment passthrough (F-14).
				"im:message:readonly",
				// Group / 1:1 triggers.
				"im:message.group_at_msg:readonly",
				"im:message.p2p_msg:readonly",
				// Future messaging features (v0.3+).
				"im:message.pins:read",
				"im:message.pins:write_only",
				"im:message:recall",
				"im:message:send_multi_users",
				"im:message:send_sys_msg",
				// Media upload + chat metadata.
				"im:resource",
				"im:chat:read",
				"im:chat:update",
				"im:chat.members:bot_access",
				// User lookup (sender display name, etc.).
				"contact:contact.base:readonly",
				// Interactive cards (permission flow + status cards).
				"cardkit:card:write",
				"cardkit:card:read",
				// Diagnostics foundation: introspect the app's own
				// granted scopes via /application/v6/applications.
				"application:application:self_manage",
			},
		},
		Events: registration.AppAddonsEvents{
			Items: registration.AppAddonsEventItems{
				Tenant: []string{
					"im.message.receive_v1",
				},
			},
		},
		Callbacks: registration.AppAddonsCallbacks{
			Items: []string{
				"card.action.trigger",
			},
		},
	}
}
