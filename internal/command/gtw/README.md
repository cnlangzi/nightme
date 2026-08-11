# gtw

`/gtw` 是 nightme 的一条 slash-command,把 "在 worktree 里开 PR" 的常见
git 工作流封装成 IM 一来一回的卡片。本文件只规定**回复的排版规约**;
子命令语义、状态机、错误处理各自维护(`wip/gtw-*.md`)。

---

## 1. 标题

每个 reply 卡的**第一行**就是标题:

```
❯ <emoji> <title text>
```

| 部分 | 来源 | 说明 |
|---|---|---|
| `❯` | **feishu adapter** (`adapter.go:1588`) | 每个 `OutCommandReply` 自动加在首行,gtw **不写** |
| `<emoji>` | gtw | 状态/动作标志;见 §1.1 |
| `<title text>` | gtw | 简短陈述句,imperative mood;典型 2–5 个词 |

> **不要**在 gtw 代码里硬写 `❯`。任何 channel 之外添加 `❯` 都会在
> feishu 渲染时双前缀。

### 1.1 emoji 字典

gtw 标题只从一个闭集里选 emoji,避免散乱:

| emoji | 含义 | 例子 |
|---|---|---|
| `✅` | 单步动作完成 | `✅ Local worktree ready` / `✅ PR opened` / `✅ closed \`fix-42\`` |
| `✨` | 无副作用的"现状" | `✨ origin/main already up to date` |
| `❌` | 终态失败 | `❌ git push failed: ...` |
| `⚠️` | 警告 / 部分成功 | `⚠️ hooks config` |
| `🤖` | agent 主导 | `🤖 claude pushed` |

不在表里的 emoji 视为**违规** —— 这张表跟 ADR 一样,改它需要走 PR
+ 团队评审。

---

## 2. 内容

标题之下是"内容"。gtw 当前承认**三种**内容格式,按"gtw 解析多少"
由强到弱排列:

| 格式 | gtw 解析程度 | 适用 |
|---|---|---|
| Format 1 | 全解析(知道每行是字段) | 完成态 + 几个 side effect |
| Format 2 | 半解析(知道 emoji 是字段) | 数据块、列表、嵌套子节 |
| Format 3 | **不解析**(整段 verbatim) | agent / shell / git raw output |



### 2.1 格式 1: `→ + 内容`

每行一个 `→` 后跟**字段**,用于"完成态 + 几个 side effect"或一组
同质字段。**对齐方式**:`  ` (两个空格) 在字段名之间竖对齐,
字段名最长者为准。

```
✅ Local worktree ready
→ branch:   `fix-gtw-hooks`
→ worktree: /Users/.../fix-gtw-hooks
↳ work freely here · or `/gtw close` to drop the worktree
```

```
✅ closed `fix-gtw-hooks`
→ worktree: /Users/.../fix-gtw-hooks (removed)
→ branch: fix-gtw-hooks (deleted)
→ .nightme/gtw.yml (removed with worktree)
→ cwd → /Users/.../nightme
```

**规则:**
- `→` 后面 1 空格,再字段名;字段名后 `:` + 1 空格 + 值
- 多行之间字段名对齐(用空格 pad,不用 tab)
- 字段之间没有空行
- 若卡尾有"下一步动作",用 `↳` 单独一行(见 §3.1)

**反例:**
- 字段值带千把字原始 git output —— 改用 Format 2 的 emoji + 数据块
- `→ branch: X` 后硬接 `━━━━━━━━━━━━━━` —— 出现 ❌ 就是格式越界

### 2.2 格式 2: `<emoji> + 内容`

每行以一个**语义 emoji** 开头,emoji 本身就是字段标记;适合"raw 数据
块、列表、嵌套子节"等一行 `→` 容纳不下的内容。

```
✅ origin/main @ abc1234
📥 pulled 3 commits:
 • feat(gtw): ...
 • fix(gtw): ...
 • chore(deps): ...
```

```
⚠️ hooks config
⚠️ read /Users/.../gtw.yml: permission denied
```

**规则:**
- emoji 是行内**唯一**的字段标记,后面直接跟内容(1 空格)
- 列表项可以用 ` • ` (U+2022) 或 ` - ` 二级缩进,二选一,**全项目统一**
- 内容超过 8 行就考虑用 `━━━━━━━━━━━━━━` 嵌一个命名子节(见 §3.2)

### 2.3 格式 3: `<title> + > <intent> + <raw>` (opaque content block)

用于 "**输出是 opaque,gtw 不解析**" 的场景:agent 输出、shell 命令
输出、git 原始 stderr 等。核心特征是 —— 标题告诉你**做了什么**,
`>` 行告诉你**这条 raw 代表什么**,剩下的就是**字面原文**。

格式:

```
<emoji> <title text>
> <intent>
<raw 行 1>
<raw 行 2>
...
```

**规则:**

- **标题**:走 §1.1;`🤖 <agent> <action>` 或 `✅/⚠️ <action>` 之类
- **`> <intent>` 永远 1 行**:描述 raw 是关于什么的
  - **Agent**: `> <branch>` (entity,该 agent 操作的实体)
  - **Shell**: `> <command>` (verb,跑的命令)
  - **Git raw**: `> <branch>` (entity,push / fetch 目标)
- **raw 内容**:
  - Agent 文本:**不缩进**,verbatim
  - Shell / git 输出:**缩 2 空格**,verbatim
  - **不加** `→` / `📥` / ` > ` 之类的装饰 —— raw 就是 raw
- **失败指示**:当退出码非 0 时,在 raw 块**最前**插一行 `  ❌ exit <N>`
  (只插 exit code,不看 raw 内容);其它错误(超时、未配置等)沿用
  `⚠️ <msg>` 单独一行
- **不截断**:gtw 不主动截断 raw —— 上限交给 channel 自己(feishu 4KB、
  其他同理)。需要全文去 worktree 里的日志

**完整示例**:

Agent (`/gtw push` dirty):
```
🤖 claude pushed
> fix-gtw-hooks
abc1234 fix(gtw): hooks output uses standard ✅ title
```

Shell (`/gtw fix` before-hook):
```
✅ hooks: before
> codegraph init
  Initializing CodeGraph
  Already initialized in /Users/.../nightme
  Use "codegraph index" to re-index
```

Git raw (`/gtw push` clean):
```
✅ pushed
> fix-gtw-hooks
To github.com:foo/bar.git
   abc1234..def5678  fix-gtw-hooks -> fix-gtw-hooks
```

Shell 失败 (`❌ exit N` 注入):
```
✅ hooks: before
> ./run-tests.sh
  ❌ exit 1
  --- FAIL: TestFoo
  foo_test.go:42: expected x, got y
  --- FAIL: 1 of 5 tests passed
```

**反例**:

- raw 行前加 `→` / `📥` / `>` —— 破坏了 opaque 性质
- `> <intent>` 写成多行 —— 永远 1 行
- raw 跟 `> <intent>` 之间插空行 —— 让 raw 紧贴 `>` 行,视觉归属更清晰
- raw 块再加 `━━━━━━━━━━━━━━` 框 —— Format 3 已经够清晰,框就是噪声

---

## 3. 跨格式的辅助元素

### 3.1 `↳` hint 行

完成态卡可用 `↳` 单独一行,告诉用户"接下来你可以做什么"。**只在**
"无条件可执行的下一步"存在时用,例如:

```
↳ work freely here · or `/gtw close` to drop the worktree
↳ `/gtw commit` + `/gtw push` to ship · `/gtw close` to drop the worktree · or keep developing
```

不是 invariant 数据(像 `→ base: ...`),就别用 `↳`。

### 3.2 `━━━━━━━━━━━━━━` section 分隔(谨慎使用)

完整一行 `━`(U+2501,14 个),用于**一个标题下**嵌入一个带标签的
子节。**当前 gtw 没有任何 active reply 在用**;`renderPROpenedCard`
原本保留,2026-08-10 重写为 Format 1(全部 `→`)。下一次写新卡
**优先** Format 1 或 Format 3,这条规则主要给"接 channel 适配器
legacy case"兜底。

**规则:**
- 只在内容 ≥ 4 行、且自带语义标签时用 —— 单纯壳子没用
- 子节标签在分隔线之内的第一行,纯文本,方括号或纯短语都行
- 同一条 reply 内 `━━━━━━━━━━━━━━` 最多出现一次;出现两次就拆 reply
- 能 Format 1/3 替代就别用

> 不要把 `🤖 ... pushed` 改写成 `✅ Pushed by <agent>` —— agent reply
> 是 opaque,gtw 不解析,无法 honest ✅。

---

## 4. 完整样例

### 4.1 Format 1(close)

```
✅ closed `fix-gtw-hooks`
→ worktree: /Users/.../fix-gtw-hooks (removed)
→ branch: fix-gtw-hooks (deleted)
→ .nightme/gtw.yml (removed with worktree)
→ cwd → /Users/.../nightme
```

### 4.2 Format 2(sync)

```
✅ origin/main @ abc1234
📥 pulled 3 commits:
 • feat(gtw): user-level hooks
 • fix(gtw): hooks output uses standard ✅ title
 • chore: bump deps
```

### 4.3 Format 3(`/gtw push` clean)

```
✅ pushed
> fix-gtw-hooks
To github.com:foo/bar.git
   abc1234..def5678  fix-gtw-hooks -> fix-gtw-hooks
```

### 4.4 Format 3 agent(`/gtw push` dirty)

```
🤖 claude pushed
> fix-gtw-hooks
abc1234 fix(gtw): hooks output uses standard ✅ title
```

### 4.5 Format 1(`/gtw pr`)

```
✅ PR opened
→ branch:   fix-gtw-hooks
→ base:     main
→ url:      https://github.com/...
→ worktree: /Users/.../fix-gtw-hooks
```

---

## 5. 何时不是 gtw reply

以下场景**不**走本规约:

- reaction handler 的交互卡(`Card.Title` / `Card.Choices`)—— 见
  `WorktreeFailCard` / `BranchExistsCard`,那是 IM 原生 card 协议
- push / pr 命令从 channel 返回的 raw error(超出 gtw 边界的 stderr)
- daemon 启动横幅

---

## 6. 变更规约

往§1.1 emoji 字典里加条目,或新增"§2.1/2.2/2.3 之外的第四种内容格式",需要:

1. 描述场景 + 一个现有命令需要迁移到新格式的 case study
2. 在本 README 写 patch + 引用 `wip/gtw-*.md` 里的设计决议
3. PR review 必须有至少 1 个 maintainer ack
