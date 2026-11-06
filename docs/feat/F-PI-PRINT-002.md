# F-PI-PRINT-002 — pi one-shot turn 的 AgentBar / audit 字段

## 现状

`pi.Starter.RunOnce`(`/gtw commit`、`/gtw pr`)走 session transport:共享的
`agent.RunSession` 开一个新 session、把 prompt 写进 pi 的 JSONL 管道、drain 事件流、
关掉 session。

`RunResult` 上跟 AgentBar / audit 渲染相关的三个字段因此都有值:

| `RunResult` 字段 | 来源 | 备注 |
|---|---|---|
| `SessionID` | `get_state.sessionId` | 握手阶段拿到,带在 `EventAgentReady` 上 |
| `Model` | `get_state.data.model` | 同上 |
| `Usage.ContextWindowPct` | `get_state.data.model.contextWindow` | bridge 缓存到 translator,每个 turn 的 usage 换算成百分比 |

`replyAgent`(`internal/command/gtw/agent_reply.go`)把 `RunResult` 抄到
`OutboundMessage`,AgentBar 渲染成:

```
🤖: pi · sensenova-6.8-flash-lite · 01a00145-5153-74c6-b9e0-345160d5fde0
```

UsageBar 一段 `💰:「 new / cache / out 」` 正常,`X% (window)` 一段也正常 ——
context window 来自 `get_state`,不是从 wire 事件里猜的。

## 字段是怎么到的

`get_state` 是 pi `--mode rpc` 的握手命令,响应里带 session id 和 model:

```json
{"type":"response","command":"get_state","success":true,
 "data":{"sessionId":"01a00145-…","model":{"id":"…","name":"sensenova-6.8-flash-lite","contextWindow":8192}}}
```

bridge 的 `emitConnected` 把这份状态翻成一个 `EventAgentReady`:

```go
return []agent.AgentEvent{{
    Kind:      agent.EventAgentReady,
    SessionID: sessionID,
    Model:     modelDisplay(modelID, modelName),
    AgentName: t.agentName,
    Workspace: t.workspace,
    ...
}}
```

共享 drain 从这个 Ready 上取 session id 和 model,写进 `RunResult`;
`contextWindow` 由 translator 缓存,每个 turn 的 usage 换算成 `ContextWindowPct`。

**同一组字段在 wire 事件上也有一份**:`{"type":"session","id":..}` 带 session id,
`{"type":"message_start"|"message_update"|"message_end","message":{"role":"assistant","model":..}}`
带 model。`internal/bridge/pi/protocol.go` 的 `eventEnvelope` 因此同时声明
`ID` 和 `Message` —— 那是 wire 自己的一份拷贝。

## 一致性

同一个进程的 session id 和 model,握手和 wire 事件两处来源必须一致。不一致就是
wire 协议漂移,应该报出来而不是挑一个信。

## 测试

- `internal/bridge/pi/session_unix_test.go`:`TestSession_FullRoundTrip` 断言握手
  后的 `EventAgentReady` 带 SessionID + Model。
- `internal/bridge/pi/translate_test.go`:`TestDecodeMessageUsage_ContextWindowPct`
  锁住 usage 里的 `ContextWindowPct` 换算。
- `internal/agent/run_session_test.go`:drain 从 Ready 上取 session id 和 model,
  写进 `RunResult`。
- `internal/agent/bridge_session_largeprompt_real_unix_test.go`
  (`NIGHTME_REAL_LARGE_PROMPT=1`):one-shot turn 用超大 prompt 跑通,返回非空正文。
