# nightme

Sleep tight, code all night.

nightme 是一个单进程 daemon，把 AI Coding CLI（Claude Code / Codex / OpenCode）桥接到 IM 渠道（飞书 / WhatsApp / Web UI），让你在睡前丢一句「把 X 写了」第二天醒来收结果。

> **当前状态**：M1 — 基础架构（接口 + 配置 + registry）
> 详细设计见 [`docs/SPEC.md`](./docs/SPEC.md) 与 [`docs/PLAN.md`](./docs/PLAN.md)。

## 编译

需要 Go 1.22+。

```bash
go build -o bin/nightme ./cmd/nightme
./bin/nightme
# -> nightme v0.1.0 - Local Bridge test mode
```

## 项目结构

```
nightme/
├── cmd/nightme/         # 入口（M1 仅 hello world）
├── configs/             # YAML 配置示例
├── docs/                # 设计文档（PRD / SPEC / FEATURES / PLAN / feat/*）
├── internal/
│   ├── agent/           # Agent / AgentSession / Event 接口 + registry
│   ├── bridge/          # Bridge 三层模式（acp / sdk / pty）
│   │   ├── acp/
│   │   ├── sdk/
│   │   └── pty/
│   ├── cli/             # CLI 命令实现
│   ├── config/          # YAML 配置加载
│   ├── registry/        # JSON 进程 registry
│   └── session/         # Session Manager（pending PR #2）
├── go.mod
├── go.sum
└── README.md
```

## 下一步

M1 之后是 PR #2 / #3：Channel Adapter / Session Manager / 实际 Bridge 实现。

参见 [`docs/PLAN.md`](./docs/PLAN.md) 了解完整路线图。
