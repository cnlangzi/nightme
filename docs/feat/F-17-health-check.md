# F-17: Health Check / Heartbeat

> **Status**: deferred (v0.2)
> **Milestone**: v0.2
> **Related**: [`../FEATURES.md`](../FEATURES.md#f-17-健康检查--心跳)

## 1. Description (stub)

Session 失联自动告警，Channel 长连接断自动恢复。

## 2. 设计方向

- 每个 session 有 last_heartbeat timestamp（任何 I/O 更新）
- Background goroutine 每 30s 检查所有 session
- 超过 5min 无 I/O 的 session 发告警
- Channel adapter 心跳：lark-oapi 已有内置，重连失败告警

## 3. Open questions

- 告警通道：复用 Channel 发消息？单独 email？
- 是否区分 "agent 空闲" 和 "session 死掉"？

**详细设计在 v0.2 设计阶段补全。**
