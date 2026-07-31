# F-11: Multi-Channel Mirror Mode

> **Status**: deferred (v0.2)
> **Milestone**: v0.2
> **Related**: [`../FEATURES.md`](../FEATURES.md#f-11-多-channel-mirror-模式)

## 1. Description (stub)

一个 session 的 PTY 输出广播到多个 Chat（飞书 + Web UI + WhatsApp 同时看）。任意 Channel 的输入都进入同一个 PTY stdin。

## 2. 设计方向（待细化）

- `Session.Subscribers []Channel` 替代单一 chat_id
- Channel adapter 的 `Incoming()` 在 Session 内 fan-out
- Aggregator 的 flushFn 改为广播到所有 Subscribers

## 3. Open questions

- 消息一致性：飞书和 Web UI 收到顺序是否一致？
- 跨 Channel 鉴权：同一用户在两个 Channel 同时发命令怎么合并？
- 是否需要写操作互斥锁（防止两个 Channel 同时给 stdin 输入产生混乱）？

**详细设计在 v0.2 设计阶段补全。**
