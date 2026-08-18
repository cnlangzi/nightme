# Cross-platform agent smoke test

> **目的**: 强制 nightme 的 bridge 启动路径在每个 GitHub-hosted runner 上跑通,捕"开发只在 macOS 上手测,Windows/Linux 真机启动路径悄悄回归"这类 bug。
> **关联**:
> - 架构 + Windows 陷阱 → [`docs/WINDOWS.md`](./WINDOWS.md)
> - 单 bridge 协议 → [`docs/bridge/`](./bridge/)
> - Feishu 端到端 → [`docs/E2E_TESTING.md`](./E2E_TESTING.md)
> - CI 配置 → [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) `agent-smoke` job

---

## 0. 背景:dsh-on-Windows 事件

2026-08 中,`cmd/nightme/agents.go:55-61` 在 `init()` 里把 dsh 注册包了一层 `runtime.GOOS != "windows"` 检查,注释写:

> _"dsh is a Node.js CLI; Windows support is a non-goal per the dsh project."_

这个 gate 在 main 上存在了若干 PR 都没人 review,因为:

1. 开发团队主要在 macOS 上日常测试,这段代码肉眼可读、看起来合理,没人去 verify 注释的真伪;
2. 现有 Go 测试套件对 spawn-touching 路径大量使用 `//go:build !windows` / `//go:build !unix` 跳过(`grep -rn 'go:build !windows' internal/` 命中 ~30 处),Windows 分支在 CI 里覆盖率本就稀疏;
3. `cross-compile` job 在 ubuntu 上交叉编译到 `windows/amd64` **只跑 build,不跑 test**,所以编译过的代码没人验证它真能跑;
4. 没有 e2e 级别的"装 agent → 起 nightme → 看 agent 启动" gate。

**实际后果**:

- `nightme.exe agents` 在 Windows 上不列 dsh;
- `nightme.exe config` 同样不列 dsh;
- 用户要么以为 nightme 不支持 dsh,要么 hack `config.yaml` 加 user-defined 走 PTY 兜底,丢掉结构化能力;
- 修复点在 PR review 里被三连 reviewer 都误以为是"作者有意识地屏蔽 Windows",差点被回滚。

修复只动了一行 (`if runtime.GOOS != "windows"` 包装层) + 注释改写,但**没有 CI 防护栏的话,下次重构还会悄悄回滚**——这正是 `agent-smoke` 这个 job 存在的理由。

---

## 1. 测试金字塔里的位置

nightme 的验证覆盖分五层,从下到上:

```
            ┌───────────────────────────────┐
            │  5. 真实 e2e (Feishu 端到端)  │   ← docs/E2E_TESTING.md
            │     需 apikey + 真机/Live    │     PR 不能强制跑(配额 + 密钥)
            ├───────────────────────────────┤
            │  4. agent-smoke (CI smoke)    │   ← 本文档;三平台 runner
            │     无 apikey,只验启动        │     每个 PR 必跑
            ├───────────────────────────────┤
            │  3. 单元 + race + coverage    │   ← test job
            │     fake driver,bridge logic  │     每个 PR 必跑 (linux)
            ├───────────────────────────────┤
   ─────────┤  2. 编译时跨平台 vet/build    │   ─────── bridge 边界 ────────
            │     GOOS 切换但不实际跑       │     cross-compile job
            ├───────────────────────────────┤
            │  1. 静态检查 (go vet)         │   ← test / cross-compile job
            └───────────────────────────────┘
```

`agent-smoke` 占据第 4 层:**编译过 + 单元测试过 + 跨平台编译过** → 真起一次看 spawn 是否活着。它补的不是"功能正确性"(那是 fake driver 单测的活),而是"启动路径在每个平台上都通"——这层正好是现有套件覆盖最薄的地方。

---

## 2. 设计原则

按重要性排:

| # | 原则 | 反例 | 正例 |
|---|------|------|------|
| 1 | **不依赖 apikey / 网络** | "跑 `claude 'hello'` 然后断言响应包含 'hi'" | "跑 `claude --help`,断言非空输出" |
| 2 | **统一 5-gate 协议,任何 builtin 都套同一组检查** | 给 dsh 写一套特殊步骤 | `for agent in $LIST; do gate1; gate2; …; done` |
| 3 | **每层 gate 独立 catch 不同失败模式** | 一个大断言里塞 5 种不相关的期望 | 5 个独立 step,各自 exit code |
| 4 | **必须真有响应才算启动成功** | "spawn 后 6 秒还活着 = pass"(hung 也"活着") | "spawn 后 8 秒内输出 size > banner = pass" |
| 5 | **不断言响应内容** | grep "API key error" / "model reply" 之类 | 只断言 size > threshold(任何 byte 都是 pass) |
| 6 | **官方安装方式优先于 curl-pipe** | `curl … \| bash` 在 CI 里 cert/geo/版本都抖 | `npm install -g <official-package>` 跨平台 deterministic |
| 7 | **挂在依赖链尾,cheap-first / expensive-last** | agent-smoke 在 test 之后就起 | agent-smoke 等 `test` + `test-windows` + `cross-compile` 都绿 |

---

## 3. 5-Gate 设计

每个 agent 跑同一组 5 个 gate。每层 catch 不同失败模式,所以都跑完才算真绿。

### Gate 1: binary on PATH

```bash
for agent in claude codex opencode pi dsh; do
  command -v "$agent" || fail
done
```

**Catch 什么**:`npm install -g` 没成功 / npm bin dir 没加到 PATH / 包名拼错 / npm registry 临时挂。

**为什么不只 `which`**:nightme 内部走 `exec.LookPath` —— 跟 `command -v` 在 bash 里是同一回事(POSIX execvp + PATHEXT 顺序)。在这层 gate 上 bash 和 Go 用的是同一个查找逻辑,所以这步是真能 catch "nightme 会找不到这个 binary"。

### Gate 2: binary `--help` 响应

```bash
output=$("$agent" --help 2>&1) || true
[ -n "$output" ] || fail
```

**Catch 什么**:binary 装了但启动就崩(例如某个 Node.js native module 缺、binary 跟 OS arch 不匹配、GLIBC 太老)。

**为什么 `--help` 而不是 `--version`**:`--version` 通常只是个常量字符串,只验"binary 能加载"。`--help` 走 argv 解析 → 命令表查找 → 输出 usage,触达的代码路径是 `--version` 的超集,等价于"binary 完整可用"。

### Gate 3: `nightme agents` 列出

```bash
out=$(./dist/nightme agents)
for agent in $LIST; do
  echo "$out" | grep -qE "(^|[[:space:]])${agent}([[:space:]]|$)" || fail
done
```

**Catch 什么**:`cmd/nightme/agents.go` 的 `init()` 漏注册 / GOOS gate 错误屏蔽 / package import 失败导致 builtin list 为空。

**这层是 dsh-on-Windows 事件的直接 catch 点**:任何对 `agent.Builtins.Register(...)` 的运行时 GOOS 屏蔽都会让某 platform runner 上的这步变红。

**为什么 subset 而不是 exact match**:用户可在 `$NIGHTME_CONFIG` 里加 user-defined agents,跑这层时如果断言"恰好等于内置列表"会把合法配置当 bug。

### Gate 4: `nightme test --agent X` 必须有响应

```bash
printf 'PONG\n' | ./dist/nightme test --agent "$agent" --workspace "$workspace" \
  >"$out_file" 2>&1 &
pid=$!

deadline=$(( $(date +%s) + 8 ))
acknowledged=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  size=$(wc -c < "$out_file" | tr -d ' ')
  if [ "$size" -gt 150 ]; then
    acknowledged=1
    break
  fi
  if ! kill -0 "$pid" 2>/dev/null; then
    break  # process exited, no point waiting
  fi
  sleep 0.5
done
kill "$pid" 2>/dev/null || true
[ "$acknowledged" -eq 1 ] || fail
```

**Catch 什么**:bridge 找到了 binary 但 spawn 失败 / spawn 成功但子进程立刻崩 / 子进程 hang 永远不回应。

**为什么 poll size 而不是单纯 aliveness**:hung 的子进程 `kill -0` 也返回 0(进程存在),纯 aliveness 检查会让 hung agent 假阳性 pass。我们要求"banner 之外的任何字节",等价于"agent 至少 acknowledge 了一次"。banner 阈值 150 字节是实测上界(`[nightme] session <id> started in <ws> (agent=…, args=…)\n` 在 80-120 字节区间,150 是它的 1.25x 安全余量)。

**为什么 8 秒**:足够 PTY agent 完成 echo + 一次 protocol 初始化,足够 JSON-RPC agent 把 framing error log 写到 stderr,足够 dsh shared-host 完成端口 bind + URL 输出。任何超出 8 秒还没 acknowledge 的 agent 都不是"启动慢",是真的坏了。

**为什么不 grep 响应内容**:用户明确"提示 apikey 不存在也是一种工作的响应"。每个 agent 的协议不同:

| Agent | 期望 ack 路径 |
|---|---|
| `claude` (PTY) | `PONG\n` 被 PTY echo → 终端渲染 → 输出 >> banner |
| `codex` (stdio JSON-RPC) | 收到非 JSON 行 → framing error → stderr >> banner |
| `opencode` (同上) | 同 codex |
| `pi` (stdio JSONL) | 收到非 JSONL 行 → parse error → stderr >> banner |
| `dsh` (shared-host) | bind 127.0.0.1:3080 → stdout 打 "dsh web: http://..." >> banner |

每种都"自然产生"超出 banner 的输出,不需要 mock 任何东西。

### Gate 5 (隐含):每个 step 的非零 exit

`set -e` 隐式保证:任何 step 失败 → step exit code != 0 → action 标红。无需单独写"step 5 校验全部 exit code"。

---

## 4. 安装策略:为什么是 `npm install -g`

5 个被测 agent 全部通过 npm 官方分发:

| Agent | npm 包 | 来源 / 验证锚 |
|---|---|---|
| `claude` | `@anthropic-ai/claude-code` | `internal/agent/exec_windows.go:43` 注释引用 + Anthropic 官方 setup 文档 |
| `codex` | `@openai/codex` | `docs/WINDOWS.md:183` "npm 包内的 codex.cmd" + OpenAI 官方 docs |
| `opencode` | `opencode-ai` | `docs/WINDOWS.md:184` |
| `pi` | `@earendil-works/pi-coding-agent` | `docs/bridge/pi.md:271` |
| `dsh` | `@deepseek-ai/dsh` | `internal/bridge/dsh/starter.go:82` `Detect()` 错误信息 |

**为什么不用 curl-pipe installer**(虽然有些 agent 官方提供):

- `curl … | bash` 在 CI 里 cert pinning / geo 路由 / release channel pin 都会抖;
- npm registry 通过 lockfile-style 解析是 deterministic 的,跨 platform / 跨 runner 一致;
- `setup-node` 把 npm bin dir 自动加到 PATH,无需手维护 PATHEXT。

**`opencode` 的特殊性**:它除 npm 外还有官方 curl installer(`opencode.ai/install`),作为**第二官方渠道** —— 这是用户从非 npm 环境装 opencode 的官方方式。CI 这层选 npm 是因为 deterministic;curl 是给真机用的。

**没覆盖的 agent**:`bash`(pty.NewStarter 注册的 PTY fallback),因为它是平台自带二进制 —— Linux/macOS 的 `bash` / `apt bash`、Windows 上没有。这个 gap 由 unix-only Go 测试覆盖,不是这层 gate 的责任。

---

## 5. CI Workflow 拓扑

```
test (ubuntu build + race + coverage)        ~1-2 min
  ├─→ test-windows (windows build + tests)   ~2-3 min
  ├─→ cross-compile (4-target matrix)        ~2-3 min (并行)
  └─→ agent-smoke (matrix: ubuntu/windows/macos)
       needs: [test, test-windows, cross-compile]
       ~3-5 min × 3 runners
```

**为什么挂在末尾**:`agent-smoke` 是文件里最贵的 gate(npm install × 5 × 3 OSes + 5 spawn 尝试 × 8s 观察)。任何前置红了,跑这层就是纯烧钱。挂在链尾 = cheap-first / expensive-last。

**为什么 fail-fast: false**:`npm install -g` 可能因 registry / 网络问题在某一两个 OS 上挂,不应连带把另外两个 OS 的 gate 也拖红。每个 runner 独立报 pass/fail。

**为什么显式包含 macos-latest**:开发团队以 macOS 为主力,日常覆盖率高,但**CI 必须独立跑 macos-latest 才能捕"只有 clean hosted runner 才能暴露"的回归**——具体见 §6.2。

**已知 trade-off**:每个 `agent-smoke` runner 要等所有 3 个前置完成才启动。Linux runner 要等 `test-windows`(Windows hosted runner,~2 min),Windows runner 要等 `cross-compile`(ubuntu 上跑,~2 min),macOS runner 也要等所有 3 个。三个 runner 各浪费 ~2 min,合计 ~6 min 闲置。

如果 CI 时间预算吃紧,可拆成 3 个独立 job(每个 OS 一个 needs 链),消掉跨 OS 等待。本次保持单 job 优先。

---

## 6. 在本地复现

CI 是 hosted runner 上跑的,但开发者本地也可以跑同一组检查。

### 6.1 一键跑全套

```bash
# 前置:Go 1.22+、Node 20+
go build -o /tmp/nightme ./cmd/nightme
export PATH="/tmp:$(npm config get prefix)/bin:$PATH"

# Gate 1: PATH
for agent in claude codex opencode pi dsh; do
  command -v "$agent" >/dev/null || { echo "FAIL: $agent missing"; exit 1; }
done

# Gate 2: --help
for agent in claude codex opencode pi dsh; do
  out=$("$agent" --help 2>&1) || true
  [ -n "$out" ] || { echo "FAIL: $agent --help"; exit 1; }
done

# Gate 3: nightme agents
/tmp/nightme agents
for agent in claude codex opencode pi dsh; do
  /tmp/nightme agents | grep -qE "(^|[[:space:]])${agent}([[:space:]]|$)" \
    || { echo "FAIL: $agent not listed"; exit 1; }
done

# Gate 4: bridge spawn + response
workspace=$(mktemp -d)
for agent in claude codex opencode pi dsh; do
  out_file=$(mktemp)
  printf 'PONG\n' | /tmp/nightme test --agent "$agent" --workspace "$workspace" \
    >"$out_file" 2>&1 &
  pid=$!
  deadline=$(( $(date +%s) + 8 ))
  ok=0
  while [ "$(date +%s)" -lt "$deadline" ]; do
    [ "$(wc -c < "$out_file" | tr -d ' ')" -gt 150 ] && { ok=1; break; }
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.5
  done
  kill "$pid" 2>/dev/null
  [ "$ok" -eq 1 ] || { echo "FAIL: $agent no response"; cat "$out_file"; exit 1; }
done

echo "OK: all 5 agents across 5 gates"
```

### 6.2 为什么 CI 不能只信 macOS dev 测试

开发在 macOS 笔记本上跑同一组 gate 通常全绿,但 CI 还是独立跑 macos-latest,原因:

| 维度 | macOS dev | macos-latest hosted runner |
|---|---|---|
| `~/.dsh/settings.yaml` | 真用户配置(可能漏 API key 也"假装能跑") | clean,无配置 |
| `PATH` | 装着 10 个没列在 builtin 里的 binary(可能掩盖 PATH 顺序问题) | 只有 npm bin + 系统 PATH |
| shell env | 累积多年的 alias / export / oh-my-zsh | 默认 bash,无 user-level rc |
| node 版本 | 跟着 brew 升级漂移 | Node 20 LTS(pinned) |
| 用户态 `/tmp` | 跟其他 dev session 共享 | 每次 fresh |

CI macos-latest 跑出来跟 dev mac 不一致时,**几乎总是 CI 才对**(dev 环境有噪声掩盖 bug)。这就是为什么即使 macOS 已被开发者每天覆盖,CI 也要再独立跑一次。

---

## 7. 加一个新 builtin agent 的 checklist

要在 `cmd/nightme/agents.go` 加一个新的 builtin agent(比如 `gemini`),必须同步改 CI:

```diff
--- a/.github/workflows/ci.yml
+++ b/.github/workflows/ci.yml
@@ -234,12 +234,14 @@
         # bash is also a builtin (PTY fallback) but it's a
         # platform-provided binary — apt bash on Linux/macOS,
         # n/a on stock Windows. It's covered by unix-only Go
         # tests, not this gate.
+        #   gemini → @google/gemini-cli
+        #             (per docs/bridge/gemini.md §2 — package
+        #              name verified against Google's install
+        #              docs)
         npm install -g @anthropic-ai/claude-code
         npm install -g @openai/codex
         npm install -g opencode-ai
         npm install -g @earendil-works/pi-coding-agent
         npm install -g @deepseek-ai/dsh
+        npm install -g @google/gemini-cli
@@ -250,7 +252,7 @@
           for agent in claude codex opencode pi dsh; do
+          for agent in claude codex opencode pi dsh gemini; do
             path=$(command -v "$agent" 2>/dev/null || true)
             ...
```

每个 gate 的 `for agent in …` 列表加新名字,**一处都不能漏** —— 这是为什么 5 个 gate 都在同一个文件里集中维护,而不是分散在 N 个 yaml 文件里。

新增 builtin 同步必做:**找到 package name 的官方锚点**(agent 自己的 setup docs / WINDOWS.md / bridge 的 Detect 错误信息),写到注释里。下次有人怀疑包名错了能立刻 trace 回来源。

---

## 8. 已知不 catch 的回归

这层 gate 有意识地把以下情况留给上层 / 下层 gate:

| 漏掉的回归 | 由谁 catch |
|---|---|
| 模型响应内容正确性 | E2E_TESTING + fake driver 单测 |
| 协议 handshake 字段拼写错误 | 单测里的 fake driver 测试各 bridge 的事件流 |
| 用户态 config.yaml 解析错误 | `internal/config` 包的单元测试 |
| Feishu channel 双向消息 | E2E_TESTING.md |
| `runtime.GOOS` 漏在 call site(不是 init) | 单元测试 + vet 规则(未来可加) |
| npm 装出来的 binary 版本太老,缺关键 flag | 单测不依赖版本;CI gate 也不验版本。要 catch 加 `--version` 比对(超出本文范围) |
| agent binary 在某 OS 上是 i386 缺库 | Gate 2 的 `--help` 会失败(启动崩),间接 catch |

最关键的最后一项:Gate 4 poll size 时如果 agent 装好但某 required native lib 缺失,binary 会在 banner 之后**写一行 "error: libfoo.so missing"** 到 stderr → stderr 拼到 stdout 后总 size > 150 → **pass**(因为我们不 grep 内容)。这是已知限制:这层 gate 假定 binary 至少有基本的 platform 兼容。

---

## 9. 未来扩展

几个明确的增强方向,留给后续 PR:

1. **拆 agent-smoke 成 3 个独立 job** —— 每个 OS 一份 needs 链,消掉 §5 提到的 ~6 min 闲置
2. **加 `--version` 比对** —— 在 Gate 2 之后断言每个 agent >= 已知最低版本,捕"装出来的 binary 缺关键 flag"的回归
3. **响应内容 sanity-check** —— 不断言内容,但断言"响应里至少有一个换行"或"响应不是只 echo 我发的那一行" —— 进一步区分 PTY echo-only 和真 acknowledge
4. **加 matrix entry:每个 agent 单独跑**(现在 5 agents 串行)—— 大幅减少 wall-clock 但增加 setup 时间,trade-off 不明显
5. **本地一键脚本** —— §6.1 那个 bash 块整理成 `scripts/smoke-agents.sh` 提交到 repo,方便开发者本地预检

不在 scope(留给其他 doc):

- 单 bridge 的协议细节 → `docs/bridge/<name>.md`
- Windows 特定陷阱 → `docs/WINDOWS.md`
- Feishu 端到端 → `docs/E2E_TESTING.md`