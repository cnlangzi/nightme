# gtw hooks + agent 配置

> **状态**: 设计阶段。
> 落地代码位于 `internal/command/gtw/hooks.go`(parser/runner/resolver)和 `cmd.go` 的 factory wrapper。

---

## 背景

`teamflow v1` 落地时坚持 "零新文件"(`teamflow-config-design` memory),`~/.nightme/hooks.yaml` 被显式标记为 "未来再设计"。

`v1.1` 触发那个未来:用户希望:
1. 某些 `/gtw` 命令执行前后跑附加步骤(`codegraph init`、IM 通知、本地脚本)。
2. 不传 `-a` 时,也能指定该命令默认用哪个 agent。

两个需求共用一份配置,落在 `~/.nightme/gtw.yml`(用户级,跨仓库全局)。

---

## 铁律

> **hook 与 agent 配置都是附加的,绝不阻塞主流程。**

任何 hook 失败 / agent 找不到 / 配置 malformed / 配置缺失 → 全部以 `⚠️ warn` 形式呈回响应,主流程继续走。

这条原则覆盖所有边界:
- hook 缺失或 binary 不存在 → skip + warn
- hook 执行返回非 0 → warn + 继续下一个 hook + 继续主流程
- yml 格式错 → warn + 整个 yml 当作不存在
- yml 引用未知 agent → warn + 退到下一档优先级
- yml 完全不存在 → 静默,跟今天行为一致

---

## 配置文件

### 位置

`~/.nightme/gtw.yml`(用户级)

> 注意:仓库工作区里的 `.nightme/gtw.yml`(由 `gtw.ReadGTWYml` 读,作为 `/gtw push` 的 source of truth)与本配置**完全独立**,命名相同纯属历史巧合。

### Schema

```yaml
# ~/.nightme/gtw.yml
fix:
  agent: pi                # 可选:该命令"需要 agent 时"用的 agent 名
  hooks:
    before:
      - codegraph init                       # 字符串简写 = shell
      - run: gh issue view "$ISSUE" --json title  # 长形式,语义同 shell
    after:
      - echo "fix flow complete"
push:
  agent: pi                # 仅 pushDirty 生效(pushClean 不起 agent)
  hooks:
    before:
      - run: |
          git rev-parse --abbrev-ref HEAD > /tmp/last-branch
    after: []
# close:   # v1 也支持,但默认空(用户可按需配)
# sync:
```

每个 `<cmd>` 子节点三选一+组合:
- `agent: <name>` — 可选
- `hooks.before: [...]` — 可选
- `hooks.after: [...]` — 可选

任意字段缺失 = 该功能不启用,**不报错**。

### Go 类型(`internal/command/gtw/hooks.go`)

```go
type Config struct {
    Fix   CmdConfig `yaml:"fix"`
    Push  CmdConfig `yaml:"push"`
    Close CmdConfig `yaml:"close"`
    Sync  CmdConfig `yaml:"sync"`
}

type CmdConfig struct {
    Agent string `yaml:"agent"`
    Hooks Hooks  `yaml:"hooks"`
}

type Hooks struct {
    Before []Hook `yaml:"before"`
    After  []Hook `yaml:"after"`
}

// Hook 是 v1 的 shell hook。结构化对象是前向兼容:
// 未来 type=agent / type=notify 直接加 switch case,不改 schema。
type Hook struct {
    Type string `yaml:"type,omitempty"` // v1 仅识别 "" 或 "shell"
    Run  string `yaml:"run,omitempty"`  // shell 命令
    // env / cwd / timeout 等:v1 不暴露,见"非 v1 字段"小节
}
```

- `Type == ""` 或 `"shell"` → 走 shell runner。`Run` 必填,缺失 = warn + skip。
- `Type == 其他` → warn "unsupported hook type %q (v1 supports: shell)" + skip。**不报错**(铁律)。
- 字符串简写:`- codegraph init` 在 yaml 解析后等价于 `{run: "codegraph init"}`,由 `yaml.Unmarshal` 自动处理(plain string in sequence → struct with one field set)。

---

## Agent 选择 — 3 档优先级

只对"该命令需要 agent 时"生效:

| 优先级 | 来源 | 示例 |
|---|---|---|
| 1 | CLI `-a` / `--agent`(per-invocation) | `/gtw push -a claude` |
| 2 | `~/.nightme/gtw.yml` 的 `<cmd>.agent` | `push: agent: pi` |
| 3 | `cs.SelectedAgent()`(session 默认,由 `/use <name>` 设置) | — |
| 兜底 | 全部为空 | ❌ 走既有的 "no agent selected" 报错路径 |

插入位置:`pushDirty` 的两行之间(`commit_push.go:147-156`):

```go
agentName := args.Agent                       // 优先级 1
if agentName == "" {
    agentName = yml.Push.Agent                // 优先级 2(新)
}
if agentName == "" {
    agentName = cs.SelectedAgent()            // 优先级 3
}
```

**作用域**:
- `push.agent:` 只在 pushDirty 路径生效。pushClean 是纯 `git push -u origin`,不起 agent,该字段被忽略。
- `fix.agent:` v1 无调用点(当前 fix 流程不发 agent)。字段保留以备未来,加载但不消费。
- `close.agent:` / `sync.agent:` 同上,v1 保留 + 不消费。

**降级语义**:当优先级 2 选了 `pi` 但 `agent.Builtins.Get("pi")` 失败:
- ❌ 不静默退到优先级 3(用户配的就是 `pi`,默默换 agent 会让 hook 用户困惑)。
- ❌ 不报错 brick /gtw(违反铁律)。
- ✅ **走优先级 3**(fallback 到 session),响应里加一行 `⚠️ gtw.yml agent "pi" not found; using session default`。

如果优先级 3 也为空 → 走既有的 `❌ no agent selected` 报错(铁律下"业务必要失败"仍然 fail,只是 hook 类失败不阻断)。

---

## Hook 执行

### 触发点

工厂层(`internal/command/gtw/cmd.go`)的每个 `runXxx` 方法,包一层:

```go
func (f *Factory) runFix(...) {
    cfg, _ := loadHookConfig()    // 任何错误 → 返回空 cfg,warn 计入响应

    // before 全部跑完后才进主流程
    preNotes := runHooks(cfg.Fix.Hooks.Before, ctx, cs.SelectedCwd(), chatID, messageID)
    defer func() {
        // after 在主流程返回前集中跑(无论成功失败)
        postNotes := runHooks(cfg.Fix.Hooks.After, ctx, cs.SelectedCwd(), chatID, messageID)
        appendHookNotes(reply, preNotes, postNotes) // 拼到回复末尾
    }()

    // ... 既有主逻辑
}
```

> **defer 选择**:即使主流程 panic,defer 仍会跑 — 这是"after 总是跑"的关键。

### 失败语义

```
┌─ before h1 ─┐
├─ before h2 ─┤  ← 任一 before 失败:warn + 继续下一个 + 继续主流程
├─ main ───────┤
├─ after h1 ───┤  ← 主命令成功失败都跑;失败 = warn + 继续下一个
├─ after h2 ───┤
└──────────────┘
```

输出规则:
- **全部回显**(用户确认):每个 hook 的 stdout+stderr 都拼进 IM 响应。
- 失败(warn)标注:`⚠️ hook failed: codegraph init (exit 127, stderr: command not found)`
- 成功不特别标注,跟正常 stdout 一起贴出来。

### 执行细节

| 维度 | 行为 |
|---|---|
| **cwd** | `cs.SelectedCwd()`(用户已 `/cwd` 的目录)。空 → skip + warn。 |
| **shell** | `sh -c <run>`(`/bin/sh` always available,跨平台)。 |
| **timeout** | 默认 30s/hook。可被 `~/.nightme/gtw.yml` 里的 `timeout:` 字段 per-hook 覆盖(v1 暂不实现,留字段)。 |
| **env** | 继承 daemon 环境(`os.Environ()`),**不**额外注入。 |
| **stderr** | 合并到 stdout 一起回显(用户已确认全回显)。 |
| **exit code** | `0` = 成功;非 0 = warn(标注 exit code + stderr tail),不阻断。 |
| **hook binary 缺失** | shell 退出 127 → 同 exit code 非 0 处理,warn + 继续。 |

### 非 v1 字段(env / cwd / timeout)

`Hook` struct 上**不暴露**这些字段(v1 简化),但 schema 设计留好位:
- v2 加 `Timeout time.Duration` 字段:yaml `timeout: 30s` 直接生效,不破坏已有 yml。
- v2 加 `Env map[string]string`:模板不展开,直接 `os.Setenv` + `defer Unsetenv`。
- cwd 永远跟随 `cs.SelectedCwd()`,**永不开 per-hook 配置**(避免用户误以为 hook 能跑到别的目录)。

---

## 反应(reaction)路径

`gtw` reaction(草稿卡确认 / cancel)走 `ChatSession.HandleReaction`,**不触发 hook**。

理由:
- reaction 是同一 fix 流程的延续,不是新的 "fix invocation"。如果 reaction 也触发,用户点一个 ✅ 可能跑两遍 `codegraph init`。
- reaction 自身无 main command 可"before/after"。

v1.2+ 可考虑 `hooks.onReaction` 之类的扩展(per-reaction-emoji),但当前 YAGNI。

---

## 配置加载

```go
// internal/command/gtw/hooks.go
func loadHookConfig() Config {
    home, err := os.UserHomeDir()
    if err != nil { return Config{} }   // 静默
    path := filepath.Join(home, ".nightme", "gtw.yml")

    data, err := os.ReadFile(path)
    if errors.Is(err, os.ErrNotExist) {
        return Config{}                  // 文件不存在 = 空配置
    }
    if err != nil {
        return warnEmpty("read ~/.nightme/gtw.yml: %v", err)
    }

    var c Config
    if err := yaml.Unmarshal(data, &c); err != nil {
        return warnEmpty("parse ~/.nightme/gtw.yml: %v", err)
    }
    return c
}
```

- 不存在 → 空 cfg,无 warn(对应铁律 "缺失静默")
- malformed → 空 cfg + warn 入响应
- 加载成功但 `<cmd>` 节点缺失 → 该 cmd 视为无配置(部分 yml OK)

性能:`/gtw` 命令频率低(人手触发),每次重新读文件成本可忽略。**不缓存**,用户编辑 yml 后立即生效。

---

## 实施 TODO(任务已建)

1. `internal/command/gtw/hooks.go` — parser + runner + agent resolver + Config/Loader
2. `cmd.go` — factory 层 `runFix/runPush/runClose/runSync` 包 `withHooks`
3. `commit_push.go:147-156` — `pushDirty` agent 优先级链插入 yml 中档
4. 测试:
   - parser: 字符串简写 / 长形式 / 缺失字段 / 未知 type
   - runner: 成功 / exit≠0 / binary 缺失 / timeout / cwd
   - factory wrapper: before 失败不阻断 / after 总是跑 / 输出拼接
   - agent: 3 档优先级 + 未知 agent 降级
5. 更新 `teamflow-config-design` memory(v1 → v1.1)

---

## 未来(v2+)

- `type: agent` — hook 内起 agent 做 prompt(走 `agent.RunOnce`)
- `type: notify` — IM 通知(走 `cs.Channel().Send`)
- `timeout:` / `env:` per-hook 字段
- `~/.nightme/hooks.d/*.yml` — 多文件拆分(用户 yml 主文件 + 子目录包含)
- 反应路径 hook(`hooks.onReaction`)