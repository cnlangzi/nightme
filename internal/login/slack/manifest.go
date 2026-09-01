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
  name: NightMe
  description: Your pair programmer. Set it running, stay in the loop.
  background_color: "#1a1a2e"
features:
  bot_user:
    display_name: NightMe
    always_online: true
  # Two related toggles in App Home — both must be set explicitly,
  # because Slack defaults both to false / read-only when omitted,
  # and the resulting UX is "sending messages to this app has been
  # turned off" with no message.im events ever firing.
  #
  #   messages_tab_enabled: true
  #     Show the Messages tab at all (Display Chat Tab toggle in the
  #     UI). Defaults to false when omitted.
  #
  #   messages_tab_read_only_enabled: false
  #     Let users send messages TO the bot (the "Allow users to send
  #     Slash commands and messages from the chat tab" checkbox in
  #     the UI). The name is the inverse of its effect: false means
  #     "not read-only" → users CAN send. Defaults to true (read-
  #     only) when omitted. Without this set, the Messages tab is
  #     visible but users cannot type into it.
  #
  # Both default to the broken state, so both must be pinned in the
  # manifest — otherwise any reinstall silently breaks inbound DMs
  # and the adapter never sees a message.im. See
  # docs.slack.dev/surfaces/app-home#messages-tab.
  app_home:
    messages_tab_enabled: true
    messages_tab_read_only_enabled: false
  assistant_view:
    assistant_description: Drive your local coding agents from Slack.
    suggested_prompts: []
  # 2026-09-01 改用 dollar-sign 前缀消息匹配（docs/channel/slack.md §6.2.1）。
  # 不再在 manifest 注册任何 slash command——dollar-sign 不在 Slack 任何保留
  # 字列表，0 reinstall 摩擦。加新命令 = 0 Slack 改动（只改
  # internal/command/<name>/cmd.go，dispatcher 自动识别）。
  #
  # 旧设计（保留在 git history）曾是 12 个 slash_commands + slash-kclose
  # 翻译绕开 Slack 的 slash-close 保留字。新设计 dollar-close 直接就是 close。
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
