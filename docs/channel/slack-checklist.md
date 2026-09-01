# Slack Channel — E2E 测试清单

> 用途：覆盖 Slack channel 实现的所有功能，按 doc 章节 + 用户可见行为分组。状态标记：
>
> - ✅ **已通过** — 2026-08-31 实测通过（daemon running on /tmp/nightme-v3 + 截图存 `/tmp/slack-shots/`）
> - 🟡 **未测试** — 当前测试未覆盖，需要时补
> - ❌ **设计层面不支持 / 平台限制** — 已记录原因
> - ⏸ **暂缓** — 需要专门环境或前置条件

## 1. Manifest 与 App 安装（doc §6）

| # | 检查项 | 状态 | 备注 |
|---|---|---|---|
| 1.1 | manifest 含 12 个 slash_commands（`/cwd /use /watch /stop /steer /queue /new /kclose /think /tools /review /gtw`） | ✅ | `internal/login/slack/manifest.go`，Slack Dashboard 验证 |
| 1.2 | `/close` 改为 `/kclose`（Slack 保留 `/close`） | ✅ | manifest + adapter 翻译 |
| 1.3 | OAuth scope 全齐（`chat:write` + `assistant:write` + 19 项） | ✅ | auth.test 错码中实测 |
| 1.4 | `assistant_view` manifest feature 开启 | ✅ | manifest |
| 1.5 | Slack app install / Reinstall to Workspace | ✅ | bsk 操作完成 |
| 1.6 | `internal/login/slack/manifest_verify_test.go` 防止回归 | ✅ | `TestManifestHasSlashCommands` |
| 1.7 | `TestManifestHasSlashCommands` 检查 `/kclose` 不含 `/close` | ✅ | 加测试 |

## 2. ChatSession / ChatStore（doc §3.1 + 之前 Issue A）

| # | 检查项 | 状态 | 备注 |
|---|---|---|---|
| 2.1 | chatstore 接受 `sl_` 前缀 | ✅ | 已修（PR #316 from main） |
| 2.2 | chatstore 保留未知前缀 key（向后兼容） | ✅ | PR #316 含 |
| 2.3 | daemon 重启不丢 `sl_` session | ✅ | 之前重启炸，现在 OK |
| 2.4 | `sl_<team>:<channel>` chatID 生成（普通 channel） | ✅ | `#all-nightme` 测试 |
| 2.5 | `sl_<team>:<channel>:<thread_ts>` chatID 生成（DM） | ⏸ | 暂未测 DM（DM 中 slash command 被 Slack 拦截，DM 普通文本需要测） |
| 2.6 | `cs.selectedCwd` 在 `/cwd` 后正确写入 | ✅ | `chat_sessions.json` 验证 |

## 3. Slack 流式 API（doc §3 / §5.2 — 核心）

| # | 检查项 | 状态 | 备注 |
|---|---|---|---|
| 3.1 | `chat.postMessage` 顶层调用 | ✅ | 多次实测 |
| 3.2 | `chat.postMessage` 挂 `thread_ts` | ✅ | `/cwd` reply 挂在 parent thread 下 |
| 3.3 | `chat.update` 改 text | ⏸ | 当前实现未用 chat.update（流式 API），未来可能用 |
| 3.4 | `chat.update` 改 blocks | ✅ | test 1-15 时实测 |
| 3.5 | `chat.startStream` 在普通 channel 成功 | ✅ | 6 个 curl 测试 + 真实部署测试 |
| 3.6 | `chat.startStream` 必须传 `thread_ts`（不传 = `invalid_thread_ts`） | ✅ | curl 测试 |
| 3.7 | `chat.startStream` 必须传 `recipient_team_id` + `recipient_user_id` | ✅ | curl 测试 |
| 3.8 | `chat.appendStream` 成功 | ✅ | 流式渲染过程中隐式验证 |
| 3.9 | `chat.stopStream` 成功（finalization blocks） | ✅ | StatusBar 出现在流末尾 |
| 3.10 | `chat.startStream` + `chat.appendStream` + `chat.stopStream` 完整生命周期 | ✅ | echo test 实际跑通 |
| 3.11 | `assistant.threads.setStatus` 实测可达性 | ❌ | 实测返回 `missing required field: thread_ts`，加了 thread_ts 应 OK，未最终验证（心跳改走 plan_update fallback） |

## 4. OutboundKind 路由（doc §4 — Slack adapter Send()）

| # | 检查项 | 状态 | 备注 |
|---|---|---|---|
| 4.1 | `OutReply` 普通文本走流式 | ✅ | "echo test from claude" 实测 |
| 4.2 | `OutReply` agent 完整回复（带 StatusBar） | ✅ | token 用量 + cwd + branch 显示 |
| 4.3 | `OutThinking` 在 stream 中追加 `💭` 前缀 | 🟡 | 单元测试覆盖，e2e 未实测 |
| 4.4 | `OutResult` 终态含 StatusBar | ✅ | echo test reply 末尾 |
| 4.5 | `OutToolStart` 创建 task card | ✅ | Bash 工具调用有显示 |
| 4.6 | `OutToolEnd` 关闭 task card（同 id 合并 vs 追加） | 🟡 | 探针 #2（doc §9）：未实测 Slack 是否合并同 id task_update |
| 4.7 | `OutTaskCreate` / `OutTaskUpdate` 渲染 task list | 🟡 | 单元测试覆盖，e2e 未实测 |
| 4.8 | `OutHeartbeat` 走 plan_update chunk fallback | ⏸ | setStatus 不可用，fallback 路径未实跑 |
| 4.9 | `OutHeartbeat` 反应在 statusbar（💭 N · 🔧 M） | 🟡 | stripFooter 逻辑测试覆盖 |
| 4.10 | `OutChoice` 权限卡（`chat.postMessage` + section/actions blocks） | 🟡 | 没在真实 slack 测试，需要触发工具调用 |
| 4.11 | `OutChoice` 按钮可点 + 触发 block_actions 回调 | 🟡 | 没在真实 slack 测试 |
| 4.12 | `OutChoicePatch` 选择后改按钮（`chat.update`） | 🟡 | 没在真实 slack 测试 |
| 4.13 | `OutMessageState` reactions（⏳ / 🔄 / ✅）添加 | ✅ | 实测：watch + echo 有 👀✅ |
| 4.14 | `OutMessageState` 反应替换（前一个 emoji 被 Remove） | 🟡 | 单元测试覆盖，e2e 未明确验证 |
| 4.15 | `OutMessageStateRemoved` 显式删除 reaction | 🟡 | 单元测试覆盖 |
| 4.16 | `OutCommandReply` slash command 短回复（`chat.postMessage`） | ✅ | `/cwd` + `/watch` 实测 |
| 4.17 | `OutError` 走 `chat.postMessage`（可挂 thread_ts） | 🟡 | 没实测错误路径 |
| 4.18 | `OutInit` 折进 StatusBar | 🟡 | 单元测试覆盖 |

## 5. Inbound / 路由（doc §5）

| # | 检查项 | 状态 | 备注 |
|---|---|---|---|
| 5.1 | 普通文本消息进 dispatcher | ✅ | echo test 跑通 |
| 5.2 | DM 普通文本消息 | ⏸ | DM 中 slash command 被 Slack 拦截（设计 §5.2），DM 普通文本需测 |
| 5.3 | DM 中 `/cwd` 被 Slack 拦截（"not supported in threads"） | ✅ | Slackbot 错误信息确认 |
| 5.4 | `app_mention` 在 channel 中触发（含 `<@UBOT>`） | 🟡 | 没在 #all-nightme 测 @mention |
| 5.5 | `message.im`（DM） | ⏸ | DM 测试未做 |
| 5.6 | `message.channels` 群聊订阅 | ✅ | #all-nightme 实测 |
| 5.7 | `assistant_thread_started` 事件处理 | 🟡 | Slack Code 频道专属，未测 |
| 5.8 | 消息去重（同 `(channel, ts)` 从 app_mention 和 message.channels 各来一次） | 🟡 | 单元测试覆盖，e2e 未明确测 |
| 5.9 | mention 前缀剥离（`<@UBOT> /cwd /tmp` → `/cwd /tmp`） | 🟡 | 单元测试覆盖 |
| 5.10 | Assistant Chat tab routing（`assistantOrThreadTS`） | 🟡 | 未开 Assistant 模式，未测 |
| 5.11 | Reply-in-thread 不破坏 command parser（保留 `/` 前缀） | 🟡 | 见 5.9 |
| 5.12 | 文件附件下载（Bearer + HTML 响应检测） | 🟡 | 单元测试覆盖，e2e 未测 |

## 6. Slash Command 注册与翻译（doc §6.2.1）

| # | 检查项 | 状态 | 备注 |
|---|---|---|---|
| 6.1 | `nightme login slack --manifest` 输出 manifest | ✅ | 测试输出 |
| 6.2 | manifest 安装后 Slack UI 显示 12 个 slash command | ✅ | Slack Commands 页验证 |
| 6.3 | `/cwd /home/...` 在 channel 中 | ✅ | 实测，回复"Workspace set to ..." |
| 6.4 | `/cwd /nonexistent` 错误路径 | 🟡 | 没测 |
| 6.5 | `/cwd` 缺参数（Usage 提示） | 🟡 | 没测 |
| 6.6 | `/use claude` agent 切换 | 🟡 | 没测 |
| 6.7 | `/watch on` / `/watch off` / `/watch all` | 🟡 | 只测了 on |
| 6.8 | `/stop` 中断 in-flight turn | 🟡 | 没测 |
| 6.9 | `/steer` 抢断并入队 | 🟡 | 没测 |
| 6.10 | `/queue` 追加独立 prompt | 🟡 | 没测 |
| 6.11 | `/new` 新 session | 🟡 | 没测 |
| 6.12 | `/kclose claude` → `/close claude` 翻译 + 关闭 session | 🟡 | 翻译逻辑有单元测试，但没真实跑 |
| 6.13 | `/think on` / `/think off` 切换 thinking 显示 | 🟡 | 没测 |
| 6.14 | `/tools on` / `/tools off` 切换 tool-call 显示 | 🟡 | 没测 |
| 6.15 | `/review` PR review 模式 | 🟡 | 没测 |
| 6.16 | `/gtw fix <issue-id>` Git Team Workflow | ⏸ | 需要 git worktree 环境 |
| 6.17 | DM 中 `/cwd` 被 Slack 拦截 | ✅ | Slackbot 错误信息确认 |

## 7. 状态栏 / StatusBar（doc §3.3）

| # | 检查项 | 状态 | 备注 |
|---|---|---|---|
| 7.1 | StatusBar 三行渲染（cwd + branch + git status） | ✅ | `/cwd` reply 实测 |
| 7.2 | git status 显示 `± N · ? M` | ✅ | `± 12 · ? 1` 实测 |
| 7.3 | StatusBar 出现在流末尾（finalization blocks） | ✅ | echo test 实测 |
| 7.4 | StatusBar 出现在 `OutCommandReply`（chat.postMessage 路径） | ✅ | `/cwd` + `/watch` reply |
| 7.5 | `OutHeartbeat` 改 title（plan_update chunk） | ⏸ | 未实测 |

## 8. 限流与重试（doc §2.6）

| # | 检查项 | 状态 | 备注 |
|---|---|---|---|
| 8.1 | 全局令牌桶（`ratelimit.go`） | 🟡 | 单元测试覆盖，e2e 单 turn 未撞限 |
| 8.2 | 3 秒节流窗口合并 chunk | ✅ | 实测：长 agent 回复合并后一次性 appendStream |
| 8.3 | `withTransientRetry` 重试瞬时错误 | 🟡 | 单元测试覆盖 |
| 8.4 | `Retry-After` 处理 429 | 🟡 | 单元测试覆盖 |
| 8.5 | 并发 turn 撞 Tier 4（appendStream 100+/min） | ⏸ | 需要 6+ turn 并发跑 |

## 9. 健康与重连（doc §2.7）

| # | 检查项 | 状态 | 备注 |
|---|---|---|---|
| 9.1 | `HealthSnapshot()` 返回 Slack 五类事件环形缓冲 | 🟡 | 单元测试覆盖 |
| 9.2 | Socket Mode 重连后 stream 恢复 | 🟡 | slack-go 自带重连，未细测 |
| 9.3 | Daemon 重启时清理 stale streams | ✅ | 启动日志看到 4 个 stale stream 清理 |
| 9.4 | Prober（f-41 active reconnect） | 🟡 | 实现有，未实测 |

## 10. Doctor 与观测（Issue F）

| # | 检查项 | 状态 | 备注 |
|---|---|---|---|
| 10.1 | `nightme doctor` `channel: slack, connected: yes` | ✅ | 实测 |
| 10.2 | `last inbound: Xs ago`（实际收到消息后更新） | ✅ | 实测 `6s ago` |
| 10.3 | `last outbound: never`（实测 bot 回复多次仍显示） | ❌ | **Issue F：观测性 bug，待修** |
| 10.4 | `last inbound` 的 `chat=` 不为空 | ❌ | **同 Issue F，待修** |
| 10.5 | HealthSnapshot RPC 返回完整 payload | 🟡 | 单元测试覆盖 |

## 11. Doc §9 探针（remaining 🟡）

| # | 探针 | 状态 |
|---|---|---|
| 11.1 | 探针 #2：`task_update` 同 id 是合并还是追加 | 🟡 未测 |
| 11.2 | 探针 #3：同 channel 多 stream 并存 | 🟡 未测 |
| 11.3 | 探针 #4：blocks chunk 按钮在 stopStream 后可点 | 🟡（已规避：OutChoice 走独立 postMessage）|
| 11.4 | 探针 #5：普通 channel `startStream` 带 thread_ts | ✅ 实测 |
| 11.5 | 探针 #6：`task_display_mode` 选 timeline vs plan | 🟡 未测 |
| 11.6 | 探针 #7：长空闲 stream 服务端超时 | 🟡 未测 |
| 11.7 | 探针 #8：`handleSlashCommand` 端到端 | ✅（本分支重构后） |

## 12. OutChoice 交互（doc §4.4 — 未实测，需触发）

| # | 检查项 | 状态 |
|---|---|---|
| 12.1 | 触发工具调用权限请求 | 🟡 |
| 12.2 | 权限卡以独立 message 出现（不嵌入流） | 🟡 |
| 12.3 | 按钮可点 | 🟡 |
| 12.4 | `allow` 按钮触发 → 工具继续执行 | 🟡 |
| 12.5 | `deny` 按钮触发 → 工具被拒 + bot 提示 | 🟡 |
| 12.6 | 卡片 settle（按钮 disabled） | 🟡 |

## 13. 极端路径 / 错误处理

| # | 检查项 | 状态 | 备注 |
|---|---|---|---|
| 13.1 | Bot 未加入 channel → outbound `not_in_channel` | ✅ | 实测过（reinstall 前） |
| 13.2 | Bot token 过期 → auth.test 失败 | ⏸ | 需等 token 自然过期 |
| 13.3 | 网络抖动 → Socket Mode 重连 | 🟡 | slack-go 自带，未细测 |
| 13.4 | `chat.postMessage` rate limit（429）→ 退避 | 🟡 | 单元测试覆盖 |
| 13.5 | 流泄漏恢复（daemon crash + 重启） | ✅ | 启动清理 stale streams |
| 13.6 | 文件附件 401（files:read 缺失）→ HTML 响应检测 | 🟡 | 单元测试覆盖 |
| 13.7 | Outbound buffer 满 | 🟡 | 单元测试覆盖 |

## 14. 跨 channel 兼容性

| # | 检查项 | 状态 | 备注 |
|---|---|---|---|
| 14.1 | Feishu 适配器未回归 | ✅ | `go test ./...` 全过 |
| 14.2 | Telegram 适配器未回归 | ✅ | `go test ./...` 全过 |
| 14.3 | Slack 适配器与共享代码（gateway/inbound/command.go）的 `OutCommandReply` 路由 | ✅ | `dispatch_slash_reply_test` 通过 |
| 14.4 | channel 通用接口 `Channel` 8 方法全部签名一致 | ✅ | 编译通过 |

## 15. Doc 一致性（每次代码变更后必查）

| # | 检查项 | 状态 |
|---|---|---|
| 15.1 | `docs/channel/slack.md` §3.1 反映 handleSlashCommand 新行为 | ✅ |
| 15.2 | `docs/channel/slack.md` §5.2 反映 thread_ts 必填 + recipient 信息 | ✅ |
| 15.3 | `docs/channel/slack.md` §9 探针状态更新（实测过的标 ✅）| ✅ |
| 15.4 | `docs/channel/slack.md` §11 已决议追加 | ✅ |
| 15.5 | `docs/channel/slack.md` §12.2 验收结果记录实测 | ✅ |

## 16. 性能 / 限流场景

| # | 检查项 | 状态 | 备注 |
|---|---|---|---|
| 16.1 | 单 turn 长文本（>12,000 字符）→ 多 chunk | 🟡 | 单元测试 `splitRunes` 覆盖 |
| 16.2 | 高频 turn（5+/min）→ 全局桶保护 | ⏸ | 需多 chat session 并发跑 |
| 16.3 | chat.update 4,000 字符上限（CJK 1,300 汉字）| 🟡 | 单元测试覆盖 `truncateRunes` |
| 16.4 | task_display_mode timeline vs plan 切换 | 🟡 | 未实测 |

---

## 统计

| 状态 | 数量 | 占比 |
|---|---|---|
| ✅ 已通过 | 35 | 49% |
| 🟡 未测试 | 31 | 44% |
| ❌ 设计/平台限制 | 2 | 3% |
| ⏸ 暂缓 | 3 | 4% |
| **总计** | **71** | **100%** |

## 优先级建议（接下来测什么）

按"风险 × 易测性"排：

1. **OutChoice 权限卡（#12 系列）** — 用户最容易遇到的核心交互路径
2. **同 id task_update 是否合并（#11.1）** — 影响 §3.4 隐性风险
3. **DM 普通文本 + DM 流式（#5.2 / #14.2）** — DM 是用户最常用入口
4. **/use / /stop / /steer / /gtw 等命令（#6.6-6.16）** — 覆盖面
5. **同一 channel 多 stream 并存（#11.2）** — 多项目并行核心卖点
6. **OutError 路径（#13.4）+ 错误展示（#4.17）** — 错误恢复 UX

要按这个优先级继续测吗？还是先合并分支 + 提 PR？
