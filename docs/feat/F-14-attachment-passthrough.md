# F-14: Image / File Attachment Passthrough

> **Status**: implemented (v0.2 — receive only; v1.1 数据结构升级为 blocks)
> **Milestone**: v0.2
> **Related**: [`SPEC.md`](../SPEC.md) v1.1 §2.1; [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md); [`F-25-input-buffer.md`](./F-25-input-buffer.md); [`F-20-gateway.md`](./F-20-gateway.md) §5

---

## 1. Description

When a user sends an image, file, audio, or video to nightme in Feishu, the binary is downloaded into a per-session inbox directory and the agent receives a structured user turn (`[]agent.ContentBlock`) containing both the user's caption text and the local paths to the downloaded files. Stickers are silently skipped (Feishu blocks their resource download). Sending attachments back to the user is out of scope.

**v0.2 scope** (this implementation):

- Receive image / file / audio / video messages from Feishu
- Receive post rich-text messages with embedded images
- Download binaries with 3-attempt retry (500ms → 1500ms backoff)
- Store under `~/.nightme/inbox/<session_id>/<filename>` (0700)
- Forward to agent as structured `ContentBlock` slice (text + image + file blocks)
- User-facing notification when all or some downloads fail (separate chat reply, never mixed into the agent turn)
- One Feishu message = one agent turn (never split caption from attachment)

**Out of scope**:

- Sending attachments back to the user (no `SendImage` / `SendFile` API path)
- Stream-json content-block upgrade for the Claude Code bridge
- Inbox TTL / size-cap cleanup (manual today)
- Post `tag:"media"` (inline video in rich text) extraction
- Sticker forwarding (Feishu currently blocks download — keep an eye on API change)

---

## 2. Data flow (v1.1)

```
Feishu inbound
  → channel.Adapter.handleMessage
       ├── extractAttachments populates gateway.InboundMessage.Attachments
       └── Attachments = [{Type, FileKey, FileName, LocalPath, Error}, ...]
  → DownloadAttachments (sync, populates LocalPath / Error per attachment)
       ├── retry x3 with exponential backoff
       └── returns DownloadResult {Atts, AllFailed, FailureKeys}
  → channel.Adapter.handleMessage 将 InboundMessage 推入 ch.Incoming()

Gateway.pumpInbound → dispatchLoop → DispatchInbound (inboundDispatcher) → messageDispatcher
  ├── AllFailed? → ch.Send(OutText ❌ notification) + drop → skip receipt
  ├── partial?   → ch.Send(OutText ⚠️ notification) + 继续
  └── ok?        → 继续

  → BuildBlocks(msg.Text, msg.Attachments) → []agent.ContentBlock
       ├── 第一个 ContentText block = msg.Text
       ├── 成功的 attachments 转为 ContentImage / ContentFile blocks
       └── 失败的 attachments 丢弃（前面已通知 user）

  → messageDispatcher(a) ch.CreateReceipt(ctx, msg.ChatID, msg.MessageID, blocks)
  → messageDispatcher(b) receipts[userMsgID] = {..., state: Pending}
  → messageDispatcher(c) session.QueueUserMessage(blocks, userMsgID)
       ├── Idle → 立即 SendBlocks(blocks) → ch.UpdateReceipt(executing)
       └── Busy → 入队 → onFlush 钩子触发时批量 SendBlocks + UpdateReceipt
```

**关键 v1.1 变化**（相对 v0.2 文档）：
- ❌ v0.2：`BuildForwardedText → single string`（文本拼接 attachment 路径）
- ✅ v1.1：`BuildBlocks → []agent.ContentBlock`（结构化 blocks，image 是 ContentImage block 含 base64 / path，file 同理）
- ❌ v0.2：`InputBuffer.Add(forwardedText, messageID)`
- ✅ v1.1：`InputBuffer.Add(blocks, messageID)`（buffer 持 `[]bufferedMsg{Blocks, UserMsgID}`）
- ❌ v0.2：`session` 间接 import feishu via `BuildForwardedTextFromBlocks`
- ✅ v1.1：`BuildBlocks` 是 `internal/channel/feishu` 包私有函数；session 不知道 text 拼接细节

---

## 3. blocks 流经的三层

```
[Channel Adapter]                  [Gateway]                       [Session.InputBuffer]
       │                                │                                    │
       │ BuildBlocks(msg.Text, Att)     │                                    │
       │ → []ContentBlock               │                                    │
       │                                │                                    │
       │ CreateReceipt(blocks) ─────────│                                    │
       │ → opaque Receipt               │ receipts[umid] = {rcpt, state}      │
       │                                │                                    │
       │                                │ QueueUserMessage(blocks, umid) ────│
       │                                │                                    │ Add(blocks, umid)
       │                                │                                    │
       │                                │                                    │ Idle → SendBlocks
       │ UpdateReceipt(Executing) ◄─────│                                    │
       │                                │                                    │
       │                                │                                    │ Busy → buffer
       │ UpdateReceipt(Executing) ◄─────│ onFlush hook (gw 注入) ◄────────────│
       │                                │                                    │ SendBlocks(combined)
```

**关键 invariant**：blocks 是 gateway 流转的"通用货币"，每层接收/产出 `[]ContentBlock`：
- Channel.CreateReceipt 接收 blocks → 返回 receipt + 内部格式化 receipt 文本
- Session.InputBuffer.Add 接收 blocks → 缓冲或 dispatch
- Session.AgentSession.SendBlocks 接收 blocks → Bridge 编码为 agent 原生格式（stream-json content-array / PTY "@<path>" / ACP content-array）

---

## 4. ContentBlock 类型

```go
// internal/agent/agent.go
type ContentBlock struct {
    Type      ContentBlockType  // text / image / file
    Text      string            // for ContentText
    Path      string            // for ContentImage / ContentFile (绝对路径)
    MediaType string            // MIME (image/png; advisory for file)
}
```

| Block.Type | 用途 | Bridge 处理（Claude Code stream-json）|
|------------|------|-------------------------------------|
| `ContentText` | 文本段 | 转 `{type:"text", text:...}` |
| `ContentImage` | 图片 | base64-inlined `{type:"image", source:{type:"base64", media_type, data}}`（vision）|
| `ContentFile` | 文件 | PDF：base64-inlined `{type:"document", ...}`；其他：转 text 引用 `<path>` |

**`/cwd` 和 `/run` 不在 receipt path 上**：slash command handler 走 gateway.handler.* 直接 reply，不创建 receipt。

---

## 5. Download error 通知模板

| 场景 | 用户通知 | receipt 行为 |
|------|---------|-------------|
| AllFailed（所有 attachment 下载失败）| `❌ N attachments failed to download, please retry` | 不创建 receipt；message 丢弃 |
| Partial（部分失败）| `⚠️ K of N attachments failed; sending the rest` | 创建 receipt；只把成功的 blocks 传给 agent |
| AllOk | 无通知 | 创建 receipt；所有 blocks 传给 agent |

---

## 6. Edge cases

| 场景 | 处理 |
|------|------|
| 单条消息 5 个 image + 1 个 caption | blocks = [text(caption), image1, image2, image3, image4, image5]；agent 看到 1 turn |
| 单条消息 1 个 image 无 caption | blocks = [image1]；agent 看到 1 turn with only image |
| 群聊多人各发 1 image + 各自 caption | 每条 message 各自 blocks；每条 receipt 独立；agent 看到 N turns（除非 Buffer Busy 合并）|
| 附件超大（> 20MB）| Feishu SDK 限制；下载失败 → 走 AllFailed/Partial 通知 |
| 附件路径含空格 / 中文 | 原样保留；agent 的 Read 工具能处理 |
| 用户撤回 attachment 消息 | v1.1 忽略；receipt 已 dispose |
| Sticker（表情包）| Feishu 阻止下载；adapter 跳过；blocks 只含 text（如有）|
| 富文本 post（含 inline image）| 解析 inline image 节点 → 同单 image 处理 |

---

## 7. Test plan

**单元测试**：
- `BuildBlocks(text, []Attachment{...})` → 正确 block 序列
- Attachment download 失败 → Error 字段填充
- `DownloadAttachments` retry 逻辑（mock 网络）

**集成测试**：
- mock Channel adapter → 3 个 attachment（2 ok + 1 fail）→ 验证 messageDispatcher 收到 2 个 ContentBlock + 1 个 partial warning
- mock Channel adapter → 3 个 attachment（全部 fail）→ 验证 messageDispatcher 不创建 receipt

**手动 E2E**：
- 飞书发 1 image + caption → Claude Code 看到 caption + image（via stream-json content-array）
- 飞书发 5 image + caption → Claude Code 看到 1 turn with 5 images
- 飞书发 PDF + caption → Claude Code 看到 PDF（document block）
- 飞书发 sticker → Claude Code 看不到附件（只有 caption，如有）
- 飞书发非 PDF 文件（.txt, .mp4）→ Claude Code 看到 text 引用 `@<path>`

---

## 8. Open questions

- inbox 目录清理：何时删？v1.1 不自动删；用户手动 `rm -rf ~/.nightme/inbox/<sid>/`
- 大文件 (> 50MB) 是否需要 streaming？v1.1 全量读 + base64；超大文件接受 base64 膨胀
- Claude Code 当前 TUI 模式对 inline image 的处理：vision only via stream-json content-array；TUI 自己用 `@<path>` 也能 read
- 其他 Bridge（PTY / ACP）如何处理 ContentImage：PTY 转 text block `@<path>`；ACP 用其 content-array 格式

---

## 9. Cross-references

- **ContentBlock 类型定义**：见 [`internal/agent/agent.go`](../../internal/agent/agent.go) §ContentBlock
- **Receipt FSM（blocks 怎么变成 receipt）**：见 [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md) §4
- **InputBuffer blocks 入队**：见 [`F-25-input-buffer.md`](./F-25-input-buffer.md) §5
- **Claude Code bridge 怎么 encoding blocks**：见 [`F-24-claudecode-bridge.md`](./F-24-claudecode-bridge.md)

---

## 10. Change log

- **2026-08-02** — v1.1: 数据流更新为 blocks 流（不是 string 拼接）。Session 不再 import feishu。Doc 重写。
- **2026-08-01** — v0.2: 原始附件下载 + text 拼接透传。已被 v1.1 取代（blocks 化让 attachment 信息更结构化）。