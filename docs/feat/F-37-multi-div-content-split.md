# F-37: Multi-Div Content Split (Receipt 大内容拆 div)

> **Status**: implemented (v1.3.x)
> **Scope**: `internal/channel/feishu/receipt.go` — receipt card 把单 entry 内容拆成多个 `div` 元素,绕过 `div` text 1000 char 硬限
> **目的**: 解决"final reply > 1000 char 被现有 600 B 截断切掉一半"的问题,同时保留 `lark_md` 渲染
> **Related docs**:
> - [docs/feat/F-25-rolling-log.md](./F-25-rolling-log.md) — receipt 整体滚动日志 UX
> - [docs/feat/F-08-channel-abstraction.md](./F-08-channel-abstraction.md) — Channel 自治渲染
> - [docs/channel/feishu.md §10](../channel/feishu.md) — Feishu 字符/元素硬限表
> - [docs/SPEC.md §13.3](../SPEC.md) — `OutResult` 600 字节截断 backlog(本 feature 直接 resolve)
> **设计参考**: cc-connect 多级收紧 (`platform/feishu/feishu.go:6270-6283` `perLane/textLen` 阶梯);openclaw-lark 不做客户端防御,FAIL-through SDK(F-25 §11.2)。

---

## 0. 背景

### 0.1 旧结构问题

v1.3.x 之前,receipt card 里包含完整 event 流:`💭 thinking + 🔧 tool + ✅ done + 💬 answer + ...`。每个 event 一个 `div` 元素,eviction 触发时按 entry 级 FIFO 丢。

**带来的容量问题**:
- 30 KB card body envelope (`Service/im/v1/resource.go:1381` SDK 注释验证)
- 50 elements 上限
- 每个 `div` text 1000 char 上限 (`docs/feishu.md:562`)

夜间存量策略是 conservative 防御:
- `replyMaxBytes = 24 * 1024` (24 KB)
- `replyMaxEntries = 45` (45 + 5 reserve)
- `perEntryMaxBytes = 600` (单 entry 600 B 上限)

旧结构下,30 KB / 50 elements 频繁触发,eviction 是 hot path。

### 0.2 v1.3.x 重构后

tool/think 移到独立 reply 后,receipt 只剩 final reply 内容。重新算账:

| 限制 | 旧结构 | 新结构 |
|---|---|---|
| 30 KB envelope | 频繁触顶 | 几乎触不到 |
| 50 elements | 频繁触顶 | 几乎触不到 |
| `div` text 1000 char | 偶尔触发 | **偶尔触发**(Claude Code 1-3 KB reply) |
| `perEntryMaxBytes` 600 B | 偶尔触发 | **经常触发** ⚠️ |

**问题没消失,只是换了一种表现** —— 30 KB 溢出变成单 entry 600 B 截断。Claude Code 的 `result.Result` 经常 1-3 KB,被 600 B 一刀切掉,用户看到"half answer"(SPEC §13.3 已记录 backlog)。

### 0.3 候选方案

| 方案 | 内容上限 | 保留 markdown | 复杂度 |
|---|---|---|---|
| (a) text fallback | 150 KB (中文 ~50K chars) | ❌ 丢失 | 低 |
| (b) **多 div 拆分**(本 feature) | ~9 KB (中文) / ~26 KB (英文) | ✅ 保留 | 中 |
| (c) 维持 600 B 截断 | 600 B | ✅ | — |

**选择 (b)**: 容量对 Claude Code 实际 final reply 足够 (1-3 KB 远小于 9-26 KB),保住 lark_md 是 UX 关键差异,splitter 是纯函数好测。text fallback 留作将来"硬超 30 KB envelope 的极端 case"再补。

---

## 1. 设计

### 1.1 核心思路

**单 entry 内容 > 1000 chars 时,按段落/语义边界拆成多个 `div` 元素,渲染上拼接成完整 markdown。**

每张 card 的 element 数量从原来的"entry 数 + 3"(header + hr + footer)变成"split 后 div 数 + 3"。每个 div 仍受 `div` text 1000 char 限制,但容量的总封顶移到了 30 KB envelope。

### 1.2 容量算账

| 语言 | 单 div 字节 | envelope 29.5 KB 可放 div 数 | 总内容上限 |
|---|---|---|---|
| 中文 (3 B/char) | 1000 chars × 3 B + ~100 B envelope ≈ 3100 B | ~9 divs | ~9000 chars |
| 英文 (1 B/char) | 1000 chars × 1 B + ~100 B envelope ≈ 1100 B | ~26 divs | ~26000 chars |

**Envelop 30 KB 才是 binding constraint**,50 elements 不会触顶。

### 1.3 Splitter 规则

`splitMarkdownForDivs(text string, maxRunes int) []string` 切分算法:

1. **整段 ≤ maxRunes** → 单元素返回
2. **否则**切 top-level blocks,逐块塞入 current chunk:
   - 当前 chunk + block + 段落间隔 ≤ maxRunes → append
   - 否则 flush 当前 chunk,新开 chunk 放 block
3. **block 本身 > maxRunes**(单 block 超长)→ 递归切
4. **永不切的边界**:
   - **代码块** (`` ```...``` `` 整块必在同一个 div 内)
   - **列表项** (单 `- item` / `1. item` 整行)
5. **soft 切点优先级**(从上到下):
   - 段落边界 `\n\n`
   - 行边界 `\n`
   - 标点后 `,` / `;` / `:` / `.`
   - 空格 ` `
6. **fallback**: 找不到 soft 切点时,在 `maxRunes` 处的 rune 边界硬切

### 1.4 receipt 集成

**改造点 1: `buildReceiptCard` 多 div 路径**

```go
// 内化在 buildReceiptCard(r) 里
for _, e := range r.entries {
    chunks := splitMarkdownForDivs(e.Icon + " " + e.Text, divTextCharLimit)
    for _, chunk := range chunks {
        elements = append(elements, divElement(chunk))
    }
}
```

**改造点 2: `perEntryMaxBytes` 调整**

```go
// 旧 (B): 单 entry 600 B 上限(过 restrictive)
const perEntryMaxBytes = 600

// 新 (rune): 提升到 envelope 预算内,允许 ≥ 1000 char 让 splitter 拆
const perEntryMaxRunes = 8000  // ~9 KB 中文 / ~8 KB 英文
```

调用 `truncateForLog(text, perEntryMaxRunes)` 时,语义从字节变 rune。

**改造点 3: `totalLogBytesLocked` 估算修正**

旧公式按"1 entry = 1 element"算,多 div 后不对:

```go
// 旧
total += len(e.Icon) + 1 + len(e.Text) + perElementOverhead

// 新: 多 div 拆出来的 chunk 数要算
chunks := 1 + (len(e.Text) / divTextCharBytes)  // 估算
total += len(e.Icon) + 1 + len(e.Text) + chunks * perElementOverhead
```

或者更准: 在 `buildReceiptCard` 拿到真实 JSON 长度后缓存到 receipt,跳过估算。

### 1.5 边界 case 处理

| 场景 | 行为 |
|---|---|
| 单 entry ≤ 1000 char | 1 div (现状) |
| 单 entry 1001-3000 char | 2-3 divs,按段落切 |
| 单 entry > 8000 char | 仍按 `perEntryMaxRunes` 截断,落到 8000 char(splitter 输出 ≈ 8 divs) |
| 单 entry 整个是个巨型代码块 | splitter 在 block 边界 flush,code block 单独一个 div(可能 > 1000 char,接受) |
| complete stream > 30 KB envelope | eviction 触发(`replyMaxBytes`),丢最老 entry(级 entry,不级 div) |
| 全部 entries 段落间距大,合计仍超 30 KB | eviction + 末位补 "…(前 N 条已省略)"(现状) |

### 1.6 与现有架构关系

- **不动 Channel interface** — `Send` 仍 fire-and-ack
- **不动 receiptBot interface** — `SendCard` / `PatchMessage` 仍调用
- **不动 receipt FSM** — `Waiting → Executing → Completed/Error` 不变
- **不动 MessageState FSM** — reaction emoji 仍走 userMsgID,与 receipt 解耦
- **不动 F-35 / F-36** — 限速 / retry 在 SDK call 层,跟 splitter 正交
- **直接 resolve SPEC §13.3** — `OutResult` 600 字节截断 backlog 自然消除

---

## 2. 配置

无新增配置项。常量定义在 `internal/channel/feishu/receipt.go`:

```go
// divTextCharLimit = 每个 div 元素的 text 上限 = Feishu 硬限
const divTextCharLimit = 1000

// 与 Feishu lark_md 的 per-line 1000 chars 限制对齐
// 见 docs/feishu.md §10

// perEntryMaxRunes = 单 entry 内容的接受上限(splitter 输入上限)
const perEntryMaxRunes = 8000

// 选取理由:
// - 单 div 1000 chars × 8 divs = 8000 chars
// - 中文 8000 chars × 3 B = 24 KB,加上 envelope 仍卡在 30 KB envelope 内
// - 英文 8000 chars × 1 B = 8 KB,远低于 30 KB envelope
// - 极端情况:超 8000 chars 仍 truncateForLog 截断,但触发概率 < 1%
```

---

## 3. 实现细节

### 3.1 `splitMarkdownForDivs` 函数签名

```go
// splitMarkdownForDivs splits markdown text into chunks, each ≤ maxRunes.
// Each chunk is a paragraph-boundary-respecting markdown fragment that
// renders correctly when wrapped in a Feishu card div element with
// lark_md content.
//
// Preservation guarantees:
//   - Code blocks (```...```) are never split internally
//   - List items (single - / 1. line) are never split internally
//
// Soft split points (in priority order):
//   1. Paragraph boundary (\n\n)
//   2. Line boundary (\n)
//   3. Punctuation followed by space
//   4. Space
//
// Fallback: hard split at rune boundary at maxRunes.
//
// Empty input returns empty slice.
func splitMarkdownForDivs(text string, maxRunes int) []string
```

### 3.2 `buildReceiptCard` 改动

```go
func buildReceiptCard(r *MessageReceipt) (string, error) {
    // ... 不变: header, evicted marker, foot note layout ...

    for _, e := range r.entries {
        // 关键改动: 1 entry → N divs
        chunks := splitMarkdownForDivs(e.Icon+" "+e.Text, divTextCharLimit)
        for _, chunk := range chunks {
            elements = append(elements, map[string]any{
                "tag": "div",
                "text": map[string]any{
                    "tag":     "lark_md",
                    "content": chunk,
                },
            })
        }
    }

    // ... 不变: hr, footer, config ...
}
```

### 3.3 `totalLogBytesLocked` 估算修正

```go
func totalLogBytesLocked(r *MessageReceipt) int {
    const perElementOverhead = 100  // 现 96 → 100,余量稍宽
    total := len(r.state.headerLine(r)) + perElementOverhead
    if r.evicted > 0 {
        total += len(fmt.Sprintf(logEvictedMarker, r.evicted)) + perElementOverhead
    }
    for _, e := range r.entries {
        // 估算: 1 entry 在 divTextCharLimit 边界下会切出几块
        entryText := e.Icon + " " + e.Text
        chunkCount := 1
        if n := len([]rune(entryText)); n > divTextCharLimit {
            chunkCount = (n + divTextCharLimit - 1) / divTextCharLimit
        }
        total += len(entryText) + chunkCount * perElementOverhead
    }
    if note := r.state.footLine(r); note != "" {
        total += len(note) + 2*perElementOverhead
    }
    return total
}
```

> 备选:在 `buildReceiptCard` 拿到真实 JSON 长度后 `r.lastBodySize = len(body)` 缓存,eviction 跳过估算。**第一批实现先走估算**,后续 PR 优化。

### 3.4 `truncateForLog` 限额放宽

```go
// receipt_event.go
func truncateForLog(text string, maxRunes int) (string, bool) {
    runes := []rune(text)
    if len(runes) <= maxRunes {
        return text, runes != nil
    }
    return string(runes[:maxRunes]) + "…(truncated)", true
}

// 调用处
func eventToEntry(ev agent.AgentEvent, now time.Time, last *LogEntry) (LogEntry, bool) {
    // ...
    case agent.EventText:
        text, ok := truncateForLog(text, perEntryMaxRunes)  // 旧 600 → 新 8000
        // ...
}
```

### 3.5 关键不变式

- **`truncateForLog` 仍是 byte/rune aware 边界** — 但 600 B 是 byte 测量,新 8000 runes 是 rune 测量。后者对中文/emoji 安全
- **Eviction 仍是 entry 级** — 不级 div 不级 rune,丢一条 entry 整条消失
- **PATCH 节流不变** — `renderLocked` 的 300ms min interval 保持
- **messageStates 幂等不变** — reaction FSM 走 userMsgID,与 receipt 拆分逻辑独立

---

## 4. 测试计划

### 4.1 `splitMarkdownForDivs` 单元测试 (`receipt_split_test.go`)

| 测试 | 验证 |
|---|---|
| `TestSplit_Empty` | `""` → `[]` |
| `TestSplit_ShortParagraph` | 200 chars → 1 chunk |
| `TestSplit_ExactlyAtLimit` | 1000 chars → 1 chunk |
| `TestSplit_JustOverLimit` | 1001 chars with paragraph break → 2 chunks |
| `TestSplit_MultipleParagraphs` | 5 paragraphs × 300 chars → 1 chunk |
| `TestSplit_SpanningMultipleChunks` | 5 paragraphs × 600 chars → 3 chunks |
| `TestSplit_CodeBlockPreserved` | 含 ``` 块,块不在 chunk 边界被切 |
| `TestSplit_LongCodeBlock` | 1 个 2000 char code block,不被切,即使超 maxRunes |
| `TestSplit_ListPreserved` | 列表项不跨 chunk |
| `TestSplit_ChineseRuneAware` | 中文 800 chars → 1 chunk (按 rune 算) |
| `TestSplit_EmojiRuneAware` | 含 emoji 1000 chars → 1 chunk |
| `TestSplit_HardSplitFallback` | 单 5000 char 无空格 token,硬切 |
| `TestSplit_PunctuationPriority` | 段落边界 > 标点 > 空格 |
| `TestSplit_WhitespaceOnly` | "   \n\n  " → 处理 OK |

### 4.2 `buildReceiptCard` 集成测试 (`receipt_test.go` 增减)

| 测试 | 验证 |
|---|---|
| `TestBuildCard_LongEntry_MultiDivs` | EventResult 2500 chars → JSON 含 3 div 元素,每 div ≤ 1000 chars |
| `TestBuildCard_HugeEntry_StaysBounded` | EventResult 10000 chars → JSON 含 ~10 divs,总 bytes ≤ 30 KB |
| `TestBuildCard_HeaderFooterRespected` | header/footer/evicted marker 不参与 split 仍按 1 element 计算 |
| `TestBuildCard_ReceiptBytesCardinality` | 100 entry × 200 chars → N divs 验证 |

### 4.3 receipt_test.go 回归

| 测试 | 验证 |
|---|---|
| `TestReceipt_RenderLocked_StaysInplace` | 长 final reply 三次 PATCH,同一 receipt ID,内容逐渐变长 |
| `TestReceipt_EvictOverflow_MultiDivAware` | 大量 entries → 触发 eviction,丢最老 entry |

### 4.4 端到端 (E2E)

- ✅ 真实飞书 DM round-trip,发 final reply 1.5 KB → 飞书端看到 ~2 div 拼接的完整 markdown
- ✅ final reply 4 KB → 飞书端看到 ~4 div 拼接,代码块完整保留
- ✅ final reply 50 KB → envelope 超限 → eviction 触发 → marker 显示

---

## 5. 验收

| 项 | 状态 |
|---|---|
| `go build ./...` | 必过 |
| `go test ./internal/channel/feishu/...` | 必过 |
| `go vet ./...` | 必过 |
| `golangci-lint run` | 必过 |
| Split unit tests 14 case 全过 | 必过 |
| receipt_test.go 回归测试全过 | 必过 |
| E2E: 飞书 DM 真实 1.5 KB final reply | 必跑 |
| E2E: 飞书 DM 真实 4 KB final reply | 必跑 |

---

## 6. 落地 checklist

- [ ] `internal/channel/feishu/receipt.go`
  - [ ] 新增 `divTextCharLimit = 1000` 常量
  - [ ] 新增 `perEntryMaxRunes = 8000` 常量
  - [ ] 修改 `perEntryMaxBytes` → `perEntryMaxRunes`(语义: byte → rune)
  - [ ] 修改 `totalLogBytesLocked` 估算多 div
- [ ] `internal/channel/feishu/receipt_split.go` (新文件)
  - [ ] `splitMarkdownForDivs` 函数
- [ ] `internal/channel/feishu/receipt_event.go`
  - [ ] `truncateForLog` 改用 rune 计数
  - [ ] `eventToEntry` 调用 `perEntryMaxRunes`
- [ ] `internal/channel/feishu/adapter.go::buildReceiptCard`
  - [ ] per entry 调 `splitMarkdownForDivs`,emit N divs
- [ ] `internal/channel/feishu/receipt_split_test.go` (新文件)
  - [ ] 14 个单元测试
- [ ] `internal/channel/feishu/receipt_test.go`
  - [ ] 3 个集成测试
- [ ] `docs/SPEC.md`
  - [ ] §13.3 backlog 标记 resolved
  - [ ] §11 backlog 加 F-37 引用
- [ ] `docs/channel/feishu.md`
  - [ ] §10 limits 表加 `div text 1000 chars` 引用为本 feature 触发源
- [ ] `CHANGELOG.md`
  - [ ] v1.3.x 增加 F-37 entry

---

## 7. 已知风险 & 后续

### 7.1 风险

- **超大 entry 仍截断** — `perEntryMaxRunes = 8000` 后仍 truncateForLog,rune 算的 8000 chars 不一定等于 envelope 预算。**实测**:中文 8000 chars ≈ 24 KB,英文 8000 chars ≈ 8 KB。两者在 30 KB envelope 内,安全
- **代码块超 maxRunes 时超出** — 长 code block 超过 1000 char 仍单 div,触发 1000 char 限制 → 飞书可能截掉末段。**极端**:巨型 stack trace 块。**缓解**:未来可在 code block 边界单独 flush,code block 单独 div 不被切;超长则按 code 内部切
- **估算 vs 真实 JSON 字节** — `totalLogBytesLocked` 仍是估算,evict 可能过早或过晚。**缓解**:后续 PR 缓存真实 JSON 长度

### 7.2 后续 PR (backlog,本 feature 不做)

- 真实 JSON 长度缓存,跳过 `totalLogBytesLocked` 估算
- code block 内部 long line 切分(避免 1000 char 限制 binding 到代码块)
- 配 PATCH 失败的 multi-div fallback(单 div 失败 → 退到 text 消息)
- 自适应 `perEntryMaxRunes`(根据实际 envelope 余额动态调整)

---

## 8. 变更日志

- **2026-08-04** — 初始方案。F-37 多 div 拆分设计;替换 SPEC §13.3 backlog 600 B 截断限制
