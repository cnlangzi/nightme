# F-63: Heartbeat — Receipt 顶部活动计数器与最后心跳时间

> **Status**: Revised
> **Date**: 2026-08-16
> **Author**: 夜me
> **Branch**: `fix-working`(原 `feat-hearbeat` 合并后,继续在主仓迭代)
> **触发**: 用户长 turn(30s+)时,飞书占位卡静态显示 "⌨️ Working..." 让人误以为 agent 卡死。需要在 receipt 顶部动态显示 agent 真实进度(thinking / tool 调用次数 + 最近活动时间)。

## 0. 修订记录

### 2026-08-16 — 视觉收敛:Working 前缀与活动计数互斥

原方案在有活动时把 `🤖 Working` 和 `💭 N · 🔧 M · ⏱ HH:MM:SS` 拼在同一行。实际使用中 `🤖 Working` 与 `💭/🔧` 同屏重复表述"agent 在干活"——`💭/🔧` 已经在计数了,前缀显得冗余。

新规则:**两段互斥**。Receipt 顶部只有以下两种形态之一:

1. **前半部分**(无活动) — `🤖 Working`
   - 触发:`ThinkCount == 0 && ToolCount == 0` 且 entries/tasks 都空
   - 视觉语义:"agent 已经在排队,但还没开始动手 / 纯快速回答转瞬即逝"
2. **后半部分**(有活动) — `💭 N · 🔧 M · ⏱ HH:MM:SS`
   - 触发:`ThinkCount > 0 || ToolCount > 0`
   - 视觉语义:"agent 真的在做事"+ "最近活动时刻"
   - ⏱ 时间戳作为"还在活动"的副信号,跟计数同段同生

`🤖 Working` 不再作为后半部分的前缀。两段不会同时出现,二选一。

实现侧的代码改动在 `internal/channel/feishu/adapter.go:buildReceiptCard` 的 switch 与 `renderHeartbeatHeader`,见 §3.6。

---

## 1. 背景与动机

### 1.1 用户体感问题

典型场景:用户向飞书 bot 发送一条复杂任务(例如"重构 nightme 的 event bus"),agent 在 turn 内会经历:

1. 多次 thinking(每次 ~3-10s)
2. 多次 tool 调用(每次 0.5-30s)
3. 偶尔一轮长思考(15s+)

当前 receipt 卡片(`internal/channel/feishu/adapter.go:786` `ensureReceiptForTyping`)在第一条 OutReply 到达前只显示 `⌨️ Working...`;到达后变成 rolling-log。**整个 turn 内 receipt 顶部没有任何"agent 还在推进"的视觉信号**,长 thinking 间隙里用户以为 bot 死了。

### 1.2 核心约束

UX 上必须满足:

- **极简**:不要堆砌明细,只要"次数 + 最后时间"(用户原话)
- **真实**:计数反映 agent **真实动作**,不反映显示策略
- **抗丢**:`/think off` / `/tools off` 不能让计数失真——这两个开关只影响显示,不影响 agent 行为
- **复用现有机制**:不引入新管道,沿用 OutboundMessage 主链路

### 1.3 现有架构能复用的部分

- **OutboundKind 枚举**(`internal/messages/outbound.go`)已是事实上的"事件总线":新增一个 kind 即可被所有 channel adapter 自动按 Kind 分发,无需改 `Channel` 接口
- **runtime handler 的 policy 链**(`internal/runtime/handler.go:203`)是天然的分层点:Translate → Identity stamp → **【插入心跳观测】** → Policies → Send
- **`outbound.emitImpl.Send`** 是单一对外出口:任何 OutboundMessage 经过它都自动获得 telemetry / git status / rate limit 等现成行为
- **Feishu `MessageReceipt`** 已是 PATCH-friendly:同一 `cardMsgID` 上 `renderLocked` 持续更新(300ms 节流 + same-body 短路)

---

## 2. 目标 / 非目标

### 目标

- **观测在最高拦截点**:runtime handler 唯一入口 `HeartbeatTracker.Observe(userMsgID, out.Kind)`,所有 channel 共享同一计数器
- **观测在 policy 之前**:`/think off` / `/tools off` 不影响计数(核心不变量,§3.2 详述)
- **新增 `OutHeartbeat` OutboundKind**:走同一条 `em.Send`,adapter 在 `Send` 里识别并 PATCH receipt 顶部
- **Feishu receipt 顶部 heartbeat header(两段互斥)**:
  - 有活动时(`ThinkCount > 0 || ToolCount > 0`):第一行渲染 `💭 N · 🔧 M · ⏱ HH:MM:SS`
  - 无活动时(`ThinkCount == 0 && ToolCount == 0`):第一行渲染 `🤖 Working` 占位
  - 两段不共存,二选一(详见 §3.6)
- **LRU 自动淘汰**:tracker 不做显式 Drop,满 cap 时最久未用的 userMsgID 自然出队,内存上界可控(~32KB)

### 非目标

- 不加 "Others" 桶:`OutReply` 是流式 chunks,一回复可能 50+ 个,纳入会让数字瞬间膨胀到几百,**完全无信息价值**——`LastBeatAt` 已经承担"agent 还活着"信号
- 不在 `Prompt` 上重复状态:tracker 是唯一来源,`Prompt.Heartbeat` 字段不引入
- 不改 `Channel` 接口:走 OutboundMessage 主流水线,echo / capture / debug 等 adapter 自动 no-op
- 不持久化 heartbeat:daemon 重启即清零(与 `Prompt` 既有约定一致)
- 不做 per-tool 拆解(Read ×3 / Bash ×2):用户明确说"不想显示那么多明细"
- 不做 agent 名字段("🤖 Claude working"):agent 名已在 footer(Line 1 agentbar),顶部不重复

---

## 3. 设计

### 3.1 数据结构:OutboundMessage 上的 Heartbeat 字段

`internal/messages/outbound.go`:

```go
const (
    // ... 现有常量 ...
    OutError
    OutHeartbeat // per-turn 心跳增量:ThinkCount / ToolCount / LastBeatAt
)

type OutboundMessage struct {
    // ... 现有字段 ...
    Heartbeat *HeartbeatSnapshot // 仅 OutHeartbeat 使用
}

// HeartbeatSnapshot per-turn 进度信号。Channel 据此渲染"还在工作"
// 的视觉提示。ThinkCount / ToolCount 单调累加,LastBeatAt 每次刷新。
// 字段语义稳定,不会被 Channel 之外的消费者依赖。
type HeartbeatSnapshot struct {
    ThinkCount int       `json:"think_count"`
    ToolCount  int       `json:"tool_count"`
    LastBeatAt time.Time `json:"last_beat_at"`
}

func (s HeartbeatSnapshot) Empty() bool {
    return s.ThinkCount == 0 && s.ToolCount == 0 && s.LastBeatAt.IsZero()
}
```

### 3.2 ⭐ 核心不变量:观测在 Policy 之前

**这是整个 F-63 最重要的不变量**。任何后续重构都必须保留。

```go
// internal/runtime/handler.go NewEventHandler 闭包(伪代码,完整代码见 commit 3)
out, ok := outbound.Translate(chatID, *ev)
if !ok { return }
out.ReplyTo = userMsgID
// ... identity stamping ...

// ══════════════════════════════════════════════════════════════════
// ⭐ F-63 核心不变量:Heartbeat 观测必须在 Policy 链之前 ⭐
// ══════════════════════════════════════════════════════════════════
// 原因:/think off (ThinkModeGatePolicy) 和 /tools off (ToolsModeGatePolicy)
// 是"显示策略",不是 agent 行为策略。它们在 Policy.Apply 时 drop 消息,
// 但 agent 实际上仍在 thinking / 调工具。计数器必须反映真实动作,
// 才能让用户在关闭显示后仍能感知到 agent 还在推进。
hbChanged := cs.Heartbeat().Observe(userMsgID, out.Kind)
if hbChanged {
    snap := cs.Heartbeat().Snapshot(userMsgID)
    _ = em.Send(context.Background(), messages.OutboundMessage{
        ChatID:    chatID,
        Kind:      messages.OutHeartbeat,
        ReplyTo:   userMsgID,
        Heartbeat: &snap,
    })
}

// ══════════════════════════════════════════════════════════════════
// Policy 链:/think off / /tools off 在这里 drop 原始 Out*
//   但计数已落定 + OutHeartbeat 已发出 → receipt 顶部照常更新
// ══════════════════════════════════════════════════════════════════
for _, p := range policies {
    if p.Apply(&out, env) { return }
}

if err := em.Send(context.Background(), out); err != nil { /* log */ }
```

**这条不变量由 `handler_test.go::TestEventHandler_ThinkOff_StillCounts` 和 `TestEventHandler_ToolsOff_StillCounts` 守护,任一破坏即 test fail。**

### 3.3 观测规则:只计 ThinkCount / ToolCount,刷 LastBeatAt

`internal/runtime/heartbeat.go`(新文件):

```go
const defaultHeartbeatCap = 1024

// HeartbeatTracker per-ChatSession 心跳累计器,LRU 淘汰。
// 不做显式 Drop:userMsgID 在 LRU 自然出队即丢弃。
//
// 计数规则(纯抽象,与 channel 无关):
//   OutThinking  → ThinkCount++  (changed=true)
//   OutToolStart → ToolCount++   (changed=true)
//   其他          → 仅刷 LastBeatAt (changed=false,不触发 OutHeartbeat)
//
// 持久化:不写盘。daemon 重启即清零,与 Prompt 既有约定一致。
type HeartbeatTracker struct {
    mu    sync.Mutex
    cap   int
    order []string                          // 环形 LRU,头=最新,尾=最旧
    snaps map[string]messages.HeartbeatSnapshot
}

// Observe 累计一条计数。返回 changed=true 表示 ThinkCount/ToolCount 变化;
// LastBeatAt 永远刷新但不触发 changed(避免高频 time.Now 触发无意义
// OutHeartbeat)。
func (t *HeartbeatTracker) Observe(userMsgID string, kind messages.OutboundKind) bool {
    if userMsgID == "" { return false }
    t.mu.Lock()
    defer t.mu.Unlock()

    snap, ok := t.snaps[userMsgID]
    snap.LastBeatAt = time.Now()

    switch kind {
    case messages.OutThinking:
        snap.ThinkCount++
    case messages.OutToolStart:
        snap.ToolCount++
    default:
        // 只刷时间,不触发 changed → 不发 OutHeartbeat
        // LastBeatAt 已是"agent 还活着"的统一信号
        t.snaps[userMsgID] = snap
        t.touchLocked(userMsgID)
        return false
    }
    t.snaps[userMsgID] = snap
    t.touchLocked(userMsgID)
    return true
}

// Snapshot 取拷贝,供发 OutHeartbeat 用。
func (t *HeartbeatTracker) Snapshot(userMsgID string) messages.HeartbeatSnapshot {
    t.mu.Lock()
    defer t.mu.Unlock()
    return t.snaps[userMsgID]
}

// touchLocked 把 userMsgID 移到 LRU 头部;超 cap 时淘汰尾部。
func (t *HeartbeatTracker) touchLocked(userMsgID string) {
    for i, u := range t.order {
        if u == userMsgID {
            t.order = append(t.order[:i], t.order[i+1:]...)
            break
        }
    }
    t.order = append([]string{userMsgID}, t.order...)
    for len(t.order) > t.cap {
        evicted := t.order[len(t.order)-1]
        t.order = t.order[:len(t.order)-1]
        delete(t.snaps, evicted)
    }
}
```

LRU 用切片实现,O(n) 但 cap=1024 时每次 Observe 开销 < 1µs,无需引入额外库。

### 3.4 信号链路:OutHeartbeat 走 em.Send,绕过 policy

二次发送的 `OutHeartbeat` 走**同一条** `outbound.Emitter.Send` 管道:

```go
hbChanged := cs.Heartbeat().Observe(userMsgID, out.Kind)
if hbChanged {
    snap := cs.Heartbeat().Snapshot(userMsgID)
    _ = em.Send(context.Background(), messages.OutboundMessage{
        ChatID:    chatID,
        Kind:      messages.OutHeartbeat,
        ReplyTo:   userMsgID,
        Heartbeat: &snap,
    })
}
```

**OutHeartbeat 自身不进入观测路径**(handler 用的是 Translate 的原 OutKind,不是 OutHeartbeat),不会递归触发。
**OutHeartbeat 不进 policy 链**(在 handler 显式 em.Send,绕过 for ... policies),不会被任何未来 gate 吞掉。

### 3.5 渲染层:Adapter 只做事,Feishu 走 receipt.ApplyHeartbeat

`internal/channel/feishu/adapter.go` `Adapter.Send` 加一个 case:

```go
case messages.OutHeartbeat:
    if m.Heartbeat == nil || m.Heartbeat.Empty() {
        return nil
    }
    r := a.receiptFor(ctx, m.ChatID, m.ReplyTo)
    if r == nil {
        return nil
    }
    r.ApplyHeartbeat(ctx, *m.Heartbeat)
    return nil
```

Echo / capture / debug channel 对未识别 Kind 自动 no-op,无需任何改动。

### 3.6 Feishu Receipt:ApplyHeartbeat + 2s 节流 + buildReceiptCard hb 参数

`internal/channel/feishu/receipt.go`:

```go
type MessageReceipt struct {
    // ... 现有字段 ...
    heartbeat            messages.HeartbeatSnapshot
    lastHeartbeatRender  time.Time
    heartbeatMinInterval = 2 * time.Second  // 单独节流,防密集 thinking 流
}

// ApplyHeartbeat 由 Adapter.Send 的 OutHeartbeat 分支调用。
// 写入快照后只在 think/tool 计数发生变化时触发一次 renderLocked;
// 同一快照幂等(多次 ApplyHeartbeat(snap) 不会触发多次 PATCH)。
func (r *MessageReceipt) ApplyHeartbeat(ctx context.Context, snap messages.HeartbeatSnapshot) {
    if r == nil { return }
    r.mu.Lock()
    prev := r.heartbeat
    r.heartbeat = snap
    changed := snap.ThinkCount != prev.ThinkCount || snap.ToolCount != prev.ToolCount
    r.mu.Unlock()
    if !changed { return }

    r.mu.Lock()
    skip := r.shouldThrottleHeartbeat()
    r.mu.Unlock()
    if skip {
        return  // 下次 ApplyHeartbeat 再补
    }

    if err := r.renderLocked(ctx); err != nil {
        r.logger.Warn("feishu receipt: heartbeat render failed",
            "err", err, "card_msg_id", r.cardMsgID)
    }
    r.mu.Lock()
    r.lastHeartbeatRender = time.Now()
    r.mu.Unlock()
}

func (r *MessageReceipt) shouldThrottleHeartbeat() bool {
    if r.heartbeatMinInterval == 0 {
        return false
    }
    return time.Since(r.lastHeartbeatRender) < r.heartbeatMinInterval
}
```

`buildReceiptCard` 签名扩展(向后兼容):

```go
func buildReceiptCard(
    entries []LogEntry,
    tasks []agent.AgentTaskItem,
    footerLines []string,
    hb *messages.HeartbeatSnapshot, // 新增
) (string, error) {
    elements := make([]map[string]any, 0, len(entries)+6)

    // 头:两段互斥 ——
    //   前半部分 = "🤖 Working"   (无活动)
    //   后半部分 = "💭 N · 🔧 M · ⏱ HH:MM:SS"   (有活动)
    // 二选一,不会同时渲染。
    switch {
    case hb != nil && (hb.ThinkCount > 0 || hb.ToolCount > 0):
        // 后半部分:有任意一个计数器 > 0 → 走活动态
        elements = append(elements, map[string]any{
            "tag":     "markdown",
            "content": renderHeartbeatHeader(hb),
        })
    case len(entries) == 0 && len(tasks) == 0:
        // 前半部分:无活动 + 无 rolling-log → 走静默占位
        elements = append(elements, map[string]any{
            "tag":     "markdown",
            "content": "🤖 Working",
        })
    }
    // 其他情况(无活动但 entries/tasks 已有内容):不渲染头部,
    // rolling-log 自身已经表达"agent 在工作"。

    // entries / tasks / footer 完全不变
    // ...
}

// renderHeartbeatHeader 只产出后半部分(三块拼接),不再带 "🤖 Working" 前缀。
//   示例:💭 3 · 🔧 12 · ⏱ 14:35:22
//   - ThinkCount == 0 时省略 💭 块
//   - ToolCount  == 0 时省略 🔧 块
//   - LastBeatAt 为零时省略 ⏱ 块
// 互斥由 buildReceiptCard 的 switch 保证;此处不需判断 ThinkCount/ToolCount 是否全 0。
func renderHeartbeatHeader(hb *messages.HeartbeatSnapshot) string {
    if hb == nil { return "" }
    var parts []string
    if hb.ThinkCount > 0 {
        parts = append(parts, fmt.Sprintf("💭 %d", hb.ThinkCount))
    }
    if hb.ToolCount > 0 {
        parts = append(parts, fmt.Sprintf("🔧 %d", hb.ToolCount))
    }
    if !hb.LastBeatAt.IsZero() {
        parts = append(parts, "⏱ "+hb.LastBeatAt.Format("15:04:05"))
    }
    return strings.Join(parts, " · ")
}
```

**互斥规则**:
- `ThinkCount > 0 || ToolCount > 0` → 后半部分 `💭 N · 🔧 M · ⏱ HH:MM:SS`
- 其他 → 前半部分 `🤖 Working` 占位(仅当 entries/tasks 都空时;否则不渲染头部)

两段不会同时出现。`🤖 Working` 仅在 receipt 还未收到任何 think/tool 事件时出现;一旦首个 think/tool 事件落定(ApplyHeartbeat 触发 PATCH),立刻切到后半部分,后续只要还有活动就一直保持后半部分。

**为什么把"无活动但有时间"的情况归到前半部分**:`LastBeatAt` 是在所有 13 种 OutboundKind 上都刷新的(§3.3 `Observe`),但 `ApplyHeartbeat` 只在 ThinkCount/ToolCount 变化时才真正 PATCH(`receipt.go:ApplyHeartbeat` 的 `changed` 短路)。换言之,receipt 持有的快照一旦 ThinkCount/ToolCount 全 0,意味着从未有过 think/tool 事件,此时即便 LastBeatAt 已被 `OutReply` 推进过、也不曾让 PATCH 触发,所以渲染面读不到"有时间"的状态。理论上"hb 已下发但 think/tool 仍 0"在 receipt 路径上不会持久存在,落到 switch 的"前半"分支是安全的兜底。

所有 `buildReceiptCard(...)` 调用点同步改为传 `&r.heartbeat`(`ensureReceiptForTyping` / `AppendEntryWithFooter` / `SetTaskListWithFooter` / `RolloverTo` 等)。

### 3.7 行为矩阵

| 用户配置 | agent 真实动作 | receipt 卡片内容 | 心跳头部(互斥) |
|---|---|---|---|
| `/think on` `/tools on` (默认) | think × 3 + tool × 5 | 💭 卡片 ×3 + 🔧 行 ×5 + 💬 最终回复 | `💭 3 · 🔧 5 · ⏱ HH:MM:SS`(后半) |
| `/think off` `/tools on` | think × 3 + tool × 5 | (无 thinking 卡片) + 🔧 行 ×5 + 💬 最终回复 | `💭 3 · 🔧 5 · ⏱ HH:MM:SS` ✓ 计数不变 |
| `/think on` `/tools off` | think × 3 + tool × 5 | 💭 卡片 ×3 + (无 tool 行) + 💬 最终回复 | `💭 3 · 🔧 5 · ⏱ HH:MM:SS` ✓ 计数不变 |
| `/think off` `/tools off` | think × 3 + tool × 5 | (无 thinking) + (无 tool) + 💬 最终回复 | `💭 3 · 🔧 5 · ⏱ HH:MM:SS` ✓ 计数不变 |
| 全开,单纯快速回答 (0 think + 0 tool) | 0 think + 0 tool + reply chunks | 💬 最终回复 | `🤖 Working`(前半,无 ⏱) |

第 5 行:无 think/tool 活动时,头部回到前半部分静默占位。⏱ 时间戳只在后半部分出现 —— 既然"无活动"也意味头部不会持续变,⏱ 自然也无需"agent 还活着"的副信号。视觉更干净。

### 3.8 边界 case 显式处理

1. **OutHeartbeat 自身不递归**
   - handler 里 `Observe(out.Kind)` 接收的是 `gateway.Translate(*ev)` 的输出,`ev` 是 bridge 事件,不是 OutHeartbeat
   - 二次发的 OutHeartbeat 走 `em.Send` 直送 adapter,**不进** handler 的 policy 链,更不会再次触发 `Observe`

2. **`gateway.Translate` 返回 `(zero, false)` 时**
   - Translate 已经 drop 了的事件(如空文本、malformed ToolStart)不观测
   - 预期行为:能 Translate 出来的事件才有"真实活动"语义

3. **`EventAgentDone`**
   - Translate 返回 false(terminal marker,无 Out 输出),不观测
   - 心跳头保留作为本轮汇总(receipt 不被销毁,下一次 PATCH 时仍显示)

4. **`EventAgentError`**
   - Translate 在 Diagnostic 非 nil 时发 OutError,否则返回 false
   - 走 default 分支,仅刷 LastBeatAt 不计 count
   - 错误显示通过 OutError 单独通道,不在 heartbeat 顶部

5. **Toggle 模式中途切换**
   - 用户 turn 中途 `/think off` → DefaultPolicies 重建或 Apply 行为变化
   - 已经累计的计数不会回退
   - 后续事件按新 policy 处理,但 Observe 永远在 policy 前 → 计数继续累计

6. **LRU 淘汰与 receipt 状态**
   - tracker 里的 userMsgID 被 LRU 淘汰时,receipt 仍持有自己的 `r.heartbeat` 快照
   - 后续 PATCH 用 receipt 自己的快照,tracker 淘汰不影响渲染
   - 真要触发新 OutHeartbeat 时,Snapshot 取到 zero value,`m.Heartbeat.Empty()` 判定后 adapter 自动 no-op

---

## 4. 代码改动一览

| 文件 | 类型 | 关键改动 |
|---|---|---|
| `internal/messages/outbound.go` | 改 | 新增 `OutHeartbeat` 常量 / `HeartbeatSnapshot` 类型 / `OutboundMessage.Heartbeat` 字段 |
| `internal/runtime/heartbeat.go` | **新** | `HeartbeatTracker`(LRU) + `Observe` / `Snapshot` |
| `internal/runtime/heartbeat_test.go` | **新** | LRU 行为 / Observe 规则 / LastBeatAt 刷新单测 |
| `internal/chatsession/chatsession.go` | 改 | `ChatSession.heartbeat` 字段 + `New()` 初始化 + `Heartbeat()` 访问器 |
| `internal/runtime/handler.go` | 改 | NewEventHandler 闭包加观测 + 二次 Send(§3.2 锁定位) |
| `internal/runtime/handler_test.go` | 改 | ThinkOff/ToolsOff 不影响计数 / OutHeartbeat 不递归等不变量测试 |
| `internal/channel/feishu/adapter.go` | 改 | `Adapter.Send` 加 OutHeartbeat case |
| `internal/channel/feishu/receipt.go` | 改 | MessageReceipt 加 heartbeat 字段 / ApplyHeartbeat / 节流 / buildReceiptCard hb 参数 / 调用点同步 |
| `internal/channel/feishu/adapter_test.go` | 改 | OutHeartbeat 分支 + buildReceiptCard header 渲染单测 |
| `internal/channel/feishu/receipt_test.go` | 改 | ApplyHeartbeat 幂等 + 节流单测 |
| `docs/feat/F-63-heartbeat.md` | **新** | 本文档 |

**预计 diff**: 核心生产代码约 +200 行,含测试 +375 行,含文档 ~440 行。

---

## 5. 用户可见行为(契约)

| 场景 | 修复前 | 修复后 |
|---|---|---|
| 长 turn(30s+)有 think/tool 活动 | 卡片静态显示 `⌨️ Working...` | 顶部 `💭 N · 🔧 M · ⏱ HH:MM:SS` 实时更新(后半) |
| 长 thinking 间隙(15s+,有 think 事件) | 完全无信号 | ⏱ 时间戳持续推进 + 💭 计数递增,证明 agent 还在推理 |
| `/think off` + 长 turn(有 think) | (无 thinking 卡片) + `⌨️ Working...` 一直挂着 | (无 thinking 卡片) + 顶部 `💭 N · 🔧 M · ⏱ HH:MM:SS`,💭 数字照常累计 |
| `/tools off` + 长 turn(有 tool) | (无 tool 行) + `⌨️ Working...` 一直挂着 | (无 tool 行) + 顶部 `💭 N · 🔧 M · ⏱ HH:MM:SS`,🔧 数字照常累计 |
| 快速回答 turn(0 think + 0 tool) | `⌨️ Working...` 短暂后变 reply | 顶部 `🤖 Working` 占位(前半),无 ⏱ 噪声 |
| prompt 结束 | receipt 改 ✅ reaction | receipt 改 ✅ reaction + 顶部保留本轮汇总(`💭/🔧` 终态,后半部分) |
| turn 切换(新一轮 think 事件落定) | (整个 turn 重新计数) | 后半部分保持,`💭` `🔧` `⏱` 自然反映当前 turn;不再有 `🤖 Working` 前缀闪烁 |

---

## 6. 可观测性

- **slog 新增**:`runtime: heartbeat observe` (DEBUG, `user_msg_id` / `kind` / `changed` / `think_count` / `tool_count`),仅 handler 路径 debug 模式下输出
- **slog 新增**:`feishu receipt: heartbeat render failed` (WARN, `card_msg_id` / `err`)
- **telemetry**:OutHeartbeat 走 `em.Send` → `recordOutbound`,自动记录在 `OutboundSample` / `HealthEvent` 里,与 OutReply 等同
- **doctor**:无新增字段;`PROBER` 块(F-61)无变动
- **metrics**:无新增

---

## 7. 测试策略

### 7.1 单元测试

| 文件 | 测试名 | 关键断言 |
|---|---|---|
| `heartbeat_test.go` | `TestObserve_ThinkIncrementsCount` | OutThinking → ThinkCount++,返回 true |
| `heartbeat_test.go` | `TestObserve_ToolStartIncrementsCount` | OutToolStart → ToolCount++,返回 true |
| `heartbeat_test.go` | `TestObserve_ToolEndNoCount` | OutToolEnd → 不增加 ToolCount,返回 false |
| `heartbeat_test.go` | `TestObserve_ReplyNoCount` | OutReply → 不增加任何 count,返回 false |
| `heartbeat_test.go` | `TestObserve_ResultNoCount` | OutResult → 同上 |
| `heartbeat_test.go` | `TestObserve_ErrorNoCount` | OutError → 同上 |
| `heartbeat_test.go` | `TestObserve_AllKindsRefreshLastBeat` | 任意 13 种 OutboundKind 都让 LastBeatAt 推进 |
| `heartbeat_test.go` | `TestObserve_LastBeatAlone` | 仅刷时间不触发 changed |
| `heartbeat_test.go` | `TestObserve_LRUEvicts` | 写入超过 cap 的 uid 后,最久未访问的 uid 被淘汰,`Snapshot` 返回 zero |
| `heartbeat_test.go` | `TestObserve_LRUTouchUpdates` | 重复 Observe 同一 uid 不会让其被淘汰 |
| `heartbeat_test.go` | `TestObserve_ConcurrentSafe` | 并发 Observe + Snapshot,`-race` 不报 |
| `heartbeat_receipt_test.go` | `TestApplyHeartbeat_Idempotent` | 同 snapshot 二次调用不触发 renderLocked |
| `heartbeat_receipt_test.go` | `TestApplyHeartbeat_Throttled` | 2s 内连续 ApplyHeartbeat 多次只渲染 ≤1 次 |
| `heartbeat_receipt_test.go` | `TestApplyHeartbeat_PATCHCardHasHeader` | ApplyHeartbeat 后 buildReceiptCard 输出含 header(back part),且不出现 "🤖 Working" 前缀 |
| `heartbeat_receipt_test.go` | `TestBuildReceiptCard_HeartbeatHeader_NilSnapshot` | hb=nil + 无 entries/tasks → 渲染 front part `"🤖 Working"`,不带任何计数/时间 |
| `heartbeat_receipt_test.go` | `TestBuildReceiptCard_HeartbeatHeader_EmptySnapshot` | hb=zero snapshot + 无 entries/tasks → 渲染 front part `"🤖 Working"`(精确字符串,无后缀点) |
| `heartbeat_receipt_test.go` | `TestBuildReceiptCard_HeartbeatHeader_ThinkOnly` | hb={Think:3, Beat:t} → 后半 `💭 3 · ⏱ HH:MM:SS`;**不**带 "🤖 Working" 前缀;无 🔧 chip |
| `heartbeat_receipt_test.go` | `TestBuildReceiptCard_HeartbeatHeader_ToolOnly` | hb={Tool:12, Beat:t} → 后半 `🔧 12 · ⏱ HH:MM:SS`,无 💭 chip,无 "🤖 Working" 前缀 |
| `heartbeat_receipt_test.go` | `TestBuildReceiptCard_HeartbeatHeader_AllPopulated` | hb={Think:3, Tool:12, Beat:t} → 后半 `💭 3 · 🔧 12 · ⏱ HH:MM:SS`,无 "🤖 Working" 前缀 |
| `heartbeat_receipt_test.go` | `TestBuildReceiptCard_HeartbeatHeader_LastBeatOnly` | hb={Beat:t} + 无 entries/tasks → 渲染 front part `"🤖 Working"`,**不**带 ⏱ chip(新行为;旧的 !hb.Empty() gate 会渲染 `🤖 Working · ⏱ HH:MM:SS`) |
| `heartbeat_receipt_test.go` | `TestBuildReceiptCard_HeartbeatHeader_WithEntries` | hb populated + entries 非空 → header 排在 entries 之前,且不出现 "🤖 Working" 前缀 |
| `heartbeat_receipt_test.go` | `TestBuildReceiptCard_HeartbeatHeader_MutualExclusion` | 7 个子用例的表驱动矩阵(front/empty / back/think-only / back/tool-only / back/think+tool+time / front/LastBeatAt-only / no-header/entries-only / front/nil-hb),逐 case 断言 `wantHas` 与 `wantNot` 不重叠 — 钉死 §3.6 互斥契约的 4-way 行为 |
| `heartbeat_receipt_test.go` | `TestRenderHeartbeatHeader_Direct` | 直接调 `renderHeartbeatHeader` 验证 `{Think:2, Tool:5, Beat:t}` → `"💭 2 · 🔧 5 · ⏱ HH:MM:SS"`(无 Working 前缀);空快照 → `""` |
| `heartbeat_receipt_test.go` | `TestRenderHeartbeatHeader_OmitsWorkingPrefix` | 5 种 hb 输入下,返回值从**任何位置**都不含 "🤖 Working"(守住契约边界) |
| `heartbeat_receipt_test.go` | `TestRenderHeartbeatHeader_NoLastBeat` | 4 个子用例(think-only / tool-only / both / 全零)在 `LastBeatAt=zero` 下输出 back part 不带 ⏱ chip |

### 7.2 集成测试

| 文件 | 测试名 | 场景 | 关键断言 |
|---|---|---|---|
| `handler_test.go` | `TestEventHandler_ThinkOff_StillCounts` | policy = `[ThinkModeGatePolicy{Hide}]` | mock em 收到 `OutHeartbeat{ThinkCount: 1}`,原 OutThinking 被 drop |
| `handler_test.go` | `TestEventHandler_ToolsOff_StillCounts` | policy = `[ToolsModeGatePolicy{Hide}]` | mock em 收到 `OutHeartbeat{ToolCount: 1}`,原 OutToolStart 被 drop |
| `handler_test.go` | `TestEventHandler_BothOff_StillCounts` | 两个 gate 都 Hide | 计数准确,原 Out* 各被 drop |
| `handler_test.go` | `TestEventHandler_ObserveOrder_BeforePolicy` | Observe 返回 false 的事件 | 不发 OutHeartbeat,原 Out* 仍按 policy 决策 |
| `handler_test.go` | `TestEventHandler_OutHeartbeat_NoRecursion` | 二次 Send OutHeartbeat | 总共 2 次 send,OutHeartbeat 不会引发第三次 |
| `handler_test.go` | `TestEventHandler_DefaultMode_NoPolicyDrop` | policies 为空 | 计数与原 Out* 数量一致;OutHeartbeat 与原消息都到达 |

### 7.3 回归

- F-53 `Message`/`Prompt`/`endPrompt` 全量(不动)
- F-61 HungPrompt → respawn 全量(不动)
- `cmd/nightme/test.go` 涉及 `NewEventHandler` 的所有用例

---

## 8. 迁移 / Rollout

- **磁盘 schema**: 不变。`Prompt` 不新增字段,`ChatSession` 也不持久化 heartbeat。
- **daemon 重启**: 行为不变(每次重启心跳从 0 开始,符合用户预期)。
- **回滚**: 5 个 commit 任一不放心 revert 即可,无破坏性 schema 变更。
- **风险点**:
  - `HeartbeatTracker.mu` 持锁时间需短(只做 map + slice 操作,不调外部函数)——已确认
  - `renderLocked` 的 2s 心跳节流不能影响 entries / tasks 的 300ms 节流——已通过单独字段隔离
  - OutHeartbeat 不能被现有任何 policy 吞——已确认(handler 显式 em.Send 绕过 policies)
- **不发版公告**: 走 CHANGELOG.md `Unreleased` 段。

---

## 9. 后续(Out of Scope)

- **多 channel 适配**(Web / TUI):echo / capture 已天然 no-op;未来新增 channel 时只需识别 OutHeartbeat,语义已统一
- **per-tool 拆解**(Read ×3 / Bash ×2):用户明确不要明细,本期不做
- **失败工具计数**(`out.Err != nil` 时额外计 `⚠️`):需要新 OutboundKind 或 OutboundMessage 字段,本期不动
- **心跳持久化**:超出范围,receipt / Prompt 本来就不持久化
- **跨 turn 累计**:每次 Submit 新建 Prompt,计数清零(符合直觉)
- **`/diagnose` 暴露 heartbeat**:诊断面增强,独立 PR

### 时区说明

心跳头里的 `⏱ HH:MM:SS` 时间戳使用 daemon 进程**本地时区**(`time.Time.Format("15:04:05")` 默认走本地时区)。正常部署场景下,daemon 跑在与用户相同的机器上(同一台 Mac / Linux),本地时区一致,飞书客户端显示也用同一时区,无差异。

**daemon 与用户跨时区部署时**(如 daemon 跑在 UTC 机器,用户在 UTC+8 飞书客户端看),心跳头的时间戳会与用户本地时间相差数小时——但仍是"agent 最近活动时间"的真实信号,只是显示上需要脑补时差。如果将来要做严格对齐,需在 `HeartbeatSnapshot` 增加 `Loc *time.Location` 字段或在 render 时按用户时区格式化——本期不做。

---

## 10. 关联文档

- [`F-runtime`](./F-runtime.md) — ChatSession / AgentSession 边界,handler / policy 链所在
- [`F-message-flow`](./F-message-flow.md) — 全链路端到端切片,确认观测点位于何处
- [`F-08-channel-abstraction`](./F-08-channel-abstraction.md) — `Channel` 接口为何不变(走 OutboundMessage 主流水线)
- [`F-CLAUDE-PRINT-002-statusbar-refactor`](./F-CLAUDE-PRINT-002-statusbar-refactor.md) — 相邻的"中央观测 + per-channel 渲染"模式(StatusBarStampPolicy + OutboundMessage.StatusBar)
- [`F-53-message-prompt-lifecycle`](./F-53-message-prompt-lifecycle.md) — `Message`/`Prompt`/`endPrompt` 语义源头,确认 heartbeat 不动这套语义
- [`F-61-bot-failure-recovery`](./F-61-bot-failure-recovery.md) — watchdog / respawn,与心跳独立但同属"agent 健康度"维度
- [`F-62-inflight-cwd-home`](./F-62-inflight-cwd-home.md) — 相邻的 chat 级状态机修复,可参考其 commit 拆分风格
- [`../SPEC.md`](../SPEC.md) §4.3 — 并发约束与 EventCallback 切换
- [`internal/runtime/policy.go`](../../internal/runtime/policy.go) — `ThinkModeGatePolicy` / `ToolsModeGatePolicy` 源码,验证不变量
- [`internal/channel/feishu/adapter.go`](../../internal/channel/feishu/adapter.go) — `ensureReceiptForTyping` / `buildReceiptCard` 源码