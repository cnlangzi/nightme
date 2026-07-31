# F-18: Token / API Key Injection [CANCELLED]

> **Status**: **cancelled** (2026-07-31)
> **Decision**: 违背 nightme "完全透传"原则（PRD §4.1），采用透传替代
> **Related**: [PRD §4.1](../PRD.md#41-完全透传不解析), [`../FEATURES.md`](../FEATURES.md)

## 决策记录

原计划：检测 CLI 进入 "hidden input" 模式（密码 / API key）→ 弹出飞书 input card → 用户输入走加密通道，不写入飞书聊天记录。

**为什么取消**：
- nightme 的整个存在意义是"完全透传"
- 多一层"密码检测 / 飞书 card 重定向"等于在 byte pipe 中间插入一个语义识别器
- 复杂度上升、安全收益有限（飞书聊天记录本身就不算高安全边界）
- 违背 PRD §3 "Minimal 原则"

**新方案**：密码 / API key 跟普通文本一样，从 IM 输入 → nightme 原样转给 PTY stdin。代价是密码出现在 IM 聊天记录——这是透传的必然结果，用户已知情。

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
