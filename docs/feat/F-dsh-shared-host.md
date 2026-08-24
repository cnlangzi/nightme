# F-dsh-shared-host — dsh 单例共享 Host 改造

> **Status**: 设计稿定稿,待实施
> **Scope**: `internal/bridge/dsh/` 改为**单一全局 dsh web 实例**服务所有 ChatSession / AgentSession
> **设计稿**: [../bridge/dsh-shared-host.md](../bridge/dsh-shared-host.md) — 实机验证 + 组件 API + 生命周期
> **Wire 参考**: [../bridge/dsh-api.md](../bridge/dsh-api.md) — 权威协议
> **当前形态**: [../bridge/dsh.md](../dsh.md) — per-ChatSession dsh 子进程
> **改动分支**: `fix-dsh-tasks`(本分支直接改)
> **2026-08-16 修订**: 用户决定**不保留旧版兼容性**,plan 改为只保留最新实现,无 env flag 切换,无 dual-run 期

---

## 0. 目标 & 边界

### 0.1 一句话

把"每 ChatSession 启一个 dsh 子进程"换成"daemon 启动一个全局 dsh,所有 ChatSession 共享"。**一次性切换,旧路径不留**。

### 0.2 必达项

1. 单 dsh 进程支持 N 个 session(实测 8 个 attached 同时活跃 ✓,目标 ≥50)
2. mux stream 单 WS,demux 路由到 N 个 ChatSession
3. approval/question 用 frame.rpcId 全局寻址,跨 session 不串扰
4. dsh 重启后所有 session 可通过 `session.list` + `session.create({sessionId, cwd})` re-attach
5. AgentSession.Drop / ChatSession.Close 只 unsubscribe + session.cancel,**不杀 host**
6. 旧 per-ChatSession `exec.Command("dsh"…)` + 子进程管理代码**全部删除**,不留 compat flag

### 0.3 边界(本期不做)

- subagent.* / workspace.* / agentPreset.* 等 dsh 协议已暴露但 nightme 暂未使用的 RPC(留 F 后续)
- per-chat 独立 permission policy(目前全局 `DSH_PERMISSION_MODE=danger-full-access`,符合 [[agent-no-config-tampering]])
- dsh session 持久归档清理(`workspace.archiveSession` — 见 [dsh.md §15](../bridge/dsh.md) commit `e9aa23d` repo-scoped workspace 策略)
- 多 nightme daemon 共享同一 dsh(每 daemon 独立起自己的)

---

## 1. 架构总览

```
                      ┌──────────────────────────────────────┐
                      │   dsh web (single Node process)      │
                      │   --profile web --port 3080          │
                      │                                      │
                      │   /api/{method}   ── shared HTTP RPC  │
                      │   /api/events.mux── ONE WS, N sess   │
                      │   /api/events.host── ONE WS           │
                      └─────────────┬────────────────────────┘
                                    │ (HTTP + 1×mux WS + 1×host WS)
                                    ▼
              ┌──────────────────────────────────────────────────┐
              │  GlobalDshClient (internal/bridge/dsh/host/)     │
              │  ─────────────────                                │
              │   client.go     : RPC + http.Client               │
              │   stream.go     : single mux/host WS + reconnect  │
              │   router.go     : per-session frame → AgentEvent  │
              │   recovery.go   : session.list → re-attach        │
              │   lifecycle.go  : dsh spawn + watchdog + shutdown │
              └─────────┬──────────────────┬──────────────────────┘
                        │ demux            │ demux
                        ▼                  ▼
                ChatSession(A)        ChatSession(B)
                │      │              │
                ▼      ▼              ▼
                AS-a (s-aaa)         AS-b (s-bbb)
```

**对比现状**(全量切换,无 fallback):
| | 现状(per-ChatSession) | 新(共享 host) |
|---|---|---|
| dsh 进程 | N 个,每个 ChatSession 1 个 | 1 个,daemon 启动时启 |
| mux WS | N 条 | 1 条 |
| pendingApprovals map | N 个,每个 driver 一个 | 1 个全局,key=(sessionId, rpcId) |
| HTTP client | N 个 | 1 个共享 |
| 子进程生命周期 | 每个 AS.Close 杀自己 | daemon 退出时统一杀 |
| 跨 session 视图 | 无 | `workspace.*` / `subagent.*` 可用(留 F) |
| 旧 driver 代码 | 存在 | **删除** |
| 旧 driver 测试 | 存在 | **删除/改写** |
| Env flag `DSH_GLOBAL_HOST` | 不存在 | **不引入** |

---

## 2. 阶段化实施(单线推进)

每阶段独立可落地 + 可测。最后阶段收尾。

---

### Phase 1 — Build & Replace(主体)

**目标**: 一次性把 GlobalDshClient 落地,把 `internal/bridge/dsh/session.go` 的旧 driver 全部替换为新的 GlobalDshSession(走 GlobalDshClient 共享连接)。`cmd/nightme/main.go` 启动时初始化 GlobalDshClient,关闭时 shutdown。**不留任何兼容代码**。

#### 2.1.1 新增文件

```
internal/bridge/dsh/host/
├── client.go          # http.Client + RPC envelope + high-level wrappers
├── client_test.go     # mock httptest.Server, envelope round-trip
├── stream.go          # mux+host WS + reconnect + Subscribe API
├── stream_test.go     # mock WS server, demux verification
├── router.go          # per-session Router + PendingApprovalMap
├── router_test.go     # frame injection → AgentEvent assertion
├── recovery.go        # RecoverSession / RecoverAll
├── recovery_test.go   # mock session.list + session.create flow
├── lifecycle.go       # Start dsh + Watchdog + graceful shutdown
├── lifecycle_test.go  # mock dsh binary (shell script)
└── testdata/
    ├── mux-subscribe-3sess.jsonl
    ├── approval-requested.json
    └── mock-dsh.sh    # lightweight dsh mock for integration tests
```

#### 2.1.2 改写文件

**`internal/bridge/dsh/session.go`** — 完全重写,从 per-driver 改为 GlobalDshSession adapter:

```go
package dsh

import (
    "context"
    "github.com/.../internal/agent"
    "github.com/.../internal/bridge/dsh/host"
)

// session 是 bridge.Session 接口的 dsh 实现。它本身不持有 dsh 进程,
// 进程由 GlobalDshClient(lifecycle.go)管理,session 只持有:
//   - 一个 sessionId(在 dsh host 上的逻辑会话)
//   - 一个 GlobalDshClient 引用(用于 RPC + Subscribe)
//   - 一个 AgentEvent 通道(从 mux demux 来)
type session struct {
    sessionID host.SessionID
    client    *host.Client
    hub       *host.StreamHub
    router    *host.Router
    events    <-chan agent.AgentEvent
    closeOnce sync.Once
}

func (s *session) Send(ctx context.Context, blocks []agent.ContentBlock) error {
    return s.client.SessionPrompt(ctx, s.sessionID, "queue", blocks, "")
}

func (s *session) Cancel(ctx context.Context) error {
    return s.client.SessionCancel(ctx, s.sessionID)
}

func (s *session) Events() <-chan agent.AgentEvent { return s.events }

func (s *session) Close() error {
    var err error
    s.closeOnce.Do(func() {
        // 1. cancel in-flight turn (best-effort, 3s)
        _ = s.client.SessionCancel(context.Background(), s.sessionID)
        // 2. unsubscribe mux
        s.hub.Unsubscribe(s.sessionID)
        // 不杀 dsh 进程,daemon 退出时才统一杀
    })
    return err
}
```

**`internal/bridge/dsh/starter.go`** — 改写 `Start`:

```go
func Start(ctx context.Context, cfg StartConfig) (bridge.Session, error) {
    // 拿全局 client(由 main.go 注入)
    client, hub := host.GetGlobal()  // 全局单例
    if client == nil {
        return nil, errors.New("dsh: global host not initialized")
    }

    // 创建或 resume session
    var sessionID host.SessionID
    if cfg.SessionID != "" {
        // resume 路径:用 cfg.SessionID + cfg.Workspace re-attach
        if err := client.RecoverSession(ctx, host.SessionID(cfg.SessionID), cfg.Workspace); err != nil {
            return nil, fmt.Errorf("dsh: resume unhealthy: %w", err)
        }
        sessionID = host.SessionID(cfg.SessionID)
    } else {
        sId, err := client.SessionCreate(ctx, host.SessionCreateOpts{
            CWD:         cfg.Workspace,
            AgentPreset: cfg.AgentPreset,
        })
        if err != nil {
            return nil, err
        }
        sessionID = sId
    }

    // 订阅 mux 流
    frameCh, err := hub.Subscribe(ctx, sessionID)
    if err != nil {
        return nil, err
    }

    // 构造 router 把 mux 帧转 AgentEvent
    router := host.NewRouter(sessionID, frameCh, client)
    router.Start(ctx)

    return &session{
        sessionID: sessionID,
        client:    client,
        hub:       hub,
        router:    router,
        events:    router.Events(),
    }, nil
}
```

**`cmd/nightme/main.go`** — 启动时初始化 GlobalDshClient:

```go
func runDaemon(ctx context.Context, cfg *Config) error {
    // 1. 初始化全局 dsh client
    dshClient, err := host.NewClient(ctx, host.ClientConfig{
        Workspace:       cfg.Workspace,
        PermissionMode:  "danger-full-access",  // per [[agent-no-config-tampering]]
        HostCmd:         "dsh",
    })
    if err != nil {
        return fmt.Errorf("dsh: failed to start shared host: %w", err)
    }
    defer dshClient.Shutdown(context.Background())

    // 2. 启动 mux/host stream hub
    if err := dshClient.StreamHub().Start(ctx); err != nil {
        return fmt.Errorf("dsh: stream hub: %w", err)
    }

    // 3. Phase 2 加:recover all persisted sessions
    // (本 Phase 先空跑)

    // 4. 注册到 host 全局
    host.SetGlobal(dshClient)

    // 5. 现有 chatsession / agent 初始化逻辑继续
    return runExistingDaemon(ctx, cfg)
}
```

#### 2.1.3 删除文件 / 代码段

- `internal/bridge/dsh/session.go::newDriver`:`exec.CommandContext("dsh", ...)` + parseWebURL + 子进程 env 注入 + cmd.Wait 生命周期 + SIGINT/SIGKILL + drainStdout/drainStderr + lifecycle goroutine + `parseWebURL` / `dshURLPattern`(整段搬到 `host/lifecycle.go`)
- `internal/bridge/dsh/session.go::Close()`:改为上面 session.Close 的简化版
- `internal/bridge/dsh/session.go::readMuxPump` / `readHostPump` / `handleMuxFrame` / `handleHostFrame`:由 `host/stream.go` + `host/router.go` 取代
- `internal/bridge/dsh/session.go::pendingApprovals` map:由 `host/router.go::PendingApprovalMap` 取代
- `internal/bridge/dsh/print.go`:保留(print-mode 跟本 F 无关)
- 相关测试全部删除/改写:
  - `resume_unix_test.go` 大部分删除(子进程 resume 不存在了)
  - `auto_resume_e2e_test.go` / `resume_e2e_test.go` / `todo_e2e_test.go` 改写为 GlobalDshSession 的 e2e
  - `dispatch_test.go` / `handle_mux_test.go` / `state_test.go` / `view_test.go` 改写为针对 router 的测试
  - `print_real_unix_test.go` 保留(print-mode 跟本 F 无关)
  - `session_smoke_test.go` 改写
  - `deliver_nonblock_test.go` 改为 router_test 的相关 case
  - `translate_regression_test.go` 保留(translate 逻辑不变)
  - `contentblocks_test.go` 保留(协议格式不变)
  - `starter_test.go` 改写

**预期净变化**:删除 ~1500 行旧代码,新增 ~2500 行 host/ 包 + 简化版 session.go。

#### 2.1.4 dispatch.go standardRegistry 扩展

注册 probe 发现的 9 个新 event type(见 [dsh-shared-host.md §5](../bridge/dsh-shared-host.md)):

```go
standardRegistry = map[string]frameHandler{
    // ... 已有 11 个 ...
    "permission/preset":     debugOnly,    // 初始 sandbox 配置
    "sandbox/mode":          debugOnly,
    "approval/policy":       debugOnly,
    "agent/inbox/spliced":   debugOnly,    // queue spliced by server
    "user/message":          debugOnly,    // ⚠ 不能 emit,避免重复气泡
    "request/header":        debugOnly,
    "request/context":       debugOnly,
    "step/start":            handleStepBoundary, // no-op; sessionStats, not TodoPanel
    "step/end":              handleStepBoundary,
    "session/title":         emitTitle,
    "session/title-llm-request": debugOnly,
}
```

#### 2.1.5 测试覆盖

- `host/client_test.go`: ≥6 个 case(session.list / create / prompt / cancel / fork / Respond)
- `host/stream_test.go`: mux 单连接多 session + demux + reconnect
- `host/router_test.go`: 注入 session/event 各类型 → 验证 emit AgentEvent 正确
- `host/recovery_test.go`: list N 个 + recover all + 验证 re-attach 计数
- `host/lifecycle_test.go`: mock dsh binary,验证启停 + SIGINT/SIGKILL 时序
- `dsh/session_test.go`(改写):通过 mock GlobalDshClient,验证 Send/Cancel/Close 走对 RPC
- e2e:`DSH=1` 跑真 dsh,触发 5 条连续消息,验证每条都有 OutReply

#### 2.1.6 退出标准

- `go test ./... -count=1` 全绿
- `go vet ./...` 无 warning
- 新代码覆盖率 ≥ 80%
- `grep -r 'exec.Command("dsh"' internal/` 只在 `host/lifecycle.go` 出现 1 次
- `grep -r 'cmd.Process.Signal' internal/bridge/dsh/` 只在 `host/lifecycle.go` 出现(daemon shutdown 路径)
- `grep -r 'DSH_GLOBAL_HOST' .` 为 0(无遗留 env flag)
- 5 条连续消息 smoke test 全绿,OutReply 不串扰

#### 2.1.7 风险

- **R1(高)**: mux demux bug 导致事件串扰。缓解:`host/router_test.go` 注入帧级断言 + 5 条消息 smoke + dev mode 下 `debug/inspect mux` 命令实时查看 demux 状态
- **R2(中)**: dsh 启动失败 → daemon 启动失败(无法 fallback)。缓解:`cmd/nightme/main.go` 启动顺序把 dsh 放在最早,fail-fast + systemd/launchd 自动重启

---

### Phase 2 — Restart Recovery

**目标**: dsh 重启 / nightme daemon 重启后,**所有 persisted sessionId 自动 re-attach + mux 自动 subscribe + ChatSession 无感知**。

#### 2.2.1 改动

- `internal/registry/chat_session_file.go` 加字段:
  ```go
  type ChatSessionEntry struct {
      ...
      DshSessionID    string  // dsh 上的 sessionId,空 = fresh
      DshCWD          string  // re-attach 时回传
      DshAgentPreset  string
  }
  ```
- `internal/registry/agent_session_file.go` 加 `DshSessionID string`
- `internal/chatsession/chatsession.go::FromPersisted`:
  - 读 `DshSessionID` → 如果非空,异步调 `host.RecoverSession(sId, cwd)`
  - 失败 → 打日志 + 标记 `DshSessionID = ""`(等同 fresh session)
- `cmd/nightme/main.go` 启动 GlobalDshClient 后:
  ```go
  go func() {
      result, err := dshClient.RecoverAll(ctx, func() []host.PersistedSession {
          return loadAllPersistedDshSessions()
      })
      metrics.RecordRecover(result)
  }()
  ```
- `host/recovery.go` 加 metrics 输出(`attached=N orphaned=M new=K`)

#### 2.2.2 不变量

- re-attach 失败(session.create 返 `session-conflict`)→ 不 panic,只 warn + 视为 fresh
- re-attach 期间 ChatSession 已收到 chat 消息 → 走 fresh session.create 路径(用户感知为"这次是新 session")
- re-attach 后 dsh session 的 history 必须能通过 `session.history` 拉回(已在 §2.6 验证)

#### 2.2.3 测试

- 单元:`recovery_test.go` 加 case — registry 有 3 个 persisted,recover 期间 mock dsh 返回 ok / fail / conflict 三种
- 集成(harness `test-recovery.sh`):
  ```bash
  #!/bin/bash
  ./bin/nightme _daemon &
  sleep 5
  # 触发 dsh session + 重启 dsh 后验证 recovery
  kill -TERM $(pgrep -f 'dsh.*--profile web')
  sleep 3
  dsh --profile web --port 3080 &         # 重启 dsh
  sleep 5
  # 此时给主聊天发任意 user prompt,期望:OutReply 正常,dsh sessionId 跟之前一致
  ```
- 灾难恢复:kill dsh + 重启 daemon 双双发生,验证仍能恢复

#### 2.2.4 退出标准

- `test-recovery.sh` 通过
- `metrics.RecordRecover` 输出数字符合预期
- 5 次连续 daemon 重启(每次都恢复)无 sessionId 漂移

#### 2.2.5 风险

- **R3(中)**: re-attach 时 cwd 漂移(用户改了 cwd) → `session.create` 返 `session-conflict` → 视为 fresh,用户感知为新 session(可接受)
- **R4(中)**: re-attach 期间 ChatSession 已收新消息 → 走 fresh 路径,旧历史不丢(可接受)

---

### Phase 3(并行)— Bug fixes + Event type 完整覆盖

**目标**: 顺手修 dsh-api.md §11 列的 5 个 BUG + 完成 standardRegistry 全覆盖。

#### 2.3.1 改动

- `internal/bridge/dsh/protocol.go:projectionEnvelope`:
  ```go
  type projectionEnvelope struct {
      SessionID string          `json:"sessionId"`
      Key       string          `json:"key"`         // was: Projection
      Value     json.RawMessage `json:"value"`
      Seq       int             `json:"seq"`
  }
  ```
- `internal/bridge/dsh/host/permissions.go`(新位置):`SendPermission` envelope 改为 `client-response`
- outcome vocabulary 改为 `"allowed-once"/"rejected"`(dsh-api.md §2.12.1)
- `protocol.go:questionPayload.Options` 改为 `[]AskUserQuestionOption{label,description?}`
- `permissions.go:handleQuestionRequested` 用 frame.rpcId 作 pending key,response payload 用 `QuestionResponsePayload` shape
- Phase 1 已完成 dispatch 注册,本 Phase 加 `state_test.go` 回归 + 新 event type 的 view_test

#### 2.3.2 测试

- 单元: 每个 BUG 一个 test case,模拟 dsh 端点验证 envelope shape
- 实机: 真触发 approval/requested(用 permission-policy 拒绝的 prompt),验证 Respond 路径

#### 2.3.3 退出标准

- dsh-api.md §11 的 5 个 BUG 表格从 ❌ 变 ✅
- standardRegistry 覆盖 0.1.0-rc.6 全部已知 event type(无"unknown method" warning)

#### 2.3.4 风险: 低(纯 wire 层,dsh 0.1.0-rc.6 双接受,fix 走 canonical 不会 break)

---

## 3. 测试策略

### 3.1 测试金字塔

```
            ┌─────────────────────────────────────┐
            │  E2E (manual + harness scripts)      │  ← Phase 1 / 2
            ├─────────────────────────────────────┤
            │  Integration (mock dsh binary)        │  ← Phase 1
            ├─────────────────────────────────────┤
            │  Unit (mock httptest + injected frames)│  ← Phase 1 / 3
            └─────────────────────────────────────┘
```

### 3.2 mock dsh binary

`internal/bridge/dsh/host/testdata/mock-dsh.sh`(Go 程序或 shell script):
- listen $PORT, parse minimal HTTP
- session.list → 返 N 个 canned sessions
- session.create → 返固定 sessionId("test-s-1")
- session.prompt → 立即模拟 `assistant/chunk` + `turn/end` 序列
- mux WS: 发 session/subscribed + 推 canned event 流
- approval/requested: 周期性推 1 帧让 bridge 测 Respond 路径

### 3.3 真 dsh 测试 guard

`internal/bridge/dsh/host/testhelpers_realdsh_test.go`:
```go
func requireRealDsh(t *testing.T) {
    t.Helper()
    if _, err := exec.LookPath("dsh"); err != nil {
        t.Skipf("dsh not on PATH: %v", err)
    }
}
```
真 dsh 测试只在本地(有 dsh)跑,CI SKIP。

### 3.4 每 Phase 退出标准

1. `go test ./... -count=1` 全绿
2. 新代码覆盖率 ≥ 80%
3. 无新 lint warning
4. 文档同步更新([dsh-shared-host.md](../bridge/dsh-shared-host.md) + 本 F 文档 status 字段)

---

## 4. 风险登记

| # | 风险 | 影响 | 概率 | 缓解 | Owner |
|---|---|---|---|---|---|
| R1 | mux demux bug 导致事件串扰 | 高 | 中 | router_test 注入帧级断言 + 5 条消息 smoke + `debug/inspect mux` 命令 | TBD |
| R2 | dsh 启动失败 → daemon 启动失败 | 高 | 低 | fail-fast + systemd/launchd 重启 | TBD |
| R3 | re-attach 时 cwd 漂移 | 中 | 中 | session-conflict → 视为 fresh | TBD |
| R4 | re-attach 期间 chat 已收消息 | 中 | 中 | 走 fresh 路径,旧历史不丢 | TBD |
| R5 | Phase 1 大改,regression 面广 | 中 | 中 | 已有完整 mock dsh 测试 + e2e harness | TBD |
| R6 | dsh 0.1.0-rc.6 上游变更 | 中 | 低 | wire 测试 + dsh-api.md 锁定 | TBD |
| R7 | 多 nightme daemon 同机端口冲突 | 低 | 低 | OS 分配端口即可 | TBD |
| R8 | 5 个 bridge BUG 修完上游行为变更 | 低 | 低 | dsh 0.1.0-rc.6 双接受,fix 走 canonical 不会 break | TBD |

**已删除的风险**(旧 plan 有,新版不需要):
- ~~R1(原): DSH_GLOBAL_HOST=0 回退~~ → 不留 compat flag,无需回退路径
- ~~R2(原): approval/requested routing 错误~~ → 合并到 R1 由 router_test 覆盖

---

## 5. Rollout 计划

### 5.1 时间表(估)

| Phase | 估时 | 累计 | 并行可做 |
|---|---|---|---|
| **Phase 1**: Build & Replace | 5d | 5d | Phase 3 |
| **Phase 2**: Restart Recovery | 2d | 7d | — |
| **Phase 3**: BUG fix + event types | 2d | 7d | 与 Phase 1 并行 |

总计 ~7 工作日(Phase 3 2d 与 Phase 1 部分并行)。

### 5.2 上线顺序

1. **Phase 3(并行)**: BUG fix + new event types,先合(纯 wire 层,影响面小)
2. **Phase 1**: 主体 PR,内部 review + 跑通本地 e2e
3. **Phase 1 合入后**: 本地 dogfood 1 周(用户实际聊天跑通 dsh 路径)
4. **Phase 2**: Restart Recovery,合入 + 跑 `test-recovery.sh` 5 轮

### 5.3 回滚方案

- **Phase 1**: 不可逆(旧代码已删)。需要 git revert commit。引入 forward-only 限制:**Phase 1 合并前必须本地 dogfood 充分**
- **Phase 2**: 可逆(删 registry 字段 + 启动 RecoverAll 调用,向后兼容)
- **Phase 3**: 可逆(纯 envelope shape 修复,改回去 dsh 仍接受)

### 5.4 Communication

- Phase 1 合并前发 RFC(因为不可逆)
- Phase 2 完成发 changelog
- Phase 3 完成发 changelog

---

## 6. 验收清单(Definition of Done)

- [ ] `internal/bridge/dsh/host/` 5 个核心文件(client/stream/router/recovery/lifecycle)+ 测试齐全
- [ ] `internal/bridge/dsh/session.go` 改写为 GlobalDshSession adapter(< 200 行,无子进程管理)
- [ ] mux demux 实机验证 ≥ 50 个 attached session(压测脚本)
- [ ] approval/requested 实机验证 frame.rpcId routing 正确
- [ ] 5 条连续消息 e2e 测试通过,OutReply 不串扰
- [ ] Phase 2:d sh 重启后所有 persisted session 自动 re-attach(test-recovery.sh 通过 5 轮)
- [ ] Phase 3:dsh-api.md §11 5 个 BUG 全修,9 个新 event type 注册
- [ ] `grep -r 'DSH_GLOBAL_HOST' .` 为 0(无遗留 env flag)
- [ ] `grep -r 'exec.Command("dsh"' internal/bridge/dsh/` 只在 `host/lifecycle.go` 1 处
- [ ] `go test ./...` 全绿,新代码覆盖率 ≥ 80%
- [ ] [dsh-shared-host.md](../bridge/dsh-shared-host.md) 与本 F 文档同步
- [ ] CHANGELOG 更新,用户文档更新

---

## 7. Open Questions(实施中遇到再答)

1. **DSH 端口**: 默认 `3080`(与浏览器 dsh 兼容)还是 `--port 0`(OS 分配)?
   - 倾向 3080,但要检测端口已被占用 → fallback 到 0
2. **session.list cache TTL**: 启动期一次扫后,本地 cache 怎么 invalidate?
   - 候选: 靠 host/session-added/removed 流 + 周期 5min 重扫
3. **re-attach 失败的 chat**: 是否要主动通知用户"您的历史 session 已失效"?
   - 当前倾向: 仅 debug log,UI 层面等用户主动发新消息时静默 fallback
4. **Phase 1 不可逆**: 是否要先在独立分支验证,再合本分支?
   - 当前 plan: 直接在 `fix-dsh-tasks` 改,本地 dogfood 1 周后合

---

## 8. Change log

- 2026-08-16: 初稿,基于 [dsh-shared-host.md](../bridge/dsh-shared-host.md) 实机 probe + 用户提案
- 2026-08-16 修订: 用户决定不保留旧版兼容性,删除 Phase 1(dual-run)+ Phase 3(删旧路径),合并为 Phase 1(Build & Replace);移除 DSH_GLOBAL_HOST env flag;移除 dual-run sanity 期;移除"不可逆"标记改为 Phase 1 整体不可逆;移除 R1/R2 旧风险项
- 2026-08-16 修订: dashboard To-dos 条对齐 `todo/write` + `todos` 投影;`step/start|end` 改为 registry no-op(不是 `emitStepBoundary` / OutTask*)