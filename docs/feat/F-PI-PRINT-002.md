# F-PI-PRINT-002 — pi print-mode AgentBar / audit 补齐 SessionID + Model

## 背景

F-PI-PRINT-001 把 `pi.Starter.RunOnce` 改成了 `--mode json -p` 模式
(`internal/bridge/pi/print.go:runPrintMode`)，绕开 RPC 长 session 在
daemon 负载下的丢事件问题。这条路径跑通后，`/gtw commit` 等 one-shot
命令能稳定完成 commit，但 RunOnce 的返回 `RunResult` 一直缺两个对
AgentBar 渲染关键的字段：

- **`SessionID`** —— `RunResult.SessionID` 在 print-mode 下始终是
  空字符串（`internal/agent/agent.go` 旧注释写「Pi print-mode: empty —
  no session event on that path」）。
- **`Model`** —— assistant 实际跑在哪个模型，print-mode 下也不上
  报。

后果：`replyAgent`（`internal/command/gtw/agent_reply.go`）把
`RunResult` 抄到 `OutboundMessage` 时，`out.Model` / `out.SessionID`
是空，`channel/feishu/usage_footer.go:188-198` 的 AgentBar 渲染只
剩 `🤖: pi`，没有 `· <model> · <sid>`。

UsageBar 同源：`parsePrintStream` 把 `EventAgentResult.Usage` 抄到
`RunResult.Usage` 是正确的，所以 `💰:「 new / cache / out 」` 一段
正常；但 `ContextWindowPct` / `ContextWindow` 在 print-mode 下永远
是 0（没有 `get_state` 响应，`t.contextWindow` 没人写），所以
`X% (window)` 一段缺——这是 **未补**，见下面的"未做的部分"。

## 改动

### 1. `internal/bridge/pi/print.go`

新加 `peekPrintMeta(line, *RunResult)`：

- 在 `parsePrintStream` 的 for-loop 里、每次 `translator.translate`
  之前调用一次（一次 `json.Unmarshal`，开销可忽略）。
- 顶部 short-circuit：两个字段都填上后直接 return，省掉后续
  ~99% line 的 unmarshal。
- `{"type":"session","id":..}` → 第一个非空 `id` 写到
  `result.SessionID`（first-non-empty wins）。
- `{"type":"message_start"|"message_update"|"message_end","message":{"role":"assistant","model":..}}` →
  第一个非空 `message.model` 写到 `result.Model`。
- `user` 角色的 message 不动；malformed JSON 静默吞掉
  （`translator.translate` 走「`pi: translate: ...`」包装路径
  报错，避免双重报告）。
- envelope 结构用 `protocol.go` 里既有的 `eventEnvelope`
  扩展（加上 `ID` + `Message.Role` + `Message.Model`），不在
  `peekPrintMeta` 里再声明匿名 struct——wire shape 只在一处定义。

`appendAuditFields(result, true)` 在 no-settled 失败路径上启用
SessionID 段，让 daemon 日志里能 grep 到 `[session_id=...]`。
更新了误导性的旧注释（说「pi print-mode doesn't surface
SessionID」）。

### 2. `internal/bridge/pi/protocol.go`

`eventEnvelope` 扩展两个字段：

- `ID string` — `{"type":"session","id":..}` 形态。
- `Message { Role, Model string }` — assistant 的
  `message_*` 事件用。

RPC mode 的 `emitConnected` 从 `get_state.data.model` +
`get_state.sessionId` 拿同一组字段，**重复是有意的**——print-mode
没有 `get_state` handshake，只能从 wire event 抓。两者应该一致
（同一进程的 sessionId 和 model），不一致就视为 wire 协议 bug。

### 3. `internal/agent/agent.go`

`RunResult.SessionID` / `RunResult.Model` 字段注释更新：
「Pi print-mode: empty」改成「peeked from the wire via
`internal/bridge/pi/print.go:peekPrintMeta`」。

### 4. 测试

- `internal/bridge/pi/print_internal_test.go`：新加
  `TestPeekPrintMeta`（11 个子用例覆盖 session_with_id /
  session_no_id / message_start_assistant /
  message_update_assistant / message_end_assistant /
  user_message_no_model / unknown_event_type /
  malformed_json_silent / first_non_empty_wins_session /
  first_non_empty_wins_model / session_id_picked_up_when_pre_empty）。
- `internal/bridge/pi/print_mock_test.go`：新加
  `TestPrintMode_Mock_CleanRun_PopulatesSessionIDAndModel`，端到端
  锁住「mock 跑的 print-mode RunOnce 必须返回 `SessionID ==
  "mock-print-session"` 且 `Model == "mock"`」。
- `internal/bridge/pi/print_realpi_test.go`：追加了
  `TestPrintMode_RealPi_PopulatesSessionIDAndModel`（与原有的
  `TestPrintMode_RealPi_CommitPrompt` 共用 `NIGHTME_REAL_PI=1`
  opt-in 开关 + `requireRealPrintMode` 守卫），真机确认
  `SessionID` 是 `01a00145-…` 这种 UUID 形态、`Model` 是
  `sensenova-6.8-flash-lite` 这种实际模型名。比 CommitPrompt
  轻很多（不需要 git repo），wire 协议漂移在秒级被发现。
- `TestAppendAuditFields_SessionIDGated` 的 stale 注释更新。

## 验收

```
go test ./internal/bridge/pi/ -run "PrintMode|PeekPrintMeta|AppendAuditFields" -v
```

实测（`NIGHTME_REAL_PI=1`）：

```
=== RUN   TestPrintMode_RealPi_PopulatesSessionIDAndModel
    print_realpi_test.go:199: SessionID: 01a0014d-2150-739c-b016-5145f201d1df
    print_realpi_test.go:204: Model:     sensenova-6.8-flash-lite
--- PASS
```

`replyAgent` → `OutboundMessage` → `usage_footer.formatStatusBarLines`
的链路完全没动，所以 AgentBar 现在渲染成：

```
🤖: pi · sensenova-6.8-flash-lite · 01a00145-5153-74c6-b9e0-345160d5fde0
```

## 未做的部分

**`ContextWindow` 没补，UsageBar 的 `X% (window)` 在 print-mode 下
依旧缺。**

原因：pi print-mode 的 wire frame 里没有 context window 字段——

- `session` 事件只有 `id / version / timestamp / cwd`。
- `message_start` / `message_update` / `message_end` 的 `message`
  只有 `model`，没有 `contextWindow`。
- RPC mode 是通过 `get_state` 响应拿到 `data.model.contextWindow`
  写到 `translator.contextWindow` 的（`translate.go:1011`）；print-mode
  没有 `get_state` 这一步。

可行的两条路径（都没做）：

1. **本地 model → contextWindow catalog**：在 `internal/bridge/pi/`
   维护一张 `{"sensenova-6.8-flash-lite": 8192, ...}` 的静态表。
   缺点：依赖具体 provider 的模型列表，bridge 升级 / 模型改名 /
   新模型上线时 catalog 容易过期；不适合长期方案。
2. **等 pi 上游在 wire 里暴露 contextWindow**：例如在
   `message_end.message` 上加一个 `contextWindow` 字段，或者在
   print-mode 里也允许 `get_state`。一旦上游支持，bridge 改一行
   就能补上。

短期策略：保持现状（`X% (window)` 在 pi RunOnce 上不显示），
把 `Model` / `SessionID` 这两个本来就在 wire 上的字段补齐。后续
如果业务上需要 `X% (window)`，走 catalog 或等 pi 上游。
