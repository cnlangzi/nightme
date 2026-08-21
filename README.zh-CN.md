# NightMe

<p align="center">
  <img src="./logo.png" alt="NightMe Logo" width="150">
</p>

> 安享好梦，NightMe 彻夜向前。
>
> 奔赴你的星辰大海，拥有你的自由生活。而那些必须死守电脑、避无可避的无奈，就让 NightMe 替你守候。

[English](./README.md) · **简体中文**

![Release](https://img.shields.io/github/v/release/cnlangzi/nightme)
![CI](https://github.com/cnlangzi/nightme/actions/workflows/ci.yml/badge.svg)
![License](https://img.shields.io/badge/license-MIT-green)
![Go Reference](https://pkg.go.dev/badge/github.com/cnlangzi/nightme.svg)

![Platforms](https://img.shields.io/badge/platforms-macOS%20%7C%20Linux%20%7C%20Windows-lightgrey)
![Single Binary](https://img.shields.io/badge/distribution-single%20binary-success)
![GitHub stars](https://img.shields.io/github/stars/cnlangzi/nightme?style=social)

## What is NightMe

**NightMe** 把你的本地 AI Coding Agent（Claude Code、Codex、DeepSeek Harness (DSH)、Pi、OpenCode 等）放进聊天里跑。你在任何已接入的 IM 里发条消息，NightMe 就把消息路由到对应的 agent 进程，回复以结构化卡片形式返回。

多个 chat 并行——一个项目一个，目录就是项目本身：每个 ChatSession 跑在自己的工作目录上，目录即项目本体。多个 Agent 并行工作——切换是即时的，无需冷启动。`git` worktree 操作被封装进 `Git Team Workflow`（`/gtw`）：fix / hooks / close——每一步一张 IM 回复卡，集成 GitHub、GitLab 之类平台。NightMe 不替换你的 agent 订阅或记忆，只在它们前面做一个轻量代理。

## Why NightMe

### 一个 chat，一个 CWD，一个项目

作为独立开发者，你一个人同时管很多项目。每个飞书群组或 DM 是一个 **ChatSession**，每个 ChatSession 有自己的 **CWD**（当前工作目录）。CWD *就是* 项目：用 `/cwd <path>` 设置，随时可以改。多个 chat 并行跑，各自绑定各自的目录。

```
                            You (Feishu)
                                  │
                                  ▼

    ┌─ ChatSession ─┐  ┌─ ChatSession ─┐  ┌─ ChatSession ─┐
    │ CWD: ~/a      │  │ CWD: ~/b      │  │ CWD: ~/c      │
    │               │  │               │  │               │
    │ AI Agents:    │  │ AI Agents:    │  │ AI Agents:    │
    │   Claude Code │  │   Claude Code │  │   Claude Code │
    │   Codex       │  │   Codex       │  │   Codex       │
    │   Pi          │  │   Pi          │  │   Pi          │
    │   OpenCode    │  │   OpenCode    │  │   OpenCode    │
    │   DSH         │  │   DSH         │  │   DSH         │
    └───────────────┘  └───────────────┘  └───────────────┘

   ▲ CWD = 项目；agent 在该 CWD 里跑；全部从同一个 NightMe 实例并行 ▲
```

![飞书聊天列表——多个并行 ChatSession（DM + 群组），每个钉在各自项目上](docs/images/feishu-multi-chats.png)

**项目之间靠目录隔开。** 每个 ChatSession 的 CWD 互不影响——重跑 `/cwd` 只改当前 session 的工作目录。多个项目同时跑着，互不打扰。

**与传统工具的差异**（Hermes、openclaw、cc-connect、happycoder）：他们一次只激活一个 session。NightMe 在一个实例上同时跑所有项目。切换 chat 是即时的——同一个 daemon，无需冷启动，无需重新初始化。

### 三项核心能力

| 能力 | 实际意义 |
|---|---|
| **并行多 ChatSession** | 一台机器 N 个 session，各自跑不同项目或任务。 |
| **CWD = 项目** | 每个 ChatSession 绑定一个当前工作目录——目录 *就是* 项目。用 `/cwd <path>` 设置；随时切换。 |
| **同 Chat 多 agent** | `/use <agent>` 切换当前 agent。前一个 agent 切到后台继续跑——任务推进、结果照常回，只是新消息不再路由给它。 |

## Prerequisites

- **macOS、Linux 或 Windows** — NightMe 是单文件静态 Go 二进制；无运行时依赖。
- **一个 Feishu 账号** — 目前唯一支持的 IM。`nightme login feishu` 通过扫码完成 bot 注册。
- **至少一个本地 AI Coding Agent** — Claude Code、Pi、OpenCode、Codex、DeepSeek Harness (DSH) 任一。装好 CLI 放到 `$PATH` 上，NightMe 会作为子进程拉起。

## Install

三种方式装 `nightme`：

1. **One-liner**（推荐）：

   **macOS / Linux：**
   ```bash
   curl -fsSL https://nightme.dev/install.sh | bash
   ```

   **Windows（PowerShell）：**
   ```powershell
   powershell -c "irm https://nightme.dev/install.ps1 | iex"
   ```

   会把最新版 release 装到 `$PATH` 上的稳定路径，并跑 `nightme version`
   验证。

2. **预编译二进制**（手动）：

   - 去 [latest release page](https://github.com/cnlangzi/nightme/releases/latest)
     下载对应平台的压缩包（如 `nightme_<version>_darwin_amd64.tar.gz`、
     `nightme_<version>_linux_amd64.tar.gz`、
     `nightme_<version>_windows_amd64.zip`）
   - 解压后里面的二进制就叫 `nightme`（Windows 是 `nightme.exe`），
     放到 `$PATH` 上并加可执行权限：
     ```bash
     tar -xzf nightme_<version>_darwin_amd64.tar.gz
     mv nightme /usr/local/bin/nightme
     chmod +x /usr/local/bin/nightme
     ```
     Windows 上解压后把 `nightme.exe` 放到 `PATH` 上的某个目录即可。

3. **从源码**（开发或钉死一个 commit）：

   ```bash
   git clone https://github.com/cnlangzi/nightme.git
   cd nightme
   make dev
   ```

   `make dev` 直接从源码跑 nightme，配置文件用 example config，无需单独构建。

---

## Quickstart

```bash
nightme login feishu   # 终端打印二维码；用 Feishu 移动端扫码
nightme start          # daemon 在后台跑起来
```

`start` 返回后，NightMe 会给你的 Feishu DM 发一条 welcome message——这就是它已经 ready 的信号。

### CLI commands

大多数时候你都在 chat 里。需要回终端的只有这几条：

| 命令 | 作用 |
|---|---|
| `nightme start` / `stop` / `restart` | 开关 NightMe。开也好关也好，你的 agent 照常干活。 |
| `nightme status` | NightMe 在跑吗？ |
| `nightme list` | 列出你所有的 agent：在哪个 chat、哪个项目、还活着还是已结束。 |
| `nightme kill` | 一次性停掉所有 agent。在 chat 里发条消息就回来了，对话不丢。 |
| `nightme logs` | 实时看 NightMe 在干什么。 |
| `nightme doctor` | 觉得哪里不对时，看一眼 NightMe 是否健康。 |
| `nightme agents` | 你配了哪些 AI agent。 |

「停」分三个范围：`/close`（单个项目）· `nightme kill`（所有 agent）· `nightme stop`（NightMe 自己）。三种都不会丢对话。

---

## Slash commands

Chat 级别的斜杠命令。`/gtw` 子命令见 [它们自己的 section](#git-team-workflow-gtw)，这里不列。

| 命令 | 干什么 |
|---|---|
| `/cwd <path>` | 绑定这个 Chat 到一个工作区。会校验路径，下次发消息时 lazy-spawn。 |
| `/use <agent>` | 切换当前 agent（`claude` / `codex` / `dsh` / `opencode` / `pi`）。前一个 agent 切到后台继续跑——任务推进、结果照常回，只是新消息不再路由给它。 |
| `/stop` | 停掉当前 agent 上的 in-flight turn。会话留着，队列里的消息继续流。 |
| `/steer <msg>` | 停掉 in-flight turn 并把 `<msg>` 插到队首。下个 turn agent 第一眼看到的就是这条。 |
| `/close [agent]` | 终止当前工作区里 AgentSession 的 bridge 进程。AgentSession 记录保留——下次发消息触发 respawn 时会用 `--resume <sessionID>` 接着聊。 |
| `/new [agent]` | 重置 agent 对话上下文（Claude Code 的 `/clear` 等价）。进程保留，队列清空。 |
| `/watch on\|off` | 当前 Chat 的消息监听模式（默认群内只听 `@bot` / `@_all`）。 |
| `/think on\|off` | 是否在回复卡里展示 agent 思考过程。 |
| `/tools on\|off` | 是否展示每个工具的独立线程回复（默认关）。 |
| `/help` | 在 Chat 里列出所有斜杠命令。 |

`!cmd` 在当前 Chat 的 CWD 里直接跑 shell 命令——规则见 [Shell mode](#shell-mode)。

任何不匹配斜杠命令（或 `!cmd`）的消息，都会被透传到底层 agent 当作普通 prompt——跟你直接在 Claude Code 自己的 CLI 里发消息一样。NightMe 不拦截也不改写；agent 收到原样消息，会运行它自带的斜杠命令（例如 Claude Code 的 `/clear` / `/compact` / `/init` 等）。

---

## Always-in-the-loop

你随时知道 agent 在哪、干到哪、烧了多少钱——每条回复都带一张固定页脚，写清楚关键信息，不用离开 chat。多数「AI 写代码 + 聊天」工具都跟黑盒一样；NightMe 把可视化当成一等公民。

### StatusBar——钉在每条 Feishu 回复上

![飞书页脚卡——CWD / git / agent / token，每条回复都带](docs/images/feishu-statusbar.png)

每条回复都带一张固定页脚，写清楚关键信息：

- **CWD** — 当前 ChatSession 处于哪个目录（也就是项目）
- **Git status** — 分支、是否 dirty、ahead / behind
- **Agent status** — `idle` / `running` / `thinking`
- **Token usage** — 当前 session 已用 / 上限

别的工具把你丢进黑盒。NightMe 让你随时看到 agent 在哪、干到哪、花了多少。

### Flexible visibility

| 开关 | 作用 |
|---|---|
| `/think on\|off` | 是否展示 agent 的思考过程。 |
| `/tools on\|off` | 是否展示每个工具的独立线程回复（默认关）。 |
| `/watch on\|off` | 监听群内所有消息，不再只听 `@bot` / `@_all`。 |

**为什么这很重要：** NightMe 默认把状态摊开，想安静的时候随时切回静音——这是你的选择，不会突然跳出来。

---

## What we do differently


| 特性 | openclaw / Hermes | NightMe |
|---|---|---|
| Sessions survive daemon restart | ❌ | ✅ |
| 真的 `/stop` 和 `/steer` | ❌ | ✅ |
| 没有 server-side timeout | 30 min | none |
| Clean prompts, no preamble | ❌ | ✅ |

四点不同，简而言之：

1. **Sessions survive。** Daemon 重启、网络抖动、合盖休眠——聊天从你断的地方接住。上游 CLI 的 session 通过 `--resume <session-id>` 恢复。

2. **真的 escape hatches。** `/stop` 停掉 in-flight turn。`/steer <msg>` 改方向。两者都保留 session 和上下文。

3. **No clock on you。** Claude 跑 30 分钟，NightMe 等 30 分钟。你决定什么时候停。

4. **No prompt padding。** 没有 preamble、没有 brand voice、没有 injected system message。CLI 只看到你写的话。

We sit in front of Claude / Codex / DSH (DeepSeek Harness) / Pi / OpenCode. You stay in control. Nothing in a black box.

---

## Shell mode

你并不总是需要 agent 跑 shell 命令。用 Claude Code / Codex 时，让 agent 跑命令要绕它的 tool loop——链路长，context 也被消耗，agent 忙着读 shell 输出，你正事反而被挤到一边。

`!cmd` 跳过这一切。在 chat 里敲 `!make test`，NightMe 直接在 chat 的 CWD 里跑命令。回复是一张简洁的 IM 卡。No agent, no round trip, no context eaten。

项目里那些本来就有的脚本——`make`、`npm test`、deploy hook——谁 run 都一样。agent 在中间想一遍反而浪费时间。

```
✅ $ make test
exit 0 · 12ms · ~/work/foo
stdout:
  All tests passed
```

---

## Git Team Workflow (`/gtw`)

`git worktree` 给你按任务隔离的分支。`gh pr create` 给你一次性 PR。AI agent 给你随叫随到的编码帮手。`/gtw` 把这三者粘起来——每个 `/gtw <cmd>` 是一个斜杠命令，**起一个 short-lived agent** 干重活，返回一张干净的 IM 卡。agent 跑一次就退出。主 chat 保持干净。

GitHub / GitLab issues 是任务流——每次 `/gtw fix` 钉到一个 issue，工作随着 subcommand 推进在 issue 的 state 里流动。

### The local dev loop: fix → hooks → close

三个 subcommand 串起来就是一个完整的 **local multi-branch development workflow**。3 个可以并行跑——三个 issue、三个 worktree、三个 agent，state 不打架。

> **`/gtw sync` 不在这个 loop 里。** `sync`（`git checkout main && git pull --rebase origin main`）是 **main-repo 操作**——它切当前分支到 main 并拉。从 worktree 里调 sync 是错的，代码层就 refuse。**`/gtw fix`**（第 1 步）和 **`/gtw close`**（最后一步）都已经在 main repo 上自动 sync 了，所以你不需要手动 sync。`close` 之后 main 是新的，下次 `fix` 直接基于它。

1. **`/gtw fix -n <branch>`** — 在刚刚最新的 main 上开一个新 worktree（叫 `<branch>`），起一个 one-shot agent 干任务。纯本地——不需要 GitHub issue。你在主 chat 继续聊。

   走 GitHub / GitLab 流程的话，用 `/gtw fix <issue-id>` 把 worktree 钉到远程 issue。第一次跑什么都不用配，开箱即用。

2. **hooks 自动跑** — 新 worktree 里开发环境自己重新装起来。CodeGraph 重新索引，`npm install` / `go mod download` / `cargo build`——你项目要啥就装啥。编辑 `~/.nightme/gtw.yml`：

   ```yaml
   # ~/.nightme/gtw.yml
   fix:
     hooks:
       after:
         - codegraph init                # bare string = shell hook
         - npm install
         - go mod download
   ```

3. （你干活。需要时让 agent 出手，或者直接自己改文件。）

4. **`/gtw close`** — 任务做完了（或者决定不做了），`/gtw close` 拆 worktree、回到 main，分支 ready to ship（或者 discard）。

### Hooks——把开发环境一起带过去

AI 工具的索引（CodeGraph、语言服务器、缓存）一般放在仓库里。每个 worktree 是新 checkout——都要重建。Hooks 把这件事自动化了。

最常见的是 `fix: hooks: after`——在 `/gtw fix` 开新 worktree 之后立刻跑，让新 worktree 的 dev env 就地装起来：

```yaml
# ~/.nightme/gtw.yml
fix:
  hooks:
    after:
      - codegraph init                # 重新索引新 worktree
      - npm install                   # 装依赖
      - go mod download               # 下载 Go modules
```

每个命令（不只是 `fix`）都有 `hooks.before` 和 `hooks.after`：

| Hook | 触发时机 | 典型用途 |
|---|---|---|
| `before` | 主流程之前 | 记录起始 SHA、拍状态快照 |
| `after` | 主流程之后（即使失败也跑） | 重新索引、装依赖、暖缓存 |

铁律（来自代码）：

- v1 只支持 **shell hook**——其他类型会告警并跳过。
- Hook 失败 **绝不阻塞主流程**。挂了就在回复里挂个 `⚠️` 提示，主命令继续。
- stdout / stderr 全部回显，能看到实际跑过什么。
- 单个 hook 默认 30s 超时。

---

## For developers

### Architecture (advanced)

```
┌─────────────┐    ┌─────────────┐    ┌──────────────────────────┐
│  Channel    │ →  │  Gateway    │ →  │  ChatSession (per chat)   │
│  (Feishu,   │ ←  │  (router +  │ ←  │  ├─ AgentSession pool     │
│   Web TUI)  │    │   binding)  │    │  │  (agent, cwd) 1:1       │
└─────────────┘    └─────────────┘    │  ├─ InputBuffer FSM       │
                                     │  ├─ readPump              │
                                     │  └─ EventHandler          │
                                     │           ↓               │
                                     │  AgentSession → Bridge    │
                                     │     (PTY / ACP / SDK /    │
                                     │      JSON-IO / RPC)       │
                                     │           ↓               │
                                     │       Agent CLI           │
                                     └──────────────────────────┘
```

- **Channel** 拥有 transport。
- **Gateway** 路由入站。`inbound` 子包负责斜杠命令派发链；其它都转发到 ChatSession 的 active AgentSession。
- **ChatSession** 是每个 chat 的 context。拥有 AgentSession 池和 InputBuffer FSM。daemon 重启之间持久化。
- **AgentSession** 是每个 CLI 进程的句柄。每个 `(agent, cwd)` 一份，`/use` 和 `/cwd` 切换之间保活。
- **Bridge** 是每个 agent 的 transport——`acp`、`claudecode`、`codex`、`dsh`、`opencode`、`pi` 或 `pty`（在 `internal/bridge/` 下），按 CLI 支持情况选。

完整责任表见 [`docs/SPEC.md`](./docs/SPEC.md) §1，"Channel 是 dumb renderer" 的设计动机见 §0.1。

### Configuration

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
  - name: dsh
    bridge: dsh
    command: dsh

feishu:
  app_id: "cli_xxxxxxxxxxxxxxxx"
  app_secret: "xxxxxx…xxxx"
  verification_token: ""
  encrypt_key: ""

session:                                 # 初始 PTY + aggregator 参数
  default_pty_cols: 80
  default_pty_rows: 24
  output_chunk_size: 4096        # bytes
  output_flush_interval_ms: 200  # milliseconds

logging:
  level: "info"          # debug | info | warn | error
  file: ""               # 空 = stdout；非空 = 文件路径

paths:
  data_dir: "~/.nightme"  # chat_sessions.json + agent_sessions.json 的根
```

`/gtw` 工作流读的是 **另一个文件**：`~/.nightme/gtw.yml`——见上文的 [Hooks 那一节](#hooks把开发环境一起带过去)。

完整 schema 和每个 bridge 的说明见 [`configs/nightme.example.yaml`](./configs/nightme.example.yaml)。

日志写到 `~/.nightme/nightme.log`（权限 `0600`），JSON 格式。属性名里含 `secret`、`token`、`password` 的会被自动改成 `***REDACTED***`。

### Documentation

| 文档 | 内容 |
|---|---|
| [`docs/PRD.md`](./docs/PRD.md) | 产品定义——做什么、为什么做、为谁做。不讲技术。 |
| [`docs/SPEC.md`](./docs/SPEC.md) | 技术架构——组件、数据流、NFR。 |
| [`docs/FEATURES.md`](./docs/FEATURES.md) | 功能索引——每个 F-XX 一行。 |
| [`docs/WFE.md`](./docs/WFE.md) | Workflow YAML + 引擎运行时架构——触发器、步骤、bot↔wfe 边界。 |
| [`docs/feat/`](./docs/feat/) | 每个 feature 的设计文档。 |
| [`docs/bridge/`](./docs/bridge/) | 每个 agent bridge 的设计：claude、codex、dsh、opencode、pi。 |
| [`docs/channel/feishu.md`](./docs/channel/feishu.md) | 飞书 adapter 参考（渲染规则、卡片语义、线程路由）。 |
| [`docs/flow/`](./docs/flow/) | 横切流程文档（如 3-layer doc model）。 |
| [`docs/E2E_TESTING.md`](./docs/E2E_TESTING.md) | 飞书端到端手动测试 + 排错。 |
| [`CHANGELOG.md`](./CHANGELOG.md) | 当前 snapshot（单 `[Unreleased]` 段）。 |
| [`MIGRATION.md`](./MIGRATION.md) | 历史 snapshot 之间的 breaking change 列表。 |

### Development

```bash
make build     # ./bin/nightme，带 version metadata
make test      # go test -race ./...   (~20 packages, race-tested)
make lint      # go vet ./...          (0 warnings required by CI)
make install   # go install to $GOBIN
make dev       # go run ./cmd/nightme  (uses example config)
```

CI 跑在 GitHub Actions（`.github/workflows/ci.yml`），每次 push 和 PR 都会跑：`go vet`、`go test -race`、`go build` 必须全过。

### Project layout

```
cmd/nightme/                       # cobra CLI (start / stop / restart / status / logs / doctor / test / config / list / login / agents / name)
configs/                           # example YAML config
docs/
  PRD.md SPEC.md FEATURES.md       # 3-layer doc model
  WFE.md                           # workflow YAML + 引擎运行时（schema / 触发器 / 步骤 / bot↔wfe 边界）
  feat/                            # F-XX per-feature design
  bridge/  channel/  flow/         # per-subsystem design
  images/                          # README-served screenshots
internal/
  agent/                           # Agent / AgentEvent / Info / Starter interface
  agentsession/                    # AgentSession + Prompt + Spawner (per-CLI-process runtime unit)
  bridge/                          # Bridge abstraction, one sub-package per agent
    acp/  claudecode/  codex/  dsh/  opencode/  pi/  pty/
  channel/                         # Channel interface
    echo/  feishu/                 # adapters (Feishu is the production one)
  chatsession/                     # ChatSession + pool manager + persistence
  cli/                              # shared CLI helpers (config / doctor / login)
  command/                         # Slash-command Commander / Registry / Factory
    cwd/ close/ newcmd/ use/ think/ tools/ watch/ stop/ steer/ services/
    gtw/                           # /gtw fix / hooks / close (worktree workflow)
  config/                          # YAML loader + env overrides
  daemoncontrol/                   # IPC for `nightme doctor` / `status`
  errors/                          # CodedError + ExitCode
  gateway/                         # Slash router + binding + receipt FSM
    inbound/  outbound/            # inbound dispatch chain + outbound sender
  gatewaytest/                     # integration test harness
  logging/                         # slog + secret redaction
  login/                           # Feishu app registration / QR login
  messages/                        # IM message types + dispatch
  prcache/                         # PR metadata cache (per-F-50)
  registry/                        # JSON-backed chat_sessions.json + agent_sessions.json (0600, atomic)
  shell/                           # `!cmd` shell-mode dispatcher
  statusbar/                       # Feishu footer-card stamp runtime (per F-58, F-133)
  testdata/                        # shared test fixtures
  version/                         # build-time version metadata
```

### Exit codes

| Code | 含义 |
|------|---------|
| 0 | Success |
| 1 | Generic / unmapped error |
| 2 | Config error |
| 3 | Auth error |
| 4 | Channel error |
| 5 | Session error |
| 6 | Agent error |
| 7 | Bridge error |
| 8 | Validation error |
| 9 | Not found |

---

## Contributing

PRs 和 issues 都欢迎。大改动的话先开 issue 聊聊，再写代码。

详细指南在 [`/docs`](./docs/) — 设计流程参考 [3-layer doc model](./docs/README.md)。

感谢使用 NightMe——我们欢迎更多 **channels**（Feishu、Web TUI、其他）和更多 **AI Coding Agents**（Claude Code、Codex、DeepSeek Harness (DSH)、Pi、OpenCode、任何新的）接入。Drop 一个 `Channel` / `Bridge`，架构处理剩下的事。

联系维护者：

- Twitter：[@imlangzi](https://x.com/imlangzi)
- 微信：`langzi`（加好友请注明 "NightMe"）

---

## License

[MIT](./LICENSE)——全文见 [`LICENSE`](./LICENSE)。
