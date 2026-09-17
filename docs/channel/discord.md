# Discord Channel - 接入设计与实现

> **Scope**: nightme Discord Bot 适配器（`internal/channel/discord/*`）+ 登录提供器（`cmd/nightme/login_discord.go`）
> **Goal**: 用 Discord Gateway v10 + REST v10 承载 per-chat 会话，对齐 nightme 已有的 `OutboundKind` / `OutboundMessage` 契约。
> **Related docs**:
>
> - [feishu.md](./feishu.md) - 飞书 receipt / 交互卡实现（**OutboundKind 渲染映射的基线**）
> - [telegram.md](./telegram.md) - Telegram 适配器（**F-61 attachment retry 的对照**）
> - [feishu-reliability.md](./feishu-reliability.md) - 重试 / 限流 / WS 重连分层
> - [F-08-channel-abstraction.md](../feat/F-08-channel-abstraction.md) - Channel 抽象与 Gateway 边界
> - [F-message-flow.md](../feat/F-message-flow.md) - 消息生命周期
> - [SPEC.md](../SPEC.md) - 统一消息模型
>
> **Discord 官方文档**:
>
> - [Gateway API](https://discord.com/developers/docs/topics/gateway)
> - [Gateway Opcodes & Close Codes](https://discord.com/developers/docs/topics/opcodes-and-status-codes)
> - [Bot Account Intents](https://discord.com/developers/docs/topics/gateway#privileged-intents)
> - [Rate Limits](https://discord.com/developers/docs/topics/rate-limits)
> - [Message Components V1](https://discord.com/developers/docs/interactions/message-components)
> - [Modal Submit](https://discord.com/developers/docs/interactions/message-components#text-inputs)
> - [Interactions / Callbacks](https://discord.com/developers/docs/interactions/receiving-and-responding)

## 1. Per-turn message flow (inbound + outbound)

### 1.1 Inbound: `MESSAGE_CREATE` → `messages.InboundMessage`

Gateway read loop (`internal/channel/discord/gateway.go:readLoop`) unmarshals every `op=0 t=MESSAGE_CREATE` frame into `discord.Message`, then dispatches to `Adapter.onMessage` (`internal/channel/discord/adapter.go:onMessage`). Mapping rules:

| Discord 字段              | 落到 InboundMessage                                              |
| ------------------------- | ---------------------------------------------------------------- |
| `author.id == botUserID`  | 静默丢弃（self-loop 防御）                                        |
| `channel_type` 非法       | 静默丢弃（voice / stage / forum / media 频道不在 nightme 数据模型）|
| `chat_id`                 | `sessionChatID(channel_id)` = `dc_<channel_id>`                  |
| `user_id`                 | `author.id`（snowflake 字符串）                                  |
| `Text`                    | `content`（明文 markdown，**不作 Discord-native 渲染**）         |
| `MessageID`               | `id`（snowflake 字符串）                                         |
| `Time`                    | `timestamp`（RFC3339）                                           |
| `ReplyTo`                 | `message_reference.message_id` 优先；`referenced_message.id` 兜底 |
| `HasMention`              | DM 始终为 true；group 看 `mention_everyone` / `mentions` / `/` 前缀 |
| `Attachments`             | 通过 `downloadAndPublish` 异步下载（见 §9）                       |

`botUserID` 在两条路径上填充：`Start` 调 `GetMe`（`internal/channel/discord/adapter.go:Start`）拿到 bot 自报身份；`READY` 事件再次覆盖（`gateway.go:onReady` → `adapter.go:onReady`）以保持一致。

### 1.2 Outbound: `OutboundMessage` → Discord wire

`Send` (`internal/channel/discord/send.go:Send`) 按 `OutboundKind` 分发，路由表见 §8。

## 2. Choice / Modal interaction

### 2.1 Choice（`OutChoice` → V1 components）

`sendChoice` (`internal/channel/discord/send.go`) 把 `messages.Choice.Options` 渲染为 Discord Message Components V1 的 `ActionRow[Button]`：
- 每个 option 一个 `Button`，`custom_id = "c:<shortReqID>:<idx>"`。
- "Type your answer" 入口是一个 `custom_id = "i:<shortReqID>"` 的按钮，按下后弹出 modal（§2.3）。
- 超过 20 个 option 时（`maxOptionButtons = 20` in `choice_store.go`），剩余 option 折叠为 modal。

`Choice.RequestID` 是 runtime 生成的 UUID；Discord wire 上携带短哈希（`ShortID`，8 字节）以匹配 100-byte `custom_id` 上限。

### 2.2 INTERACTION_CREATE ACK deadline

Discord 在 `INTERACTION_CREATE` 后 3 秒内未收到 ACK 会失效 interaction token。`interactionAckBudget = 2500ms`（`internal/channel/discord/callback.go`）预留 500 ms 给外层 ctx 取消。`onInteraction` 用 `context.WithTimeout(context.Background(), interactionAckBudget)` 起一个 fresh ctx — 不复用 gateway ctx，避免 cancelled ctx 阻断 ACK。

`InteractionTypeMessageComponent` 走 `handleComponentClick`：
- `custom_id` 以 `c:` 开头 → `handleChoiceClick` → ACK type=7 (`UPDATE_MESSAGE`) → `OutboundMessage.Action{Option, Index}` 推到 `a.incoming`。
- `custom_id` 以 `i:` 开头 → `handleInputClick` → ACK type=9 (`MODAL`) 弹输入框。

### 2.3 Modal submit

用户提交 modal 后 Discord 发第二个 `INTERACTION_CREATE`（`Type=5` MODAL_SUBMIT）。`handleModalSubmit` 从 `data.components[*].components[*].value` 取出答案，发布 `InboundMessage.Action{Option: "custom", Form: {"answer": <input>}}`，ACK type=7。

Modal 的 text input 上限 4000 字符（Discord 硬限制；超出会被 Discord 在客户端直接截断）。

## 3. chatID namespace (`dc_<channel_id>`)

`sessionChatID` (`internal/channel/discord/session_chatid.go`) 是 chat id 的纯函数：

```go
const chatIDPrefix = "dc_"
func sessionChatID(channelID string) string { return chatIDPrefix + channelID }
```

注册位置：`internal/channel/discord/init.go:Register("discord", chatIDPrefix, ...)`。Discord 频道 id 是 decimal snowflake，UTF-8 安全，不需要转义。

**无 thread 后缀**：Discord 的 thread 频道有独立 snowflake，nightme 不在数据模型中引入 thread 概念（F-33 D4）。`isSupportedChannelType` 支持 `0/1/5/10/11/12`（guild text / DM / announcement / 三种 thread），其它类型静默丢弃。

## 4. Onboarding (`nightme login discord`)

`nightme login discord` 引导操作员完成 Developer Portal 配置。流程：

1. **创建 Application**：访问 [Discord Developer Portal](https://discord.com/developers/applications) → "New Application" → 命名。
2. **创建 Bot**：左侧 "Bot" tab → "Add Bot"。复制 token 备用（**只显示一次**）。
3. **启用 Privileged Intents**：同一页 → "Privileged Gateway Intents" → 至少勾选 `MESSAGE_CONTENT`。否则夜间 100 个 server 时 Discord 会拒绝 IDENTIFY 并返回 close code 4014（`closeCodeDisallowedIntents`）。
4. **OAuth2 邀请**：左侧 "OAuth2 → URL Generator" → scopes 勾 `bot` + `applications.commands`；Bot Permissions 至少勾 `Send Messages` / `Read Message History` / `Add Reactions` / `Use Slash Commands` / `Embed Links` / `Attach Files`。复制生成的 URL 在浏览器打开，把 bot 邀请进目标 server。
5. **填入配置**：`nightme login discord` 交互提示输入 token（或把 `bot_token` 写入 `configs/config.yaml` 的 `discord.bot_token`）。

`Intents` 默认值是 `MessageContent | GuildMessages | DirectMessages | Guilds` = `46593`。NightMe 写入 `internal/config.DiscordConfig.Intents`。

## 5. Gateway lifecycle (Hello / Heartbeat / Identify / Resume)

`gatewayClient.run` (`internal/channel/discord/gateway.go:run`) 是常驻 loop。每轮迭代：

1. **解析 URL**：从持久化 `state.json` 读 `SessionID + LastSeq + ResumeURL`；若 state 与 `intentsVersion` 匹配且 `lastSeq > 0`，下一轮走 RESUME；否则 `GetGatewayBot` 拿 `wss://gateway.discord.gg?v=10&encoding=json`。
2. **Dial + HELLO**：标准 WS upgrade。`readJSON` 读第一帧 `{"heartbeat_interval": <ms>}` 启动 heartbeat goroutine（带 jittered first-delay）。
3. **IDENTIFY / RESUME**：IDENTIFY 携带 token + intents + OS/browser=device=`"nightme"`；RESUME 携带 token + sessionID + lastSeq。
4. **Read loop**：
   - `op=0` dispatch → `g.metrics.recordEvent(time.Now())`（`last_event_unix_ts` lag probe）→ 按 `frame.T` 分发 `READY` / `MESSAGE_CREATE` / `INTERACTION_CREATE` / `RESUMED` / 其它。
   - `op=7` Reconnect → 返回 `(_, _, nil)` 让 run loop 重 dial。
   - `op=9` Invalid Session（`d=false`）→ `g.state.clear()` + 返回 `(_, _, errInvalidSession)`，下一轮 fresh-IDENTIFY。
5. **Close handling**：close frame 走 `decideOnCloseCode`（见 §7）。
6. **Backoff & reconnect**：见 §7。

`LastSeq` 在每个 dispatch 上 `g.state.setSeq(*frame.S)` 持久化；`state.json` 落盘是 lazy + coalesced（`stateStore` 实现细节）。

## 6. Rate limit handling

`internal/channel/discord/ratelimit.go` 实现 token-bucket limiter，关键表面：

- **Per-route bucket** keyed by Discord's `X-RateLimit-Bucket` header。同一 route 的请求串行化。
- **Global cap**：50 req/s（`DefaultLimiterConfig`）。
- **429 handling**：等待 `max(retryAfter, X-RateLimit-Reset-After)`；标记 bucket 在 reset 时重新放行。
- **Global 429**：`X-RateLimit-Global: true` 触发全局 backoff，所有 route 阻塞到 reset。

REST 客户端 (`httpREST`) 在每次请求前 `limiter.Wait(ctx)`，所以调用层不感知限流。Retry layer (`retry.go::WithTransientRetry`) 处理 5xx + transient transport error，独立于限流。

## 7. Reconnect strategy（close-code 决策表）

`internal/channel/discord/reconnect.go::decideOnCloseCode` 把 Discord close code 映射成 `reconnectAction` + `Backoff`：

| Code | Action       | Backoff | Reason                                              |
| ---- | ------------ | ------- | --------------------------------------------------- |
| 4000 | Reconnect    | 1 s     | unknown error                                       |
| 4001 | Reconnect    | 1 s     | unknown opcode                                      |
| 4003 | Resume       | 1 s     | not authenticated（resume dropped, retry resume）    |
| **4004** | **Stop** | 0     | authentication failed（检查 `bot_token`）            |
| 4007 | ReIdentify   | 1 s     | invalid seq（resume dropped, re-identify）           |
| 4008 | Reconnect    | 30 s    | rate limited（long backoff）                          |
| 4009 | ReIdentify   | 5 s     | session timed out（re-identify）                     |
| **4013** | **Stop** | 0     | invalid intent(s)（检查 intents bitfield）            |
| **4014** | **Stop** | 0     | disallowed intent(s)（在 Developer Portal 启用 privileged intent）|
| other| Reconnect    | 1 s     | 默认 fallback                                        |

调用点：`gateway.go:readLoop` 把 close code 转 `decideOnCloseCode`；当 `Action == Stop` 时 readLoop 返回 `terminal=true`，`connectOnce` 把 terminal 透传给 run，run 包成错误退出 daemon —— 不进入 reconnect 循环。

## 8. OutboundKind → Discord wire mapping

| OutboundKind           | 来源事件                          | Wire 形态                                          | Allowed mentions    | Attachments      |
| ---------------------- | --------------------------------- | -------------------------------------------------- | ------------------- | ---------------- |
| `OutInit`              | runtime session 启动              | `CreateMessage` 一条欢迎气泡                        | `Parse: []`         | 不支持           |
| `OutReply`             | runtime 主回复                    | `CreateMessage`                                     | `Parse: []`         | 不支持           |
| `OutCommandReply`      | `/cwd` `/use` 等命令回复           | `CreateMessage`                                     | `Parse: []`         | 不支持           |
| `OutResult`            | runtime 最终结果                   | `CreateMessage`，记入 `lastResultMessageIDDB`         | `Parse: []`         | 不支持           |
| `OutThinking`          | runtime 思考增量                   | `CreateMessage` 带前缀 `💭 `                         | `Parse: []`         | 不支持           |
| `OutToolStart`         | 工具调用开始                       | `CreateMessage`（`● Tool(args)` 单行）               | `Parse: []`         | 不支持           |
| `OutToolEnd`           | 工具调用结束                       | `CreateMessage`（`⎿  📄 Read → 47 lines` 单行）       | `Parse: []`         | 不支持           |
| `OutTaskCreate`        | 任务列表创建                       | `CreateMessage` 含 `📋 Tasks` markdown checklist      | `Parse: []`         | 不支持           |
| `OutTaskUpdate`        | 任务列表更新                       | `CreateMessage`（同上）                             | `Parse: []`         | 不支持           |
| `OutMessageState`      | `MessageSubmitted`                | `AddReaction(👌)` on user message id               | n/a                 | n/a              |
| `OutMessageStateRemoved` | runtime 撤回                   | `RemoveOwnReaction(👌)`                              | n/a                 | n/a              |
| `OutError`             | 非优雅退出 / 下载失败通知         | `CreateMessage` + 可选 stderr tail                  | `Parse: []`         | 不支持           |
| `OutChoice`            | runtime 交互选择                   | `CreateMessage` 含 V1 `ActionRow[Button]`            | `Parse: []`         | 不支持           |
| `OutChoicePatch`       | 用户选择 / runtime 结算            | `EditMessage`（PATCH），清空或替换 components         | `Parse: []`         | 不支持           |
| `OutHeartbeat`         | runtime 心跳                       | **静默丢弃**（无 native surface）                    | n/a                 | n/a              |
| `OutPromptEnded`       | runtime 提示终止                   | **走 `OnPromptEnded`** → result msg 落 `🎉`/`❌`       | n/a                 | n/a              |

`Send` 中未匹配的 `Kind` 同样走 default 静默丢弃分支（与 `OutHeartbeat` 同路径）。

## 9. Attachments（CDN + F-61 retry）

### 9.1 CDN download

Discord CDN URL 无需认证（每个 URL 独立签名）；`httpREST.Download(ctx, url)` 直接 GET。落盘路径：

```
<dataDir>/discord/<chatID>/<messageID>/<attachmentID>-<sanitized-filename>
```

`<chatID>` 已带 `dc_` 前缀。`<attachmentID>` 前缀保证同 message 上的同名文件不会互相覆盖。

### 9.2 F-61 outer ladder

`downloadAttachmentsWithRetry` (`internal/channel/discord/attachment.go`) 在单次 `downloadAttachments` 之上包 3-attempt ladder：

| Attempt | Backoff 前 |
| -------- | ---------- |
| 1        | 0          |
| 2        | 5 s        |
| 3        | 15 s       |

常量在 `downloadRetryConfig`（与 feishu / telegram 完全一致）。第 1 次立即触发；后续两次前 `time.Sleep` 等 ladder backoff（ctx cancel 时立即返回）。

### 9.3 User-visible failure notice

三轮全部失败后（`downloadResult.AllFailed == true`），`downloadAndPublish` 走两条分支：
- **纯图消息**（`inbound.Text == ""`）：不 publish 到 `a.incoming`（避免幽灵消息进 dispatcher），改发 `OutError` 通知："❌ N attachment(s) failed to download after 3 attempts. Message dropped — please retry."。
- **文本 + 附件**：publish text-only（`inbound.Attachments = nil`），同时发 `OutError` 通知："⚠️ N attachment(s) failed to download after 3 attempts; sending text only."。

通知用 `context.Background()` 起 fresh ctx —— F-61 复盘确认根因就是复用已 cancel 的 inbound ctx。`AttachmentDownloadFailures` 计数器 `+1`（HealthSnapshot §10）。

## 10. Reactions（append-only）

Discord 的 native `AddReaction` / `RemoveOwnReaction` 走 REST：

| Reaction | 触发                                            | 目标消息           |
| -------- | ----------------------------------------------- | ------------------ |
| 👌       | `OutMessageState{State: MessageSubmitted}`       | user message id    |
| 🎉       | `OnPromptEnded{Reason: Success}`                  | last result msg id |
| ❌       | `OnPromptEnded{Reason: Error / Canceled}`         | last result msg id |

`OutMessageState` 落 👌 时按 `(chatID, userMsgID)` 比对 `lastMessageStateDB`，重复 transition 不重复打 emoji。`🎉` / `❌` 优先落 `lastResultMessageIDDB[resultKey(chatID, userMsgID)]`；若 result 没发出（bridge crash、`/think off` 等），回落到 user message id。

`OutMessageStateRemoved` 走 `RemoveOwnReaction`（撤回 👌 时使用）。其它 reaction 选择由 Discord 客户端 hardcode（无 nightme 侧渲染）。

## 11. Acceptance criteria

Phase 1 / Phase 2 / Phase 3 联合验收：

1. **MESSAGE_CREATE → InboundMessage**：DM / guild text / 三种 thread 全部走到 dispatcher；bot 自消息、unsupported channel 静默丢弃。
2. **OutboundKind 完整覆盖**：`OutInit` / `OutReply` / `OutCommandReply` / `OutResult` / `OutThinking` / `OutToolStart` / `OutToolEnd` / `OutTaskCreate` / `OutTaskUpdate` / `OutMessageState` / `OutMessageStateRemoved` / `OutError` / `OutChoice` / `OutChoicePatch` 全部映射到 Discord wire（见 §8 表）。
3. **Choice / Modal**：V1 components 渲染 + `INTERACTION_CREATE` 2.5 s ACK 预算 + type=7/9/5 完整分支 + 3 s interaction token TTL 内部约束。
4. **chatID 命名空间**：所有 channel id 走 `sessionChatID`，prefix `dc_`。
5. **Login 流程**：`nightme login discord` 引导完整 OAuth + intents 启用。
6. **Gateway lifecycle**：Hello / Heartbeat / Identify / Resume 状态机正确；session_id + last_seq 持久化；close code 4014 / 4013 / 4004 → `gw.run` 返回 terminal error（**不进入无限重连**）。
7. **Rate limit**：X-RateLimit-* 头解析；50 req/s global cap；per-route bucket keyed on `X-RateLimit-Bucket`。
8. **Attachment retry**：3-attempt ladder 0/5/15 s；total failure 发一次 `OutError`（或 drop pure-image）；`AttachmentDownloadFailures` 计数器累加。
9. **HealthSnapshot**：返回 enriched payload 含 `reconnect_count` / `last_event_unix_ts` / `last_close_code` / `last_reconnect_unix_ts` / `attachment_download_failures_total`。
10. **Tests**：`go test -race ./internal/channel/discord/...` 全绿；不存在 `-short` 跳过；`make build` / `make fmt` / `make lint` 全绿。

## 12. Known limits

- **Voice / Activity / Rich Presence**：nightme 不发送 voice state、activity、presence。Bot 仅以 `online` 默认 presence 在线。
- **Slash Commands**：nightme 不注册 Discord slash commands；用户发的 `/cwd` `/use` `/clear` 等由 `computeHasMention`（`adapter.go`）通过 `HasMention` 标记为 addressed，由 runtime 解析。`APPLICATION_COMMANDS` intent 在 `DefaultIntents` 外，按需开启。
- **Pin / Unpin**：Discord 有 `pin_message` / `unpin_message` REST，nightme 不调用。
- **Polls**：Discord 的 polls API 不可用；runtime 的 `OutPoll`（若有）会走 default 静默丢弃分支。
- **Multi-shard**：单 bot / 单 daemon。>2 500 guild 需要 Discord 强制 sharding，超出 nightme 当前架构。Operator 配置 bot 数量受限于 1。
- **Forum thread auto-create**：Discord forum channel 的 sentinel thread 不由 bot 自动创建。Bot 只在用户已创建的 thread 里收消息。
- **Custom reaction emoji**：仅支持 Unicode 标准 emoji（`AddReaction` 的 emoji 字段必须是不带 `:` 的 unicode 字面）；自定义 guild emoji 走 `:name:id` 形态但 nightme 当前未使用。
- **Modal input 长度**：Discord 硬限制 4000 字符；超出会被客户端截断。
- **V1 components only**：`IS_COMPONENTS_V2` flag 未启用。`content` + `embeds` + V1 components 共存于单条 message。
- **Thread 消息继承父频道权限**：Discord 的 thread 权限从父频道继承；nightme 不强制覆盖。

## 13. Privileged intents 阈值（10,000-user）

`MESSAGE_CONTENT` 是 privileged intent。Discord 对 <100 guild 的 bot 不强制审核；达到 100 guild 后 Developer Portal 会要求 verify，并通过后才能保留 privileged intent。NightMe 在 `NewAdapter` 中只校验 token 非空，不在启动期强制 intents —— 错误会在首次 IDENTIFY 时以 close code 4014 暴露（见 §7），通过 `daemoncontrol` health RPC 的 `last_close_code: 4014` 直接看出。

---

文档对应源代码版本：`feat-discord-phase-3-reconnect-health-docs` 分支起点（Phase 2 PR #411 之后）。所有行为以本仓库代码为准；任何 Discord 官方文档与本仓库实现的差异以代码为准。
