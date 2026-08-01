# F-14: Image / File Attachment Passthrough

> **Status**: implemented (v0.2 — receive only)
> **Milestone**: v0.2
> **Related**: [`../FEATURES.md`](../FEATURES.md#f-14-图片--文件附件透传)

## 1. Description

When a user sends an image, file, audio, or video to nightme in Feishu, the binary is downloaded into a per-session inbox directory and the agent receives a single user turn containing both the user's caption text and the local paths to the downloaded files. Stickers are silently skipped (Feishu blocks their resource download). Sending attachments back to the user is out of scope for v0.2.

**v0.2 scope** (this implementation):

- Receive image / file / audio / video messages from Feishu
- Receive post rich-text messages with embedded images
- Download binaries with 3-attempt retry (500ms → 1500ms backoff)
- Store under `~/.nightme/inbox/<session_id>/<filename>` (0700)
- Forward to agent as `attachment (<type>): <abs path>` lines, mixed with caption text
- User-facing notification when all or some downloads fail (separate chat reply, never mixed into the agent turn)
- One Feishu message = one agent turn (never split caption from attachment)

**Out of scope (Phase 2)**:

- Sending attachments back to the user (no `SendImage` / `SendFile` API path)
- Stream-json content-block upgrade for the Claude Code bridge (relies on Claude Code's path auto-detection in TUI/stream-json today)
- Inbox TTL / size-cap cleanup (manual today)
- Post `tag:"media"` (inline video in rich text) extraction
- Sticker forwarding (Feishu currently blocks download — keep an eye on API change)

## 2. Data flow

```
Feishu inbound
  → channel.Message {Text, MessageID, Attachments}
       ├── extractAttachments (handleMessage) populates Attachments
       └── Attachments = [{Type, FileKey, FileName}, ...]
  → DownloadAttachments (sync, populates LocalPath / Error per attachment)
       ├── retry x3 with exponential backoff
       └── returns DownloadResult {Atts, AllFailed, FailureKeys}
  → Dispatcher (cmd/nightme/run.go):
       ├── AllFailed? → Send ❌ notification, drop the message (skip gateway)
       ├── partial?   → Send ⚠️ notification, forward with successful paths
       └── ok?        → forward normally
  → BuildForwardedText → single string "<caption>\nattachment (...): /path\n..."
  → Gateway fallback handler → InputBuffer.Add(forwardedText, messageID)
       ├── idle: bypass → sess.SendText(forwardedText)
       └── busy: queue → OnTurnEnded flush → sess.SendText(combined)
```

The `InputBuffer` already coalesces multiple user messages into one agent turn (see `internal/session/input_buffer.go:OnTurnEnded`). So if 3 image+caption messages arrive while the agent is busy, all 3 download, all 3 queue, and one `SendText` carries all 3 captions + all 3 attachment paths to the agent.

## 3. Storage

```
~/.nightme/inbox/
  └── <session_id>/
      ├── img_v2_abc.png       # from Feishu image msg
      ├── report.pdf           # from Feishu file msg
      └── voice.m4a            # from Feishu audio msg
```

- Per-session isolation: concurrent sessions in different chats do not stomp on each other's downloads.
- Mode 0700 on the directory, 0600 on files (the SDK writes 0666 by default; we chmod).
- Filename collision handling: if two attachments share a name, the second gets `_<n>` suffix (`report_2.pdf`, etc.).
- No cleanup daemon — the user manages their own disk.

## 4. Failure handling

**All downloads fail** (every attachment returned Error after retries):

- The inbound message is **dropped entirely** — never reaches the agent. Sending a half-broken user turn (caption without the image it referred to) would mislead the agent.
- A separate Feishu notification goes to the user: `❌ 文件下载失败 (重试 3 次后)\n请重新发送该消息。`
- The receipt (F-25) for this user message transitions Waiting → Completed with the failure text in the reply line.

**Some downloads succeed** (partial failure):

- The successful attachments' paths go through to the agent (better than nothing).
- A separate Feishu notification lists the failed keys: `⚠️ 部分文件下载失败: img_v2_xxx, file_yyy\n请重新发送未下载成功的文件。`
- The receipt transitions normally (Waiting → Executing → Completed).

**No attachments carried** (text-only or unknown msg_type):

- No download attempted. Forwarding path is the same as today (text-only).

## 5. Scope

The F-22 nightme registration now requires:

```
im:message:send_as_bot         # existing
im:message.reactions:write_only  # F-25
im:message:readonly            # NEW: download message resources (F-14)
im:message:receive_v1          # existing
```

Existing nightme users must re-run `nightme auth login feishu` to bind the new scope. The QR consent page will list "获取消息资源" alongside the existing permissions.

## 6. Tests

- `internal/channel/feishu/extract_test.go` — 19 unit tests covering each Feishu msg_type, post-msg paragraph walks, ignored tags, and `BuildForwardedText` edge cases.
- `internal/channel/feishu/attachment_test.go` — inbox dir creation, mode 0700, unique-path collision, backoff schedule.
- Manual integration: send image / file / sticker / post in a real Feishu DM; observe daemon logs and the agent's reasoning over the downloaded files.
