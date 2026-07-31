# F-18: Token / API Key Injection

> **Status**: deferred (v0.2)
> **Milestone**: v0.2
> **Related**: [`../FEATURES.md`](../FEATURES.md#f-18-token--api-key-注入)

## 1. Description (stub)

检测 CLI 进入 "hidden input" 模式（密码 / API key）→ 弹出飞书 input card → 用户输入走加密通道，**不**写入飞书聊天记录。

## 2. 设计方向

- 检测 PTY 输出含 `\x1b[8m`（隐藏字符）+ 等待输入模式
- Channel adapter 发送 card "Please enter secret value"（飞书 input 组件）
- 用户输入回传给 nightme（飞书 input card 走独立通道，不进 chat history）
- nightme 写 PTY stdin

## 3. Open questions

- 飞书 input card 真的不进聊天历史？需验证
- 如果用户拒绝输入，CLI 怎么处理？
- 是否做 secret vault（macOS Keychain / 1Password CLI）？

**详细设计在 v0.2 设计阶段补全。**
