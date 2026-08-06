# F-50: GitProvider 抽象 + 两阶段 Provider 探测

> **Status**: 🟡 设计阶段（doc-first；代码在 F-50 PR 落地）
> **Milestone**: v1.3.x
> **Scope**:
> - `internal/gtw/platform.go` — 重命名为 `provider.go`；类型 `PlatformClient` → `GitProvider`、`PlatformKind` → `ProviderKind`、`GitHubClient` → `GitHubProvider`、`GitLabClient` → `GitLabProvider`、`ErrUnsupportedPlatform` → `ErrUnsupportedProvider`、`NewPlatformClient` → `NewProvider`、`DetectPlatform` → `Detect`
> - `internal/gtw/provider.go` (NEW) — `HTTPProber` 接口 + `ExecHTTPProber` 实现
> - `internal/gtw/fix.go::RunFix` + `internal/gtw/rebuild.go::RebuildContext` — 调用 `Detect(ctx, remoteURL, prober)` 而非 `DetectPlatform(remoteURL)`
> - `internal/gtw/api.go::HandlerDeps` — `NewPlatform func(PlatformKind) (PlatformClient, error)` 字段改为 `Prober HTTPProber`
> - 全部调用方：`cmd/nightme/run.go` / `cmd/nightme/debug.go` / `internal/gateway/handlers_gtw*.go` 字段同步
> - 测试：`internal/gtw/gtw_test.go` 加两阶段探测测试 + `fakeHTTPProber` fixture
> - 文档同步：`FEATURES.md` 加 F-50 行；修复 `F-45 §3.5` / `gtw §5.x` / `F-45 §7.2` 全部悬空引用
>
> **Depends on**: 无（纯抽象 + 网络探测；不依赖 F-45 / F-46 / F-49）
> **Related**: [`SPEC.md`](../SPEC.md) §2.2（gtw 流程概念图）+ [`internal/gtw/platform.go`](../../internal/gtw/platform.go)（旧实现，作为本 PR 的 diff 基线）；本设计文档是所有 `F-45 §3.5` / `F-45 §7.2` / `gtw §5.x` 悬空引用的归宿（合并到 §6 引用图）

---

## 0. 背景

### 0.1 v1.3.x 现状

nightme v1.3.x 已经把 `/gtw fix <id>` 跑通，但 Git Provider 的接入面有几个一致性 / 可扩展性问题：

1. **抽象层命名误导**：`PlatformClient` / `PlatformKind` 把 GitHub / GitLab 称作「platform」，跟 IM 平台（Feishu / Slack / Web）的「channel / platform」用词打架。chat 文档里 `OutboundMessage` 走 `Channel.Send`，gtw 走 `PlatformClient.GetIssue`——两套名词体系并存，新人 onboarding 容易混淆。
2. **Provider 检测只覆盖官网**：v1.3.x 的 `DetectPlatform` 是纯字符串匹配（`strings.Contains(url, "github.com")` / `strings.Contains(url, "gitlab")`）。GitHub Enterprise（`https://github.acme.internal/foo/bar`）和自建 GitLab（`https://gitlab.acme.internal/group/foo/bar`）虽然 URL 命中 `"gitlab"` 子串能命中，但 GitHub Enterprise 的 host 不是 `github.com` —— **完全探测不到**。
3. **缺 server version 探测**：v1.3.x 拿到 provider 后只能调 `gh` / `glab` CLI；server 自身的 version（如 GitLab 16.5.0 / GitHub Enterprise 3.13）从未探测。后续如果要按 version 走 capability 路由（API v3 vs v4、issue 字段差异），无 metadata 可用。
4. **`CLIRunner` 抽象够，但 HTTP 探测没有对应抽象**：`gh` / `glab` 调用走 `CLIRunner` 接口（生产 `ExecCLIRunner` + 测试 fake），但 Detect 流程如果引入 HTTP 探测（Stage B），需要平行的 `HTTPProber` 抽象，否则测试无法 mock 网络行为。

### 0.2 设计目标

1. **抽象层改名**：`Platform*` 全部改成 `Provider*`，消除跟 IM 平台用词的混淆。
2. **两阶段探测**：URL 字符串命中官网 → 零网络直返；URL 含糊 → 主动探测 API 端点（GitLab `/api/v4/version` + GitHub Enterprise `/api/v3/meta`）。
3. **Version 暴露**：每个 provider 实例缓存探测到的 server version，`Version() string` 可读（供后续 capability 路由用）。
4. **HTTP 抽象对齐 `CLIRunner` 模式**：新增 `HTTPProber` 接口 + `ExecHTTPProber` 实现，测试时塞 `fakeHTTPProber` 直接喂 fixture JSON。
5. **错误语义统一**：`ErrUnsupportedProvider` 是探测失败的唯一 sentinel；用户提示带 host URL 便于排错。
6. **悬空引用一次性清理**：`F-45 §3.5` / `gtw §5.x` / `F-45 §7.2` 全部指向本设计文档的对应章节。

---

## 1. 设计

### 1.1 抽象层：`PlatformClient` → `GitProvider`

#### 1.1.1 接口

```go
// internal/gtw/provider.go (renamed from platform.go)

// GitProvider is the abstract /gtw interface to a git hosting
// platform's issue tracker. Production has two implementations:
// GitHubProvider (wraps the `gh` CLI) and GitLabProvider (wraps
// the `glab` CLI). Tests inject fakes; future hosts (Gitea,
// Bitbucket) plug in by satisfying the same interface.
//
// All methods take a ctx for cancellation / deadline propagation
// from the gtw.RunFix or gtw.RebuildContext caller.
type GitProvider interface {
    // Kind returns the provider identity — ProviderGitHub or
    // ProviderGitLab. Cheap; safe to call on every call site.
    Kind() ProviderKind

    // Host returns the server host this provider is bound to
    // (e.g. "github.com" / "gitlab.acme.internal"). Set by
    // Detect from the parsed remote URL.
    Host() string

    // Version returns the server-reported version string
    // (e.g. "16.5.0" for GitLab), or "" if the probe failed or
    // the provider has no version endpoint (e.g. github.com).
    // Cached on the provider instance after Detect probes once.
    Version() string

    // GetIssue fetches the issue with the given id. Returns
    // ErrIssueNotFound when the platform responds 404.
    GetIssue(ctx context.Context, owner, repo string, id int) (*Issue, error)

    // AddLabel adds `label` to the issue. Idempotent — adding
    // an already-present label is a no-op.
    AddLabel(ctx context.Context, owner, repo string, id int, label string) error

    // RemoveLabel removes `label` from the issue. Idempotent.
    RemoveLabel(ctx context.Context, owner, repo string, id int, label string) error
}
```

#### 1.1.2 Rename mapping

| 旧（v1.3.x） | 新（F-50） | 说明 |
|---|---|---|
| `PlatformClient` | `GitProvider` | 接口 |
| `PlatformKind` | `ProviderKind` | 类型 alias |
| `PlatformGitHub` | `ProviderGitHub` | enum 常量 |
| `PlatformGitLab` | `ProviderGitLab` | enum 常量 |
| `GitHubClient` | `GitHubProvider` | struct + 文件建议改名 `github_provider.go` |
| `GitLabClient` | `GitLabProvider` | struct + 文件建议改名 `gitlab_provider.go` |
| `NewPlatformClient(kind)` | `NewProvider(kind, host)` | 工厂；新签名多收 host 参数 |
| `DetectPlatform(url)` | `Detect(ctx, url, prober)` | 入口；新签名带 ctx + prober |
| `ErrUnsupportedPlatform` | `ErrUnsupportedProvider` | sentinel error |
| `Issue` | `Issue`（不变） | 不绑 provider 概念 |
| `CLIRunner` | `CLIRunner`（不变） | gh/glab CLI 抽象 |
| `ExecCLIRunner` | `ExecCLIRunner`（不变） | 生产 CLI runner |

### 1.2 两阶段探测

#### 1.2.1 整体流程

```
RunFix / RebuildContext 拿到 remoteURL
       │
       ▼
parseRemoteHost(remoteURL)                    ── 抽 host（全 URL 形态矩阵见 §1.2.3）
       │
       ├─ URL 解析失败 → return ErrInvalidRemoteURL  (不再 wrap 为 ErrUnsupportedProvider；见 §1.2.2 D3 split)
       │
       ▼
Detect(ctx, remoteURL, prober)
   │
   ├─ Stage A · URL hint（零网络）
   │     ├─ substring "github.com"  → ProviderGitHub  (host = remoteURL's host)
   │     ├─ substring "gitlab"      → ProviderGitLab  (host = remoteURL's host)
   │     └─ otherwise               → fall through to Stage B
   │
   ├─ Stage B · Live API probe（仅 Stage A 含糊时）
   │     ├─ GET <host>/api/v4/version
   │     │     ├─ 200 + JSON {"version": "16.5.0", ...}  → ProviderGitLab + version cached
   │     │     └─ 任何失败（404 / 403 / TLS / DNS / timeout）→ continue to next probe
   │     │
   │     └─ GET <host>/api/v3/meta
   │           ├─ 200 + JSON 含 "verifiable_password_authentication" 字段
   │           │                                                  → ProviderGitHub + version "" (GHE 无公开 version)
   │           └─ 任何失败 → return ErrUnsupportedProvider
   │
   └─ 所有路径都失败 → return ErrUnsupportedProvider
```

**关键不变量**：
- Stage A 命中时**零网络**、**零延迟**、**永不失败**（纯字符串 contains）。即使是 GitHub.com / GitLab.com 也能被 Stage B 识别，但 Stage A 优先返回避免任何 round-trip。
- Stage B 探测有 3s default deadline（`ExecHTTPProber.Timeout`）；超时 / TLS 错 / 自签证书 → 视为「探测失败」继续下一次，不立即 return。
- Stage B 的两次探测**顺序固定**：先 GitLab（`/api/v4/version` 必返 JSON + version），再 GitHub（`/api/v3/meta` 无 version 字段但响应形态唯一）。如果 GitLab 端点恰好在 GitHub 上返回了 200 + 类似 JSON（理论极小概率），GitLab 会被误判 —— 接受这个风险，因为 GitHub.com / GitHub Enterprise 的 `/api/v3/meta` 才是合法路径，Stage A 早已命中。
- **`parseRemoteHost` 必须正确**（§1.2.3）：Stage B 的探测目标完全由它提取的 host 决定。host 错了 = 探测打到错的 server = 探测失败 / 误判。
- **`ExecHTTPProber` 是 pointer receiver**：避免 `(*ExecHTTPProber)(nil)` 通过接口传值时 panic。Probe 第一行 `if p == nil { p = &ExecHTTPProber{} }` 自动 guard。

#### 1.2.2 失败语义

`Detect` 返回的错误分两类，对应不同的 sentinel + 用户提示：

| 触发条件 | sentinel | 用户提示 |
|---|---|---|
| `parseRemoteHost` 失败（URL 为空 / 缺协议 / 不能 lex 出 host） | `ErrInvalidRemoteURL`（**不** wrap `ErrUnsupportedProvider`） | ❌ 无效的 remote URL: "..." + 期望格式示例 |
| remoteURL 没 origin（更上层的 preflight） | （既有逻辑） | ❌ 无 `origin` remote。Add with `git remote add origin <url>` |
| Stage A 命中 github.com / gitlab | `nil` | （直接走主流程） |
| Stage A 含糊，Stage B 两次都失败 | `ErrUnsupportedProvider`（wrapped 带 host） | ❌ 暂不支持的 Git 平台 (host: X — neither github.com/gitlab.com URL hint nor /api/v3/meta or /api/v4/version probe recognised it) |
| Stage B 探测到 GitLab 但无 version 字段 | `nil` | （直接走主流程；Version() 返空字符串） |
| Stage B 探测到 GitHub Enterprise | `nil` | （直接走主流程；Version() 返空字符串） |

**D3 split 关键不变量**：
- `errors.Is(err, ErrInvalidRemoteURL)` 命中 → URL 自身语法错（用户可修）
- `errors.Is(err, ErrUnsupportedProvider)` 命中 → 平台未实现（用户无能为力，等 F-XX）
- 两个 sentinel **互不 wrap** —— 同一 err 不会同时 `Is` 两个

**fix.go 分流**（`internal/gtw/fix.go::RunFix`）：
```go
switch {
case errors.Is(err, ErrInvalidRemoteURL):
    return reply(..., "❌ 无效的 remote URL: %q\n  Expected: https://github.com/<owner>/<repo>.git, ...", ...)
default:  // ErrUnsupportedProvider 或 wrap
    return reply(..., "❌ 暂不支持的 Git 平台 (host: ...)", ...)
}
```

#### 1.2.3 parseRemoteHost — URL 形态矩阵

`parseRemoteHost` 是 Stage A 和 Stage B 共用的 host 抽取函数。**必须**正确处理 Git 在野外出现的所有 URL 形态，否则 Stage B 探测打到错的 server：

| 输入 | 抽出 host | 备注 |
|---|---|---|
| `https://github.com/foo/bar.git` | `github.com` | 标准 HTTPS |
| `http://github.com/foo/bar` | `github.com` | HTTP（罕见） |
| `ssh://git@github.com/foo/bar.git` | `github.com` | ssh:// 显式协议 |
| `ssh://git@github.com:2222/foo/bar.git` | `github.com:2222` | ssh:// + 自定义端口（host 保留端口） |
| `git@github.com:foo/bar.git` | `github.com` | scp-style legacy SSH（无协议前缀，`:` 是 host/path 分隔符） |
| `git://github.com/foo/bar.git` | `github.com` | 罕见但合法的 Git 协议 |
| `https://ghp_xxx@github.com/foo/bar.git` | `github.com` | userinfo 含 PAT（gh/glab auth helper 注入） |
| `https://oauth2:secret@gitlab.acme.io/foo/bar.git` | `gitlab.acme.io` | userinfo 含 username + password |
| `https://gitlab.acme.internal:8929/foo/bar.git` | `gitlab.acme.internal:8929` | 自建 GitLab 自定义端口 |
| `https://github.com/foo/bar.git?ref=main` | `github.com` | query 字符串剥除 |
| `https://github.com/foo/bar.git#readme` | `github.com` | fragment 剥除 |
| `  https://github.com/foo/bar.git\n` | `github.com` | whitespace tolerance |
| `""` / `"   "` | `ErrInvalidRemoteURL` | 空串 |
| `"github.com/foo/bar"` | `ErrInvalidRemoteURL` | 无协议前缀 — 拒绝（防止误探用户笔误的裸 hostname） |

**关键 lex 规则**（按顺序）：
1. **协议前缀**：识别 5 种 — `https://` / `http://` / `ssh://` / `git://` / scp-style `git@`。其他形态（裸 hostname）直接 `ErrInvalidRemoteURL`。
2. **userinfo 剥除**：第一个 `@` 之前如果是 userinfo（后面没有 `/`，或 `@` 在 `/` 之前），剥掉。**这一步必须在 colon-to-slash 之前** —— 否则 user:token 中的 colon 会被错误地当成 scp-style 分隔符。
3. **query / fragment 剥除**：第一个 `?` 或 `#` 之后的内容丢弃。
4. **`.git` 后缀剥除**。
5. **scp-style vs port 歧义**：第一个 `:` 后跟的内容如果像 TCP 端口（digits + `/`/`?`/`#`/EOL）→ **保留** colon（port）。否则 → 替换为 `/`（scp-style host/path 分隔符）。
6. **`SplitN("/", 2)`** 拿第一段作为 host。

**port 判定 helper**（避免正则）：
```go
func isPort(s string) bool {
    // digits followed by ("/" | "?" | "#" | EOL)
    i := 0
    for i < len(s) && s[i] >= '0' && s[i] <= '9' { i++ }
    if i == 0 { return false }
    if i == len(s) { return true }
    switch s[i] { case '/', '?', '#': return true }
    return false
}
```

测试覆盖：`TestParseRemoteHost`（16 个 case，含上述全矩阵）。

### 1.3 HTTPProber 抽象

```go
// internal/gtw/provider.go

// HTTPProber abstracts the HTTP client used to probe provider
// version / meta endpoints. Same pattern as CLIRunner / GitRunner:
// production uses ExecHTTPProber; tests inject a fake that returns
// canned JSON for fixture-driven unit tests.
type HTTPProber interface {
    // Probe issues a GET <host><path> with a 3s default timeout
    // and returns the response body on 200. Any non-200 status,
    // network error, TLS error, or timeout returns an error
    // (caller decides what to do — typically "treat as probe
    // failure, try next endpoint").
    Probe(ctx context.Context, host, path string) ([]byte, error)
}

// ExecHTTPProber is the production HTTPProber.
type ExecHTTPProber struct {
    // Timeout bounds the entire Probe call. Zero defaults to
    // 3s — long enough for a healthy GitLab / GitHub Enterprise
    // response, short enough that stalled servers don't block
    // the /gtw message path.
    Timeout time.Duration

    // InsecureSkipVerify disables TLS verification. Defaults to
    // false. Self-hosted GitHub Enterprise / GitLab with self-
    // signed certs: users can set this via the prober at
    // construction (no env var / config wired yet — see §6 backlog).
    InsecureSkipVerify bool
}

func (p ExecHTTPProber) Probe(ctx context.Context, host, path string) ([]byte, error) {
    if p.Timeout == 0 {
        p.Timeout = 3 * time.Second
    }
    url := "https://" + host + path
    req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
    if err != nil {
        return nil, err
    }
    req.Header.Set("Accept", "application/json")
    req.Header.Set("User-Agent", "nightme-git-provider-detect/1.0")
    cli := &http.Client{
        Timeout: p.Timeout,
        // Note: Transport kept nil so http.DefaultTransport is
        // used; future self-hosted cert handling plugs in here.
    }
    resp, err := cli.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    if resp.StatusCode != 200 {
        return nil, fmt.Errorf("probe %s: status %d", url, resp.StatusCode)
    }
    return io.ReadAll(resp.Body)
}
```

### 1.4 HandlerDeps 字段变化

```go
// internal/gtw/api.go

type HandlerDeps struct {
    Now      func() time.Time
    Send     SendFunc
    SendCard SendCardFunc
    Git      GitRunner
    // F-50: replaced NewPlatform func(PlatformKind) (PlatformClient, error)
    //         with two injection points (Prober + Detect). Prober feeds
    //         the Stage B API probe; Detect is the full entry point that
    //         tests can override to skip URL hint + probe entirely and
    //         return a fakeProvider directly. nil → package-level Detect
    //         with ExecHTTPProber{} default.
    Prober HTTPProber
    Detect func(ctx context.Context, remoteURL string, prober HTTPProber) (GitProvider, error)
}
```

> **设计落地调整**：原 §1.4 草稿只加了 `Prober` 一个字段。实际实现时
> 发现测试需要一个「直接返回 fake provider」的注入点（跳过 Stage A
> short-circuit 也跳过 Stage B probe），否则用 `github.com` URL 的
> happy path 测试会调到真实的 `gh issue edit`。新增 `Detect` 函数字段
> 解决这个问题，跟 `Send` / `SendCard` 同一套 callable field 模式。

**调用顺序简化**（对比 v1.3.x）：

```
v1.3.x:                                          F-50:
  DetectPlatform(url) → PlatformKind               Detect(ctx, url, prober) → GitProvider
  deps.NewPlatform(kind) → PlatformClient            (内部已 bind host + version)
  使用 platform.GetIssue(...)                       使用 provider.GetIssue(...)
```

少一次显式工厂调用 —— Detect 已经把 host / version 都绑好。

---

## 2. 文件 & 接口

### 2.1 `internal/gtw/platform.go` → `provider.go`

**改动**：
- 文件重命名（git mv）
- 所有类型 / 函数按 §1.1.2 表重命名
- 新增 `HTTPProber` 接口 + `ExecHTTPProner` 实现
- 新增 `Detect(ctx, remoteURL, prober)` 函数
- 保留 `ParseRepoOwner` / `RemoteOriginURL` / `RepoRoot` / `Issue` / `CLIRunner` / `ExecCLIRunner`（与 provider 无关）

**新签名**：

```go
// Detect identifies the git provider for the given remote URL.
//
// Two-stage detection (see §1.2):
//   Stage A · URL hint (zero network) — substring "github.com" / "gitlab"
//   Stage B · Live API probe (only when Stage A ambiguous):
//     GET <host>/api/v4/version → GitLab (with version cached)
//     GET <host>/api/v3/meta    → GitHub Enterprise (no version)
//
// Returns ErrUnsupportedProvider when neither stage recognises
// the host. The returned GitProvider is already bound to the host
// (call provider.Host() to inspect).
func Detect(ctx context.Context, remoteURL string, prober HTTPProber) (GitProvider, error)
```

### 2.2 `internal/gtw/fix.go::RunFix`

**改动**：
- 删除 `DetectPlatform(remoteURL)` 调用
- 替换为 `Detect(ctx, remoteURL, deps.Prober)`
- **D3 分流**：user-facing 消息按 `errors.Is(err, ErrInvalidRemoteURL)` vs `ErrUnsupportedProvider` 区分（§1.2.2）

```go
// before
platformKind, err := DetectPlatform(remoteURL)
if err != nil { ... "only github.com / gitlab.com are supported" ... }
platform, err := deps.NewPlatform(platformKind)

// after
detect := deps.Detect
if detect == nil { detect = Detect }
prober := deps.Prober
if prober == nil { prober = &ExecHTTPProber{} }
platform, err := detect(ctx, remoteURL, prober)
if err != nil {
    switch {
    case errors.Is(err, ErrInvalidRemoteURL):
        return reply(ctx, deps.Send, chatID, messageID,
            fmt.Sprintf("❌ 无效的 remote URL: %q\n  Expected: https://...", remoteURL)), nil
    default:  // ErrUnsupportedProvider 或 wrap
        return reply(ctx, deps.Send, chatID, messageID,
            fmt.Sprintf("❌ 暂不支持的 Git 平台 (host: %s — ...)", remoteURL)), nil
    }
}
```

### 2.3 `internal/gtw/rebuild.go::RebuildContext`

平行改造：`DetectPlatform(remoteURL)` → `Detect(ctx, remoteURL, nil)`（daemon recovery 不在乎 3s 探测延迟，但保持同一入口便于测试 mock）。

### 2.4 `internal/gtw/api.go::HandlerDeps`

字段 `NewPlatform func(PlatformKind) (PlatformClient, error)` 删除；新增 `Prober HTTPProber`。

### 2.5 `internal/gateway/handlers_gtw.go` / `handlers_gtw_debug.go`

`deps.NewPlatform = gtw.NewPlatformClient` 改为 `deps.Prober = nil`（生产用 ExecHTTPProber 默认值）。或保留 `deps.Prober` 字段注入用于测试。

### 2.6 `cmd/nightme/run.go` / `debug.go`

`gtw.HandlerDeps{NewPlatform: gtw.NewPlatformClient}` 改为 `gtw.HandlerDeps{Prober: &gtw.ExecHTTPProber{}}`（pointer receiver；§1.2.1 nil-safe guard 要求传 pointer）。

### 2.7 `internal/gtw/action.go::executeXxxAction`（rollback 路径）

取消 / worktree-fail 路径需要从 `FixDraftPayload.Platform` (string) 重建一个 provider 来 `RemoveLabel`。变更：

```go
// before
plat, platErr := deps.NewPlatform(PlatformKind(p.Platform))

// after
plat, platErr := NewProvider(ProviderKind(p.Platform), "")
// host 为空 OK：RemoveLabel 不需要 host，只要 `gh/glab --repo owner/repo` 就够
```

---

## 3. 自建实例探测矩阵

### 3.1 GitLab

| 部署形态 | host 例子 | Stage A | Stage B `/api/v4/version` | 结果 |
|---|---|---|---|---|
| SaaS (https) | `https://gitlab.com/group/foo.git` | ✅ 命中 `"gitlab"` | (短路) | `GitLabProvider{host: "gitlab.com", version: ""}` |
| SaaS (ssh://) | `ssh://git@gitlab.com/group/foo.git` | ✅ | (短路) | 同上 |
| SaaS (scp-style) | `git@gitlab.com:group/foo.git` | ✅ | (短路) | 同上 |
| SaaS (git://) | `git://gitlab.com/group/foo.git` | ✅ | (短路) | 同上 |
| 自建（标准）| `gitlab.acme.internal` | ❌ 含糊 | ✅ 200 + `{"version":"16.5.0"}` | `GitLabProvider{host: "gitlab.acme.internal", version: "16.5.0"}` |
| 自建（子路径）| `gl.acme.com/git` | ❌ 含糊 | ✅ 200（同上） | `GitLabProvider{host: "gl.acme.com", version: "16.5.0"}` |
| 自建（端口）| `gl.acme.com:8929` | ❌ 含糊 | ✅ 200（同上） | `GitLabProvider{host: "gl.acme.com:8929", version: "..."}` |
| 自建（userinfo = PAT）| `https://oauth2:secret@gitlab.acme.io/foo/bar.git` | ❌ 含糊 | ✅ 200（同上） | `GitLabProvider{host: "gitlab.acme.io", version: "16.5.0"}` |
| 自建（ssh:// + 端口）| `ssh://git@gitlab.acme.io:2222/foo/bar.git` | ❌ 含糊 | ✅ 200（同上） | `GitLabProvider{host: "gitlab.acme.io:2222", version: "16.5.0"}` |
| 自建（旧版 < 12.0 无 version 字段）| 同上 | ❌ 含糊 | ⚠️ 200 但无 version | `GitLabProvider{host: H, version: ""}`（仍识别为 GitLab） |
| 自建（端口 firewall 拦截）| 同上 | ❌ 含糊 | � timeout / connection refused | 继续 Stage B 二次探测 → 仍失败 → `ErrUnsupportedProvider` |

**Stage B 识别信号**：响应 body 是合法 JSON + 顶层含 `version` **或** `revision` 字段（12.0+）。**或**任何 200 + JSON 含 `name` 字段（早期版本）。

**parseRemoteHost 边界条件**（完整矩阵见 §1.2.3）：端口保留、userinfo 剥除、scp-style vs port colon 歧义由 `isPort()` 解决。

### 3.2 GitHub

| 部署形态 | host 例子 | Stage A | Stage B `/api/v3/meta` | 结果 |
|---|---|---|---|---|
| SaaS (https) | `https://github.com/owner/repo.git` | ✅ 命中 `"github.com"` | (短路) | `GitHubProvider{host: "github.com", version: ""}` |
| SaaS (ssh://) | `ssh://git@github.com/owner/repo.git` | ✅ | (短路) | 同上 |
| SaaS (scp-style) | `git@github.com:owner/repo.git` | ✅ | (短路) | 同上 |
| SaaS (git://) | `git://github.com/owner/repo.git` | ✅ | (短路) | 同上 |
| Enterprise（标准）| `https://github.acme.internal/foo/bar.git` | ❌ 含糊 | ✅ 200 + JSON 含 `verifiable_password_authentication` | `GitHubProvider{host: "github.acme.internal", version: ""}` |
| Enterprise（端口）| `https://github.acme.internal:8443/foo/bar.git` | ❌ 含糊 | ✅ 200（同上） | `GitHubProvider{host: "github.acme.internal:8443", version: ""}` |
| Enterprise（userinfo）| `https://ghp_xxx@github.acme.internal/foo/bar.git` | ❌ 含糊 | ✅ 200（同上） | `GitHubProvider{host: "github.acme.internal", version: ""}` |
| Enterprise（自签证书）| 同上 | ❌ 含糊 | ❌ TLS handshake failed | 失败 → `ErrUnsupportedProvider`（除非用户配 `InsecureSkipVerify=true`） |
| Data Residency | `*.ghe.com` | ❌ 含糊（substring 不含 "github.com"） | ✅ 200 + GHE meta | `GitHubProvider{host: "acme.ghe.com", version: ""}` |

### 3.3 真正不支持的（v1.3.x 现状 → v1.4 留 F-XX）

| 平台 | v1.3.x 行为 | F-50 行为 | 后续 |
|---|---|---|---|
| Bitbucket | `ErrUnsupportedProvider` | 同上 | F-XX 添加 BitbucketProvider（实现 `GitProvider` 接口） |
| Gitea | `ErrUnsupportedProvider` | 同上 | F-XX 添加 GiteaProvider |
| Azure DevOps | `ErrUnsupportedProvider` | 同上 | F-XX 添加 AzDOProvider |
| SourceHut | `ErrUnsupportedProvider` | 同上 | F-XX |

---

## 4. 不变式

### 4.1 抽象层不变式

- **`GitProvider` 接口 100% typed**：不引入 platform-specific 字段；`Issue` 是统一的最小形态（`ID` / `Title` / `Body` / `State` / `Labels` / `URL`），所有 provider 实现都翻译到这个 shape。
- **`Host() string` 永远是 bound 的**：Detect 阶段就把 host 绑到 provider 实例上，调用方不需要重新解析。
- **`Version() string` 是 best-effort**：GitLab 通常有；GitHub Enterprise 没有公开 version 端点；空字符串合法。
- **测试注入点完整**：`GitProvider` / `HTTPProber` / `CLIRunner` / `GitRunner` 四个接口都有对应的 fake 实现（`fakeProvider` / `fakeHTTPProber` / `fakeCLIRunner` / `fakeGitRunner`）。

### 4.2 探测不变式

- **Stage A 优先**：URL 命中立即返回，零网络。Stage B 只在 Stage A 含糊时启动。
- **Stage B 顺序固定**：GitLab 先于 GitHub（GitLab 端点响应更便宜 / 更确定）。
- **Stage B 单次探测失败不立即 return**：超时 / TLS / 404 都视为「这一端点不是它」，继续下一个。**只有两次都失败才返 `ErrUnsupportedProvider`**。
- **探测 timeout = 3s default**：足够健康 server 响应；stalled server 不阻塞消息路径。

### 4.3 不变量（与现有 SPEC 一致）

- **§1.4 抽象 / 具体 边界规范**：`GitProvider` 是抽象，channel 不感知；gtw 不感知 provider 实现细节（除了 `Kind()` 用于 telemetry / 日志）。
- **§1.3 Channel 不 import gtw / chatsession**：Provider 抽象属于 gtw 包；Channel 通过 `gtw.SendFunc` 间接使用。
- **bridge 协议零变化**：bridges 仍发 `EventInit` / `EventUsage` / ...;runtime 翻译；Provider 探测完全是 runtime 行为。
- **nightme 不持久化 token**：gh / glab 的 auth 委托给 `gh auth login` / `glab auth login`（已有约定，见 `internal/gtw/api.go:17`）。

---

## 5. 测试

### 5.1 单元测试（`internal/gtw/gtw_test.go`）

| 测试 | 覆盖 |
|---|---|
| `TestParseRemoteHost` (16 case) | URL 形态全矩阵：https / http / ssh:// / scp / git:// / 自建 / userinfo / port / query / fragment / whitespace / empty / no-scheme |
| `TestDetect_URLHint` (4 case) | git@github.com:foo / https://github.com/foo / git@gitlab.com:foo / 自建 GitLab 含 "gitlab" |
| `TestDetect_URLHint_GitProtocol` | `git://github.com/foo/bar.git` → Stage A 命中 |
| `TestDetect_StageB_GitLabVersion` | URL 含糊 + fakeHTTPProber 返 `/api/v4/version` JSON → GitLabProvider{version:"16.5.0"}；callOrder 只 1 条 |
| `TestDetect_StageB_GitHubEnterpriseMeta` | URL 含糊 + `/api/v4/version` 404 + `/api/v3/meta` 返 meta → GitHubProvider；callOrder 2 条 |
| `TestDetect_StageB_BothFail` | URL 含糊 + fakeHTTPProber 全失败 → ErrUnsupportedProvider；callOrder 2 条 |
| `TestDetect_StageA_ShortCircuits` | github.com URL + prober 即使能返 → 不调 Stage B；callOrder 0 条 |
| `TestDetect_InvalidURL_ReturnsInvalidRemoteURL` | 空 URL / 无 scheme → ErrInvalidRemoteURL（**不** wrap ErrUnsupportedProvider；D3 split） |
| `TestDetect_StageA_PathologicalHosts` (4 case) | trailing-slash / 无 .git / deep group / URL 嵌 PAT |
| `TestDetect_NilProber_UsesExecHTTPProber` | nil prober + Stage A 命中 → 不 hang（隐式断言） |
| `TestNewProvider_KindHostBinding` | `NewProvider(kind, host)` 各 kind 正确；未知 kind → ErrUnsupportedProvider |
| `TestExecHTTPProber_NilPointer_Guarded` | `var p *ExecHTTPProber = nil; p.Probe(...)` 不 panic（B2） |
| `TestExecHTTPProber_End2End` (3 case) | httptest.NewTLSServer：200 happy / 503 error / timeout error（InsecureSkipVerify=true 接自签证书） |

### 5.2 集成测试（`internal/gtw/gtw_test.go` EXTEND）

| 测试 | 覆盖 |
|---|---|
| `TestRunFix_SelfHostedGitLab_End2End` | mock fakeGitRunner 返 `origin = git@gitlab.acme.internal:group/foo.git` + fakeHTTPProber 返 version JSON + fakeCLIRunner 模拟 `glab issue view` → RunFix 全流程跑通 |
| `TestRunFix_SelfHostedGitHubEnterprise_End2End` | 平行测 GHE |
| `TestRunFix_UnsupportedHost` | remote URL = `git@bitbucket.org:foo/bar` + fakeHTTPProber 全失败 → RunFix 返 ❌ 暂不支持的 Git 平台 reply |

### 5.3 边界测试

| 场景 | 期望 |
|---|---|
| remoteURL 空字符串 | Detect 返 `("", ErrUnsupportedProvider)`（与 v1.3.x 一致） |
| remoteURL 带 query string / fragment | Detect 正确剥 query / fragment，host 解析正确 |
| remoteURL 带端口（`git@gitlab.acme.com:8929:foo/bar`）| Git 协议 SSH 端口在 path 里；Detect 只看 host 部分，正确识别 |
| remoteURL 带 userinfo（`https://token@github.com/foo/bar.git`）| Detect 剥 userinfo 再判 |
| fakeHTTPProber 返回 malformed JSON | Stage B 失败 → 继续下一个 probe |
| fakeHTTPProber 返回 503 | 同上（视为探测失败） |

---

## 6. 引用图：本设计文档的归宿

### 6.1 本 F-50 是以下悬空引用的归宿

| 旧引用 | 新引用 | 备注 |
|---|---|---|
| `F-45 §3.5`（reaction/action 路由设计） | [`internal/gateway/gateway.go::dispatchAction`](../../internal/gateway/gateway.go) + [`internal/chatsession/chatsession.go::HandleAction`](../../internal/chatsession/chatsession.go) + [`internal/gtw/action.go::HandleAction`](../../internal/gtw/action.go) | §3.5 是 reaction 路由设计，不属于 Provider 抽象 → 指向代码 |
| `F-45 §7.2`（Provider 检测设计） | F-50 §1.2 + §3 | 本设计文档拥有 |
| `gtw §5.3.1 / §5.3.3`（decision card 场景） | [`F-46-interactive-cards.md`](./F-46-interactive-cards.md) §3.3 | 决策卡是 F-46 内容，不属于 Provider 抽象 |

**为什么不直接给 §3.5 写一篇 F-XX 文档**：reaction / action 路由是 gtw 流程的一部分但跟 Provider 抽象解耦——它只关心「用户的 emoji / button 怎么映射到 gtw 决策执行」。单独成文档需要重写大量现有 F-46 §1.1 / §3.5 内容，性价比不高。本 F-50 §6.1 表格直接指向代码位置作为「canonical source」。

### 6.2 本 F-50 是以下章节的 canonical write-up

| 受影响 | 当前 | 应改成 |
|---|---|---|
| `docs/SPEC.md` §2.5 / §3.5 / §2.6 | 提到 gtw 但未指定 Provider 抽象入口 | + cross-link `F-50 §1.1` |
| `docs/FEATURES.md` 第 41 行 F-45 | 只描述 SessionFooter | + 新行 `F-50 GitProvider 抽象 + 两阶段探测` |
| `docs/channel/feishu.md` §13.22 引用 `F-45 §13.22` | 仍正确（F-45-session-footer §13.22 真的存在） | 不变 |
| `docs/feat/F-25-rolling-log.md:54` | 引用 `gtw §5.3.1 / §5.3.3` | 改为 `F-46 §3.3` |

---

## 7. Migration & 兼容性

### 7.1 代码层

- **`PlatformClient` / `PlatformKind` / `GitHubClient` / `GitLabClient` 全部删除**（不是 deprecated）：F-50 是 v1.3.x 的演进，不是 v1.4 的新增 API。
- **`DetectPlatform` 删除**：被 `Detect(ctx, url, prober)` 取代。
- **`NewPlatformClient` 删除**：被 `NewProvider(kind, host)` 取代。
- **`HandlerDeps.NewPlatform` 删除**：被 `HandlerDeps.Prober` 取代。
- **`ErrUnsupportedPlatform` 删除**：被 `ErrUnsupportedProvider` 取代。

### 7.2 wire / storage 层

- **零 wire 变化**：`OutboundMessage` / `gtw.OutMsg` / ChatSession 持久化都不涉及 Provider 抽象。
- **零 config 变化**：没有 cfg 字段被本 PR 影响。

### 7.3 行为兼容性

- **`/gtw fix <id>` 在 github.com / gitlab.com 上的行为完全不变**（Stage A 命中即返回，跟 v1.3.x 等价）。
- **`/gtw fix <id>` 在自建 GitLab 上行为增强**：v1.3.x 也走 Stage A 命中（substring `"gitlab"`），F-50 多走一次 `/api/v4/version` 把 version 缓存到 provider（默认行为下行为可见差异：探测多 1 次 round-trip，可选关闭——见 §6 backlog）。
- **`/gtw fix <id>` 在自建 GitHub Enterprise 上从「探测不到」变成「探测得到」**：Stage B `/api/v3/meta` 主动识别。

---

## 8. 不在本 PR 范围

- **Bitbucket / Gitea / AzDO / SourceHut provider 实现**：F-XX 后置；本 PR 只保证它们的 host 走到 `ErrUnsupportedProvider` 时错误信息准确。
- **InsecureSkipVerify 配置入口**：当前 `ExecHTTPProber.InsecureSkipVerify` 字段是公开的，但**没有**cfg 字段 wire 到它。自建 GHE + 自签证书的用户需要手动 hack `HandlerDeps.Prober` 来打开；v1.4 加 `cfg.Provider.InsecureSkipVerify` 字段。
- **Capability-based routing**（按 version 走不同 API 路径）：version 字段已暴露但**不**消费；v1.4+ 按需要路由。
- **Cache 探测结果**：当前每次 `Detect` 调用都跑 Stage B（如果需要）。同一进程对同一 host 多次探测可考虑加 LRU cache（v1.4+）。

---

## 9. 实施计划

按 6 个独立 commit 顺序落地，每步可单独 revert：

1. **`docs(feat): F-50-git-provider.md design (canonical Provider abstraction write-up)`**
   - 新文件 `docs/feat/F-50-git-provider.md`
   - 修复 `F-45 §3.5` / `gtw §5.x` / `F-45 §7.2` 悬空引用（指向 F-50 / F-46 / 代码位置）
   - 更新 `docs/FEATURES.md` 加 F-50 行

2. **`refactor(gtw): rename Platform → Provider (interface + types + structs)`**
   - `internal/gtw/platform.go` → `internal/gtw/provider.go` (git mv)
   - 所有类型 / 函数按 §1.1.2 表重命名
   - `NewPlatformClient(kind)` → `NewProvider(kind, host)`
   - `ErrUnsupportedPlatform` → `ErrUnsupportedProvider`
   - 暂时**不**实现 Detect；保留 `DetectPlatform` 作过渡

3. **`feat(gtw): HTTPProber interface + ExecHTTPProber (3s default timeout)`**
   - 新接口 + 生产实现
   - `fakeHTTPProber` test fixture（fixtures: 200 GitLab version JSON / 200 GitHub meta JSON / 503 / timeout）

4. **`feat(gtw): Detect(ctx, url, prober) — two-stage URL hint + API probe fallback`**
   - 新 `Detect` 函数
   - `DetectPlatform` 删除（已被 Detect 替代）
   - 测试见 §5

5. **`refactor(gateway): HandlerDeps.NewPlatform → HandlerDeps.Prober`**
   - `internal/gtw/api.go::HandlerDeps` 字段调整
   - `internal/gateway/handlers_gtw*.go` + `cmd/nightme/run.go` + `debug.go` 同步
   - `internal/gtw/fix.go::RunFix` + `rebuild.go::RebuildContext` 改调 `Detect`

6. **`test(gtw): full Detect coverage + RunFix integration with fakeHTTPProber`**
   - §5.1 + §5.2 + §5.3 测试落地

---

## 10. 变更日志

| 日期 | 版本 | 变更 |
|---|---|---|
| 2026-08-06 | 草案 | 初稿；解决 v1.3.x「只匹配官网」「缺 server version」「抽象层命名歧义」三个一致性问题；一次性清理 `F-45 §3.5` / `gtw §5.x` / `F-45 §7.2` 全部悬空引用 |
| 2026-08-06 | 修订 | Code review 后补强：`parseRemoteHost` 增全 URL 形态支持（git:// / ssh:// / scp / userinfo / port / query / fragment / whitespace / no-scheme）；新增 §1.2.3 URL 形态矩阵 + §1.2.2 D3 split（`ErrInvalidRemoteURL` 与 `ErrUnsupportedProvider` 分离，user-facing 消息分流）；`ExecHTTPProber.Probe` 改 pointer receiver + nil-safe guard；provider struct 字段 private 化（C1 cleanup）；`HandlerDeps` 加 `Detect` 函数字段（测试注入 fakeProvider）；§3 自建实例探测矩阵扩列；§5.1 测试列表对齐实际实现（27 个新 case，race detector clean） |
| 2026-08-06 | 安全 | **Security review（/code-review —fix）后补强**：(1) `fix.go` 3 处 user-facing 错误信息**绝不** echo 原 `remoteURL`（PAT / oauth2:token 会泄露到 IM channel）；新增 `redactForDisplay()` 剥 userinfo + query + fragment + 256 rune 上限（`TestRedactForDisplay` 10 个 case，含 2 个 `forbid` 凭据子串断言）。(2) `RebuildContext` 接受 `prober` 参数，`RunFix` 把自己的 prober 透传过去——避免 Detect 内部再新建一份 `ExecHTTPProber` 导致 self-hosted 探测 latency 从 3s 翻到 6s。(3) `RunFix` 加 `platform == nil` 防御性 guard（自定义 `deps.Detect` 返 `(nil, nil)` 时不 panic）。(4) `worktree.go:73` + `run.go:320` 修两个 stale 引用 `internal/gtw/platform.go` → `provider.go` |
