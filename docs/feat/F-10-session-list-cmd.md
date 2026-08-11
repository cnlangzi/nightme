# F-10: Session List Command

> **Status**: designed (v0.1)
> **Milestone**: M3 (CLI tools)
> **Depends on**: F-05 (Registry)
> **Related docs**: [SPEC.md](../SPEC.md) §2.5

## 1. Description

`nightme list` CLI 命令通过本地 HTTP API（127.0.0.1:7823）查询主进程，返回所有 session 状态（running / detached / exited）。`nightme kill <sid>` 强制 kill 指定 session。

## 2. Interface

```go
// internal/ipc/server.go
type Server interface {
    Start(ctx context.Context) error  // listen 127.0.0.1:7823
    Stop(ctx context.Context) error
}

type ListResponse struct {
    Sessions []SessionInfo `json:"sessions"`
}

type SessionInfo struct {
    SID        string    `json:"sid"`
    ChatID     string    `json:"chat_id"`
    Workspace  string    `json:"workspace"`
    Agent      string    `json:"agent"`
    PID        int       `json:"pid"`
    Status     string    `json:"status"`  // "running" | "detached" | "exited"
    StartedAt  time.Time `json:"started_at"`
    ExitCode   *int      `json:"exit_code,omitempty"`
}

// HTTP routes (internal/ipc/router.go):
//   GET  /v1/sessions        → ListResponse
//   GET  /v1/sessions/{sid}  → SessionInfo
//   POST /v1/sessions/{sid}/close  → 204
```

**CLI 命令**：
```bash
nightme list              # 列出所有 session
nightme list --json       # 输出 JSON
nightme kill <sid>        # 强制 kill session CLI
nightme status            # 检查主进程是否在跑 + session 数
```

## 3. Implementation

**文件**：
- `internal/ipc/server.go` — HTTP server（仅 127.0.0.1:7823）
- `internal/ipc/router.go` — chi router + handlers
- `cmd/nightme/list.go` — `nightme list` 子命令
- `cmd/nightme/close.go` — `nightme kill` 子命令

**输出格式**（text mode）：
```
SID              AGENT   WORKSPACE                              PID     STATUS      STARTED
s_01HF8XXXXX     claude  /home/devin/code/bailing               12345   running     10:30:00
s_01HF9XXXXX     claude  /home/devin/code/nightme               12350   detached    10:35:12
s_01HF10XXXXX    claude  /tmp/test                               -       exited(0)   11:00:00
```

**实现选择**：用 `cobra` 做 CLI 框架。

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| 主进程没跑（nightme daemon 没启动）| list 命令 HTTP connect 失败 → 提示 "nightme daemon not running" |
| registry 文件损坏 | API 返回 500 + error message |
| kill 的 session 不存在 | API 返回 404 |
| kill 的 session 状态是 exited | 返回 200，提示 "session already exited" |
| 多个 nightme daemon（用户误操作）| v0.1 不防，假设只有一个 |
| 本地 7823 端口被占用 | Server.Start 返回 error，主进程退出 |
| 通过远程访问（v0.1 不支持）| Server 仅 listen 127.0.0.1，远程不可达 |

## 5. Test plan

**单元测试**：
- 表格格式化：3 个 session → 输出 3 行 + header
- JSON 序列化：SessionInfo → JSON marshal 一致

**集成测试**：
- 启动 mock IPC server → 调用 GET /v1/sessions → 验证 response
- POST /v1/sessions/{sid}/close → 验证 mock process 被 kill

**手动测试**：
- `nightme list` → 看到当前 session
- `nightme kill s_XXX` → 飞书 DM 收到 "session killed"

## 6. Open questions

- 是否需要 `nightme attach <sid>` 进入交互式 terminal？v0.1 不做，留 v0.2
- 是否需要 `nightme logs <sid>` 看 stdout 历史？v0.1 不记录历史（F-15 v0.2）
- 是否需要 Unix socket（不用 TCP）？v0.1 用 TCP 简单；v0.2 改 socket
- IPC 是否要鉴权？v0.1 不需要（仅 127.0.0.1）
