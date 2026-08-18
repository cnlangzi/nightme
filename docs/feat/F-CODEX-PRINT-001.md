# F-CODEX-PRINT-001 — codex RunOnce 走 `codex exec` print mode

> **状态**: 已落地 (2026-08-14)
> **范围**: `internal/bridge/codex/print.go` + `starter.go` 委托
> **姊妹**: [F-CLAUDE-PRINT-001](./F-CLAUDE-PRINT-001.md) · [F-PI-PRINT-001](./F-PI-PRINT-001.md)

---

## 背景

`codex.Starter.RunOnce`(原 `internal/bridge/codex/starter.go:76`)和
`codex.Starter.Start` 共用同一条 `app-server` spawn 配方:

```text
codex app-server --listen stdio:// \
  -c approval_policy="never" \
  -c sandbox_mode="danger-full-access"
```

(`internal/bridge/codex/session.go:262-265`。) 这套 argv 是为**长生命周期
JSON-RPC stdin 模式**设计的:握手 (`initialize` + `initialized` +
`thread/start`) + 持续喂 `turn/start` + 收 `turn/completed` /
`item/*` 事件 + 跨 turn 持有 `events` chan。chat session 的多 turn
路径正确依赖这套。

one-shot 调用(`/gtw commit`、`/gtw pr`、`buildAgentPrompt`)本来不需要
这种模式。复用 `Start` 等于让 bridge 起一个长 JSON-RPC session,
送一条 `turn/start`,等到 `turn/completed`,再 `defer Close()`。

这条路继承了 `Start` 的全部 surface,**没有任何一项是 one-shot 需要的**:

- JSON-RPC 握手 + 6 条 `optOutNotificationMethods`(`protocol.go:36-42`)
- `readPump` + `stderrLoop` + `lifecycle` + `rpcClient` 四个 goroutine
- 跨 turn 持有的 `events` chan + `pumpWG`/`exitDone` 同步
- 5s `closeDrainTimeout` 上限(`session.go:53`)——一次性场景等满 5s 是浪费
- resume-preservation 探测 + busy guard + `pendingTurnActive` 锁

---

## claudecode / pi 的同类经验

| Bridge | Feature | 教训 |
|---|---|---|
| `pi` | F-PI-PRINT-001 (2026-08-13) | `RunOnce` 复用 Start 的 RPC mode → production 高负载下偶发"prompt RPC ack 后 2-5s stdout pipe 关闭但无事件"。务实修法:one-shot 改走 `pi --mode json -p <prompt>` |
| `claudecode` | F-CLAUDE-PRINT-001 (2026-08-14) | 长生命周期 stdin 模式被强用于 one-shot。务实修法:`claude -p <prompt>` 跑完即退 |

codex 这边是同一形态——长生命周期 JSON-RPC session 被强用于 one-shot。
**按 pi / claudecode 的同款修法落到 codex 上**,即为 F-CODEX-PRINT-001。

---

## codex CLI 一次性入口核对

实测 `codex-cli 0.145.0`(2026-08-14,本机 binary):

```text
$ codex exec --help | head -40
Run Codex non-interactively

Usage: codex exec [OPTIONS] [PROMPT]

Options:
  -c, --config <key=value>         # TOML override,e.g. -c model="o3"
      --enable <FEATURE>           # -c features.<name>=true
  -i, --image <FILE>...            # ← 可重复,实测有效
  -m, --model <MODEL>              # ← 选模型
  -s, --sandbox <SANDBOX_MODE>     # read-only | workspace-write | danger-full-access
      --dangerously-bypass-approvals-and-sandbox
                                   # ← 一 flag 抵 app-server 的两个 -c
  -C, --cd <DIR>                   # workspace
      --skip-git-repo-check        # 非 git 工作树兜底
      --ephemeral                  # 不持久化 session 文件
  -o, --output-last-message <FILE> # ← final agent_message 单独写到文件(无 progress 噪声)
      --json                       # ← stdout NDJSON 事件流(thread.started / turn.completed.usage)
  -h, --help
```

**关键 flag 行为(逐项实测)**:

- `--dangerously-bypass-approvals-and-sandbox`:一 flag 同时 bypass 审批 + sandbox,
  等价 app-server 的 `-c approval_policy="never" -c sandbox_mode="danger-full-access"`。
- `-o <file>`:只写 **最终 `agent_message`** 到文件,**不**写 tool_call progress、
  **不**写 user/codex marker——实测验证(用 `codex exec -o /tmp/x.txt` 跑,文件
  内容只有 `PONG`,stderr 才有 noise)。
- `--json`:stdout = NDJSON 事件流。`thread.started` 带 `thread_id`,
  `turn.completed.usage` 带 `{input_tokens, cached_input_tokens, output_tokens}`——
  实测 SessionID 和 Usage 都能从这些事件解析出来。
- `-i <path>`:可重复,实测喂 1×1 PNG 后模型正确描述图片(注:实测输出含 fallback
  tool 调用解析 PNG chunk,因为我们用了 1×1 极小图;正常图片直接进模型视觉)。
- `--skip-git-repo-check`:`/gtw commit` 可能在非 git 目录跑(例如临时 worktree),
  没这个 flag 会 false alarm。
- `--`:**必须**有。codex 0.145 在 `-i` 与 positional prompt 共存时若没 `--`
  会把 prompt 当 stdin 读(实测 bug)。

---

## 设计

### RunOnce 的 spawn 配方

```go
// internal/bridge/codex/print.go::buildPrintArgs
args := []string{
    "exec",
    "--dangerously-bypass-approvals-and-sandbox",
    "-C", cfg.Workspace,
    "--skip-git-repo-check",
}
// ContentImage → 追加 -i <Path>(repeatable)
// ContentText + ContentFile → 合成 positional prompt
prompt := strings.Join(promptParts, "\n")
if prompt == "" {
    prompt = "(see attached content)" // sentinel:避免 codex exec 走 stdin 回退
}

// runPrintMode 续接:
args = append(args,
    "-o", tmpPath,       // ← 动态 tempfile
    "--json",            // ← NDJSON 事件流
    "--",                // ← 必须
    prompt,
)
```

### 文件改动

| 文件 | 改动 |
|---|---|
| `internal/bridge/codex/starter.go` | `RunOnce` 改 1 行委托到 `runPrintMode`(原实现 `Start + defer Close + agent.RunOnceDrain` 整段删除) |
| `internal/bridge/codex/print.go` *(新)* | `runPrintMode` + `buildPrintArgs` + `runNDJSON` + wire types (`codexExecEvent` / `codexExecUsage`) |
| `internal/bridge/codex/print_internal_test.go` *(新)* | 7 个 argv 构造单测(不需 codex binary,CI 必跑) |
| `internal/bridge/codex/print_real_unix_test.go` *(新)* | 4 个 e2e(`NIGHTME_REAL_CODEX=1` 才跑真 binary) |
| `docs/bridge/codex.md` | §1.2 / §1.3 / §13 更新 + 新增 §1.4 Print Mode 章节 |

`Start` 路径(`Starter.Start` → `newDriver` → `newSession` →
JSON-RPC handshake) **完全不动**——chat session 仍走长生命周期
JSON-RPC 模式。

### `runPrintMode` 形态

```text
proc.New(ctx, s.command, args...)    ← 一次性启子进程
  ├ cmd.Dir = cfg.Workspace
  ├ cmd.StdoutPipe() → runNDJSON()        ← 解析 thread.started + turn.completed.usage
  └ cmd.StderrPipe() → goroutine drain    ← 仅失败路径 wrap 进 error
↓
os.ReadFile(tmpPath)                       ← -o 文件内容 = RunResult.Text
↓
cmd.Wait()                                 ← 进程自然退出,无需 closeDrainTimeout
↓
return RunResult{
    Text:       strings.TrimSpace(string(finalBytes)),
    SessionID:  thread.started.thread_id,        ← 从 NDJSON 解析
    Usage:      turn.completed.usage → *UsageInfo, ← 从 NDJSON 解析
    DurationMs: time.Since(startTime).Milliseconds(),
    Subtype:    exit==0 ? "completed" : "failed",
}
```

### RunResult 字段映射

| 字段 | 来源 | 与 app-server 的关系 |
|---|---|---|
| `Text` | `-o <tmpfile>` 文件内容(干净,无 noise) | app-server 是 `EventAgentResult.Result.Text`(`translate.go` finishTurn) |
| `SessionID` | `thread.started.thread_id`(NDJSON 第一条) | app-server 是 `sess.threadID`(`session.go:436`)——**同源语义** |
| `Usage` | `turn.completed.usage{input_tokens, cached_input_tokens, output_tokens}` | app-server 是 `appServerUsage{InputTokens, CachedInputTokens, OutputTokens}`(`translate.go:684`)——**同源语义**,wire 字段名仅大小写风格不同 |
| `Subtype` | exit code 0/非零 → `"completed"` / `"failed"` | app-server 是 `turn.status`(`completed`/`failed`)——**同源语义** |
| `DurationMs` | 我们 wall-clock 测 | app-server 是 `ev.Result.DurationMs`(`EventAgentResult` payload) |
| `Model` | 空(当前 `StartConfig` 无 `Model` 字段) | 后续可由 `-m` flag 注入 |

### `codex` 与 claudecode / pi 的对称性

| Bridge | Print 入口 | Chat 入口 |
|---|---|---|
| `claudecode` | `claude -p <prompt> --output-format stream-json --verbose --permission-mode bypassPermissions` | `claude --print --input-format stream-json --output-format stream-json --permission-mode bypassPermissions --verbose` (held stdin) |
| `pi` | `pi --mode json -p <prompt>` | `pi --mode rpc <socket>` (RPC session) |
| `codex` | `codex exec --dangerously-bypass-approvals-and-sandbox -C <ws> --skip-git-repo-check --json -o <tmpfile> [-i <imgs>] -- <prompt>` | `codex app-server --listen stdio:// -c approval_policy="never" -c sandbox_mode="danger-full-access" [-c model=...]` (JSON-RPC session) |

三 bridge 现在 print / chat 路径完全独立,各自走自己 CLI 的一次性 /
长生命周期入口。

---

## 测试覆盖

### 1. `print_internal_test.go` —— argv 构造单测(7 个,CI 必跑)

| 测试 | 锁定点 |
|---|---|
| `TestBuildPrintArgs_TextOnly` | 固定 prefix:`exec` / `--dangerously-bypass-...` / `-C` / `--skip-git-repo-check` |
| `TestBuildPrintArgs_TextJoinsWithNewlines` | 多 ContentText 用 `\n` 拼成 prompt,空 Text 跳过 |
| `TestBuildPrintArgs_ImageFlagRepeated` | 多 ContentImage 各自一个 `-i <Path>`(顺序保持),`countImageFlags` 验证 |
| `TestBuildPrintArgs_FileAsAtRef` | ContentFile 走 `@<Path>` 文本注解(无 file flag) |
| `TestBuildPrintArgs_AllImagesFallsBackToSentinel` | 全 image 时 prompt = `"(see attached content)"` |
| `TestBuildPrintArgs_EmptyBlocksFallsBackToSentinel` | blocks=nil 也走 sentinel |
| `TestBuildPrintArgs_SkipEmptyPaths` | 空 Path 在 image/file 上静默 drop |

### 2. `print_real_unix_test.go` —— e2e(4 个,`NIGHTME_REAL_CODEX=1` 启用)

| 测试 | 锁定点 |
|---|---|
| `TestRunPrintMode_HappyPath` | 跑真 binary,prompt `Reply with exactly: PONG`,验证 Text / Subtype / DurationMs / SessionID / Usage 全填好 |
| `TestRunPrintMode_EmptyWorkspaceFails` | 守卫前置条件:`cfg.Workspace == ""` → 立即 `workspace is required` 错误 |
| `TestRunPrintMode_EmptyBlocksDoesNotHang` | blocks=nil → sentinel 让 codex exec 不走 stdin 回退,正常返回 |
| `TestRunPrintMode_BinaryNotFound` | 错误 binary 名 → `codex: start: exec: ...` 错误 |

### 3. 实测数据(codex-cli 0.145.0,本机 e2e)

```
$ NIGHTME_REAL_CODEX=1 go test ./internal/bridge/codex/ \
    -run "TestRunPrintMode_HappyPath" -count=1 -v

=== RUN   TestRunPrintMode_HappyPath
INFO [codex] PrintMode Start command=codex workspace=/tmp/foo
                          prompt_bytes=85 args_count=10 image_count=0 pid=52274
INFO [codex] PrintMode Exit  pid=52274 elapsed_ms=9828 wait_err=<nil>
                          stderr_bytes=39 session_id=019fff55-11bc-7283-b757-b8be07822c6a
result: Text="PONG"  Subtype=completed  DurationMs=9828
        SessionID=019fff55-11bc-7283-b757-b8be07822c6a
        Usage=InputTokens:17869 OutputTokens:22 CacheReadInputTokens:128
--- PASS: TestRunPrintMode_HappyPath (9.83s)
```

全 5 个 RunResult 字段都从真 binary 拿到了 ✅。

### 4. 已有测试 + 上游 caller

| 检查 | 结果 |
|---|---|
| `go build ./internal/bridge/codex/...` | ✅ |
| `go vet ./internal/bridge/codex/...` | ✅ clean |
| `go test ./internal/bridge/codex/... -short -timeout 1m` | ✅ 全量 PASS(4.4s) |
| `go build ./internal/command/gtw/...`(`/gtw` 是 Starter.RunOnce 的真实 caller) | ✅ |
| `go build ./...` 全工程 | ✅ |

---

## 不变性 & 不破坏的事

- **chat-session 路径完全不动**。`Starter.Start` → `newDriver` →
  `newSession` → JSON-RPC handshake → 持续 turn 流程,均不修改。
  chat 用户(夜跑会话)的体验完全一致。
- **`agent.RunOnceDrain` 不变**(本 PR 时点)。该函数仍是 `internal/agent/runonce.go` 的
  公开 helper,**本次迁移只删了 codex 这一个 bridge 对它的引用**(acp / opencode
  还在用)。后续 Phase 1b / 1c(acp / opencode)按相同模式迁完之后,会一并删除。
  *(后续 F-OPENCODE-PRINT-001 / F-RUNONCEDRAIN-INTERNAL 已完成此合并,见对应 feat doc。)*
- **`Starter` 接口签名不变**。`Starter.RunOnce` 还是 4 个参数 + 2 个返回值,
  `/gtw commit` / `/gtw pr` / `buildAgentPrompt` 调用方零改动。
- **`Starter.Info` / `Starter.Detect` / `Starter.Start` 不变**。docs §1.2
  "chat-session 单选 `app_server`"决策继续生效——`exec` 是 print-mode
  入口,**不**是 chat-session 备用后端。

---

## 后续 Phase(不在本 PR)

| Phase | 内容 | 状态 |
|---|---|---|
| 1b | `acp/print.go` 同样按 print-mode 思路切(若 ACP CLI 有 `-p` / 一次性 flag) | 待评估 *(ACP 无 CLI-side print-mode flag,  此 Phase 跳过,见 F-RUNONCEDRAIN-INTERNAL)* |
| 1c | `opencode/print.go` 同样按 print-mode 思路切(若 opencode CLI 有) | ✅ done in F-OPENCODE-PRINT-001 |
| 3 | 三 bridge 全部迁完后,删除 `agent.RunOnceDrain` + `runonce.go` + `runonce_test.go` | ✅ done in F-RUNONCEDRAIN-INTERNAL — helper 被内联进 `(*acp.Starter).collectResult` |

---

## 回滚

需要的话只需 revert 本 PR:
- `internal/bridge/codex/starter.go` 恢复 `Start + defer Close + agent.RunOnceDrain` *(F-RUNONCEDRAIN-INTERNAL 后 acp 的对应实现变为 `Start + defer Close + (*Starter).collectResult`,内联 drain 循环)*
- 删除 `internal/bridge/codex/print*.go`
- docs 改动回滚

无 schema 变更,无持久化变更,无依赖变更,revert 安全。