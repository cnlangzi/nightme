# NightMe

> 安享好梦，NightMe 彻夜向前。
>
> 奔赴你的星辰大海，拥有你的自由生活。而那些必须死守电脑、避无可避的无奈，就让 NightMe 替你守候。

[English](./README.md) · **简体中文**

![Status](https://img.shields.io/badge/status-development-blue)
![Go](https://img.shields.io/badge/go-1.22%2B-00ADD8)
![License](https://img.shields.io/badge/license-MIT-green)
![Platforms](https://img.shields.io/badge/platforms-macOS%20%7C%20Linux%20%7C%20Windows-lightgrey)
![Single Binary](https://img.shields.io/badge/distribution-single%20binary-success)

## Install

```bash
curl -fsSL https://nightme.dev/install.sh | bash
```

---

## NightMe 能做什么

### 三个真实场景

**场景 A：多个项目一起跑**

在一台机器上同时开三个 Chat Session：

- Chat #1 在重构你的鉴权服务
- Chat #2 在改计费仓库的 bug
- Chat #3 在写一个临时的迁移脚本

一台机器，三个项目并行。openclaw 之类的工具切会话就丢上下文，NightMe 让每个会话一直活着，对话历史一条不漏。

**场景 B：单仓库多任务**

你一直在 main 上聊。每开一个支线任务，就 `/gtw fix` 一下——自动开 worktree，把你拉进一个绑定该分支的新 Chat Session。不用再和分支较劲、不用 stash 一把梭、切分支也不用烧香。

**场景 C：同一 Chat 里换 agent**

重构用 Claude，写模板用 Codex，查文档用 Pi。`/use claude` ↔ `/use codex` ↔ `/use pi`，老 agent 进程留在池子里活着，切回去上下文还在。

### 三项核心能力

| 能力 | 实际意义 |
|---|---|
| **并行多 Chat Session** | 一台机器 N 个会话，各自对应不同项目或任务。 |
| **项目 × 工作区，任意切换** | `/cwd` 绑定或切换工作区。旧工作区缓存不杀。 |
| **同 Chat 多 agent** | `/use <agent>` 切当前 agent，老的留在池子里带上下文。 |

---

## `/gtw`：把 Git 工作流做成斜杠命令

内置的 Git 团队工作流。每一步就是一个斜杠命令，回复是一张结构化卡片（分支 / 基线 / URL / worktree），不是一坨 `git` 灌水。

```
/gtw fix                              # 开 worktree + 提议分支 + 拉起 agent
/gtw push                             # 提交 + 推送（用配置的 agent 写 dirty-push 的 commit message）
/gtw pr                               # 通过 gh / glab 开 PR
/gtw close                            # 拆 worktree，回到 main
/gtw sync                             # 把 origin/main 拉进 worktree 并 fast-forward
```

### ★ Hooks：把开发环境一起带过去

AI 工具的索引（CodeGraph、语言服务器、缓存）一般放在仓库里。开 worktree 就得重建——目前你得手敲 `codegraph init`。

Hooks 就是干这事的。编辑 `~/.nightme/gtw.yml`：

```yaml
# ~/.nightme/gtw.yml
fix:
  hooks:
    after:
      - codegraph init                # 裸字符串 = shell hook
      - npm install
      - go mod download
```

写法简繁都行：

- `- codegraph init` — 简写，当 shell hook 处理
- `- type: shell / run: codegraph init` — 长写，语义一致（为以后的 `type: agent` / `type: notify` 留口子）

每个命令都有 `hooks.before` 和 `hooks.after`：

| Hook | 触发时机 | 典型用途 |
|---|---|---|
| `before` | 主流程之前 | 记录起始 SHA、拍状态快照 |
| `after` | 主流程之后（即便失败也跑） | `codegraph init`、装依赖、暖缓存 |

铁律（来自代码）：

- v1 只支持 **shell hook**——其他类型会告警并跳过。
- Hook 失败 **绝不阻塞主流程**。挂了就在回复里挂个 `⚠️` 提示，主命令继续。
- stdout / stderr 全部回显，能看到实际跑过什么。
- 单个 hook 默认 30s 超时。

### ★ 轻量 agent 干杂活

`/gtw` 里的杂活（写 commit message 之类）不需要重型编码 agent——那东西会往对话里塞上下文，浪费 token。push 这类动作可以走轻量 agent（Pi 或类似），在 yml 里通过 `<cmd>.agent` 指定：

```yaml
# ~/.nightme/gtw.yml
push:
  agent: pi                          # 写 commit message 用的轻量 agent
```

Agent 选择走三级优先级：

| 优先级 | 来源 | 示例 |
|---|---|---|
| 1 | CLI flag | `/gtw push -a claude` |
| 2 | yml `<cmd>.agent` | `push: agent: pi` |
| 3 | 当前 Chat 的 `/use` agent | — |
| 兜底 | 都没设 | 维持原 `❌ no agent selected` 行为 |

**作用域（依据代码）**：`push.agent` 在 `pushDirty` 里生效；`pushClean` 是纯 `git push -u origin`，不带 agent。`fix / close / sync` 预留 `agent` 字段以备将来，当前不消费。

**降级策略**：yml.agent 配了一个没注册的 agent（比如 `pi` 不在你的 agents 列表里），NightMe 会先警告（`⚠️ gtw.yml agent "pi" not found; falling back to session default`）再退回优先级 3——绝不在你不知情的情况下换 agent。

**实际效果**：重型思考留在主 Chat 的 Claude / Codex 上。`/gtw` 让 Pi 写 commit message，让 shell hook 跑 init 脚本。主会话保持干净，token 省下来。

### `/gtw` 怎么和「多 Chat + 多 agent」配合

- Chat #1 —— 继续在 main 上用 Claude 改东西
- Chat #2 —— `/gtw fix` → 开 worktree → hook 重建索引 → 一次性 Pi 生成 Conventional Commit → `/gtw pr` 开 PR
- 每个 worktree 都有自己的 AI 索引、自己的依赖、自己的 agent 进程，互不打架。

---

## Always-In-The-Loop

大多数「AI 写代码 + 聊天」工具都跟黑盒一样——你发一段、滚一屏、心里没底。NightMe 把「可观测」当成一等公民。

### StatusBar：飞书 Kino 页脚卡片

agent 发的每条消息在 Kino 上都带一个固定页脚卡片，告诉你关键信息，不用来回切窗口：

- **工作目录** —— 当前活跃的是哪个项目 / 哪个分支
- **Git 状态** —— 分支名、是否 dirty、ahead / behind
- **Agent 状态** —— `idle` / `running` / `thinking`
- **Token 用量** —— 当前 session 已用 / 上限

别的工具把你丢进黑盒。NightMe 让你随时看到 agent 在哪、干什么、花了多少。

### Flexible visibility

| 开关 | 作用 |
|---|---|
| `/think on\|off` | 是否展示 agent 的思考过程。 |
| `/tools on\|off` | 是否展示每个工具的独立线程回复（默认关，卡片更干净）。 |
| `/watch on\|off` | 当前 Chat 是否监听群消息（默认群内只听 `@bot` / `@_all`）。 |
| 主动进度 + 显式确认 | 关键节点主动推上来，不用刷新也能知道 agent 还活着。 |

**为什么这很重要**：大半夜你拿手机驱动多 agent 干活，「我知道发生了什么」和「我在猜发生了什么」之间的距离，就是「高效」和「窝火」之间的距离。NightMe 默认把状态摊开，想安静的时候再切到静音。

---

## 稳定可预测

针对 openclaw 之类工具的三个老毛病，NightMe 一一接住：

| 痛点 | NightMe 的解法 |
|---|---|
| Agent 跑到一半挂了，对话没了 | **进程级恢复。** 守护进程 / 网络 / 休眠中断——重启后所有 ChatSession 自动重连，所有处于 `StatusDetached` 的 AgentSession 用 `--resume <session-id>`（Claude / Pi 走等价机制）重新拉起。对话不丢。 |
| 静默超时被踢下线 | **会话由用户管理。** 没有「N 分钟自动断开」这种意外。`/stop` 停掉当前 turn（会话保留）、`/steer` 改方向（会话保留）、`/new` 清上下文（进程保留）、`/close` 终止 bridge 进程（会话记录保留，下次按需恢复）。 |
| 项目记忆跑到一半蒸发 | **默认一直压缩，永不归零。** 显式 `/new` 才会重置 agent 对话上下文。 |

原则：NightMe **不自己造一套记忆系统**。上下文交给上游的 Claude Code / Codex / Pi 自己管——你本来就在为它们付费。表现稳定可预期：你看到什么，CLI 看到什么，NightMe 不夹私货。CLI 自己压上下文你就知道；NightMe 啥都不干你也知道。

---

## 为编码而生，但不锁死在编码

默认工作流是编码。命令、`/gtw`、agent 菜单——都围着开发场景转。

但 NightMe 实际上是个 **透传字节的守护进程，驱动一个现成的 CLI 进程**。不改 prompt、不自带 agent runtime、不装作比你的 CLI 更懂——所以：

- 编码工作流？给你最顺手的配置。`gtw fix / push / pr / close / sync` 就是你想要的团队工作流。
- 非编码 agent 任务？也能跑。CLI 跑得动，NightMe 就驱动得了。
- 想换 CLI？在 config 里加个 `agents:` 项就完事，编排层不用动。

NightMe 不抢 LangChain / OpenAI Agents 的赛道——它不是「通用 Agent 框架」。它说的是一件更窄、更硬的事：**为你已经在用的那些 CLI 做编码工作流编排。**

---

## Quick reference

### Chat 输入路由

每条入站消息按首字符分派，三条路由由独立 package 拥有，互不串味：

| 前缀 | 分派给 | Package |
|---|---|---|
| `!` / `！`（全角） | Shell 分派——在当前 Chat 的 CWD 用 `sh -c` / `cmd /c` 执行 | `internal/shell/` |
| `/` | 命令分派——斜杠命令（`/cwd`、`/use`、`/gtw fix` …） | `internal/command/` |
| 其他 | Agent prompt——转给当前活跃的 AgentSession | （无 package——默认路由） |

### Shell 模式（`!cmd`）

行首的 `!`（或全角 `！`）把后面那段当真正的 shell 命令，**在当前 Chat 的 CWD 下**执行——不绕 agent，不需要 `/cwd` / `/use` 上下文，前提是这个 Chat 绑了工作区。回复是一张 C 风格摘要卡：

```
✅ $ ls -la
exit 0 · 12ms · /Users/you/projects/foo
stdout:
  drwxr-xr-x  …
  -rw-r--r--  …
```

```
❌ $ go test ./...
exit 1 · 4321ms · /Users/you/projects/foo
stdout:
  ok  	github.com/foo/bar	0.124s
stderr:
  # github.com/foo/baz
  ./baz.go:42:9: undefined: qux
```

**适合的场景**：快速侦察（`!ls`、`!git status`、`!tail -n 50 app.log`）、环境探针（`!go version`、`!which gh`），或任何不值得跑一次 agent round-trip 的事。CWD 永远是你最近一次 `/cwd` 绑定的那个工作区，所以 `!cmd` 总是落在你正在看的那份代码上。

**规则**（由 `parseShell` + `internal/shell/dispatch_test.go` 锁住）：

- **必须行首。** `!cmd` 命中，`echo !hi` 不命中（`!` 必须是第一个非空白字符）。
- **空内容就是空操作。** 单独的 `!` 或 `!   ` 直接落到 agent prompt——不会误触发空 shell。
- **两种叹号都行。** `!cmd` 和 `！cmd` 等价，手机 / 全角输入法的用户不用切键盘。
- **没绑 CWD → 友好报错。** 还没绑工作区的话，回复这张卡，啥也不跑：

  ```
  ❌ shell: no CWD configured for this chat
  Try `/use <path>` first.
  ```
- **5 分钟上限。** `!cmd` 超过 5 分钟会被砍掉。耗时更长的活儿自己上 screen / tmux。
- **异步 + 尽力回复。** 命令在 detached goroutine 里跑，结果以线程回复的形式发出去；网关立刻返回——慢 `!cmd` 不会卡住下一条消息。回复是尽力而为——如果守护进程正在重启（`!make restart`），新守护进程会重新接 Chat，结果可能落在那里。
- **Panic-safe。** 命令出岔（或发送方出 bug）会在 goroutine 里 recover——守护进程不掉，你丢一条回复，没别的。

**跨平台**：macOS / Linux 用 `sh -c <cmd>`；Windows 用 `cmd /c <cmd>`。通过 build tag 隔离在 `internal/shell/dispatch_unix.go` / `dispatch_windows.go`。

**输出截断**：stdout 默认内联前 50 行；超过就显示 `… N more lines truncated`，避免 `!cat huge.log` 把 IM 消息体撑爆。stderr 不限行数，但永远在 stdout 后面。

### 斜杠命令

| 命令 | 干什么 |
|---|---|
| `/cwd <path>` | 绑定这个 Chat 到一个工作区。会校验路径，下次发消息时 lazy-spawn。 |
| `/use <agent>` | 切当前 agent（`claude` / `codex` / `opencode` / `pi`）。复用或新拉。 |
| `/stop` | 停掉当前 agent 上的 in-flight turn。会话留着，队列里的消息继续流。 |
| `/steer <msg>` | 停掉 in-flight turn 并把 `<msg>` 插到队首。下个 turn agent 第一眼看到的就是这条。 |
| `/close [agent]` | 终止当前工作区里 AgentSession 的 bridge 进程。AgentSession 记录保留——下次发消息触发 respawn 时会用 `--resume <sessionID>` 接着聊。 |
| `/new [agent]` | 重置 agent 对话上下文（Claude Code 的 `/clear` 等价）。进程保留，队列清空。 |
| `/watch on\|off` | 当前 Chat 的消息监听模式（默认群内只听 `@bot` / `@_all`）。 |
| `/think on\|off` | 是否在回复卡里展示 agent 思考过程。 |
| `/tools on\|off` | 是否展示每个工具的独立线程回复（默认关）。 |
| `/gtw fix [-a <agent>]` | 在 `git worktree` 里拉起一次性 agent，自动提议分支名 + 任务。 |
| `/gtw push [-a <agent>]` | 提交 + 推送；回复卡里展示分支 / 基线 / URL。 |
| `/gtw pr  [-a <agent>]` | 一次性 agent 生成 Conventional Commits 标题 + 正文，通过 `gh` / `glab` 开 PR。 |
| `/gtw close` | 拆 worktree，回到 main，删分支。 |
| `/gtw sync` | 把 `origin/main` 拉进 worktree，fast-forward。 |
| `/help` | 在 Chat 里列出所有斜杠命令。 |

所有斜杠命令都走 `command.Commander` / `Registry` / `Factory`（`internal/command/`）——加一个新命令就是一次 `Factory` 注册，网关和 channel 层不用动。

### `/gtw` hooks 速查

```yaml
# ~/.nightme/gtw.yml
fix:
  hooks:
    before: [echo "starting fix flow"]
    after:  [codegraph init, npm install, go mod download]

push:
  agent: pi                          # 写 commit message 的轻量 agent

# close:  # 预留
# sync:   # 预留
```

---

## NightMe 和同类工具的差异

NightMe 面向的是「已经在本地用多个 AI 编码 CLI、想从聊天里调度它们」的开发者。对比一下同类：

| Project | Process keep-alive on switch | Worktree-as-slash-command | StatusBar / transparency | Flexible visibility |
|---|---|---|---|---|
| **NightMe** | ✅ 池化——老 CLI 进程活着，对话保留 | ✅ `/gtw fix / push / pr / close / sync` | ✅ Kino 页脚展示 cwd / git / agent / token | ✅ `/think /tools /watch` + 主动进度 |
| openclaw | ❌ 切工作区就重启 | ❌ | ❌ | ⚠️ |
| cc-connect | ⚠️ 每项目一个进程，池化弱 | ⚠️ 通用 cron + memory，不感知 worktree | ⚠️ | ⚠️ |
| happycoder | ❌ 单 agent，不保活 | ❌ | ❌ | ⚠️ |
| hermes | — | ❌ | ❌ | — |

NightMe 真正比同行多的：

- **真正的进程保活，不是「多 agent 编排」。** 切回上一个 agent 是瞬时的——同一个 CLI 进程，同一段对话。
- **`/gtw` 是一等公民的工作流。** fix → push → pr → close → sync，每一步一张 IM 回复卡。`cc-connect` 给你通用 `cron` + `memory`；`openclaw` 完全没有。
- **状态栏透明。** 其他工具把你丢进黑盒。NightMe 每条回复都展示 cwd、git 状态、agent 状态、token 用量。
- **Hooks 自动搭开发环境。** `codegraph init` 在 worktree 开起来之后自动跑——不用手敲重初始化。
- **轻量 agent 干杂活。** push 的 commit message 默认走 `pi`，不是主 Chat 里那个重的 Claude。省 token。
- **单文件 Go 静态二进制。** ~30 MB。没有 Node + 插件宿主 + LLM 栈。没有 Python 虚拟环境。没有手机端 App。

---

## Configuration

NightMe 从 `~/.nightme/config.yaml`（或 `$NIGHTME_CONFIG` 指定的文件）读 YAML。环境变量可覆盖：`NIGHTME_<SECTION>_<KEY>`（如 `NIGHTME_PRIMARY`）。

```yaml
primary: claude                          # 全局默认 agent

agents:                                  # 列表：每项 = name / bridge / command
  - name: claude
    bridge: claude
    command: "claude --dangerously-skip-permissions"
  - name: codex
    bridge: codex
    command: codex
  - name: opencode
    bridge: opencode
    command: opencode
  - name: pi
    bridge: pi
    command: "pi"

feishu:
  app_id: "cli_xxxxxxxxxxxxxxxx"
  app_secret: "xxxxxx…xxxx"
  verification_token: ""
  encrypt_key: ""

session:
  default_pty_cols: 80
  default_pty_rows: 24
  output_chunk_size: 4096
  output_flush_interval_ms: 200

logging:
  level: "info"          # debug | info | warn | error
  file: ""               # 空 = stdout；非空 = 文件路径

paths:
  data_dir: "~/.nightme"
```

`/gtw` 工作流读的是 **另一个文件**：`~/.nightme/gtw.yml`——见上文的 [hooks 速查](#gtw-hooks-速查)。

完整 schema 和每个 bridge 的说明见 [`configs/nightme.example.yaml`](./configs/nightme.example.yaml)。

日志写到 `~/.nightme/nightme.log`（权限 `0600`），JSON 格式。属性名里含 `secret`、`token`、`password` 的会被自动改成 `***REDACTED***`。

---

## Documentation

| 文档 | 内容 |
|---|---|
| [`docs/PRD.md`](./docs/PRD.md) | 产品定义——做什么、为什么做、为谁做。不讲技术。 |
| [`docs/SPEC.md`](./docs/SPEC.md) | 技术架构——组件、数据流、NFR。 |
| [`docs/FEATURES.md`](./docs/FEATURES.md) | 功能索引——每个 F-XX 一行。 |
| [`docs/feat/`](./docs/feat/) | 每个 feature 的设计文档。 |
| [`docs/bridge/`](./docs/bridge/) | 每个 agent bridge 的设计：claude、codex、opencode、pi。 |
| [`docs/channel/feishu.md`](./docs/channel/feishu.md) | 飞书 adapter 参考（渲染规则、卡片语义、线程路由）。 |
| [`docs/E2E_TESTING.md`](./docs/E2E_TESTING.md) | 飞书端到端手动测试 + 排错。 |
| [`CHANGELOG.md`](./CHANGELOG.md) | 当前 snapshot（单 `[Unreleased]` 段）。 |
| [`MIGRATION.md`](./MIGRATION.md) | 历史 snapshot 之间的 breaking change 列表。 |

---

## License

[MIT](./LICENSE)——全文见 [`LICENSE`](./LICENSE)。
