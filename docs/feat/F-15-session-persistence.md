# F-15: Session Persistence (Stdout History)

> **Status**: deferred (v0.2)
> **Milestone**: v0.2
> **Related**: [`../FEATURES.md`](../FEATURES.md#f-15-session-持久化)

## 1. Description (stub)

Session 退出后 stdout 保留到磁盘，nightme 重启后可通过 `nightme history <sid>` 查询。

## 2. 设计方向

- Aggregator flush 时同时写入 `{data_dir}/history/{sid}.log`
- 截断滚动（如保留最后 1MB / 10000 行）
- `nightme history <sid>` 通过 IPC 查询 + 输出

## 3. Open questions

- 历史是否加密（含敏感信息）？
- 是否自动清理（> 30 天的 session history）？
- 是否提供搜索功能？

**详细设计在 v0.2 设计阶段补全。**
