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

	// AppPreset pre-fills the app's name/avatar on the consent
	// page; nil means an empty preset (the user types whatever
	// they want).
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
// defaulted to DefaultAddons(); opts.Out falls back to os.Stdout.
//
// AppPreset is left nil when opts.AppPreset is nil. ExistingAppID
// and CreateOnly pass through unchanged.
func NewFeishuAuth(opts FeishuAuthOptions) *FeishuAuth {
	addons := opts.Addons
	if addons == nil {
		addons = DefaultAddons()
	}
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	return &FeishuAuth{
		opts:   opts,
		addons: addons,
		preset: opts.AppPreset,
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
			fmt.Fprintf(f.out, "status: %s\n", info.Status)
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
func (f *FeishuAuth) printQRCode(info *registration.QRCodeInfo) {
	fmt.Fprintf(f.out, "Scan this QR code with Feishu mobile, or open this URL:\n%s\n(expires in %d seconds)\n\n",
		info.URL, info.ExpireIn)
	// Errors here mean stdout is broken (closed pipe); nothing for
	// the user to do, and registration.RegisterApp will still
	// block on the polling loop.
	_ = RenderASCII(info.URL, f.out, false)
}

// DefaultAddons returns the scope + event + callback set nightme
// asks for at QR-code registration time. Callers may override at
// construction.
//
// Scope rationale (covers all v0.2 + near-term v0.3 use cases):
//
//	im:message:send_as_bot             send text / interactive / image / file
//	                                  (msg_type discriminator on the send
//	                                  endpoint; one scope covers all)
//	im:message:receive_v1              receive message events (the SDK
//	                                  binds this name to both the scope
//	                                  and the event)
//	im:message.reactions:write_only    ⏳/🔄/✅ receipts on incoming msgs
//	im:message.group_at_msg:readonly   bot triggered by @-mention in groups
//	im:message.p2p_msg:readonly        bot triggered in 1:1 chats
//	im:message:readonly                fetch historical messages AND
//	                                  download inbound attachment
//	                                  resources via F-14 passthrough
//	im:resource                        upload images/files for sending
//	im:chat:read                       read chat metadata
//	im:chat.members:bot_access         read member list of chats bot is in
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
				"im:message:send_as_bot",
				"im:message.reactions:write_only",
				"im:message:readonly",
				"im:message:receive_v1",
				"im:message.group_at_msg:readonly",
				"im:message.p2p_msg:readonly",
				"im:resource",
				"im:chat:read",
				"im:chat.members:bot_access",
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
