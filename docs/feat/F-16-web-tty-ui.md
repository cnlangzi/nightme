# F-16: Web TTY UI

> **Status**: deferred (v0.2)
> **Milestone**: v0.2
> **Related**: [`../FEATURES.md`](../FEATURES.md#f-16-web-tty-ui)

## 1. Description (stub)

浏览器实时看 + 操作 session PTY。基于 xterm.js + WebSocket。

## 2. 设计方向

- nightme 启动时同时 serve HTTP（`127.0.0.1:7824/web/`）
- 前端单页应用（React or SolidJS）
- xterm.js 渲染 ANSI escape 序列
- WebSocket 双向：浏览器输入 → PTY stdin；PTY stdout → 浏览器
- 鉴权：本地随机 token + 浏览器本地 cookie

## 3. Open questions

- 是否打包前端到 binary（embed.FS）还是单独 serve？
- 是否需要 mobile 友好的 UI（手机浏览器也能用）？
- 是否和 F-11 multi-channel mirror 整合（web 和飞书同时看）？

**详细设计在 v0.2 设计阶段补全。**
