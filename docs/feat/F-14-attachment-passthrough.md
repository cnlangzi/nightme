# F-14: Image / File Attachment Passthrough

> **Status**: implemented (v0.2 receive; v1.1 blocks; **v1.4 ordered post rich-text + sync download**)
> **Milestone**: v0.2; v1.1; v1.4
> **Related**: [`SPEC.md`](../SPEC.md) v1.4 §2.1; [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md); [`F-25-rolling-log.md`](./F-25-rolling-log.md); [`F-20-gateway.md`](./F-20-gateway.md) §5

---

## 1. Description

When a user sends an image, file, audio, or video to nightme in Feishu, the binary is downloaded into a per-session inbox directory and the agent receives a structured user turn (`[]agent.ContentBlock`) carrying the local paths of the downloaded files. Two inbound shapes are supported:

- **Single-resource messages** (`image`, `file`, `audio`, `media`): a single binary plus an optional text caption.
- **Post rich-text messages** (`post`): an ordered list of paragraphs, each paragraph a list of nodes (`text` / `a` / `img` / `media` / `at` / `emotion`). The ordering of nodes within each paragraph is **preserved verbatim** through the gateway and into the agent turn.

Stickers are silently skipped (Feishu blocks their resource download). Sending attachments back to the user is out of scope.

**v1.4 scope (current)**:

- v0.2 receive (image / file / audio / video)
- v1.1 blocks (replacing v0.2 string passthrough)
- **v1.4a** Synchronous download inside `channel.Adapter.handleMessage` — every inbound attachment has its `LocalPath` populated *before* the message is published on `ch.Incoming()`. This invariant replaces the broken v1.1 behaviour where `DownloadAttachments` was defined but never called.
- **v1.4b** Post rich-text ordering preservation — `extractAttachments` returns an `[]agent.ContentBlock` pre-slice whose order matches the original Feishu paragraph exactly. Image blocks carry `FileKey` placeholders that the post-download `resolveBlocks` step back-fills with `LocalPath` so the wire shape survives download.
- Download binaries with 3-attempt retry (500ms → 1500ms backoff)
- Store under `~/.nightme/inbox/<session_id>/<filename>` (0700)
- User-facing notification when all or some downloads fail (separate chat reply, never mixed into the agent turn)
- One Feishu message = one agent turn (never split caption from attachment)

**Out of scope**:

- Sending attachments back to the user (no `SendImage` / `SendFile` API path)
- Stream-json content-block upgrade for the Claude Code bridge
- Inbox TTL / size-cap cleanup (manual today)
- Post `tag:"media"` (inline video in rich text) extraction — `tag:"img"` only in v1.4
- Sticker forwarding (Feishu currently blocks download — keep an eye on API change)

---

## 2. Data flow (v1.4)

```
Feishu inbound
  → channel.Adapter.handleMessage
       ├── extractAttachments(msgType, content)
       │     ├── text  string                       (legacy single-resource caption)
       │     ├── atts  []channel.Attachment        (download candidates; FileKey set, LocalPath empty)
       │     └── blocks []agent.ContentBlock       (NEW, post rich-text only — ordered, FileKey placeholders)
       │
       ├── DownloadAttachments(ctx, lark, messageID, atts, sessionID)   ← NEW: actually called
       │     ├── retry x3 with exponential backoff
       │     └── returns DownloadResult {Atts, HasAttachments, AllFailed, FailureKeys}
       │           ├── Atts[i].LocalPath populated on success
       │           └── Atts[i].Error populated on failure
       │
       ├── AllFailed?
       │     └── ch.Send(OutText "❌ N attachments failed…") + return nil   ← user-facing notification
       │           (never published to ch.Incoming; never reaches the agent)
       │
       ├── partial?  → ch.Send(OutText "⚠️ K of N failed; sending the rest")
       │           (continue; failure attachments are silently dropped downstream)
       │
       ├── resolveBlocks(preBlocks, downloadedAtts)   ← NEW: back-fill FileKey → LocalPath
       │           (preserves post paragraph order)
       │
       └── publish InboundMessage{Text, Attachments, Blocks (post path), ...} → ch.Incoming()

Gateway.pumpInbound → dispatchLoop → DispatchInbound (inboundDispatcher) → messageDispatcher
  ├── messageDispatcher: blocks source selection
  │     ├── msg.Blocks != nil (post path)   → blocks = msg.Blocks        ← ordered
  │     └── else (legacy single-resource)   → blocks = feishu.BuildBlocks(msg.Text, msg.Attachments)
  │
  └── ChatSession.QueueUserMessage(blocks, msg.MessageID)
        ├── Idle → 立即 SendBlocks(blocks) → ChatSession.currentTurnUserMsgID = msg.MessageID
        └── Busy → 入队 → onFlush 钩子触发时批量 SendBlocks(currentTurnUserMsgID = last userMsgID)
```

**v1.4 invariants** (review-time checklist):

- **`InboundMessage.Attachments[i].LocalPath` is populated before publish.** A new Channel emits this; Gateway reads it; downstream `BuildBlocks` skips any attachment with `LocalPath == ""`. The WARN log `feishu: inbound attachments decoded with empty LocalPath` indicates a Channel failed to call `DownloadAttachments` (bug, not a normal state).
- **`InboundMessage.Blocks` is non-nil ONLY for post rich-text messages.** Non-post msg_types leave `Blocks == nil` and the messageDispatcher falls back to legacy `BuildBlocks(msg.Text, msg.Attachments)`. `Blocks` and `Attachments` are not redundant — `Attachments` carries the download candidates (binary sources); `Blocks` carries the ordered user-visible turn shape.
- **Order is preserved end-to-end.** `extractAttachments` walks the Feishu paragraphs in source order and emits one `ContentBlock` per node. The post-download `resolveBlocks` step preserves index alignment. `BuildBlocks` (legacy) preserves source order of the `Attachments` slice.

**v1.3 / v1.4 change log**:

- v1.3 (F-25): Gateway no longer holds receipts; Channel cold-creates on first `OutboundMessage{ReplyTo=userMsgID}`.
- **v1.4a**: `DownloadAttachments` is now invoked synchronously in `handleMessage`. v1.1 had the function defined and unit-tested but the production call site was missing — every inbound attachment silently fell through to `BuildBlocks`'s "skip empty LocalPath" branch.
- **v1.4b**: Post rich-text `extractAttachments` returns an ordered `[]agent.ContentBlock` slice (was previously flattened into `text` + `[]Attachment` losing paragraph-internal ordering).

---

## 3. blocks 流经的三层

```
[Channel Adapter]                  [Gateway]                       [Session.InputBuffer]
       │                                │                                    │
       │ BuildBlocks(msg.Text, Att)     │                                    │
       │   OR resolveBlocks(preBlocks,  │                                    │
       │      downloadedAtts) (post)    │                                    │
       │ → []ContentBlock               │                                    │
       │                                │                                    │
       │ Channel.Send(OutboundMessage)  │                                    │
       │    ReplyTo=userMsgID           │                                    │
       │ ◄──────────────────────────────│                                    │
       │ (cold-creates receipt card)    │                                    │
       │                                │ QueueUserMessage(blocks, umid) ────│
       │                                │                                    │ Add(blocks, umid)
       │                                │                                    │
       │                                │                                    │ Idle → SendBlocks
       │                                │                                    │
       │ UpdateReceipt(PATCH) ◄─────────│                                    │
       │                                │                                    │ Busy → buffer
       │                                │                                    │ SendBlocks(combined)
       │                                │ onFlush hook (gw 注入) ◄────────────│
```

**关键 invariant**：blocks 是 gateway 流转的"通用货币"，每层接收/产出 `[]ContentBlock`：
- Channel.Send → cold-creates receipt on first OutboundMessage for a given userMsgID
- Session.InputBuffer.Add → 缓冲或 dispatch
- Session.AgentSession.SendBlocks → Bridge 编码为 agent 原生格式（stream-json content-array / PTY "@<path>" / ACP content-array）

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

**顺序就是事实**：blocks 在 `[]ContentBlock` 里的位置决定了 Agent 看到的顺序。`BuildBlocks`（legacy 路径）保证 `[ContentText(caption), ContentImage×N, ContentFile×M]` 的顺序。`resolveBlocks`（post 路径）保证 **Feishu 原始 paragraph node 顺序**。

**为什么是 slice 而不是 Text-with-placeholder**：Anthropic API message 协议的 `content` 字段是 heterogeneous array（每个元素 type 不同），不是单一字符串。`[]ContentBlock` slice 1:1 对应该数组，文本 / 图片 / 文件的相对位置天然由数组下标表达。若改成"Text 里嵌入 `[img:xxx]` 占位符"会引入三类问题：
1. 解析歧义：用户文本里若含 `[img:` 字面字符会被误吃
2. 类型丢失：placeholder 无法区分 image vs document(API 两种不同 JSON shape)
3. 协议弱化：Anthropic 的 multi-modal 协议就是数组，靠 placeholder 倒退回字符串方案会破坏 SDK 一致性

---

## 4.5 Wire format: blocks → Claude stream-json `content[]`

`Session.SendBlocks(ctx, blocks)` 的契约(`internal/agent/agent.go:622-639`):

```text
blocks  := [text, image, text, image, text]    // 有序 slice
        ↓ bridge SendBlocks
content := [
  {type:"text",    text:"看一下这只"},
  {type:"image",   source:{type:"base64", media_type:"image/png", data:"<cat>"}},
  {type:"text",    text:"跟这只"},
  {type:"image",   source:{type:"base64", media_type:"image/jpeg", data:"<dog>"}},
  {type:"text",    text:"的区别"}
]
        ↓ envelope
msg := {type:"user", message:{role:"user", content: content}}
        ↓ writeLine(json.Marshal(msg) + "\n")
claude 子进程 stdin
```

### 字段映射表

| Block.Type | JSON element | 何时降级 |
|------------|--------------|----------|
| `ContentText` | `{"type":"text","text": block.Text}` | Text 为空 → skip |
| `ContentImage` | `{"type":"image","source":{"type":"base64","media_type": block.MediaType,"data": base64(file)}}` | size > 5 MiB → 降级为 `{"type":"text","text":"Image (too large, N bytes): /path"}`；Path 为空 / os.Stat 失败 / base64 失败 → skip |
| `ContentFile` (PDF) | `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data": base64(file)}}` | 失败 → skip |
| `ContentFile` (其他 MIME) | `{"type":"text","text":"File: /path"}` | — |

> Anthropic API 不支持 inline non-PDF files,降级为文本引用,Claude 自己的 Read 工具读 path。其他 Bridge(PTY 模式)统一用 `@<path>` 形态。

### 实现位置

- Claude Code stream-json:`internal/bridge/claudecode/session.go::SendBlocks` (line 243-353),envelope 在 line 341-352 拼装
- Pi RPC:`internal/bridge/pi/session.go::SendBlocks`,documented at `docs/feat/F-32-pi-rpc-bridge.md` §6
- PTY fallback:每 block 一行 text(`@<path>`)或 verbatim text,blocks 之间 `\n` 拼接

### 关键不变量

1. **顺序保持**:`[]ContentBlock` 的下标 i 直接对应 `content[i]` 的 wire 位置,中间没有 reordering
2. **空 slice → noop**:`SendBlocks(nil)` / `SendBlocks([])` 立即返回 nil,不写 envelope
3. **失败 block 不破坏 array**:skip 的 block 不会替换为 placeholder,而是直接从 `content[]` 剔除(避免 Claude 把"半截 array"误读)
4. **5 MiB 是 Anthropic API 硬限制**:超过会被 API 拒绝(`bridge/claudecode/session.go:269`),降级为文本引用

---

## 5. Download error 通知模板

| 场景 | 用户通知 | receipt 行为 |
|------|---------|-------------|
| AllFailed（所有 attachment 下载失败）| `❌ N attachments failed to download, please retry` | **不 publish 到 ch.Incoming**;整条消息丢弃,Agent 看不到 |
| Partial（部分失败）| `⚠️ K of N attachments failed; sending the rest` | publish;resolve 时失败的 attachment 节点从 blocks 中 skip(对应位置的 text 仍保留) |
| AllOk | 无通知 | publish;所有 blocks 传给 agent |

**post rich-text 的特殊处理**：partial 场景下,失败的 image 节点被 **omit**(保留位置上下文),不替换为占位符 —— 否则 Agent 会把失败的占位当成"用户传了 4 张图但只看到 3 张"的歧义指令。失败的 image 节点的 user 上下文已经通过前后的 text 段落保留。

---

## 6. Edge cases

| 场景 | 处理 |
|------|------|
| 单条消息 5 个 image + 1 个 caption | blocks = [text(caption), image1, image2, image3, image4, image5]；agent 看到 1 turn |
| 单条消息 1 个 image 无 caption | blocks = [image1]；agent 看到 1 turn with only image |
| 群聊多人各发 1 image + 各自 caption | 每条 message 各自 blocks；每条 receipt 独立；agent 看到 N turns（除非 Buffer Busy 合并）|
| 附件超大（> 5MB for image, > 32MB for PDF）| Bridge layer 5 MiB / 32 MiB 限制；超大被 bridge 退化为 text 引用（`@<path>` / `File: <path>`）|
| 附件超大（> 20MB 总 size）| Feishu SDK 限制；下载失败 → 走 AllFailed/Partial 通知 |
| 附件路径含空格 / 中文 | 原样保留；agent 的 Read 工具能处理 |
| 用户撤回 attachment 消息 | v1.4 忽略；receipt 已 dispose |
| Sticker（表情包）| Feishu 阻止下载；adapter 跳过；blocks 只含 text（如有）|
| **富文本 post：paragraph 内 text+img+text 顺序** | **NEW v1.4b**：extractAttachments 按 Feishu node 顺序产出 blocks；resolveBlocks 在下载后回填 Path；blocks 顺序 = 用户原意 |
| 富文本 post：多段落 | 段落间用 `\n` 合并到同一段 text block,或每个段落一个独立 text block(当前实现:每个段落一行,paragraph 之间换行)|
| 富文本 post：inline media(视频)| `tag:"media"` v1.4 仍然 drop(后续 PR 单独处理)|
| 富文本 post：tag:"at"(mention)、tag:"emotion" | skip —— 不影响 user payload 转播 |

---

## 7. Test plan

**单元测试**：

- `extractAttachments` 单 resource msg_types (text/image/file/audio/media) → 旧的 `(text, atts)` 行为不变
- **`extractAttachments` post 路径** → 返回有序 blocks,blocks 顺序 = Feishu paragraph node 顺序
- **`resolveBlocks` (post)** → file_key → local_path 回填,顺序保持
- `BuildBlocks` (legacy 路径) → 单 text + 多 attachment 顺序正确
- `BuildBlocks` legacy 跳过 empty LocalPath
- `DownloadAttachments` retry 逻辑 (mock 网络)

**集成测试**：

- mock Channel adapter → 3 个 attachment (2 ok + 1 fail) → 验证 `handleMessage` 发出 1 条 partial warn + published `InboundMessage{Attachments:[2 OK, 1 fail], Blocks:ordered-with-1-image-omitted}`
- mock Channel adapter → 3 个 attachment (全部 fail) → 验证 `handleMessage` 发出 1 条 AllFailed warn,**没有** publish 到 ch.Incoming
- mock Channel adapter → post rich-text (text + image + text) → 验证 `InboundMessage.Blocks == [text, image(file_key), text]` 在 download 后变为 `[text, image(local_path), text]`

**手动 E2E**：

- 飞书发 1 image + caption → Claude Code 看到 caption + image（via stream-json content-array）
- 飞书发 5 image + caption → Claude Code 看到 1 turn with 5 images
- **飞书发 post 富文本 "看这只 🐱 跟这只 🐶 的区别"** → Claude Code 看到 text+img+text+img+text,**顺序正确**
- 飞书发 PDF + caption → Claude Code 看到 PDF（document block）
- 飞书发 sticker → Claude Code 看不到附件（只有 caption，如有）
- 飞书发非 PDF 文件（.txt, .mp4）→ Claude Code 看到 text 引用 `@<path>`

---

## 8. Open questions

- inbox 目录清理：何时删？v1.4 不自动删；用户手动 `rm -rf ~/.nightme/inbox/<sid>/`
- 大文件 (> 50MB) 是否需要 streaming？v1.4 全量读 + base64；超大文件接受 base64 膨胀
- Claude Code 当前 TUI 模式对 inline image 的处理：vision only via stream-json content-array；TUI 自己用 `@<path>` 也能 read
- 其他 Bridge（PTY / ACP）如何处理 ContentImage：PTY 转 text block `@<path>`；ACP 用其 content-array 格式
- post 富文本"@mention"是否要还原成"@某用户名"的 text 节点？v1.4 不做（tag:"at" 直接 skip）

---

## 9. Cross-references

- **ContentBlock 类型定义**：见 [`internal/agent/agent.go`](../../internal/agent/agent.go) §ContentBlock
- **InboundMessage 形状**：见 [`internal/gateway/messages.go`](../../internal/gateway/messages.go) `InboundMessage`
- **Receipt card 内容(blocks 怎么渲染到 receipt)**:见 [`F-25-rolling-log.md`](./F-25-rolling-log.md) §3.1 (Feishu 实现)
- **InputBuffer blocks 入队**：见 [`F-25-rolling-log.md`](./F-25-rolling-log.md) §5
- **Claude Code bridge 怎么 encoding blocks**：见 [`F-24-claudecode-bridge.md`](./F-24-claudecode-bridge.md)

---

## 10. Change log

- **2026-08-04** — v1.4b: post 富文本 paragraph-level ordering preservation. `extractAttachments` returns ordered blocks; `resolveBlocks` back-fills LocalPath after download; messageDispatcher prefers `msg.Blocks` when non-nil.
- **2026-08-04** — v1.4a: synchronous download in `handleMessage`. Closes the v1.1–v1.3 regression where `DownloadAttachments` was defined but never called from production code (every inbound attachment was silently dropped at `BuildBlocks`).
- **2026-08-02** — v1.1: 数据流更新为 blocks 流（不是 string 拼接）。Session 不再 import feishu。Doc 重写。
- **2026-08-01** — v0.2: 原始附件下载 + text 拼接透传。已被 v1.1 取代（blocks 化让 attachment 信息更结构化）。