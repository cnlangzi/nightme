package slack

import "net/url"

// ManifestURL builds Slack's "create app from manifest" deep link.
//
// Clicking it opens the app-creation flow with every scope and event
// already filled in, so the user only picks a workspace and confirms.
// This matters most on a phone: pasting a long config blob into the
// console's code editor is the single worst step of the setup, and
// this skips it entirely.
//
// Documented at https://api.slack.com/tools/manifests.
func ManifestURL() string {
	return "https://api.slack.com/apps?new_app=1&manifest_yaml=" +
		url.QueryEscape(AppManifest)
}

// AppManifest is the Slack app manifest for nightme, in YAML form.
//
// Slack's "Create New App → From an app manifest" accepts both JSON
// and YAML; YAML was chosen because it carries far less syntactic
// noise (no braces, no quotes around keys) and reads more naturally
// inside the walkthrough box where the user is reading it.
//
// Pasting this into the YAML tab of the manifest editor configures
// every scope and event subscription in one step, so the user never
// has to hunt through the settings UI.
//
// Scope policy: everything the adapter can use is requested up front
// (decided 2026-08-29). The bot is self-hosted, in the operator's
// own workspace, so there is no third party to protect from the
// broad grant — and a narrow grant would silently break features
// later, after the user has forgotten which checkbox they skipped.
//
// The message.* subscriptions are what make `/watch all` possible.
// Subscribing to app_mention alone (as cc-connect does) means the bot
// never sees a channel message it was not tagged in, so "watch
// everything" could not be implemented at all. The cost is that an
// @-mention arrives twice — once per subscription — which the
// adapter's dedup index absorbs.
//
// assistant_view enables the AI-app surface that chat.startStream
// and assistant.threads.setStatus depend on.
const AppManifest = `display_information:
  name: nightme
  description: Your pair programmer. Set it running, stay in the loop.
  background_color: "#1a1a2e"
features:
  bot_user:
    display_name: nightme
    always_online: true
  assistant_view:
    assistant_description: Drive your local coding agents from Slack.
    suggested_prompts: []
oauth_config:
  scopes:
    bot:
      - app_mentions:read
      - assistant:write
      - channels:history
      - channels:read
      - chat:write
      - chat:write.public
      - commands
      - files:read
      - files:write
      - groups:history
      - groups:read
      - im:history
      - im:read
      - im:write
      - mpim:history
      - mpim:read
      - reactions:read
      - reactions:write
      - users:read
settings:
  event_subscriptions:
    bot_events:
      - app_mention
      - assistant_thread_started
      - message.channels
      - message.groups
      - message.im
      - message.mpim
  interactivity:
    is_enabled: true
  org_deploy_enabled: false
  socket_mode_enabled: true
  token_rotation_enabled: false
`
