// Package feishu implements login.Provider for the Feishu (飞书) channel.
//
// The provider wraps the larksuite/oapi-sdk-go v3 `registration`
// scene: it asks Feishu for a device code, prints the verification
// URL + ASCII QR to the terminal via the callback, then blocks
// polling until the user scans and approves on the Feishu consent
// page (or ctx is cancelled).
//
// Upon success the registration returns the new App's ClientID /
// ClientSecret, which we surface as a generic login.Credentials. The
// CLI is responsible for persisting them into the on-disk Config.
package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"

	"github.com/cnlangzi/nightme/internal/login"
)

// greetTimeout caps the channel-side hello call so a Feishu stall
// cannot keep the CLI open after the QR scan finished. 15s is well
// above the p99 create_message latency observed on the openclaw-lark
// reference bot (sub-second in normal operation, sub-5s in the
// worst corner cases) but well below the 10-minute user-visible
// login deadline.
const greetTimeout = 15 * time.Second

// Options configures a single login flow run. AppPreset and
// DefaultAddons are merged with built-in fallbacks in New
// so callers only need to set what they care about.
type Options struct {
	// Addons overrides the default scopes/events. nil = use
	// DefaultAddons.
	Addons *registration.AppAddons

	// AppPreset pre-fills the app's name/description/avatar on the
	// consent page; nil means use DefaultAppPreset (the nightme
	// brand default — "NightMe" / "Sleep tight, NightMe code all
	// night."). The user can still edit the fields on the consent
	// page before submitting; the final values are whatever they
	// enter.
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

// Provider is a login.Provider for Feishu.
type Provider struct {
	opts   Options
	addons *registration.AppAddons
	preset *registration.AppPreset
	out    io.Writer

	// ownerOpenID is the channel-side identifier of the user who
	// just completed the QR-code flow. Captured by Login from
	// registration.RegisterAppResult.UserInfo.OpenID and consumed
	// by Greet. Empty when the SDK did not echo user_info back
	// (rare — see Greet's no-op contract).
	ownerOpenID string

	// larkClient is built lazily inside Login() from the
	// freshly-issued AppID/AppSecret. nil before Login.
	larkClient *lark.Client

	// sendDM is the SDK-bound send boundary used by Greet. Defaults
	// to defaultSendDM (the real Feishu CreateMessage call) and is
	// the testable seam — tests can swap it to record calls without
	// instantiating a real lark.Client.
	sendDM sendDMFunc
}

// sendDMFunc is the testable seam for Greet's per-message send.
// The real implementation hits Feishu's CreateMessage API; tests
// substitute a stub that captures (ctx, body) tuples without
// touching the network.
type sendDMFunc func(ctx context.Context, body login.GreetingBody) error

// New returns a ready-to-Login Provider. opts.Addons is
// defaulted to DefaultAddons(); opts.AppPreset is defaulted to
// DefaultAppPreset(); opts.Out falls back to os.Stdout.
//
// ExistingAppID and CreateOnly pass through unchanged.
func New(opts Options) *Provider {
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
	return &Provider{
		opts:   opts,
		addons: addons,
		preset: preset,
		out:    out,
		sendDM: nil, // wired in Login() once larkClient is ready; nil = no-op in Greet
	}
}

// Name implements login.Provider.
func (f *Provider) Name() string { return "feishu" }

// Login runs the device-authorization flow against Feishu and
// returns the freshly-issued credentials. It blocks until the user
// scans + approves (default ~10 min, see registration.Options).
//
// The implementation is intentionally thin: registration.RegisterApp
// does the heavy lifting (HTTP, polling, error wrapping). All we add
// is the QR callback, the status callback, and a sentinel-error wrap
// so callers can errors.Is-match without depending on the SDK.
//
// Login does NOT itself send the greeting DM — that is the
// orchestrator's job (calls Provider.Greet right after Login
// returns). Login only captures the owner ID + builds the lark
// client so Greet is ready to fire.
func (f *Provider) Login(ctx context.Context) (*login.Credentials, error) {
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
			return nil, fmt.Errorf("feishu: %w: %v", login.ErrLoginTimeout, err)
		}
		var regErr *registration.RegisterAppError
		if errors.As(err, &regErr) {
			return nil, fmt.Errorf("feishu: %w: %s: %s", login.ErrLoginFailed, regErr.Code, regErr.Description)
		}
		return nil, fmt.Errorf("feishu: register: %w", err)
	}

	// Capture the owner ID for Greet. tenant_brand is no longer
	// used since Strategy B (bilingual post envelope) lets the
	// receiver's Feishu client pick the locale — see
	// docs/channel/feishu.md §Greeting Localization.
	if result.UserInfo != nil {
		f.ownerOpenID = result.UserInfo.OpenID
	}
	// Build the SDK client here (after RegisterApp) because the
	// client authenticates with the freshly-issued AppID/Secret.
	// We only construct it when an owner was captured — without
	// one, Greet is a no-op and a client would be wasted. Wire
	// the Greet seam at the same point so the function value
	// captures the freshly-built lark.Client + owner ID.
	if f.ownerOpenID != "" {
		f.larkClient = lark.NewClient(result.ClientID, result.ClientSecret)
		f.sendDM = f.defaultSendDM
	}

	name := ""
	if f.preset != nil {
		name = f.preset.Name
	}
	return &login.Credentials{
		AppID:     result.ClientID,
		AppSecret: result.ClientSecret,
		AppName:   name,
		CreatedAt: time.Now(),
	}, nil
}

// Greet implements login.Provider. It fires the canonical NightMe
// greeting at the user who just completed the QR-code flow.
//
// Each greeting element is sent as one Feishu `post` message with
// both `zh_cn` and `en_us` blocks. The receiver's Feishu client
// picks the locale tag matching its UI language, so the same
// payload renders correctly for any user regardless of their
// locale — no tenant_brand guessing required. See
// docs/channel/feishu.md §Greeting Localization for the empirical
// verification of the locale-pick behavior.
//
// Per-message budget: each post gets its own greetTimeout context,
// so a slow first post doesn't steal budget from the second.
//
// Greet does NOT take an owner argument — the recipient identity
// is captured by Login and lives on the Provider struct. The
// caller just needs to know "send the greeting"; the provider has
// already worked out "to whom".
//
// Best-effort: a failed greeting does NOT roll back the successful
// registration. The CLI orchestrator is expected to log + swallow
// the error. We use a fresh context.Background() so a cancelled
// parent ctx (e.g. user hit Ctrl+C after the scan succeeded) does
// not abort the greeting before it even starts.
//
// Returns nil (no-op) when sendDM is nil (Login was bypassed in
// tests, or the SDK did not echo user_info back so the function
// field was never wired) — better to skip silently than to attempt
// a malformed send. The reason is logged so the operator can tell
// "I forgot to ask" from "Feishu did not return one".
//
// ctx is the parent of the per-message deadline: each iteration
// derives its own deadline-capped context from ctx, so a cancelled
// caller (orchestrator's timeout, user Ctrl+C) aborts the
// remaining sends without abandoning the in-flight one.
func (f *Provider) Greet(ctx context.Context, messages login.GreetingMessages) error {
	if f.sendDM == nil {
		fmt.Fprintln(f.out, "greeting skip: sendDM not wired (Login bypassed or no owner captured)")
		return nil
	}
	if len(messages) == 0 {
		return nil
	}
	fmt.Fprintf(f.out, "Sending %d greeting DM(s)...\n", len(messages))

	for i, body := range messages {
		msgCtx, cancel := context.WithTimeout(ctx, greetTimeout)
		err := f.sendDM(msgCtx, body)
		cancel()
		if err != nil {
			return fmt.Errorf("feishu: greet: send post %d: %w", i, err)
		}
	}
	return nil
}

// postLang is the per-locale envelope Feishu expects inside a
// msg_type=post content string. Per docs/feishu/create_message §post
// schema, every locale must carry a `content` array of paragraphs
// of element nodes — the SDK does not surface a builder for the
// inner shape, so we hand-roll the JSON envelope and pass it as
// the `content` string.
type postLang struct {
	Content [][]postNode `json:"content"`
}

// postNode is one inline element inside a paragraph. `tag:"text"`
// yields a plain text span; the family also supports at / a / img,
// but we only need text for the greeting.
type postNode struct {
	Tag  string `json:"tag"`
	Text string `json:"text"`
}

// buildPostEnvelope renders the bilingual GreetingBody into the
// Feishu post envelope. The client picks the locale tag matching
// the receiver's UI language — that's why we ship both halves in
// one message. The post form is the documented multi-language
// carrier; see docs/channel/feishu.md §19.
//
// Pulled out as a pure helper so the wire-level shape can be unit
// tested without instantiating a lark.Client.
func buildPostEnvelope(body login.GreetingBody) (string, error) {
	envelope := struct {
		ZhCn postLang `json:"zh_cn"`
		EnUs postLang `json:"en_us"`
	}{
		ZhCn: postLang{Content: [][]postNode{{{Tag: "text", Text: body.Chinese}}}},
		EnUs: postLang{Content: [][]postNode{{{Tag: "text", Text: body.English}}}},
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

// defaultSendDM is the production implementation wired into f.sendDM
// at Login time. It builds the post envelope via buildPostEnvelope
// and hits Feishu's CreateMessage API.
//
// json.Marshal handles quote-escape in both bodies so a future
// translation that contains " or \ round-trips safely.
func (f *Provider) defaultSendDM(ctx context.Context, body login.GreetingBody) error {
	envelope, err := buildPostEnvelope(body)
	if err != nil {
		return fmt.Errorf("feishu: greet: marshal envelope: %w", err)
	}
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeOpenId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(f.ownerOpenID).
			MsgType("post").
			Content(envelope).
			Build()).
		Build()
	resp, err := f.larkClient.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if resp != nil && !resp.Success() {
		return fmt.Errorf("rejected: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// printQRCode renders the QR for the user. The QR itself is the
// thing humans visually compare to a screenshot — get it right.
//
// The trailing "Waiting for you to scan…" line tells the user what
// the next step is. We intentionally do not show it inside the
// OnStatusChange callback: the SDK calls OnStatusChange on every
// poll cycle while waiting, so printing there would spam the
// terminal once a second for ten minutes.
func (f *Provider) printQRCode(info *registration.QRCodeInfo) {
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
func (f *Provider) printStatus(info *registration.StatusChangeInfo) {
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
// "Sleep tight, NightMe code all night.". Callers can override any
// field at construction time via Options.AppPreset; the user
// can still edit them on the consent page before submitting.
func DefaultAppPreset() *registration.AppPreset {
	return &registration.AppPreset{
		Name: "NightMe",
		Desc: "Sleep tight, NightMe code all night.",
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
//	                                  (F-watch: kept alongside group_msg;
//                                   per-chat gate in nightme decides
//	                                   drop or pass; see SPEC §3.1.1)
//	im:message.group_msg               F-watch: receive ALL group messages
//	                                  (not just @-mentions). Default-on at
//	                                  install time; the runtime messageDispatcher
//	                                  passes ChatSession.WatchMode + HasMention
//	                                  to chatsession.Manager.AcceptInbound
//	                                  to gate processing. NOT :readonly
//	                                  because bot needs to reply.
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
// F-watch (2026-08-03): `im:message.group_msg` is included by
// default. Decision record: Devin rejected an opt-in CLI flag
// (`--group-messages`) in favour of "default-on, opt-out per chat
// via `/watch off`". Users who want the bot to never see
// non-mention group messages run `/watch off` once per chat; the
// feishu platform still delivers them but nightme drops them at
// the dispatcher gate (docs/SPEC.md §3.1.1).
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
				// Group triggers: at-only + all (F-watch).
				"im:message.group_at_msg:readonly",
				"im:message.group_msg",
				// 1:1 triggers.
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