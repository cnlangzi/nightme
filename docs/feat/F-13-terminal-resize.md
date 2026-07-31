# F-13: Terminal Size Adjustment

> **Status**: deferred (v0.2)
> **Milestone**: v0.2
> **Related**: [`../FEATURES.md`](../FEATURES.md#f-13-终端大小调整)

## 1. Description (stub)

用户手机横竖屏切换 / Web UI resize → PTY SIGWINCH → CLI 收到信号 → 重排输出。

## 2. 设计方向

- Channel adapter 检测屏幕方向 / 浏览器 resize
- 调用 `Bridge.Setsize(cols, rows)`
- Web UI 用 `window.matchMedia` 监听
- 飞书侧：依赖用户手机系统的"屏幕方向"事件，可能不可靠

## 3. Open questions

- 飞书侧是否能检测屏幕方向？倾向：不能，只能用户手动 `/resize`
- 多少列宽是合理的手机屏幕？iPhone 14 约 40 cols

**详细设计在 v0.2 设计阶段补全。**
