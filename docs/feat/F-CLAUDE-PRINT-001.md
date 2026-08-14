# F-CLAUDE-PRINT-001 — claudecode RunOnce 走 print mode

## 背景

`claudecode.Starter.RunOnce`(原 `internal/bridge/claudecode/starter.go:112`)
和 `claudecode.Starter.Start` 共用同一份 spawn 配方:

```
claude --print --input-format stream-json --output-format stream-json
       --permission-mode bypassPermissions --verbose
```

(`internal/bridge/claudecode/permissions.go:62` 的 `DefaultArgs`。)

这套 argv 是为**长生命周期 stdin 模式**设计的:`--print` 关掉 TUI,
`--input-format stream-json` 让 bridge 通过 stdin 持续喂 user message,
`--output-format stream-json` 让 claude 持续吐 wire event——bridge hold
住 stdin,claude 就不会退出,可以持续接多轮 turn。

one-shot 调用(`/gtw commit`、`buildAgentPrompt`)本来不需要这种模式。
复用 Start 的 spawn 等于让 bridge 起一个长 session,送一条 user
message,等到 result event,再强关 session。

这条路继承了 Start 的全部 surface,**没有任何一项是 one-shot 需要的**:

- stream-json stdin 协议解析 + response correlation
- `--resume` resume-preservation probe(`probeResume`,看 `claudecode.go`)
- busy guard(发 user message 时的 in-flight 跟踪)
- 长时间 hold 住的 pipe

## pi 的同类经验(F-PI-PRINT-001, 2026-08-13)

pi bridge 当年同样问题——`Starter.RunOnce` 复用 Start 的 RPC mode,
在 production 里 `/gtw commit` 触发到"prompt RPC ack 后 2-5s stdout
pipe 关闭但没有任何事件被消费"的失败模式。同一段 `RunOnce` 流程在
`go test` 烟雾测试里稳定通过,production 高负载下偶发失败。

排查到最后,最可能的元凶是 RPC-mode 特有的状态(long-lived pipe、
response-correlation map、pending waiter)在大 daemon 负载下偶发丢
事件。务实修法:one-shot **不走 RPC**,改走 `pi --mode json -p
<prompt>`,这条路径:

- 没有 long-lived pipe
- 没有 response correlation
- 没有 pending waiter
- 进程自己 exit,自然 turn-end

完整叙述见 `internal/bridge/pi/print_unix.go:1-45`(F-PI-PRINT-001 段)。

claudecode 这边是同一形态——长生命 stdin 模式被强用于 one-shot。
**按 pi 的同款修法落到 claudecode 上**,即为 F-CLAUDE-PRINT-001。

## Claude Code `-p` 官方语义核对

来源:`https://code.claude.com/docs/en/cli-reference` +
`https://code.claude.com/docs/en/headless`(已抓取核对)。

- `-p` / `--print`:non-interactive 模式。`claude -p "<prompt>"`
  是单条命令式,进程跑完即退。`--print` 单独配 `--input-format
  stream-json` 才进入长生命周期 stdin 模式(bridge 现在的用法)。
- `--output-format stream-json`:stdout 为 LF-delimited JSON event。
- `--verbose`:**必须**带——`stream-json` 输出不开 `--verbose`
  不会生效。
- `--bare`:默认**不传**。RunOnce 必须与 chat session 的 Start
  路径用同样的环境(CLAUDE.md / hooks / MCP / OAuth),任何一项
  跳过都会让 `/gtw commit` 等与用户日常体感不一致。
  兜底:未来 `StartConfig.Bare=true` 时按需带上。
- `--allowedTools`:逗号分隔预批准工具列表;本次先不接
  `StartConfig.AllowedTools`,默认 `bypassPermissions` 兜底。
- `--permission-mode`:本次先不接 `StartConfig.PermissionMode`,
  默认 `bypassPermissions`。

## 设计

### RunOnce 的 spawn 配方

```go
// internal/bridge/claudecode/print_unix.go
args := []string{
    "-p", prompt,
    "--output-format", "stream-json",
    "--verbose",
    "--permission-mode", "bypassPermissions",
}
```

要点:

- `-p <prompt>` 把 prompt 作为 positional argv,claude 不读 stdin,
  进程跑完 turn 即退。**和 Start 路径的 `--input-format stream-json`
  互斥,本配方绝不传**。
- **`--bare` 默认不开**:RunOnce 必须与 Start 走相同的环境
  (CLAUDE.md / hooks / MCP / OAuth)。未来 `StartConfig.Bare=true`
  时按需带上。
- `--allowedTools` 暂时不接,留待后续按需。

### 文件改动

| 文件 | 改动 |
|---|---|
| `internal/bridge/claudecode/starter.go` | `RunOnce` 不再调 `s.Start`,改调 `runPrintMode`;新增 `blocksToPrompt`(镜像 `pi/starter.go:117`) |
| `internal/bridge/claudecode/print_unix.go` *(新)* | spawn + stderr drain + `cmd.Wait()`(镜像 `pi/print_unix.go` 结构) |
| `internal/bridge/claudecode/print_mock_test.go` *(新)* | mock-script 测试套件 |
| `internal/testdata/claude_print_mock.sh` *(新)* | mock `claude -p` 二进制,通过 env-var 驱动失败模式 |

Start 路径(`Starter.Start` → `newDriver`)完全不动——chat session
仍走长生命周期 stream-json stdin 模式。

### `runPrintMode` 形态

镜像 `pi.Starter.RunOnce` → `pi.runPrintMode`(细节见
`internal/bridge/pi/print_unix.go:79`):

1. `agent.NewCmd(ctx, command, args...)` 起进程,`cmd.Dir = workspace`
2. `stdout` / `stderr` 走 `cmd.StdoutPipe` / `cmd.StderrPipe`
3. 后台 goroutine drain stderr 到 `strings.Builder`(失败路径带
   stderr 一起 wrap 进 error)
4. `streamPrintEvents(ctx, stdout, ...)` 同步跑:
   - 一行一行 `json.Unmarshal` 进 `streamEvent`
   - 复用 `stream.go::translate` 翻译(协议事件相同)
   - 监听 `EventAgentResult` 捕获 `result.result` 文本
5. `cmd.Wait()` 拿 exit code + stderr
6. 没看到 `result` 事件 → 报 `claudecode: exit without result event`
7. `waitErr != nil` → 报 `claudecode: exit: ... (stderr: ...)`

settle 判据用 **`result` event**,而不是 `EventAgentDone`(后者
在 print-mode 下语义模糊,`Done` 是 process exit marker)。

### 测试覆盖

1. **mock-script**(`print_mock_test.go` + `claude_print_mock.sh`):
   - clean run → 返回 text
   - 非零退出 + stderr → 错误信息带 stderr
   - 无 `result` event → 报 `exit without result event`
   - workspace 透传(自定义 + 空 workspace 提前报错)
   - sanity: mock script 路径可解析

2. **T-alive 真 claude**(可选,需要 `claude` 在 PATH + 守门
   `NIGHTME_TALIVE_RUNONCE=1`):
   - `claude -p "echo 42" --output-format stream-json --verbose --bare`
     断言能拿到 `"42"` 文本
   - 验证 `--bare` 不会破坏 print-mode 协议

3. **回归**:现有 `claudecode_test.go` 里所有 Start 路径测试应
   当一行不改全绿。这是关键——RunOnce 改了不能影响 Start。

## 图片附件:调研结论

**`claude -p` 不支持图片附件。**

调研依据:
- [Claude Code CLI reference](https://code.claude.com/docs/en/cli-reference) 只列出
  `-p` / `--print` / `--output-format stream-json` / `--input-format stream-json`
  / `--append-system-prompt-file` 等。**没有 `--file` flag,没有 `@file` 语法,
  没有 stdin 图片管道的文档**。
- [Anthropic Vision docs](https://platform.claude.com/docs/en/build-with-claude/vision)
  描述了 Messages API 的图片能力(base64 / URL / Files API `file_id`),
  但这些是 **API 层的能力,不是 CLI 的能力**——CLI `-p` 不暴露它们。

后果:
- `blocksToPrompt` 把 `ContentImage` 降级成 `[image: /path (mime/png)]` 字符串
  是当前唯一可行的路径(`/gtw commit` 等 caller 拿不到真实图片)。
- 如果 caller 必须传图,只能走 Start 路径(交互模式),让用户手动挂附件。
- 这不是 F-CLAUDE-PRINT-001 的退让——是 claude `-p` 协议本身的限制。后续
  如果 Anthropic 加 `--file` flag 到 `-p`,我们再补一条 `ContentImage`
  转发路径即可。

## 行为变化(用户可见)

1. **RunOnce 不再读 stdin**(`-p` 模式天然如此)。如果未来有
   caller 想通过 stdin 喂 prompt,需要先 `blocksToPrompt` 后走
   `-p`,或继续走 Start 路径。

2. **`--bare` 默认不开**——RunOnce 与 chat session 共享环境
   (`~/.claude/CLAUDE.md` / hooks / MCP / OAuth)。这条是
   F-CLAUDE-PRINT-001 review 后的明确选择,理由:nightme 的
   多数用户走 OAuth 登录,`--bare` 会让他们无法认证;另外用户
   配的 hooks / CLAUDE.md 在 RunOnce 必须生效,否则行为与
   chat session 不一致。

3. **没有 resume / no session-id probe**。RunOnce 一直都没有
   session(注释里写了 `cfg.SessionID is always empty for one-shot`),
   但之前是通过 Start 间接绕过的;现在直接不进 Start 路径,
   probe 永远不会跑,`Start` 的 resume 健康检查也保护不到 RunOnce。
   实际影响:RunOnce 一次失败就失败,没有"fallback to fresh session"
   这种行为(本来也没有),所以零行为变化。

## 回滚

把 `Starter.RunOnce` 改回 `s.Start(ctx, cfg)` 一行就回到现状:

```go
func (s *Starter) RunOnce(ctx, cfg, blocks) (string, error) {
    a, err := s.Start(ctx, cfg)
    if err != nil { ... }
    defer a.Close()
    return agent.RunOnceDrain(ctx, a, blocks, s.Info().Name)
}
```

`print_unix.go` / `print_mock_test.go` / `claude_print_mock.sh`
可以保留作为档案,不影响编译。

## 后续

- acp / pty / codex 同样有"RunOnce 复用 Start"的问题。pi 和
  claude 是先撞到的,其它三个目前没人反馈。**先不动**,等
  真实 fail mode 出现再按相同模板改造。
- `StartConfig.AllowedTools` / `StartConfig.PermissionMode` / 
  `StartConfig.NoBare` 字段按需引入,不预先加。