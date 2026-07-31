# F-14: Image / File Attachment Passthrough

> **Status**: deferred (v0.2)
> **Milestone**: v0.2
> **Related**: [`../FEATURES.md`](../FEATURES.md#f-14-图片--文件附件透传)

## 1. Description (stub)

Channel 收到的图片 / 文件 → 保存到 workspace 临时目录 → 写 `file://` 路径到 PTY stdin。

## 2. 设计方向

- Channel adapter 在 SendLongMessage 之外加 `SendAttachment(chatID, fileURL)`
- nightme 下载文件到 `{workspace}/.nightme/inbox/`
- 给 PTY 写入 `@.nightme/inbox/{filename}\n`（取决于 agent 是否支持）
- v0.2 不做大文件，限制 < 10MB

## 3. Open questions

- 飞书图片上传到 OSS 有有效期，nightme 需要立即下载
- 不同 agent 引用文件的方式不同（Claude Code 用 `@`，Codex 用 `Read` 工具）
- workspace 在 remote machine 上怎么办（上传/下载延迟）

**详细设计在 v0.2 设计阶段补全。**
