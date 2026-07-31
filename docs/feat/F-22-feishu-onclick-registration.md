# F-22: Feishu One-Click App Registration (QR 扫码授权)

> **Status**: designed (v0.1)
> **Milestone**: M2 (跟 M2 一起实施)
> **Depends on**: F-08 (Channel)
> **Used by**: 用户 onboarding 流程
> **Related docs**: SPEC.md §1.1 (Channel), [F-08-channel-abstraction.md](./F-08-channel-abstraction.md), [F-20-gateway.md](./F-20-gateway.md)
> **官方文档**: https://open.feishu.cn/document/mcp_open_tools/integrating-agents-with-feishu/create-an-app-in-one-click-go

## 1. Description

Feishu 官方 SDK（`lark-oapi-go/v3`）提供 `registration.RegisterApp` 函数，实现 **OAuth 2.0 Device Authorization Grant (RFC 8628)**：

1. nightme 调 `RegisterApp` → 飞书返回 verification URL + device code
2. nightme 在终端显示 URL + 二维码
3. 用户用飞书 App 扫码（或打开 URL）→ 飞书显示授权确认页（含 nightme 请求的 scopes/events）
4. 用户点确认 → 飞书**自动创建 app** 并把 credentials 返给 nightme
5. nightme 把 `ClientID` (App ID) + `ClientSecret` 存到 config

**用户零手动**：不需要去飞书开放平台建 app，不需要复制 app_id/app_secret。

**实现来源**：lark-oapi-go v3 的 `scene/registration` 子包。

## 2. Interface

```go
// internal/auth/auth.go
package auth

// Provider is the interface for channel-specific auth flows.
// v0.1 only has Feishu; v0.2+ may add Lark (international) etc.
type Provider interface {
    // Name returns the channel name (e.g. "feishu", "lark").
    Name() string

    // Login performs the auth flow. Blocks until user completes
    // (or timeout). Returns the credentials for storing in config.
    Login(ctx context.Context) (*Credentials, error)
}

type Credentials struct {
    AppID     string  // = result.ClientID
    AppSecret string  // = result.ClientSecret
    TenantAccessToken string  // optional, for app-only API calls
    // Metadata from the registration:
    AppName   string
    CreatedAt time.Time
}

// internal/auth/feishu/feishu.go
type FeishuAuth struct {
    // Pre-configured scopes/events the user wants
    DefaultAddons *registration.AppAddons
    // Pre-filled app name/description (optional)
    AppPreset *registration.AppPreset
    // For update flow: pre-existing AppID
    ExistingAppID string
    // For CreateOnly mode (default false)
    CreateOnly bool
}

func NewFeishuAuth(opts FeishuAuthOptions) *FeishuAuth { ... }

func (f *FeishuAuth) Login(ctx context.Context) (*Credentials, error) {
    result, err := registration.RegisterApp(ctx, &registration.Options{
        AppID:      f.ExistingAppID,  // empty = create new
        CreateOnly: f.CreateOnly,
        Addons:     f.DefaultAddons,
        AppPreset:  f.AppPreset,
        OnQRCode: func(info *registration.QRCodeInfo) {
            // Display URL + render QR in terminal
        },
        OnStatusChange: func(info *registration.StatusChangeInfo) {
            // Print polling status
        },
    })
    if err != nil {
        var regErr *registration.RegisterAppError
        if errors.As(err, &regErr) {
            return nil, fmt.Errorf("register app failed: %s - %s", regErr.Code, regErr.Description)
        }
        return nil, err
    }
    return &Credentials{
        AppID:     result.ClientID,
        AppSecret: result.ClientSecret,
        AppName:   "nightme",  // 来自 AppPreset.Name
        CreatedAt: time.Now(),
    }, nil
}

func (f *FeishuAuth) Name() string { return "feishu" }
```

## 3. Implementation

**文件结构**：
```
internal/auth/
├── auth.go                       # Provider interface + Credentials
└── feishu/
    ├── feishu.go                 # FeishuAuth 实现
    ├── feishu_test.go            # 单元测试
    └── qrencode.go               # 终端 QR 渲染

cmd/nightme/
└── auth.go                       # `nightme auth login feishu` 子命令
```

**新增依赖**：
- `github.com/skip2/go-qrcode` — 终端 QR 渲染（ASCII 字符）

**`nightme auth login feishu` CLI 流程**：
```
$ nightme auth login feishu
[nightme] requesting Feishu authorization...

Scan this QR code with Feishu mobile, or open this URL:
https://accounts.feishu.cn/oauth2/...   (expires in 600 seconds)

[QR CODE]

polling...
status: polling, next check in 5s
status: polling, next check in 5s
✓ App registered successfully!
  App ID:     cli_xxxx
  App Name:   nightme
  Scopes:     im:message:send_as_bot, im:message:receive_v1
  Credentials saved to: ~/.config/nightme/config.yaml (chmod 0600)

Next: run `nightme run` to start the gateway.
```

**预配置的 scopes/events**（nightme 必须的最小集）：

| 类型 | 名称 | 用途 |
|------|------|------|
| Scope (tenant) | `im:message:send_as_bot` | bot 发消息 |
| Scope (tenant) | `im:message:receive_v1` (event) | 接收消息 |
| Scope (tenant) | `im:message.group_at_msg:readonly` | 群消息 @ 提及（v0.1 可选）|
| Event | `im.message.receive_v1` | 收消息事件 |

**Credentials 持久化**：
- 写入 `~/.config/nightme/config.yaml` 的 `channels.feishu.accounts.main` 段
- 文件权限 0600
- 原子写（temp + rename，跟 registry 一样）

**Manual 模式 fallback**：
- config 里已有 `appId` / `appSecret` → `nightme auth login` 报错 "already configured, use --force to overwrite"
- `--force` flag：覆盖现有配置

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| 用户在 10 分钟内没扫码 | context timeout → 报错 "auth timeout" |
| QR 扫描后飞书显示"权限不足" | 飞书返回 error code → nightme 提示用户检查企业权限 |
| 凭证写到 config 失败（disk full / permission） | 保留 in-memory credential，提示用户手动复制到 config |
| config 已有 appId | 报错 "already configured"（除非 `--force`）|
| 用户选了不存在的 tenant | 飞书返回 error，nightme 透传 |
| Lark (international) vs Feishu (国内) | 自动检测 `tenant_brand`，用对应 `LarkDomain` 或 `Domain`（SDK 自动处理）|
| 注册时网络断 | ctx timeout → 报错，下次重试无需重新写 config |
| 用户想看当前凭证 | `nightme auth status feishu` → 显示当前 app_id（不显示 secret）|
| 用户想撤销 app | 文档说明需要去飞书开放平台手动撤销 + 删 config |
| `--update` 模式 | 用 `Options.AppID` 走 update flow，重新授权现有 app |

## 5. 验收流程（v0.1 演示）

1. `nightme auth login feishu`
2. 终端显示 QR
3. 用飞书 App 扫码
4. 飞书显示"nightme 申请 X 权限"
5. 点确认
6. 终端 ✓ + config 自动更新
7. `nightme run` → 飞书 round-trip 工作

**全部不需要去飞书开发者后台**。

## 6. Test plan

**单元测试**（用 mock SDK）：
- `TestFeishuAuth_Login` 模拟注册成功 → 验证返回 credentials
- `TestFeishuAuth_LoginTimeout` 模拟 context cancel → 验证返回 error
- `TestFeishuAuth_LoginError` 模拟飞书返回 error code → 验证 error wrapping
- `TestWriteCredentials_Atomic` 验证 config 写入是原子的 + 0600 权限
- `TestWriteCredentials_AlreadyExists` 验证不覆盖（除非 --force）

**集成测试**（需要 mock registration.RegisterApp）：
- `TestAuthCommand_Success` 模拟整个 QR flow → config 写入正确
- `TestAuthCommand_AlreadyConfigured` 验证错误信息

**E2E（手动）**：
- 在真实飞书账号上跑 `nightme auth login feishu`
- 验证 QR 显示
- 用飞书 App 扫
- 验证 config 写入
- 跑 `nightme run` 验证 round-trip

## 7. 与现有功能的关系

| 现有 | 关系 |
|------|------|
| F-08 Channel Abstraction | 依赖 F-22：Channel 用 F-22 拿到的 credentials 工作 |
| F-20 Gateway | 独立——Gateway 路由 slash command，不涉及 auth |
| 现有 `internal/config/` | F-22 写入 config.yaml 的 `channels.feishu.accounts` 段（chmod 0600）|

**对 F-08 的影响**：
- F-08 Channel 启动时读 `channels.feishu.accounts.main.appId/appSecret`
- 这两个值可以来自：
  1. `nightme auth login feishu`（F-22 写入）✅ 推荐
  2. 手动编辑 config.yaml（v0.1 兼容）

## 8. 实施顺序

| 阶段 | 任务 | 估时 |
|------|------|------|
| M2 PR #4 | F-22 实施（auth package + CLI）| 2-3 commits |
| M2 PR #4 | F-08 Channel adapter 实施（lark-oapi-go 长连接）| 3-4 commits |
| M2 PR #5 | 飞书 round-trip 集成 | 3-4 commits |

PR #4 内部先做 F-22，再做 F-08（让 F-08 测试时用 F-22 拿到的真凭证）。

## 9. 注意事项

- **lark-oapi-go v3 必需**：v3 的 `registration` 是新增的，v1/v2 没有
- **二维码显示**：用 `skip2/go-qrcode` 输出到 stdout（黑底白字）
- **超时默认 10 分钟**：context.WithTimeout，可通过 flag 调整
- **不要打印 ClientSecret 到日志**：log 时 redact
- **不存储 secret 到任何 log 文件**

## 10. Open questions

- Lark (国际版) 是否用相同 SDK？倾向：同 SDK，自动检测 tenant_brand
- 多 tenant 支持（一个 nightme 跑多个飞书 app）？v0.1 不支持，v0.2 考虑
- 用户拒绝授权后如何清理（feishu 侧是否残留）？飞书只授权后才会创建 app，拒绝则不创建，无需清理
- 是否支持 `nightme auth export feishu` 输出可分享的 env var？v0.2 考虑
- 与 M2 现有计划的 conflicts：原 M2 假设 manual setup，新增 F-22 是增量（向后兼容）
- registry vs config：F-22 凭证存 config.yaml（用户可读可改），不是 registry（机器管理的 runtime state）
