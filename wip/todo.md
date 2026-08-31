# Handover: Slack greeting — direct-send to owner (auto-discovered)

本文件交接 **Slack greeting 修复** 这条线。只覆盖本次会话的工作,不涉
及其他分支/模块。

## 任务背景

- **原始需求**:修复 `slack.Greet`(原本是 no-op),只发英文、忽略中文。
- **用户纠正方向**:setup 完成后**直接 DM 给 owner**,不等用户先 hi bot。
  参考 feishu/telegram 的实现。
- **最终定形**:Login 阶段用 `users.list` → `is_primary_owner` 自动发现
  owner,直接 `chat.postMessage(channel=<ownerUserID>)` 发送,**仅英文**。
  无 prompt、无 polling、无等待。

## ✅ 已完成

### 1. telegram 文档修正(已编译通过)
`internal/login/telegram/provider.go` — 2 处误导注释:
"daemon 会补发 greeting / pick up chat_id" → 改为"纯 CLI 直发,无
daemon fallback,daemon 永不重放 login greeting"。代码逻辑本身没动。

### 2. slack provider.go 重构为 direct-send(**编译通过**)
`internal/login/slack/provider.go`:
- `Options.Owner` — 可选 override(默认自动发现)
- `Provider` 字段:`ownerUserID` / `listUsers`(接缝)/ `postDM`(接缝)
- `wireGreet(ctx)` — 构建 slackgo client,接 `listUsers` 接缝,调
  `discoverOwner`,接 `postDM`(owner 烘焙进闭包)
- `discoverOwner(ctx)` — `users.list` → 第一个
  `IsPrimaryOwner && !IsBot && !Deleted`;`opts.Owner` 优先
- `Greet` — 直接发送,guard:`botToken==""` / `SkipGreet` / `postDM==nil`
- `sendGreeting` — `body.English` only,`body.Chinese` 跳过
- **已删除** polling 全套:`greetWaitTimeout` / `pollInterval` / `dmChannel`
  / `listDMsFunc` / `waitForFirstDM` / `snapshotDMs` / `parseSlackTS`

### 3. manifest 修正
`internal/login/slack/manifest.go` — 加 `features.app_home.messages_tab_enabled: true`
(修用户的 "sending messages to this app has been turned off" 报错)。
YAML 已校验通过。

### 4. 实测(独立程序 `/tmp/slack_greet_live_test/`,不依赖本包)
| 项 | 结果 |
|---|---|
| `auth.test` | ✅ bot=`nightme`(U0BTPL0JFPY), workspace=`nightmeworkspace` |
| `users.list` → `is_primary_owner` | ✅ owner = `U0BTR4JTD26`("Lz") |
| `chat.postMessage(channel=ownerUserID)` | ✅ 2 条英文 greeting 全部 ok+ts |
| 端到端(自动发现 → 投递) | ✅ 已投递到 owner DM |

**排除掉的方案(实测证明)**:
- bot token 认不出 installer(`auth.test` 只返回 bot 自己)
- `conversations.list(types=im)` 第一个是 **Slackbot**(`USLACK`)→ 会发错
- `conversations.list` 的 `latest` 字段**全空** → polling 必然超时
- **Messages tab 默认 off** → 用户不能 DM bot,`message.im` 永不触发
- `apps.permissions.*`:`unknown_method`(granular-only,不适用)
- `team.info`:`missing_scope`

## ⚠️ 未完成(当前 build 状态)

- `go build ./...` ✅ **通过**(provider.go 没问题)
- `go vet` / `go test ./internal/login/slack/` ❌ **失败** — 原因如下

### 1. provider_test.go 旧 Greet 测试未重写(编译失败主因)
5 个测试仍引用已删除的 `dmChannel` / `listDMs` / 3 参 `postDM`:
- L213 `TestGreet_SkipsWhenNoBotToken`
- L235 `TestGreet_SkipsWhenSkipGreet`
- L253 `TestGreet_SendsEnglishOnlyAndIgnoresChinese`
- L300 `TestGreet_TimesOutSoftlyAndPostsNothing`(polling 语义,该删)
- L327 `TestGreet_IgnoresStaleMessagesInKnownDM`(polling 语义,该删)

`containsCJK` 辅助函数已在文件末尾保留(英文-only 断言仍用)。

### 2. register.go help 文案过期
`internal/login/slack/register.go`:
- Long help 仍写 "waits up to 2 minutes for the owner to DM the bot"
- `--no-greet` 描述 "skip the post-login greeting (and its 2-minute DM wait)"
- 需改为"自动发现 owner 直接发送,无等待"

### 3. register.go 未接 `--owner` flag
`Options.Owner` 存在,但 `loginSlackCmdFlags` 无对应字段/flag,CLI 无法设置
override。需加 `--owner` flag(可选,默认自动发现)。

### 4. manifest 测试未加断言
`manifestView` 需加 `Features.AppHome.MessagesTabEnabled bool` 字段,
`TestAppManifest_CarriesRequiredScopesAndEvents` 加断言,防止
`messages_tab_enabled` 回归。

## 📋 计划

1. **重写 provider_test.go 的 Greet 测试**为 auto-discover + direct-send 形态:
   - `TestGreet_SkipsWhenNoBotToken` — `botToken==""` → nil,不碰接缝
   - `TestGreet_SkipsWhenSkipGreet` — `SkipGreet` → nil
   - `TestGreet_SkipsWhenNoOwnerDiscovered` — `postDM==nil` → 打印 skip
     提示,返回 nil(替代 TimesOut / Stale 两个 polling 测试)
   - `TestGreet_SendsEnglishOnlyToOwner` — 设 `ownerUserID`+`postDM`(记录)
     → 恰好 2 条英文,无 CJK(`containsCJK`)
   - `TestDiscoverOwner_PicksPrimaryOwner` — `listUsers` 返回多个 userView
     → 选 `is_primary_owner` 那个
   - `TestDiscoverOwner_OwnerFlagOverrides` — `opts.Owner` 设 → 直接返回,
     不查 `listUsers`
   - `TestDiscoverOwner_SkipsBotsAndDeleted` — `is_primary_owner+is_bot=true`
     跳过;`deleted` 跳过
2. **register.go** — Long help + `--no-greet` 描述更新;加 `--owner` flag
   接入 `Options.Owner`
3. **manifest 测试** — `manifestView` 加字段 + 加断言
4. `gofmt` + `go build` + `go test ./internal/login/slack/` 全绿
5. (可选)清理 `/tmp/slack_greet_live_test/`(独立实测程序,已完成使命)

## 设计决策记录

- **Slack 与 telegram 不同**:telegram 无 owner 概念,只能 polling;Slack 有
  `is_primary_owner`,可自动发现 → direct-send,无需 prompt
- **比 feishu 更优**:feishu 依赖 consent flow 拿 open_id;Slack 靠
  `users.list` 一个标志位,**零用户输入**
- **英文-only**:Slack 无 Feishu 式双语 post 块,发两条只会加倍噪音(同
  `telegram.sendGreeting` 决策)
- **messages_tab_enabled 必须 true**:否则用户不能 DM bot,`message.im` 永不
  触发,**daemon 收不到任何 DM**——这是独立 bug,已修 manifest,但**用户需要
  reinstall app 或手动开 App Home → Messages Tab 开关才生效**

## 文件改动清单(git status)

```
 M internal/login/slack/manifest.go       (+8 行, messages_tab_enabled)
 M internal/login/slack/provider.go       (+246/-, direct-send 重构)
 M internal/login/slack/provider_test.go  (+170 行, 旧测试未重写)
 M internal/login/slack/register.go       (+14 行, 旧 help 未更新)
 M internal/login/telegram/provider.go    (+13 行, 文档修正,已完成)
```
