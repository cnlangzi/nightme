# F-12: Multi IM Adapter (WhatsApp / Telegram / Slack / Web)

> **Status**: deferred (v0.2)
> **Milestone**: v0.2
> **Related**: [`../FEATURES.md`](../FEATURES.md#f-12-多-im-adapter)

## 1. Description (stub)

实现 `Channel` interface 的其他 IM adapter。

## 2. 优先级

| Adapter | 优先级 | SDK / 备注 |
|---------|--------|-----------|
| WhatsApp | P0 | wa-automate / Baileys（无官方 Go SDK，需自实现）|
| Telegram | P1 | gotd/td（Go 官方）|
| Slack | P2 | slack-go/slack |
| Web UI | P2 | xterm.js + WebSocket（F-16）|

## 3. Open questions

- WhatsApp 无官方 Go SDK，使用第三方是否合规？
- 是否做统一的 Chat 抽象层（不同 IM 的 ChatID 格式不同）？

**详细设计在 v0.2 设计阶段补全。**
