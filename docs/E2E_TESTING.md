# Feishu E2E testing guide

This guide verifies the M2 PR #4 Feishu channel and daemon wiring with a real
Feishu bot. It intentionally stops before the Gateway: messages are printed by
`nightme run` as `received: <text>` instead of being sent to an agent.

## Prerequisites

- A Feishu developer account and a Feishu mobile app account that can scan a
  QR code.
- `nightme` built or installed locally.
- Go 1.22 or newer when building from source.
- Network access to Feishu's Open Platform endpoints.

## Test procedure

1. Build the binary if it is not already installed:

   ```bash
   go build -o bin/nightme ./cmd/nightme
   ```

2. Start the one-click Feishu registration flow:

   ```bash
   ./bin/nightme auth login feishu
   ```

   A QR code and verification URL appear in the terminal. Scan the QR code
   with the Feishu mobile app and approve the requested scopes. On success,
   `app_id` and `app_secret` are saved to
   `~/.config/nightme/config.yaml` with restrictive permissions. A manually
   configured `feishu.app_id` / `feishu.app_secret` pair works as well.

3. Start the daemon:

   ```bash
   ./bin/nightme run
   ```

   The terminal should show `Feishu WebSocket connected`. Keep this process
   running while testing. Stop it with `Ctrl-C` (SIGINT) when finished.

4. In the Feishu mobile or desktop app, open a chat with the bot and send:

   ```text
   hello
   ```

5. Confirm that the daemon terminal prints:

   ```text
   received: hello
   ```

   This output is the expected PR #4 echo. It proves that the WebSocket event
   was decoded into the common `channel.Message` shape and reached the daemon.

## Troubleshooting

### Webhook versus WebSocket

`nightme run` uses Feishu's **WebSocket long connection**, not an HTTP webhook
callback. In the Feishu app configuration, enable the bot capability and
subscribe to the receive-message event for the long-connection mode. A webhook
URL or a webhook-only event subscription will not deliver messages to this
command.

### Permissions and scopes

The bot needs permission to receive messages addressed to it. For the one-click
flow, approve the requested receive and send scopes, including the receive
message event. For a manually created app, check that the tenant has granted
the corresponding `im:message:*` permissions and that the bot is available to
the test user/chat.

### Event subscription

Verify that the event name is `im.message.receive_v1` and that the application
has been published or otherwise enabled for the tenant used by the test. In a
group chat, also verify the relevant @mention permission; a direct message is
the simplest first test.

### No connection message or authentication errors

- Check `nightme auth status feishu` and confirm that an App ID is present.
- Re-run `nightme auth login feishu --force` if the credentials are stale.
- Confirm that `app_secret` was not copied with surrounding whitespace.
- Check that the machine can reach Feishu's Open Platform endpoint and that
  the app has not been disabled or had its permissions revoked.

### The bot receives nothing

Start with a direct message containing only `hello`, then inspect the Feishu
subscription and bot-availability settings. Images, files, and other rich
message types are not part of this PR's receive test; the adapter only
normalizes text content.

## Known limitations in M2 PR #4

- **The bot does not reply in Feishu yet.** PR #4 has no Gateway connection;
  `nightme run` only prints `received: <text>` locally. PR #5 adds Gateway
  routing and the Feishu round-trip.
- **A session is not created automatically.** In the eventual Gateway flow, the
  user must first send `/cwd` and then `/run` in the chat. The Gateway is not
  connected in PR #4, so those commands are not active in this E2E test.
- The permission-card renderer is present as an adapter rendering primitive,
  but card-action routing is not connected to a running Gateway.
- WebSocket reconnect and Feishu-side delivery behavior remain dependent on the
  official Lark SDK and the app's tenant permissions.
