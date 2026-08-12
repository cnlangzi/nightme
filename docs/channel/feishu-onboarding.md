# Feishu Onboarding (附件透传 / App 注册)

## A1. F-14: Image / File Attachment Passthrough

> **Source**: `../channel/feishu-onboarding.md`


> **Related**: [`SPEC.md`](../SPEC.md); [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md); [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md); [`F-gateway.md`](./F-gateway.md) §5

---

## 1. Description

When a user sends an image, file, audio, or video to nightme in Feishu, the binary is downloaded into a per-session inbox directory and the agent receives a structured user turn (`[]agent.ContentBlock`) carrying the local paths of the downloaded files. Two inbound shapes are supported:

- **Single-resource messages** (`image`, `file`, `audio`, `media`): a single binary plus an optional text caption.
- **Post rich-text messages** (`post`): an ordered list of paragraphs, each paragraph a list of nodes (`text` / `a` / `img` / `media` / `at` / `emotion`). The ordering of nodes within each paragraph is **preserved verbatim** through the gateway and into the agent turn.

Stickers are silently skipped (Feishu blocks their resource download). Sending attachments back to the user is out of scope.

**scope (current)**:

- receive (image / file / audio / video)
- blocks (replacing string passthrough)
- **** Synchronous download inside `channel.Adapter.handleMessage` — every inbound attachment has its `LocalPath` populated *before* the message is published on `ch.Incoming()`. This invariant replaces the broken behaviour where `DownloadAttachments` was defined but never called.
- **** Post rich-text ordering preservation — `extractAttachments` returns an `[]agent.ContentBlock` pre-slice whose order matches the original Feishu paragraph exactly. Image blocks carry `FileKey` placeholders that the post-download `resolveBlocks` step back-fills with `LocalPath` so the wire shape survives download.
- Download binaries with 3-attempt retry (500ms → 1500ms backoff)
- Store under `~/.nightme/inbox/<session_id>/<filename>` (0700)
- User-facing notification when all or some downloads fail (separate chat reply, never mixed into the agent turn)
- One Feishu message = one agent turn (never split caption from attachment)

**Out of scope**:

- Sending attachments back to the user (no `SendImage` / `SendFile` API path)
- Stream-json content-block upgrade for the Claude Code bridge
- Inbox TTL / size-cap cleanup (manual today)
- Post `tag:"media"` (inline video in rich text) extraction — `tag:"img"` only in - Sticker forwarding (Feishu currently blocks download — keep an eye on API change)

---

## 2. Data flow

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

**invariants** (review-time checklist):

- **`InboundMessage.Attachments[i].LocalPath` is populated before publish.** A new Channel emits this; Gateway reads it; downstream `BuildBlocks` skips any attachment with `LocalPath == ""`. The WARN log `feishu: inbound attachments decoded with empty LocalPath` indicates a Channel failed to call `DownloadAttachments` (bug, not a normal state).
- **`InboundMessage.Blocks` is non-nil ONLY for post rich-text messages.** Non-post msg_types leave `Blocks == nil` and the messageDispatcher falls back to legacy `BuildBlocks(msg.Text, msg.Attachments)`. `Blocks` and `Attachments` are not redundant — `Attachments` carries the download candidates (binary sources); `Blocks` carries the ordered user-visible turn shape.
- **Order is preserved end-to-end.** `extractAttachments` walks the Feishu paragraphs in source order and emits one `ContentBlock` per node. The post-download `resolveBlocks` step preserves index alignment. `BuildBlocks` (legacy) preserves source order of the `Attachments` slice.

**/ change log**:

- (F-25): Gateway no longer holds receipts; Channel cold-creates on first `OutboundMessage{ReplyTo=userMsgID}`.
- ****: `DownloadAttachments` is now invoked synchronously in `handleMessage`. had the function defined and unit-tested but the production call site was missing — every inbound attachment silently fell through to `BuildBlocks`'s "skip empty LocalPath" branch.
- ****: Post rich-text `extractAttachments` returns an ordered `[]agent.ContentBlock` slice (was previously flattened into `text` + `[]Attachment` losing paragraph-internal ordering).

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

### 实现位置

- Claude Code stream-json:`internal/bridge/claudecode/session.go::SendBlocks` (line 243-353),envelope 在 line 341-352 拼装
- Pi RPC:`internal/bridge/pi/session.go::SendBlocks`,documented at `docs/bridge/pi.md` §6
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
| 用户撤回 attachment 消息 | 忽略；receipt 已 dispose |
| Sticker（表情包）| Feishu 阻止下载；adapter 跳过；blocks 只含 text（如有）|
| **富文本 post：paragraph 内 text+img+text 顺序** | extractAttachments 按 Feishu node 顺序产出 blocks；resolveBlocks 在下载后回填 Path；blocks 顺序 = 用户原意 |
| 富文本 post：多段落 | 段落间用 `\n` 合并到同一段 text block,或每个段落一个独立 text block(当前实现:每个段落一行,paragraph 之间换行)|
| 富文本 post：inline media(视频)| `tag:"media"` 仍然 drop(后续 PR 单独处理)|
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

- inbox 目录清理：何时删？不自动删；用户手动 `rm -rf ~/.nightme/inbox/<sid>/`
- 大文件 (> 50MB) 是否需要 streaming？全量读 + base64；超大文件接受 base64 膨胀
- Claude Code 当前 TUI 模式对 inline image 的处理：vision only via stream-json content-array；TUI 自己用 `@<path>` 也能 read
- 其他 Bridge（PTY / ACP）如何处理 ContentImage：PTY 转 text block `@<path>`；ACP 用其 content-array 格式
- post 富文本"@mention"是否要还原成"@某用户名"的 text 节点？不做（tag:"at" 直接 skip）

---

## 9. Cross-references

- **ContentBlock 类型定义**：见 [`internal/agent/agent.go`](../../internal/agent/agent.go) §ContentBlock
- **InboundMessage 形状**：见 [`internal/gateway/messages.go`](../../internal/gateway/messages.go) `InboundMessage`
- **Receipt card 内容(blocks 怎么渲染到 receipt)**:见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) §3.1 (Feishu 实现)
- **InputBuffer blocks 入队**：见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) §5
- **Claude Code bridge 怎么 encoding blocks**：见 [`../bridge/claude.md`](./../bridge/claude.md)

---

## 10. Change log

---

## A2. F-22: Feishu One-Click App Registration (QR 扫码授权)

> **Source**: `../channel/feishu-onboarding.md`


> **Depends on**: F-08 (Channel)

> **Related docs**: SPEC.md §1.1 (Channel), [F-08-channel-abstraction.md](./F-08-channel-abstraction.md), [F-gateway.md](./F-gateway.md)

## 1. Description

Feishu 官方 SDK（`lark-oapi-go/v3`）提供 `registration.RegisterApp` 函数，实现 **OAuth 2.0 Device Authorization Grant (RFC 8628)**：

1. nightme 调 `RegisterApp` → 飞书返回 verification URL + device code
2. nightme 在终端显示 URL + 二维码
3. 用户用飞书 App 扫码（或打开 URL）→ 飞书显示授权确认页（含 nightme 请求的 scopes/events）
4. 用户点确认 → 飞书**自动创建 app** 并把 credentials 返给 nightme
5. nightme 把 `ClientID` (App ID) + `ClientSecret` 存到 config

**用户零手动**：不需要去飞书开放平台建 app，不需要复制 app_id/app_secret。

**实现来源**：lark-oapi-go v3 的 `scene/registration` 子包。

## 2. Interface

```go
// internal/auth/auth.go
package auth

// Provider is the interface for channel-specific auth flows.
// only has Feishu;  may add Lark (international) etc.
type Provider interface {
    // Name returns the channel name (e.g. "feishu", "lark").
    Name() string

    // Login performs the auth flow. Blocks until user completes
    // (or timeout). Returns the credentials for storing in config.
    Login(ctx context.Context) (*Credentials, error)
}

type Credentials struct {
    AppID     string  // = result.ClientID
    AppSecret string  // = result.ClientSecret
    TenantAccessToken string  // optional, for app-only API calls
    // Metadata from the registration:
    AppName   string
    CreatedAt time.Time
}

// internal/auth/feishu/feishu.go
type FeishuAuth struct {
    // Pre-configured scopes/events the user wants
    DefaultAddons *registration.AppAddons
    // Pre-filled app name/description (optional)
    AppPreset *registration.AppPreset
    // For update flow: pre-existing AppID
    ExistingAppID string
    // For CreateOnly mode (default false)
    CreateOnly bool
}

func NewFeishuAuth(opts FeishuAuthOptions) *FeishuAuth { ... }

func (f *FeishuAuth) Login(ctx context.Context) (*Credentials, error) {
    result, err := registration.RegisterApp(ctx, &registration.Options{
        AppID:      f.ExistingAppID,  // empty = create new
        CreateOnly: f.CreateOnly,
        Addons:     f.DefaultAddons,
        AppPreset:  f.AppPreset,
        OnQRCode: func(info *registration.QRCodeInfo) {
            // Display URL + render QR in terminal
        },
        OnStatusChange: func(info *registration.StatusChangeInfo) {
            // Print polling status
        },
    })
    if err != nil {
        var regErr *registration.RegisterAppError
        if errors.As(err, &regErr) {
            return nil, fmt.Errorf("register app failed: %s - %s", regErr.Code, regErr.Description)
        }
        return nil, err
    }
    return &Credentials{
        AppID:     result.ClientID,
        AppSecret: result.ClientSecret,
        AppName:   "nightme",  // 来自 AppPreset.Name
        CreatedAt: time.Now(),
    }, nil
}

func (f *FeishuAuth) Name() string { return "feishu" }
```

## 3. Implementation

**文件结构**：
```
internal/auth/
├── auth.go                       # Provider interface + Credentials
└── feishu/
    ├── feishu.go                 # FeishuAuth 实现
    ├── feishu_test.go            # 单元测试
    └── qrencode.go               # 终端 QR 渲染

cmd/nightme/
└── login.go                      # `nightme login feishu` 子命令
```

**新增依赖**：
- `github.com/skip2/go-qrcode` — 终端 QR 渲染（ASCII 字符）

**`nightme login feishu` CLI 流程**：
```
$ nightme login feishu
[nightme] requesting Feishu authorization...

Scan this QR code with Feishu mobile, or open this URL:
https://accounts.feishu.cn/oauth2/...   (expires in 600 seconds)

[QR CODE]

polling...
status: polling, next check in 5s
status: polling, next check in 5s
✓ App registered successfully!
  App ID:     cli_xxxx
  App Name:   nightme
  Scopes:     im:message:send_as_bot, im:message:receive_v1
  Credentials saved to: ~/.nightme/config.yaml (chmod 0600)

Next: run `nightme run` to start the gateway.
```

**预配置的 scopes/events**（nightme 必须的最小集）：

| 类型 | 名称 | 用途 |
|------|------|------|
| Scope (tenant) | `im:message:send_as_bot` | bot 发消息 |
| Scope (tenant) | `im:message:receive_v1` (event) | 接收消息 |
| Scope (tenant) | `im:message:readonly` | 下载消息资源 (F-14 接收图片/文件/音视频) |
| Scope (tenant) | `im:message.reactions:write_only` | 在用户消息上加 reaction emoji (F-25 receipt) |
| Scope (tenant) | `im:message.group_at_msg:readonly` | 群消息 @ 提及（为 Feishu 侧 server-side filter 保留）|
| Scope (tenant) | `im:message.group_msg`（**F-watch，默认包含**） | 接收群聊全部消息；由 nightme 侧 `WatchMode` gate 决定处理还是 drop。**不带 `:readonly`** —— bot 需要回复到群里 |
| Event | `im.message.receive_v1` | 收消息事件 |

**Credentials 持久化**：
- 写入 `~/.nightme/config.yaml` 的 `feishu.app_id` / `feishu.app_secret` 顶层字段
- 文件权限 0600
- 原子写（temp + rename，跟 registry 一样）

**single-account**：nightme 只支持一个飞书 app（一个 CLI / 进程一个 app）。多 account 场景后续。
- 如果 需要 multi-account，再重构为 `channels.feishu.accounts.<name>.{app_id, app_secret}` 嵌套结构
- 届时 `F-08 Channel` 也要扩展为支持多 instance

**Manual 模式 fallback**：
- config 里已有 `appId` / `appSecret` → `nightme login feishu` **直接覆盖**（rebind 即 login，无 `--force` flag）。这是升级 scope（如新增 `im:message.reactions:write_only`）的标准操作流程。
- 无需 `--force`，登录动作本身就是强制重新绑定。

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| 用户在 10 分钟内没扫码 | context timeout → 报错 "auth timeout" |
| QR 扫描后飞书显示"权限不足" | 飞书返回 error code → nightme 提示用户检查企业权限 |
| 凭证写到 config 失败（disk full / permission） | 保留 in-memory credential，提示用户手动复制到 config |
| config 已有 appId | **直接覆盖**（login 即 rebind），无需 `--force` |
| 用户选了不存在的 tenant | 飞书返回 error，nightme 透传 |
| Lark (international) vs Feishu (国内) | 自动检测 `tenant_brand`，用对应 `LarkDomain` 或 `Domain`（SDK 自动处理）|
| 注册时网络断 | ctx timeout → 报错，下次重试无需重新写 config |
| 用户想看当前凭证 | 直接 `cat ~/.nightme/config.yaml`（无 status 命令；用户可读 config）|
| 用户想撤销 app | 文档说明需要去飞书开放平台手动撤销 + 删 config |
| `--update` 模式 | 用 `Options.AppID` 走 update flow，重新授权现有 app |
| 升级 scope（如新增 reaction scope） | 重新跑 `nightme login feishu`，飞书重新创建 app，新凭证覆盖 config |
| DM 中 `/watch` 为 no-op | DM chat_type 下 `HasMention` 永远 true，`/watch on/off` 状态正常写入但不影响消息处理 —— DM 消息全收，与模式无关 |

## 5. 验收流程

1. `nightme login feishu`
2. 终端显示 QR
3. 用飞书 App 扫码
4. 飞书显示"nightme 申请 X 权限"
5. 点确认
6. 终端 ✓ + config 自动更新
7. `nightme run` → 飞书 round-trip 工作

**全部不需要去飞书开发者后台**。

## 6. Test plan

**单元测试**（用 mock SDK）：
- `TestFeishuAuth_Login` 模拟注册成功 → 验证返回 credentials
- `TestFeishuAuth_LoginTimeout` 模拟 context cancel → 验证返回 error
- `TestFeishuAuth_LoginError` 模拟飞书返回 error code → 验证 error wrapping
- `TestWriteCredentials_Atomic` 验证 config 写入是原子的 + 0600 权限
- `TestLogin_AlwaysRebinds` 验证重新 login 时无条件覆盖现有凭证（无 `--force` flag）

**集成测试**（需要 mock registration.RegisterApp）：
- `TestLogin_Success` 模拟整个 QR flow → config 写入正确
- `TestLogin_ProviderError` 验证 provider 失败时错误透传（无凭证写入磁盘）

**E2E（手动）**：
- 在真实飞书账号上跑 `nightme login feishu`
- 验证 QR 显示
- 用飞书 App 扫
- 验证 config 写入
- 跑 `nightme run` 验证 round-trip

## 7. 与现有功能的关系

| 现有 | 关系 |
|------|------|
| F-08 Channel Abstraction | 依赖 F-22：Channel 用 F-22 拿到的 credentials 工作 |
| F-20 Gateway | 独立——Gateway 路由 slash command，不涉及 auth |
| 现有 `internal/config/` | F-22 写入 config.yaml 的 `feishu.app_id` / `feishu.app_secret` 顶层字段（chmod 0600）|

**对 F-08 的影响**：
- F-08 Channel 启动时读 `feishu.app_id` / `feishu.app_secret` 顶层字段
- 这两个值可以来自：
  1. `nightme login feishu`（F-22 写入）✅ 推荐
  2. 手动编辑 config.yaml

## 8. 实施顺序

| 阶段 | 任务 | 估时 |
|------|------|------|
| M2 PR #4 | F-22 实施（auth package + CLI）| 2-3 commits |
| M2 PR #4 | F-08 Channel adapter 实施（lark-oapi-go 长连接）| 3-4 commits |
| M2 PR #5 | 飞书 round-trip 集成 | 3-4 commits |

PR #4 内部先做 F-22，再做 F-08（让 F-08 测试时用 F-22 拿到的真凭证）。

## 9. 注意事项

- **lark-oapi-go v3 必需**：v3 的 `registration` 是新增的，v1/v2 没有
- **二维码显示**：用 `skip2/go-qrcode` 输出到 stdout（黑底白字）
- **超时默认 10 分钟**：context.WithTimeout，可通过 flag 调整
- **不要打印 ClientSecret 到日志**：log 时 redact
- **不存储 secret 到任何 log 文件**

## 10. Open questions (resolved)

- Lark (国际版) 是否用相同 SDK？倾向：同 SDK，自动检测 tenant_brand
- ~~多 tenant 支持（一个 nightme 跑多个飞书 app）？不支持，考虑~~ — **2026-07-31 决议：明确 single-account，不预留 multi-account 结构。需要时重构为 `channels.feishu.accounts.<name>.{...}`。**
- 用户拒绝授权后如何清理（feishu 侧是否残留）？飞书只授权后才会创建 app，拒绝则不创建，无需清理
- 是否支持 `nightme login export feishu` 输出可分享的 env var？考虑
- 与 M2 现有计划的 conflicts：原 M2 假设 manual setup，新增 F-22 是增量（向后兼容）
- registry vs config：F-22 凭证存 config.yaml（用户可读可改），不是 registry（机器管理的 runtime state）

---

