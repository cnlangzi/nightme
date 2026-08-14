# F-dsh — DeepSeek Harness (dsh) print-mode Bridge

> **Status**: 方案定稿,待实施
> **Scope**: `internal/bridge/dsh/` — dsh 作为 nightme 的 RunOnce agent(`/gtw commit`、`/gtw pr` 等)
> **方向(已锁定)**: `dsh --profile headless` CLI 直接调用,plain stdout,**完全不改 dsh 本机默认配置**
> **核心原则(用户 2026-08-14 锁定)**: 接入底层 AI agent,**不修改 agent 本地默认配置**;nightme 只管 transport + permissions(权限默认全开)
> **姊妹文档**:
> - [F-CODEX-PRINT-001](./F-CODEX-PRINT-001.md) — 同形态:codex `exec` print-mode,本方案模板
> - [F-CLAUDE-PRINT-001](./F-CLAUDE-PRINT-001.md) — claude `-p` print-mode
> - [F-PI-PRINT-001](./F-PI-PRINT-001.md) — pi `--print` print-mode
> - [bridge/cli-transport.md](../bridge/cli-transport.md) — pipe / lifecycle 通用约束

---

## 1. 为什么是 print-mode,不是 chat-session bridge

### 1.1 实机环境约束

| dsh 入口 | 状态 | 多 turn? | 备注 |
|---------|------|---------|------|
| `dsh --profile headless` (npm `@deepseek-ai/dsh`) | ✅ 实机已装 + 实测 `PONG` 通 | ❌ **不支持 --resume**,headless profile 写死 "Answer one task, print the final assistant message, and exit" | 实测命令:`echo PONG \| dsh --profile headless -- "Reply with the single word PONG"` |
| `dsh --profile web` (HTTP :3080) | ✅ 可起,但 API 是浏览器用 | ✅ 理论上 | 需逆向 `/api` 端点,不现实 |
| `dsh-jsonrpc-agent-pkg` (pip 单文件) | ❌ 未装 | ✅ | 需 `pip install deepseek-harness-runtime-bin` |
| `pnpm run demo:acp` (ACP,需 dsh 源码仓) | ❌ 没 clone 仓 | ✅ | npm `@deepseek-ai/dsh` 不含 demo:acp 脚本 |

**实机只有 npm CLI 一条路**。headless 不支持多 turn,所以本方案仅做 **RunOnce(一次性)**;chat session 多 turn 需要 dsh-jsonrpc-agent-pkg,**单独 PR 后续做**。

### 1.2 已对齐的"无修改"原则

本方案**严格遵循**用户锁定的接入原则(见 `~/.claude/projects/.../agent-no-config-tampering.md`):

| 维度 | nightme 注入 | 走 dsh 自己默认 |
|------|--------------|-----------------|
| Model | ❌ **不注入** | `~/.dsh/settings.yaml` 的 `agent-default-model.model`(实测 `MiniMax-M3`) |
| Provider | ❌ **不注入** | `~/.dsh/settings.yaml` 的 `agent-default-model.provider`(实测 `minimax-cn`) |
| Credentials | ❌ **不注入** | `~/.dsh/.credentials.yaml` 或 env(如 `MINIMAX_CN_API_KEY`) |
| Workspace | ✅ **`cmd.Dir = cfg.Workspace`** | (运行时上下文,不是配置) |
| Permissions | ✅ **`DSH_PERMISSION_MODE=danger-full-access`** | (用户明确「权限默认全开」) |
| Transport | ✅ **`--profile headless` + positional `<prompt>`** | (这是 transport,不是配置) |

**nightme 不 bundled cordis.yml**、不读 `~/.dsh/settings.yaml`、不读 `~/.dsh/.credentials.yaml`、不传 `DEEPSEEK_API_KEY` / `DSH_MODEL` / `DEEPSEEK_BASE_URL`。

### 1.3 与 codex print-mode 形态对齐

| | codex (`F-CODEX-PRINT-001`) | **dsh(本方案)** |
|---|---|---|
| chat 后端 | `codex app-server --listen stdio://` JSON-RPC(长生命周期) | ❌ **本方案不做**(headless 不支持) |
| print-mode 命令 | `codex exec --json -o <tmpfile>` | `dsh --profile headless -- "<prompt>"` |
| RunOnce 实现 | spawn → wait → read tmpfile → return | spawn → wait → read stdout → return |
| Workaround | `--` 分隔 flag 与 positional prompt | `--` 同 |
| 多模态图片 | `-i` flag | (本期不支持;dsh headless 不暴露 image flag) |
| Workspace | `cmd.Dir` | `cmd.Dir = cfg.Workspace`(同) |
| Permission | `approval_policy="never" + sandbox_mode="danger-full-access"` | `DSH_PERMISSION_MODE=danger-full-access` |

---

## 2. Wire & 接口

### 2.1 spawn 配方

```go
// internal/bridge/dsh/starter.go
cmd := exec.CommandContext(ctx, "dsh", "--profile", "headless", "--", prompt)
cmd.Dir = cfg.Workspace
cmd.Env = append(os.Environ(),
    "DSH_PERMISSION_MODE=danger-full-access",  // ← 唯一注入:权限放开
    // 其他全走 dsh 本机默认
)
```

**关键不变量**:
- `cmd.Dir = cfg.Workspace` — workspace 单一来源,dsh 内部 `process.cwd()` 自动命中
- 只增 1 个 env(`DSH_PERMISSION_MODE`),其他完全不动
- argv 用 `--` 分隔,避免 dsh 把 prompt 当 stdin

### 2.2 exit / 输出契约

| 退出码 | 含义 | RunResult |
|--------|------|-----------|
| 0 | 成功 | `Text = stdout`,其他字段零值 |
| 非 0 | 失败 | `(RunResult{}, err)`,stderr 进 error message |
| cmd 启动失败 | `dsh` 不在 PATH | `err = exec.ErrNotFound` |

**输出**:headless 把最终 assistant message **写到 stdout**(per `examples/headless-agent/README.md` 「prints the final assistant text, and exits」)。所以直接 `cmd.Output()` 拿到完整回复。

### 2.3 RunResult 字段映射

| RunResult 字段 | 来源 | 备注 |
|----------------|------|------|
| `Text` | `cmd.Output()` bytes | plain text,**不含** user/codex/dsh 标记、无 tool_call progress |
| `Usage` | 零值(nil) | dsh headless 不暴露 usage |
| `Model` | 零值("") | 不知道 dsh 实际用了哪个 model(provider 解析在 dsh 侧) |
| `SessionID` | 零值("") | dsh headless 不暴露 sessionId |
| `DurationMs` | wall-clock 测 | cmd 退出时间戳 |
| `Subtype` | exit code:0=`"completed"`,非零=`"failed"` | 与 codex exec 语义一致 |

**取舍**:本方案拿不到 usage/model/sessionId;若 `/gtw commit` / `/gtw pr` 后续需要 audit cost,需要 chat-session bridge(JSON-RPC 那条线)补,这是 print-mode 的固有限制。

### 2.4 starter 接口

```go
// internal/bridge/dsh/starter.go
type Starter struct {
    name        string  // "dsh"
    path        string  // exec.LookPath("dsh") 结果;空则 lazy LookPath
}

func NewStarter(name string) *Starter {
    lp, _ := exec.LookPath("dsh")
    return &Starter{name: name, path: lp}
}

func (s *Starter) Info() agent.Info {
    cmd := "dsh"
    args := []string{"--profile", "headless"}
    return agent.NewInfo(s.name, agent.ModeJSONIO, cmd, args, nil)
}

func (s *Starter) Detect() error {
    if _, err := s.lookPath(); err != nil {
        return fmt.Errorf("dsh: not found in PATH. Install via `npm install -g @deepseek-ai/dsh`")
    }
    return nil
}

func (s *Starter) lookPath() (string, error) {
    if s.path != "" { return s.path, nil }
    lp, err := exec.LookPath("dsh")
    if err != nil { return "", err }
    s.path = lp
    return lp, nil
}

// Start 不实现(chat session 走 JSON-RPC,后续 PR)
// 本期只做 RunOnce;Start() 返 ErrNotSupported,要求调用方用 RunOnceDrain。
// *(F-RUNONCEDRAIN-INTERNAL 后 acp 路径已改为 `(*Starter).collectResult`,
//   dsh 走 print-mode 所以不涉及。)
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
    return nil, errors.New("dsh: chat session not implemented (RunOnce only)")
}

func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
    a, err := s.startEphemeral(ctx, cfg, blocks)
    if err != nil { return agent.RunResult{}, err }
    defer a.Close()
    return agent.RunOnceDrain(ctx, a, blocks, s.Info().Name)
// *(F-RUNONCEDRAIN-INTERNAL 后已删除,acp 改用 `(*Starter).collectResult`)*
}

// startEphemeral spawn 一个临时 driver,只为 drain 一次 turn。
// 因为 chat session 不支持,每次 RunOnce 都重新 spawn + close。
func (s *Starter) startEphemeral(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (*agent.Agent, error) {
    d := &driver{
        ctx: ctx, cancel: func(){},
        workspace: cfg.Workspace,
        events:    make(chan agent.AgentEvent, 64),
    }
    cmdPath, err := s.lookPath()
    if err != nil { return nil, err }
    d.cmd = exec.CommandContext(ctx, cmdPath, "--profile", "headless", "--", agent.BlocksToPrompt(blocks))
    d.cmd.Dir = cfg.Workspace
    d.cmd.Env = append(os.Environ(), "DSH_PERMISSION_MODE=danger-full-access")
    // ... wait cmd.Output() 异步 → deliver to events chan → close
    return agent.NewAgent(s.Info(), d.cmd.Process.Pid, d.events, d), nil
}
```

(详细实现 ~200 行,镜像 `internal/bridge/codex/print.go` 的 tmpfile/drain 模式,只是 dsh 没 `-o <tmpfile>` 这种 clean-output 机制,所以直接读 stdout。)

---

## 3. 与现有不变式的兼容性

### 3.1 抽象层不变
- **§1.3 / §1.4**:dsh 特有 wire 字段(`DSH_PERMISSION_MODE` env、`--profile headless` flag)在 bridge 边界 normalize 成 `Starter.RunOnce` 通用契约,运行时只见 `agent.RunResult`
- **§1.4 "无 Meta" 约束**:dsh 没新增抽象字段

### 3.2 AgentSpec / Info 字段
dsh 不暴露 model / credentials 进 `Info`,完全符合 memory 原则 (`agent-no-config-tampering`)。

### 3.3 持久化
- daemon 重启后 dsh AgentSession 仍是 `Detached`(本方案无 chat session)
- `cfg.SessionID` 不透传(headless 不支持 --resume)
- `/use dsh` 在 chat session 路径里**直接报错**「dsh supports RunOnce only, use via /gtw commit」

---

## 4. 测试金字塔

### 4.1 单元(不需真 dsh)

| 测试 | 锁 |
|------|----|
| `TestNewStarter_Info` | `Name()=="dsh"`,`Mode()==ModeJSONIO`,`Command()=="dsh"`,`Args()==["--profile","headless"]` |
| `TestDetect_NoDsh` | PATH 临时清掉 `dsh` → 错 "install via npm" |
| `TestDetect_OK` | PATH 含 `dsh` → nil |
| `TestBlocksToPrompt` | `ContentText{Text:"hi"}+ContentImage{Path:"/tmp/x.png"}` → `"hi\n[image: /tmp/x.png]"`(text + bracketed 注解,无 image native) |
| `TestBuildArgs` | argv == `["--profile","headless","--","hi there"]`,env 含 `DSH_PERMISSION_MODE=danger-full-access` |
| `TestRunOnce_PlainStdout` | fake shell 回 `"hello\n"` → `RunResult.Text == "hello"`,exit 0 → `Subtype == "completed"` |
| `TestRunOnce_NonZeroExit` | fake shell exit 1 + stderr `"boom"` → `err` 含 "boom",`RunResult == zero` |
| `TestRunOnce_EmptyStdout` | fake shell 回 `""` → `RunResult.Text == ""`,**不报错**(headless 可能空 stdout) |
| `TestStart_NotImplemented` | `Starter.Start(...)` → 返错 "chat session not implemented" |

### 4.2 Real CLI e2e(`NIGHTME_REAL_DSH=1`,本机 `dsh` 在 PATH)

| 测试 | 锁 |
|------|----|
| `TestE2E_PongSmoke` | `Starter.RunOnce(ctx, cfg, blocks{text:"Reply with PONG"})` → `Text=="PONG"`,DurationMs 合理(<30s),Subtype=="completed" |
| `TestE2E_RealWorkspace` | `cfg.Workspace = t.TempDir()` → dsh 在该 cwd 跑(从 stderr 或 usage 推断) |
| `TestE2E_PermissionEnvPropagates` | inspect `os.Getpid()` 进程组能看到 `DSH_PERMISSION_MODE=danger-full-access` 被传给 child(用 `os.Environ` 比对) |
| `TestE2E_NpmBinAbsent` | `unset PATH; PATH=/nonexistent` + `Starter.Detect()` → 错 "install via npm install -g @deepseek-ai/dsh" |

### 4.3 持久化 + 全库回归

- daemon 重启不影响 dsh(`Starter.RunOnce` 无状态,无 AgentSession 持久化)
- `/use dsh` 在 chat 入口报错(清晰信息,不静默失败)
- `/gtw commit` 走 `Starter.RunOnce` → 拿到 commit message → `/gtw pr` 走 `Starter.RunOnce` → 拿到 PR body

```bash
go test ./internal/bridge/dsh/ -race
NIGHTME_REAL_DSH=1 go test ./internal/bridge/dsh/ -run Real -v
go test ./... -race
```

---

## 5. 包结构(~200 行,~1 天工作量)

```
internal/bridge/dsh/
  ├── doc.go                 # package doc(引用本文件 F-dsh-bridge.md)
  ├── starter.go             # Starter + NewStarter + Info + Detect
  ├── print.go               # RunOnce 实现(spawn + drain stdout)
  ├── prompt.go              # *(F-RUNONCEDRAIN-INTERNAL 后删除,使用 agent.BlocksToPrompt)*
  ├── starter_test.go        # 单测覆盖 Detect / Info / BuildArgs
  └── print_real_unix_test.go # NIGHTME_REAL_DSH=1 e2e(PongSmoke)
```

**注册**(`cmd/nightme/agents.go`,1 行):
```go
agent.Builtins.Register(dsh.NewStarter("dsh"))
```

**`nightme agents` 输出多一行**:
```
NAME    BRIDGE   COMMAND
cc      json-io  claude --dangerously-skip-permissions
codex   json-io  codex
dsh     json-io  dsh --profile headless      ← 新增
opencode opencode opencode
pi      json-io  pi
bash    pty      bash
```

**`config.yaml` 不需要新字段**:不读 model/provider/key,不写 dsh 段,完全零配置。

---

## 6. 验收清单(Definition of Done)

- [ ] `internal/bridge/dsh/` 200 行落地,`cmd/nightme/agents.go` 注册 1 行
- [ ] 单测 ≥8 个 + e2e ≥3 个 全绿
- [ ] 实机 smoke:`Starter.RunOnce(...)` 跑 `Reply with PONG` → `Text=="PONG"`
- [ ] `nightme agents` 表格多 `dsh` 行
- [ ] `cmd/nightme/agents_test.go` 列表断言加 `dsh`
- [ ] `/use dsh` 在 chat session 入口返清晰错(不静默失败)
- [ ] `/gtw commit` 走 dsh `RunOnce`,成功生成 commit message(本机 dsh 实测)
- [ ] 文档:本文件落地,`docs/FEATURES.md` 加索引
- [ ] **没有** `configs/dsh/cordis.yml`、**没有** `~/.nightme/config.yaml` 的 `dsh:` 段(原则验证)

---

## 7. 不在范围(明确 deferred,单独 PR)

| 项 | 理由 | 何时做 |
|----|------|--------|
| dsh chat session bridge(多 turn) | 实机无 `dsh-jsonrpc-agent-pkg` 或 ACP server,本方案只做 RunOnce | 用户 `pip install deepseek-harness-runtime-bin` 后单独 PR;~7 天工作量 |
| dsh multi-modal 图片(`ContentImage` 原生投递) | dsh headless profile 当前不暴露 image flag;CLI 无对应能力 | dsh 加 image CLI 支持后 |
| dsh 自定义 model/provider | 违反 "无修改" 原则,走 `~/.dsh/settings.yaml` 改 dsh 自己的 default | 用户自己在 dsh 侧改 |
| dsh API key 管理 | 走 dsh `~/.dsh/.credentials.yaml` 或 env | dsh 自己管 |
| `--resume` 续 session | headless 不支持;即使支持也会突破 RunOnce 单 turn 模型 | chat session bridge 才有意义 |
| dsh `sandbox.mode` / `compaction.*` 配置 | 同上,本机 default 已 OK | 用户改 `~/.dsh/settings.yaml` |
| Windows 支持 | dsh 在 Windows 上没测,且 `go-pty` 在 Windows 行为差异 | 后续 |

---

## 8. 排错速查

| 症状 | 根因 | 修法 |
|------|------|------|
| `dsh: not found in PATH` | 用户没装 npm `@deepseek-ai/dsh` | `npm install -g @deepseek-ai/dsh`(实测本机已 OK) |
| `RunOnce` exit 1 + stderr `error: unknown option` | dsh 版本太老不支持 `--profile headless` | 升级 `npm install -g @deepseek-ai/dsh@latest` |
| `RunOnce` exit 1 + stderr `error: DEEPSEEK_API_KEY not in env` | dsh headless 默认配置没设凭证(虽然本机走 minimax-cn,但 dsh 还会读 deepseek 作为 fallback) | 用户在 `~/.dsh/.credentials.yaml` 补 `MINIMAX_CN_API_KEY: sk-...` 或设 env |
| `RunResult.Text` 是空 | dsh 配置问题 / API 失败但 dsh 吞错 | 检查 stderr(`cmd.Stderr` 捕获),`NIGHTME_LOGGING_LEVEL=debug` 重跑 dsh 看真实 wire |
| dsh 输出格式变了(不再是 plain text,带 ANSI / spinner) | dsh 升级加了 stdout logger | 反馈 dsh 仓;workaround 用 `pty` 模式抓 visible text(回归 codex 的 PTY 兜底) |
| `Starter.Start` 报错 "chat session not implemented" | 调用方在 chat session 路径用了 dsh | 改用 `/gtw commit` / `/gtw pr` 走 RunOnce;或换 claude/codex/pi |

---

## 9. 时间线(预估,单人)

| 阶段 | 内容 | 工作量 |
|------|------|--------|
| 1 | `starter.go` + `prompt.go` + 单测 6 个 | 0.5d |
| 2 | `print.go` + e2e 3 个(NIGHTME_REAL_DSH=1)| 0.5d |
| 3 | `cmd/nightme/agents.go` 注册 + `agents_test.go` 加断言 | 0.25d |
| 4 | 实机 smoke(`/gtw commit` 跑一次真 dsh) | 0.25d |
| 5 | 文档(本文件落地 + `docs/FEATURES.md` 索引)| 0.25d |
| 6 | review + 修 | 0.25d |
| **总计** | | **~2d** |

vs 之前 7 天 JSON-RPC bridge 方案:**节省 ~5 天**。

---

## 10. 已对齐 memory 原则

本方案严格遵循 [[agent-no-config-tampering]]:
- ❌ 不 bundled cordis.yml
- ❌ 不读 `~/.dsh/settings.yaml`
- ❌ 不读 `~/.dsh/.credentials.yaml`
- ❌ 不传 `DEEPSEEK_API_KEY` / `DSH_MODEL` / `DEEPSEEK_BASE_URL` / `DSH_SYSTEM_PROMPT`
- ✅ `cmd.Dir = cfg.Workspace`(运行时上下文)
- ✅ `DSH_PERMISSION_MODE=danger-full-access`(权限放开,用户原话)
- ✅ `dsh --profile headless -- "<prompt>"`(transport flag)

**原则对照**:nightme 改 agent 行为的唯一入口 = permissions。其他全放手给 dsh 本机 default。
