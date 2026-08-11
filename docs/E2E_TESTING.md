# Feishu E2E testing guide

This guide verifies the end-to-end round-trip: a Feishu chat
talks to nightme via the Gateway, which drives ChatSession
lifecycle and forwards user input to the live agent. Messages
flow both directions: chat → agent → chat.

## Prerequisites

- A Feishu developer account and a Feishu mobile app account that can scan a
  QR code.
- `nightme` built or installed locally.
- Go 1.22 or newer when building from source.
- Network access to Feishu's Open Platform endpoints.
- An AI Coding CLI installed and on `PATH` (e.g. `claude`, `codex`,
  `opencode`, `pi`). The agent's binary must be discoverable by
  `exec.LookPath` — verify with `which claude`.

## Quick Start

1. Build the binary if it is not already installed:

   ```bash
   go build -o bin/nightme ./cmd/nightme
   ```

2. Register the Feishu app (one-click QR flow):

   ```bash
   ./bin/nightme login feishu
   ```

   A QR code and verification URL appear in the terminal. Scan the QR code
   with the Feishu mobile app and approve the requested scopes. On success,
   `app_id` and `app_secret` are saved to
   `~/.nightme/config.yaml` with restrictive permissions. A manually
   configured `feishu.app_id` / `feishu.app_secret` pair works as well.

3. Configure the agent list in `~/.nightme/config.yaml`:

   ```yaml
   primary: claude
   agents:
     - name: claude
       command: claude
     - name: codex
       command: codex
     - name: opencode
       command: opencode
   ```

   Each agent must be on `PATH`; `nightme config` runs an interactive menu
   that generates this section. `primary` is the agent used when a chat
   starts and has no override.

4. Start the daemon:

   ```bash
   ./bin/nightme run
   ```

   The terminal should show `Feishu WebSocket connected`. Keep this
   process running while testing. Stop it with `Ctrl-C` (SIGINT) when
   finished.

## Round-trip test procedure

The round-trip exercises the full Gateway: slash commands drive
ChatSession lifecycle, plain text is forwarded to the agent, and
agent output is rendered back to the chat.

5. In the Feishu mobile or desktop app, open a 1:1 chat with the
   bot and send each of the following in order. The expected
   bot reply is shown for each step.

   | # | User sends | Bot replies |
   |---|------------|-------------|
   | 1 | `/cwd /tmp` | `Workspace set to /tmp.` |
   | 2 | `hello` | `hello` (from the agent's PTY echo) |
   | 3 | `/use claude` | `Switched to claude` (or new spawn if not yet started) |
   | 4 | `/use codex` | `Switched to codex` (new spawn, claude process kept alive in pool) |
   | 5 | `/use claude` | `Switched to claude` (reuses prior claude process; conversation context preserved) |
   | 6 | `/close` | per-entry list of killed sessions |
   | 7 | `/list` | session table (active / detached status, workspace, pid) |

   > If you don't have API keys for the real CLIs, register an
   > `echo` binary as an agent (`command: /bin/echo`) for steps 2-5.

6. Verify the daemon's terminal output. Each user message appears
   as `received: <text>`, and outbound replies are sent to Feishu
   (no log line in the daemon by default).

8. To prove the bridge is real, run `ps -ef | grep claude` (or
   `echo`) in another terminal during steps 2-5 — the spawned
   child should be alive with the recorded PID.

> **Screenshots:** captured during real Feishu runs are tracked
> alongside the docs in `docs/images/`.

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
has been published or otherwise enabled for the tenant used for the test. In a
group chat, also verify the relevant @mention permission; a direct message is
the simplest first test.

### No connection message or authentication errors

- Inspect `~/.nightme/config.yaml` and confirm that an `feishu.app_id` is present.
- Re-run `nightme login feishu` if the credentials are stale.
- Confirm that `app_secret` was not copied with surrounding whitespace.
- Check that the machine can reach Feishu's Open Platform endpoint and that
  the app has not been disabled or had its permissions revoked.

### The bot receives nothing

Start with a direct message containing only `hello`, then inspect the Feishu
subscription and bot-availability settings. Images, files, and other rich
message types are not part of this PR's receive test; the adapter only
normalizes text content.

### `/use` returns "unknown agent"

The agent registry is built from `config.yaml`'s `agents` list.
Make sure the section exists and the CLI binary is on `PATH` —
`which claude` should succeed. Restarting `nightme run` re-reads
the config.

### `/use` returns "binary not found"

The agent's `Detect()` ran `exec.LookPath` and failed. Install the
CLI or fix the path. The error message comes from the OS's
`exec.LookPath` so it includes the unresolved name.

### Bot replies feel laggy

PTY mode emits an EventText per Read syscall, which renders as
one Feishu send per chunk. For chatty CLIs (e.g. Claude Code's
spinner), the channel adapter's `SendLongMessage` chunks at
newline boundaries. If you see one-character frames, the issue is
the agent's TTY discipline — not nightme.

## Known limits

- **One chat at a time per agent process.** ChatSession is 1:1
  bound to its IM chat; opening a second chat with the bot
  creates a new ChatSession with an independent AgentSession
  pool. Sharing one agent process across chats is not supported.
- **Single account per Feishu tenant.** Per F-22, the config
  holds one `app_id` / `app_secret`. Multi-account support is not
  implemented.
- **WebSocket reconnect behavior** depends on the official Lark SDK
  and the app's tenant permissions. Active reconnect (F-41)
  reduces the worst-case wait to 30s. If the daemon drops its
  connection, restart `nightme run`; the persisted session table
  replays `StatusRunning` records as `StatusDetached` so a
  subsequent message will re-spawn cleanly.